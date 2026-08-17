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

package nbdsync

import (
	"bytes"
	"context"
	"fmt"

	"strconv"
	"time"

	"vmsync/pkg/trace"

	nbd "libguestfs.org/libnbd"
)

// defaultMaxBufferSize is the read/write chunk size CopyExtentsTCP falls
// back to when neither NBD server advertises a maximum block size (see its
// use below). 1MiB is a conservative, well-tested chunk size for block
// copies -- large enough to avoid per-chunk overhead dominating, small
// enough that even the default -io-depth (8 chunks in flight) stays at a
// modest 8MiB of buffer memory.
const defaultMaxBufferSize = 1024 * 1024

// maxNegotiatedBufferSize hard-caps whatever negotiateBufferSize would
// otherwise pick from a server's advertised maximum block size. Without
// this, a misconfigured or buggy remote NBD server -- inherently the
// untrusted side of a connection vmsync can reach over SSH to a separate
// host it doesn't control -- advertising an unrealistically large maximum
// block size would make every one of ioDepth's AIO buffers allocate at
// that size: a cgo call into libnbd's native allocator, not Go's own, so a
// failure there (malloc returning NULL, or the OS's OOM-killer stepping in
// once the process actually touches that much memory) aborts the whole
// process immediately, mid-sync, with no Go-level panic/recover able to
// catch it and no chance to clean up. 32MiB comfortably covers every real
// qemu-nbd maximum block size seen in practice while keeping ioDepth *
// bufferSize's worst case bounded to a sane figure (256MiB at the default
// -io-depth of 8) regardless of what any server, well-behaved or not,
// claims to support.
const maxNegotiatedBufferSize = 32 * 1024 * 1024

// clampBufferSize caps size at limit, leaving it unchanged when it's
// already within bounds. Extracted from negotiateBufferSize purely so this
// specific safety cap is directly testable without a live NBD connection.
func clampBufferSize(size, limit uint64) uint64 {
	if size > limit {
		return limit
	}
	return size
}

// noProgressTimeout bounds how long CopyExtentsTCP's and compareTCP's AIO
// pipelines will run without a single read or write completing anywhere
// before concluding the connection is stuck rather than merely slow. A
// half-open TCP connection -- the remote end vanished without a FIN/RST,
// e.g. behind a NAT/firewall that silently dropped the mapping, or after a
// hard network partition -- leaves every in-flight command pending forever:
// Poll never errors and AioCommandCompleted never returns done, so nothing
// in either pipeline would otherwise ever notice. Left unbounded, this
// spins the calling goroutine forever while holding the source's
// block-copy backup job open, blocking any future checkpoint or reinit
// against that domain until the process is killed by hand. 120s is
// generous enough to tolerate one genuinely slow chunk (even a full
// negotiated buffer over a heavily-compressed, high-latency link) without
// tripping on legitimate slowness.
const noProgressTimeout = 120 * time.Second

// stalled reports whether more than timeout has elapsed since lastProgress.
// Extracted so the exact boundary (>= timeout, not >) is directly testable
// without needing a real, artificially-stuck NBD connection.
func stalled(lastProgress, now time.Time, timeout time.Duration) bool {
	return now.Sub(lastProgress) >= timeout
}

type Extent struct {
	Offset uint64
	Length uint64
	Dirty  bool
}

func ChangedExtentsTCP(ctx context.Context, host string, port int, exportName, checkpointName string, incremental bool) ([]Extent, uint64, uint64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, 0, err
	}
	trace.Info("nbd connect for extents", "host", host, "port", port, "export", exportName, "checkpoint", checkpointName, "incremental", incremental)
	h, err := nbd.Create()
	if err != nil {
		return nil, 0, 0, fmt.Errorf("create nbd handle: %w", err)
	}
	defer h.Close()

	ctxName := "base:allocation"
	if incremental {
		ctxName = "qemu:dirty-bitmap:" + checkpointName
	}
	if err := h.AddMetaContext(ctxName); err != nil {
		return nil, 0, 0, fmt.Errorf("add meta context %s: %w", ctxName, err)
	}
	if exportName != "" {
		if err := h.SetExportName(exportName); err != nil {
			return nil, 0, 0, fmt.Errorf("set export name %s: %w", exportName, err)
		}
	}
	if err := h.ConnectTcp(host, strconv.Itoa(port)); err != nil {
		return nil, 0, 0, fmt.Errorf("connect nbd tcp %s:%d: %w", host, port, err)
	}
	trace.Info("nbd connected for extent query", "export", exportName)

	size, err := h.GetSize()
	if err != nil {
		return nil, 0, 0, fmt.Errorf("nbd get size: %w", err)
	}
	trace.Info("nbd export size", "export", exportName, "bytes", size)

	req := uint64(4294967295)
	align := uint64(512)
	if min, err := h.GetBlockSize(nbd.SIZE_MINIMUM); err == nil && min > 0 {
		align = min
	}
	if align > 1 && req >= align {
		req = req - align + 1
	}
	var offset uint64
	var out []Extent
	lastProgress := uint64(0)
	var dirty uint64 = 0
	for offset < uint64(size) {
		select {
		case <-ctx.Done():
			return nil, 0, 0, fmt.Errorf("nbd extent scan cancelled at offset %d: %w", offset, ctx.Err())
		default:
		}

		chunk := req
		if remain := uint64(size) - offset; remain < chunk {
			chunk = remain
		}

		// describedEnd tracks how far the server's reply actually covered --
		// NBD_CMD_BLOCK_STATUS replies are explicitly allowed to describe
		// less than the requested range (e.g. a server-side limit on how
		// many extents fit in one reply), or for the final extent to run
		// past it. Advancing the scan by the raw requested chunk size
		// regardless (as this used to do) either silently drops whatever
		// sub-range the server declined to describe -- those bytes are
		// never re-queried and never copied, with the run still reporting
		// success -- or, in the overrun case, re-queries and duplicates a
		// sub-range already covered. See nextExtentScanOffset below.
		describedEnd := offset
		err = h.BlockStatus(chunk, offset, func(meta string, offs uint64, entries []uint32, cbErr *int) int {
			if cbErr != nil && *cbErr != 0 {
				return -1
			}
			if meta != ctxName {
				return 0
			}
			for i := 0; i+1 < len(entries); i += 2 {
				length := entries[i]
				flags := entries[i+1]
				data := isDirtyExtent(flags, incremental)
				if data {
					dirty++
				}
				out = append(out, Extent{Offset: offs, Length: uint64(length), Dirty: data})
				offs += uint64(length)
			}
			if offs > describedEnd {
				describedEnd = offs
			}
			return 0
		}, nil)
		if err != nil {
			return nil, 0, 0, fmt.Errorf("block status failed at %d: %w", offset, err)
		}
		offset, err = nextExtentScanOffset(offset, chunk, describedEnd)
		if err != nil {
			return nil, 0, 0, err
		}
		if offset-lastProgress >= 1024*1024*1024 || offset == uint64(size) {
			trace.Debug("nbd extent scan progress", "export", exportName, "offset", offset, "size", size)
			lastProgress = offset
		}
	}
	trace.Info("nbd extent scan complete", "export", exportName, "extents", len(out), "selected", dirty)
	return out, size, dirty, nil
}

// isDirtyExtent reports whether an NBD status flags value represents data
// that needs to be copied/compared. For the incremental (qemu:dirty-bitmap:)
// context, bit 0 (NBD_STATE_DIRTY) SET means "changed since the checkpoint".
// For the full (base:allocation) context, bit 0 (NBD_STATE_HOLE) CLEAR means
// "allocated, not a hole" -- tested as a mask against just that one bit,
// per the NBD spec's requirement that clients ignore status bits they don't
// recognize, rather than enumerating the full set of currently-known flag
// combinations (0=allocated, 1=hole, 2=allocated+zero, 3=hole+zero) and
// treating anything outside that set as skippable, which would silently
// drop real data behind any future or non-qemu server that ever sets an
// additional status bit on an otherwise-allocated, non-hole extent.
func isDirtyExtent(flags uint32, incremental bool) bool {
	if incremental {
		return flags&1 != 0
	}
	return flags&1 == 0
}

// nextExtentScanOffset decides the next BLOCK_STATUS query offset after a
// call that requested [requestedOffset, requestedOffset+requestedChunk) and
// whose reply actually covered up to describedEnd. describedEnd <=
// requestedOffset means the reply covered nothing at all (matched no
// context, or reported zero extents) -- reported as an error rather than
// silently continuing, which would otherwise either spin forever (if the
// offset never advances) or, with a blind requestedChunk advance, skip an
// arbitrary amount of the disk without anyone ever knowing.
func nextExtentScanOffset(requestedOffset, requestedChunk, describedEnd uint64) (uint64, error) {
	if describedEnd <= requestedOffset {
		return 0, fmt.Errorf("nbd block status made no progress at offset %d (requested %d bytes, server described none)", requestedOffset, requestedChunk)
	}
	return describedEnd, nil
}

// negotiateBufferSize picks the read/write chunk size to use between two
// already-connected NBD handles, from each side's advertised maximum block
// size (nbd.SIZE_MAXIMUM). GetBlockSize(SIZE_MAXIMUM) legitimately returns 0
// (no error) when a server doesn't advertise a maximum block size at all --
// "unconstrained", not "the limit is 0 bytes". Naively taking min() of the
// two raw values lets an unconstrained side's 0 silently override a REAL,
// smaller constraint the other side actually advertised (min(65536, 0) ==
// 0), discarding it entirely instead of respecting it -- and left
// unguarded altogether, a 0 buffer size makes a chunk-flattening loop spin
// forever (step stays 0, so offsets never advance) before a single byte is
// transferred. Only falls back to defaultMaxBufferSize when *both* sides
// are unconstrained; a real constraint from whichever side has one always
// wins over the other side's "no constraint" sentinel. The result is then
// run through clampBufferSize -- see maxNegotiatedBufferSize's own doc
// comment for why an advertised constraint is trusted to shrink the
// buffer but never to grow it past that cap. roleA/roleB label the trace
// output only (e.g. "src"/"dst" for a copy, "source"/"target" for a
// compare).
func negotiateBufferSize(a, b *nbd.Libnbd, roleA, roleB string) uint64 {
	maxA, _ := a.GetBlockSize(nbd.SIZE_MAXIMUM)
	trace.Debug(roleA+" block", "size", maxA)
	maxB, _ := b.GetBlockSize(nbd.SIZE_MAXIMUM)
	trace.Debug(roleB+" block", "size", maxB)
	bufferSize := min(maxA, maxB)
	switch {
	case maxA == 0 && maxB == 0:
		bufferSize = defaultMaxBufferSize
	case maxA == 0:
		bufferSize = maxB
	case maxB == 0:
		bufferSize = maxA
	}
	bufferSize = clampBufferSize(bufferSize, maxNegotiatedBufferSize)
	trace.Debug("use nbd buffer", "size", bufferSize)
	return bufferSize
}

// CopyExtentsTCP copies extents from src to dst, returning the number of
// bytes actually written even when it returns a non-nil error (best-effort,
// reflecting whatever was copied before the failure), so callers can still
// report partial progress (e.g. in metrics) on a failed sync. ioDepth is the
// number of read/write pairs kept in flight simultaneously (see the
// pipelining comment below); values less than 1 are clamped to 1.
func CopyExtentsTCP(ctx context.Context, srcHost string, srcPort int, srcExport string, dstHost string, dstPort int, extents []Extent, ioDepth int) (writtenBytes uint64, err error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	src, err := nbd.Create()
	if err != nil {
		return 0, fmt.Errorf("create source nbd handle: %w", err)
	}
	defer src.Close()

	dst, err := nbd.Create()
	if err != nil {
		return 0, fmt.Errorf("create target nbd handle: %w", err)
	}
	defer dst.Close()

	if srcExport != "" {
		if err := src.SetExportName(srcExport); err != nil {
			return 0, fmt.Errorf("set source export name %s: %w", srcExport, err)
		}
	}
	if err := src.ConnectTcp(srcHost, strconv.Itoa(srcPort)); err != nil {
		return 0, fmt.Errorf("connect source nbd tcp %s:%d: %w", srcHost, srcPort, err)
	}

	if err := dst.ConnectTcp(dstHost, strconv.Itoa(dstPort)); err != nil {
		return 0, fmt.Errorf("connect target nbd tcp %s:%d: %w", dstHost, dstPort, err)
	}

	buffer_size := negotiateBufferSize(src, dst, "src", "dst")

	totalBytes := uint64(0)
	for _, ex := range extents {
		if ex.Dirty && ex.Length > 0 {
			totalBytes += ex.Length
		}
	}

	// Flatten extents into a flat list of buffer_size-capped (offset, length)
	// chunks up front, so the pipeline below runs over one simple sequence
	// instead of juggling extent boundaries mid-flight.
	type copyChunk struct {
		offset uint64
		length uint64
	}
	var chunks []copyChunk
	for _, ex := range extents {
		if !ex.Dirty || ex.Length == 0 {
			continue
		}
		remaining := ex.Length
		cur := ex.Offset
		for remaining > 0 {
			step := buffer_size
			if remaining < step {
				step = remaining
			}
			chunks = append(chunks, copyChunk{offset: cur, length: step})
			cur += step
			remaining -= step
		}
	}

	start := time.Now()
	lastLog := start
	lastProgress := start

	// Pipeline reads and writes instead of doing them strictly one at a time:
	// on local/low-latency links the dominant per-chunk cost isn't network
	// bandwidth, it's the round-trip itself (real disk I/O on both ends, NBD
	// protocol framing) -- a synchronous Pread-then-Pwrite loop pays that
	// cost twice, serially, per chunk. Keeping a window of ioDepth chunks
	// in-flight lets a chunk's write overlap with the next chunk's read,
	// hiding one side's latency behind the other's.
	pipelineDepth := ioDepth
	if pipelineDepth < 1 {
		pipelineDepth = 1
	}

	const (
		slotFree = iota
		slotReading
		slotWriting
	)
	type slot struct {
		state  int
		buf    nbd.AioBuffer
		offset uint64
		length uint64
		cookie uint64
		opErr  int
	}
	slots := make([]slot, pipelineDepth)

	nextChunk := 0
	reading, writing := 0, 0

	logProgress := func() {
		now := time.Now()
		if now.Sub(lastLog) < time.Second && writtenBytes != totalBytes {
			return
		}
		elapsed := now.Sub(start).Seconds()
		if elapsed <= 0 {
			elapsed = 0.001
		}
		percent := 100.0
		if totalBytes > 0 {
			percent = (float64(writtenBytes) / float64(totalBytes)) * 100.0
		}
		mibPerSec := (float64(writtenBytes) / (1024.0 * 1024.0)) / elapsed
		trace.Info(fmt.Sprintf("nbd: copy progress (%s)  %.2f%% (%d/%d bytes) %.2f MiB/s", srcExport, percent, writtenBytes, totalBytes, mibPerSec))
		lastLog = now
	}

	// copyErr carries an abort reason out of the loop below instead of
	// returning directly from inside it, so every exit path -- normal
	// completion or an aborted one -- flows through the drain step right
	// after the loop before this function returns. That matters because
	// libnbd requires a buffer passed to AioPread/AioPwrite to stay valid
	// until its command is confirmed complete (AioCommandCompleted); on an
	// early abort, other slots can still have genuinely in-flight commands,
	// and freeing their buffers immediately (as an old version of this
	// function did, relying on a deferred cleanup that ran before the
	// connections were even closed) is a use-after-free race against
	// whatever libnbd is still doing with that memory.
	var copyErr error

copyLoop:
	for nextChunk < len(chunks) || reading > 0 || writing > 0 {
		select {
		case <-ctx.Done():
			copyErr = fmt.Errorf("nbd copy cancelled: %w", ctx.Err())
			break copyLoop
		default:
		}
		if stalled(lastProgress, time.Now(), noProgressTimeout) {
			copyErr = fmt.Errorf("nbd copy stalled: no read or write completed in over %s -- source or target connection may be half-open", noProgressTimeout)
			break copyLoop
		}

		// Fill any free slot with the next chunk's read.
		for i := range slots {
			if slots[i].state != slotFree || nextChunk >= len(chunks) {
				continue
			}
			c := chunks[nextChunk]
			nextChunk++
			idx := i
			slots[idx].buf = nbd.MakeAioBuffer(uint(c.length))
			slots[idx].offset = c.offset
			slots[idx].length = c.length
			slots[idx].opErr = 0
			cookie, err := src.AioPread(slots[idx].buf, c.offset, &nbd.AioPreadOptargs{
				CompletionCallbackSet: true,
				CompletionCallback: func(errp *int) int {
					if errp != nil {
						slots[idx].opErr = *errp
					}
					return 0
				},
			})
			if err != nil {
				// Never actually issued -- libnbd never took ownership of
				// this buffer, so freeing it immediately (rather than via
				// the drain below) is safe.
				slots[idx].buf.Free()
				copyErr = fmt.Errorf("source nbd aio_pread offset=%d len=%d: %w", c.offset, c.length, err)
				break copyLoop
			}
			slots[idx].cookie = cookie
			slots[idx].state = slotReading
			reading++
		}

		if reading > 0 {
			if _, err := src.Poll(10); err != nil {
				copyErr = fmt.Errorf("source nbd poll: %w", err)
				break copyLoop
			}
		}
		if writing > 0 {
			if _, err := dst.Poll(10); err != nil {
				copyErr = fmt.Errorf("target nbd poll: %w", err)
				break copyLoop
			}
		}

		// Reads that finished: check their result, then hand the same
		// buffer straight to the target write (no Go-side copy needed).
		for i := range slots {
			if slots[i].state != slotReading {
				continue
			}
			done, err := src.AioCommandCompleted(slots[i].cookie)
			if err != nil {
				copyErr = fmt.Errorf("source nbd aio command check offset=%d: %w", slots[i].offset, err)
				break copyLoop
			}
			if !done {
				continue
			}
			lastProgress = time.Now()
			reading--
			if slots[i].opErr != 0 {
				// Confirmed complete (done == true) even though it failed,
				// so per libnbd's contract the buffer is safe to free now.
				slots[i].buf.Free()
				slots[i].state = slotFree
				copyErr = fmt.Errorf("source nbd pread offset=%d len=%d: errno %d", slots[i].offset, slots[i].length, slots[i].opErr)
				break copyLoop
			}
			idx := i
			slots[idx].opErr = 0
			cookie, err := dst.AioPwrite(slots[idx].buf, slots[idx].offset, &nbd.AioPwriteOptargs{
				CompletionCallbackSet: true,
				CompletionCallback: func(errp *int) int {
					if errp != nil {
						slots[idx].opErr = *errp
					}
					return 0
				},
			})
			if err != nil {
				// The read into this buffer already completed (just
				// confirmed above) and this write was never issued, so
				// libnbd holds no reference to the buffer either way --
				// freeing it now is safe.
				slots[idx].buf.Free()
				slots[idx].state = slotFree
				copyErr = fmt.Errorf("target nbd aio_pwrite offset=%d len=%d: %w", slots[idx].offset, slots[idx].length, err)
				break copyLoop
			}
			slots[idx].cookie = cookie
			slots[idx].state = slotWriting
			writing++
		}

		// Writes that finished: the chunk is now durable-pending on the
		// target (still needs the final dst.Flush below), free its buffer
		// and count it.
		for i := range slots {
			if slots[i].state != slotWriting {
				continue
			}
			done, err := dst.AioCommandCompleted(slots[i].cookie)
			if err != nil {
				copyErr = fmt.Errorf("target nbd aio command check offset=%d: %w", slots[i].offset, err)
				break copyLoop
			}
			if !done {
				continue
			}
			lastProgress = time.Now()
			writing--
			if slots[i].opErr != 0 {
				slots[i].buf.Free()
				slots[i].state = slotFree
				copyErr = fmt.Errorf("target nbd pwrite offset=%d len=%d: errno %d", slots[i].offset, slots[i].length, slots[i].opErr)
				break copyLoop
			}
			slots[i].buf.Free()
			slots[i].state = slotFree
			writtenBytes += slots[i].length
			logProgress()
		}
	}

	// Wait for every slot the loop above left in flight (slotReading or
	// slotWriting) to genuinely settle before touching its buffer -- this
	// is what makes the early-abort paths above safe, since none of them
	// free any OTHER slot's buffer themselves. Bounded so a truly dead
	// connection (where neither Poll nor AioCommandCompleted ever errors,
	// but nothing ever completes either) can't hang this function forever;
	// hitting a check error still frees the buffer, since libnbd itself
	// told us the command is over. Hitting the deadline does NOT free the
	// remaining buffers, though -- see the timeout branch below for why.
	drainDeadline := time.Now().Add(30 * time.Second)
	for {
		pending := false
		for i := range slots {
			if slots[i].state == slotFree {
				continue
			}
			h := src
			if slots[i].state == slotWriting {
				h = dst
			}
			done, derr := h.AioCommandCompleted(slots[i].cookie)
			if derr != nil {
				trace.Warning("nbd: could not confirm in-flight command completion during cleanup, freeing buffer anyway", "offset", slots[i].offset, "error", derr)
				slots[i].buf.Free()
				slots[i].state = slotFree
				continue
			}
			if done {
				// A write slot confirmed complete here genuinely landed on
				// the target -- the only reason its completion is being
				// observed here instead of the main loop above is that
				// copyLoop broke out (on some other slot's error) before
				// this one's own turn came up, not that anything is wrong
				// with this particular write. Credit it the same way the
				// main loop does, or writtenBytes silently undercounts real
				// progress on a failed sync, contradicting this function's
				// own documented contract. A slotReading completion has
				// nothing to credit -- its write was never even issued.
				if slots[i].state == slotWriting && slots[i].opErr == 0 {
					writtenBytes += slots[i].length
				}
				slots[i].buf.Free()
				slots[i].state = slotFree
				continue
			}
			pending = true
		}
		if !pending {
			break
		}
		if time.Now().After(drainDeadline) {
			// Do NOT free these buffers. Reaching the deadline means
			// AioCommandCompleted never confirmed one way or the other --
			// libnbd's C side may still consider this buffer live, still
			// waiting on a connection that's very slow rather than
			// genuinely dead. Freeing it here and having the command
			// actually complete afterward would be a native use-after-free
			// the moment libnbd writes the result into memory Go has
			// already released. Abandoning (leaking) these few in-flight
			// buffers is the safe tradeoff: this function is about to
			// return a fatal error either way, ending the sync this
			// connection belongs to, so the leak's lifetime is bounded by
			// however much longer the process keeps running.
			trace.Warning("nbd: timed out waiting for in-flight commands to settle during cleanup, abandoning remaining buffers without freeing them to avoid a use-after-free if they later complete")
			for i := range slots {
				slots[i].state = slotFree
			}
			break
		}
		src.Poll(10)
		dst.Poll(10)
	}

	if copyErr != nil {
		return writtenBytes, copyErr
	}

	if err := dst.Flush(nil); err != nil {
		return writtenBytes, fmt.Errorf("flush target nbd: %w", err)
	}
	elapsed := time.Since(start)
	avgMibPerSec := 0.0
	if elapsed.Seconds() > 0 {
		avgMibPerSec = (float64(writtenBytes) / (1024.0 * 1024.0)) / elapsed.Seconds()
	}
	trace.Info("nbd copy complete", "written_bytes", writtenBytes, "device", srcExport, "elapsed", elapsed.Round(time.Millisecond).String(), "avg_mib_per_sec", fmt.Sprintf("%.2f", avgMibPerSec))
	return writtenBytes, nil
}

// CompareTCP does a full, byte-for-byte comparison of two NBD exports (a and
// b), pipelining ioDepth read pairs concurrently via libnbd's AIO API --
// the same approach CopyExtentsTCP uses for the copy direction, ported to a
// symmetric read/read/compare workload instead of read/write. This exists
// because qemu-img compare (the tool this replaces for -verify=fast)
// reads one chunk at a time, synchronously, on both images before advancing
// -- round-trip-latency-bound, not bandwidth-bound, so it can never benefit
// from vmsync's own compress/netbuffer bridge. Pipelining fixes that by
// keeping the link busy with multiple outstanding reads, which is also what
// then makes compression/buffering actually matter.
//
// Deliberately compares the *entire* [0, size) range, with no block-status-
// based skipping of regions unallocated on both sides (unlike qemu-img
// compare) -- correctness takes priority over matching that sparse-image
// shortcut. Returns nil only if every byte matches; otherwise a descriptive
// error, on the first mismatch this pipeline happens to detect -- under
// pipelining, chunks can complete out of order, so the reported offset is
// *a* mismatch, not necessarily the file's lowest-offset one.
//
// Unlike CopyExtentsTCP's single buffer per slot (a completed read's buffer
// flows straight into the write that reuses it), each slot here holds two
// independent buffers that must both be read before either can be freed --
// whichever side finishes first has to hold its buffer open, unread by the
// caller, until its pipeline partner also completes and the two can finally
// be compared. That doubles the memory footprint per slot relative to
// CopyExtentsTCP for the same ioDepth (2 * ioDepth * negotiated buffer
// size), which is why ioDepth isn't given a separate, larger default here.
func CompareTCP(ctx context.Context, aHost string, aPort int, aExport string, bHost string, bPort int, ioDepth int) error {
	_, err := compareTCP(ctx, aHost, aPort, aExport, bHost, bPort, ioDepth, false)
	return err
}

// MismatchRange is a byte range where source and target bytes differed.
// Deliberately distinct from Extent (which carries Dirty/allocation
// semantics that don't apply here), so a caller can't accidentally conflate
// a mismatch range with a ChangedExtentsTCP dirty-bitmap range.
type MismatchRange struct {
	Offset uint64
	Length uint64
}

// mismatchScanGranularity is the sub-block size diffSubRanges scans at once a
// whole AIO chunk (up to tens of MiB) has already been found to differ
// somewhere. It operates purely on in-memory buffers already read into RAM,
// not on new NBD requests, so no protocol-level block-alignment requirement
// applies here -- unlike the request-size chosen elsewhere in this file,
// this value only trades comparison precision against CPU time and can be
// any value.
const mismatchScanGranularity = 4096

// diffSubRanges scans two equal-length buffers in mismatchScanGranularity
// chunks and returns the precise sub-ranges (relative to baseOffset) where
// they actually differ, merging adjacent differing chunks into a single
// range. Without this, compareTCP would have to report a mismatch anywhere
// inside a buffer as spanning the buffer's *entire* AIO chunk -- and
// -verify=online's dirty-bitmap reconciliation (overlapsAnyExtent) discards
// a whole MismatchRange if it overlaps a guest write anywhere in it, so an
// unrelated write next to real corruption elsewhere in the same wide chunk
// would silently hide that corruption. Called only after a whole-buffer
// bytes.Equal has already confirmed a difference exists, so the returned
// slice is never empty for well-formed equal-length inputs.
func diffSubRanges(baseOffset uint64, a, b []byte, granularity uint64) []MismatchRange {
	var out []MismatchRange
	n := uint64(len(a))
	var runStart uint64
	inRun := false
	for pos := uint64(0); pos < n; pos += granularity {
		end := pos + granularity
		if end > n {
			end = n
		}
		if !bytes.Equal(a[pos:end], b[pos:end]) {
			if !inRun {
				runStart = pos
				inRun = true
			}
		} else if inRun {
			out = append(out, MismatchRange{Offset: baseOffset + runStart, Length: pos - runStart})
			inRun = false
		}
	}
	if inRun {
		out = append(out, MismatchRange{Offset: baseOffset + runStart, Length: n - runStart})
	}
	return out
}

// CompareTCPCollect is CompareTCP, except it scans the entire image even
// past the first mismatch, returning every mismatched range instead of
// aborting on the first one. For -verify=online, where a lone mismatch is
// inconclusive on its own (the guest may have legitimately written there
// during the compare) and must be cross-referenced against a dirty bitmap
// afterward -- which needs every mismatch, not just the first. A genuine
// I/O/protocol error (never a data mismatch) still aborts immediately, same
// as CompareTCP; mismatches reflects whatever was collected before such an
// abort, mirroring CopyExtentsTCP's own "return partial progress on error"
// contract.
func CompareTCPCollect(ctx context.Context, aHost string, aPort int, aExport string, bHost string, bPort int, ioDepth int) ([]MismatchRange, error) {
	return compareTCP(ctx, aHost, aPort, aExport, bHost, bPort, ioDepth, true)
}

// compareTCP is the shared implementation behind CompareTCP and
// CompareTCPCollect. With collectMismatches false, it's byte-for-byte
// CompareTCP's original behavior: the first mismatch aborts immediately and
// is the sole error. With it true, mismatches are appended to the returned
// slice and the scan continues to the end of the image.
func compareTCP(ctx context.Context, aHost string, aPort int, aExport string, bHost string, bPort int, ioDepth int, collectMismatches bool) (mismatches []MismatchRange, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	a, err := nbd.Create()
	if err != nil {
		return nil, fmt.Errorf("create source nbd handle: %w", err)
	}
	defer a.Close()

	b, err := nbd.Create()
	if err != nil {
		return nil, fmt.Errorf("create target nbd handle: %w", err)
	}
	defer b.Close()

	if aExport != "" {
		if err := a.SetExportName(aExport); err != nil {
			return nil, fmt.Errorf("set source export name %s: %w", aExport, err)
		}
	}
	if err := a.ConnectTcp(aHost, strconv.Itoa(aPort)); err != nil {
		return nil, fmt.Errorf("connect source nbd tcp %s:%d: %w", aHost, aPort, err)
	}
	if err := b.ConnectTcp(bHost, strconv.Itoa(bPort)); err != nil {
		return nil, fmt.Errorf("connect target nbd tcp %s:%d: %w", bHost, bPort, err)
	}

	sizeA, err := a.GetSize()
	if err != nil {
		return nil, fmt.Errorf("nbd get source size: %w", err)
	}
	sizeB, err := b.GetSize()
	if err != nil {
		return nil, fmt.Errorf("nbd get target size: %w", err)
	}
	if sizeA != sizeB {
		return nil, fmt.Errorf("image size mismatch: source=%d target=%d", sizeA, sizeB)
	}
	size := sizeA

	bufferSize := negotiateBufferSize(a, b, "source", "target")

	type compareChunk struct {
		offset uint64
		length uint64
	}
	var chunks []compareChunk
	for offset := uint64(0); offset < size; {
		step := bufferSize
		if remain := size - offset; remain < step {
			step = remain
		}
		chunks = append(chunks, compareChunk{offset: offset, length: step})
		offset += step
	}

	pipelineDepth := ioDepth
	if pipelineDepth < 1 {
		pipelineDepth = 1
	}

	const (
		sideIdle = iota
		sidePending
		sideReady
	)
	// Each side of a slot moves independently through idle -> pending ->
	// ready: idle means no buffer is live; pending means an AIO read is in
	// flight and the buffer must not be freed yet; ready means the read
	// completed successfully and the buffer holds valid data, held open
	// (not freed) until the *other* side of the same slot also reaches
	// ready, at which point the two get compared and both freed together.
	type slot struct {
		offset, length uint64

		bufA    nbd.AioBuffer
		stateA  int
		cookieA uint64
		errA    int

		bufB    nbd.AioBuffer
		stateB  int
		cookieB uint64
		errB    int
	}
	slots := make([]slot, pipelineDepth)

	nextChunk := 0
	anyOutstanding := func() bool {
		for i := range slots {
			if slots[i].stateA != sideIdle || slots[i].stateB != sideIdle {
				return true
			}
		}
		return false
	}

	start := time.Now()
	lastProgress := start
	var compareErr error

compareLoop:
	for nextChunk < len(chunks) || anyOutstanding() {
		select {
		case <-ctx.Done():
			compareErr = fmt.Errorf("nbd compare cancelled: %w", ctx.Err())
			break compareLoop
		default:
		}
		if stalled(lastProgress, time.Now(), noProgressTimeout) {
			compareErr = fmt.Errorf("nbd compare stalled: no read completed in over %s -- source or target connection may be half-open", noProgressTimeout)
			break compareLoop
		}

		// Fill any fully-idle slot with the next chunk's paired reads.
		for i := range slots {
			if slots[i].stateA != sideIdle || slots[i].stateB != sideIdle || nextChunk >= len(chunks) {
				continue
			}
			c := chunks[nextChunk]
			nextChunk++
			idx := i
			slots[idx].offset = c.offset
			slots[idx].length = c.length
			slots[idx].errA = 0
			slots[idx].errB = 0

			slots[idx].bufA = nbd.MakeAioBuffer(uint(c.length))
			cookieA, err := a.AioPread(slots[idx].bufA, c.offset, &nbd.AioPreadOptargs{
				CompletionCallbackSet: true,
				CompletionCallback: func(errp *int) int {
					if errp != nil {
						slots[idx].errA = *errp
					}
					return 0
				},
			})
			if err != nil {
				// Never actually issued -- libnbd never took ownership,
				// safe to free immediately.
				slots[idx].bufA.Free()
				compareErr = fmt.Errorf("source nbd aio_pread offset=%d len=%d: %w", c.offset, c.length, err)
				break compareLoop
			}
			slots[idx].cookieA = cookieA
			slots[idx].stateA = sidePending

			slots[idx].bufB = nbd.MakeAioBuffer(uint(c.length))
			cookieB, err := b.AioPread(slots[idx].bufB, c.offset, &nbd.AioPreadOptargs{
				CompletionCallbackSet: true,
				CompletionCallback: func(errp *int) int {
					if errp != nil {
						slots[idx].errB = *errp
					}
					return 0
				},
			})
			if err != nil {
				// bufB was never handed to libnbd -- safe to free right
				// away. bufA, though, is now genuinely in flight (issued
				// just above): it must NOT be freed here, only once its own
				// completion is confirmed, below or in the drain phase.
				slots[idx].bufB.Free()
				compareErr = fmt.Errorf("target nbd aio_pread offset=%d len=%d: %w", c.offset, c.length, err)
				break compareLoop
			}
			slots[idx].cookieB = cookieB
			slots[idx].stateB = sidePending
		}

		pendingA, pendingB := false, false
		for i := range slots {
			if slots[i].stateA == sidePending {
				pendingA = true
			}
			if slots[i].stateB == sidePending {
				pendingB = true
			}
		}
		if pendingA {
			if _, err := a.Poll(10); err != nil {
				compareErr = fmt.Errorf("source nbd poll: %w", err)
				break compareLoop
			}
		}
		if pendingB {
			if _, err := b.Poll(10); err != nil {
				compareErr = fmt.Errorf("target nbd poll: %w", err)
				break compareLoop
			}
		}

		// Reads finished on the source side.
		for i := range slots {
			if slots[i].stateA != sidePending {
				continue
			}
			done, err := a.AioCommandCompleted(slots[i].cookieA)
			if err != nil {
				compareErr = fmt.Errorf("source nbd aio command check offset=%d: %w", slots[i].offset, err)
				break compareLoop
			}
			if !done {
				continue
			}
			lastProgress = time.Now()
			if slots[i].errA != 0 {
				// Confirmed complete (done == true) even though it failed,
				// so per libnbd's contract the buffer is safe to free now.
				slots[i].bufA.Free()
				slots[i].stateA = sideIdle
				compareErr = fmt.Errorf("source nbd pread offset=%d len=%d: errno %d", slots[i].offset, slots[i].length, slots[i].errA)
				break compareLoop
			}
			slots[i].stateA = sideReady
		}

		// Reads finished on the target side.
		for i := range slots {
			if slots[i].stateB != sidePending {
				continue
			}
			done, err := b.AioCommandCompleted(slots[i].cookieB)
			if err != nil {
				compareErr = fmt.Errorf("target nbd aio command check offset=%d: %w", slots[i].offset, err)
				break compareLoop
			}
			if !done {
				continue
			}
			lastProgress = time.Now()
			if slots[i].errB != 0 {
				slots[i].bufB.Free()
				slots[i].stateB = sideIdle
				compareErr = fmt.Errorf("target nbd pread offset=%d len=%d: errno %d", slots[i].offset, slots[i].length, slots[i].errB)
				break compareLoop
			}
			slots[i].stateB = sideReady
		}

		// Slots where both sides are now ready: compare and free.
		for i := range slots {
			if slots[i].stateA != sideReady || slots[i].stateB != sideReady {
				continue
			}
			bufSliceA := slots[i].bufA.Slice()
			bufSliceB := slots[i].bufB.Slice()
			match := bytes.Equal(bufSliceA, bufSliceB)
			offset, length := slots[i].offset, slots[i].length
			var subRanges []MismatchRange
			if !match {
				// Precise sub-ranges must be computed before Free() --
				// bufSliceA/bufSliceB become invalid once the underlying AIO
				// buffers are released.
				subRanges = diffSubRanges(offset, bufSliceA, bufSliceB, mismatchScanGranularity)
				if len(subRanges) == 0 {
					// Defensive: should be unreachable given match == false,
					// but silently reporting nothing for a confirmed
					// mismatch would be exactly the bug this is fixing, so
					// fall back to the whole chunk rather than drop it.
					subRanges = []MismatchRange{{Offset: offset, Length: length}}
				}
			}
			slots[i].bufA.Free()
			slots[i].bufB.Free()
			slots[i].stateA = sideIdle
			slots[i].stateB = sideIdle
			if !match {
				if !collectMismatches {
					first := subRanges[0]
					compareErr = fmt.Errorf("images differ: mismatch at offset=%d length=%d (within chunk offset=%d length=%d)", first.Offset, first.Length, offset, length)
					break compareLoop
				}
				mismatches = append(mismatches, subRanges...)
			}
		}
	}

	// A side already confirmed complete and held for comparison (sideReady)
	// when the loop above aborted never gets compared -- just cleaned up.
	for i := range slots {
		if slots[i].stateA == sideReady {
			slots[i].bufA.Free()
			slots[i].stateA = sideIdle
		}
		if slots[i].stateB == sideReady {
			slots[i].bufB.Free()
			slots[i].stateB = sideIdle
		}
	}

	// Wait out whatever's still genuinely in flight (sidePending) before
	// touching its buffer, same reasoning and bounded deadline as
	// CopyExtentsTCP's own drain phase: libnbd requires a buffer passed to
	// an AIO call to stay valid until AioCommandCompleted confirms it, so
	// hitting the deadline below does NOT free the remaining buffers --
	// see CopyExtentsTCP's own timeout branch for why. Never compares
	// here, and never overwrites an already-set compareErr with anything
	// found during drain -- the first real error/mismatch found above
	// always wins.
	drainDeadline := time.Now().Add(30 * time.Second)
	for {
		pending := false
		for i := range slots {
			if slots[i].stateA == sidePending {
				done, derr := a.AioCommandCompleted(slots[i].cookieA)
				switch {
				case derr != nil:
					trace.Warning("nbd: could not confirm in-flight source command completion during cleanup, freeing buffer anyway", "offset", slots[i].offset, "error", derr)
					slots[i].bufA.Free()
					slots[i].stateA = sideIdle
				case done:
					slots[i].bufA.Free()
					slots[i].stateA = sideIdle
				default:
					pending = true
				}
			}
			if slots[i].stateB == sidePending {
				done, derr := b.AioCommandCompleted(slots[i].cookieB)
				switch {
				case derr != nil:
					trace.Warning("nbd: could not confirm in-flight target command completion during cleanup, freeing buffer anyway", "offset", slots[i].offset, "error", derr)
					slots[i].bufB.Free()
					slots[i].stateB = sideIdle
				case done:
					slots[i].bufB.Free()
					slots[i].stateB = sideIdle
				default:
					pending = true
				}
			}
		}
		if !pending {
			break
		}
		if time.Now().After(drainDeadline) {
			// Do NOT free these buffers -- see CopyExtentsTCP's own
			// timeout branch for why: a command AioCommandCompleted never
			// confirmed one way or the other could still be genuinely
			// in flight on a very slow (not dead) connection, and freeing
			// its buffer now risks a native use-after-free if it later
			// completes. Abandoning them is the safe tradeoff, bounded by
			// this function returning a fatal error right after.
			trace.Warning("nbd: timed out waiting for in-flight compare commands to settle during cleanup, abandoning remaining buffers without freeing them to avoid a use-after-free if they later complete")
			for i := range slots {
				slots[i].stateA = sideIdle
				slots[i].stateB = sideIdle
			}
			break
		}
		a.Poll(10)
		b.Poll(10)
	}

	if compareErr != nil {
		return mismatches, compareErr
	}

	trace.Info("nbd compare complete", "device", aExport, "bytes", size, "mismatches", len(mismatches), "elapsed", time.Since(start).Round(time.Millisecond).String())
	return mismatches, nil
}

func WaitForTCPExport(host string, port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		h, err := nbd.Create()
		if err == nil {
			err = h.ConnectTcp(host, strconv.Itoa(port))
			h.Close()
			if err == nil {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("nbd export not ready on %s:%d", host, port)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
