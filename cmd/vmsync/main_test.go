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
	"errors"
	"strings"
	"testing"
	"time"

	"vmsync/pkg/metrics"
	"vmsync/pkg/nbdsync"
)

// TestOverlapsAnyExtent exercises the half-open-interval overlap check used
// by -verify=online to distinguish a genuine mismatch from one that merely
// falls inside a region the guest wrote during the compare window.
func TestOverlapsAnyExtent(t *testing.T) {
	cases := []struct {
		name    string
		m       nbdsync.MismatchRange
		touched []nbdsync.Extent
		want    bool
	}{
		{
			name: "fully inside a dirty extent",
			m:    nbdsync.MismatchRange{Offset: 120, Length: 20}, // [120,140)
			touched: []nbdsync.Extent{
				{Offset: 100, Length: 100, Dirty: true}, // [100,200)
			},
			want: true,
		},
		{
			name: "fully outside all extents",
			m:    nbdsync.MismatchRange{Offset: 300, Length: 10}, // [300,310)
			touched: []nbdsync.Extent{
				{Offset: 100, Length: 100, Dirty: true}, // [100,200)
			},
			want: false,
		},
		{
			name: "partially overlapping at an edge",
			m:    nbdsync.MismatchRange{Offset: 190, Length: 20}, // [190,210)
			touched: []nbdsync.Extent{
				{Offset: 100, Length: 100, Dirty: true}, // [100,200), overlap [190,200)
			},
			want: true,
		},
		{
			name: "touching but not overlapping -- mismatch ends exactly where the extent begins",
			m:    nbdsync.MismatchRange{Offset: 150, Length: 50}, // [150,200)
			touched: []nbdsync.Extent{
				{Offset: 200, Length: 50, Dirty: true}, // [200,250)
			},
			want: false,
		},
		{
			name: "non-dirty extent covering the same range is skipped",
			m:    nbdsync.MismatchRange{Offset: 120, Length: 20}, // [120,140)
			touched: []nbdsync.Extent{
				{Offset: 100, Length: 100, Dirty: false}, // [100,200), but not dirty
			},
			want: false,
		},
		{
			name:    "empty touched slice",
			m:       nbdsync.MismatchRange{Offset: 0, Length: 10},
			touched: []nbdsync.Extent{},
			want:    false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := overlapsAnyExtent(tc.m, tc.touched)
			if got != tc.want {
				t.Fatalf("overlapsAnyExtent(%+v, %+v) = %v, want %v", tc.m, tc.touched, got, tc.want)
			}
		})
	}
}

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
		{verify: "", attempted: false, want: false},  // plain sync / -reinit -- the critical case
		{verify: "", attempted: true, want: false},   // defensive: shouldn't happen in practice
		{verify: "compare", attempted: false, want: false},
		{verify: "fast", attempted: false, want: false},
		{verify: "online", attempted: false, want: false},
		{verify: "compare", attempted: true, want: true},
		{verify: "fast", attempted: true, want: true},
		{verify: "online", attempted: true, want: true},
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
		name              string
		parent            string
		checkpointErr     error
		metadataCheckpoint string
		wantErr           bool
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
		name                   string
		metadataEntryCheckpoint string
		parent                 string
		want                   bool
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

// TestTargetPortsNeeded pins the reservation against the offsets the rest
// of this file actually binds at. The two must agree exactly: reserve too
// few and a run binds outside the range it was allocated, colliding with
// whatever else the operator put there; reserve too many and a range that
// should fit is rejected.
//
// The layout is four blocks of N at fixed offsets -- exports [T, +N),
// their bridges [+N, +2N), verify exports [+2N, +3N), verify bridges
// [+3N, +4N). The verify block sits at +2N whether or not bridging is on
// (see runVerify's own comment for why it must not depend on the write
// export's port), so verification alone still reserves through 3N with the
// bridge block left idle.
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
		{name: "bridged sync reserves the bridge block too", disks: 3, bridging: true, want: 6},
		{name: "verify without bridging still reaches +3N", disks: 3, verify: true, want: 9},
		{name: "verify with bridging reserves all four blocks", disks: 3, bridging: true, verify: true, want: 12},
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

	// The highest offset any code path binds at is base+4N-1, so the
	// reservation for a fully-enabled run must cover exactly that and no
	// more -- a direct restatement of the invariant, independent of the
	// table above.
	const disks = 4
	need := targetPortsNeeded(disks, true, true)
	highestBound := 4*disks - 1
	if need != highestBound+1 {
		t.Errorf("targetPortsNeeded(%d, true, true) = %d, but the highest offset bound is base+%d, so %d ports are required",
			disks, need, highestBound, highestBound+1)
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
