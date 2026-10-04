/*
	Copyright (C) 2026  Orsiris de Jong <ozy@netpower.fr>
	Copyright (C) 2026  Michael Ablassmeier <abi@grinser.de>

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
	"slices"
	"testing"
)

// TestCliModesEveryModeIsReachable is the regression guard for the defect
// that made -release-promotion a no-op: it was listed as a mode, so the
// mutual-exclusion check accepted it and main's dispatch claimed it, but no
// branch ran it and the process exited 0 having changed nothing.
//
// Every mode must therefore either carry a handler or be named in
// modesHandledInline. A mode with neither is unreachable, and unreachable
// here does not mean "fails loudly" -- it means "reports success".
func TestCliModesEveryModeIsReachable(t *testing.T) {
	for _, m := range cliModes(syncConfig{}) {
		if m.run == nil && !slices.Contains(modesHandledInline, m.name) {
			t.Errorf("mode %s has no handler and is not listed in modesHandledInline: "+
				"it would exit 0 without doing anything. Give it a handler in cliModes, "+
				"or a block of its own in main and an entry in modesHandledInline.", m.name)
		}
		if m.run != nil && slices.Contains(modesHandledInline, m.name) {
			t.Errorf("mode %s has a handler AND is listed in modesHandledInline: "+
				"the handler wins, so the block in main is dead code -- remove one of them.", m.name)
		}
	}
}

// TestModesHandledInlineAreAllListed catches the mirror image: a name left in
// modesHandledInline after its mode was given a handler or removed outright,
// which would quietly excuse a future mode of the same name from the check
// above.
func TestModesHandledInlineAreAllListed(t *testing.T) {
	modes := cliModes(syncConfig{})
	for _, name := range modesHandledInline {
		if !slices.ContainsFunc(modes, func(m cliMode) bool { return m.name == name }) {
			t.Errorf("modesHandledInline names %s, which is not a mode in cliModes: remove the stale entry", name)
		}
	}
}

// TestCliModesNamesAreUniqueAndFlagShaped keeps the conflict message
// readable: it joins these names verbatim, so a blank or duplicated one
// turns "-promote and -invert cannot be combined" into nonsense.
func TestCliModesNamesAreUniqueAndFlagShaped(t *testing.T) {
	seen := map[string]bool{}
	for _, m := range cliModes(syncConfig{}) {
		if len(m.name) < 2 || m.name[0] != '-' {
			t.Errorf("mode name %q should be the flag that selects it, leading dash included", m.name)
		}
		if seen[m.name] {
			t.Errorf("duplicate mode name %q", m.name)
		}
		seen[m.name] = true
	}
}

// TestCliModesOffByDefault asserts the zero config selects no mode at all.
// An `on` expression that is true for the zero value would make an ordinary
// sync take that mode's exit path instead of syncing -- and with two such
// entries, refuse as "conflicting modes" with no mode flag passed.
func TestCliModesOffByDefault(t *testing.T) {
	for _, m := range cliModes(syncConfig{}) {
		if m.on {
			t.Errorf("mode %s reports itself selected for a zero syncConfig: "+
				"an ordinary sync would be diverted into it", m.name)
		}
	}
}

// TestCliModesSelectedByTheirOwnFlag pins each entry's `on` expression to the
// field whose flag selects it. A copy-paste slip there -- two entries reading
// the same field -- shows up as "conflicting modes" for a single flag, or as
// one mode silently running another.
func TestCliModesSelectedByTheirOwnFlag(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  syncConfig
	}{
		{"-promote", syncConfig{Promote: true}},
		{"-invert", syncConfig{Invert: true}},
		{"-shutdown-domain", syncConfig{ShutdownDomain: true}},
		{"-fence-domain", syncConfig{FenceDomain: true}},
		{"-read-fence", syncConfig{ReadFence: true}},
		{"-update-role", syncConfig{UpdateRole: "target"}},
		{"-release-promotion", syncConfig{ReleasePromotion: true}},
		{"-break-target-lock", syncConfig{BreakTargetLock: true}},
		{"-list-restore-points", syncConfig{ListRestorePoints: true}},
		{"-clone-restore-point", syncConfig{CloneRestorePoint: "tag"}},
		{"-restore-restore-point", syncConfig{RestoreRestorePoint: "tag"}},
		{"-explain-domain", syncConfig{ExplainDomain: "vm"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var selected []string
			for _, m := range cliModes(tc.cfg) {
				if m.on {
					selected = append(selected, m.name)
				}
			}
			if len(selected) != 1 || selected[0] != tc.name {
				t.Errorf("config selecting %s selected %v instead", tc.name, selected)
			}
		})
	}
}
