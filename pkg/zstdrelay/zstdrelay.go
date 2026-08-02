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

// Package zstdrelay provides the compression/buffering primitives shared by
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
package zstdrelay

import (
	"io"
	"sync"
	"sync/atomic"

	"github.com/klauspost/compress/zstd"
)

// NewEncoder wraps w with a zstd encoder configured for low-latency,
// per-chunk-flush use against a live, long-lived connection rather than a
// file: concurrency=1 disables the library's internal parallel-block
// goroutines (there to help bulk file throughput, not to push small chunks
// out immediately), which is what makes Flush()'s guarantees exact.
func NewEncoder(w io.Writer, level int) (*zstd.Encoder, error) {
	return zstd.NewWriter(w,
		zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(level)),
		zstd.WithEncoderConcurrency(1),
	)
}

// NewDecoder wraps r with a zstd decoder that decompresses incrementally, on
// demand, as bytes are requested -- rather than the library's default mode,
// which uses background goroutines tuned for bulk throughput.
func NewDecoder(r io.Reader) (*zstd.Decoder, error) {
	return zstd.NewReader(r, zstd.WithDecoderConcurrency(1))
}

// flushWriter is satisfied by *zstd.Encoder. Flush pushes everything written
// so far to the underlying writer without ending the compressed frame --
// unlike Close, the stream can still be written to afterward.
type flushWriter interface {
	Write(p []byte) (int, error)
	Flush() error
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

// Write enqueues a copy of p, blocking while the buffer is full. It never
// partially writes: once it returns with a nil error, all of p has been
// enqueued.
func (b *BoundedBuffer) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for b.curBytes >= b.maxBytes && !b.closed {
		b.notFull.Wait()
	}
	if b.closed {
		return 0, io.ErrClosedPipe
	}
	cp := make([]byte, len(p))
	copy(cp, p)
	b.chunks = append(b.chunks, cp)
	b.curBytes += len(cp)
	b.notEmpty.Signal()
	return len(p), nil
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
