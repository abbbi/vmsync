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

package restorepoint

import (
	"testing"
	"time"
)

func inRemove(p Plan, t Tag) bool {
	for _, got := range p.Remove {
		if got.String() == t.String() {
			return true
		}
	}
	return false
}

func inKeep(p Plan, t Tag) bool {
	for _, got := range p.Keep {
		if got.String() == t.String() {
			return true
		}
	}
	return false
}

// THE RUN MUST NOT DELETE THE POINT IT JUST MADE.
//
// Ordering here is by Tag.At, which comes from the SOURCE's checkpoint
// rather than from the clock on the host holding the points. So "newest" is
// not the same as "the one just published, and when a source has been
// running ahead the two come apart:
//
//	a clock jumps forward two days -> points are dated two days ahead
//	the clock is corrected          -> the next point is dated BELOW them
//	the store is at its count       -> the new point sorts last and is cut
//
// The run then paid for a reflink copy of every disk, published it, and
// chose it for deletion -- reporting outcome=taken, overdue=0, count=Count
// throughout. Every number healthy, and no new history. The only signal was
// two adjacent log lines carrying the same tag.
func TestPruneNeverRemovesThePointJustPublished(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	ahead := now.Add(48 * time.Hour)

	existing := []Tag{
		{At: ahead, Checkpoint: "vmsync-cpt-000001"},
		{At: ahead.Add(-time.Hour), Checkpoint: "vmsync-cpt-000002"},
		{At: ahead.Add(-2 * time.Hour), Checkpoint: "vmsync-cpt-000003"},
	}
	// Taken after the clock was corrected, so it is genuinely the newest
	// copy and the oldest-looking tag.
	fresh := Tag{At: now, Checkpoint: "vmsync-cpt-000004"}
	all := append(append([]Tag{}, existing...), fresh)

	plan := Prune(all, fresh, nil, Policy{Count: 3, Interval: time.Hour})

	if inRemove(plan, fresh) {
		t.Error("the point this run just published was chosen for deletion. The run copied every disk, published it, and removed it again -- while reporting that it had taken one")
	}
	if !inKeep(plan, fresh) {
		t.Error("the point this run just published is in neither Keep nor Remove")
	}
	// The policy still holds: three points after this, not four.
	if len(plan.Keep) != 3 {
		t.Errorf("Keep holds %d points, want the policy's 3 -- protecting the new one must reserve a slot, not add one", len(plan.Keep))
	}
	// ...and the slot came from the oldest, which is what retention means.
	oldest := existing[2]
	if !inRemove(plan, oldest) {
		t.Errorf("the oldest point (%s) was not the one dropped to make room", oldest)
	}
}

// Retention off means everything present is somebody's leftovers -- except
// a point this very run made, which is not leftovers by any reading.
func TestPruneWithRetentionOffStillSparesTheNewPoint(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	fresh := Tag{At: now, Checkpoint: "vmsync-cpt-000009"}
	old := Tag{At: now.Add(-time.Hour), Checkpoint: "vmsync-cpt-000008"}

	plan := Prune([]Tag{old, fresh}, fresh, nil, Policy{})

	if inRemove(plan, fresh) {
		t.Error("retention is off, and the point this run just made was listed for removal along with the leftovers")
	}
	if !inRemove(plan, old) {
		t.Error("the pre-existing point was not reported as a leftover")
	}
}

// The zero Tag means "pruning without having published one", and must
// behave exactly as before: no point is privileged.
func TestPruneWithNoNewPointPrivilegesNothing(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	tags := []Tag{
		{At: now, Checkpoint: "vmsync-cpt-000003"},
		{At: now.Add(-time.Hour), Checkpoint: "vmsync-cpt-000002"},
		{At: now.Add(-2 * time.Hour), Checkpoint: "vmsync-cpt-000001"},
	}
	plan := Prune(tags, Tag{}, nil, Policy{Count: 2, Interval: time.Hour})

	if len(plan.Keep) != 2 || len(plan.Remove) != 1 {
		t.Fatalf("Keep=%d Remove=%d, want 2 and 1", len(plan.Keep), len(plan.Remove))
	}
	if plan.Remove[0].Checkpoint != "vmsync-cpt-000001" {
		t.Errorf("removed %s, want the oldest", plan.Remove[0])
	}
}

// Keep stays newest-first, which the Plan's own contract promises and the
// slot-reservation above could easily have broken by appending.
func TestPruneKeepsKeepOrdered(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	ahead := now.Add(48 * time.Hour)
	fresh := Tag{At: now, Checkpoint: "vmsync-cpt-000004"}
	// FOUR points for a count of three, so the reservation path actually
	// runs -- at three it returns early with the sort already done, and
	// this test would pass against an unsorted append. And fresh sorts in
	// the MIDDLE of what survives, so appending it at the end is visibly
	// wrong rather than accidentally right.
	all := []Tag{
		{At: ahead, Checkpoint: "vmsync-cpt-000001"},
		fresh,
		{At: now.Add(-time.Hour), Checkpoint: "vmsync-cpt-000002"},
		{At: now.Add(-2 * time.Hour), Checkpoint: "vmsync-cpt-000003"},
	}
	plan := Prune(all, fresh, nil, Policy{Count: 3, Interval: time.Hour})
	if len(plan.Keep) != 3 {
		t.Fatalf("Keep holds %d, want 3 -- the reservation path did not run, so this proves nothing about ordering", len(plan.Keep))
	}
	for i := 1; i < len(plan.Keep); i++ {
		if plan.Keep[i].At.After(plan.Keep[i-1].At) {
			t.Errorf("Keep is not newest-first at index %d: %s then %s", i, plan.Keep[i-1], plan.Keep[i])
		}
	}
}
