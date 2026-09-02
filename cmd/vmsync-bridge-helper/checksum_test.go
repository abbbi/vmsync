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
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"vmsync/pkg/blockdigest"
	"vmsync/pkg/nbdclient"
	"vmsync/pkg/nbdclient/nbdclienttest"
)

func diskPattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*13 + i/97)
	}
	return b
}

func startExport(t *testing.T, data []byte, export string) *nbdclienttest.Server {
	t.Helper()
	s, err := nbdclienttest.NewServer(data, export)
	if err != nil {
		t.Fatalf("start fake export: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func checksumCfg(s *nbdclienttest.Server, export string) checksumConfig {
	return checksumConfig{
		NBDAddr: s.Addr(),
		Export:  export,
		Timeout: 5 * time.Second,
	}
}

// request builds what vmsync sends: a header declaring the longest range in
// the body, then the ranges verbatim.
func request(t *testing.T, declaredMax uint64, ranges ...blockdigest.Range) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	if err := blockdigest.WriteRequest(&buf, blockdigest.DefaultHeader(declaredMax), ranges); err != nil {
		t.Fatalf("WriteRequest: %v", err)
	}
	return &buf
}

// expected computes the digests vmsync's side would hold: the very same
// ranges, hashed over the source bytes. Verbatim, matching what the helper
// must do -- no planning on either side.
func expected(ranges []blockdigest.Range, source []byte) []blockdigest.Block {
	var blocks []blockdigest.Block
	for _, r := range ranges {
		if r.Length == 0 {
			continue
		}
		blocks = append(blocks, blockdigest.Block{
			Offset: r.Offset,
			Length: r.Length,
			Digest: blockdigest.Sum(source[r.Offset : r.Offset+r.Length]),
		})
	}
	return blocks
}

// chunked mimics the copy's own chunking: one span broken into chunk-sized
// ranges. This is the shape vmsync actually sends -- CopyExtentsTCP caps
// each chunk at the negotiated buffer size and hashes them individually --
// so tests that want a realistic request build it this way rather than
// asking for one enormous range.
func chunked(offset, length, chunk uint64) []blockdigest.Range {
	var out []blockdigest.Range
	for length > 0 {
		n := chunk
		if length < n {
			n = length
		}
		out = append(out, blockdigest.Range{Offset: offset, Length: n})
		offset += n
		length -= n
	}
	return out
}

// readResponse parses the helper's output and checks its header, the way
// vmsync will.
func readResponse(t *testing.T, out *bytes.Buffer, declaredMax uint64) []blockdigest.Block {
	t.Helper()
	h, blocks, err := blockdigest.ReadResponse(out)
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	if err := h.Check(blockdigest.DefaultHeader(declaredMax)); err != nil {
		t.Fatalf("response header Check: %v", err)
	}
	return blocks
}

// The property that matters: the digests the helper prints must equal the
// digests computed independently over the same plan and the same bytes. If
// they do, vmsync's side (which hashes during the copy) will agree with it.
func TestRunChecksumDigestsMatchTheSourceOfTruth(t *testing.T) {
	const bs = 4096
	data := diskPattern(32 * bs)
	s := startExport(t, data, "vm-vda")

	written := append(chunked(0, 8*bs, bs), chunked(20*bs, 3*bs, bs)...)

	var out bytes.Buffer
	if err := runChecksum(context.Background(), checksumCfg(s, "vm-vda"), request(t, bs, written...), &out); err != nil {
		t.Fatalf("runChecksum: %v", err)
	}
	got := readResponse(t, &out, bs)
	want := expected(written, data)

	if len(got) != len(want) {
		t.Fatalf("got %d digests, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("block %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	m, err := blockdigest.Compare(want, got)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if len(m) != 0 {
		t.Errorf("Compare found %d mismatches on identical data", len(m))
	}
}

// The end this whole feature exists for: a target whose bytes differ from
// what the copy sent must be reported, and localised to the right block.
func TestRunChecksumSurfacesTargetSideCorruption(t *testing.T) {
	const bs = 4096
	source := diskPattern(16 * bs)

	target := make([]byte, len(source))
	copy(target, source)
	target[6*bs+11] ^= 0x80 // one flipped bit in block 6

	s := startExport(t, target, "vm-vda")
	written := chunked(0, uint64(len(source)), bs)

	var out bytes.Buffer
	if err := runChecksum(context.Background(), checksumCfg(s, "vm-vda"), request(t, bs, written...), &out); err != nil {
		t.Fatalf("runChecksum: %v", err)
	}

	m, err := blockdigest.Compare(expected(written, source), readResponse(t, &out, bs))
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if len(m) != 1 {
		t.Fatalf("got %d mismatches, want exactly 1: %v", len(m), m)
	}
	if m[0].Offset != 6*bs {
		t.Errorf("mismatch reported at %d, want %d", m[0].Offset, 6*bs)
	}
}

// Unwritten regions must not be hashed. A target whose UNWRITTEN bytes
// differ from the source is not this run's problem -- reporting it would
// turn the pre-commit check from "did this run land" into "is the whole
// disk correct", which is what a periodic -verify is for.
func TestRunChecksumIgnoresBytesOutsideTheWrittenRanges(t *testing.T) {
	const bs = 4096
	source := diskPattern(16 * bs)

	target := make([]byte, len(source))
	copy(target, source)
	for i := 8 * bs; i < 16*bs; i++ {
		target[i] ^= 0xff // wholly different, but never written by this run
	}

	s := startExport(t, target, "e")
	written := chunked(0, 8*bs, bs)

	var out bytes.Buffer
	if err := runChecksum(context.Background(), checksumCfg(s, "e"), request(t, bs, written...), &out); err != nil {
		t.Fatalf("runChecksum: %v", err)
	}
	m, err := blockdigest.Compare(expected(written, source), readResponse(t, &out, bs))
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if len(m) != 0 {
		t.Errorf("mismatches reported for bytes this run never wrote: %v", m)
	}
}

// A sync that wrote nothing must not dial: the check should be a no-op
// rather than depending on the export existing. It must still emit a
// well-formed header, so the far side's version check runs.
func TestRunChecksumEmptyPlanNeitherDialsNorReportsBlocks(t *testing.T) {
	s := startExport(t, diskPattern(4096), "e")

	cfg := checksumCfg(s, "e")
	// A deliberately unreachable address: if the empty plan dialled, this
	// would fail, which is the assertion.
	cfg.NBDAddr = "127.0.0.1:1"

	var out bytes.Buffer
	if err := runChecksum(context.Background(), cfg, request(t, 4096), &out); err != nil {
		t.Fatalf("runChecksum with an empty plan: %v", err)
	}
	if got := readResponse(t, &out, 4096); len(got) != 0 {
		t.Errorf("empty plan reported %d blocks", len(got))
	}
	if n := s.Reads(); n != 0 {
		t.Errorf("empty plan issued %d reads", n)
	}
}

// Zero-length ranges plan no blocks, same as no ranges at all.
func TestRunChecksumZeroLengthRangesPlanNothing(t *testing.T) {
	s := startExport(t, diskPattern(4096), "e")
	var out bytes.Buffer
	in := request(t, 4096, blockdigest.Range{Offset: 0, Length: 0}, blockdigest.Range{Offset: 2048, Length: 0})
	if err := runChecksum(context.Background(), checksumCfg(s, "e"), in, &out); err != nil {
		t.Fatalf("runChecksum: %v", err)
	}
	if got := readResponse(t, &out, 4096); len(got) != 0 {
		t.Errorf("reported %d blocks for zero-length ranges", len(got))
	}
	if n := s.Reads(); n != 0 {
		t.Errorf("issued %d reads for zero-length ranges", n)
	}
}

// Asking for an export the server does not have must fail loudly, and with
// the typed error, because it is the signature of connecting to a stale
// qemu-nbd from an earlier run rather than to this disk.
func TestRunChecksumWrongExportIsRefused(t *testing.T) {
	s := startExport(t, diskPattern(4096), "right-name")
	var out bytes.Buffer
	err := runChecksum(context.Background(), checksumCfg(s, "wrong-name"),
		request(t, 4096, blockdigest.Range{Offset: 0, Length: 4096}), &out)
	if err == nil {
		t.Fatal("runChecksum against the wrong export succeeded")
	}
	if !errors.Is(err, nbdclient.ErrExportNotFound) {
		t.Errorf("err = %v, want ErrExportNotFound", err)
	}
	if out.Len() != 0 {
		t.Errorf("stdout got %q on failure; it must carry a response only", out.String())
	}
}

// stdout carries a response and nothing else, so a failure mid-pass must
// leave it empty rather than truncated -- a truncated but well-formed
// response would reach vmsync as a plan mismatch and hide the real error.
func TestRunChecksumWritesNothingOnFailure(t *testing.T) {
	const bs = 4096
	// The export is smaller than the plan, so a later read fails after
	// earlier ones have already succeeded.
	s := startExport(t, diskPattern(2*bs), "e")
	var out bytes.Buffer
	// Chunked, so the first two ranges read fine and a later one runs off
	// the end -- a genuine mid-pass failure, not a request refused up front.
	err := runChecksum(context.Background(), checksumCfg(s, "e"),
		request(t, bs, chunked(0, 8*bs, bs)...), &out)
	if err == nil {
		t.Fatal("reading past the end of the export succeeded")
	}
	if out.Len() != 0 {
		t.Errorf("stdout got %q after a mid-pass failure, want nothing", out.String())
	}
}

func TestRunChecksumRejectsMalformedRequest(t *testing.T) {
	s := startExport(t, diskPattern(4096), "e")
	const hdr = "vmsync-digest 1 xxh64 4096\n"
	for _, tc := range []struct{ name, in string }{
		{"no header", "0 512\n"},
		{"three body fields", hdr + "0 512 7\n"},
		{"one body field", hdr + "512\n"},
		{"non-numeric", hdr + "start 512\n"},
		{"shell diagnostic", hdr + "0 512\nsh: no such file\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := runChecksum(context.Background(), checksumCfg(s, "e"), strings.NewReader(tc.in), &out); err == nil {
				t.Errorf("runChecksum(%q) succeeded, want an error", tc.in)
			}
		})
	}
}

// Version skew must be refused before any work, and as ErrFormatMismatch --
// hashing with the wrong algorithm would produce digests differing
// everywhere, arriving as "this replica is corrupt".
func TestRunChecksumRefusesUnsupportedRequestHeader(t *testing.T) {
	s := startExport(t, diskPattern(64<<10), "e")
	for _, tc := range []struct{ name, header string }{
		{"newer format version", "vmsync-digest 99 xxh64 4096"},
		{"unknown algorithm", "vmsync-digest 1 blake3 4096"},
		{"the algorithm we moved away from", "vmsync-digest 1 crc32c 4096"},
		{"zero block size", "vmsync-digest 1 xxh64 0"},
		{"absurd block size", fmt.Sprintf("vmsync-digest 1 xxh64 %d", uint64(blockdigest.MaxBlockSize)+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := runChecksum(context.Background(), checksumCfg(s, "e"),
				strings.NewReader(tc.header+"\n0 4096\n"), &out)
			if !errors.Is(err, blockdigest.ErrFormatMismatch) {
				t.Errorf("err = %v, want ErrFormatMismatch", err)
			}
			if out.Len() != 0 {
				t.Errorf("stdout got %q for a refused request", out.String())
			}
			if n := s.Reads(); n != 0 {
				t.Errorf("a refused request still issued %d reads", n)
			}
		})
	}
}

// The defining property of the exchange: the helper hashes exactly the
// ranges it was given, one digest per requested range, with no coalescing,
// splitting or reordering.
//
// This is what lets vmsync hash the copy's own chunks -- whose boundaries
// follow extent starts and match no grid -- rather than re-chunking the copy
// onto one. If the helper canonicalised instead, it would hash different
// bytes than the copy did and every run would report a mismatch.
func TestRunChecksumHashesRequestedRangesVerbatim(t *testing.T) {
	const bs = 4096
	data := diskPattern(16 * bs)
	s := startExport(t, data, "e")

	// Deliberately awkward: unaligned, out of order, adjacent-but-separate,
	// and of differing lengths -- everything a canonicalising planner would
	// "tidy up", and none of which it may.
	written := []blockdigest.Range{
		{Offset: 3 * bs, Length: bs},   // out of order relative to the next
		{Offset: 17, Length: 100},      // unaligned, tiny
		{Offset: bs, Length: bs},       // adjacent to the next, must NOT merge
		{Offset: 2 * bs, Length: bs},   //   "
		{Offset: 9*bs + 7, Length: bs}, // unaligned, straddles a grid line
	}

	var out bytes.Buffer
	if err := runChecksum(context.Background(), checksumCfg(s, "e"), request(t, bs, written...), &out); err != nil {
		t.Fatalf("runChecksum: %v", err)
	}
	got := readResponse(t, &out, bs)

	if len(got) != len(written) {
		t.Fatalf("got %d blocks for %d requested ranges -- the helper re-planned them: %v", len(got), len(written), got)
	}
	for i, r := range written {
		if got[i].Offset != r.Offset || got[i].Length != r.Length {
			t.Errorf("block %d = %d+%d, want the requested %d+%d (order and boundaries must be preserved)",
				i, got[i].Offset, got[i].Length, r.Offset, r.Length)
		}
	}
	m, err := blockdigest.Compare(expected(written, data), got)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if len(m) != 0 {
		t.Errorf("digests disagree on identical data: %v", m)
	}
}

// A range longer than the header's declared maximum means the two sides
// disagree about the format, so it is refused rather than split -- the
// responder sized its buffer on that promise.
func TestRunChecksumRefusesRangeLongerThanDeclaredMax(t *testing.T) {
	s := startExport(t, diskPattern(64<<10), "e")
	var out bytes.Buffer
	err := runChecksum(context.Background(), checksumCfg(s, "e"),
		// Declares 4096, then asks for 8192.
		strings.NewReader("vmsync-digest 1 xxh64 4096\n0 8192\n"), &out)
	if !errors.Is(err, blockdigest.ErrFormatMismatch) {
		t.Errorf("err = %v, want ErrFormatMismatch", err)
	}
	if out.Len() != 0 {
		t.Errorf("stdout got %q for a refused request", out.String())
	}
}

// The response must echo the request's header, so vmsync's Check confirms
// the helper obeyed rather than assuming it.
func TestRunChecksumEchoesTheRequestHeader(t *testing.T) {
	const bs = 8192
	s := startExport(t, diskPattern(32<<10), "e")
	var out bytes.Buffer
	if err := runChecksum(context.Background(), checksumCfg(s, "e"),
		request(t, bs, chunked(0, 32<<10, bs)...), &out); err != nil {
		t.Fatalf("runChecksum: %v", err)
	}
	h, _, err := blockdigest.ReadResponse(&out)
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	if want := blockdigest.DefaultHeader(bs); h != want {
		t.Errorf("response header = %+v, want %+v", h, want)
	}
}

func TestRunChecksumRespectsCancelledContext(t *testing.T) {
	s := startExport(t, diskPattern(64<<10), "e")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out bytes.Buffer
	err := runChecksum(ctx, checksumCfg(s, "e"),
		request(t, 4096, chunked(0, 64<<10, 4096)...), &out)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// Whatever shape the ranges arrive in, the digests must agree with what
// vmsync computed over the same ranges. Different shapes legitimately give
// different BLOCKS now -- the helper hashes verbatim -- but each shape must
// still be self-consistent between the two sides, which is the only property
// the comparison depends on.
func TestRunChecksumAgreesWithVmsyncForEveryRequestShape(t *testing.T) {
	const bs = 4096
	data := diskPattern(16 * bs)
	s := startExport(t, data, "e")

	shapes := [][]blockdigest.Range{
		{{Offset: 0, Length: 8 * bs}},
		{{Offset: 4 * bs, Length: 4 * bs}, {Offset: 0, Length: 4 * bs}},
		{{Offset: 0, Length: bs}, {Offset: bs, Length: bs}, {Offset: 2 * bs, Length: bs}},
		{{Offset: 11, Length: 7}, {Offset: 5*bs + 1, Length: bs - 2}},
	}
	for i, shape := range shapes {
		var out bytes.Buffer
		if err := runChecksum(context.Background(), checksumCfg(s, "e"), request(t, 8*bs, shape...), &out); err != nil {
			t.Fatalf("shape %d: runChecksum: %v", i, err)
		}
		m, err := blockdigest.Compare(expected(shape, data), readResponse(t, &out, 8*bs))
		if err != nil {
			t.Fatalf("shape %d: Compare: %v", i, err)
		}
		if len(m) != 0 {
			t.Errorf("shape %d: digests disagree on identical data: %v", i, m)
		}
	}
}
