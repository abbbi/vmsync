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

import "testing"

// IsManagedCheckpointName had no test anywhere in the tree, and four callers
// ask it: ListManagedCheckpoints deciding what to report, the inversion's
// offline cleanup deciding which dirty BITMAPS it may delete off a disk image,
// leftoverCheckpointBitmaps producing the stale-bitmap list, and
// CreateCheckpoint's self-heal gate deciding whether a colliding bitmap is
// vmsync's to remove.
//
// Two of those four point rm-style operations at qcow2 bitmaps, which is why a
// three-line predicate earns a test: a false positive deletes somebody else's
// bitmap, and a false negative leaves one whose checkpoint has gone, which
// blocks every later sync for that pair.
func TestIsManagedCheckpointName(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
		why  string
	}{
		{checkpointNameFor(testToken, 1), true, "the shape vmsync mints"},
		{CheckpointPrefix + "-000001", true, "prefixed, so it is vmsync's to clean up even though its name predates the generation token -- ownership and readability are different questions"},
		{CheckpointPrefix + "-", true, "the prefix and nothing after it is still the prefix; there is no shape test here, deliberately"},
		{CheckpointPrefix, false, "the constant alone has no separator, so a domain checkpoint literally named vmsync-cpt is not ours"},
		{"vmsync-cpt", false, "same, spelled out"},
		{"", false, "an empty name belongs to nobody"},
		{"backup-000001", false, "somebody else's bitmap on the same image"},
		{"vmsync-other-000001", false, "a vmsync-ish prefix is not the prefix"},
		{"Vmsync-cpt-000001", false, "the test is byte-exact, and a name differing in case is a different libvirt object"},
		{" " + CheckpointPrefix + "-000001", false, "a leading space makes a different name, and HasPrefix is not asked to trim"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsManagedCheckpointName(tc.name); got != tc.want {
				t.Errorf("IsManagedCheckpointName(%q) = %v, want %v: %s", tc.name, got, tc.want, tc.why)
			}
		})
	}
}

// The verify-window name must stay invisible to the chain logic, and nothing
// asserted it.
//
// VerifyWindowCheckpointName is deliberately NOT prefixed, and its own doc
// comment explains the consequence it is avoiding: NextCheckpointName hard
// fails on a tip that does not end in a numeric suffix, so a leaked
// verify-window checkpoint must be unable to REACH that path at all rather
// than merely be cleaned up in time.
//
// The generation token raises the stakes. The stale-bitmap report classifies
// everything the prefix admits, so a name that leaked into the managed set
// would be reported as an unattributable leftover on every sync, for ever --
// a permanent false positive in the one report whose value is that it is
// usually silent.
func TestTheVerifyWindowCheckpointIsInvisibleToTheChain(t *testing.T) {
	if IsManagedCheckpointName(VerifyWindowCheckpointName) {
		t.Fatalf("%q is managed, so ListManagedCheckpoints would return it, NextCheckpointName would refuse the chain it is the tip of, -reinit would try to delete it, and the stale-bitmap report would name it on every sync",
			VerifyWindowCheckpointName)
	}
	// The mechanism, pinned so a rename cannot quietly restore the hazard:
	// invisibility comes from the name not starting with the prefix.
	if len(VerifyWindowCheckpointName) >= len(CheckpointPrefix)+1 &&
		VerifyWindowCheckpointName[:len(CheckpointPrefix)+1] == CheckpointPrefix+"-" {
		t.Errorf("%q begins with %q, so it is only invisible by accident", VerifyWindowCheckpointName, CheckpointPrefix+"-")
	}
	// And it must not be mistaken for a chain tip even if it did get through.
	if _, ok := GenerationToken(VerifyWindowCheckpointName); ok {
		t.Errorf("%q parses as a tokenised checkpoint name", VerifyWindowCheckpointName)
	}
}
