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
	"testing"
	"time"

	"vmsync/pkg/restorepoint"
)

// reclaimNow is the sweeping host's clock for these tests, fixed so the age
// arithmetic is repeatable.
var reclaimNow = time.Unix(1800000000, 0).UTC()

// leftoverAged builds one displaced set of a given age.
func leftoverAged(kind string, age time.Duration) restorepoint.Leftover {
	return restorepoint.Leftover{
		Path:          "/data/web01.qcow2" + restorepoint.ReplicaReplacedSuffix + "1",
		Kind:          kind,
		AtUnix:        reclaimNow.Add(-age).Unix(),
		StampFromName: true,
		Bytes:         32 << 30,
	}
}

// Reclaiming is off unless an operator asked for it, and "off" means nothing is
// a candidate rather than everything being one.
//
// The default matters more than it looks. These files are made by the DEFAULT
// -replaced-disk-action, on every rebuild, on every pair -- so a sweep that
// defaulted to deleting them would have turned a known accumulation into an
// unannounced deletion of the documented recovery for a failed rebuild, on every
// estate that upgraded.
func TestNothingIsReclaimedUnlessAnOperatorSetADuration(t *testing.T) {
	found := []restorepoint.Leftover{
		leftoverAged(restorepoint.LeftoverReplacedDisk, 365*24*time.Hour),
		leftoverAged(restorepoint.LeftoverAsideStore, 365*24*time.Hour),
	}
	for _, after := range []time.Duration{0, -time.Hour} {
		reclaim, keep, refused := leftoversToReclaim(found, after, reclaimNow, "")
		if len(reclaim) != 0 {
			t.Errorf("after=%s reclaimed %+v; with no duration set nothing has been asked for", after, reclaim)
		}
		if len(keep) != len(found) {
			t.Errorf("after=%s kept %d of %d", after, len(keep), len(found))
		}
		if refused != "" {
			t.Errorf("after=%s refused %q; nothing was refused, nothing was asked", after, refused)
		}
	}
}

func TestLeftoversToReclaimTakesOnlyWhatIsOldEnough(t *testing.T) {
	older := leftoverAged(restorepoint.LeftoverReplacedDisk, 31*24*time.Hour)
	older.Path = "/data/old.qcow2" + restorepoint.ReplicaReplacedSuffix + "1"
	exactly := leftoverAged(restorepoint.LeftoverReplacedDisk, 30*24*time.Hour)
	exactly.Path = "/data/exact.qcow2" + restorepoint.ReplicaReplacedSuffix + "1"
	newer := leftoverAged(restorepoint.LeftoverRestoreStaging, 29*24*time.Hour)
	newer.Path = "/data/new.qcow2" + restorepoint.ReplicaReplacedSuffix + "1"
	// Neither the name nor the filesystem could say when this was displaced.
	unknown := restorepoint.Leftover{Path: "/data/unknown.qcow2" + restorepoint.ReplicaReplacedSuffix + "1", Kind: restorepoint.LeftoverReplacedDisk}

	reclaim, keep, refused := leftoversToReclaim(
		[]restorepoint.Leftover{older, exactly, newer, unknown},
		30*24*time.Hour, reclaimNow, "",
	)
	if refused != "" {
		t.Fatalf("refused = %q", refused)
	}
	got := map[string]bool{}
	for _, l := range reclaim {
		got[l.Path] = true
	}
	if !got[older.Path] {
		t.Error("a set a day past the duration was kept")
	}
	if !got[exactly.Path] {
		t.Error("a set exactly at the duration was kept; the boundary is inclusive, so a daily run with a 30-day duration reclaims on day 30 rather than never quite reaching it")
	}
	if got[newer.Path] {
		t.Error("a set a day short of the duration was reclaimed")
	}
	if got[unknown.Path] {
		t.Error("a set with no readable age was reclaimed -- \"no idea how old this is\" must not read as \"older than anything\"")
	}
	if len(keep) != 2 {
		t.Errorf("kept %d, want the 2 that are not old enough: %+v", len(keep), keep)
	}
}

// An incomplete replica refuses the whole sweep, and that is the interlock this
// feature could not ship without.
//
// replica_incomplete means a full copy did not finish. The aside files beside
// those disks are the COMPLETE replica that copy replaced, and putting them back
// is the documented recovery -- vmsync's own promotion refusal says so in as many
// words. Reclaiming them would leave a half-written image whose last_checkpoint,
// last_sync_timestamp and replica_source all still describe the replica it
// replaced, so every other health check reads green.
//
// The value's PRESENCE is the finding, so a marker this build cannot parse still
// stops the sweep.
func TestAnIncompleteReplicaRefusesTheWholeSweep(t *testing.T) {
	found := []restorepoint.Leftover{
		leftoverAged(restorepoint.LeftoverReplacedDisk, 400*24*time.Hour),
		leftoverAged(restorepoint.LeftoverAsideStore, 400*24*time.Hour),
	}
	for _, marker := range []string{
		"verb=reinit,at=1758441600,action=9f3c1a2b4d5e6f70,host=hv-a,aside=1758441600",
		// Unparsable, and a future build's spelling. Both still refuse.
		"something this build has never seen",
		"  verb=reinit  ",
	} {
		reclaim, keep, refused := leftoversToReclaim(found, 24*time.Hour, reclaimNow, marker)
		if refused == "" {
			t.Errorf("marker %q did not refuse the sweep: these asides are the only complete copy of the replica there is", marker)
		}
		if len(reclaim) != 0 {
			t.Errorf("marker %q still reclaimed %+v -- a caller that ignored the refusal would delete the recovery", marker, reclaim)
		}
		if len(keep) != len(found) {
			t.Errorf("marker %q kept %d of %d", marker, len(keep), len(found))
		}
	}
	// And whitespace alone is not a marker: an empty element is not a finding.
	if _, _, refused := leftoversToReclaim(found, 24*time.Hour, reclaimNow, "   "); refused != "" {
		t.Errorf("whitespace was read as a finding (%q); the marker is absent, and refusing for ever on it would make the sweep unreachable", refused)
	}
}

func TestBytesOfSumsWhatWouldBeFreed(t *testing.T) {
	if got := bytesOf(nil); got != 0 {
		t.Errorf("bytesOf(nil) = %d", got)
	}
	got := bytesOf([]restorepoint.Leftover{{Bytes: 30 << 30}, {Bytes: 5 << 30}, {Bytes: 0}})
	if got != int64(35)<<30 {
		t.Errorf("bytesOf = %d, want %d", got, int64(35)<<30)
	}
}

// The floor is what stops a mistyped duration sweeping an aside made minutes
// ago, and it is worth pinning because the two realistic typos both land under
// it: a "0h" that means "off" to the person writing it, and a "30" meant as days
// that ParseDuration refuses outright.
func TestReclaimFloorIsLongerThanAWorkingDay(t *testing.T) {
	if minReclaimLeftoversAfter < 24*time.Hour {
		t.Errorf("minReclaimLeftoversAfter is %s: the window in which somebody notices a rebuild went wrong and wants the previous replica back is measured in hours to days, so a floor under a day automates away the recovery", minReclaimLeftoversAfter)
	}
	// Sanity: a set younger than the floor can never be reclaimed at the
	// shortest duration the flag accepts.
	fresh := leftoverAged(restorepoint.LeftoverReplacedDisk, time.Hour)
	reclaim, _, _ := leftoversToReclaim([]restorepoint.Leftover{fresh}, minReclaimLeftoversAfter, reclaimNow, "")
	if len(reclaim) != 0 {
		t.Errorf("an aside made an hour ago was reclaimed at the shortest accepted duration: %+v", reclaim)
	}
}
