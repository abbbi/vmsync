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

	// Reported on the same cadence as vmsync's own side of the pass (see
	// trace.ProgressInterval), because the two are read against each other:
	// "nbd: digest progress" is the source, this is the target, and which of
	// them is the slow one is the first question anybody asks of a verify
	// that is taking too long.
	//
	// Bare lines, with no timestamp and no level: vmsync stamps them as it
	// reads them, so a line carries the time it was SEEN by the side doing
	// the waiting rather than two clocks that may disagree.
	totalBytes := blockdigest.TotalBytes(blocks)
	start := time.Now()
	lastLog := start
	var hashedBytes uint64
	logProgress := func() {
		if cfg.Progress == nil {
			return
		}
		now := time.Now()
		if !trace.DueForProgress(lastLog, now, hashedBytes == totalBytes) {
			return
		}
		elapsed := now.Sub(start).Seconds()
		if elapsed <= 0 {
			elapsed = 0.001
		}
		percent := 100.0
		if totalBytes > 0 {
			percent = (float64(hashedBytes) / float64(totalBytes)) * 100.0
		}
		mibPerSec := (float64(hashedBytes) / (1024.0 * 1024.0)) / elapsed
		// No "checksum:" prefix: vmsync adds its own ("checksum: target: ")
		// as it logs the line, and two of them read as a stutter.
		fmt.Fprintf(cfg.Progress, "digest progress (%s)  %.2f%% (%d/%d bytes) %.2f MiB/s\n",
			cfg.Export, percent, hashedBytes, totalBytes, mibPerSec)
		lastLog = now
	}

	for i := range blocks {
		if err := ctx.Err(); err != nil {
			return err
		}
		b := blocks[i]
		p := buf[:b.Length]
		if err := c.ReadAt(p, b.Offset); err != nil {
			return fmt.Errorf("read %d+%d from export %q: %w", b.Offset, b.Length, cfg.Export, err)
		}
		blocks[i].Digest = blockdigest.Sum(p)
		hashedBytes += b.Length
		logProgress()
	}

	return blockdigest.WriteResponse(out, header, blocks)
}
