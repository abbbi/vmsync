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

// testToken is the generation every chain built here belongs to.
//
// ONE token per chain, threaded through rather than minted per name, because a
// chain IS a generation: names carrying different tokens inside one
// parent-linked chain describe a state production cannot produce, and a
// fixture in that state would make the carry-forward rule untestable -- any
// implementation would satisfy it.
const testToken = "2610211430-a1b2"

// otherToken is a DIFFERENT generation, for the fixtures that need two.
const otherToken = "2610221500-9f3c"

// chain builds a well-formed chain of n checkpoints, oldest first, all in one
// generation, with the creation times ASCENDING -- the ordinary case, where
// the clock and the links agree.
func chain(n int, start time.Time) []Checkpoint {
	out := make([]Checkpoint, 0, n)
	for i := 1; i <= n; i++ {
		c := Checkpoint{
			Name: checkpointNameFor(testToken, i),
			Time: start.Add(time.Duration(i) * time.Second),
		}
		if i > 1 {
			c.Parent = checkpointNameFor(testToken, i-1)
		}
		out = append(out, c)
	}
	return out
}

// checkpointNameFor spells a name the way NextCheckpointName does: prefix,
// generation token, six-digit position. Taking the token as a parameter is
// what lets a fixture express two generations.
func checkpointNameFor(token string, i int) string {
	s := "00000" + itoa(i)
	return CheckpointPrefix + "-" + token + "-" + s[len(s)-6:]
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

		tip, err := ChainTip(cps)
		if err != nil {
			t.Fatalf("chainTip: %v", err)
		}
		if tip.Name != checkpointNameFor(testToken, 3) {
			t.Errorf("tip = %q, want %q -- the end of the chain is the one nothing claims as a parent, whatever the times say",
				tip.Name, checkpointNameFor(testToken, 3))
		}

		_, parent, err := NextCheckpointName(cps)
		if err != nil {
			t.Fatalf("NextCheckpointName: %v", err)
		}
		if parent != checkpointNameFor(testToken, 3) {
			t.Errorf("parent = %q, want %q: an incremental built on %q computes its delta from the wrong bitmap and silently omits everything that changed after it",
				parent, checkpointNameFor(testToken, 3), parent)
		}
	})

	t.Run("two checkpoints in the same second still give one answer", func(t *testing.T) {
		// The tie that used to be resolved arbitrarily by sort.Slice.
		cps := chain(2, base)
		cps[0].Time = base
		cps[1].Time = base

		tip, err := ChainTip(cps)
		if err != nil {
			t.Fatalf("chainTip: %v", err)
		}
		if tip.Name != checkpointNameFor(testToken, 2) {
			t.Errorf("tip = %q, want %q", tip.Name, checkpointNameFor(testToken, 2))
		}
	})

	t.Run("the order the listing arrives in does not matter", func(t *testing.T) {
		cps := chain(4, base)
		forward, err := ChainTip(cps)
		if err != nil {
			t.Fatalf("chainTip forward: %v", err)
		}
		reversed := make([]Checkpoint, len(cps))
		for i, c := range cps {
			reversed[len(cps)-1-i] = c
		}
		backward, err := ChainTip(reversed)
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
	// What an interrupted adoption leaves: the bitmap's own name, no parent,
	// stamped with the adopting run's clock.
	//
	// A FOREIGN token, because that is where such a bitmap comes from.
	// ClearOrphanBitmap adopts under the bitmap's existing name, and a bitmap
	// with no checkpoint behind it belongs to a generation that is gone -- so
	// the realistic stray carries a token that is not this chain's.
	stray := checkpointNameFor(otherToken, 42)
	cps := append(chain(3, base), Checkpoint{
		Name: stray,
		Time: base.Add(2 * time.Hour),
	})

	if _, err := ChainTip(cps); err == nil {
		t.Fatal("a chain with two ends was resolved to one tip rather than refused")
	} else {
		msg := err.Error()
		for _, want := range []string{"2 ends", checkpointNameFor(testToken, 3), stray, "checkpoint-delete"} {
			if !strings.Contains(msg, want) {
				t.Errorf("the refusal does not mention %q, so an operator cannot tell which end is the stray: %s", want, msg)
			}
		}
	}

	// REFUSED FOR THE FORK, and the reason has to be checked rather than just
	// the fact of an error. The stray's token differs from the chain's, so a
	// token refusal is now also available here -- and it would be the wrong
	// one: it would report an unreadable generation where the real problem is
	// two ends, and the remedy differs.
	if _, _, err := NextCheckpointName(cps); err == nil {
		t.Error("NextCheckpointName returned a parent for a forked chain, so an incremental would be built on a guess")
	} else if !strings.Contains(err.Error(), "2 ends") {
		t.Errorf("a forked chain was refused for some other reason than the fork, so this test no longer measures fork detection: %v", err)
	}
}

// A cycle cannot be repaired here either, and must not loop or pick.
func TestACyclicChainIsRefused(t *testing.T) {
	cps := []Checkpoint{
		{Name: checkpointNameFor(testToken, 1), Parent: checkpointNameFor(testToken, 2)},
		{Name: checkpointNameFor(testToken, 2), Parent: checkpointNameFor(testToken, 1)},
	}
	_, err := ChainTip(cps)
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
		{Name: checkpointNameFor(testToken, 1), Time: base},
		{Name: checkpointNameFor(testToken, 9), Parent: checkpointNameFor(testToken, 1), Time: base.Add(time.Second)},
		{Name: checkpointNameFor(testToken, 2), Parent: checkpointNameFor(testToken, 9), Time: base.Add(2 * time.Second)},
	}

	name, parent, err := NextCheckpointName(cps)
	if err != nil {
		t.Fatalf("NextCheckpointName: %v", err)
	}
	if parent != checkpointNameFor(testToken, 2) {
		t.Errorf("parent = %q, want %q -- that is the end of the chain", parent, checkpointNameFor(testToken, 2))
	}
	// THE TOKEN COMES FROM THE TIP, asserted against chainTip rather than
	// against a literal, and that is deliberate. NextCheckpointName rebuilds
	// the name from CheckpointPrefix and discards the prefix group
	// highestCheckpointNumber captured, so an implementation that mints on an
	// empty chain but forgets to re-insert the tip's token on a non-empty one
	// is caught here and in almost nowhere else: chainTip is link-based,
	// highestCheckpointNumber is digit-based, and the metadata round-trips are
	// byte-opaque. A literal would pin the same thing until somebody changed
	// the fixture's token and updated the literal to match.
	//
	// It also has to be the TIP's token and not the highest-numbered entry's.
	// Here they are the same, because a chain is one generation;
	// TestTheTokenIsReadFromTheTip below makes them differ.
	tipToken, ok := GenerationToken(parent)
	if !ok {
		t.Fatal("the fixture's tip carries no readable token, so this test cannot check what carries forward")
	}
	// Taking the number from the tip would give 000003, which is free here;
	// the point is that it must clear everything in use, so 000009 forces
	// 000010.
	if want := checkpointNameFor(tipToken, 10); name != want {
		t.Errorf("name = %q, want %q: the number must clear every one in use -- a name already on the domain is refused by libvirt outright, and that refusal is not the bitmap collision CreateCheckpoint can heal from, so the run simply dies -- and the token must be the tip's, carried forward unchanged", name, want)
	}
}

// WHICH ENTRY the token is read from, which only a mixed chain can ask.
//
// Every chain production creates is one generation, so in any well-formed
// fixture the tip's token and the highest-numbered entry's token are the same
// string and an implementation reading either passes. This fixture is
// therefore a state vmsync must never produce -- a parent link crossing two
// generations -- and it exists for exactly that reason: it is the only shape
// in which "carry the TIP's token" and "carry the highest-numbered entry's
// token" give different answers, and the second is the plausible mistake,
// since the number already comes from the highest.
//
// Numbers out of order on purpose: tip is 000002 of the newer generation while
// 000009 of the older one holds the highest number.
func TestTheTokenIsReadFromTheTip(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	cps := []Checkpoint{
		{Name: checkpointNameFor(otherToken, 1), Time: base},
		{Name: checkpointNameFor(otherToken, 9), Parent: checkpointNameFor(otherToken, 1), Time: base.Add(time.Second)},
		{Name: checkpointNameFor(testToken, 2), Parent: checkpointNameFor(otherToken, 9), Time: base.Add(2 * time.Second)},
	}

	name, parent, err := NextCheckpointName(cps)
	if err != nil {
		t.Fatalf("NextCheckpointName: %v", err)
	}
	if parent != checkpointNameFor(testToken, 2) {
		t.Fatalf("parent = %q, want the tip %q", parent, checkpointNameFor(testToken, 2))
	}
	got, ok := GenerationToken(name)
	if !ok {
		t.Fatalf("the minted name %q carries no readable token", name)
	}
	if got != testToken {
		t.Errorf("the next name carries token %q, want the TIP's %q (the name is %q). Reading it from the highest-numbered entry instead gives %q, which attributes the new checkpoint to a generation that is not the one it extends",
			got, testToken, name, otherToken)
	}
	// And the number still clears everything in use.
	if want := checkpointNameFor(testToken, 10); name != want {
		t.Errorf("name = %q, want %q", name, want)
	}
}
