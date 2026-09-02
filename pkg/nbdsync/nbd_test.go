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

// This package's exported functions all take live *nbd.Libnbd handles --
// there is no interface seam to fake them behind, unlike most of the rest
// of this codebase's untestable-without-a-real-dependency surface (see
// pkg/libvirtsync, which needs a real libvirtd). The tests below drive the
// real code against a real, local qemu-nbd process instead of mocking
// anything -- qemu-nbd is already a hard runtime dependency of vmsync
// itself (every real sync starts one on the target host), so relying on it
// here doesn't introduce a new kind of dependency, just uses the existing
// one locally instead of over SSH. Every test that needs it calls
// requireQemuNBD first and skips (not fails) if the binary isn't on PATH,
// so this file is harmless in an environment that doesn't have it (this
// package has none of its own tests otherwise, so skipped is still a large
// improvement over the previous total absence of coverage here).
//
// Deliberately NOT covered by this pass, and why:
//   - Dropped-connection mid-copy/mid-compare (a real WAN interruption,
//     mirroring pkg/nbdbridge/local_test.go's own such tests): killing
//     qemu-nbd mid-transfer and asserting a bounded, non-hanging failure is
//     valuable but meaningfully more involved to get non-flaky here, since
//     it also depends on the timing of libnbd's own AIO completion
//     callbacks rather than just TCP-level behavior. Left as a natural
//     follow-up.
//   - The AIO drain-timeout path itself (both CopyExtentsTCP's and
//     CompareTCP's 30-second bounded wait for a stuck command to settle):
//     the timeout is a hardcoded constant, not a parameter, so reproducing
//     it deterministically would mean either waiting 30 real seconds per
//     test or a production-code change (making it configurable) beyond
//     this task's scope.
//   - negotiateBufferSize's "one side reports an unconstrained 0, the other
//     a real constraint" branch specifically: this needs control over what
//     maximum block size a server advertises, which a real, unconfigured
//     qemu-nbd instance doesn't expose a way to force. The test below
//     exercises the realistic path (whatever qemu-nbd actually advertises)
//     and confirms the one invariant that actually matters in production --
//     the result is never 0 -- rather than each individual switch branch.
//   - ChangedExtentsTCP's incremental (qemu:dirty-bitmap:) mode: needs a
//     qcow2 image with an actual persistent bitmap attached via `qemu-img
//     bitmap --add` and a matching `qemu-nbd -B`, a meaningfully bigger
//     setup than the plain base:allocation case covered here.
//   - compareTCP's own buffer-free-on-AIO-error branches specifically (as
//     opposed to CopyExtentsTCP's, which the tests below do cover): unlike
//     CopyExtentsTCP, compareTCP takes no caller-supplied extents and
//     requires both sides to already report the same size before it ever
//     builds a chunk, so there's no way to construct an in-bounds-looking
//     request that actually reads past either side's real end the way
//     CopyExtentsTCP's tests do -- the only way to make one of its AIO
//     reads genuinely fail would be a real dropped connection or a
//     corrupted export, the same "meaningfully more involved to get
//     non-flaky" class of test already excluded above.
package nbdsync

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"vmsync/pkg/blockdigest"

	nbd "libguestfs.org/libnbd"
)

// requireQemuNBD skips the calling test (rather than failing it) when
// qemu-nbd isn't available -- see this file's own top-of-file comment for
// why relying on it here is reasonable despite that possibility.
func requireQemuNBD(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("qemu-nbd"); err != nil {
		t.Skip("qemu-nbd not found on PATH -- skipping test that needs a real NBD server")
	}
}

// freeTCPPort reserves an OS-assigned local TCP port and immediately
// releases it, for handing to an external process that will bind it
// itself. Carries the same small, unavoidable reserve-then-release race as
// this project's own port allocation elsewhere (e.g. pkg/nbdbridge's local
// bridge listener) -- acceptable for a test that isn't running under
// adversarial port-scanning conditions.
func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a free tcp port: %v", err)
	}
	_, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		ln.Close()
		t.Fatalf("parse reserved port %q: %v", ln.Addr().String(), err)
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("release reserved port: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("convert reserved port %q to int: %v", portStr, err)
	}
	return port
}

// writeRawFile writes content to name under dir and returns the full path.
// The resulting file IS the raw-format NBD export's backing content --
// qemu-nbd --format=raw serves a file's bytes directly, with no container
// format on top, and no qemu-img involvement needed to create one.
func writeRawFile(t *testing.T, dir, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write raw test file %s: %v", path, err)
	}
	return path
}

// patternBytes returns size bytes cycling through every possible byte
// value. Preferred over random data for these tests: it's deterministic
// (no seed to manage) and touches every bit pattern, which makes a corrupt
// copy (a dropped byte, an off-by-one, a masked bit) far more likely to
// show up as a mismatch than a run of coincidentally-similar random bytes
// would.
func patternBytes(size int) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

// qemuNBDExport starts a real, local qemu-nbd process serving path (raw
// format) on a freshly reserved loopback TCP port, waits for it to actually
// be ready (via this package's own WaitForTCPExport, so this doubles as
// that function's first real exercise), and registers a cleanup that kills
// it. --persistent (unlike this project's own production qemu-nbd
// invocations in cmd/vmsync/main.go) is deliberate: production uses --fork
// because it launches qemu-nbd over a remote shell with no live process
// handle to hold onto afterward, so it needs the daemon to detach and a
// separate pidfile to stop it later. Here, cmd.Process IS that live handle,
// so forking would only make cleanup harder (it would kill the short-lived
// parent, not the detached child actually serving the port) -- and
// --persistent still allows multiple sequential connections against the
// same instance across a test that needs more than one.
func qemuNBDExport(t *testing.T, path string) int {
	t.Helper()
	port := freeTCPPort(t)

	cmd := exec.Command("qemu-nbd",
		"--format=raw",
		"--persistent",
		"--bind", "127.0.0.1",
		"--port", strconv.Itoa(port),
		path,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start qemu-nbd for %s: %v", path, err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	if err := WaitForTCPExport("127.0.0.1", port, "", 5*time.Second); err != nil {
		t.Fatalf("qemu-nbd for %s on port %d never became ready: %v (stderr: %s)", path, port, err, stderr.String())
	}
	return port
}

// mismatchesCover reports whether byteOffset falls inside any of the given
// mismatch ranges. Used instead of asserting an exact range count/offset
// list: CompareTCPCollect's chunk boundaries depend on whatever buffer size
// negotiateBufferSize actually negotiates against a real qemu-nbd server,
// which this test suite doesn't control -- but regardless of how finely
// (or coarsely) the image ends up chunked, a real differing byte must
// always be covered by at least one returned range.
func mismatchesCover(mismatches []MismatchRange, byteOffset uint64) bool {
	for _, m := range mismatches {
		if byteOffset >= m.Offset && byteOffset < m.Offset+m.Length {
			return true
		}
	}
	return false
}

// The digests CopyExtentsTCP collects must describe exactly the bytes it
// read, verified against an independent hash of the source file rather than
// against anything the copy itself computed.
//
// This is the source half of the pre-commit integrity check. If it is wrong,
// the check condemns healthy replicas -- so it is checked against the file
// on disk, three ways: every digest matches a hash of that range of the
// file, the digests cover exactly the copied bytes and nothing else, and
// they come back sorted and non-overlapping.
func TestCopyExtentsTCPCollectsDigestsOfWhatItRead(t *testing.T) {
	requireQemuNBD(t)
	dir := t.TempDir()

	const size = 3 * 1024 * 1024
	srcContent := patternBytes(size)
	srcPath := writeRawFile(t, dir, "src.raw", srcContent)
	dstPath := writeRawFile(t, dir, "dst.raw", make([]byte, size))

	srcPort := qemuNBDExport(t, srcPath)
	dstPort := qemuNBDExport(t, dstPath)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Two separate dirty extents with a gap, so "covers exactly the copied
	// bytes" is a real assertion rather than trivially the whole disk.
	const chunk = 1024 * 1024
	extents := []Extent{
		{Offset: 0, Length: chunk, Dirty: true},
		{Offset: chunk, Length: chunk, Dirty: false},
		{Offset: 2 * chunk, Length: chunk, Dirty: true},
	}
	written, digests, err := CopyExtentsTCP(ctx, "127.0.0.1", srcPort, "", "127.0.0.1", dstPort, "", extents, 4, true)
	if err != nil {
		t.Fatalf("CopyExtentsTCP: %v", err)
	}
	if written != 2*chunk {
		t.Fatalf("wrote %d bytes, want %d", written, 2*chunk)
	}
	if len(digests) == 0 {
		t.Fatal("collectDigests was set but no digests came back")
	}

	// Every digest must match an independent hash of that range of the file.
	for i, b := range digests {
		if b.Offset+b.Length > uint64(len(srcContent)) {
			t.Fatalf("digest %d covers %d+%d, past the end of a %d-byte source", i, b.Offset, b.Length, len(srcContent))
		}
		if want := blockdigest.Sum(srcContent[b.Offset : b.Offset+b.Length]); b.Digest != want {
			t.Errorf("digest %d (%d+%d) = %#x, want %#x from the source file itself", i, b.Offset, b.Length, b.Digest, want)
		}
	}

	// Sorted and non-overlapping: the AIO pipeline completes chunks out of
	// order, so this is the property that says the sort actually ran.
	for i := 1; i < len(digests); i++ {
		prev, cur := digests[i-1], digests[i]
		if cur.Offset < prev.Offset {
			t.Errorf("digests are not sorted: block %d starts at %d, after %d", i, cur.Offset, prev.Offset)
		}
		if cur.Offset < prev.Offset+prev.Length {
			t.Errorf("digest %d (%d+%d) overlaps the previous (%d+%d)", i, cur.Offset, cur.Length, prev.Offset, prev.Length)
		}
	}

	// Covering exactly the copied bytes matters in both directions: fewer
	// would leave part of the write unchecked, more would judge bytes this
	// run never wrote (which, against the target's flattened view, means
	// reporting pre-existing drift as damage from this run).
	if got := blockdigest.TotalBytes(digests); got != written {
		t.Errorf("digests cover %d bytes but the copy wrote %d", got, written)
	}
	for _, b := range digests {
		if b.Offset >= chunk && b.Offset < 2*chunk {
			t.Errorf("digest at %d falls inside the clean extent, which was never copied", b.Offset)
		}
	}

	// And no digests at all when it is not asked for -- the check has to be
	// genuinely off, not merely unused.
	if _, none, err := CopyExtentsTCP(ctx, "127.0.0.1", srcPort, "", "127.0.0.1", dstPort, "", extents, 4, false); err != nil {
		t.Fatalf("second copy: %v", err)
	} else if none != nil {
		t.Errorf("collectDigests was false but %d digests came back", len(none))
	}
}

// HashRangesTCP is the core of a digest verify, so it gets checked against
// an independent hash of the file rather than against anything the package
// itself computed.
//
// Two properties matter equally. The digests must be right, and the returned
// order must equal the PLAN's order -- not offset order, and not completion
// order. The AIO pipeline finishes chunks out of order, and the comparison
// against the target is matched positionally, so a reordering here would
// report every block as differing on a perfectly healthy replica.
func TestHashRangesTCPPreservesPlanOrderAndDigests(t *testing.T) {
	requireQemuNBD(t)
	dir := t.TempDir()

	const size = 3 * 1024 * 1024
	content := patternBytes(size)
	port := qemuNBDExport(t, writeRawFile(t, dir, "src.raw", content))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Deliberately NOT in offset order, and deliberately uneven in length,
	// so "returned in plan order" is distinguishable from "returned sorted".
	plan := []blockdigest.Block{
		{Offset: 2 * 1024 * 1024, Length: 512 * 1024},
		{Offset: 0, Length: 1024 * 1024},
		{Offset: 1536 * 1024, Length: 4096},
		{Offset: 1024 * 1024, Length: 512 * 1024},
	}

	got, err := HashRangesTCP(ctx, "127.0.0.1", port, "", plan, 4)
	if err != nil {
		t.Fatalf("HashRangesTCP: %v", err)
	}
	if len(got) != len(plan) {
		t.Fatalf("got %d digests for a %d-block plan", len(got), len(plan))
	}
	for i, want := range plan {
		if got[i].Offset != want.Offset || got[i].Length != want.Length {
			t.Errorf("block %d = %d+%d, want the plan's %d+%d -- order was not preserved",
				i, got[i].Offset, got[i].Length, want.Offset, want.Length)
			continue
		}
		if d := blockdigest.Sum(content[want.Offset : want.Offset+want.Length]); got[i].Digest != d {
			t.Errorf("block %d (%d+%d) digest = %#x, want %#x from the file itself",
				i, want.Offset, want.Length, got[i].Digest, d)
		}
	}

	// An empty plan must not even connect, so a verify with nothing to
	// compare does not depend on the export existing.
	if d, err := HashRangesTCP(ctx, "127.0.0.1", 1, "", nil, 4); err != nil || d != nil {
		t.Errorf("HashRangesTCP(empty plan) = %v, %v; want nil, nil without dialling", d, err)
	}
}

// The two halves must agree: a plan from CompareChunkPlanTCP, hashed on both
// sides with HashRangesTCP, must match for identical images and localise the
// damage for a tampered one. This is a digest verify end to end, minus the
// helper (which is exercised in cmd/vmsync-bridge-helper's own tests).
func TestCompareChunkPlanAndHashRangesAgreeEndToEnd(t *testing.T) {
	requireQemuNBD(t)
	dir := t.TempDir()

	const size = 3 * 1024 * 1024
	content := patternBytes(size)
	aPort := qemuNBDExport(t, writeRawFile(t, dir, "a.raw", content))

	tampered := make([]byte, size)
	copy(tampered, content)
	const badOffset = 2*1024*1024 + 4096
	tampered[badOffset] ^= 0xff
	bPort := qemuNBDExport(t, writeRawFile(t, dir, "b.raw", tampered))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	plan, err := CompareChunkPlanTCP(ctx, "127.0.0.1", aPort, "", "127.0.0.1", bPort, "")
	if err != nil {
		t.Fatalf("CompareChunkPlanTCP: %v", err)
	}
	if len(plan) == 0 {
		t.Fatal("empty plan for two fully-allocated images")
	}

	aDigests, err := HashRangesTCP(ctx, "127.0.0.1", aPort, "", plan, 4)
	if err != nil {
		t.Fatalf("HashRangesTCP(a): %v", err)
	}
	bDigests, err := HashRangesTCP(ctx, "127.0.0.1", bPort, "", plan, 4)
	if err != nil {
		t.Fatalf("HashRangesTCP(b): %v", err)
	}

	mismatches, err := blockdigest.Compare(aDigests, bDigests)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if len(mismatches) != 1 {
		t.Fatalf("got %d mismatches for a single tampered byte, want 1: %v", len(mismatches), mismatches)
	}
	m := mismatches[0]
	if badOffset < m.Offset || badOffset >= m.Offset+m.Length {
		t.Errorf("mismatch reported at %d+%d, which does not cover the tampered byte at %d",
			m.Offset, m.Length, badOffset)
	}

	// And the same plan over the same image must compare clean, so the
	// mismatch above is the tamper and not an artefact of the plan.
	if clean, err := blockdigest.Compare(aDigests, aDigests); err != nil || len(clean) != 0 {
		t.Errorf("comparing a digest list against itself gave %v, %v", clean, err)
	}
}

func TestCopyExtentsTCPRoundTrip(t *testing.T) {
	requireQemuNBD(t)
	dir := t.TempDir()

	const size = 3 * 1024 * 1024
	srcContent := patternBytes(size)
	srcPath := writeRawFile(t, dir, "src.raw", srcContent)
	dstPath := writeRawFile(t, dir, "dst.raw", make([]byte, size))

	srcPort := qemuNBDExport(t, srcPath)
	dstPort := qemuNBDExport(t, dstPath)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	extents := []Extent{{Offset: 0, Length: uint64(size), Dirty: true}}
	written, _, err := CopyExtentsTCP(ctx, "127.0.0.1", srcPort, "", "127.0.0.1", dstPort, "", extents, 4, false)
	if err != nil {
		t.Fatalf("CopyExtentsTCP: %v", err)
	}
	if written != uint64(size) {
		t.Fatalf("CopyExtentsTCP wrote %d bytes, want %d", written, size)
	}

	got, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatalf("read back destination file: %v", err)
	}
	if !bytes.Equal(got, srcContent) {
		t.Fatal("destination file content does not match source after CopyExtentsTCP")
	}

	// Cross-check with the package's own comparison logic too, over a
	// fresh pair of connections to the same two exports -- both functions
	// get exercised together, agreeing with each other and with the
	// independent, direct file-content check above.
	if err := CompareTCP(ctx, "127.0.0.1", srcPort, "", "127.0.0.1", dstPort, "", 4); err != nil {
		t.Fatalf("CompareTCP after a successful copy reported a mismatch: %v", err)
	}
}

func TestCopyExtentsTCPSkipsNonDirtyExtents(t *testing.T) {
	requireQemuNBD(t)
	dir := t.TempDir()

	const chunkSize = 1024 * 1024
	const size = 2 * chunkSize
	srcContent := patternBytes(size)
	srcPath := writeRawFile(t, dir, "src.raw", srcContent)
	originalDst := bytes.Repeat([]byte{0xAA}, size)
	dstPath := writeRawFile(t, dir, "dst.raw", originalDst)

	srcPort := qemuNBDExport(t, srcPath)
	dstPort := qemuNBDExport(t, dstPath)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Only the second half is marked dirty -- the first half must be left
	// entirely untouched on the target, regardless of how CopyExtentsTCP
	// internally re-chunks the dirty half for its own pipelining.
	extents := []Extent{
		{Offset: 0, Length: chunkSize, Dirty: false},
		{Offset: chunkSize, Length: chunkSize, Dirty: true},
	}
	written, _, err := CopyExtentsTCP(ctx, "127.0.0.1", srcPort, "", "127.0.0.1", dstPort, "", extents, 4, false)
	if err != nil {
		t.Fatalf("CopyExtentsTCP: %v", err)
	}
	if written != uint64(chunkSize) {
		t.Fatalf("CopyExtentsTCP reported %d bytes written, want exactly the dirty extent's %d bytes", written, chunkSize)
	}

	got, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatalf("read back destination file: %v", err)
	}
	if !bytes.Equal(got[:chunkSize], originalDst[:chunkSize]) {
		t.Error("the non-dirty extent was modified on the target -- it must be left exactly as it was")
	}
	if !bytes.Equal(got[chunkSize:], srcContent[chunkSize:]) {
		t.Error("the dirty extent was not correctly copied to the target")
	}
}

// TestCopyExtentsTCPReusesSlotsAcrossManyChunks specifically forces AIO
// slot recycling -- something none of this file's other CopyExtentsTCP
// tests do, since they all use a single extent no bigger than a handful of
// MiB against ioDepth=4, and negotiateBufferSize's real, environment-
// dependent result could plausibly be that size or larger, meaning every
// chunk gets its own never-reused slot. This uses four separate,
// non-overlapping, individually tiny (256KiB) extents instead of one big
// one: CopyExtentsTCP's own chunk-flattening loop turns each extent
// smaller than the negotiated buffer size into exactly one chunk
// regardless of what that size actually is (see its own per-extent
// "remaining > 0" loop), so this reliably produces 4 chunks contending for
// only 2 slots without this test needing to know or control the real
// negotiated buffer size at all. Verifies both that each region's own
// content survives correctly (a slot whose state doesn't get fully reset
// between reuses could hand a later chunk stale data from an earlier one)
// and that the untouched gaps between extents stay untouched (a slot
// reused with the wrong offset bookkeeping could write correct bytes to
// the wrong place instead).
func TestCopyExtentsTCPReusesSlotsAcrossManyChunks(t *testing.T) {
	requireQemuNBD(t)
	dir := t.TempDir()

	const fileSize = 2 * 1024 * 1024
	const extentSize = 256 * 1024
	srcContent := patternBytes(fileSize)
	srcPath := writeRawFile(t, dir, "src.raw", srcContent)
	dstPath := writeRawFile(t, dir, "dst.raw", make([]byte, fileSize))

	srcPort := qemuNBDExport(t, srcPath)
	dstPort := qemuNBDExport(t, dstPath)

	extents := []Extent{
		{Offset: 0, Length: extentSize, Dirty: true},
		{Offset: 512 * 1024, Length: extentSize, Dirty: true},
		{Offset: 1024 * 1024, Length: extentSize, Dirty: true},
		{Offset: 1536 * 1024, Length: extentSize, Dirty: true},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// ioDepth=2 against 4 chunks guarantees every slot is reused at least
	// once -- the exact path this test exists to exercise.
	written, _, err := CopyExtentsTCP(ctx, "127.0.0.1", srcPort, "", "127.0.0.1", dstPort, "", extents, 2, false)
	if err != nil {
		t.Fatalf("CopyExtentsTCP: %v", err)
	}
	if written != uint64(len(extents)*extentSize) {
		t.Fatalf("CopyExtentsTCP wrote %d bytes, want %d", written, len(extents)*extentSize)
	}

	got, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatalf("read back destination file: %v", err)
	}
	for _, ex := range extents {
		want := srcContent[ex.Offset : ex.Offset+ex.Length]
		gotRange := got[ex.Offset : ex.Offset+ex.Length]
		if !bytes.Equal(want, gotRange) {
			t.Errorf("extent at offset %d: content differs after a slot-reusing copy -- a reused slot likely handed this chunk stale or corrupted data", ex.Offset)
		}
	}
	// The gap between the first and second extent (256KiB..512KiB) was
	// never marked dirty -- it must still be exactly zero.
	gap := got[extentSize : 512*1024]
	if !bytes.Equal(gap, make([]byte, len(gap))) {
		t.Error("the untouched gap between extents was modified -- a reused slot likely wrote to the wrong offset")
	}
}

// TestCopyExtentsTCPFreesBufferOnSourceReadError exercises the "confirmed
// complete but failed" branch in the reads-finished loop -- reached only
// when an AIO read command completes with a nonzero errno, distinct from
// every other test in this file, which either succeeds outright or (for
// CompareTCP) finds a data mismatch, neither of which frees a buffer on a
// genuine AIO command error. An extent that runs past the end of the
// source export is a real, deterministic NBD protocol error (not a
// simulated one), without needing to kill a process mid-transfer -- see
// this file's own top-of-file comment for why that harder scenario is
// deliberately left uncovered.
func TestCopyExtentsTCPFreesBufferOnSourceReadError(t *testing.T) {
	requireQemuNBD(t)
	dir := t.TempDir()

	const size = 1024 * 1024
	srcPath := writeRawFile(t, dir, "src.raw", patternBytes(size))
	dstPath := writeRawFile(t, dir, "dst.raw", make([]byte, size))

	srcPort := qemuNBDExport(t, srcPath)
	dstPort := qemuNBDExport(t, dstPath)

	// Starts 100 bytes before the end of the export and reads 10000 bytes
	// -- comfortably past size, and far smaller than any buffer size
	// negotiateBufferSize could plausibly negotiate, so this always ends
	// up as a single out-of-bounds chunk rather than being split.
	extents := []Extent{{Offset: uint64(size) - 100, Length: 10000, Dirty: true}}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _, err := CopyExtentsTCP(ctx, "127.0.0.1", srcPort, "", "127.0.0.1", dstPort, "", extents, 4, false)
	if err == nil {
		t.Fatal("CopyExtentsTCP with an out-of-bounds source read returned nil, want an error")
	}
	if !strings.Contains(err.Error(), "pread") {
		t.Errorf("error = %q, want it to mention the failed pread", err.Error())
	}
}

// TestCopyExtentsTCPFreesBufferOnTargetWriteError isolates the "writes
// that finished" loop's own opErr branch specifically -- distinct from the
// source-read-error test above, which never reaches the write side at
// all. The target export is deliberately smaller than the source, so
// every chunk's read (always within the source's own bounds) succeeds,
// but writing that same range back out eventually crosses the target's
// smaller size and fails there instead -- regardless of exactly where
// negotiateBufferSize happens to draw chunk boundaries, some chunk's
// write must cross that size difference.
func TestCopyExtentsTCPFreesBufferOnTargetWriteError(t *testing.T) {
	requireQemuNBD(t)
	dir := t.TempDir()

	const srcSize = 1024 * 1024
	const dstSize = 512 * 1024
	srcPath := writeRawFile(t, dir, "src.raw", patternBytes(srcSize))
	dstPath := writeRawFile(t, dir, "dst.raw", make([]byte, dstSize))

	srcPort := qemuNBDExport(t, srcPath)
	dstPort := qemuNBDExport(t, dstPath)

	extents := []Extent{{Offset: 0, Length: srcSize, Dirty: true}}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _, err := CopyExtentsTCP(ctx, "127.0.0.1", srcPort, "", "127.0.0.1", dstPort, "", extents, 4, false)
	if err == nil {
		t.Fatal("CopyExtentsTCP writing past a smaller target's end returned nil, want an error")
	}
	if !strings.Contains(err.Error(), "pwrite") {
		t.Errorf("error = %q, want it to mention the failed pwrite", err.Error())
	}
}

func TestCompareTCPIdenticalReturnsNil(t *testing.T) {
	requireQemuNBD(t)
	dir := t.TempDir()

	content := patternBytes(2 * 1024 * 1024)
	aPath := writeRawFile(t, dir, "a.raw", content)
	bPath := writeRawFile(t, dir, "b.raw", append([]byte(nil), content...))

	aPort := qemuNBDExport(t, aPath)
	bPort := qemuNBDExport(t, bPath)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := CompareTCP(ctx, "127.0.0.1", aPort, "", "127.0.0.1", bPort, "", 4); err != nil {
		t.Fatalf("CompareTCP on byte-for-byte identical images returned an error: %v", err)
	}
}

func TestCompareTCPReportsMismatch(t *testing.T) {
	requireQemuNBD(t)
	dir := t.TempDir()

	const size = 2 * 1024 * 1024
	content := patternBytes(size)
	aPath := writeRawFile(t, dir, "a.raw", content)
	diverged := append([]byte(nil), content...)
	diverged[0] ^= 0xFF // the very first byte -- always inside whatever the first chunk turns out to be, regardless of chunk size
	bPath := writeRawFile(t, dir, "b.raw", diverged)

	aPort := qemuNBDExport(t, aPath)
	bPort := qemuNBDExport(t, bPath)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := CompareTCP(ctx, "127.0.0.1", aPort, "", "127.0.0.1", bPort, "", 4)
	if err == nil {
		t.Fatal("CompareTCP on images differing at offset 0 returned nil, want a mismatch error")
	}
	if !strings.Contains(err.Error(), "images differ") {
		t.Errorf("CompareTCP error %q does not report a mismatch", err.Error())
	}
	if !strings.Contains(err.Error(), "offset=0") {
		t.Errorf("CompareTCP error %q does not report the mismatch's offset as 0, where the only differing byte is", err.Error())
	}
}

func TestCompareTCPCollectFindsAllMismatches(t *testing.T) {
	requireQemuNBD(t)
	dir := t.TempDir()

	const size = 3 * 1024 * 1024
	content := patternBytes(size)
	aPath := writeRawFile(t, dir, "a.raw", content)
	diverged := append([]byte(nil), content...)
	diverged[0] ^= 0xFF      // first byte of the image
	diverged[size-1] ^= 0xFF // last byte of the image
	bPath := writeRawFile(t, dir, "b.raw", diverged)

	aPort := qemuNBDExport(t, aPath)
	bPort := qemuNBDExport(t, bPath)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	mismatches, err := CompareTCPCollect(ctx, "127.0.0.1", aPort, "", "127.0.0.1", bPort, "", 4)
	if err != nil {
		t.Fatalf("CompareTCPCollect: %v", err)
	}
	if len(mismatches) == 0 {
		t.Fatal("CompareTCPCollect found no mismatches despite two differing bytes")
	}
	// Regardless of how the pipeline happens to have chunked the image,
	// CompareTCPCollect's documented contract is that it never stops at
	// the first mismatch the way CompareTCP does -- its returned ranges,
	// taken together, must account for every differing byte, including
	// the one at the very end, which "abort on first mismatch" behavior
	// would never even have reached.
	if !mismatchesCover(mismatches, 0) {
		t.Errorf("CompareTCPCollect's mismatches %v do not cover the differing byte at offset 0", mismatches)
	}
	if !mismatchesCover(mismatches, uint64(size-1)) {
		t.Errorf("CompareTCPCollect's mismatches %v do not cover the differing byte at the last offset %d -- looks like it stopped early instead of scanning the whole image", mismatches, size-1)
	}
}

// TestDiffSubRanges is a pure-function regression test for the bug this was
// extracted to fix: compareTCP used to report any mismatch inside an AIO
// chunk as spanning the chunk's entire offset/length, which the former -verify=online's
// dirty-bitmap reconciliation (overlapsAnyExtent) would then discard
// wholesale if a guest write touched the chunk anywhere -- silently hiding
// real corruption elsewhere in the same wide chunk. Needs no qemu-nbd at
// all.
func TestDiffSubRanges(t *testing.T) {
	const g = 4 // small granularity so cases stay readable

	cases := []struct {
		name       string
		baseOffset uint64
		a, b       []byte
		want       []MismatchRange
	}{
		{
			name:       "no difference",
			baseOffset: 100,
			a:          []byte{1, 2, 3, 4, 5, 6, 7, 8},
			b:          []byte{1, 2, 3, 4, 5, 6, 7, 8},
			want:       nil,
		},
		{
			name:       "single differing sub-block in the middle",
			baseOffset: 100,
			a:          []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12},
			b:          []byte{1, 2, 3, 4, 0, 6, 7, 8, 9, 10, 11, 12},
			want:       []MismatchRange{{Offset: 104, Length: 4}},
		},
		{
			name:       "multiple non-adjacent differing sub-blocks stay separate",
			baseOffset: 0,
			a:          []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12},
			b:          []byte{0, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 0},
			want: []MismatchRange{
				{Offset: 0, Length: 4},
				{Offset: 8, Length: 4},
			},
		},
		{
			// One byte differs inside each of two adjacent sub-blocks --
			// diffSubRanges must merge them into a single range spanning
			// both blocks rather than reporting two separate ones.
			name:       "contiguous differing run spanning multiple sub-blocks merges into one range",
			baseOffset: 0,
			a:          []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12},
			b:          []byte{1, 2, 3, 4, 5, 0, 7, 8, 9, 10, 0, 12},
			want:       []MismatchRange{{Offset: 4, Length: 8}},
		},
		{
			name:       "difference at the very start of the buffer",
			baseOffset: 0,
			a:          []byte{1, 2, 3, 4, 5, 6, 7, 8},
			b:          []byte{0, 2, 3, 4, 5, 6, 7, 8},
			want:       []MismatchRange{{Offset: 0, Length: 4}},
		},
		{
			name:       "difference at the very end of the buffer",
			baseOffset: 0,
			a:          []byte{1, 2, 3, 4, 5, 6, 7, 8},
			b:          []byte{1, 2, 3, 4, 5, 6, 7, 0},
			want:       []MismatchRange{{Offset: 4, Length: 4}},
		},
		{
			name:       "buffer length not evenly divisible by granularity -- final partial sub-block",
			baseOffset: 0,
			a:          []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10},
			b:          []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 0},
			want:       []MismatchRange{{Offset: 8, Length: 2}},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := diffSubRanges(c.baseOffset, c.a, c.b, g)
			if len(got) != len(c.want) {
				t.Fatalf("diffSubRanges(...) = %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("diffSubRanges(...) = %v, want %v", got, c.want)
				}
			}
		})
	}
}

// TestStalled is a pure-function regression test for the bug this was
// extracted to fix: CopyExtentsTCP's and compareTCP's AIO pipelines only
// ever checked ctx.Done() between iterations, so a half-open TCP connection
// -- one where the OS-level socket stays open but the remote end never
// sends or acknowledges anything again -- left Poll returning cleanly on
// every 10ms timeout and AioCommandCompleted never confirming a command
// either way, spinning the loop forever with no error for anything to
// observe. Needs no qemu-nbd, and deliberately avoids a real 120-second
// sleep by driving the pure time comparison directly.
func TestStalled(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const timeout = 120 * time.Second

	cases := []struct {
		name         string
		lastProgress time.Time
		now          time.Time
		want         bool
	}{
		{
			name:         "well within the timeout",
			lastProgress: base,
			now:          base.Add(1 * time.Second),
			want:         false,
		},
		{
			name:         "just under the timeout",
			lastProgress: base,
			now:          base.Add(timeout - time.Millisecond),
			want:         false,
		},
		{
			name:         "exactly at the timeout counts as stalled",
			lastProgress: base,
			now:          base.Add(timeout),
			want:         true,
		},
		{
			name:         "well past the timeout",
			lastProgress: base,
			now:          base.Add(timeout + time.Hour),
			want:         true,
		},
		{
			name:         "no time elapsed at all -- fresh progress",
			lastProgress: base,
			now:          base,
			want:         false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := stalled(c.lastProgress, c.now, timeout)
			if got != c.want {
				t.Fatalf("stalled(lastProgress, now, %s) = %v, want %v", timeout, got, c.want)
			}
		})
	}
}

// TestNegotiateBufferSizeReturnsPositiveValue exercises negotiateBufferSize
// directly (it's unexported, hence this test living in-package) against two
// real, connected NBD handles. See this file's own top-of-file comment for
// why only the realistic path -- whatever qemu-nbd actually advertises --
// is covered here, not each individual switch branch in its source.
func TestNegotiateBufferSizeReturnsPositiveValue(t *testing.T) {
	requireQemuNBD(t)
	dir := t.TempDir()

	aPath := writeRawFile(t, dir, "a.raw", make([]byte, 1024*1024))
	bPath := writeRawFile(t, dir, "b.raw", make([]byte, 1024*1024))
	aPort := qemuNBDExport(t, aPath)
	bPort := qemuNBDExport(t, bPath)

	a, err := nbd.Create()
	if err != nil {
		t.Fatalf("create source nbd handle: %v", err)
	}
	defer a.Close()
	if err := a.ConnectTcp("127.0.0.1", strconv.Itoa(aPort)); err != nil {
		t.Fatalf("connect source nbd handle: %v", err)
	}

	b, err := nbd.Create()
	if err != nil {
		t.Fatalf("create target nbd handle: %v", err)
	}
	defer b.Close()
	if err := b.ConnectTcp("127.0.0.1", strconv.Itoa(bPort)); err != nil {
		t.Fatalf("connect target nbd handle: %v", err)
	}

	got := negotiateBufferSize(a, b, "test-a", "test-b")
	if got == 0 {
		t.Fatal("negotiateBufferSize returned 0 against two live NBD connections -- this would spin a chunking loop forever without transferring a single byte")
	}
	if got > maxNegotiatedBufferSize {
		t.Fatalf("negotiateBufferSize returned %d, which exceeds maxNegotiatedBufferSize (%d) -- the cap is not actually being applied on the real code path", got, maxNegotiatedBufferSize)
	}
}

// TestClampBufferSize is a pure-function regression test for the fix this
// was extracted to make possible: negotiateBufferSize used to trust
// whatever a remote NBD server's advertised maximum block size worked out
// to, with no ceiling -- letting a misconfigured or buggy server drive an
// oversized native (cgo) AIO buffer allocation that could abort the whole
// process under memory pressure instead of failing cleanly. Needs no
// qemu-nbd at all.
func TestClampBufferSize(t *testing.T) {
	cases := []struct {
		name  string
		size  uint64
		limit uint64
		want  uint64
	}{
		{name: "well under the limit -- unchanged", size: 1024, limit: maxNegotiatedBufferSize, want: 1024},
		{name: "exactly at the limit -- unchanged", size: maxNegotiatedBufferSize, limit: maxNegotiatedBufferSize, want: maxNegotiatedBufferSize},
		{name: "just over the limit -- capped", size: maxNegotiatedBufferSize + 1, limit: maxNegotiatedBufferSize, want: maxNegotiatedBufferSize},
		{name: "wildly over the limit -- capped, not zeroed or wrapped", size: 4 * 1024 * 1024 * 1024, limit: maxNegotiatedBufferSize, want: maxNegotiatedBufferSize},
		{name: "zero size -- stays zero, never raised to the limit", size: 0, limit: maxNegotiatedBufferSize, want: 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := clampBufferSize(c.size, c.limit); got != c.want {
				t.Errorf("clampBufferSize(%d, %d) = %d, want %d", c.size, c.limit, got, c.want)
			}
		})
	}
}

// TestNextExtentScanOffset is a pure-function regression test for the bug
// this was extracted to fix: ChangedExtentsTCP used to advance its scan
// offset by the REQUESTED chunk size regardless of what the server's
// BLOCK_STATUS reply actually described, silently dropping any sub-range
// a server declined to describe (NBD_CMD_BLOCK_STATUS explicitly permits
// replying with less than requested) and re-describing/duplicating any
// sub-range a final extent overran into. Needs no qemu-nbd at all, unlike
// the rest of this file -- see nbd.go's own doc comment on this function.
func TestNextExtentScanOffset(t *testing.T) {
	cases := []struct {
		name                                          string
		requestedOffset, requestedChunk, describedEnd uint64
		wantOffset                                    uint64
		wantErr                                       bool
	}{
		{
			name:            "server described the full requested range",
			requestedOffset: 1000,
			requestedChunk:  500,
			describedEnd:    1500,
			wantOffset:      1500,
		},
		{
			name:            "server described less than requested -- must not skip the remainder",
			requestedOffset: 1000,
			requestedChunk:  500,
			describedEnd:    1200,
			wantOffset:      1200,
		},
		{
			name:            "final extent overran the requested range -- must not re-describe it",
			requestedOffset: 1000,
			requestedChunk:  500,
			describedEnd:    1800,
			wantOffset:      1800,
		},
		{
			name:            "server described nothing at all -- must error, not silently skip or spin",
			requestedOffset: 1000,
			requestedChunk:  500,
			describedEnd:    1000,
			wantErr:         true,
		},
		{
			name:            "describedEnd behind the request entirely -- must error",
			requestedOffset: 1000,
			requestedChunk:  500,
			describedEnd:    900,
			wantErr:         true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := nextExtentScanOffset(c.requestedOffset, c.requestedChunk, c.describedEnd)
			if c.wantErr {
				if err == nil {
					t.Fatalf("nextExtentScanOffset(%d, %d, %d) = (%d, nil), want an error", c.requestedOffset, c.requestedChunk, c.describedEnd, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("nextExtentScanOffset(%d, %d, %d) unexpected error: %v", c.requestedOffset, c.requestedChunk, c.describedEnd, err)
			}
			if got != c.wantOffset {
				t.Fatalf("nextExtentScanOffset(%d, %d, %d) = %d, want %d", c.requestedOffset, c.requestedChunk, c.describedEnd, got, c.wantOffset)
			}
		})
	}
}

// TestIsDirtyExtent is a pure-function regression test for the bug this
// was extracted to fix: the full-mode (base:allocation) decision used to
// enumerate the exact currently-known flag combinations (0, 1, 2, 3)
// instead of masking just the hole bit, so any additional status bit a
// future or non-qemu NBD server ever set on an otherwise-allocated extent
// would silently turn it into a skipped one. Needs no qemu-nbd at all.
func TestIsDirtyExtent(t *testing.T) {
	cases := []struct {
		name        string
		flags       uint32
		incremental bool
		want        bool
	}{
		{"full: allocated, non-zero -> dirty", 0, false, true},
		{"full: hole -> not dirty", 1, false, false},
		{"full: allocated, zero-flagged -> dirty", 2, false, true},
		{"full: hole+zero -> not dirty", 3, false, false},
		{"full: unrecognized extra bit on an allocated extent -> still dirty", 4, false, true},
		{"full: unrecognized extra bit combined with the hole bit -> still not dirty", 5, false, false},
		{"incremental: dirty bit set -> dirty", 1, true, true},
		{"incremental: dirty bit clear -> not dirty", 0, true, false},
		{"incremental: unrecognized extra bit, dirty bit set -> still dirty", 3, true, true},
		{"incremental: unrecognized extra bit, dirty bit clear -> still not dirty", 2, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isDirtyExtent(c.flags, c.incremental); got != c.want {
				t.Fatalf("isDirtyExtent(%d, incremental=%v) = %v, want %v", c.flags, c.incremental, got, c.want)
			}
		})
	}
}

// TestChangedExtentsTCPDetectsAllocatedRegion covers the plain
// base:allocation (non-incremental) path -- see this file's own
// top-of-file comment for why the incremental qemu:dirty-bitmap: path
// isn't covered here.
func TestChangedExtentsTCPDetectsAllocatedRegion(t *testing.T) {
	requireQemuNBD(t)
	dir := t.TempDir()

	const chunkSize = 1024 * 1024
	const size = 3 * chunkSize
	path := filepath.Join(dir, "sparse.raw")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create sparse test file: %v", err)
	}
	if err := f.Truncate(size); err != nil {
		f.Close()
		t.Fatalf("truncate sparse test file to %d: %v", size, err)
	}
	// Only the middle chunk is actually written -- the rest stays a real,
	// unallocated hole on any filesystem that supports sparse files
	// (ext4/xfs do; this package only ever runs on Linux in practice).
	if _, err := f.WriteAt(patternBytes(chunkSize), chunkSize); err != nil {
		f.Close()
		t.Fatalf("write allocated region: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close sparse test file: %v", err)
	}

	port := qemuNBDExport(t, path)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	extents, gotSize, dirty, err := ChangedExtentsTCP(ctx, "127.0.0.1", port, "", "", false)
	if err != nil {
		t.Fatalf("ChangedExtentsTCP: %v", err)
	}
	if gotSize != uint64(size) {
		t.Fatalf("ChangedExtentsTCP reported size %d, want %d", gotSize, size)
	}
	if dirty == 0 {
		t.Fatal("ChangedExtentsTCP reported zero dirty entries despite a real allocated region")
	}

	// Structural sanity: the returned extents must partition [0, size)
	// exactly, with no gaps or overlaps, regardless of exactly how finely
	// qemu-nbd happens to have split them up.
	var offset, dirtyBytes uint64
	for _, ex := range extents {
		if ex.Offset != offset {
			t.Fatalf("extent list has a gap or overlap: expected next offset %d, got %d", offset, ex.Offset)
		}
		if ex.Dirty {
			dirtyBytes += ex.Length
		}
		offset += ex.Length
	}
	if offset != uint64(size) {
		t.Fatalf("extent list covers %d bytes total, want the full image size %d", offset, size)
	}
	if dirtyBytes < uint64(chunkSize) {
		t.Errorf("dirty extents total %d bytes, want at least the %d bytes actually written", dirtyBytes, chunkSize)
	}
	if dirtyBytes >= uint64(size) {
		t.Errorf("dirty extents total %d bytes, want less than the full image size %d -- the untouched regions should not be reported as allocated", dirtyBytes, size)
	}
}

func TestWaitForTCPExportSucceedsOnceListening(t *testing.T) {
	requireQemuNBD(t)
	dir := t.TempDir()
	path := writeRawFile(t, dir, "export.raw", make([]byte, 4096))
	// qemuNBDExport already calls WaitForTCPExport once internally to know
	// the server is up before returning -- this call is a second,
	// independent one against an export already confirmed ready.
	port := qemuNBDExport(t, path)

	if err := WaitForTCPExport("127.0.0.1", port, "", 5*time.Second); err != nil {
		t.Fatalf("WaitForTCPExport against an already-listening export: %v", err)
	}
}

func TestWaitForTCPExportTimesOutWhenNothingListening(t *testing.T) {
	port := freeTCPPort(t) // reserved and released -- guaranteed nothing is listening on it

	start := time.Now()
	err := WaitForTCPExport("127.0.0.1", port, "", 500*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("WaitForTCPExport succeeded against a port nothing is listening on")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("WaitForTCPExport took %s to give up on a 500ms timeout -- looks hung rather than just imprecise", elapsed)
	}
	// Regression pin: this used to always return a generic "nbd export not
	// ready on host:port" message, discarding the real connection failure
	// (here, a TCP-level refusal, since nothing is listening) every single
	// iteration. "refused" is what a real connection attempt against a
	// closed port actually produces -- if this ever regresses back to the
	// generic message, this substring stops appearing.
	if !strings.Contains(strings.ToLower(err.Error()), "refused") {
		t.Errorf("WaitForTCPExport error = %q, want it to mention the real underlying connection failure (e.g. \"connection refused\"), not just a generic \"not ready\" message", err)
	}
}

// The three tests below need no real NBD server at all: ChangedExtentsTCP,
// CopyExtentsTCP, and compareTCP (behind CompareTCP/CompareTCPCollect) all
// check ctx.Err() as their very first action, before creating any NBD
// handle or touching the network. Deliberately unreachable host/port values
// prove that check actually ran first -- if it were ever removed or
// reordered, these would hang or fail with a connection error instead of
// promptly returning the context's own error.
func TestCopyExtentsTCPReturnsImmediatelyOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := CopyExtentsTCP(ctx, "127.0.0.1", 1, "", "127.0.0.1", 2, "", nil, 1, false)
	if err == nil {
		t.Fatal("CopyExtentsTCP with an already-cancelled context returned a nil error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("CopyExtentsTCP with an already-cancelled context returned %v, want it to be context.Canceled", err)
	}
}

func TestCompareTCPReturnsImmediatelyOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := CompareTCP(ctx, "127.0.0.1", 1, "", "127.0.0.1", 2, "", 1)
	if err == nil {
		t.Fatal("CompareTCP with an already-cancelled context returned a nil error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("CompareTCP with an already-cancelled context returned %v, want it to be context.Canceled", err)
	}
}

func TestChangedExtentsTCPReturnsImmediatelyOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, _, err := ChangedExtentsTCP(ctx, "127.0.0.1", 1, "", "", false)
	if err == nil {
		t.Fatal("ChangedExtentsTCP with an already-cancelled context returned a nil error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("ChangedExtentsTCP with an already-cancelled context returned %v, want it to be context.Canceled", err)
	}
}

// TestDueForProgressLog pins the throttle both long-running pipelines share.
//
// Driven as a pure time comparison, like TestStalled above: needs no
// qemu-nbd, and no real minute of waiting.
//
// The `done` case is the one worth having. Without it a copy or compare that
// finishes shortly after a tick would never log its own completion, so the
// last line an operator saw would be a stale percentage and the operation
// would look like it stopped partway rather than finished.
func TestDueForProgressLog(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name    string
		lastLog time.Time
		now     time.Time
		done    bool
		want    bool
	}{
		{
			name:    "a second in is far too soon",
			lastLog: base,
			now:     base.Add(time.Second),
			want:    false,
		},
		{
			name:    "just under the interval stays quiet",
			lastLog: base,
			now:     base.Add(progressLogInterval - time.Millisecond),
			want:    false,
		},
		{
			name:    "exactly at the interval logs",
			lastLog: base,
			now:     base.Add(progressLogInterval),
			want:    true,
		},
		{
			name:    "well past the interval logs",
			lastLog: base,
			now:     base.Add(progressLogInterval + time.Hour),
			want:    true,
		},
		{
			name:    "done forces a line however recent the last one was",
			lastLog: base,
			now:     base,
			done:    true,
			want:    true,
		},
		{
			name:    "not done and no time elapsed stays quiet",
			lastLog: base,
			now:     base,
			want:    false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dueForProgressLog(tc.lastLog, tc.now, tc.done); got != tc.want {
				t.Errorf("dueForProgressLog(%v, %v, done=%v) = %v, want %v",
					tc.lastLog, tc.now, tc.done, got, tc.want)
			}
		})
	}
}

// The interval is a deliberate choice, not an accident of tuning: a sync
// runs for tens of minutes, so a per-second line is thousands of entries
// that bury everything else in a journal at exactly the moment somebody is
// reading it to find out what went wrong. Guarded so a well-meaning "make
// progress more responsive" change has to argue with this comment first.
func TestProgressLogIntervalIsCoarse(t *testing.T) {
	if progressLogInterval < 30*time.Second {
		t.Errorf("progressLogInterval = %s; anything under 30s makes a long sync's log unreadable, "+
			"which is the problem this constant was introduced to fix", progressLogInterval)
	}
	// And it must stay well inside the stall timeout, or a healthy-but-slow
	// operation could trip the stall check before it ever reports progress.
	if progressLogInterval >= noProgressTimeout {
		t.Errorf("progressLogInterval (%s) must be shorter than noProgressTimeout (%s)",
			progressLogInterval, noProgressTimeout)
	}
}

// zr builds a base:allocation-style extent list from (offset, length, zero)
// triples, for the planner tests below.
func zr(triples ...[3]uint64) []Extent {
	var out []Extent
	for _, t := range triples {
		out = append(out, Extent{Offset: t[0], Length: t[1], Zero: t[2] == 1})
	}
	return out
}

func TestZeroRangesCoalesces(t *testing.T) {
	got := zeroRanges(zr(
		[3]uint64{0, 100, 1},
		[3]uint64{100, 100, 1}, // adjacent -> merges
		[3]uint64{200, 50, 0},  // not zero -> dropped, and breaks adjacency
		[3]uint64{250, 100, 1},
	))
	want := []Extent{
		{Offset: 0, Length: 200, Zero: true},
		{Offset: 250, Length: 100, Zero: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("zeroRanges = %+v, want %+v", got, want)
	}
}

// planChunkInvariant asserts the property that must hold whatever the
// planner decides: everything is either compared or skipped, exactly once,
// in order.
func planChunkInvariant(t *testing.T, size, skipped uint64, chunks []compareChunk) {
	t.Helper()
	var covered, prevEnd uint64
	for i, c := range chunks {
		if c.length == 0 {
			t.Errorf("chunk %d is empty", i)
		}
		if c.offset < prevEnd {
			t.Fatalf("chunk %d at %d overlaps the previous one ending at %d", i, c.offset, prevEnd)
		}
		covered += c.length
		prevEnd = c.offset + c.length
	}
	if prevEnd > size {
		t.Errorf("chunks run past the end of the image: %d > %d", prevEnd, size)
	}
	if covered+skipped != size {
		t.Errorf("compared %d + skipped %d = %d, want the whole image (%d) accounted for",
			covered, skipped, covered+skipped, size)
	}
}

// TestPlanCompareChunks is the safety test for the allocation-aware compare.
//
// Getting it wrong is worse than the slowness it removes: a range skipped
// when the two sides could differ is a verify that reports a corrupt replica
// clean. Most cases below are about NOT skipping.
func TestPlanCompareChunks(t *testing.T) {
	const bs = 1024

	for _, tc := range []struct {
		name        string
		size        uint64
		minSkip     uint64
		aZero       []Extent
		bZero       []Extent
		wantSkipped uint64
		wantChunks  []compareChunk
	}{
		{
			// No allocation information from either side: a server that will
			// not negotiate base:allocation, or a query that failed. Must
			// degrade to comparing everything.
			name: "no information compares everything",
			size: 3072, minSkip: 1,
			wantChunks: []compareChunk{{0, 1024}, {1024, 1024}, {2048, 1024}},
		},
		{
			// One side alone proves nothing about the other.
			name: "source zeros alone authorise nothing",
			size: 2048, minSkip: 1, aZero: zr([3]uint64{0, 2048, 1}),
			wantChunks: []compareChunk{{0, 1024}, {1024, 1024}},
		},
		{
			name: "target zeros alone authorise nothing",
			size: 2048, minSkip: 1, bZero: zr([3]uint64{0, 2048, 1}),
			wantChunks: []compareChunk{{0, 1024}, {1024, 1024}},
		},
		{
			name: "both sides zero throughout skips everything",
			size: 2048, minSkip: 1,
			aZero: zr([3]uint64{0, 2048, 1}), bZero: zr([3]uint64{0, 2048, 1}),
			wantSkipped: 2048,
		},
		{
			// Only the intersection is provable.
			name: "partial overlap skips only what both cover",
			size: 4096, minSkip: 1,
			aZero: zr([3]uint64{0, 3072, 1}), bZero: zr([3]uint64{2048, 2048, 1}),
			wantSkipped: 1024,
			wantChunks:  []compareChunk{{0, 1024}, {1024, 1024}, {3072, 1024}},
		},
		{
			// The point of building from the data: a skip in the middle does
			// not have to line up with any grid. The chunk before it is sized
			// to the data, not rounded up to bs.
			name: "chunks follow the data, not a fixed grid",
			size: 4096, minSkip: 1,
			aZero: zr([3]uint64{100, 3000, 1}), bZero: zr([3]uint64{100, 3000, 1}),
			wantSkipped: 3000,
			wantChunks:  []compareChunk{{0, 100}, {3100, 996}},
		},
		{
			// A run too small to be worth a boundary is read straight
			// through -- more chunks would cost more than the read saves.
			name: "a skip below the threshold is not taken",
			size: 4096, minSkip: 2048,
			aZero: zr([3]uint64{1024, 1024, 1}), bZero: zr([3]uint64{1024, 1024, 1}),
			wantChunks: []compareChunk{{0, 1024}, {1024, 1024}, {2048, 1024}, {3072, 1024}},
		},
		{
			name: "a run at exactly the threshold is taken",
			size: 4096, minSkip: 1024,
			aZero: zr([3]uint64{1024, 1024, 1}), bZero: zr([3]uint64{1024, 1024, 1}),
			wantSkipped: 1024,
			wantChunks:  []compareChunk{{0, 1024}, {2048, 1024}, {3072, 1024}},
		},
		{
			// A compare region longer than the buffer is still split at it.
			name: "regions are split at the buffer size",
			size: 5000, minSkip: 1,
			aZero: zr([3]uint64{4000, 1000, 1}), bZero: zr([3]uint64{4000, 1000, 1}),
			wantSkipped: 1000,
			wantChunks:  []compareChunk{{0, 1024}, {1024, 1024}, {2048, 1024}, {3072, 928}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chunks, skipped := planCompareChunks(tc.size, bs, tc.minSkip, tc.aZero, tc.bZero)
			if skipped != tc.wantSkipped {
				t.Errorf("skipped %d bytes, want %d", skipped, tc.wantSkipped)
			}
			if !reflect.DeepEqual(chunks, tc.wantChunks) {
				t.Errorf("chunks = %+v, want %+v", chunks, tc.wantChunks)
			}
			planChunkInvariant(t, tc.size, skipped, chunks)
		})
	}
}

// A hole is NOT a zero. An unallocated cluster in a qcow2 with a backing
// file reads the backing file, so two images can both report a hole over a
// range and still differ. The planner must key on Zero alone.
func TestPlanCompareChunksIgnoresHolesThatAreNotZero(t *testing.T) {
	holes := zeroRanges(zr([3]uint64{0, 4096, 0}))
	chunks, skipped := planCompareChunks(4096, 1024, 1, holes, holes)
	if skipped != 0 || len(chunks) != 4 {
		t.Errorf("planner skipped %d bytes over ranges that are merely holes; a hole over a "+
			"backing file does not read as zeros, so those two sides can differ", skipped)
	}
}

// The shape this optimisation exists for, taken from a real replica: a 10 GiB
// disk holding about 3 MiB of data in eight small runs.
//
// A fixed grid would keep a whole 32 MiB chunk alive for each 64 KiB run and
// read ~192 MiB. Building from the data reads the data.
func TestPlanCompareChunksOnARealSparseDisk(t *testing.T) {
	const (
		size = 10737418240      // 10 GiB
		bs   = 32 * 1024 * 1024 // what qemu-nbd negotiates in practice
	)
	// The data runs from `qemu-img map` on hap01l disk1.
	data := [][2]uint64{
		{0, 65536}, {2047868928, 131072}, {4219994112, 65536},
		{6392119296, 65536}, {6392184832, 2686976}, {8564244480, 65536},
		{10736238592, 131072}, {10737352704, 65536},
	}
	// Everything else reads as zeros on both sides.
	var zeros []Extent
	pos := uint64(0)
	for _, d := range data {
		if d[0] > pos {
			zeros = append(zeros, Extent{Offset: pos, Length: d[0] - pos, Zero: true})
		}
		pos = d[0] + d[1]
	}
	if pos < size {
		zeros = append(zeros, Extent{Offset: pos, Length: size - pos, Zero: true})
	}

	chunks, skipped := planCompareChunks(size, bs, minSkipBytes, zeros, zeros)
	planChunkInvariant(t, size, skipped, chunks)

	var compared uint64
	for _, c := range chunks {
		compared += c.length
	}
	// The two 6392119296 runs are adjacent and merge; nothing else does. The
	// exact figure matters less than the order of magnitude: single-digit
	// MiB, not hundreds.
	if compared > 8<<20 {
		t.Errorf("compares %d bytes of a 10 GiB disk holding ~3 MiB of data; the whole point is "+
			"to read the data rather than the grid squares it happens to sit in", compared)
	}
	t.Logf("10 GiB disk, ~3 MiB of data: comparing %d bytes in %d chunks (skipping %d)",
		compared, len(chunks), skipped)
}
