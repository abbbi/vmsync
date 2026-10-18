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
	"strings"
	"testing"
	"time"
)

// runCycles drives the real publish-and-prune loop and reports which runs'
// points survive, in order.
//
// instantFor decides what the tag of run i is dated, so a caller can make the
// clock do anything it likes while the sequence counts normally.
func runCycles(runs int, policy Policy, instantFor func(i int) time.Time) []string {
	seq := map[string]int64{}
	var store []Tag
	for i := 1; i <= runs; i++ {
		fresh := Tag{At: instantFor(i), Checkpoint: fmt.Sprintf("run-%03d", i)}
		seq[fresh.String()] = int64(i)
		plan := Prune(append(append([]Tag{}, store...), fresh), fresh, seq, policy)
		store = plan.Keep
	}
	out := make([]string, 0, len(store))
	for _, t := range store {
		out = append(out, t.Checkpoint)
	}
	return out
}

// NO CLOCK CAN REORDER RETENTION. This is the property the whole field exists
// for, and the one neither earlier fix could provide.
//
// The first fix ordered by the sidecar's taken_at stamped with vmsync's own
// clock, which was INERT -- that is the clock that also stamps the tag, so
// skewing it moved both together. The second read taken_at from the TARGET's
// clock, which was better and still rested on one wall clock: driven over 18
// cycles with a target clock that stepped +48h and was corrected, it
// reproduced the original symptom exactly, two points frozen at the top and
// every later cycle deleting the one before it.
//
// A counter cannot be stepped. Three runs of the same loop, with tag instants
// doing three completely different things, must produce the identical answer
// -- and if any clock could still influence the order, two of these three
// would differ.
func TestPruneOrderingIsUnaffectedByAnyClock(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	const runs = 20
	policy := Policy{Count: 3, Interval: time.Hour}

	sane := runCycles(runs, policy, func(i int) time.Time {
		return base.Add(time.Duration(i) * time.Hour)
	})

	// A clock 48h fast for the first few runs, then corrected -- the shape
	// that produced {ghost, ghost, one real point}.
	correctedSkew := runCycles(runs, policy, func(i int) time.Time {
		at := base.Add(time.Duration(i) * time.Hour)
		if i <= 3 {
			return at.Add(48 * time.Hour)
		}
		return at
	})

	// And a clock that is simply deranged: forward, backward, repeating.
	deranged := runCycles(runs, policy, func(i int) time.Time {
		switch i % 4 {
		case 0:
			return base.Add(72 * time.Hour)
		case 1:
			return base.Add(-72 * time.Hour)
		case 2:
			return base // every one of these is the same instant
		default:
			return base.Add(time.Duration(i) * time.Minute)
		}
	})

	want := []string{"run-020", "run-019", "run-018"}
	for _, c := range []struct {
		name string
		got  []string
	}{
		{"a sane clock", sane},
		{"48h fast then corrected", correctedSkew},
		{"forward, backward and repeating", deranged},
	} {
		if strings.Join(c.got, ",") != strings.Join(want, ",") {
			t.Errorf("with %s the store ends holding %v, want %v. Retention must order by the publish sequence, which no clock can touch",
				c.name, c.got, want)
		}
	}
}

// A point with no sequence ranks below every point that has one, whatever
// their instants say -- so a store that gains the field mid-life converges on
// the sequenced points rather than keeping the unsequenced ones forever.
func TestPruneRetiresUnsequencedPointsFirst(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	policy := Policy{Count: 3, Interval: time.Hour}

	// Three points from before the field existed, dated in the FUTURE so that
	// any clock-based order would keep them.
	seq := map[string]int64{}
	var store []Tag
	for i := 1; i <= 3; i++ {
		store = append(store, Tag{At: base.Add(time.Duration(48+i) * time.Hour), Checkpoint: fmt.Sprintf("old-%03d", i)})
	}

	for i := 1; i <= 4; i++ {
		fresh := Tag{At: base.Add(time.Duration(i) * time.Hour), Checkpoint: fmt.Sprintf("new-%03d", i)}
		seq[fresh.String()] = int64(i)
		plan := Prune(append(append([]Tag{}, store...), fresh), fresh, seq, policy)
		store = plan.Keep
	}

	var got []string
	for _, tg := range store {
		got = append(got, tg.Checkpoint)
	}
	if strings.Join(got, ",") != "new-004,new-003,new-002" {
		t.Errorf("the store holds %v, want the three sequenced points. A point with no place in the order must not outrank one that has it, however it is dated", got)
	}
}

// Within the known tier the order is the sequence and nothing else: the tag
// instants here are deliberately the reverse of the sequence.
func TestPruneKnownTierOrdersBySequenceNotInstant(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	a := Tag{At: base.Add(3 * time.Hour), Checkpoint: "vmsync-cpt-000001"} // newest tag, oldest publish
	b := Tag{At: base.Add(2 * time.Hour), Checkpoint: "vmsync-cpt-000002"}
	c := Tag{At: base.Add(time.Hour), Checkpoint: "vmsync-cpt-000003"} // oldest tag, newest publish
	seq := map[string]int64{a.String(): 1, b.String(): 2, c.String(): 3}

	plan := Prune([]Tag{a, b, c}, Tag{}, seq, Policy{Count: 2, Interval: time.Hour})

	var kept []string
	for _, t := range plan.Keep {
		kept = append(kept, t.Checkpoint)
	}
	if strings.Join(kept, ",") != "vmsync-cpt-000003,vmsync-cpt-000002" {
		t.Errorf("Keep = %v, want the two highest sequences. Ordering by the tag instant would have kept 000001 and 000002", kept)
	}
	if len(plan.Remove) != 1 || plan.Remove[0].Checkpoint != "vmsync-cpt-000001" {
		t.Errorf("Remove = %v, want the lowest sequence", plan.Remove)
	}
}

// A ZERO IN THE MAP IS NOT A PLACE IN THE ORDER.
//
// publishOrder filters zeroes out before Prune ever sees one, so this guards
// against a different caller -- Prune is exported, and a map built by
// something that did not know zero means "no sequence" would otherwise put
// that point in the sequenced tier, where it would outrank every point whose
// sequence is genuinely unknown. 0 compares as a perfectly good int64.
//
// THE FIXTURE NEEDS A POINT ABSENT FROM THE MAP, which is the part that took
// two attempts. With every other point sequenced, a zero sorts last whether
// it is treated as tier 0 with value 0 or as tier 1 -- the two behaviours
// coincide and nothing can tell them apart. It is only against a point with
// no entry at all that they diverge: correct, both are unsequenced and the
// instants decide; wrong, the zero jumps into the sequenced tier and wins.
func TestPruneTreatsAZeroSequenceAsNoSequence(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	// Explicitly zero, and dated OLDER than its rival.
	zeroed := Tag{At: base.Add(time.Hour), Checkpoint: "zeroed-001"}
	// Absent from the map entirely, and dated NEWER.
	absent := Tag{At: base.Add(5 * time.Hour), Checkpoint: "absent-001"}

	seq := map[string]int64{zeroed.String(): 0}

	plan := Prune([]Tag{zeroed, absent}, Tag{}, seq, Policy{Count: 1, Interval: time.Hour})

	if !inRemove(plan, zeroed) {
		var kept []string
		for _, t := range plan.Keep {
			kept = append(kept, t.Checkpoint)
		}
		t.Errorf("the point whose sequence is 0 was kept over a point with no sequence at all and a newer instant; Keep=%v. A zero must not buy a place in the sequenced tier", kept)
	}
	if inRemove(plan, absent) {
		t.Error("the newer unsequenced point was removed in favour of one carrying a zero")
	}
}
