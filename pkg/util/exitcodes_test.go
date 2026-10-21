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

package util

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// exitStatuses is every status these binaries can leave behind, including
// the ones defined elsewhere and the bare literals, because the collision
// this guards against does not care where a number was written.
var exitStatuses = map[int]string{
	0:   "success",
	1:   "the run failed",
	2:   "bad invocation, nothing attempted",
	3:   "RemoteLockExitNoFlock",
	4:   "RemoteLockExitNoDir",
	5:   "RemoteLockExitNoCreate",
	6:   "RemoteLockExitBusy",
	7:   "RemoteLockExitNoSameFile",
	75:  "ExitBusy",
	124: "ExitSupervisedTimeout",
	125: "ExitSuperviseFailed",
	137: "test-only self-kill (-test=die-writing-base)",
}

// definedExitConstants returns every integer constant exitcodes.go declares, by
// name, read from the file itself.
//
// It used to be a list written by hand next to the loop below, and that is
// exactly how RemoteLockExitNoSameFile = 7 was added without being registered
// anywhere: the loop iterated the hand-written list, so a constant missing from
// THAT list was never checked at all. The guard whose entire purpose is to stop
// the next number colliding with an existing meaning reported health about a
// number nobody had recorded, and the suite stayed green.
//
// Reading the declarations inverts that. The only way to add a status without
// documenting it is now to fail this test, which is what it was for.
func definedExitConstants(t *testing.T) map[string]int {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "exitcodes.go", nil, 0)
	if err != nil {
		t.Fatalf("parse exitcodes.go: %v", err)
	}
	out := map[string]int{}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.INT {
					continue
				}
				n, err := strconv.Atoi(lit.Value)
				if err != nil {
					continue
				}
				out[name.Name] = n
			}
		}
	}
	// ANTI-VACUITY. A parse that silently returned nothing, or a rename of the
	// file, would leave the loop below with no work and report success -- the
	// same shape of blindness this function exists to remove.
	if len(out) < 4 {
		t.Fatalf("only %d integer constants found in exitcodes.go; this guard has stopped reading the file it guards", len(out))
	}
	return out
}

// TestExitStatusesDoNotCollide is the reason these were collected into one
// file.
//
// vmsync-bridge-helper is the binary that makes it matter: it exits with the
// run-lock codes in one mode and the supervision codes in another, so its
// statuses share a single space, and a new mode picking a number already
// spoken for would be read as the wrong thing by whoever is parsing it --
// vmsync, vmsync-agent, or contrib/runner's validExitCodes list.
//
// TWO NAMED STATUSES SHARING A VALUE IS CAUGHT BY THE COMPILER, not by this
// test: constant keys that collide are duplicate keys in the literal below,
// and Go refuses to build it. That is the better guard and it is free, so
// the literal is written this way on purpose.
//
// What this test adds is the other half -- that every named status is also
// IN the map above, spelled the same. A status defined and never documented
// there is invisible to the next person choosing a number, which is how the
// collision the compiler would catch gets written in the first place.
//
// MUTATION CAUGHT: renumbering a status, or adding one without listing it.
func TestExitStatusesDoNotCollide(t *testing.T) {
	for name, value := range definedExitConstants(t) {
		documented, known := exitStatuses[value]
		if !known {
			t.Errorf("%s = %d is not in the map in exitcodes.go; the next person choosing a number will not see it taken", name, value)
			continue
		}
		if documented != name {
			t.Errorf("%d is %s in code and %q in the map in exitcodes.go", value, name, documented)
		}
	}
}

// TestExitBusyStaysSeventyFive pins the one value something outside this
// repository is configured against.
//
// contrib/runner/vmsync-parallel.sh passes "0;75" as its validExitCodes, so
// changing this number turns every legitimate scheduling overlap in the
// estate into a logged failure that can reach SendAlert -- silently, because
// nothing in Go would fail.
func TestExitBusyStaysSeventyFive(t *testing.T) {
	if ExitBusy != 75 {
		t.Errorf("ExitBusy = %d, want 75: contrib/runner/vmsync-parallel.sh allowlists \"0;75\", and callers that treat any non-zero code as an error were told to accept 75", ExitBusy)
	}
}

// TestSupervisionStatusesFollowTimeout keeps the borrowed convention intact.
// An operator reading 124 off a wedged target command is reading GNU
// timeout(1)'s meaning; picking different numbers would make that reading
// wrong without making anything here fail.
func TestSupervisionStatusesFollowTimeout(t *testing.T) {
	if ExitSupervisedTimeout != 124 {
		t.Errorf("ExitSupervisedTimeout = %d, want 124 (GNU timeout(1)'s \"command timed out\")", ExitSupervisedTimeout)
	}
	if ExitSuperviseFailed != 125 {
		t.Errorf("ExitSuperviseFailed = %d, want 125 (GNU timeout(1)'s \"the supervisor itself failed\")", ExitSuperviseFailed)
	}
}
