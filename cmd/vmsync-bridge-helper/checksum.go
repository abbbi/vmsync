/*
	Copyright (C) 2026  Orsiris de Jong <ozy@netpower.fr>

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

package main

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"vmsync/pkg/blockdigest"
	"vmsync/pkg/nbdclient"
	"vmsync/pkg/trace"
)

// The helper's second mode: hash the target's own disk content, locally.
//
// This is what makes the pre-commit integrity check affordable. The
// alternative is for vmsync to pull the written extents back across the
// network and hash them itself, which on a full sync means transferring the
// whole disk a second time -- roughly doubling a -reinit. Computed here, on
// the host that already has the bytes, what crosses the network is one short
// line per megabyte instead: ~41 KB of digests for a 10 GiB disk.
//
// It reads through NBD rather than off the qcow2 file directly, using
// pkg/nbdclient's pure-Go client. Both halves of that matter. Reading via
// NBD is required for correctness: two qcow2 images with identical guest
// content differ byte-for-byte, so only a format-aware read can be compared
// against the source. And the client being pure Go is required for
// deployment: this binary is copied to every target host in an estate by
// hand, and linking libnbd (as cmd/vmsync does) would turn that into a
// compiled-dependency problem.
//
// Unlike the relay mode this is one-shot: read a request from stdin, print a
// response to stdout, exit. It never listens, and it dials only the address
// vmsync tells it to -- normally loopback, since the whole point is that the
// bytes are already on this host.

// checksumConfig is the resolved configuration for one checksum pass.
//
// A struct for the same reason helperConfig is one: a transposed pair of
// same-typed values would compile and then fail at runtime in a way that
// reads like a data mismatch rather than a configuration mistake -- the
// single worst outcome for a feature whose entire job is to report data
// mismatches.
//
// Note what is NOT here: the block size. It arrives in the request header,
// because vmsync dictates it and this side obeys. That is deliberately
// stronger than having both sides configured and checking they agree -- with
// one source of the value they cannot disagree at all.
type checksumConfig struct {
	// NBDAddr is the host:port of the qemu-nbd export to read.
	NBDAddr string
	// Export is the NBD export name to ask for. Every export vmsync creates
	// is named, and asking by name is what makes connecting to a stale
	// export from an earlier run fail the handshake instead of silently
	// hashing the wrong disk.
	Export string
	// Timeout bounds each individual socket operation, not the pass as a
	// whole -- a large disk legitimately takes many round trips.
	Timeout time.Duration
	// Progress is where per-minute progress lines go, or nil for none.
	//
	// Separate from out, which carries the digest response and must contain
	// nothing else. In production this is stderr, which vmsync reads line by
	// line while the pass runs -- the only way the side that WAITS can say
	// anything about how the side that WORKS is getting on. A verify of a
	// terabyte hashes here for the better part of an hour, and with nothing
	// on this writer the whole of it was invisible: vmsync logged that it
	// had asked, and then either the answer or, if the run died first,
	// nothing at all.
	Progress io.Writer
	// Interval is how often the beat below reports, or zero for
	// trace.ProgressInterval.
	//
	// Settable only so a test can watch a pass that is mid-block without
	// waiting a minute for the evidence -- the cadence vmsync's own bound is
	// a multiple of is the default, and nothing in main.go sets this. A
	// deployment cannot change it either: vmsync refuses to run against a
	// helper that is not its own version, so the two always agree about the
	// clock by construction.
	Interval time.Duration
}

// runChecksum reads a digest request from in, hashes the named ranges off the
// NBD export, and writes a digest response to out.
//
// Errors go to the caller rather than to out: stdout carries only a response,
// so that a failure can never be mistaken for a short but valid one. That is
// also why the response is written in one go at the end instead of streamed
// -- a pass that died halfway would otherwise leave a truncated, well-formed
// digest list on stdout, and vmsync would compare it against a longer plan
// and report a plan mismatch instead of the real error. Buffering costs 24
// bytes per block (~1.2 MB for a 50 GiB disk), a cheap price for not lying
// about a partial result.
func runChecksum(ctx context.Context, cfg checksumConfig, in io.Reader, out io.Writer) error {
	header, ranges, err := blockdigest.ReadRequest(in)
	if err != nil {
		return err
	}
	// Refuse a request from a vmsync this binary cannot speak to, before
	// doing any work. Reporting version skew as skew is the whole reason the
	// header exists: hashing with the wrong algorithm would produce digests
	// that disagree everywhere and arrive as "this replica is corrupt".
	if err := header.CheckSupported(); err != nil {
		return err
	}

	// Verbatim, NOT re-planned. The request names the exact ranges vmsync
	// hashed during the copy -- its own buffer-sized chunks, whose
	// boundaries follow extent starts and line up with no fixed grid.
	// Canonicalising them here would hash different bytes than vmsync did
	// and report a mismatch on every single run.
	blocks, err := blockdigest.BlocksFromRanges(ranges, header.BlockSize)
	if err != nil {
		return err
	}
	if len(blocks) == 0 {
		// A sync that wrote nothing plans nothing. Not an error: vmsync
		// expects an empty list and agrees with it. The header still goes
		// out, so the exchange is well-formed and the version check on the
		// far side still runs. Returning before dialling also means a no-op
		// check does not depend on the export even existing.
		return blockdigest.WriteResponse(out, header, nil)
	}

	c, err := nbdclient.Dial(ctx, cfg.NBDAddr, cfg.Export, cfg.Timeout)
	if err != nil {
		return fmt.Errorf("connect nbd export %q at %s: %w", cfg.Export, cfg.NBDAddr, err)
	}
	defer func() { _ = c.Close() }()

	// One buffer, sized to the largest block, reused for every read. Safe to
	// allocate from untrusted input because two checks have already bounded
	// it: CheckSupported capped header.BlockSize at MaxBlockSize, and
	// BlocksFromRanges refused any range longer than header.BlockSize.
	maxLen := uint64(0)
	for _, b := range blocks {
		if b.Length > maxLen {
			maxLen = b.Length
		}
	}
	buf := make([]byte, maxLen)

	// Progress and liveness, on one clock and from one writer.
	//
	// ONE WRITER, deliberately: the hashing loop below only updates counters
	// and the beat goroutine does all the printing. Two goroutines writing
	// to the same stream interleave, and a torn line is a line vmsync's
	// reader drops -- which, for output whose whole job is to prove this
	// side is alive, would be the one failure that looks exactly like the
	// fault it exists to rule out.
	//
	// Started HERE rather than in main(), and that placement is load
	// bearing: everything above -- reading the request from stdin, the
	// version check, and the dial -- is upstream of the first byte read, and
	// a beat that covered it would report "alive" for a helper hung on a
	// stdin that never closed or a dial that never completed. Silence still
	// covers those, so liveness only ever claims what it can actually prove:
	// that this loop is running and scheduled.
	//
	// Same cadence as vmsync's own side of the pass (trace.ProgressInterval),
	// because the two are read against each other: "nbd: digest progress" is
	// the source, this is the target, and which of them is slow is the first
	// question anybody asks of a verify that is taking too long.
	totalBytes := blockdigest.TotalBytes(blocks)
	start := time.Now()
	prog := &digestProgress{totalBytes: totalBytes}

	interval := cfg.Interval
	if interval <= 0 {
		interval = trace.ProgressInterval
	}
	stopBeat := func() {}
	if cfg.Progress != nil {
		ticker := time.NewTicker(interval)
		beatDone := make(chan struct{})
		var beating sync.WaitGroup
		beating.Add(1)
		go func() {
			defer beating.Done()
			defer ticker.Stop()
			digestBeat(beatDone, ticker.C, prog.snapshot, func(s digestState) {
				fmt.Fprint(cfg.Progress, formatDigestLine(cfg.Export, s, time.Since(start)))
			})
		}()
		var once sync.Once
		stopBeat = func() { once.Do(func() { close(beatDone); beating.Wait() }) }
	}
	// Deferred as well as called below, so no error path leaves a goroutine
	// printing into a writer whose owner has given up on this pass.
	defer stopBeat()

	for i := range blocks {
		if err := ctx.Err(); err != nil {
			return err
		}
		b := blocks[i]
		// Before the read, not after: the read is what blocks, so a block
		// recorded afterwards would name the wrong one for the whole of the
		// wait anybody needs it for.
		prog.reading(b.Offset, b.Length)
		p := buf[:b.Length]
		if err := c.ReadAt(p, b.Offset); err != nil {
			return fmt.Errorf("read %d+%d from export %q: %w", b.Offset, b.Length, cfg.Export, err)
		}
		blocks[i].Digest = blockdigest.Sum(p)
		prog.hashed(b.Length)
	}

	// The completion line, written here because the beat has stopped and
	// been waited for, so this is once again the only writer. A pass shorter
	// than one tick would otherwise report nothing at all, and 100% is the
	// line that says the target's half finished rather than merely stopped.
	stopBeat()
	if cfg.Progress != nil {
		done := prog.snapshot()
		done.Moved = true
		fmt.Fprint(cfg.Progress, formatDigestLine(cfg.Export, done, time.Since(start)))
	}

	return blockdigest.WriteResponse(out, header, blocks)
}

// digestState is one reading of the pass, taken under one lock so the fields
// cannot disagree with each other: a line that pairs this minute's byte count
// with last minute's offset describes a pass that never existed.
type digestState struct {
	HashedBytes uint64
	TotalBytes  uint64
	// Offset and Length are the block currently being read. They are what
	// makes an alive line worth printing: the same block named twice in a
	// row is a pass that is up but not advancing, which no byte count can
	// show because the byte count is only credited when a block completes.
	Offset uint64
	Length uint64
	// Moved is set by the beat, not by the pass: it reports whether
	// HashedBytes has changed since the last line this beat emitted.
	Moved bool
}

// digestProgress is the pass's state as the beat sees it.
//
// A mutex rather than atomics because the three values are read together and
// have to be consistent with one another; the cost is one uncontended lock
// per block, against a block that took a disk read to produce.
type digestProgress struct {
	mu          sync.Mutex
	totalBytes  uint64
	hashedBytes uint64
	offset      uint64
	length      uint64
}

// reading records the block about to be read, before the read blocks.
func (p *digestProgress) reading(offset, length uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.offset, p.length = offset, length
}

// hashed credits a completed block.
func (p *digestProgress) hashed(n uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hashedBytes += n
}

func (p *digestProgress) snapshot() digestState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return digestState{
		HashedBytes: p.hashedBytes,
		TotalBytes:  p.totalBytes,
		Offset:      p.offset,
		Length:      p.length,
	}
}

// digestBeat emits one line per tick for as long as the pass runs: progress
// when bytes have been credited since the last line, liveness when they have
// not.
//
// THE POINT OF THE LIVENESS LINE. The side that waits has to tell a helper
// that is wedged from one that is merely slow, and before this it could not:
// a line was only produced when a BLOCK completed, so silence meant either.
// A block is up to 32 MiB, read in 4 MiB requests, each of which is three
// separately deadlined socket operations -- so a helper doing exactly what it
// is supposed to can legitimately say nothing for tens of minutes, and any
// bound safe against that is too long to bound anything. Beating on a fixed
// clock separates the two questions: silence now means the helper, or the
// channel carrying its output, is gone. How SLOW it is allowed to be stops
// being a number anybody has to guess, and a legitimately slow pass runs to
// completion however long it takes.
//
// It emits on every tick, never two lines for one tick, so the cost stays at
// one line a minute whatever the pass is doing -- the same budget the
// progress-only version spent.
//
// Pure with respect to the clock and the pass, both of which arrive as
// arguments, so what it emits and when is provable without an export to read
// or a minute to wait.
func digestBeat(stop <-chan struct{}, tick <-chan time.Time, snapshot func() digestState, emit func(digestState)) {
	reported := uint64(0)
	first := true
	for {
		select {
		case <-stop:
			return
		case _, open := <-tick:
			if !open {
				return
			}
			// A stop and a tick that are both ready are chosen between at
			// random, and the pass that just finished its last block is
			// exactly when both are. Checking again costs nothing and keeps
			// "still reading the block at X" out of the log of a pass that
			// has already finished reading it.
			if beatStopped(stop) {
				return
			}
			s := snapshot()
			s.Moved = first || s.HashedBytes != reported
			reported = s.HashedBytes
			first = false
			emit(s)
		}
	}
}

// beatStopped reports whether stop has been closed, without waiting for it.
func beatStopped(stop <-chan struct{}) bool {
	select {
	case <-stop:
		return true
	default:
		return false
	}
}

// formatDigestLine renders one line.
//
// The progress form is byte-for-byte what this helper has always emitted:
// operators grep it and vmsync's log carries it verbatim, so the liveness
// line is a new shape beside it rather than a change to it.
//
// Bare, with no timestamp and no level: vmsync stamps each line as it reads
// it, so the time on a line is the time the WAITING side saw it rather than
// two clocks that may disagree. And no "checksum:" prefix -- vmsync adds its
// own, and two read as a stutter.
func formatDigestLine(export string, s digestState, elapsed time.Duration) string {
	percent := 100.0
	if s.TotalBytes > 0 {
		percent = (float64(s.HashedBytes) / float64(s.TotalBytes)) * 100.0
	}
	if !s.Moved {
		return fmt.Sprintf("digest alive (%s)  %.2f%% (%d/%d bytes), still reading the block at %d+%d after %s\n",
			export, percent, s.HashedBytes, s.TotalBytes, s.Offset, s.Length,
			elapsed.Round(time.Second))
	}
	seconds := elapsed.Seconds()
	if seconds <= 0 {
		seconds = 0.001
	}
	mibPerSec := (float64(s.HashedBytes) / (1024.0 * 1024.0)) / seconds
	return fmt.Sprintf("digest progress (%s)  %.2f%% (%d/%d bytes) %.2f MiB/s\n",
		export, percent, s.HashedBytes, s.TotalBytes, mibPerSec)
}
