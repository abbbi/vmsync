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
	"fmt"
	"strings"
	"testing"
	"time"

	"vmsync/pkg/restorepoint"
)

// leftoverScanMarker is what LeftoverScanCommand has the target echo before its
// listing. Spelled out here because it is unexported, and that is safe: if it
// ever changes, ParseLeftoverScan returns "the command did not run" and every
// test below fails loudly rather than quietly attributing nothing.
const leftoverScanMarker = "__VMSYNC_LEFTOVER_SCAN__"

// scanned turns scan lines into the Leftovers the REAL attributor produces.
//
// Hand-written Leftover literals were how the first version of these tests went
// wrong: two of its fixtures were shapes AttributeLeftovers cannot emit at all
// -- a path with vmsync's suffix and nothing after it is not attributed as a
// leftover, so a test built on one proves nothing about any input the sweep can
// actually receive. Deriving them means a fixture that stops being reachable
// stops being a fixture, and says so here rather than passing for ever.
//
// wantCount is asserted because a dropped fixture is otherwise invisible: the
// table shrinks and every assertion about it still holds.
func scanned(t *testing.T, wantCount int, lines ...string) []restorepoint.Leftover {
	t.Helper()
	entries, err := restorepoint.ParseLeftoverScan(leftoverScanMarker + "\n" + strings.Join(lines, "\n") + "\n")
	if err != nil {
		t.Fatalf("parse the scan: %v", err)
	}
	found, unparsed := restorepoint.AttributeLeftovers(entries, []string{"/data/web01.qcow2"}, "web01")
	if len(unparsed) != 0 {
		t.Fatalf("unparsed entries: %v", unparsed)
	}
	if len(found) != wantCount {
		t.Fatalf("the attributor produced %d leftovers, want %d -- a fixture below is a shape it does not emit:\n%s",
			len(found), wantCount, describe(found))
	}
	return found
}

func describe(ls []restorepoint.Leftover) string {
	var b strings.Builder
	for _, l := range ls {
		fmt.Fprintf(&b, "  %s kind=%s at=%d stamped=%t\n", l.Path, l.Kind, l.AtUnix, l.StampFromName)
	}
	return b.String()
}

// reclaimFixture is every shape the attributor can hand the sweep, at ages
// either side of a 30-day duration.
//
// The unstamped ones carry vmsync's suffix with a non-timestamp after it, which
// is the only shape that reaches that bucket -- and only for replaced disks and
// restore staging copies. An aside store whose trailer is not all digits is not
// attributed at all (sameDirAside), so that kind can never be unstamped, and
// there is deliberately no fixture pretending otherwise.
func reclaimFixture(t *testing.T) []restorepoint.Leftover {
	t.Helper()
	oldStamp := reclaimNow.Add(-40 * 24 * time.Hour).Unix()
	oldMtime := oldStamp
	freshStamp := reclaimNow.Add(-1 * time.Hour).Unix()
	freshMtime := freshStamp

	return scanned(t, 7,
		// Name-stamped and old: these should go.
		fmt.Sprintf("f\t%d\t/data/web01.qcow2%s%d", oldMtime, restorepoint.ReplicaReplacedSuffix, oldStamp),
		fmt.Sprintf("f\t%d\t/data/web01.qcow2%s%d", oldMtime, restorepoint.RestoreTempSuffix, oldStamp),
		fmt.Sprintf("d\t%d\t/data/.vmsync-rp/%s%sweb01-%d", oldMtime, restorepoint.AsidePrefix, restorepoint.DomainPrefix, oldStamp),

		// Name-stamped and not old enough.
		fmt.Sprintf("f\t%d\t/data/web01.qcow2%s%d", freshMtime, restorepoint.ReplicaReplacedSuffix, freshStamp),

		// NO stamp: vmsync's suffix with something else after it. Old by mtime,
		// and the remover will not touch either of them.
		fmt.Sprintf("f\t%d\t/data/web01.qcow2%skeepme", oldMtime, restorepoint.ReplicaReplacedSuffix),
		fmt.Sprintf("f\t%d\t/data/web01.qcow2%skeepme", oldMtime, restorepoint.RestoreTempSuffix),

		// No stamp AND not old enough: the state that must stay in keep rather
		// than being reported as something to deal with by hand.
		fmt.Sprintf("f\t%d\t/data/web01.qcow2%skeep-recent", freshMtime, restorepoint.ReplicaReplacedSuffix),
	)
}

func pathsOf(ls []restorepoint.Leftover) []string {
	out := make([]string, 0, len(ls))
	for _, l := range ls {
		out = append(out, l.Path)
	}
	return out
}

func has(ls []restorepoint.Leftover, substr string) bool {
	for _, l := range ls {
		if strings.Contains(l.Path, substr) {
			return true
		}
	}
	return false
}

// THE INVARIANT, and the defect was its violation: everything chosen for
// removal has to be something the remover will actually remove.
//
// leftoversToReclaim decided on age alone, while LeftoverRemoveCommand refuses
// any path not ending in its kind's suffix followed by a unix timestamp -- the
// only evidence vmsync made the thing. A set whose age came from an mtime
// therefore satisfied the first and failed the second, on every run, for ever:
// selected, refused, "leaving a displaced set in place" in the log, repeat.
//
// Asserted as a property over attributor-derived inputs, because the two halves
// live in different packages and will be edited by different changes.
func TestEverythingSelectedForReclaimCanActuallyBeRemoved(t *testing.T) {
	d := leftoversToReclaim(reclaimFixture(t), 30*24*time.Hour, reclaimNow, "")
	if d.Refused != "" {
		t.Fatalf("refused = %q, want none", d.Refused)
	}
	if len(d.Reclaim) == 0 {
		t.Fatal("nothing was selected, so this proves only that an empty list is consistent")
	}
	for _, l := range d.Reclaim {
		cmd, err := restorepoint.LeftoverRemoveCommand(l)
		if err != nil {
			t.Errorf("selected for removal but the remover refuses it, so this run logs \"leaving a displaced set in place\" and so will every run after it:\n  path: %s\n  kind: %s\n  stamp_from_name: %t\n  error: %v",
				l.Path, l.Kind, l.StampFromName, err)
			continue
		}
		if !strings.HasPrefix(cmd, "rm -rf -- ") {
			t.Errorf("%s: command is %q, which is not the removal this decision authorises", l.Path, cmd)
		}
	}
}

// Each set lands in exactly one bucket. The sweep's reported byte figures are
// derived from the three lists, so a set in two of them is double-counted and
// one in none has vanished from a report whose whole purpose is to account for
// disk space.
func TestEveryDisplacedSetLandsInExactlyOneBucket(t *testing.T) {
	found := reclaimFixture(t)
	d := leftoversToReclaim(found, 30*24*time.Hour, reclaimNow, "")

	seen := map[string]int{}
	for _, group := range [][]restorepoint.Leftover{d.Reclaim, d.Keep, d.Unstamped} {
		for _, l := range group {
			seen[l.Path]++
		}
	}
	for _, l := range found {
		switch seen[l.Path] {
		case 1:
		case 0:
			t.Errorf("%s is in no bucket, so it is missing from the sweep's accounting entirely", l.Path)
		default:
			t.Errorf("%s is in %d buckets, so its bytes are counted more than once", l.Path, seen[l.Path])
		}
	}
	if got := len(d.Reclaim) + len(d.Keep) + len(d.Unstamped); got != len(found) {
		t.Errorf("the three buckets hold %d of %d sets", got, len(found))
	}
}

// What goes where, stated once, so a change that moves a set between buckets
// has to say which and why.
func TestTheBucketsSplitByAgeAndByEvidence(t *testing.T) {
	d := leftoversToReclaim(reclaimFixture(t), 30*24*time.Hour, reclaimNow, "")

	// Old and provably vmsync's: all three kinds.
	if len(d.Reclaim) != 3 {
		t.Errorf("reclaim = %v, want the 3 name-stamped sets past the duration", pathsOf(d.Reclaim))
	}
	for _, l := range d.Reclaim {
		if !l.StampFromName {
			t.Errorf("%s was selected although its age came from an mtime", l.Path)
		}
	}

	// Old by mtime only: reported, never removed.
	if len(d.Unstamped) != 2 {
		t.Errorf("unstamped = %v, want the 2 old sets whose age came from an mtime", pathsOf(d.Unstamped))
	}
	for _, l := range d.Unstamped {
		if l.StampFromName {
			t.Errorf("%s is name-stamped yet reported unreclaimable; it could simply have been removed", l.Path)
		}
	}

	// NOT OLD ENOUGH, stamped or not. The unstamped-but-recent one is the case
	// that matters: filing it as unreclaimable would send somebody to delete a
	// set by hand that nothing was going to touch anyway.
	if len(d.Keep) != 2 {
		t.Errorf("keep = %v, want the 2 that are not old enough", pathsOf(d.Keep))
	}
	if !has(d.Keep, "keep-recent") {
		t.Errorf("the unstamped set that is not old enough yet is not in keep: %v", pathsOf(d.Keep))
	}
	if has(d.Unstamped, "keep-recent") {
		t.Error("an unstamped set that is not old enough yet was reported as something to deal with by hand, which is advice to delete a set this sweep was never going to touch")
	}
}

// The incomplete-replica interlock empties all three buckets, not just two.
// These asides are the only complete replica there is when that marker is set,
// and "needs a person" is not a reason to start naming them in a warning that
// tells somebody to delete things by hand.
func TestAnIncompleteReplicaAlsoSuppressesTheUnreclaimableReport(t *testing.T) {
	found := reclaimFixture(t)
	d := leftoversToReclaim(found, 30*24*time.Hour, reclaimNow, "copy:1799999999")
	if d.Refused == "" {
		t.Fatal("the sweep was not refused although the replica is marked incomplete")
	}
	if len(d.Reclaim) != 0 {
		t.Errorf("reclaim = %v, want none", pathsOf(d.Reclaim))
	}
	if len(d.Unstamped) != 0 {
		t.Errorf("unstamped = %v, want none: while the whole sweep is refused there is no advice to give about any of them", pathsOf(d.Unstamped))
	}
	if len(d.Keep) != len(found) {
		t.Errorf("kept %d of %d; a caller that ignored Refused has to still see everything on the keep side", len(d.Keep), len(found))
	}
}
