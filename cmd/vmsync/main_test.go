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

package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"vmsync/pkg/disk"
	"vmsync/pkg/libvirtsync"
	"vmsync/pkg/metrics"
	"vmsync/pkg/nbdsync"
)

// TestOptionalValueFlag verifies the bare/"=value"/"=false" tri-state
// behavior optionalValueFlag hijacks from flag.Value's IsBoolFlag mechanism
// (see the type's doc comment in main.go).
func TestOptionalValueFlag(t *testing.T) {
	t.Run("bare -name resolves to bareDefault", func(t *testing.T) {
		f := optionalValueFlag{bareDefault: "s2"}
		if err := f.Set("true"); err != nil {
			t.Fatalf("Set(true) returned unexpected error: %v", err)
		}
		if f.value != "s2" {
			t.Fatalf("value = %q, want %q", f.value, "s2")
		}
	})

	t.Run("-name=false disables it", func(t *testing.T) {
		f := optionalValueFlag{bareDefault: "s2"}
		if err := f.Set("false"); err != nil {
			t.Fatalf("Set(false) returned unexpected error: %v", err)
		}
		if f.value != "" {
			t.Fatalf("value = %q, want empty string", f.value)
		}
	})

	t.Run("-name=x is a literal passthrough", func(t *testing.T) {
		f := optionalValueFlag{bareDefault: "s2"}
		if err := f.Set("zstd"); err != nil {
			t.Fatalf("Set(zstd) returned unexpected error: %v", err)
		}
		if f.value != "zstd" {
			t.Fatalf("value = %q, want %q", f.value, "zstd")
		}
	})

	t.Run("IsBoolFlag is always true", func(t *testing.T) {
		f := optionalValueFlag{bareDefault: "s2"}
		if !f.IsBoolFlag() {
			t.Fatal("IsBoolFlag() = false, want true")
		}
		_ = f.Set("false")
		if !f.IsBoolFlag() {
			t.Fatal("IsBoolFlag() = false after Set(false), want true")
		}
	})

	t.Run("String reflects the current value", func(t *testing.T) {
		f := optionalValueFlag{bareDefault: "s2"}
		if err := f.Set("zstd"); err != nil {
			t.Fatalf("Set(zstd) returned unexpected error: %v", err)
		}
		if f.String() != "zstd" {
			t.Fatalf("String() = %q, want %q", f.String(), "zstd")
		}
	})
}

// TestCallWithTimeout checks the three behaviors callWithTimeout's doc
// comment promises: a fast success passes through untouched, a fast failure
// passes through untouched, and a call that outlives its timeout is given up
// on -- without callWithTimeout itself blocking on the still-running fn.
func TestCallWithTimeout(t *testing.T) {
	t.Run("fast success returns nil well before the timeout", func(t *testing.T) {
		timeout := 200 * time.Millisecond
		start := time.Now()
		err := callWithTimeout("op", timeout, func() error {
			return nil
		})
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("callWithTimeout returned %v, want nil", err)
		}
		if elapsed >= timeout {
			t.Fatalf("callWithTimeout took %s, want well under the %s timeout", elapsed, timeout)
		}
	})

	t.Run("fast failure propagates the exact error", func(t *testing.T) {
		timeout := 200 * time.Millisecond
		wantErr := errors.New("boom")
		err := callWithTimeout("op", timeout, func() error {
			return wantErr
		})
		if err != wantErr {
			t.Fatalf("callWithTimeout returned %v, want the exact sentinel error %v", err, wantErr)
		}
		if errors.Is(err, ErrCallTimedOut) {
			t.Fatal("errors.Is(err, ErrCallTimedOut) = true for a fast, real failure -- fn already returned, its goroutine isn't abandoned, callers must not treat this the same as a timeout")
		}
	})

	t.Run("a call that outlives the timeout is given up on promptly", func(t *testing.T) {
		timeout := 50 * time.Millisecond
		sleep := 300 * time.Millisecond
		start := time.Now()
		err := callWithTimeout("stuck-op", timeout, func() error {
			time.Sleep(sleep)
			return nil
		})
		elapsed := time.Since(start)

		if err == nil {
			t.Fatal("callWithTimeout returned nil, want a timeout error")
		}
		if !strings.Contains(err.Error(), "timed out after") {
			t.Fatalf("error = %q, want it to contain %q", err.Error(), "timed out after")
		}
		if !errors.Is(err, ErrCallTimedOut) {
			t.Fatalf("errors.Is(err, ErrCallTimedOut) = false for %q, want true -- callers rely on this to know fn's goroutine may still be running", err)
		}
		// callWithTimeout must not wait for the orphaned goroutine to finish
		// its full sleep -- it should give up right around timeout, well
		// short of the fn's full sleep duration.
		if elapsed >= sleep {
			t.Fatalf("callWithTimeout took %s, want it to return well before the fn's %s sleep completes", elapsed, sleep)
		}
	})
}

// TestVerificationRan pins down the exact gating rule that keeps
// vmsync_verification_state/vmsync_verification_timestamp_seconds out of a
// plain sync or -reinit run's metrics: verification must have both been
// requested (verify != "") AND actually reached the compare block
// (attempted), not merely requested.
func TestVerificationRan(t *testing.T) {
	cases := []struct {
		verify    string
		attempted bool
		want      bool
	}{
		{verify: "", attempted: false, want: false}, // plain sync / -reinit -- the critical case
		{verify: "", attempted: true, want: false},  // defensive: shouldn't happen in practice
		{verify: "qemu-img", attempted: false, want: false},
		{verify: "fast", attempted: false, want: false},
		{verify: "full", attempted: false, want: false},
		{verify: "qemu-img", attempted: true, want: true},
		{verify: "fast", attempted: true, want: true},
		{verify: "full", attempted: true, want: true},
	}

	for _, tc := range cases {
		got := verificationRan(tc.verify, tc.attempted)
		if got != tc.want {
			t.Errorf("verificationRan(%q, %v) = %v, want %v", tc.verify, tc.attempted, got, tc.want)
		}
	}
}

// TestFinalRunState covers the exact guarantee this was extracted to make
// testable and enforceable: an interrupted run must report failure no
// matter what runErr/fsFreezeFailed say, because run()'s own deferred
// metrics write and the signal handler's own direct one (always
// metrics.StateFailure) can genuinely race on writing the same textfile --
// making both computations agree, via wasInterrupted, removes the race's
// only consequence instead of trying to prevent the race itself.
func TestFinalRunState(t *testing.T) {
	boom := errors.New("boom")
	cases := []struct {
		name           string
		runErr         error
		wasInterrupted bool
		fsFreezeFailed bool
		want           int
	}{
		{name: "clean success", runErr: nil, wasInterrupted: false, fsFreezeFailed: false, want: metrics.StateSuccess},
		{name: "runErr alone -> failure", runErr: boom, wasInterrupted: false, fsFreezeFailed: false, want: metrics.StateFailure},
		{name: "interrupted alone, runErr nil -> still failure", runErr: nil, wasInterrupted: true, fsFreezeFailed: false, want: metrics.StateFailure},
		{name: "interrupted takes priority over a completed freeze", runErr: nil, wasInterrupted: true, fsFreezeFailed: true, want: metrics.StateFailure},
		{name: "runErr takes priority over a completed freeze", runErr: boom, wasInterrupted: false, fsFreezeFailed: true, want: metrics.StateFailure},
		{name: "fsFreezeFailed alone -> degraded, not full failure", runErr: nil, wasInterrupted: false, fsFreezeFailed: true, want: metrics.StateFSFreezeFailed},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := finalRunState(tc.runErr, tc.wasInterrupted, tc.fsFreezeFailed); got != tc.want {
				t.Errorf("finalRunState(runErr=%v, wasInterrupted=%v, fsFreezeFailed=%v) = %v, want %v", tc.runErr, tc.wasInterrupted, tc.fsFreezeFailed, got, tc.want)
			}
		})
	}
}

// TestUnverifiableCheckpointMetadataError covers the exact guarantee this
// was extracted to make testable: an incremental sync (parent != "") must
// abort when the target's own last_checkpoint metadata can't be trusted,
// while a full sync (parent == "") -- which has no earlier checkpoint to
// verify against in the first place -- must never abort on this, no matter
// how broken that metadata is.
func TestUnverifiableCheckpointMetadataError(t *testing.T) {
	parseErr := errors.New("xml: malformed metadata element")

	cases := []struct {
		name               string
		parent             string
		checkpointErr      error
		metadataCheckpoint string
		wantErr            bool
	}{
		{name: "incremental, metadata read fine -> no error", parent: "vmsync-cpt-000042", checkpointErr: nil, metadataCheckpoint: "vmsync-cpt-000042", wantErr: false},
		{name: "incremental, empty metadata -> must abort", parent: "vmsync-cpt-000042", checkpointErr: nil, metadataCheckpoint: "", wantErr: true},
		{name: "incremental, unparsable metadata -> must abort", parent: "vmsync-cpt-000042", checkpointErr: parseErr, metadataCheckpoint: "", wantErr: true},
		{name: "full sync, empty metadata -> advisory only", parent: "", checkpointErr: nil, metadataCheckpoint: "", wantErr: false},
		{name: "full sync, unparsable metadata -> advisory only", parent: "", checkpointErr: parseErr, metadataCheckpoint: "", wantErr: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := unverifiableCheckpointMetadataError("test-domain", tc.parent, tc.checkpointErr, tc.metadataCheckpoint)
			if tc.wantErr && err == nil {
				t.Fatalf("unverifiableCheckpointMetadataError(parent=%q) = nil, want a non-nil error", tc.parent)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unverifiableCheckpointMetadataError(parent=%q) = %v, want nil", tc.parent, err)
			}
		})
	}
}

// TestCheckpointChainConsistent covers unverifiableCheckpointMetadataError's
// companion guard: once the target's last_checkpoint metadata has actually
// been read and parsed fine, it must still name the SAME checkpoint this
// run computed as its expected parent from the source's own chain, or an
// incremental sync would apply this run's delta on top of the wrong base
// -- producing a target that looks fine (the run reports success) but
// silently reflects a mixed, incorrect history. Before this function was
// extracted, this exact comparison lived inline in run() with nothing to
// call directly, making it the one checkpoint-chain guard in this file
// with no test coverage at all.
func TestCheckpointChainConsistent(t *testing.T) {
	cases := []struct {
		name                    string
		metadataEntryCheckpoint string
		parent                  string
		want                    bool
	}{
		{name: "matches -- the common case on every normal incremental sync", metadataEntryCheckpoint: "vmsync-cpt-000042", parent: "vmsync-cpt-000042", want: true},
		{name: "mismatch -- target's own metadata disagrees with the expected parent", metadataEntryCheckpoint: "vmsync-cpt-000041", parent: "vmsync-cpt-000042", want: false},
		{name: "empty metadata -- unverifiableCheckpointMetadataError's own responsibility, not a mismatch here", metadataEntryCheckpoint: "", parent: "vmsync-cpt-000042", want: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := checkpointChainConsistent(tc.metadataEntryCheckpoint, tc.parent); got != tc.want {
				t.Errorf("checkpointChainConsistent(%q, %q) = %v, want %v", tc.metadataEntryCheckpoint, tc.parent, got, tc.want)
			}
		})
	}
}

// TestRefuseReinitIfTargetRunning covers the one guard standing between a
// normal -reinit and an unconditional `rm -f` running against a target
// disk qemu still has open: this is the exact regression class flagged as
// untested -- inverting or short-circuiting this check would let -reinit
// delete a running target's disk file out from under it, silently
// reverting the replica to nothing the next time that domain shuts down.
func TestRefuseReinitIfTargetRunning(t *testing.T) {
	cases := []struct {
		name    string
		exists  bool
		running bool
		wantErr bool
	}{
		{name: "target doesn't exist at all -- nothing to protect", exists: false, running: false, wantErr: false},
		{name: "target doesn't exist, running is meaningless/stale -- still fine", exists: false, running: true, wantErr: false},
		{name: "target exists, shut off -- exactly what -reinit expects", exists: true, running: false, wantErr: false},
		{name: "target exists and is running -- must refuse", exists: true, running: true, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := refuseReinitIfTargetRunning("test-domain", tc.exists, tc.running)
			if tc.wantErr && err == nil {
				t.Fatalf("refuseReinitIfTargetRunning(exists=%v, running=%v) = nil, want a non-nil error", tc.exists, tc.running)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("refuseReinitIfTargetRunning(exists=%v, running=%v) = %v, want nil", tc.exists, tc.running, err)
			}
		})
	}
}

// TestTargetPortsNeeded pins the count against what a run actually BINDS.
//
// The expectations changed when the fixed-offset layout went away, and the
// verify-without-bridging case is why. It used to assert 3N, mirroring a
// layout that put the verify block at +2N whether or not bridging was on --
// so a run reserved through 3N and bound 2N, holding the middle block idle.
// The test was correct about the code and both were wrong about the need: a
// four-disk -verify=qemu-img run with no compression demanded twelve
// consecutive ports to use eight, and was refused on a fragmented range that
// could have served it. Nothing reserves a block now (see
// portalloc.Allocator: each export binds its own port), so the padding has no
// layout left to mirror and the answer is the count.
//
// One port per disk for the write export, plus one per disk for its bridge
// when bridging, plus one per disk for the verify export when verifying, plus
// one per disk for the verify export's bridge when both.
//
// The integrity check, which is ON by default, is deliberately absent from
// this function and so from this test: the pre-commit check exports over a
// Unix socket and the digest-based -verify reuses the verify export already
// counted. Neither costs a port. TestChecksumCheckCostsNoPorts below states
// that as its own assertion, so that adding a port to either path fails a
// test rather than quietly costing every run more ports.
func TestTargetPortsNeeded(t *testing.T) {
	cases := []struct {
		name     string
		disks    int
		bridging bool
		verify   bool
		want     int
	}{
		{name: "plain sync, one disk", disks: 1, want: 1},
		{name: "plain sync, three disks", disks: 3, want: 3},
		{name: "bridging adds one port per disk", disks: 3, bridging: true, want: 6},
		// The case the padding was in: two exports per disk, not three.
		{name: "verify without bridging binds two per disk", disks: 3, verify: true, want: 6},
		{name: "verify with bridging binds four per disk", disks: 3, bridging: true, verify: true, want: 12},
		{name: "single disk, everything on", disks: 1, bridging: true, verify: true, want: 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := targetPortsNeeded(tc.disks, tc.bridging, tc.verify); got != tc.want {
				t.Errorf("targetPortsNeeded(%d, bridging=%v, verifying=%v) = %d, want %d",
					tc.disks, tc.bridging, tc.verify, got, tc.want)
			}
		})
	}

	// The same invariant stated independently of the table, and restated for
	// the new model: the total must be the sum of the per-stage counts, each
	// of which is one port per disk. It used to check the total against the
	// highest offset a code path bound at (base+4N-1), which was the right
	// check for a contiguous layout and is meaningless without one -- and it
	// passed while the 3N padding was in place, because that case is not
	// fully-enabled.
	const disks = 4
	if got, want := targetPortsNeeded(disks, false, false), disks; got != want {
		t.Errorf("plain: got %d, want %d (write export only)", got, want)
	}
	if got, want := targetPortsNeeded(disks, true, false), 2*disks; got != want {
		t.Errorf("bridging: got %d, want %d (write export + bridge)", got, want)
	}
	if got, want := targetPortsNeeded(disks, false, true), 2*disks; got != want {
		t.Errorf("verifying: got %d, want %d (write export + verify export, with NO bridge block in between)", got, want)
	}
	if got, want := targetPortsNeeded(disks, true, true), 4*disks; got != want {
		t.Errorf("both: got %d, want %d (write export + bridge + verify export + verify bridge)", got, want)
	}
	// Each stage costs exactly one port per disk, so the increments must be
	// equal. A stage that ever costs two would break this without any single
	// case above having to be updated.
	plain := targetPortsNeeded(disks, false, false)
	if bridged, verified := targetPortsNeeded(disks, true, false), targetPortsNeeded(disks, false, true); bridged-plain != verified-plain {
		t.Errorf("bridging costs %d extra ports and verifying costs %d; each stage should cost one per disk",
			bridged-plain, verified-plain)
	}
}

// The integrity check must not cost a single port, in either of its forms,
// and this states it as an assertion rather than leaving it to a comment.
//
// It is worth pinning because the temptation to give the pre-commit export a
// TCP port of its own is real -- it is what every other export here does --
// and doing so would widen EVERY run's reservation by N whether or not the
// check ran, shrinking how many concurrent runs an auto-allocated range can
// hold. The Unix socket is what avoids that, and the digest-based -verify
// reuses the export -verify already pays for.
func TestChecksumCheckCostsNoPorts(t *testing.T) {
	const disks = 3
	for _, tc := range []struct {
		name             string
		bridging, verify bool
	}{
		{"plain", false, false},
		{"bridged", true, false},
		{"verifying", false, true},
		{"bridged and verifying", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// targetPortsNeeded takes no checksum parameter at all, which is
			// the point: there is no combination of flags under which the
			// check changes the answer. If a port is ever added for it, this
			// function's signature has to change and this test stops
			// compiling -- which is the alarm.
			got := targetPortsNeeded(disks, tc.bridging, tc.verify)
			want := disks
			switch {
			case tc.verify && tc.bridging:
				want = 4 * disks
			case tc.verify:
				// Two per disk, not three: the write export and the verify
				// export, with nothing held between them. See
				// TestTargetPortsNeeded for why this used to be 3N.
				want = 2 * disks
			case tc.bridging:
				want = 2 * disks
			}
			if got != want {
				t.Errorf("targetPortsNeeded(%d, %v, %v) = %d, want %d -- the integrity check must not add ports",
					disks, tc.bridging, tc.verify, got, want)
			}
		})
	}
}

func TestSourcePortsNeeded(t *testing.T) {
	// The source side is the libvirt backup export, plus its bridge helper
	// at +1 only when compression or buffering is on. The verify phase
	// reuses the same export rather than opening a second one.
	if got := sourcePortsNeeded(false); got != 1 {
		t.Errorf("sourcePortsNeeded(false) = %d, want 1", got)
	}
	if got := sourcePortsNeeded(true); got != 2 {
		t.Errorf("sourcePortsNeeded(true) = %d, want 2 (export plus its bridge at +1)", got)
	}
}

// targetFileNewerThanSync is the guard that catches somebody writing to the
// replica between syncs -- and, before it took a tolerance, the guard that
// turned a one-second NTP disagreement into a permanent replication outage.
// The two timestamps it compares come from different hosts' clocks.
func TestTargetFileNewerThanSync(t *testing.T) {
	const sync = "1756041600"

	for name, tc := range map[string]struct {
		mtime     string
		tolerance time.Duration
		wantNewer bool
	}{
		// The ordinary case: the disk was written before the metadata, so
		// its mtime is at or behind the sync timestamp.
		"older than the sync": {"1756041500", 0, false},
		"exactly the sync":    {sync, 0, false},
		// Zero tolerance is what the flag exists to escape: one second of
		// forward drift on the target fails every incremental sync, forever,
		// with an error blaming out-of-band modification.
		"one second ahead, no tolerance": {"1756041601", 0, true},
		"one second ahead, 30s allowed":  {"1756041601", 30 * time.Second, false},
		"30s ahead, 30s allowed":         {"1756041630", 30 * time.Second, false},
		// The boundary is exclusive: 31s ahead of a 30s tolerance is over.
		"31s ahead, 30s allowed": {"1756041631", 30 * time.Second, true},
		// A tolerance does not blind the check. An out-of-band write is
		// normally minutes or hours after a sync, not inside NTP jitter.
		"an hour ahead, 30s allowed": {"1756045200", 30 * time.Second, true},
		// Behind by a lot is not "newer" at any tolerance -- a target clock
		// running slow is a different problem and not this one's to report.
		"an hour behind": {"1756038000", 0, false},
	} {
		t.Run(name, func(t *testing.T) {
			newer, _, err := targetFileNewerThanSync(tc.mtime, sync, tc.tolerance)
			if err != nil {
				t.Fatalf("targetFileNewerThanSync: %v", err)
			}
			if newer != tc.wantNewer {
				t.Errorf("newer = %v, want %v", newer, tc.wantNewer)
			}
		})
	}
}

func TestTargetFileNewerThanSyncReportsTheSkew(t *testing.T) {
	// The number is what tells the two causes apart: seconds means two
	// clocks disagree, hours means somebody wrote to the replica. The error
	// message quotes it so an operator can size the flag from one failure.
	_, ahead, err := targetFileNewerThanSync("1756041690", "1756041600", 0)
	if err != nil {
		t.Fatalf("targetFileNewerThanSync: %v", err)
	}
	if ahead != 90*time.Second {
		t.Errorf("ahead = %v, want 90s", ahead)
	}
}

func TestTargetFileNewerThanSyncRefusesNonNumericInput(t *testing.T) {
	// The old string comparison reported "newer" for anything that was not a
	// number -- a stat that printed an error, say -- which is the wrong
	// answer in the dangerous direction: it fails the sync while describing
	// a modification that never happened.
	if _, _, err := targetFileNewerThanSync("stat: No such file or directory", "1756041600", 0); err == nil {
		t.Error("a stat error was accepted as an mtime")
	}
	if _, _, err := targetFileNewerThanSync("1756041600", "not-a-timestamp", 0); err == nil {
		t.Error("an unparsable last_sync_timestamp was accepted")
	}
}

func TestTargetFileNewerThanSyncTreatsANegativeToleranceAsZero(t *testing.T) {
	// A negative value can only come from a typo, and reading it as "even
	// stricter than exact" would refuse syncs whose mtime is correctly equal
	// to the timestamp.
	newer, _, err := targetFileNewerThanSync("1756041600", "1756041600", -5*time.Second)
	if err != nil {
		t.Fatalf("targetFileNewerThanSync: %v", err)
	}
	if newer {
		t.Error("a negative tolerance made an equal timestamp count as newer")
	}
}

// TestSyncFloor pins the property that makes replica_written_at safe to
// introduce at all: max() can only ever RELAX the out-of-band check.
//
// A change whose entire purpose is removing a spurious refusal must not be
// able to create one. Every case below is checked against that, not just
// against the expected value.
func TestSyncFloor(t *testing.T) {
	for _, tc := range []struct {
		name          string
		lastSync      string
		writtenAt     int64
		haveWrittenAt bool
		wantFloor     string
		wantFromWA    bool
	}{
		{
			// A replica written by a vmsync that predates the field, or a
			// disk this build never stamped. Must behave exactly as before.
			name:     "no stamp falls back to last_sync",
			lastSync: "1700000000", haveWrittenAt: false,
			wantFloor: "1700000000", wantFromWA: false,
		},
		{
			// The direction that caused the bug: the target's clock runs
			// ahead, so a healthy replica's mtime exceeds last_sync. The
			// stamp is larger, wins, and makes the comparison same-clock.
			name:     "target clock ahead: the stamp wins and the check becomes exact",
			lastSync: "1700000000", writtenAt: 1700000030, haveWrittenAt: true,
			wantFloor: "1700000030", wantFromWA: true,
		},
		{
			// The permissive direction. last_sync wins; the check is exactly
			// as strict as it has always been, never stricter.
			name:     "target clock behind: last_sync wins, no new refusal",
			lastSync: "1700000030", writtenAt: 1700000000, haveWrittenAt: true,
			wantFloor: "1700000030", wantFromWA: false,
		},
		{
			name:     "equal values keep the compatibility floor",
			lastSync: "1700000000", writtenAt: 1700000000, haveWrittenAt: true,
			wantFloor: "1700000000", wantFromWA: false,
		},
		{
			// A usable stamp beats a value nothing can compare against.
			name:     "unparsable last_sync yields to a real stamp",
			lastSync: "not-a-timestamp", writtenAt: 1700000000, haveWrittenAt: true,
			wantFloor: "1700000000", wantFromWA: true,
		},
		{
			// ...but with no stamp it is passed through unchanged, so
			// targetFileNewerThanSync reports the parse error itself rather
			// than this function hiding it.
			name:     "unparsable last_sync with no stamp is passed through",
			lastSync: "not-a-timestamp", haveWrittenAt: false,
			wantFloor: "not-a-timestamp", wantFromWA: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			floor, fromWA := syncFloor(tc.lastSync, tc.writtenAt, tc.haveWrittenAt)
			if floor != tc.wantFloor || fromWA != tc.wantFromWA {
				t.Errorf("syncFloor(%q, %d, %v) = (%q, %v), want (%q, %v)",
					tc.lastSync, tc.writtenAt, tc.haveWrittenAt, floor, fromWA, tc.wantFloor, tc.wantFromWA)
			}
		})
	}
}

// The invariant stated as a property rather than a table: for any parsable
// last_sync, introducing a stamp must never move the floor DOWN, because a
// lower floor is a stricter check and could refuse a replica that today
// passes.
func TestSyncFloorNeverTightensTheCheck(t *testing.T) {
	const lastSync = "1700000000"
	for _, stamp := range []int64{0, 1, 1699999999, 1700000000, 1700000001, 1800000000} {
		floor, _ := syncFloor(lastSync, stamp, true)
		got, err := strconv.ParseInt(floor, 10, 64)
		if err != nil {
			t.Fatalf("syncFloor produced an unparsable floor %q", floor)
		}
		if got < 1700000000 {
			t.Errorf("stamp %d moved the floor down to %d; a lower floor is a STRICTER check and could refuse a replica that passes today", stamp, got)
		}
	}
}

// TestIsVerifyMismatch pins the one distinction -reinit-after-failures now
// turns on: a verification that RAN and found a difference, versus one that
// could not be performed.
//
// Getting this backwards is destructive in both directions. Classify an
// infrastructure error as a mismatch and auto-reinit stops noticing a broken
// sync mechanism -- which is the bug this replaced, where `cfg.Verify != ""`
// exempted every failure on a -verify run. Classify a real mismatch as
// infrastructure and auto-reinit answers a corruption finding by discarding
// the checkpoint chain and recopying, destroying the evidence.
func TestIsVerifyMismatch(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil is not a mismatch", nil, false},

		// The three comparators, each raising its own sentinel.
		{"fast: nbdsync found a difference", fmt.Errorf("%w: mismatch at offset=4096 length=4096", nbdsync.ErrImagesDiffer), true},
		{"qemu-img found a difference", fmt.Errorf("%w: Content mismatch at offset 0", disk.ErrImagesDiffer), true},
		{"full: collected ranges", fmt.Errorf("%w: 3 range(s) totalling 12288 bytes differ", nbdsync.ErrImagesDiffer), true},

		// It has to survive the wrapping runVerify and the error channel do
		// to it, or the gate sees a bare error and counts a corruption
		// finding as a broken sync.
		{
			"survives runVerify's own wrapping",
			fmt.Errorf("verify: disk %s does not match: %w", "vda",
				fmt.Errorf("%w: mismatch at offset=0 length=4096", nbdsync.ErrImagesDiffer)),
			true,
		},

		// Everything below is a compare that could not be performed. All of
		// these MUST count toward the failure threshold.
		{"the compare could not connect", errors.New("compare failed: connect target nbd: connection refused"), false},
		{"the export never came up", errors.New("verify: wait for read-only export 10.0.0.1:20918: timeout"), false},
		{"the run was cancelled", context.Canceled, false},
		{"an ssh session died mid-compare", errors.New("open ssh session: context canceled"), false},
		{"a plain sync failure with no verify involved", errors.New("nbd copy stalled"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isVerifyMismatch(tc.err); got != tc.want {
				t.Errorf("isVerifyMismatch(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// The regression this change exists to prevent, stated directly: a failure
// on a -verify run that is NOT a mismatch must be counted.
//
// Before this, the gate was `cfg.Verify != ""`, so an operator who ran
// -verify on their scheduled syncs got no failure counting and no
// auto-reinit at all -- silently, with the flag still on the command line.
func TestInfrastructureFailuresOnAVerifyRunAreStillCounted(t *testing.T) {
	// Representative of what actually goes wrong during a compare, none of
	// which says anything about whether the images differ.
	for _, err := range []error{
		errors.New("compare failed: read source nbd: connection reset by peer"),
		errors.New("verify: start read-only export for /data/x.qcow2: exit status 1"),
		errors.New("start verify nbd bridge for vda: dial tcp: i/o timeout"),
	} {
		if isVerifyMismatch(err) {
			t.Errorf("%v was classified as a data finding; it is a broken sync and must count toward -reinit-after-failures", err)
		}
	}
}

// TestPlanCheckpointRecovery covers the decision that removes a checkpoint
// from a live chain, which is the most consequential thing in this file.
//
// The situation it recovers from: the source's chain and the target's record
// of it advance by two different libvirt calls, and only the second can fail
// on its own. A run that copied successfully and then failed its
// DomainDefineXML left the source at cpt-000002 with the target still saying
// cpt-000001, and every later incremental refused with "checkpoint
// inconsistency detected" until somebody ran -reinit. pending_checkpoint is
// the write-ahead record that makes the state recognisable rather than
// something to infer.
func TestPlanCheckpointRecovery(t *testing.T) {
	chain := func(names ...string) []libvirtsync.Checkpoint {
		out := make([]libvirtsync.Checkpoint, 0, len(names))
		parent := ""
		for _, n := range names {
			out = append(out, libvirtsync.Checkpoint{Name: n, Parent: parent})
			parent = n
		}
		return out
	}

	t.Run("no pending record leaves the chain alone", func(t *testing.T) {
		got, err := planCheckpointRecovery("", "vmsync-cpt-000001", chain("vmsync-cpt-000001"))
		if err != nil || got != "" {
			t.Errorf("got %q, %v; want no deletion and no error", got, err)
		}
	})

	// The wedge itself: the source advanced, the target never accepted.
	t.Run("pending is the tip and was never accepted -> delete it", func(t *testing.T) {
		got, err := planCheckpointRecovery(
			"vmsync-cpt-000002", "vmsync-cpt-000001",
			chain("vmsync-cpt-000001", "vmsync-cpt-000002"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "vmsync-cpt-000002" {
			t.Errorf("got %q, want vmsync-cpt-000002 deleted", got)
		}
	})

	// Died between recording the intent and creating the checkpoint. The
	// source never advanced, so there is nothing to undo -- and nothing to
	// refuse over either.
	t.Run("pending names a checkpoint the source does not have -> nothing", func(t *testing.T) {
		got, err := planCheckpointRecovery(
			"vmsync-cpt-000002", "vmsync-cpt-000001",
			chain("vmsync-cpt-000001"))
		if err != nil || got != "" {
			t.Errorf("got %q, %v; want no deletion and no error", got, err)
		}
	})

	// The define landed and the clear did not -- impossible while both are
	// one atomic define, but it must not delete a checkpoint the target has
	// accepted.
	t.Run("pending equals the accepted checkpoint -> never delete it", func(t *testing.T) {
		got, err := planCheckpointRecovery(
			"vmsync-cpt-000002", "vmsync-cpt-000002",
			chain("vmsync-cpt-000001", "vmsync-cpt-000002"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "" {
			t.Errorf("got %q, want nothing deleted -- that checkpoint IS the accepted one", got)
		}
	})

	// Only ever the tip. Deleting the newest merges its bitmap into the
	// active tracking, which is exactly the state before it existed;
	// deleting a mid-chain one is a different operation that later
	// checkpoints depend on.
	t.Run("pending is mid-chain -> refuse rather than improvise", func(t *testing.T) {
		got, err := planCheckpointRecovery(
			"vmsync-cpt-000002", "vmsync-cpt-000001",
			chain("vmsync-cpt-000001", "vmsync-cpt-000002", "vmsync-cpt-000003"))
		if err == nil {
			t.Fatalf("got %q with no error; want a refusal", got)
		}
		if got != "" {
			t.Errorf("got %q alongside an error; want no deletion named", got)
		}
		if !strings.Contains(err.Error(), "vmsync-cpt-000003") {
			t.Errorf("err = %v, want it to name the newer checkpoint that makes this unsafe", err)
		}
	})

	t.Run("first-ever sync: pending set against an empty chain -> nothing", func(t *testing.T) {
		got, err := planCheckpointRecovery("vmsync-cpt-000001", "", nil)
		if err != nil || got != "" {
			t.Errorf("got %q, %v; want no deletion and no error", got, err)
		}
	})
}

// TestMergeCheckState pins the precedence a multi-disk run folds its
// per-disk conclusions with.
//
// The interesting case is the third: one disk's contents differ and
// another's could not be checked. The run must report the DIFFERENCE. That
// is a definite, actionable fact about the replica, while "could not check"
// only says the rest is unknown -- so reporting the unknown would bury the
// finding, which is the same mistake as reporting a failed comparison as a
// detection.
func TestMergeCheckState(t *testing.T) {
	for _, tc := range []struct {
		name          string
		current, next int
		want          int
	}{
		{"both passed", metrics.CheckStatePassed, metrics.CheckStatePassed, metrics.CheckStatePassed},
		{"a mismatch wins over a pass", metrics.CheckStatePassed, metrics.CheckStateMismatch, metrics.CheckStateMismatch},
		{"a mismatch wins over not-performed", metrics.CheckStateNotPerformed, metrics.CheckStateMismatch, metrics.CheckStateMismatch},
		{"and in the other order", metrics.CheckStateMismatch, metrics.CheckStateNotPerformed, metrics.CheckStateMismatch},
		{"not-performed wins over a pass", metrics.CheckStatePassed, metrics.CheckStateNotPerformed, metrics.CheckStateNotPerformed},
		{"not-performed with itself", metrics.CheckStateNotPerformed, metrics.CheckStateNotPerformed, metrics.CheckStateNotPerformed},
		{"a mismatch stays a mismatch", metrics.CheckStateMismatch, metrics.CheckStateMismatch, metrics.CheckStateMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mergeCheckState(tc.current, tc.next); got != tc.want {
				t.Errorf("mergeCheckState(%d, %d) = %d, want %d", tc.current, tc.next, got, tc.want)
			}
		})
	}

	// Folding must be order-independent: disks complete concurrently, so
	// the run's conclusion cannot depend on which goroutine finishes first.
	all := []int{metrics.CheckStatePassed, metrics.CheckStateMismatch, metrics.CheckStateNotPerformed}
	for _, a := range all {
		for _, b := range all {
			if mergeCheckState(a, b) != mergeCheckState(b, a) {
				t.Errorf("mergeCheckState is not commutative for (%d, %d)", a, b)
			}
		}
	}
}

// A checksum mismatch must be recognisable as one through the wrapping the
// error picks up on its way out, so the metric reports 1 (the data differs)
// rather than 2 (could not check).
func TestChecksumMismatchIsIdentifiableThroughWrapping(t *testing.T) {
	wrapped := fmt.Errorf("checksum: %s: %w -- detail here", "vda", errChecksumMismatch)
	if !errors.Is(wrapped, errChecksumMismatch) {
		t.Error("a wrapped checksum mismatch is not identifiable with errors.Is")
	}
	// And an unrelated failure must NOT look like one.
	if errors.Is(fmt.Errorf("checksum: run helper: %w", errors.New("ssh died")), errChecksumMismatch) {
		t.Error("an unrelated checksum failure matched errChecksumMismatch")
	}
}
