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
	"strings"
	"testing"

	"vmsync/pkg/libvirtsync"
)

const (
	genA = "2610211430-a1b2"
	genB = "2610221500-9f3c"
)

func cptName(token string, n int) string {
	switch n {
	case 1:
		return libvirtsync.CheckpointPrefix + "-" + token + "-000001"
	case 2:
		return libvirtsync.CheckpointPrefix + "-" + token + "-000002"
	case 7:
		return libvirtsync.CheckpointPrefix + "-" + token + "-000007"
	default:
		panic("extend cptName for this position")
	}
}

// WHICH CHECKPOINT planCheckpointRecovery CALLS THE TIP, and it matters more
// here than anywhere else: this function's non-empty return is handed straight
// to DeleteCheckpointIfExists, which removes the checkpoint AND its bitmap.
//
// It used to read existing[len(existing)-1] -- the last element of whatever
// order ListManagedCheckpoints returned, which is a sort by <creationTime>,
// a field libvirt does not validate and vmsync's own adoption path writes as
// time.Now(), with a lexicographic tie-break on the name. The positional tip
// was removed from NextCheckpointName for exactly that reason and this copy
// was left behind, in the function that deletes.
//
// TestPlanCheckpointRecovery's own fixtures cannot see the difference: its
// helper threads Parent through the argument order, so slice position and
// parent links agree in every one of them. These two do not agree, which is
// the only shape where the two notions of "newest" give different answers.
func TestPlanCheckpointRecoveryAsksTheLinksNotTheOrder(t *testing.T) {
	// The real tip is 000002 (nothing claims it as a parent); the slice puts
	// the ROOT last. A sort that ordered these by a tied creationTime and
	// broke the tie on the name would produce exactly this.
	outOfOrder := []libvirtsync.Checkpoint{
		{Name: cptName(genA, 2), Parent: cptName(genA, 1)},
		{Name: cptName(genA, 1)},
	}

	t.Run("the pending tip is still recognised and removed", func(t *testing.T) {
		got, err := planCheckpointRecovery(cptName(genA, 2), cptName(genA, 1), outOfOrder)
		if err != nil {
			t.Fatalf("the pending checkpoint IS the chain's tip, so it must be removable: %v", err)
		}
		if got != cptName(genA, 2) {
			t.Errorf("got %q, want %q removed -- read positionally the tip is the root here, and the run would be refused for a divergence that does not exist", got, cptName(genA, 2))
		}
	})

	// THE DANGEROUS DIRECTION. Read positionally, the root is "the tip" and
	// equals pending, so it would be returned for deletion -- removing a
	// checkpoint the one after it depends on, and its bitmap with it.
	t.Run("a pending checkpoint that is not the tip is refused, not deleted", func(t *testing.T) {
		got, err := planCheckpointRecovery(cptName(genA, 1), cptName(genA, 2), outOfOrder)
		if err == nil {
			t.Fatalf("got %q for deletion: positionally the root is last here, but %q is the chain's tip and depends on it. Deleting a mid-chain checkpoint removes a bitmap a later one builds on", got, cptName(genA, 2))
		}
		if !strings.Contains(err.Error(), cptName(genA, 2)) {
			t.Errorf("the refusal does not name the checkpoint that IS the tip, so an operator cannot see what the disagreement is: %v", err)
		}
	})
}

// A chain with no single end has no answer to "is the pending one still the
// tip", and this function's answer gets deleted rather than merely reported.
// So it refuses, and the refusal has to carry the chain's own problem --
// otherwise an operator reads "not the newest" and goes looking for a
// divergence when what they have is a fork.
func TestPlanCheckpointRecoveryRefusesAnUnresolvableChain(t *testing.T) {
	forked := []libvirtsync.Checkpoint{
		{Name: cptName(genA, 1)},
		{Name: cptName(genA, 2), Parent: cptName(genA, 1)},
		// An interrupted bitmap adoption: prefixed, parentless, nobody's
		// parent. Two ends.
		{Name: cptName(genB, 7)},
	}
	got, err := planCheckpointRecovery(cptName(genA, 2), cptName(genA, 1), forked)
	if err == nil {
		t.Fatalf("got %q for deletion from a chain with two ends, where which checkpoint is newest has no answer", got)
	}
	if !strings.Contains(err.Error(), "2 ends") {
		t.Errorf("the refusal does not say the chain itself cannot be resolved, so it reads as a divergence rather than a fork: %v", err)
	}
	if !strings.Contains(err.Error(), cptName(genA, 2)) {
		t.Errorf("the refusal does not name the pending checkpoint: %v", err)
	}
}

// WHY the chain check failed, which the bare message left the reader to work
// out from two strings differing in the middle.
func TestCheckpointChainMismatchDistinguishesItsCauses(t *testing.T) {
	const domain = "web01"

	t.Run("different generations name the rebuild", func(t *testing.T) {
		err := checkpointChainMismatch(domain, cptName(genA, 7), cptName(genB, 1))
		if err == nil {
			t.Fatal("no error")
		}
		msg := err.Error()
		for _, want := range []struct{ what, s string }{
			{"the domain", domain},
			{"what the replica was last written by", cptName(genA, 7)},
			{"its generation", genA},
			{"what this run would build on", cptName(genB, 1)},
			{"that generation", genB},
			{"that the chain was rebuilt", "rebuilt"},
			{"that no bitmap can describe the gap", "no bitmap on the source"},
			{"the remedy", "-reinit"},
		} {
			if !strings.Contains(msg, want.s) {
				t.Errorf("the refusal does not state %s (%q): %s", want.what, want.s, msg)
			}
		}
	})

	// The older failure, and a different remedy: one chain, the target simply
	// behind. Naming it as a rebuild would send an operator to a full copy
	// they may not need.
	t.Run("the same generation at a different position says so", func(t *testing.T) {
		err := checkpointChainMismatch(domain, cptName(genA, 1), cptName(genA, 7))
		if err == nil {
			t.Fatal("no error")
		}
		msg := err.Error()
		if !strings.Contains(msg, "same generation") {
			t.Errorf("a within-chain mismatch is not identified as one: %s", msg)
		}
		if strings.Contains(msg, "rebuilt") {
			t.Errorf("a within-chain mismatch is reported as a rebuilt chain, which sends an operator to a full copy they may not need: %s", msg)
		}
		if !strings.Contains(msg, genA) {
			t.Errorf("the generation both names share is not stated: %s", msg)
		}
	})

	// Nothing to compare: say so rather than assert a cause.
	t.Run("an unreadable generation is not guessed at", func(t *testing.T) {
		err := checkpointChainMismatch(domain, "vmsync-cpt-000007", cptName(genA, 1))
		if err == nil {
			t.Fatal("no error")
		}
		msg := err.Error()
		if !strings.Contains(msg, "no readable generation") {
			t.Errorf("an unreadable name is not reported as one: %s", msg)
		}
		if strings.Contains(msg, "same generation") || strings.Contains(msg, "rebuilt") {
			t.Errorf("a cause was claimed for names that cannot be compared: %s", msg)
		}
	})
}

// WHAT A REWIND MAY OVERWRITE. The rewind can empty the chain and turn an
// incremental into a full copy, after the two guards that ask `parent == ""`
// have already been evaluated against a parent it then discards. This is the
// decision that stands between that and an overwritten replica.
func TestRewoundOverwriteRefusal(t *testing.T) {
	const (
		domain = "web01"
		orphan = "vmsync-cpt-" + genA + "-000002"
		accept = "vmsync-cpt-" + genA + "-000001"
		onDisk = "/data/replicas/web01-disk0.qcow2"
	)

	// THE REPAIR CASE, and the one a disk-existence check alone would get
	// wrong. The run that died is the reason the chain had a pending
	// checkpoint at all, and it armed the marker before writing a byte -- so
	// the disks are a half-written base, not a replica, and copying over them
	// is the only way forward. Refusing here would make the rewind unable to
	// finish what it exists to finish.
	t.Run("a replica already marked incomplete may be overwritten", func(t *testing.T) {
		if err := rewoundOverwriteRefusal(domain, orphan, accept,
			"verb=full-sync,at=1758441600,host=hv-a", onDisk); err != nil {
			t.Errorf("refused to copy over a replica whose own metadata says a full copy into it did not finish, which is the state the rewind exists to repair: %v", err)
		}
	})

	// Nothing there to overwrite: a first-ever copy, or one after a -reinit
	// with delete.
	t.Run("absent disks may be written", func(t *testing.T) {
		if err := rewoundOverwriteRefusal(domain, orphan, accept, "", ""); err != nil {
			t.Errorf("refused a full copy with no target disk present: %v", err)
		}
	})

	// THE DESTRUCTIVE CASE. A complete replica, and nothing asked for it to
	// be replaced -- which is precisely what the full-sync preflight refuses
	// when the run was parentless from the start. The rewind must not be a
	// way around that gate.
	t.Run("a complete replica is not overwritten unasked", func(t *testing.T) {
		err := rewoundOverwriteRefusal(domain, orphan, accept, "", onDisk)
		if err == nil {
			t.Fatal("a rewind-induced full copy was allowed to overwrite a complete replica; the same run would be refused by the preflight if it had been parentless from the start")
		}
		msg := err.Error()
		for _, want := range []struct{ what, s string }{
			{"the checkpoint whose removal caused this", orphan},
			{"the domain", domain},
			{"the disk that would be overwritten", onDisk},
			{"what the replica is complete as of", accept},
			{"the verb that does ask", "-reinit"},
			{"that the copy would then be undoable", "undoable"},
		} {
			if !strings.Contains(msg, want.s) {
				t.Errorf("the refusal does not state %s (%q): %s", want.what, want.s, msg)
			}
		}
	})

	// An unreadable marker is still a marker: the field's PRESENCE is the
	// finding everywhere else in vmsync, and a value this build cannot parse
	// still means a copy did not finish.
	t.Run("an unparsable marker still counts as set", func(t *testing.T) {
		if err := rewoundOverwriteRefusal(domain, orphan, accept, "written by something newer", onDisk); err != nil {
			t.Errorf("a marker that could not be parsed was treated as absent: %v", err)
		}
	})

	// Whitespace is not a marker. SetDomainMetadataFields writes the field
	// empty to clear it, and XML round-trips can leave a blank.
	t.Run("a blank marker does not authorise an overwrite", func(t *testing.T) {
		if err := rewoundOverwriteRefusal(domain, orphan, accept, "   \n", onDisk); err == nil {
			t.Error("a whitespace-only marker was read as a copy that did not finish, which would let a complete replica be overwritten unasked")
		}
	})
}
