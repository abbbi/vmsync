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
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"vmsync/pkg/restorepoint"
)

// sweepRunner answers the three commands sweepDisplacedSets issues -- the
// listing, the size query, and then one rm -rf per set it decided to remove --
// and records the removals.
//
// This is what makes the half of the sweep that actually deletes things
// testable at all. It was not, until the replica_incomplete read moved out to
// the caller: that read takes a concrete *libvirtsync.Manager and a nil one
// panics inside it, so nothing past it could be reached and the removal loop
// was covered by neither a unit test nor an assertion. Two transpositions of
// the decision's buckets went undetected because of it -- sending the
// not-old-enough sets to rm -rf left the whole suite green.
type sweepRunner struct {
	scan  string
	sizes string
	// removed is every path rm -rf was pointed at, in order.
	removed []string
	// failRemoveOf makes one removal fail, so the sweep's own
	// carry-on-and-report path can be exercised.
	failRemoveOf string
}

func (r *sweepRunner) Run(_ context.Context, cmd string) (string, error) {
	switch {
	case strings.HasPrefix(cmd, "rm -rf -- "):
		p := strings.TrimPrefix(cmd, "rm -rf -- ")
		p = strings.Trim(p, "'")
		r.removed = append(r.removed, p)
		if r.failRemoveOf != "" && strings.Contains(p, r.failRemoveOf) {
			return "rm: read-only file system", errors.New("exit status 1")
		}
		return "", nil
	case strings.Contains(cmd, "du "):
		return r.sizes, nil
	default:
		return r.scan, nil
	}
}

// sweepFixture is the same seven shapes reclaimFixture derives, as the scan
// output a target would actually print, plus the sizes for them.
func sweepFixture() (scan, sizes string, stampedOld, unstampedOld []string) {
	oldStamp := reclaimNow.Add(-40 * 24 * time.Hour).Unix()
	freshStamp := reclaimNow.Add(-1 * time.Hour).Unix()

	stampedOld = []string{
		fmt.Sprintf("/data/web01.qcow2%s%d", restorepoint.ReplicaReplacedSuffix, oldStamp),
		fmt.Sprintf("/data/web01.qcow2%s%d", restorepoint.RestoreTempSuffix, oldStamp),
		fmt.Sprintf("/data/.vmsync-rp/%s%sweb01-%d", restorepoint.AsidePrefix, restorepoint.DomainPrefix, oldStamp),
	}
	unstampedOld = []string{
		"/data/web01.qcow2" + restorepoint.ReplicaReplacedSuffix + "keepme",
		"/data/web01.qcow2" + restorepoint.RestoreTempSuffix + "keepme",
	}
	tooYoung := []string{
		fmt.Sprintf("/data/web01.qcow2%s%d", restorepoint.ReplicaReplacedSuffix, freshStamp),
		"/data/web01.qcow2" + restorepoint.ReplicaReplacedSuffix + "keep-recent",
	}

	var scanLines, sizeLines []string
	add := func(p string, mtime int64, dir bool, bytes int64) {
		kind := "f"
		if dir {
			kind = "d"
		}
		scanLines = append(scanLines, fmt.Sprintf("%s\t%d\t%s", kind, mtime, p))
		sizeLines = append(sizeLines, fmt.Sprintf("%d\t%s", bytes, p))
	}
	for i, p := range stampedOld {
		add(p, oldStamp, strings.Contains(p, restorepoint.AsidePrefix), int64(i+1)<<30)
	}
	for i, p := range unstampedOld {
		add(p, oldStamp, false, int64(i+10)<<30)
	}
	for i, p := range tooYoung {
		add(p, freshStamp, false, int64(i+20)<<30)
	}
	return leftoverScanMarker + "\n" + strings.Join(scanLines, "\n") + "\n",
		leftoverScanMarker + "\n" + strings.Join(sizeLines, "\n") + "\n",
		stampedOld, unstampedOld
}

func sweepCfg() syncConfig {
	return syncConfig{
		TargetDomain:             "web01",
		ReclaimLeftoversAfterDur: 30 * 24 * time.Hour,
	}
}

// What rm -rf is actually pointed at.
//
// The decision is tested on its own in reclaimable_test.go; this is the other
// half, and the one that deletes things: that the bucket the removal loop
// consumes is the bucket the decision put the removable sets in. A transposition
// at the call site compiles, and this is what refuses it.
func TestTheSweepRemovesOnlyTheSetsItMayRemove(t *testing.T) {
	scan, sizes, stampedOld, unstampedOld := sweepFixture()
	r := &sweepRunner{scan: scan, sizes: sizes}

	sweep := sweepDisplacedSets(context.Background(), sweepCfg(), r, "", nil,
		[]string{"/data/web01.qcow2"}, reclaimNow)

	if len(r.removed) != len(stampedOld) {
		t.Fatalf("rm -rf was pointed at %d paths, want %d:\n  got:  %v\n  want: %v",
			len(r.removed), len(stampedOld), r.removed, stampedOld)
	}
	for _, want := range stampedOld {
		found := false
		for _, got := range r.removed {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("a set old enough and provably vmsync's was not removed: %s", want)
		}
	}
	for _, never := range unstampedOld {
		for _, got := range r.removed {
			if got == never {
				t.Errorf("rm -rf was pointed at a set whose age came from an mtime: %s -- the remover would have refused it, which is the loop this change exists to end, and reaching it at all means the decision's buckets are crossed", never)
			}
		}
	}
	if len(sweep.Removed) != len(stampedOld) {
		t.Errorf("sweep.Removed has %d, want %d", len(sweep.Removed), len(stampedOld))
	}
}

// The numbers, which are the whole point of reporting these at all.
func TestTheSweepCountsWhatItWillNeverReclaim(t *testing.T) {
	scan, sizes, _, unstampedOld := sweepFixture()
	r := &sweepRunner{scan: scan, sizes: sizes}

	sweep := sweepDisplacedSets(context.Background(), sweepCfg(), r, "", nil,
		[]string{"/data/web01.qcow2"}, reclaimNow)

	if len(sweep.Unreclaimable) != len(unstampedOld) {
		t.Fatalf("sweep.Unreclaimable has %d sets, want %d: %v",
			len(sweep.Unreclaimable), len(unstampedOld), pathsOf(sweep.Unreclaimable))
	}
	// 10 GiB + 11 GiB, from sweepFixture's own sizes.
	var want int64 = (10 << 30) + (11 << 30)
	if sweep.UnreclaimableBytes != want {
		t.Errorf("UnreclaimableBytes = %d, want %d -- this is the figure that tells an operator whether to care, and a count without it says nothing about a disk",
			sweep.UnreclaimableBytes, want)
	}
	if sweep.UnreclaimableBytes >= sweep.FoundBytes {
		t.Errorf("UnreclaimableBytes (%d) is not less than FoundBytes (%d), so it is counting sets it should not",
			sweep.UnreclaimableBytes, sweep.FoundBytes)
	}
}

// The interlock, from the caller's side now that the marker is passed in.
func TestTheSweepRemovesNothingWhenTheReplicaIsIncomplete(t *testing.T) {
	scan, sizes, _, _ := sweepFixture()

	t.Run("the marker is set", func(t *testing.T) {
		r := &sweepRunner{scan: scan, sizes: sizes}
		sweep := sweepDisplacedSets(context.Background(), sweepCfg(), r, "copy:1799999999", nil,
			[]string{"/data/web01.qcow2"}, reclaimNow)
		if len(r.removed) != 0 {
			t.Errorf("rm -rf was issued for %v although the replica is marked incomplete -- those asides are the only complete replica there is", r.removed)
		}
		if sweep.Refused == "" {
			t.Error("the sweep did not record why it stood down")
		}
		if len(sweep.Unreclaimable) != 0 {
			t.Errorf("Unreclaimable = %v, want none while the whole sweep is refused", pathsOf(sweep.Unreclaimable))
		}
	})

	// FAILS CLOSED. An unreadable marker might be a set marker.
	t.Run("the marker could not be read", func(t *testing.T) {
		r := &sweepRunner{scan: scan, sizes: sizes}
		sweep := sweepDisplacedSets(context.Background(), sweepCfg(), r, "", errors.New("libvirt is away"),
			[]string{"/data/web01.qcow2"}, reclaimNow)
		if len(r.removed) != 0 {
			t.Errorf("rm -rf was issued for %v although the marker could not be read", r.removed)
		}
		if sweep.Refused == "" {
			t.Error("the sweep did not record why it stood down")
		}
	})
}

// Reclaiming off: everything is found and reported, nothing is touched.
func TestTheSweepTouchesNothingUntilAnOperatorSetsADuration(t *testing.T) {
	scan, sizes, _, _ := sweepFixture()
	r := &sweepRunner{scan: scan, sizes: sizes}
	cfg := sweepCfg()
	cfg.ReclaimLeftoversAfterDur = 0

	sweep := sweepDisplacedSets(context.Background(), cfg, r, "", nil,
		[]string{"/data/web01.qcow2"}, reclaimNow)

	if len(r.removed) != 0 {
		t.Errorf("rm -rf was issued for %v with reclaiming off", r.removed)
	}
	if len(sweep.Found) != 7 {
		t.Errorf("found %d sets, want all 7 reported even though none may be removed", len(sweep.Found))
	}
	if sweep.FoundBytes == 0 {
		t.Error("FoundBytes is zero, so the line that tells an operator what the asides cost says nothing")
	}
	if len(sweep.Unreclaimable) != 0 {
		t.Errorf("Unreclaimable = %v: with no duration set there is no distinction to draw yet, and naming some of them would read as advice", pathsOf(sweep.Unreclaimable))
	}
}

// A removal that fails is reported and the rest continue: the sweep must never
// fail a sync, and one read-only mount must not strand the other sets.
func TestOneFailedRemovalDoesNotStopTheRest(t *testing.T) {
	scan, sizes, stampedOld, _ := sweepFixture()
	r := &sweepRunner{scan: scan, sizes: sizes, failRemoveOf: restorepoint.RestoreTempSuffix}

	sweep := sweepDisplacedSets(context.Background(), sweepCfg(), r, "", nil,
		[]string{"/data/web01.qcow2"}, reclaimNow)

	if len(r.removed) != len(stampedOld) {
		t.Errorf("rm -rf was attempted for %d paths, want all %d attempted even though one fails", len(r.removed), len(stampedOld))
	}
	if len(sweep.Removed) != len(stampedOld)-1 {
		t.Errorf("sweep.Removed has %d, want %d: a removal that failed must not be counted as freed space", len(sweep.Removed), len(stampedOld)-1)
	}
	for _, l := range sweep.Removed {
		if strings.Contains(l.Path, restorepoint.RestoreTempSuffix) {
			t.Errorf("%s failed to be removed and was still recorded as removed", l.Path)
		}
	}
}
