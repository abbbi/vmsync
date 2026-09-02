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

package blockdigest

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cespare/xxhash/v2"
)

func blocksEqual(a, b []Block) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func fmtBlocks(bs []Block) string {
	var sb strings.Builder
	for i, b := range bs {
		if i > 0 {
			sb.WriteString(" ")
		}
		fmt.Fprintf(&sb, "%d+%d", b.Offset, b.Length)
	}
	if sb.Len() == 0 {
		return "<none>"
	}
	return sb.String()
}

// --- planning ---------------------------------------------------------------

func TestPlanBlocksSplitsOnAbsoluteGrid(t *testing.T) {
	const bs = 1 << 20
	tests := []struct {
		name string
		in   []Range
		want []Block
	}{
		{
			name: "aligned range of exactly one block",
			in:   []Range{{0, bs}},
			want: []Block{{Offset: 0, Length: bs}},
		},
		{
			name: "aligned range of several blocks",
			in:   []Range{{0, 3 * bs}},
			want: []Block{{0, bs, 0}, {bs, bs, 0}, {2 * bs, bs, 0}},
		},
		{
			// The defining property: a range starting mid-block produces a
			// SHORT first block so that every following block starts on the
			// absolute grid. Splitting relative to the range start would
			// give 1 MiB blocks at 1.5 MiB, 2.5 MiB, ... and two passes that
			// coalesced extents differently would disagree.
			name: "unaligned start yields a short leading block",
			in:   []Range{{bs + bs/2, bs}},
			want: []Block{
				{bs + bs/2, bs / 2, 0},
				{2 * bs, bs / 2, 0},
			},
		},
		{
			// [0.5 MiB, 2.75 MiB): short block at each end, whole blocks
			// in between.
			name: "unaligned start and end",
			in:   []Range{{bs / 2, 2*bs + bs/4}},
			want: []Block{
				{bs / 2, bs / 2, 0},     // 0.5M -> 1M
				{bs, bs, 0},             // 1M   -> 2M
				{2 * bs, 3 * bs / 4, 0}, // 2M   -> 2.75M
			},
		},
		{
			name: "range smaller than a block, inside one cell",
			in:   []Range{{bs + 100, 200}},
			want: []Block{{bs + 100, 200, 0}},
		},
		{
			name: "range smaller than a block but crossing a boundary",
			in:   []Range{{bs - 100, 200}},
			want: []Block{{bs - 100, 100, 0}, {bs, 100, 0}},
		},
		{
			name: "zero-length ranges are dropped",
			in:   []Range{{0, 0}, {bs, 0}},
			want: nil,
		},
		{
			name: "no ranges",
			in:   nil,
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := PlanBlocks(tc.in, bs)
			if !blocksEqual(got, tc.want) {
				t.Errorf("PlanBlocks = %s, want %s", fmtBlocks(got), fmtBlocks(tc.want))
			}
		})
	}
}

// Every block must sit inside one grid cell, be non-empty, never exceed
// blockSize, and not overlap its neighbour. Checked as a property over an
// awkward input rather than a hand-written expectation.
func TestPlanBlocksInvariants(t *testing.T) {
	const bs = 64 << 10
	in := []Range{
		{0, 17},
		{bs - 1, 3},
		{5 * bs, 4 * bs},
		{100*bs + 7, 2*bs + 11},
	}
	blocks := PlanBlocks(in, bs)
	if len(blocks) == 0 {
		t.Fatal("no blocks planned")
	}
	for i, b := range blocks {
		if b.Length == 0 {
			t.Errorf("block %d has zero length", i)
		}
		if b.Length > bs {
			t.Errorf("block %d length %d exceeds block size %d", i, b.Length, bs)
		}
		if b.Offset/bs != (b.Offset+b.Length-1)/bs {
			t.Errorf("block %d (%d+%d) straddles a %d boundary", i, b.Offset, b.Length, bs)
		}
		if i > 0 {
			prev := blocks[i-1]
			if b.Offset < prev.Offset+prev.Length {
				t.Errorf("block %d (%d) overlaps previous (%d+%d)", i, b.Offset, prev.Offset, prev.Length)
			}
		}
	}
}

// The plan must cover exactly the written bytes -- no more (or the check
// would judge bytes this run never wrote) and no less.
func TestPlanBlocksCoversExactlyTheWrittenBytes(t *testing.T) {
	const bs = 1 << 20
	in := []Range{{bs / 2, bs}, {10 * bs, 3*bs + 5}}
	var want uint64
	for _, r := range in {
		want += r.Length
	}
	if got := TotalBytes(PlanBlocks(in, bs)); got != want {
		t.Errorf("planned %d bytes, want %d", got, want)
	}
}

func TestPlanBlocksCoalescesAndSorts(t *testing.T) {
	const bs = 1 << 20
	// Deliberately out of order, with a touching pair and an overlapping
	// pair. All of it describes [0, 2*bs), which must plan as two full
	// blocks -- not as four partial ones reflecting the input's chopping.
	in := []Range{
		{bs, bs},         // second half, given first
		{0, bs / 2},      // start
		{bs / 2, bs / 2}, // touches the previous exactly
		{bs / 4, bs / 2}, // overlaps both
	}
	want := []Block{{0, bs, 0}, {bs, bs, 0}}
	if got := PlanBlocks(in, bs); !blocksEqual(got, want) {
		t.Errorf("PlanBlocks = %s, want %s", fmtBlocks(got), fmtBlocks(want))
	}
}

// The whole feature rests on both sides planning identically from the same
// extents. Feeding the same set in two different orders must produce the
// same plan.
func TestPlanBlocksIsOrderIndependent(t *testing.T) {
	const bs = 1 << 20
	a := []Range{{0, 100}, {5 * bs, bs}, {3*bs + 7, 2 * bs}}
	b := []Range{{3*bs + 7, 2 * bs}, {0, 100}, {5 * bs, bs}}
	if pa, pb := PlanBlocks(a, bs), PlanBlocks(b, bs); !blocksEqual(pa, pb) {
		t.Errorf("plans differ by input order:\n a = %s\n b = %s", fmtBlocks(pa), fmtBlocks(pb))
	}
}

func TestPlanBlocksZeroBlockSizeUsesDefault(t *testing.T) {
	in := []Range{{0, 3 * DefaultBlockSize}}
	if got, want := PlanBlocks(in, 0), PlanBlocks(in, DefaultBlockSize); !blocksEqual(got, want) {
		t.Error("zero block size did not fall back to DefaultBlockSize")
	}
}

// --- verbatim blocks (the sync path) ----------------------------------------

// The defining property of the sync path: ranges become blocks unchanged.
// No coalescing, no splitting, no sorting -- because the requester hashed
// exactly these ranges (the copy's own chunks) and canonicalising them here
// would hash different bytes.
func TestBlocksFromRangesIsVerbatim(t *testing.T) {
	in := []Range{
		{Offset: 3000, Length: 100}, // out of order
		{Offset: 17, Length: 7},     // unaligned, tiny
		{Offset: 100, Length: 50},   // adjacent to the next...
		{Offset: 150, Length: 50},   // ...must NOT merge
		{Offset: 120, Length: 60},   // overlaps, must NOT merge
	}
	got, err := BlocksFromRanges(in, 1<<20)
	if err != nil {
		t.Fatalf("BlocksFromRanges: %v", err)
	}
	if len(got) != len(in) {
		t.Fatalf("got %d blocks for %d ranges: %v", len(got), len(in), got)
	}
	for i, r := range in {
		if got[i].Offset != r.Offset || got[i].Length != r.Length {
			t.Errorf("block %d = %d+%d, want %d+%d", i, got[i].Offset, got[i].Length, r.Offset, r.Length)
		}
		if got[i].Digest != 0 {
			t.Errorf("block %d has a digest already set", i)
		}
	}
}

func TestBlocksFromRangesDropsEmpties(t *testing.T) {
	got, err := BlocksFromRanges([]Range{{0, 0}, {0, 512}, {4096, 0}}, 4096)
	if err != nil {
		t.Fatalf("BlocksFromRanges: %v", err)
	}
	if len(got) != 1 || got[0].Offset != 0 || got[0].Length != 512 {
		t.Errorf("got %v, want just 0+512", got)
	}
}

// A range longer than the declared maximum means the two sides disagree
// about the format -- the responder sized its buffer on that promise -- so
// it is refused rather than split.
func TestBlocksFromRangesRefusesOversizedAndBadMax(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []Range
		max  uint64
	}{
		{"range longer than max", []Range{{0, 8192}}, 4096},
		{"second range longer than max", []Range{{0, 4096}, {4096, 4097}}, 4096},
		{"zero max", []Range{{0, 512}}, 0},
		{"max above the hard limit", []Range{{0, 512}}, uint64(MaxBlockSize) + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := BlocksFromRanges(tc.in, tc.max); !errors.Is(err, ErrFormatMismatch) {
				t.Errorf("err = %v, want ErrFormatMismatch", err)
			}
		})
	}
}

func TestBlocksFromRangesAcceptsExactlyMax(t *testing.T) {
	if _, err := BlocksFromRanges([]Range{{0, 4096}}, 4096); err != nil {
		t.Errorf("a range of exactly max was refused: %v", err)
	}
}

// The requester's own round trip: the blocks it hashed reduce to the ranges
// it sends, and the maximum it declares covers all of them.
func TestRangesFromBlocksAndMaxRangeLength(t *testing.T) {
	blocks := []Block{{0, 4096, 111}, {4096, 1 << 20, 222}, {2 << 20, 512, 333}}

	ranges := RangesFromBlocks(blocks)
	if len(ranges) != len(blocks) {
		t.Fatalf("got %d ranges for %d blocks", len(ranges), len(blocks))
	}
	for i, b := range blocks {
		if ranges[i] != (Range{Offset: b.Offset, Length: b.Length}) {
			t.Errorf("range %d = %+v, want %d+%d", i, ranges[i], b.Offset, b.Length)
		}
	}

	if got, want := MaxRangeLength(blocks), uint64(1<<20); got != want {
		t.Errorf("MaxRangeLength = %d, want %d", got, want)
	}
	if got := MaxRangeLength(nil); got != 0 {
		t.Errorf("MaxRangeLength(nil) = %d, want 0", got)
	}

	// And the declared maximum must actually admit every range it covers.
	if _, err := BlocksFromRanges(ranges, MaxRangeLength(blocks)); err != nil {
		t.Errorf("the declared maximum refused its own ranges: %v", err)
	}
}

// End to end for the sync path exactly as it runs: the requester hashes its
// own chunks, sends their boundaries, the responder hashes those verbatim,
// and the two agree.
func TestVerbatimExchangeRoundTrip(t *testing.T) {
	data := make([]byte, 40000)
	for i := range data {
		data[i] = byte(i * 7)
	}
	// Deliberately not grid-aligned, the way real copy chunks are not.
	chunks := []Range{{0, 12345}, {12345, 12345}, {24690, 15310}}

	sent := make([]Block, 0, len(chunks))
	for _, r := range chunks {
		sent = append(sent, Block{Offset: r.Offset, Length: r.Length, Digest: Sum(data[r.Offset : r.Offset+r.Length])})
	}
	h := DefaultHeader(MaxRangeLength(sent))

	var req bytes.Buffer
	if err := WriteRequest(&req, h, RangesFromBlocks(sent)); err != nil {
		t.Fatalf("WriteRequest: %v", err)
	}
	reqH, reqRanges, err := ReadRequest(&req)
	if err != nil {
		t.Fatalf("ReadRequest: %v", err)
	}
	if err := reqH.CheckSupported(); err != nil {
		t.Fatalf("CheckSupported: %v", err)
	}
	got, err := BlocksFromRanges(reqRanges, reqH.BlockSize)
	if err != nil {
		t.Fatalf("BlocksFromRanges: %v", err)
	}
	for i := range got {
		got[i].Digest = Sum(data[got[i].Offset : got[i].Offset+got[i].Length])
	}

	var resp bytes.Buffer
	if err := WriteResponse(&resp, reqH, got); err != nil {
		t.Fatalf("WriteResponse: %v", err)
	}
	respH, reported, err := ReadResponse(&resp)
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	if err := respH.Check(h); err != nil {
		t.Fatalf("response Check: %v", err)
	}
	m, err := Compare(sent, reported)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if len(m) != 0 {
		t.Errorf("mismatches on identical data: %v", m)
	}
}

// --- the digest itself ------------------------------------------------------

// Pins Sum to XXH64 with a zero seed. The digest is a wire format between
// two independently deployed binaries, so a change here is a compatibility
// break and must not pass silently as an implementation detail.
func TestSumIsXXH64ZeroSeed(t *testing.T) {
	for _, in := range []string{"", "a", "vmsync block digest", strings.Repeat("x", 1000)} {
		if got, want := Sum([]byte(in)), xxhash.Sum64([]byte(in)); got != want {
			t.Errorf("Sum(%q) = %d, want xxhash.Sum64 %d", in, got, want)
		}
	}
}

// Published XXH64 test vectors for the empty input and a known string,
// checked against the library so that swapping the library (or its seed)
// cannot silently change what goes on the wire.
func TestSumMatchesKnownVectors(t *testing.T) {
	// XXH64("") with seed 0.
	if got, want := Sum(nil), uint64(0xef46db3751d8e999); got != want {
		t.Errorf("Sum(empty) = %#x, want %#x", got, want)
	}
	if got, want := Sum([]byte("nonsense")), xxhash.Sum64String("nonsense"); got != want {
		t.Errorf("Sum and Sum64String disagree: %#x vs %#x", got, want)
	}
}

// A single flipped bit anywhere in a block must change the digest. Not a
// guarantee XXH64 offers the way a CRC does within its Hamming bound, but a
// failure here would mean the digest is not being applied to the bytes it
// should be.
func TestSumDetectsSingleBitFlips(t *testing.T) {
	block := make([]byte, 4096)
	for i := range block {
		block[i] = byte(i)
	}
	base := Sum(block)
	for byteIdx := 0; byteIdx < len(block); byteIdx += 97 {
		for bit := 0; bit < 8; bit++ {
			block[byteIdx] ^= 1 << bit
			if Sum(block) == base {
				t.Fatalf("flipping bit %d of byte %d did not change the digest", bit, byteIdx)
			}
			block[byteIdx] ^= 1 << bit
		}
	}
}

// --- header -----------------------------------------------------------------

func TestDefaultHeader(t *testing.T) {
	h := DefaultHeader(0)
	if h.Version != FormatVersion || h.Algo != AlgoXXH64 || h.BlockSize != DefaultBlockSize {
		t.Errorf("DefaultHeader(0) = %+v", h)
	}
	if got := DefaultHeader(4096); got.BlockSize != 4096 {
		t.Errorf("DefaultHeader(4096).BlockSize = %d", got.BlockSize)
	}
}

func TestHeaderCheckAcceptsAMatch(t *testing.T) {
	want := DefaultHeader(DefaultBlockSize)
	if err := want.Check(want); err != nil {
		t.Errorf("Check on identical headers: %v", err)
	}
}

// Version skew must be ErrFormatMismatch, never a data verdict: this is a
// stale helper, and reporting it as a corrupt replica is the exact failure
// the header exists to prevent.
func TestHeaderCheckRejectsSkew(t *testing.T) {
	want := DefaultHeader(DefaultBlockSize)
	for _, tc := range []struct {
		name   string
		got    Header
		expect string
	}{
		{"older format version", Header{Version: 0, Algo: AlgoXXH64, BlockSize: DefaultBlockSize}, "format version"},
		{"newer format version", Header{Version: 99, Algo: AlgoXXH64, BlockSize: DefaultBlockSize}, "format version"},
		{"different algorithm", Header{Version: FormatVersion, Algo: "crc32c", BlockSize: DefaultBlockSize}, "algorithm"},
		{"different block size", Header{Version: FormatVersion, Algo: AlgoXXH64, BlockSize: 4096}, "block size"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.got.Check(want)
			if !errors.Is(err, ErrFormatMismatch) {
				t.Fatalf("err = %v, want ErrFormatMismatch", err)
			}
			if !strings.Contains(err.Error(), tc.expect) {
				t.Errorf("err = %v, want it to mention %q", err, tc.expect)
			}
		})
	}
}

// The message has to tell the operator which deployment to move, so both
// sides' values belong in it.
func TestHeaderCheckNamesBothSides(t *testing.T) {
	got := Header{Version: FormatVersion, Algo: "crc32c", BlockSize: DefaultBlockSize}
	err := got.Check(DefaultHeader(DefaultBlockSize))
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "crc32c") || !strings.Contains(err.Error(), "xxh64") {
		t.Errorf("err = %v, want both algorithms named", err)
	}
}

// --- wire format ------------------------------------------------------------

func TestRequestRoundTrip(t *testing.T) {
	in := []Range{{0, 1 << 20}, {5 << 20, 4096}}
	h := DefaultHeader(1 << 20)

	var buf bytes.Buffer
	if err := WriteRequest(&buf, h, in); err != nil {
		t.Fatalf("WriteRequest: %v", err)
	}
	gotH, gotR, err := ReadRequest(&buf)
	if err != nil {
		t.Fatalf("ReadRequest: %v", err)
	}
	if gotH != h {
		t.Errorf("header = %+v, want %+v", gotH, h)
	}
	if len(gotR) != len(in) {
		t.Fatalf("got %d ranges, want %d", len(gotR), len(in))
	}
	for i := range in {
		if gotR[i] != in[i] {
			t.Errorf("range %d = %+v, want %+v", i, gotR[i], in[i])
		}
	}
}

func TestResponseRoundTrip(t *testing.T) {
	in := []Block{
		{0, 1 << 20, 0},
		{1 << 20, 4096, 0xdeadbeefcafef00d},
		{99 << 20, 512, ^uint64(0)},
	}
	h := DefaultHeader(1 << 20)

	var buf bytes.Buffer
	if err := WriteResponse(&buf, h, in); err != nil {
		t.Fatalf("WriteResponse: %v", err)
	}
	gotH, got, err := ReadResponse(&buf)
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	if gotH != h {
		t.Errorf("header = %+v, want %+v", gotH, h)
	}
	if !blocksEqual(got, in) {
		t.Errorf("round trip changed the blocks:\n got %v\nwant %v", got, in)
	}
}

// Pins the exact bytes. This is a wire format; a reformatting that looks
// harmless is a compatibility break.
func TestWireFormatIsPinned(t *testing.T) {
	var req bytes.Buffer
	if err := WriteRequest(&req, DefaultHeader(1048576), []Range{{1048576, 4096}}); err != nil {
		t.Fatalf("WriteRequest: %v", err)
	}
	if got, want := req.String(), "vmsync-digest 1 xxh64 1048576\n1048576 4096\n"; got != want {
		t.Errorf("request = %q, want %q", got, want)
	}

	var resp bytes.Buffer
	if err := WriteResponse(&resp, DefaultHeader(1048576), []Block{{1048576, 4096, 7}}); err != nil {
		t.Fatalf("WriteResponse: %v", err)
	}
	if got, want := resp.String(), "vmsync-digest 1 xxh64 1048576\n1048576 4096 7\n"; got != want {
		t.Errorf("response = %q, want %q", got, want)
	}
}

func TestReadResponseSkipsBlankLines(t *testing.T) {
	in := "vmsync-digest 1 xxh64 4096\n\n0 512 1\n\n512 512 2\n\n"
	_, got, err := ReadResponse(strings.NewReader(in))
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	want := []Block{{0, 512, 1}, {512, 512, 2}}
	if !blocksEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// The single most likely field failure: the helper did not run, or is too
// old to emit a header, so stdout carries a shell diagnostic or bare digest
// lines. Both must be ErrFormatMismatch and must quote what arrived.
func TestMissingHeaderIsFormatMismatch(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		{"no output at all", ""},
		{"only blank lines", "\n\n"},
		{"shell diagnostic", "sh: vmsync-bridge-helper: not found\n"},
		{"headerless digest lines from an older helper", "0 512 1\n512 512 2\n"},
		{"wrong magic", "some-other-tool 1 xxh64 4096\n"},
		{"non-numeric version", "vmsync-digest x xxh64 4096\n"},
		{"non-numeric block size", "vmsync-digest 1 xxh64 big\n"},
		{"too few header fields", "vmsync-digest 1 xxh64\n"},
		{"too many header fields", "vmsync-digest 1 xxh64 4096 extra\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := ReadResponse(strings.NewReader(tc.in))
			if !errors.Is(err, ErrFormatMismatch) {
				t.Errorf("err = %v, want ErrFormatMismatch", err)
			}
		})
	}
}

func TestMissingHeaderQuotesWhatArrived(t *testing.T) {
	_, _, err := ReadResponse(strings.NewReader("sh: vmsync-bridge-helper: not found\n"))
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("err = %v, want it to quote the offending line", err)
	}
}

// A truncated or noisy body must be an error, not silently fewer blocks --
// which would reach Compare as a plan mismatch and hide the real cause.
func TestReadResponseRejectsJunkBody(t *testing.T) {
	const hdr = "vmsync-digest 1 xxh64 4096\n"
	for _, tc := range []struct{ name, in string }{
		{"shell diagnostic after digests", hdr + "0 512 1\nsh: broken pipe\n"},
		{"too few fields", hdr + "0 512\n"},
		{"trailing junk after three fields", hdr + "0 512 1 oops\n"},
		{"non-numeric offset", hdr + "abc 512 1\n"},
		{"non-numeric length", hdr + "0 abc 1\n"},
		{"non-numeric digest", hdr + "0 512 abc\n"},
		{"digest wider than 64 bits", hdr + "0 512 18446744073709551616\n"},
		{"negative offset", hdr + "-1 512 1\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := ReadResponse(strings.NewReader(tc.in)); err == nil {
				t.Errorf("ReadResponse(%q) succeeded, want an error", tc.in)
			}
		})
	}
}

func TestReadResponseNamesTheOffendingLine(t *testing.T) {
	in := "vmsync-digest 1 xxh64 4096\n0 512 1\n512 512 2\nbroken\n"
	_, _, err := ReadResponse(strings.NewReader(in))
	if err == nil {
		t.Fatal("want an error")
	}
	// Header is line 1, so the bad line is 4.
	if !strings.Contains(err.Error(), "line 4") {
		t.Errorf("err = %v, want it to name line 4", err)
	}
}

func TestReadRequestRejectsJunkBody(t *testing.T) {
	const hdr = "vmsync-digest 1 xxh64 4096\n"
	for _, tc := range []struct{ name, in string }{
		{"three fields", hdr + "0 512 7\n"},
		{"one field", hdr + "512\n"},
		{"non-numeric", hdr + "start 512\n"},
		{"overflowing range", hdr + "18446744073709551615 2\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := ReadRequest(strings.NewReader(tc.in)); err == nil {
				t.Errorf("ReadRequest(%q) succeeded, want an error", tc.in)
			}
		})
	}
}

func TestReadResponseAcceptsTabs(t *testing.T) {
	_, got, err := ReadResponse(strings.NewReader("vmsync-digest\t1\txxh64\t4096\n0\t512\t9\n"))
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	if want := []Block{{0, 512, 9}}; !blocksEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// A header with an empty body is valid: a sync that wrote nothing.
func TestHeaderOnlyResponseIsAnEmptyPlan(t *testing.T) {
	h, got, err := ReadResponse(strings.NewReader("vmsync-digest 1 xxh64 1048576\n"))
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	if err := h.Check(DefaultHeader(DefaultBlockSize)); err != nil {
		t.Errorf("Check: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d blocks, want none", len(got))
	}
}

// --- comparison -------------------------------------------------------------

func TestCompareCleanRun(t *testing.T) {
	bs := []Block{{0, 512, 11}, {512, 512, 22}}
	m, err := Compare(bs, bs)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if len(m) != 0 {
		t.Errorf("Compare reported %d mismatches on identical input", len(m))
	}
}

func TestCompareReportsDifferingDigests(t *testing.T) {
	want := []Block{{0, 512, 11}, {512, 512, 22}, {1024, 512, 33}}
	got := []Block{{0, 512, 11}, {512, 512, 99}, {1024, 512, 33}}
	m, err := Compare(want, got)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if len(m) != 1 {
		t.Fatalf("got %d mismatches, want 1: %v", len(m), m)
	}
	if m[0] != (Mismatch{Offset: 512, Length: 512, Want: 22, Got: 99}) {
		t.Errorf("mismatch = %+v", m[0])
	}
}

// A structural disagreement is not evidence about the data. It must not be
// reportable as "the replica is corrupt", so it comes back as
// ErrPlanMismatch and not as a mismatch list.
func TestComparePlanDisagreementIsAnErrorNotAMismatch(t *testing.T) {
	for _, tc := range []struct {
		name      string
		want, got []Block
	}{
		{"fewer blocks reported", []Block{{0, 512, 1}, {512, 512, 2}}, []Block{{0, 512, 1}}},
		{"more blocks reported", []Block{{0, 512, 1}}, []Block{{0, 512, 1}, {512, 512, 2}}},
		{"same count, different offset", []Block{{0, 512, 1}}, []Block{{4096, 512, 1}}},
		{"same count, different length", []Block{{0, 512, 1}}, []Block{{0, 1024, 1}}},
		{"no blocks reported at all", []Block{{0, 512, 1}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := Compare(tc.want, tc.got)
			if !errors.Is(err, ErrPlanMismatch) {
				t.Errorf("err = %v, want ErrPlanMismatch", err)
			}
			if m != nil {
				t.Errorf("mismatches = %v, want nil alongside a plan error", m)
			}
		})
	}
}

// The three error categories must stay distinguishable by errors.Is, since
// each calls for a different response.
func TestErrorCategoriesAreDistinct(t *testing.T) {
	if errors.Is(ErrPlanMismatch, ErrFormatMismatch) || errors.Is(ErrFormatMismatch, ErrPlanMismatch) {
		t.Error("ErrPlanMismatch and ErrFormatMismatch are not distinguishable")
	}
	_, planErr := Compare([]Block{{0, 512, 1}}, nil)
	if errors.Is(planErr, ErrFormatMismatch) {
		t.Error("a plan mismatch matched ErrFormatMismatch")
	}
	_, _, fmtErr := ReadResponse(strings.NewReader("junk\n"))
	if errors.Is(fmtErr, ErrPlanMismatch) {
		t.Error("a format mismatch matched ErrPlanMismatch")
	}
}

func TestCompareBothEmptyIsClean(t *testing.T) {
	// A sync that wrote nothing plans no blocks, and that must pass rather
	// than trip the length check.
	m, err := Compare(nil, nil)
	if err != nil {
		t.Fatalf("Compare(nil, nil): %v", err)
	}
	if len(m) != 0 {
		t.Errorf("mismatches = %v, want none", m)
	}
}

func TestSummarizeMismatches(t *testing.T) {
	if got := SummarizeMismatches(nil); got != "no mismatches" {
		t.Errorf("empty summary = %q", got)
	}

	one := SummarizeMismatches([]Mismatch{{Offset: 4096, Length: 4096}})
	if !strings.Contains(one, "1 block(s)") || !strings.Contains(one, "4096 bytes") {
		t.Errorf("single summary = %q", one)
	}

	var many []Mismatch
	for i := 0; i < 10; i++ {
		many = append(many, Mismatch{Offset: uint64(i) * DefaultBlockSize, Length: DefaultBlockSize})
	}
	s := SummarizeMismatches(many)
	if !strings.Contains(s, "10 block(s)") {
		t.Errorf("summary = %q, want the count", s)
	}
	if !strings.Contains(s, "and 6 more") {
		t.Errorf("summary = %q, want the elision of the tail", s)
	}
}

// --- end to end -------------------------------------------------------------

// Over the actual mechanism: plan, hash on the "source", hash a
// deliberately damaged copy on the "target", pass the result through the
// wire format, and confirm the damaged block -- and only it -- is reported.
func TestEndToEndDetectsASingleCorruptedBlock(t *testing.T) {
	const bs = 4096
	source := make([]byte, 16*bs)
	for i := range source {
		source[i] = byte(i * 31)
	}
	written := []Range{{0, uint64(len(source))}}
	h := DefaultHeader(bs)

	want := PlanBlocks(written, bs)
	for i := range want {
		want[i].Digest = Sum(source[want[i].Offset : want[i].Offset+want[i].Length])
	}

	target := make([]byte, len(source))
	copy(target, source)
	target[5*bs+17] ^= 0x01 // one flipped bit inside block 5

	// The target side re-derives the plan from the same ranges rather than
	// being handed it, which is what the real helper does.
	var req bytes.Buffer
	if err := WriteRequest(&req, h, written); err != nil {
		t.Fatalf("WriteRequest: %v", err)
	}
	reqH, reqRanges, err := ReadRequest(&req)
	if err != nil {
		t.Fatalf("ReadRequest: %v", err)
	}
	if err := reqH.Check(h); err != nil {
		t.Fatalf("request header Check: %v", err)
	}
	got := PlanBlocks(reqRanges, reqH.BlockSize)
	for i := range got {
		got[i].Digest = Sum(target[got[i].Offset : got[i].Offset+got[i].Length])
	}

	var resp bytes.Buffer
	if err := WriteResponse(&resp, reqH, got); err != nil {
		t.Fatalf("WriteResponse: %v", err)
	}
	respH, reported, err := ReadResponse(&resp)
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	if err := respH.Check(h); err != nil {
		t.Fatalf("response header Check: %v", err)
	}

	m, err := Compare(want, reported)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if len(m) != 1 {
		t.Fatalf("got %d mismatches, want exactly 1: %v", len(m), m)
	}
	if m[0].Offset != 5*bs {
		t.Errorf("mismatch at offset %d, want %d", m[0].Offset, 5*bs)
	}
}

// A helper hashing at a different block size than vmsync must be caught by
// the header, BEFORE the digests are compared -- otherwise it surfaces as
// every block differing, i.e. as a corrupt replica.
func TestBlockSizeSkewIsCaughtByTheHeaderNotByCompare(t *testing.T) {
	data := make([]byte, 16<<10)
	for i := range data {
		data[i] = byte(i)
	}
	written := []Range{{0, uint64(len(data))}}

	vmsyncHeader := DefaultHeader(4096)
	helperHeader := DefaultHeader(8192) // stale helper, different default

	helperBlocks := PlanBlocks(written, helperHeader.BlockSize)
	for i := range helperBlocks {
		b := helperBlocks[i]
		helperBlocks[i].Digest = Sum(data[b.Offset : b.Offset+b.Length])
	}
	var resp bytes.Buffer
	if err := WriteResponse(&resp, helperHeader, helperBlocks); err != nil {
		t.Fatalf("WriteResponse: %v", err)
	}

	gotH, _, err := ReadResponse(&resp)
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	err = gotH.Check(vmsyncHeader)
	if !errors.Is(err, ErrFormatMismatch) {
		t.Fatalf("err = %v, want ErrFormatMismatch before any digest comparison", err)
	}
}
