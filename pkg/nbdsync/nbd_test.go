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
package nbdsync

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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

	if err := WaitForTCPExport("127.0.0.1", port, 5*time.Second); err != nil {
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
	written, err := CopyExtentsTCP(ctx, "127.0.0.1", srcPort, "", "127.0.0.1", dstPort, extents, 4)
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
	if err := CompareTCP(ctx, "127.0.0.1", srcPort, "", "127.0.0.1", dstPort, 4); err != nil {
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
	written, err := CopyExtentsTCP(ctx, "127.0.0.1", srcPort, "", "127.0.0.1", dstPort, extents, 4)
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
	if err := CompareTCP(ctx, "127.0.0.1", aPort, "", "127.0.0.1", bPort, 4); err != nil {
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
	err := CompareTCP(ctx, "127.0.0.1", aPort, "", "127.0.0.1", bPort, 4)
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
	mismatches, err := CompareTCPCollect(ctx, "127.0.0.1", aPort, "", "127.0.0.1", bPort, 4)
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

	if got := negotiateBufferSize(a, b, "test-a", "test-b"); got == 0 {
		t.Fatal("negotiateBufferSize returned 0 against two live NBD connections -- this would spin a chunking loop forever without transferring a single byte")
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
		name                                            string
		requestedOffset, requestedChunk, describedEnd   uint64
		wantOffset                                      uint64
		wantErr                                         bool
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

	if err := WaitForTCPExport("127.0.0.1", port, 5*time.Second); err != nil {
		t.Fatalf("WaitForTCPExport against an already-listening export: %v", err)
	}
}

func TestWaitForTCPExportTimesOutWhenNothingListening(t *testing.T) {
	port := freeTCPPort(t) // reserved and released -- guaranteed nothing is listening on it

	start := time.Now()
	err := WaitForTCPExport("127.0.0.1", port, 500*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("WaitForTCPExport succeeded against a port nothing is listening on")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("WaitForTCPExport took %s to give up on a 500ms timeout -- looks hung rather than just imprecise", elapsed)
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
	_, err := CopyExtentsTCP(ctx, "127.0.0.1", 1, "", "127.0.0.1", 2, nil, 1)
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
	err := CompareTCP(ctx, "127.0.0.1", 1, "", "127.0.0.1", 2, 1)
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
