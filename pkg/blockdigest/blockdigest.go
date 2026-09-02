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

// Package blockdigest defines the block plan, digest and wire exchange used
// by the pre-commit integrity check.
//
// The check works by hashing the same byte ranges on both sides and
// comparing digests rather than bytes. The source digests come for free:
// nbdsync's copy already holds every byte it sends in a buffer, so hashing
// there costs CPU it has to spare and no extra I/O at all. The target
// digests are computed ON the target host by vmsync-bridge-helper reading
// back through NBD, so what crosses the network is one short line per
// megabyte instead of the megabyte. That asymmetry is the entire point: a
// digest computed by whoever already has the bytes replaces moving them.
//
// # Three kinds of disagreement, deliberately kept apart
//
// Only one of them is evidence about the data, and conflating them is how a
// healthy replica gets discarded on false evidence -- the same distinction
// F3 drew for -verify, where "the comparison found a difference" and "the
// comparison could not be performed" had been sharing one code path.
//
//   - ErrFormatMismatch -- the two binaries disagree about the protocol:
//     format version, algorithm, or block size. This is version skew
//     between a vmsync and a vmsync-bridge-helper deployed at different
//     times, and it is a deployment problem.
//   - ErrPlanMismatch -- same protocol, but the digest lists do not
//     describe the same blocks. A tooling bug or a truncated transfer.
//   - a non-empty []Mismatch -- same protocol, same blocks, different
//     content. This one, and only this one, means the replica is wrong.
//
// # Why the exchange is self-describing
//
// The digest is a wire format between two independently deployed binaries.
// vmsync-bridge-helper is copied to every target host by hand, so an estate
// will inevitably run a helper older than its vmsync. If the exchange were
// bare digest lines, a helper hashing with a different algorithm or a
// different block size would produce digests that disagree everywhere --
// arriving as "this replica is corrupt" rather than "this helper is stale".
// Naming the algorithm and block size in a header line makes that skew a
// refused configuration instead of a false corruption report, and costs one
// line per exchange.
//
// # Why XXH64
//
// Non-adversarial error detection, so cryptographic strength buys nothing
// and costs 10-20x throughput. That leaves the choice between a CRC and a
// fast general-purpose hash, and two things decide it.
//
// Collision probability: 2^-64 against CRC32C's 2^-32, some 4.3 billion
// times more margin. Every comparison here is pairwise at a fixed offset
// (block N against block N), so there is no birthday effect and this is the
// flat per-block miss probability. It is not the number that decides whether
// a corruption event is caught -- missing an event needs every corrupted
// block to collide, which is 2^-64k for k bad blocks -- but the exposed
// single-block case is real, and the consequence of missing it is a corrupt
// replica that promotes clean at a disaster recovery.
//
// A CRC's answer to that is its guaranteed detection of structured errors,
// which is genuinely stronger than a probability -- but only within its
// Hamming-distance bound. CRC32C holds HD=4 up to 131,072 bits, which is
// exactly 16 KiB, and DefaultBlockSize is 1 MiB: 64x past it. Beyond the
// bound all that survives is what any degree-32 CRC gives at any length,
// and the comparison reduces to 2^-32 against 2^-64. At the block size the
// wire budget dictates, the CRC has no guarantee left to trade on.
//
// XXH64 rather than XXH3, despite XXH3 being wider and faster: XXH64 has
// produced identical output since 2014, while XXH3's output changed during
// development and only froze at xxHash v0.8.0. For a digest two
// independently deployed binaries must agree on, a settled definition is
// worth more than either width or speed -- and at ~13 GiB/s XXH64 is still
// an order of magnitude past any link this runs over, so nothing is lost.
//
// github.com/cespare/xxhash/v2 has no cgo and no transitive dependencies,
// so vmsync-bridge-helper stays a single static binary that cross-compiles
// -- which is the property that mattered, and the reason this is a
// dependency where pkg/nbdclient is hand-written. libnbd is cgo and would
// have broken it; xxhash does not.
package blockdigest

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"

	"github.com/cespare/xxhash/v2"
)

// Algo names a digest function on the wire.
//
// There is exactly one, and this type exists for compatibility rather than
// for configuration: it is what lets a future change of algorithm be
// diagnosed as skew instead of reported as corruption. It is deliberately
// not an operator-facing choice -- which hash is used is a wire-compatibility
// property of a matched vmsync/helper pair, not a preference, and exposing it
// as a flag would invite tuning a thing that must simply agree.
type Algo string

// AlgoXXH64 is XXH64 with a zero seed, as computed by
// github.com/cespare/xxhash/v2.
const AlgoXXH64 Algo = "xxh64"

// DefaultAlgo is what both sides use unless something in a future version
// negotiates otherwise.
const DefaultAlgo = AlgoXXH64

// Wire format identity. FormatMagic makes a stray line obviously not a
// header; FormatVersion changes only if the line or body grammar changes in
// a way an older parser would misread.
const (
	FormatMagic   = "vmsync-digest"
	FormatVersion = 1
)

// DefaultBlockSize is the granularity of one digest.
//
// A trade between the size of the exchange and the precision of a report. At
// 1 MiB a 50 GiB full sync plans ~51,200 blocks: ~1.2 MB in memory and
// ~1.5 MB as text on the wire, against the 50 GiB the check exists to avoid
// transferring. A mismatch still localises the damage to a megabyte, which
// is enough to tell "one bad region" from "scattered everywhere" -- the
// distinction that, in the 2026-09-01 incident, separated guest drift from
// corruption. Smaller blocks buy precision nobody has asked for at a cost
// that grows with disk size: 16 KiB blocks over 50 GiB would be ~82 MB of
// digests.
const DefaultBlockSize = 1 << 20

// Sum returns the digest of p under DefaultAlgo.
func Sum(p []byte) uint64 { return xxhash.Sum64(p) }

// ErrFormatMismatch reports that the two sides disagree about the protocol
// itself -- version, algorithm, or block size -- rather than about the data.
var ErrFormatMismatch = errors.New("digest format mismatch")

// ErrPlanMismatch reports that two digest lists do not describe the same
// blocks: different offsets, lengths, or counts.
//
// Kept distinct from a digest difference because it is not evidence about
// the data. It means the two sides disagree about what they were asked to
// hash -- a bug, or a truncated transfer -- and the correct response is to
// investigate rather than to declare the replica corrupt and discard it.
var ErrPlanMismatch = errors.New("digest block plans differ")

// Header is the first line of both halves of the exchange.
type Header struct {
	Version int
	Algo    Algo
	// BlockSize is the requester's declared MAXIMUM range length -- the
	// promise that no range in the request is longer than this, which is
	// what lets the responder size a single reusable buffer up front.
	//
	// Not a grid the responder splits on: it hashes the requested ranges
	// verbatim (see BlocksFromRanges). The name is kept because it is also
	// what a requester that plans on a fixed grid would put here, and the
	// two meanings coincide in that case.
	BlockSize uint64
}

// DefaultHeader is the header a current binary writes.
func DefaultHeader(blockSize uint64) Header {
	if blockSize == 0 {
		blockSize = DefaultBlockSize
	}
	return Header{Version: FormatVersion, Algo: DefaultAlgo, BlockSize: blockSize}
}

// BlocksFromRanges turns requested ranges into blocks to hash VERBATIM,
// with no coalescing, splitting or reordering, and validates each against
// the requester's declared maximum.
//
// This is what the responding side uses, and the verbatim part is the whole
// point. The requester sends the exact byte ranges it hashed -- for the
// sync path, the copy's own buffer-sized chunks, whose boundaries follow
// extent starts and so line up with no fixed grid. Restating those
// boundaries rather than having each side derive them means there is no plan
// to disagree about, which is a stronger guarantee than two sides computing
// a matching one. It is also why the responder must NOT run PlanBlocks over
// them: canonicalising the ranges would silently hash different bytes than
// the requester did, and every run would report a mismatch.
//
// A range longer than max is refused rather than split: the responder sizes
// one buffer from it, and the requester has already promised in its header
// that nothing exceeds it, so a longer range means the two disagree about
// the format.
func BlocksFromRanges(ranges []Range, max uint64) ([]Block, error) {
	if max == 0 || max > MaxBlockSize {
		return nil, fmt.Errorf("%w: declared maximum block size %d is out of range", ErrFormatMismatch, max)
	}
	blocks := make([]Block, 0, len(ranges))
	for i, r := range ranges {
		if r.Length == 0 {
			// Nothing to hash, and a zero-length block would be an odd thing
			// to report a digest for. Dropped rather than refused: the
			// requester's own planning already drops them, so the two agree.
			continue
		}
		if r.Length > max {
			return nil, fmt.Errorf("%w: requested range %d is %d+%d, longer than the declared maximum block size %d",
				ErrFormatMismatch, i, r.Offset, r.Length, max)
		}
		blocks = append(blocks, Block{Offset: r.Offset, Length: r.Length})
	}
	return blocks, nil
}

// RangesFromBlocks is the requesting side's counterpart: the block list it
// hashed, reduced to the offsets and lengths the responder must hash back.
func RangesFromBlocks(blocks []Block) []Range {
	ranges := make([]Range, 0, len(blocks))
	for _, b := range blocks {
		ranges = append(ranges, Range{Offset: b.Offset, Length: b.Length})
	}
	return ranges
}

// MaxRangeLength is the longest Length in blocks, which is what a requester
// declares as its header's BlockSize.
func MaxRangeLength(blocks []Block) uint64 {
	var max uint64
	for _, b := range blocks {
		if b.Length > max {
			max = b.Length
		}
	}
	return max
}

// MaxBlockSize bounds the block size a peer may ask for.
//
// Not a tuning limit but a sanity one: the receiving side allocates a buffer
// of one block, so a garbled or truncated header carrying an enormous value
// would otherwise turn a parse problem into an out-of-memory kill.
const MaxBlockSize = 64 << 20

// CheckSupported validates that h is something this binary can act on,
// without comparing it against a header of its own.
//
// This is the check the RECEIVING side of a request makes. It deliberately
// has no opinion about the block size beyond sanity: the requester dictates
// it and the responder obeys, which means the two cannot disagree about it
// at all rather than merely being able to detect that they have. The one
// remaining skew vector is the algorithm, which is exactly what the format
// version and Algo fields are here to catch.
func (h Header) CheckSupported() error {
	if h.Version != FormatVersion {
		return fmt.Errorf("%w: request uses format version %d, this binary speaks %d -- vmsync and vmsync-bridge-helper are different versions; redeploy the helper (see -bridge-helper-path)",
			ErrFormatMismatch, h.Version, FormatVersion)
	}
	if h.Algo != DefaultAlgo {
		return fmt.Errorf("%w: request asks for digest algorithm %q, this binary implements %q -- vmsync and vmsync-bridge-helper are different versions; redeploy the helper",
			ErrFormatMismatch, h.Algo, DefaultAlgo)
	}
	if h.BlockSize == 0 {
		return fmt.Errorf("%w: block size is zero", ErrFormatMismatch)
	}
	if h.BlockSize > MaxBlockSize {
		return fmt.Errorf("%w: block size %d exceeds the %d limit", ErrFormatMismatch, h.BlockSize, uint64(MaxBlockSize))
	}
	return nil
}

// Check verifies h agrees with want, returning ErrFormatMismatch otherwise.
//
// This is the check the side that SENT a request makes on the response: it
// confirms the responder honoured the header it was given, so a helper that
// silently hashed at some other granularity is caught here rather than
// surfacing as every block differing.
//
// The messages name both sides' values, because the operator reading them
// is being told to reconcile two deployments and needs to know which one to
// move.
func (h Header) Check(want Header) error {
	if h.Version != want.Version {
		return fmt.Errorf("%w: format version %d, expected %d -- vmsync and vmsync-bridge-helper are different versions; redeploy the helper (see -bridge-helper-path)",
			ErrFormatMismatch, h.Version, want.Version)
	}
	if h.Algo != want.Algo {
		return fmt.Errorf("%w: digest algorithm %q, expected %q -- vmsync and vmsync-bridge-helper are different versions; redeploy the helper",
			ErrFormatMismatch, h.Algo, want.Algo)
	}
	if h.BlockSize != want.BlockSize {
		return fmt.Errorf("%w: block size %d, expected %d -- the two sides would hash different blocks",
			ErrFormatMismatch, h.BlockSize, want.BlockSize)
	}
	return nil
}

// Range is a half-open byte range [Offset, Offset+Length).
type Range struct {
	Offset uint64
	Length uint64
}

// Block is one unit of digest: a range plus its hash. PlanBlocks leaves
// Digest zero for a caller to fill in as it hashes.
type Block struct {
	Offset uint64
	Length uint64
	Digest uint64
}

// Mismatch is one block whose two digests disagree.
type Mismatch struct {
	Offset uint64
	Length uint64
	Want   uint64
	Got    uint64
}

// PlanBlocks turns a set of ranges into a canonical list of blocks to
// digest: sorted by offset, overlapping and touching ranges coalesced, then
// each split on absolute multiples of blockSize.
//
// NOT used by the sync path. That path hashes the copy's own chunks and
// states their boundaries in the request, so there is no plan to derive on
// either side (see BlocksFromRanges). This exists for a caller that has no
// copy to piggyback on and therefore genuinely needs both ends to agree on
// a grid from the ranges alone -- the verify path, where the source is read
// separately rather than as a side effect of writing it.
//
// Canonical is the load-bearing word for such a caller. A single byte of
// disagreement about where blocks begin turns every comparison into a
// mismatch. Splitting on an ABSOLUTE grid rather than relative to each
// range's start is what prevents it: a given disk offset always lands in
// the same block no matter how the extents covering it happened to be
// coalesced.
//
// Only the written ranges are covered. Hashing whole aligned blocks that
// merely INTERSECT a written range would pull in neighbouring bytes this run
// never touched, and since the target's export serves a flattened view
// (overlay where written, base elsewhere) those bytes come from the base --
// so any pre-existing drift there would be reported as damage from this run.
// Restricting the plan to exactly what was written keeps the check answering
// "did this run land correctly" rather than the much broader "is this whole
// region correct", which is what a periodic -verify is for.
//
// A zero blockSize means DefaultBlockSize. Zero-length ranges are dropped
// rather than producing zero-length blocks.
func PlanBlocks(ranges []Range, blockSize uint64) []Block {
	if blockSize == 0 {
		blockSize = DefaultBlockSize
	}
	merged := coalesce(ranges)
	var blocks []Block
	for _, r := range merged {
		off := r.Offset
		end := r.Offset + r.Length
		for off < end {
			// Distance to the next absolute grid boundary above off. When
			// off is already on a boundary this is a full blockSize.
			next := off - off%blockSize + blockSize
			if next > end {
				next = end
			}
			blocks = append(blocks, Block{Offset: off, Length: next - off})
			off = next
		}
	}
	return blocks
}

// coalesce sorts and merges ranges, dropping empties. Touching ranges (one
// ending exactly where the next begins) merge too: they describe one
// contiguous region, and merging them makes the plan independent of how the
// extent list happened to be chopped up.
func coalesce(ranges []Range) []Range {
	in := make([]Range, 0, len(ranges))
	for _, r := range ranges {
		if r.Length == 0 {
			continue
		}
		in = append(in, r)
	}
	if len(in) == 0 {
		return nil
	}
	sort.Slice(in, func(i, j int) bool {
		if in[i].Offset != in[j].Offset {
			return in[i].Offset < in[j].Offset
		}
		return in[i].Length < in[j].Length
	})
	out := []Range{in[0]}
	for _, r := range in[1:] {
		last := &out[len(out)-1]
		lastEnd := last.Offset + last.Length
		if r.Offset <= lastEnd {
			if end := r.Offset + r.Length; end > lastEnd {
				last.Length = end - last.Offset
			}
			continue
		}
		out = append(out, r)
	}
	return out
}

// TotalBytes is how many bytes a plan covers, which is what the check reads
// back off the target.
func TotalBytes(blocks []Block) uint64 {
	var n uint64
	for _, b := range blocks {
		n += b.Length
	}
	return n
}

// WriteRequest writes the header and one "offset length" line per range:
// what vmsync sends to the helper's stdin.
//
// Ranges rather than the planned blocks, deliberately. It is what guarantees
// both sides run identical planning code instead of one side trusting a plan
// the other computed -- which is the failure this package's canonical
// planning exists to prevent.
//
// Text rather than a packed binary encoding, also deliberately: this travels
// over the same SSH command channel every other target operation uses, where
// it is captured as a string and shows up verbatim in a failure message. A
// format an operator can read in a log is worth far more than the bytes it
// costs, and the volume is irrelevant either way.
//
// On stdin rather than in argv: a full sync can plan thousands of extents,
// and argv has a length limit that an SSH command line makes tighter still.
func WriteRequest(w io.Writer, h Header, ranges []Range) error {
	bw := bufio.NewWriter(w)
	if err := writeHeader(bw, h); err != nil {
		return err
	}
	for _, r := range ranges {
		if _, err := fmt.Fprintf(bw, "%d %d\n", r.Offset, r.Length); err != nil {
			return err
		}
	}
	return bw.Flush()
}

// ReadRequest reads what WriteRequest wrote.
//
// The header is required, not optional. An absent one means the peer is a
// version that predates it, and treating that as "assume the defaults" is
// exactly how skew becomes silent -- the outcome this format exists to
// prevent.
func ReadRequest(r io.Reader) (Header, []Range, error) {
	sc := newScanner(r)
	h, err := readHeader(sc)
	if err != nil {
		return Header{}, nil, err
	}
	var ranges []Range
	line := 1
	for sc.Scan() {
		line++
		text := sc.Text()
		if text == "" {
			continue
		}
		fields, err := splitExactly(text, 2)
		if err != nil {
			return h, nil, fmt.Errorf("digest range line %d: %w", line, err)
		}
		off, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return h, nil, fmt.Errorf("digest range line %d: offset %q: %w", line, fields[0], err)
		}
		length, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return h, nil, fmt.Errorf("digest range line %d: length %q: %w", line, fields[1], err)
		}
		if off+length < off {
			return h, nil, fmt.Errorf("digest range line %d: %d+%d overflows", line, off, length)
		}
		ranges = append(ranges, Range{Offset: off, Length: length})
	}
	if err := sc.Err(); err != nil {
		return h, nil, fmt.Errorf("read digest ranges: %w", err)
	}
	return h, ranges, nil
}

// WriteResponse writes the header and one "offset length digest" line per
// block: what the helper prints on stdout.
func WriteResponse(w io.Writer, h Header, blocks []Block) error {
	bw := bufio.NewWriter(w)
	if err := writeHeader(bw, h); err != nil {
		return err
	}
	for _, b := range blocks {
		if _, err := fmt.Fprintf(bw, "%d %d %d\n", b.Offset, b.Length, b.Digest); err != nil {
			return err
		}
	}
	return bw.Flush()
}

// ReadResponse reads what WriteResponse wrote.
//
// Blank lines are skipped so a stray trailing newline is harmless. Any other
// malformed line is an error naming the line number: this parses the output
// of a remote command, where the realistic failure is not a corrupt digest
// but a shell diagnostic mixed into stdout, and silently skipping that would
// turn "the helper is missing" into "zero blocks, all agreed".
func ReadResponse(r io.Reader) (Header, []Block, error) {
	sc := newScanner(r)
	h, err := readHeader(sc)
	if err != nil {
		return Header{}, nil, err
	}
	var blocks []Block
	line := 1
	for sc.Scan() {
		line++
		text := sc.Text()
		if text == "" {
			continue
		}
		fields, err := splitExactly(text, 3)
		if err != nil {
			return h, nil, fmt.Errorf("digest line %d: %w", line, err)
		}
		off, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return h, nil, fmt.Errorf("digest line %d: offset %q: %w", line, fields[0], err)
		}
		length, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return h, nil, fmt.Errorf("digest line %d: length %q: %w", line, fields[1], err)
		}
		d, err := strconv.ParseUint(fields[2], 10, 64)
		if err != nil {
			return h, nil, fmt.Errorf("digest line %d: digest %q: %w", line, fields[2], err)
		}
		blocks = append(blocks, Block{Offset: off, Length: length, Digest: d})
	}
	if err := sc.Err(); err != nil {
		return h, nil, fmt.Errorf("read digests: %w", err)
	}
	return h, blocks, nil
}

func newScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	// Lines are short, but a long shell diagnostic on stdout should produce
	// a parse error rather than a scanner "token too long" failure.
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	return sc
}

func writeHeader(w io.Writer, h Header) error {
	_, err := fmt.Fprintf(w, "%s %d %s %d\n", FormatMagic, h.Version, h.Algo, h.BlockSize)
	return err
}

// readHeader consumes the first non-blank line as the header.
//
// A line that is not a header at all is reported as ErrFormatMismatch rather
// than as a parse error, and it quotes what arrived: the overwhelmingly
// likely cause is a helper too old to emit one, or a shell diagnostic where
// the helper's output should have been, and both are answered by looking at
// that text.
func readHeader(sc *bufio.Scanner) (Header, error) {
	for sc.Scan() {
		text := sc.Text()
		if text == "" {
			continue
		}
		fields, err := splitExactly(text, 4)
		if err != nil || fields[0] != FormatMagic {
			return Header{}, fmt.Errorf("%w: expected a %q header line, got %q -- if this is vmsync-bridge-helper output, the helper is too old or did not run",
				ErrFormatMismatch, FormatMagic, text)
		}
		version, err := strconv.Atoi(fields[1])
		if err != nil {
			return Header{}, fmt.Errorf("%w: format version %q is not a number", ErrFormatMismatch, fields[1])
		}
		blockSize, err := strconv.ParseUint(fields[3], 10, 64)
		if err != nil {
			return Header{}, fmt.Errorf("%w: block size %q is not a number", ErrFormatMismatch, fields[3])
		}
		return Header{Version: version, Algo: Algo(fields[2]), BlockSize: blockSize}, nil
	}
	if err := sc.Err(); err != nil {
		return Header{}, fmt.Errorf("read digest header: %w", err)
	}
	return Header{}, fmt.Errorf("%w: no header line at all -- vmsync-bridge-helper produced no output", ErrFormatMismatch)
}

// splitExactly splits text on runs of spaces and tabs and insists on exactly
// want fields.
//
// Hand-rolled rather than fmt.Sscanf or strings.Fields: Sscanf happily
// accepts a line with trailing junk after the fields it was asked for, which
// is precisely the shell-diagnostic-mixed-into-stdout case this must reject,
// and strings.Fields would allocate a slice per line for no benefit.
func splitExactly(text string, want int) ([]string, error) {
	fields := make([]string, 0, want)
	start := -1
	for i := 0; i <= len(text); i++ {
		atEnd := i == len(text)
		isSpace := !atEnd && (text[i] == ' ' || text[i] == '\t')
		switch {
		case !atEnd && !isSpace:
			if start < 0 {
				start = i
			}
		case start >= 0:
			if len(fields) == want {
				return nil, fmt.Errorf("expected %d fields, got more: %q", want, text)
			}
			fields = append(fields, text[start:i])
			start = -1
		}
	}
	if len(fields) != want {
		return nil, fmt.Errorf("expected %d fields, got %d: %q", want, len(fields), text)
	}
	return fields, nil
}

// Compare matches two digest lists block for block.
//
// Returns ErrPlanMismatch if they do not describe the same blocks, and
// otherwise the blocks whose digests differ -- empty meaning the check
// passed. Both lists must be in PlanBlocks order, which both producers use.
func Compare(want, got []Block) ([]Mismatch, error) {
	if len(want) != len(got) {
		return nil, fmt.Errorf("%w: %d blocks expected, %d reported", ErrPlanMismatch, len(want), len(got))
	}
	var out []Mismatch
	for i := range want {
		w, g := want[i], got[i]
		if w.Offset != g.Offset || w.Length != g.Length {
			return nil, fmt.Errorf("%w: block %d is %d+%d, reported as %d+%d",
				ErrPlanMismatch, i, w.Offset, w.Length, g.Offset, g.Length)
		}
		if w.Digest != g.Digest {
			out = append(out, Mismatch{Offset: w.Offset, Length: w.Length, Want: w.Digest, Got: g.Digest})
		}
	}
	return out, nil
}

// SummarizeMismatches renders a mismatch list for an operator, naming the
// count, the bytes involved and the first few offsets.
//
// The count-versus-spread distinction is the whole diagnostic value of doing
// this per block: "260 ranges scattered across 50 GiB" and "one 4 KiB range"
// call for opposite responses, and a bare "digests differ" would have told
// nobody which one they were looking at.
func SummarizeMismatches(m []Mismatch) string {
	if len(m) == 0 {
		return "no mismatches"
	}
	var bytes uint64
	for _, x := range m {
		bytes += x.Length
	}
	const show = 4
	s := fmt.Sprintf("%d block(s), %d bytes; first offsets:", len(m), bytes)
	for i, x := range m {
		if i == show {
			s += fmt.Sprintf(" ... and %d more", len(m)-show)
			break
		}
		s += fmt.Sprintf(" %d", x.Offset)
	}
	return s
}
