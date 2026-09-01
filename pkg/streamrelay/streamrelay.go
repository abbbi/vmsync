/*
	Copyright (C) 2026  Michael Ablassmeier <abi@grinser.de>

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU General Public License for more details.

You should have received a copy of the GNU General Public License
along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

// Package streamrelay provides the compression/buffering primitives shared by
// vmsync's local NBD bridge relay (pkg/nbdbridge) and the remote
// vmsync-bridge-helper binary (cmd/vmsync-bridge-helper).
//
// It exists because the `zstd` CLI, used via a subprocess pipe, buffers all
// input internally and only flushes to its output on EOF or once an internal
// read buffer fills -- confirmed directly: piping a few bytes into
// `zstd -q -3 | zstd -dq` while holding the writer open (no EOF) produced no
// output at all until the writer eventually closed. NBD is a synchronous,
// long-lived, small-message protocol whose connection never closes mid-sync,
// so a subprocess-piped zstd can never relay even the very first handshake
// byte. This package does compression in-process instead, with an explicit
// Flush() after every chunk, which the CLI has no equivalent for.
package streamrelay

import (
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/klauspost/compress/s2"
	"github.com/klauspost/compress/zstd"
)

// Algo selects which compression format Relay/RelayFromWire use.
type Algo string

const (
	// AlgoZstd is zstd (github.com/klauspost/compress/zstd) -- better
	// compression ratio, run single-threaded here for exact per-chunk
	// Flush() semantics. The default: the better fit when the network link
	// itself, not CPU, is the bottleneck, since every byte saved matters
	// more than compression speed.
	AlgoZstd Algo = "zstd"
	// AlgoS2 is S2 (github.com/klauspost/compress/s2), a Snappy-derived
	// format trading compression ratio for substantially higher throughput.
	// The better fit when the link (or underlying storage) is fast enough
	// that compression speed itself, not ratio, ends up being the
	// bottleneck -- e.g. observed directly: a link capable of hundreds of
	// MB/s, where raising zstd's level made things worse (more CPU work)
	// while lowering it made no difference (already at the link's own
	// ceiling, not zstd's).
	AlgoS2 Algo = "s2"
)

// ParseAlgo validates vmsync's -compress=<value> flag value, treating "" as
// AlgoZstd (the default).
func ParseAlgo(s string) (Algo, error) {
	switch Algo(s) {
	case AlgoZstd, "":
		return AlgoZstd, nil
	case AlgoS2:
		return AlgoS2, nil
	default:
		return "", fmt.Errorf("-compress must be \"zstd\" or \"s2\", got %q", s)
	}
}

// flushCloser is satisfied by both *zstd.Encoder and *s2.Writer -- both
// expose this same Write/Flush/Close shape, letting Relay use either
// interchangeably without caring which. Flush pushes everything written so
// far to the underlying writer without ending the compressed stream --
// unlike Close, the stream can still be written to afterward.
type flushCloser interface {
	Write(p []byte) (int, error)
	Flush() error
	Close() error
}

// flushWriter is the subset of flushCloser CopyFlushing actually needs.
// Satisfied by anything satisfying flushCloser too.
type flushWriter interface {
	Write(p []byte) (int, error)
	Flush() error
}

// NewEncoder wraps w with an encoder for algo, configured for low-latency,
// per-chunk-flush use against a live, long-lived connection rather than a
// file. For zstd, concurrency=1 disables the library's internal
// parallel-block goroutines (there to help bulk file throughput, not to
// push small chunks out immediately), which is what makes Flush()'s
// guarantees exact.
//
// level's accepted values depend on algo: for zstd it's a traditional
// numeric level, "1"-"19"; S2 has no numeric levels at all, only three
// discrete modes -- "default" (fastest, S2's own default), "better", or
// "best" (slowest, closest to zstd's own ratio) -- selected via
// s2.WriterBetterCompression()/s2.WriterBestCompression().
func NewEncoder(algo Algo, w io.Writer, level string) (flushCloser, error) {
	if algo == AlgoS2 {
		switch level {
		case "", "default":
			return s2.NewWriter(w, s2.WriterConcurrency(1)), nil
		case "better":
			return s2.NewWriter(w, s2.WriterConcurrency(1), s2.WriterBetterCompression()), nil
		case "best":
			return s2.NewWriter(w, s2.WriterConcurrency(1), s2.WriterBestCompression()), nil
		default:
			return nil, fmt.Errorf("--compress-level must be \"default\", \"better\", or \"best\" for -compress=s2, got %q", level)
		}
	}
	n, err := strconv.Atoi(level)
	if err != nil {
		return nil, fmt.Errorf("invalid zstd --compress-level %q: %w", level, err)
	}
	return zstd.NewWriter(w,
		zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(n)),
		zstd.WithEncoderConcurrency(1),
	)
}

// NewDecoder wraps r with a decoder for algo that decompresses
// incrementally, on demand, as bytes are requested. It returns a close
// function to call once done reading, rather than a concrete decoder type:
// *zstd.Decoder has a Close method (no error return) but *s2.Reader has none
// at all, so there's no single interface to hand back that covers both --
// the returned closure is a no-op for S2.
func NewDecoder(algo Algo, r io.Reader) (io.Reader, func(), error) {
	if algo == AlgoS2 {
		return s2.NewReader(r), func() {}, nil
	}
	dec, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return nil, nil, err
	}
	return dec, dec.Close, nil
}

// CopyFlushing is io.Copy's loop shape, plus an explicit Flush() after every
// nonzero read. This is the entire fix for the buffering problem described
// in the package doc: each chunk read from src (typically one NBD protocol
// message or reply, exactly as delivered by a single underlying TCP read) is
// pushed through dst immediately, instead of sitting in zstd's internal
// buffer until it fills or the stream ends.
func CopyFlushing(dst flushWriter, src io.Reader) (written int64, err error) {
	buf := make([]byte, 32*1024)
	for {
		nr, rerr := src.Read(buf)
		if nr > 0 {
			nw, werr := dst.Write(buf[:nr])
			written += int64(nw)
			if werr != nil {
				return written, werr
			}
			if nw != nr {
				return written, io.ErrShortWrite
			}
			if ferr := dst.Flush(); ferr != nil {
				return written, ferr
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				return written, nil
			}
			return written, rerr
		}
	}
}

// CountingWriter wraps an io.Writer, atomically accumulating the number of
// bytes actually written into Counter. Used to measure real wire traffic
// once compression means there's no subprocess stdout left to read from for
// the same purpose.
type CountingWriter struct {
	W       io.Writer
	Counter *uint64
}

func (c *CountingWriter) Write(p []byte) (int, error) {
	n, err := c.W.Write(p)
	if n > 0 {
		atomic.AddUint64(c.Counter, uint64(n))
	}
	return n, err
}

// CountingReader is CountingWriter's read-side counterpart.
type CountingReader struct {
	R       io.Reader
	Counter *uint64
}

func (c *CountingReader) Read(p []byte) (int, error) {
	n, err := c.R.Read(p)
	if n > 0 {
		atomic.AddUint64(c.Counter, uint64(n))
	}
	return n, err
}

// BoundedBuffer is a bounded, concurrent-safe FIFO byte queue: the netbuffer
// stage's implementation. Write blocks once the buffer already holds
// maxBytes worth of data (backpressure against the configured total buffer
// size); Read blocks until data is available or the buffer has been closed
// and fully drained, at which point it returns io.EOF. Decoupling a producer
// and a consumer this way provides throughput smoothing natively, without
// depending on an external CLI's own (unverified) flushing behavior.
type BoundedBuffer struct {
	mu       sync.Mutex
	notEmpty *sync.Cond
	notFull  *sync.Cond
	chunks   [][]byte
	curBytes int
	maxBytes int
	closed   bool
}

// NewBoundedBuffer creates a BoundedBuffer holding at most maxBytes of
// unread data at a time.
func NewBoundedBuffer(maxBytes int) *BoundedBuffer {
	b := &BoundedBuffer{maxBytes: maxBytes}
	b.notEmpty = sync.NewCond(&b.mu)
	b.notFull = sync.NewCond(&b.mu)
	return b
}

// Write enqueues a copy of p, blocking while the buffer is full. From the
// caller's perspective it never partially writes: once it returns with a
// nil error, all of p has been enqueued. Internally, though, a single call
// can be split across several bounded sub-writes -- each capped at however
// much room is actually left -- rather than appending p in one shot
// whenever the buffer merely isn't full *yet*. Without that, a single large
// p (e.g. io.Copy's default 32KB internal buffer, used whenever compression
// is off) could push curBytes well past maxBytes before the next Write call
// ever sees the buffer as full, silently breaking the "at most maxBytes"
// guarantee this type's own doc comment makes.
func (b *BoundedBuffer) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	written := 0
	for len(p) > 0 {
		for b.curBytes >= b.maxBytes && !b.closed {
			b.notFull.Wait()
		}
		if b.closed {
			// written may be > 0 here: some earlier iteration of this same
			// call could have already enqueued data before the buffer was
			// closed out from under a later one. Reporting that accurately
			// matters to callers like io.Copy, which accumulate whatever a
			// Writer reports even alongside a non-nil error.
			return written, io.ErrClosedPipe
		}
		room := b.maxBytes - b.curBytes
		n := len(p)
		if n > room {
			n = room
		}
		cp := make([]byte, n)
		copy(cp, p[:n])
		b.chunks = append(b.chunks, cp)
		b.curBytes += n
		b.notEmpty.Signal()
		p = p[n:]
		written += n
	}
	return written, nil
}

// Read dequeues whatever is available, blocking while the buffer is empty
// and still open. Once closed and drained, it returns io.EOF.
func (b *BoundedBuffer) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for len(b.chunks) == 0 {
		if b.closed {
			return 0, io.EOF
		}
		b.notEmpty.Wait()
	}
	chunk := b.chunks[0]
	n := copy(p, chunk)
	if n < len(chunk) {
		b.chunks[0] = chunk[n:]
	} else {
		b.chunks = b.chunks[1:]
	}
	b.curBytes -= n
	b.notFull.Signal()
	return n, nil
}

// Close marks the buffer closed: pending and future Writes fail with
// io.ErrClosedPipe, and Read returns io.EOF once any already-queued data has
// been drained.
func (b *BoundedBuffer) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	b.notEmpty.Broadcast()
	b.notFull.Broadcast()
	return nil
}
