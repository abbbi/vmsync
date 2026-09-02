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

// This file exercises the pieces of pkg/streamrelay that
// netbuffer_deadlock_test.go's Relay/RelayFromWire deadlock regressions don't
// otherwise cover: option parsing (ParseAlgo, ParseByteSize), the
// CountingWriter/CountingReader wire-traffic counters, BoundedBuffer's
// blocking/close/EOF semantics in isolation, the NewEncoder/NewDecoder pair
// on its own (rather than only via Relay), CopyFlushing's per-chunk Flush
// behavior, and a full compress-then-decompress round trip through
// Relay/RelayFromWire for both supported algorithms.
//
// Like netbuffer_deadlock_test.go, this lives under tests/ as package tests
// and only touches streamrelay's exported API -- runWithDeadline, itself
// defined in netbuffer_deadlock_test.go, is reused here rather than
// redeclared since both files are part of the same package.
//
// NewEncoder returns streamrelay's unexported flushCloser interface type, and
// CopyFlushing takes its unexported flushWriter interface type as a
// parameter. Neither is a problem from here: a value of an unexported
// interface type can still be held (via :=) and have its (exported) methods
// called without ever naming the type, and a parameter of unexported
// interface type can still be satisfied by any local concrete type whose
// method set matches structurally -- Go interface satisfaction across
// package boundaries never requires naming the interface.
package tests

import (
	"bytes"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"vmsync/pkg/streamrelay"
)

// TestParseAlgo covers vmsync's -compress=<value> flag parsing: "" defaults
// to zstd, "zstd" and "s2" are accepted verbatim, and anything else is
// rejected.
func TestParseAlgo(t *testing.T) {
	cases := []struct {
		in      string
		want    streamrelay.Algo
		wantErr bool
	}{
		{"", streamrelay.AlgoZstd, false},
		{"zstd", streamrelay.AlgoZstd, false},
		{"s2", streamrelay.AlgoS2, false},
		{"bogus", "", true},
	}
	for _, tc := range cases {
		got, err := streamrelay.ParseAlgo(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseAlgo(%q) = %q, nil, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseAlgo(%q) returned unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseAlgo(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestParseByteSize covers ParseByteSize's bare-number and suffixed forms.
// Suffixes are confirmed binary (1024-based, not 1000-based) straight from
// the source (pkg/streamrelay/relay.go): multiplier is 1024 per step, not
// 1000.
func TestParseByteSize(t *testing.T) {
	const (
		kib = 1024
		mib = 1024 * 1024
		gib = 1024 * 1024 * 1024
		tib = 1024 * 1024 * 1024 * 1024
	)
	cases := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"1024", 1024, false},
		{"1b", 1, false},
		{"1B", 1, false},
		{"1k", 1 * kib, false},
		{"1K", 1 * kib, false},
		{"1m", 1 * mib, false},
		{"1M", 1 * mib, false},
		{"1g", 1 * gib, false},
		{"1G", 1 * gib, false},
		{"1t", 1 * tib, false},
		{"1T", 1 * tib, false},
		{"abc", 0, true},
		{"", 0, true},
	}
	for _, tc := range cases {
		got, err := streamrelay.ParseByteSize(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseByteSize(%q) = %d, nil, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseByteSize(%q) returned unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseByteSize(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestCountingWriter checks that CountingWriter both forwards every byte to
// the wrapped writer unchanged and atomically tallies the byte count into
// Counter.
func TestCountingWriter(t *testing.T) {
	var underlying bytes.Buffer
	var counter uint64
	cw := &streamrelay.CountingWriter{W: &underlying, Counter: &counter}

	data := []byte("hello, counting writer")
	n, err := cw.Write(data)
	if err != nil {
		t.Fatalf("CountingWriter.Write returned error: %v", err)
	}
	if n != len(data) {
		t.Fatalf("CountingWriter.Write returned n=%d, want %d", n, len(data))
	}
	if counter != uint64(len(data)) {
		t.Fatalf("Counter = %d, want %d", counter, len(data))
	}
	if underlying.String() != string(data) {
		t.Fatalf("underlying buffer = %q, want %q", underlying.String(), string(data))
	}
}

// TestCountingReader checks that CountingReader both forwards every byte
// read from the wrapped reader unchanged and atomically tallies the byte
// count into Counter.
func TestCountingReader(t *testing.T) {
	data := []byte("hello, counting reader")
	var counter uint64
	cr := &streamrelay.CountingReader{R: bytes.NewReader(data), Counter: &counter}

	out := make([]byte, len(data))
	n, err := io.ReadFull(cr, out)
	if err != nil {
		t.Fatalf("io.ReadFull via CountingReader returned error: %v", err)
	}
	if n != len(data) {
		t.Fatalf("read n=%d, want %d", n, len(data))
	}
	if counter != uint64(len(data)) {
		t.Fatalf("Counter = %d, want %d", counter, len(data))
	}
	if !bytes.Equal(out, data) {
		t.Fatalf("read %q, want %q", out, data)
	}
}

// TestBoundedBuffer exercises BoundedBuffer's blocking, close, and EOF
// semantics directly, in isolation from Relay/RelayFromWire. Every
// sub-test that could conceivably hang on a regression is wrapped with
// runWithDeadline (or an explicit bounded select), matching this package's
// established convention of turning potential deadlocks into clean test
// failures instead of hanging the whole test binary.
func TestBoundedBuffer(t *testing.T) {
	t.Run("write then read less than capacity", func(t *testing.T) {
		b := streamrelay.NewBoundedBuffer(16)
		data := []byte("hello")

		err, returned := runWithDeadline(t, 5*time.Second, func() error {
			_, err := b.Write(data)
			return err
		})
		if !returned {
			t.Fatal("Write of fewer bytes than capacity did not return within 5s")
		}
		if err != nil {
			t.Fatalf("Write returned error: %v", err)
		}

		out := make([]byte, len(data))
		var n int
		err, returned = runWithDeadline(t, 5*time.Second, func() error {
			var rerr error
			n, rerr = b.Read(out)
			return rerr
		})
		if !returned {
			t.Fatal("Read did not return within 5s")
		}
		if err != nil {
			t.Fatalf("Read returned error: %v", err)
		}
		if n != len(data) || !bytes.Equal(out, data) {
			t.Fatalf("Read got %q (n=%d), want %q", out[:n], n, data)
		}
	})

	t.Run("write exactly capacity does not block", func(t *testing.T) {
		b := streamrelay.NewBoundedBuffer(8)
		data := []byte("12345678") // exactly maxBytes

		err, returned := runWithDeadline(t, 5*time.Second, func() error {
			_, err := b.Write(data)
			return err
		})
		if !returned {
			t.Fatal("Write of exactly maxBytes blocked -- it should fit without waiting for a reader")
		}
		if err != nil {
			t.Fatalf("Write returned error: %v", err)
		}
	})

	t.Run("write beyond capacity blocks until a read frees space", func(t *testing.T) {
		b := streamrelay.NewBoundedBuffer(8)

		// Fill the buffer completely first.
		if _, err := b.Write([]byte("12345678")); err != nil {
			t.Fatalf("initial fill Write returned error: %v", err)
		}

		// This second write has nowhere to go until something is read out,
		// so it must block.
		second := []byte("abcd")
		writeDone := make(chan error, 1)
		go func() {
			_, err := b.Write(second)
			writeDone <- err
		}()

		select {
		case <-writeDone:
			t.Fatal("second Write returned even though the buffer should still be completely full")
		case <-time.After(200 * time.Millisecond):
			// Expected: still blocked.
		}

		// Free exactly enough room for the blocked write to complete.
		readBuf := make([]byte, len(second))
		n, err := b.Read(readBuf)
		if err != nil {
			t.Fatalf("Read returned error: %v", err)
		}
		if n == 0 {
			t.Fatal("Read returned 0 bytes despite the buffer being full")
		}

		select {
		case err := <-writeDone:
			if err != nil {
				t.Fatalf("previously blocked Write returned error after Read freed space: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("previously blocked Write did not complete within 5s of Read freeing space -- possible regression of the blocking/wakeup logic")
		}
	})

	t.Run("a chunk read via several smaller reads is returned in order, one piece at a time", func(t *testing.T) {
		b := streamrelay.NewBoundedBuffer(16)
		data := []byte("hello world") // 11 bytes, enqueued as a single chunk

		if _, err := b.Write(data); err != nil {
			t.Fatalf("Write returned error: %v", err)
		}

		// Read it back 3 bytes at a time -- deliberately smaller than the
		// enqueued chunk, so the first Read can only satisfy part of it.
		// Read's own doc comment says the remainder gets put back at the
		// front of the queue (b.chunks[0] = chunk[n:]) rather than being
		// dropped or the whole chunk being reported consumed regardless of
		// how much of it the caller's buffer could actually hold -- nothing
		// before this test ever called Read with a buffer smaller than a
		// pending chunk, so that specific line was never exercised.
		var got []byte
		reads := 0
		out := make([]byte, 3)
		for len(got) < len(data) {
			err, returned := runWithDeadline(t, 5*time.Second, func() error {
				var rerr error
				var n int
				n, rerr = b.Read(out)
				got = append(got, out[:n]...)
				return rerr
			})
			if !returned {
				t.Fatalf("Read blocked instead of returning the chunk's remaining bytes (got %d/%d so far)", len(got), len(data))
			}
			if err != nil {
				t.Fatalf("Read returned error: %v", err)
			}
			reads++
			if reads > len(data) {
				t.Fatalf("Read called %d times without ever draining %d bytes -- possible infinite loop from a Read that returns 0 bytes and no error", reads, len(data))
			}
		}
		if reads < 2 {
			t.Fatalf("drained the chunk in %d read(s) with a 3-byte buffer against an 11-byte chunk -- this test isn't actually exercising the partial-read path", reads)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("reassembled partial reads = %q, want %q", got, data)
		}
	})

	t.Run("a single Write larger than capacity is split across multiple chunks and drained correctly by a concurrent reader", func(t *testing.T) {
		b := streamrelay.NewBoundedBuffer(8)
		data := make([]byte, 30) // well over 3x maxBytes
		for i := range data {
			data[i] = byte(i)
		}

		// This single Write call cannot complete on its own: after its
		// first bounded sub-write fills the buffer (see Write's own doc
		// comment on splitting one call into several bounded sub-writes),
		// it blocks until a reader drains enough room for the next
		// sub-write, and so on -- exercising the "overflow" half of this
		// test's own name: what happens when one Write call's payload
		// doesn't fit in a single pass, not just when it doesn't fit in
		// the buffer at all (already covered by the blocks-until-read test
		// above, using data no larger than maxBytes itself).
		writeErrCh := make(chan error, 1)
		go func() {
			_, err := b.Write(data)
			writeErrCh <- err
		}()

		var got []byte
		out := make([]byte, 5) // another size that doesn't evenly divide maxBytes or len(data)
		readErrCh := make(chan error, 1)
		go func() {
			for len(got) < len(data) {
				n, err := b.Read(out)
				got = append(got, out[:n]...)
				if err != nil {
					readErrCh <- err
					return
				}
			}
			readErrCh <- nil
		}()

		select {
		case err := <-writeErrCh:
			if err != nil {
				t.Fatalf("Write returned error: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Write of data far exceeding capacity did not complete within 5s of a reader draining it -- possible regression in the blocking/wakeup logic for a multi-chunk Write")
		}
		select {
		case err := <-readErrCh:
			if err != nil {
				t.Fatalf("reader goroutine returned error: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("reader goroutine did not finish draining within 5s of Write completing")
		}

		if !bytes.Equal(got, data) {
			t.Fatalf("data received across a multi-chunk overflowing Write does not match what was sent (len got=%d, want=%d)", len(got), len(data))
		}
	})

	t.Run("write after close returns ErrClosedPipe", func(t *testing.T) {
		b := streamrelay.NewBoundedBuffer(8)
		if err := b.Close(); err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
		_, err := b.Write([]byte("x"))
		if err != io.ErrClosedPipe {
			t.Fatalf("Write after Close returned %v, want io.ErrClosedPipe", err)
		}
	})

	t.Run("read after close on a drained buffer returns EOF", func(t *testing.T) {
		b := streamrelay.NewBoundedBuffer(8)
		if err := b.Close(); err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
		out := make([]byte, 4)
		n, err := b.Read(out)
		if err != io.EOF {
			t.Fatalf("Read on a closed, empty buffer returned %v, want io.EOF", err)
		}
		if n != 0 {
			t.Fatalf("Read on a closed, empty buffer returned n=%d, want 0", n)
		}
	})

	// The case the drained one above does NOT cover, and the one that loses
	// data when it is wrong: closed with bytes STILL QUEUED.
	//
	// Close's own doc promises "Read returns io.EOF once any already-queued
	// data has been drained" -- once drained, not before. This is not a
	// theoretical distinction. Relay's drain goroutine and its producer race
	// on exactly this: the producer calls Close as soon as src hits EOF, and
	// if that lands before the drain goroutine's first Read, a Read that
	// reports EOF while holding data silently discards the whole transfer.
	// io.Copy treats EOF as success, so Relay then returns nil having
	// delivered nothing -- a sync that reports success and copied no bytes.
	t.Run("read after close still drains queued data", func(t *testing.T) {
		b := streamrelay.NewBoundedBuffer(64)
		payload := []byte("queued before the close")
		if _, err := b.Write(payload); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if err := b.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		// io.ReadAll rather than a single Read, so this also pins that EOF
		// arrives only AFTER the queue empties rather than partway through.
		got, err := io.ReadAll(b)
		if err != nil {
			t.Fatalf("ReadAll after Close: %v", err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("after Close the buffer yielded %q, want %q -- data queued before "+
				"Close was discarded, which in Relay is a transfer that reports success "+
				"and delivers nothing", got, payload)
		}
	})
}

// TestEncoderDecoderRoundTrip checks NewEncoder/NewDecoder directly (rather
// than only indirectly, through Relay/RelayFromWire, as in
// TestRelayCompressRoundTrip below) for both supported algorithms: a payload
// written then closed via the encoder, decoded back out from a fresh
// bytes.Reader over the encoded bytes, must come back byte-for-byte
// identical.
//
// Only Write then Close is needed here, no explicit mid-stream Flush: Close
// itself is documented (and, for the zstd/s2 libraries this package wraps,
// implemented) to flush and finalize whatever was written first -- an
// explicit Flush only matters when more data is going to be written
// afterward on the same stream, which is exactly what Relay's use of
// CopyFlushing (Write, Flush, repeat, then Close once at the very end) is
// for.
func TestEncoderDecoderRoundTrip(t *testing.T) {
	payload := []byte(strings.Repeat("The quick brown fox jumps over the lazy dog. ", 200))

	cases := []struct {
		name  string
		algo  streamrelay.Algo
		level string
	}{
		{"zstd", streamrelay.AlgoZstd, "3"},
		{"s2", streamrelay.AlgoS2, "better"},
		// s2 at "best" was the one encoder branch no Go test ever
		// constructed, while nbdbridge.ValidateCompressLevel accepts it and
		// contrib/bench/scenarios.conf drives it. The realistic failure is a
		// loud construction error at bridge connect rather than a bad
		// replica, so this earns its place purely by costing one line in a
		// table that already exists.
		{"s2-best", streamrelay.AlgoS2, "best"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var compressed bytes.Buffer
			enc, err := streamrelay.NewEncoder(tc.algo, &compressed, tc.level)
			if err != nil {
				t.Fatalf("NewEncoder(%s): %v", tc.name, err)
			}
			n, err := enc.Write(payload)
			if err != nil {
				t.Fatalf("encoder Write: %v", err)
			}
			if n != len(payload) {
				t.Fatalf("encoder Write returned n=%d, want %d", n, len(payload))
			}
			if err := enc.Close(); err != nil {
				t.Fatalf("encoder Close: %v", err)
			}

			dec, closeDec, err := streamrelay.NewDecoder(tc.algo, bytes.NewReader(compressed.Bytes()))
			if err != nil {
				t.Fatalf("NewDecoder(%s): %v", tc.name, err)
			}
			defer closeDec()

			decoded, err := io.ReadAll(dec)
			if err != nil {
				t.Fatalf("decoder ReadAll: %v", err)
			}
			if !bytes.Equal(decoded, payload) {
				t.Fatalf("round trip mismatch for %s: got %d bytes, want %d bytes", tc.name, len(decoded), len(payload))
			}
		})
	}
}

// chunkReader is a hand-rolled io.Reader that hands back one whole slice
// from chunks per Read call (never coalescing or splitting them), then
// io.EOF once exhausted. Used to prove CopyFlushing flushes once per
// nonzero Read -- a single bytes.Reader wouldn't demonstrate this since it
// tends to return everything it has in one Read call.
type chunkReader struct {
	chunks [][]byte
	i      int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if r.i >= len(r.chunks) {
		return 0, io.EOF
	}
	n := copy(p, r.chunks[r.i])
	r.i++
	return n, nil
}

// recordingFlushWriter is a flushWriter (Write+Flush) fake that records
// every byte written, how many times Flush was called, and the exact
// interleaving of Write/Flush calls.
type recordingFlushWriter struct {
	buf        bytes.Buffer
	flushCount int
	order      []string
}

func (w *recordingFlushWriter) Write(p []byte) (int, error) {
	w.order = append(w.order, "write")
	return w.buf.Write(p)
}

func (w *recordingFlushWriter) Flush() error {
	w.order = append(w.order, "flush")
	w.flushCount++
	return nil
}

// The two ways a destination can break. Sentinels rather than fresh
// errors.New calls at each use, so the tests can assert with errors.Is
// instead of matching on message text.
var (
	errSimulatedWrite = errors.New("simulated write failure")
	errSimulatedFlush = errors.New("simulated flush failure")
)

// failingFlushWriter is a flushWriter fake that breaks on demand: a non-nil
// writeErr fails every Write, a non-nil flushErr fails every Flush, and both
// nil is a writer that simply works.
//
// It counts Writes so a test can assert CopyFlushing STOPPED, not merely
// that it returned an error -- a loop that keeps pumping a dead destination
// and reports the first failure at the end is a different bug with the same
// return value.
type failingFlushWriter struct {
	writeErr error
	flushErr error
	writes   int
}

func (w *failingFlushWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	return len(p), nil
}

func (w *failingFlushWriter) Flush() error { return w.flushErr }

// TestCopyFlushing checks CopyFlushing's two documented behaviors: an
// explicit Flush() after every nonzero Read (not just once at the end), and
// propagating a Write error as its own return value.
//
// CopyFlushing's dst parameter is streamrelay's unexported flushWriter
// interface type, but that's not an obstacle here: *recordingFlushWriter and
// failingFlushWriter each satisfy it structurally (Write + Flush), and Go
// lets any external concrete type satisfy an unexported interface parameter
// without ever needing to name that interface.
func TestCopyFlushing(t *testing.T) {
	t.Run("flushes once per nonzero read, in write-then-flush order", func(t *testing.T) {
		chunks := [][]byte{[]byte("abc"), []byte("de"), []byte("fghij")}
		src := &chunkReader{chunks: chunks}
		dst := &recordingFlushWriter{}

		written, err := streamrelay.CopyFlushing(dst, src)
		if err != nil {
			t.Fatalf("CopyFlushing returned error: %v", err)
		}

		var want bytes.Buffer
		for _, c := range chunks {
			want.Write(c)
		}
		if written != int64(want.Len()) {
			t.Fatalf("CopyFlushing reported written=%d, want %d", written, want.Len())
		}
		if dst.buf.String() != want.String() {
			t.Fatalf("dst got %q, want %q", dst.buf.String(), want.String())
		}
		if dst.flushCount != len(chunks) {
			t.Fatalf("Flush called %d times, want %d (once per nonzero Read)", dst.flushCount, len(chunks))
		}

		wantOrder := []string{"write", "flush", "write", "flush", "write", "flush"}
		if len(dst.order) != len(wantOrder) {
			t.Fatalf("call order = %v, want %v", dst.order, wantOrder)
		}
		for i := range wantOrder {
			if dst.order[i] != wantOrder[i] {
				t.Fatalf("call order[%d] = %q, want %q (full order: %v)", i, dst.order[i], wantOrder[i], dst.order)
			}
		}
	})

	t.Run("write error propagates", func(t *testing.T) {
		src := bytes.NewReader([]byte("some data that will never make it through"))
		_, err := streamrelay.CopyFlushing(&failingFlushWriter{writeErr: errSimulatedWrite}, src)
		if err == nil {
			t.Fatal("CopyFlushing returned a nil error despite the destination failing every write")
		}
		if !errors.Is(err, errSimulatedWrite) {
			t.Fatalf("CopyFlushing returned %q, want it to surface the destination's real failure", err)
		}
	})

	// The branch that actually fires in production, and the one that had no
	// test. On a compressed relay dst is a zstd or s2 encoder, and per this
	// package's own doc a small chunk handed to Write just lands in the
	// encoder's internal buffer -- CopyFlushing reads at most 32KB while
	// those buffers are larger, so Write performs no I/O at all and cannot
	// notice a dead socket. Flush is what pushes bytes out, so Flush is
	// where a broken destination first surfaces. The already-covered Write
	// branch is the one that stays quiet.
	//
	// Getting this wrong is not a silent bad replica -- both encoders latch
	// a write error -- but a failure reported late and against the wrong
	// operation, after a buffer's worth of data has been pumped into a
	// socket that is already gone.
	t.Run("flush error propagates and stops the copy", func(t *testing.T) {
		src := &chunkReader{chunks: [][]byte{[]byte("abc"), []byte("de"), []byte("fghij")}}
		dst := &failingFlushWriter{flushErr: errSimulatedFlush}

		written, err := streamrelay.CopyFlushing(dst, src)
		if err == nil {
			t.Fatal("CopyFlushing returned nil despite every Flush failing")
		}
		if !errors.Is(err, errSimulatedFlush) {
			t.Fatalf("CopyFlushing returned %q, want the destination's flush failure", err)
		}
		// Stopped at the first failure rather than draining the source.
		if dst.writes != 1 {
			t.Errorf("destination saw %d writes, want 1 -- CopyFlushing kept pumping after a failed flush", dst.writes)
		}
		if src.i != 1 {
			t.Errorf("source was read %d times, want 1 -- CopyFlushing kept reading after a failed flush", src.i)
		}
		// The count still reflects what really reached dst.Write before the
		// flush refused, so a caller can tell how far it got.
		if written != 3 {
			t.Errorf("written = %d, want 3 (the first chunk did reach Write)", written)
		}
	})
}

// TestEncoderDecoderRoundTripMultiFrame closes the one gap neither
// TestEncoderDecoderRoundTrip above nor TestRelayCompressRoundTrip below
// actually cover: a stream with more than one internal Flush boundary
// before Close. TestEncoderDecoderRoundTrip never calls Flush at all
// (single Write then Close), and TestRelayCompressRoundTrip's payload
// (9200 bytes) is smaller than CopyFlushing's own 32KB read buffer, so
// despite going through the real CopyFlushing path it's still read and
// flushed in exactly one pass -- effectively single-frame either way. In
// production, CopyFlushing (what Relay actually uses) flushes after every
// nonzero Read from its source, and any real sync moves far more than
// 32KB, so a real compressed stream always has many such boundaries
// before the encoder is finally closed once at the very end -- the one
// shape nothing before this test ever produced or decoded.
//
// Drives a real encoder through CopyFlushing against chunkReader (already
// used by TestCopyFlushing above, against a fake writer -- reused here
// against a real one) to force 20 separate Write+Flush cycles, then
// confirms the decoder reconstructs the exact original content across
// however many internal boundaries that produced.
func TestEncoderDecoderRoundTripMultiFrame(t *testing.T) {
	cases := []struct {
		name  string
		algo  streamrelay.Algo
		level string
	}{
		{"zstd", streamrelay.AlgoZstd, "3"},
		{"s2", streamrelay.AlgoS2, "better"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// Each chunk is uniform but distinct from its neighbors (all
			// bytes = i), so a chunk landing out of order, dropped, or
			// corrupted at a frame boundary shows up unmistakably rather
			// than blending into its neighbors.
			const chunkCount = 20
			const chunkSize = 500
			chunks := make([][]byte, chunkCount)
			var want []byte
			for i := range chunks {
				chunks[i] = bytes.Repeat([]byte{byte(i)}, chunkSize)
				want = append(want, chunks[i]...)
			}
			src := &chunkReader{chunks: chunks}

			var compressed bytes.Buffer
			enc, err := streamrelay.NewEncoder(tc.algo, &compressed, tc.level)
			if err != nil {
				t.Fatalf("NewEncoder(%s): %v", tc.name, err)
			}
			written, err := streamrelay.CopyFlushing(enc, src)
			if err != nil {
				t.Fatalf("CopyFlushing: %v", err)
			}
			if written != int64(len(want)) {
				t.Fatalf("CopyFlushing reported written=%d, want %d", written, len(want))
			}
			if err := enc.Close(); err != nil {
				t.Fatalf("encoder Close: %v", err)
			}

			dec, closeDec, err := streamrelay.NewDecoder(tc.algo, bytes.NewReader(compressed.Bytes()))
			if err != nil {
				t.Fatalf("NewDecoder(%s): %v", tc.name, err)
			}
			defer closeDec()

			decoded, err := io.ReadAll(dec)
			if err != nil {
				t.Fatalf("decoder ReadAll: %v", err)
			}
			if !bytes.Equal(decoded, want) {
				t.Fatalf("multi-frame round trip mismatch for %s: got %d bytes, want %d bytes", tc.name, len(decoded), len(want))
			}
		})
	}
}

// TestRelayCompressRoundTrip sends a known payload through Relay with
// compression enabled (no netbuffer stage) into an in-memory buffer standing
// in for the wire, then feeds that same wire buffer through RelayFromWire,
// for both supported algorithms. This proves the compress-then-decompress
// round trip through the actual Relay/RelayFromWire functions used in
// production, not just the raw NewEncoder/NewDecoder pair exercised above.
func TestRelayCompressRoundTrip(t *testing.T) {
	payload := []byte(strings.Repeat("The quick brown fox jumps over the lazy dog. ", 200))

	cases := []struct {
		name  string
		algo  streamrelay.Algo
		level string
	}{
		{"zstd", streamrelay.AlgoZstd, "3"},
		{"s2", streamrelay.AlgoS2, "better"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var wire bytes.Buffer
			err, returned := runWithDeadline(t, 5*time.Second, func() error {
				return streamrelay.Relay(&wire, bytes.NewReader(payload), true, tc.algo, tc.level, "", "", nil)
			})
			if !returned {
				t.Fatalf("Relay (%s) did not return within 5s", tc.name)
			}
			if err != nil {
				t.Fatalf("Relay (%s) returned error: %v", tc.name, err)
			}

			var final bytes.Buffer
			err, returned = runWithDeadline(t, 5*time.Second, func() error {
				return streamrelay.RelayFromWire(&final, bytes.NewReader(wire.Bytes()), true, tc.algo, "", "", nil)
			})
			if !returned {
				t.Fatalf("RelayFromWire (%s) did not return within 5s", tc.name)
			}
			if err != nil {
				t.Fatalf("RelayFromWire (%s) returned error: %v", tc.name, err)
			}

			if !bytes.Equal(final.Bytes(), payload) {
				t.Fatalf("round trip mismatch for %s: got %d bytes, want %d bytes", tc.name, final.Len(), len(payload))
			}
		})
	}
}

// TestRelayCountsWireBytesOverRealTCPConn pins the invariant that Relay's
// plain (compress=false, no netbuffer) pass-through direction accounts for
// every byte in wireCounter when src is a real *net.TCPConn -- the shape
// that matters, because *net.TCPConn implements io.WriterTo and so sends
// io.Copy down its src.WriteTo(dst) fast path rather than the usual
// read-then-dst.Write loop. A bytes.Buffer/bytes.Reader test (like
// TestRelayCompressRoundTrip above) cannot exercise that at all: neither
// implements the fast-path interfaces a real net.Conn does.
//
// It was originally written believing that fast path was itself the bug --
// that WriteTo bypassed CountingWriter and left the counter at 0 -- and
// Relay carried a wrapper to defeat it. That turned out not to be the
// mechanism (TCPConn.WriteTo falls through to net.genericWriteTo, which
// still calls dst.Write for every chunk; see Relay's own comment), so the
// wrapper is gone. The assertion below is what actually guards the
// invariant, and it is deliberately exact rather than merely non-zero: a
// partial or double count fails it just as loudly as no count at all.
func TestRelayCountsWireBytesOverRealTCPConn(t *testing.T) {
	payload := []byte(strings.Repeat("nbd bridge relay payload data ", 200))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			accepted <- conn
		}
	}()

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	go func() {
		client.Write(payload)
		client.Close()
	}()

	src := <-accepted
	defer src.Close()

	var wire bytes.Buffer
	var sent uint64
	relayErr, returned := runWithDeadline(t, 5*time.Second, func() error {
		return streamrelay.Relay(&wire, src, false, streamrelay.AlgoZstd, "", "", "", &sent)
	})
	if !returned {
		t.Fatal("Relay did not return within 5s")
	}
	if relayErr != nil {
		t.Fatalf("Relay returned error: %v", relayErr)
	}

	if !bytes.Equal(wire.Bytes(), payload) {
		t.Fatalf("relayed bytes mismatch: got %d bytes, want %d bytes", wire.Len(), len(payload))
	}
	if sent == 0 {
		t.Fatal("wireCounter stayed 0 despite relaying real traffic over a genuine net.Conn source -- something between Relay and CountingWriter.Write is not accounting for the bytes")
	}
	if sent != uint64(len(payload)) {
		t.Errorf("wireCounter = %d, want %d (exact payload length)", sent, len(payload))
	}
}

// TestRelayPassesThroughRealTCPConnsWithoutCounter pins the shape Relay's
// no-compression branch must leave on its fastest path: a raw *net.TCPConn
// source relayed to a raw *net.TCPConn destination, with compression off,
// netbuffer off, and no counter -- exactly how cmd/vmsync-bridge-helper
// relays uncompressed traffic in both directions.
//
// Both ends must be real TCP connections for this to mean anything: only
// then is effectiveDst an io.ReaderFrom, which is what lets io.Copy reach
// net.spliceFrom and move the data kernel-to-kernel with no userspace copy.
// spliceFrom type-switches on its reader and splices only for *TCPConn /
// tcpConnWithoutWriteTo / *UnixConn, so interposing any wrapper on src --
// an anonymous struct{ io.Reader }, say -- silently drops this path to a
// 32 KiB buffered copy. That is not something a correctness test would
// otherwise notice, which is why this one exists: it asserts the bytes
// arrive intact whichever path io.Copy takes, so anyone reintroducing a
// wrapper at least has to keep the transfer honest while doing it.
func TestRelayPassesThroughRealTCPConnsWithoutCounter(t *testing.T) {
	payload := []byte(strings.Repeat("uncompressed helper pass-through payload ", 200))

	srcLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen (source side): %v", err)
	}
	defer srcLn.Close()

	srcAccepted := make(chan net.Conn, 1)
	go func() {
		if conn, err := srcLn.Accept(); err == nil {
			srcAccepted <- conn
		}
	}()

	srcWriter, err := net.Dial("tcp", srcLn.Addr().String())
	if err != nil {
		t.Fatalf("dial (source side): %v", err)
	}
	go func() {
		srcWriter.Write(payload)
		srcWriter.Close()
	}()

	src := <-srcAccepted
	defer src.Close()

	// Destination side: whatever Relay writes is drained to completion by the
	// accepting goroutine, so Relay can never block on an unread socket.
	dstLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen (destination side): %v", err)
	}
	defer dstLn.Close()

	received := make(chan []byte, 1)
	go func() {
		conn, err := dstLn.Accept()
		if err != nil {
			received <- nil
			return
		}
		defer conn.Close()
		got, _ := io.ReadAll(conn)
		received <- got
	}()

	dst, err := net.Dial("tcp", dstLn.Addr().String())
	if err != nil {
		t.Fatalf("dial (destination side): %v", err)
	}
	defer dst.Close()

	relayErr, returned := runWithDeadline(t, 5*time.Second, func() error {
		return streamrelay.Relay(dst, src, false, streamrelay.AlgoZstd, "", "", "", nil)
	})
	if !returned {
		t.Fatal("Relay did not return within 5s")
	}
	if relayErr != nil {
		t.Fatalf("Relay returned error: %v", relayErr)
	}

	// Half-close so the draining goroutine's ReadAll sees EOF and returns.
	if tc, ok := dst.(*net.TCPConn); ok {
		tc.CloseWrite()
	}

	select {
	case got := <-received:
		if !bytes.Equal(got, payload) {
			t.Fatalf("relayed %d bytes over a raw TCP->TCP hop, want the original %d bytes intact", len(got), len(payload))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("destination side never finished reading the relayed bytes")
	}
}

var (
	_ io.Reader = (*chunkReader)(nil)
)

// TestDefaultAndResolveLevel pins the per-algorithm level defaults, which
// exist because a single -compress-level flag default cannot be correct for
// both algorithms: "3" is a valid zstd level and an invalid s2 mode, and
// "better" is the reverse.
func TestDefaultAndResolveLevel(t *testing.T) {
	if got, want := streamrelay.DefaultLevel(streamrelay.AlgoS2), "better"; got != want {
		t.Errorf("DefaultLevel(s2) = %q, want %q", got, want)
	}
	if got, want := streamrelay.DefaultLevel(streamrelay.AlgoZstd), "3"; got != want {
		t.Errorf("DefaultLevel(zstd) = %q, want %q", got, want)
	}

	// An explicit level always wins, including one that happens to equal the
	// other algorithm's default -- ResolveLevel decides "was anything
	// chosen", never "is this choice sensible". Validation is a separate
	// step, and conflating them here would silently rewrite an operator's
	// input.
	for _, tc := range []struct {
		algo  streamrelay.Algo
		given string
		want  string
	}{
		{streamrelay.AlgoS2, "", "better"},
		{streamrelay.AlgoS2, "best", "best"},
		{streamrelay.AlgoS2, "default", "default"},
		{streamrelay.AlgoZstd, "", "3"},
		{streamrelay.AlgoZstd, "19", "19"},
		{streamrelay.AlgoZstd, "better", "better"}, // kept, then refused by validation
	} {
		if got := streamrelay.ResolveLevel(tc.algo, tc.given); got != tc.want {
			t.Errorf("ResolveLevel(%q, %q) = %q, want %q", tc.algo, tc.given, got, tc.want)
		}
	}
}

// The invariant the whole change exists for: whatever ResolveLevel returns
// for an unset level must be ACCEPTED by that algorithm.
//
// This is the bug class, stated as a test. Before this, -compress-level
// declared "3" and --help said so, which is a level s2 rejects outright --
// so the one combination an operator reaches by typing bare -compress was
// exactly the one the printed default would have broken. Anyone tempted to
// give the flag a literal default again fails here.
func TestResolvedDefaultLevelIsAcceptedByItsOwnAlgorithm(t *testing.T) {
	for _, algo := range []streamrelay.Algo{streamrelay.AlgoS2, streamrelay.AlgoZstd} {
		level := streamrelay.ResolveLevel(algo, "")
		if _, err := streamrelay.NewEncoder(algo, io.Discard, level); err != nil {
			t.Errorf("the default level %q for %q is not usable by its own encoder: %v", level, algo, err)
		}
	}
}
