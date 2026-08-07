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
				data := (flags & 1) != 0
				if !incremental {
					// Full mode with base:allocation context:
					// 0=allocated,1=hole,2=zero,3=hole+zero
					data = flags == 0 || flags == 2
				}
				if data {
					dirty++
				}
				out = append(out, Extent{Offset: offs, Length: uint64(length), Dirty: data})
				offs += uint64(length)
			}
			return 0
		}, nil)
		if err != nil {
			return nil, 0, 0, fmt.Errorf("block status failed at %d: %w", offset, err)
		}
		offset += chunk
		if offset-lastProgress >= 1024*1024*1024 || offset == uint64(size) {
			trace.Debug("nbd extent scan progress", "export", exportName, "offset", offset, "size", size)
			lastProgress = offset
		}
	}
	trace.Info("nbd extent scan complete", "export", exportName, "extents", len(out), "selected", dirty)
	return out, size, dirty, nil
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
// wins over the other side's "no constraint" sentinel. roleA/roleB label
// the trace output only (e.g. "src"/"dst" for a copy, "source"/"target" for
// a compare).
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
	// hitting the deadline or a check error still frees the buffer, since
	// by that point libnbd has either told us the command is over or its
	// connection is unusable enough that waiting longer serves no purpose.
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
			trace.Warning("nbd: timed out waiting for in-flight commands to settle during cleanup, freeing remaining buffers anyway")
			for i := range slots {
				if slots[i].state != slotFree {
					slots[i].buf.Free()
					slots[i].state = slotFree
				}
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
// because qemu-img compare (the tool this replaces for -verify -verify-fast)
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
	if err := ctx.Err(); err != nil {
		return err
	}
	a, err := nbd.Create()
	if err != nil {
		return fmt.Errorf("create source nbd handle: %w", err)
	}
	defer a.Close()

	b, err := nbd.Create()
	if err != nil {
		return fmt.Errorf("create target nbd handle: %w", err)
	}
	defer b.Close()

	if aExport != "" {
		if err := a.SetExportName(aExport); err != nil {
			return fmt.Errorf("set source export name %s: %w", aExport, err)
		}
	}
	if err := a.ConnectTcp(aHost, strconv.Itoa(aPort)); err != nil {
		return fmt.Errorf("connect source nbd tcp %s:%d: %w", aHost, aPort, err)
	}
	if err := b.ConnectTcp(bHost, strconv.Itoa(bPort)); err != nil {
		return fmt.Errorf("connect target nbd tcp %s:%d: %w", bHost, bPort, err)
	}

	sizeA, err := a.GetSize()
	if err != nil {
		return fmt.Errorf("nbd get source size: %w", err)
	}
	sizeB, err := b.GetSize()
	if err != nil {
		return fmt.Errorf("nbd get target size: %w", err)
	}
	if sizeA != sizeB {
		return fmt.Errorf("image size mismatch: source=%d target=%d", sizeA, sizeB)
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
	var compareErr error

compareLoop:
	for nextChunk < len(chunks) || anyOutstanding() {
		select {
		case <-ctx.Done():
			compareErr = fmt.Errorf("nbd compare cancelled: %w", ctx.Err())
			break compareLoop
		default:
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
			match := bytes.Equal(slots[i].bufA.Slice(), slots[i].bufB.Slice())
			slots[i].bufA.Free()
			slots[i].bufB.Free()
			slots[i].stateA = sideIdle
			slots[i].stateB = sideIdle
			if !match {
				compareErr = fmt.Errorf("images differ: mismatch in chunk at offset=%d length=%d", slots[i].offset, slots[i].length)
				break compareLoop
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
	// an AIO call to stay valid until AioCommandCompleted confirms it.
	// Never compares here, and never overwrites an already-set compareErr
	// with anything found during drain -- the first real error/mismatch
	// found above always wins.
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
			trace.Warning("nbd: timed out waiting for in-flight compare commands to settle during cleanup, freeing remaining buffers anyway")
			for i := range slots {
				if slots[i].stateA != sideIdle {
					slots[i].bufA.Free()
					slots[i].stateA = sideIdle
				}
				if slots[i].stateB != sideIdle {
					slots[i].bufB.Free()
					slots[i].stateB = sideIdle
				}
			}
			break
		}
		a.Poll(10)
		b.Poll(10)
	}

	if compareErr != nil {
		return compareErr
	}

	trace.Info("nbd compare complete", "device", aExport, "bytes", size, "elapsed", time.Since(start).Round(time.Millisecond).String())
	return nil
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
