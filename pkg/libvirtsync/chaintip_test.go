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

package libvirtsync

import (
	"strings"
	"testing"
	"time"
)

// chain builds a well-formed chain of n checkpoints, oldest first, with the
// creation times ASCENDING -- the ordinary case, where the clock and the links
// agree.
func chain(n int, start time.Time) []Checkpoint {
	out := make([]Checkpoint, 0, n)
	for i := 1; i <= n; i++ {
		c := Checkpoint{
			Name: checkpointNameFor(i),
			Time: start.Add(time.Duration(i) * time.Second),
		}
		if i > 1 {
			c.Parent = checkpointNameFor(i - 1)
		}
		out = append(out, c)
	}
	return out
}

func checkpointNameFor(i int) string {
	s := "00000" + itoa(i)
	return CheckpointPrefix + "-" + s[len(s)-6:]
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// THE PARENT OF AN INCREMENTAL MUST COME FROM THE CHAIN, NOT FROM A CLOCK.
//
// The tip used to be the last element of a listing sorted by <creationTime>.
// That field is second-granular, libvirt validates nothing about it -- the
// comment on buildAdoptionXML says so -- and vmsync's own bitmap adoption path
// writes time.Now() into it. So the parent an incremental was computed from
// could be decided by a tie in a clock nobody owns, and the parent decides
// which dirty bitmap the delta comes from: the wrong one silently omits
// everything that changed before it.
func TestTheChainDecidesTheTipNotTheClock(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)

	t.Run("the clock disagreeing with the links does not move the tip", func(t *testing.T) {
		// A well-formed chain of three, but the ROOT carries the latest time.
		// A clock step backwards during the chain, or an adoption that stamped
		// time.Now() over an early checkpoint, produces exactly this.
		cps := chain(3, base)
		cps[0].Time = base.Add(time.Hour)

		tip, err := chainTip(cps)
		if err != nil {
			t.Fatalf("chainTip: %v", err)
		}
		if tip.Name != checkpointNameFor(3) {
			t.Errorf("tip = %q, want %q -- the end of the chain is the one nothing claims as a parent, whatever the times say",
				tip.Name, checkpointNameFor(3))
		}

		_, parent, err := NextCheckpointName(cps)
		if err != nil {
			t.Fatalf("NextCheckpointName: %v", err)
		}
		if parent != checkpointNameFor(3) {
			t.Errorf("parent = %q, want %q: an incremental built on %q computes its delta from the wrong bitmap and silently omits everything that changed after it",
				parent, checkpointNameFor(3), parent)
		}
	})

	t.Run("two checkpoints in the same second still give one answer", func(t *testing.T) {
		// The tie that used to be resolved arbitrarily by sort.Slice.
		cps := chain(2, base)
		cps[0].Time = base
		cps[1].Time = base

		tip, err := chainTip(cps)
		if err != nil {
			t.Fatalf("chainTip: %v", err)
		}
		if tip.Name != checkpointNameFor(2) {
			t.Errorf("tip = %q, want %q", tip.Name, checkpointNameFor(2))
		}
	})

	t.Run("the order the listing arrives in does not matter", func(t *testing.T) {
		cps := chain(4, base)
		forward, err := chainTip(cps)
		if err != nil {
			t.Fatalf("chainTip forward: %v", err)
		}
		reversed := make([]Checkpoint, len(cps))
		for i, c := range cps {
			reversed[len(cps)-1-i] = c
		}
		backward, err := chainTip(reversed)
		if err != nil {
			t.Fatalf("chainTip reversed: %v", err)
		}
		if forward.Name != backward.Name {
			t.Errorf("the tip depends on the slice order: %q forward, %q reversed", forward.Name, backward.Name)
		}
	})
}

// A fork is refused, not guessed at.
//
// The reachable way to get one: ClearOrphanBitmap adopts an orphaned bitmap as
// a checkpoint in order to delete it, and a run killed between the adopt and
// the delete leaves that adoption behind -- prefixed, therefore managed,
// parentless, and nobody's parent. Picking either end risks a delta against a
// bitmap that does not describe the replica.
func TestAForkedChainIsRefused(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	cps := append(chain(3, base), Checkpoint{
		// What an interrupted adoption leaves: the bitmap's own name, no
		// parent, stamped with the adopting run's clock.
		Name: CheckpointPrefix + "-000042",
		Time: base.Add(2 * time.Hour),
	})

	if _, err := chainTip(cps); err == nil {
		t.Fatal("a chain with two ends was resolved to one tip rather than refused")
	} else {
		msg := err.Error()
		for _, want := range []string{"2 ends", checkpointNameFor(3), CheckpointPrefix + "-000042", "checkpoint-delete"} {
			if !strings.Contains(msg, want) {
				t.Errorf("the refusal does not mention %q, so an operator cannot tell which end is the stray: %s", want, msg)
			}
		}
	}

	if _, _, err := NextCheckpointName(cps); err == nil {
		t.Error("NextCheckpointName returned a parent for a forked chain, so an incremental would be built on a guess")
	}
}

// A cycle cannot be repaired here either, and must not loop or pick.
func TestACyclicChainIsRefused(t *testing.T) {
	cps := []Checkpoint{
		{Name: CheckpointPrefix + "-000001", Parent: CheckpointPrefix + "-000002"},
		{Name: CheckpointPrefix + "-000002", Parent: CheckpointPrefix + "-000001"},
	}
	_, err := chainTip(cps)
	if err == nil {
		t.Fatal("a cycle was resolved to a tip")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("the refusal does not say the chain is a cycle: %v", err)
	}
}

// The next NAME must not collide with a checkpoint that already exists.
//
// libvirt refuses a create whose name is taken -- "domain moment ... already
// exists" -- and that is not the bitmap collision CreateCheckpoint knows how to
// heal from, so the run simply dies. Taking the number from the highest in use
// rather than from the tip means a stray higher-numbered checkpoint cannot
// cause that.
func TestTheNextNameClearsEveryNumberInUse(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	// A chain whose NUMBERS ARE OUT OF ORDER: 000001 <- 000009 <- 000002.
	//
	// The first version of this test put the high number at the tip, where
	// taking the number from the tip and taking it from the whole set give the
	// same answer -- so it passed against both and a mutation swapping them
	// survived. The number has to be high in the MIDDLE for the two to differ,
	// and that is reachable as a legacy state: the old code derived the next
	// number from whichever checkpoint a clock-sorted listing left last, so a
	// chain numbered out of sequence is exactly what it could produce.
	cps := []Checkpoint{
		{Name: checkpointNameFor(1), Time: base},
		{Name: CheckpointPrefix + "-000009", Parent: checkpointNameFor(1), Time: base.Add(time.Second)},
		{Name: checkpointNameFor(2), Parent: CheckpointPrefix + "-000009", Time: base.Add(2 * time.Second)},
	}

	name, parent, err := NextCheckpointName(cps)
	if err != nil {
		t.Fatalf("NextCheckpointName: %v", err)
	}
	if parent != checkpointNameFor(2) {
		t.Errorf("parent = %q, want %q -- that is the end of the chain", parent, checkpointNameFor(2))
	}
	// Taking it from the tip would give 000003, which is free here; the point
	// is that the number must clear everything in use, so 000009 forces 000010.
	if name != CheckpointPrefix+"-000010" {
		t.Errorf("name = %q, want %s-000010: a name already on the domain is refused by libvirt outright, and that refusal is not the bitmap collision CreateCheckpoint can heal from, so the run simply dies", name, CheckpointPrefix)
	}
}
