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
	"fmt"
	"testing"
	"time"
)

// Plan.Remove MUST BE OLDEST-FIRST, and the reason is interruption safety:
// a caller deleting them in order has to destroy the least useful history
// first, so a prune killed part-way through has not taken the recent copies.
//
// Remove used to be the reverse of the RANK order, and the rank has two tiers
// -- points with a recorded publish instant, then points without. With both
// populated the two orders diverge, and on a bulk prune they diverge badly:
// a store that grew while the sidecar read was failing has a mixture, and the
// reversed rank frees a copy from an hour ago before copies from days
// earlier. A steady state never shows it, because Remove holds one element
// per cycle and the order is unobservable -- which is why it took a bulk
// fixture to see.
func TestRemoveIsOldestFirstEvenWithBothTiersPopulated(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()

	var all []Tag
	publishedAt := map[string]time.Time{}
	for i := 0; i < 10; i++ {
		tag := Tag{At: base.Add(time.Duration(i) * time.Hour), Checkpoint: fmt.Sprintf("vmsync-cpt-%06d", i+1)}
		all = append(all, tag)
		// Only the even ones can account for themselves, so half land in the
		// known tier and half in the unknown one.
		if i%2 == 0 {
			publishedAt[tag.String()] = tag.At
		}
	}

	plan := Prune(all, Tag{}, publishedAt, Policy{Count: 3, Interval: time.Hour})
	if len(plan.Remove) != 7 {
		t.Fatalf("Remove holds %d, want 7 -- the fixture did not reach the bulk path", len(plan.Remove))
	}

	for i := 1; i < len(plan.Remove); i++ {
		if plan.Remove[i].At.Before(plan.Remove[i-1].At) {
			t.Errorf("Remove is not oldest-first at index %d: %s (%s) then %s (%s). A prune interrupted here has destroyed the newer copy and left the older one on disk, which is the opposite of what the contract promises",
				i, plan.Remove[i-1], plan.Remove[i-1].At.Format(time.RFC3339),
				plan.Remove[i], plan.Remove[i].At.Format(time.RFC3339))
		}
	}

	// Not vacuous: the first thing freed must be the genuinely oldest point
	// in the store, not merely the lowest-ranked.
	if plan.Remove[0].Checkpoint != "vmsync-cpt-000001" {
		t.Errorf("the first point freed is %s, want the oldest copy in the store", plan.Remove[0])
	}
}

// The retention-off sweep makes the same promise and builds Remove by hand.
func TestRemoveIsOldestFirstWithRetentionOff(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	a := Tag{At: base, Checkpoint: "vmsync-cpt-000001"}
	b := Tag{At: base.Add(time.Hour), Checkpoint: "vmsync-cpt-000002"}
	c := Tag{At: base.Add(2 * time.Hour), Checkpoint: "vmsync-cpt-000003"}
	// c can account for itself, so it ranks in the other tier from a and b.
	publishedAt := map[string]time.Time{c.String(): c.At}

	plan := Prune([]Tag{c, a, b}, Tag{}, publishedAt, Policy{})
	if len(plan.Remove) != 3 {
		t.Fatalf("Remove holds %d, want all 3", len(plan.Remove))
	}
	for i := 1; i < len(plan.Remove); i++ {
		if plan.Remove[i].At.Before(plan.Remove[i-1].At) {
			t.Errorf("Remove is not oldest-first at index %d: %s then %s", i, plan.Remove[i-1], plan.Remove[i])
		}
	}
}
