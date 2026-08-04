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
	"context"
	"fmt"

	"strconv"
	"time"

	"vmsync/pkg/trace"

	nbd "libguestfs.org/libnbd"
)

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

// CopyExtentsTCP copies extents from src to dst, returning the number of
// bytes actually written even when it returns a non-nil error (best-effort,
// reflecting whatever was copied before the failure), so callers can still
// report partial progress (e.g. in metrics) on a failed sync.
func CopyExtentsTCP(ctx context.Context, srcHost string, srcPort int, srcExport string, dstHost string, dstPort int, extents []Extent) (writtenBytes uint64, err error) {
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

	max_dst, _ := dst.GetBlockSize(nbd.SIZE_MAXIMUM)
	trace.Debug("dst block", "size", max_dst)
	max_src, _ := src.GetBlockSize(nbd.SIZE_MAXIMUM)
	trace.Debug("src block", "size", max_src)
	buffer_size := min(max_dst, max_src)
	trace.Debug("use nbd buffer", "size", buffer_size)

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
	// cost twice, serially, per chunk. Keeping a small window of chunks
	// in-flight lets a chunk's write overlap with the next chunk's read,
	// hiding one side's latency behind the other's.
	const pipelineDepth = 4

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
	defer func() {
		// Catches any slot still holding an allocated buffer on an early
		// (error) return -- slots freed via the normal completion path below
		// are marked slotFree first, so this never double-frees one.
		for i := range slots {
			if slots[i].state != slotFree {
				slots[i].buf.Free()
			}
		}
	}()

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

	for nextChunk < len(chunks) || reading > 0 || writing > 0 {
		select {
		case <-ctx.Done():
			return writtenBytes, fmt.Errorf("nbd copy cancelled: %w", ctx.Err())
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
				slots[idx].buf.Free()
				return writtenBytes, fmt.Errorf("source nbd aio_pread offset=%d len=%d: %w", c.offset, c.length, err)
			}
			slots[idx].cookie = cookie
			slots[idx].state = slotReading
			reading++
		}

		if reading > 0 {
			if _, err := src.Poll(10); err != nil {
				return writtenBytes, fmt.Errorf("source nbd poll: %w", err)
			}
		}
		if writing > 0 {
			if _, err := dst.Poll(10); err != nil {
				return writtenBytes, fmt.Errorf("target nbd poll: %w", err)
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
				return writtenBytes, fmt.Errorf("source nbd aio command check offset=%d: %w", slots[i].offset, err)
			}
			if !done {
				continue
			}
			reading--
			if slots[i].opErr != 0 {
				return writtenBytes, fmt.Errorf("source nbd pread offset=%d len=%d: errno %d", slots[i].offset, slots[i].length, slots[i].opErr)
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
				return writtenBytes, fmt.Errorf("target nbd aio_pwrite offset=%d len=%d: %w", slots[idx].offset, slots[idx].length, err)
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
				return writtenBytes, fmt.Errorf("target nbd aio command check offset=%d: %w", slots[i].offset, err)
			}
			if !done {
				continue
			}
			writing--
			if slots[i].opErr != 0 {
				return writtenBytes, fmt.Errorf("target nbd pwrite offset=%d len=%d: errno %d", slots[i].offset, slots[i].length, slots[i].opErr)
			}
			slots[i].buf.Free()
			slots[i].state = slotFree
			writtenBytes += slots[i].length
			logProgress()
		}
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
