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
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// THE WIRING of the two reclaim decisions, not the decisions themselves.
//
// Both are pure and both are tested where they live: leftoversToReclaim and
// sweepDisplacedSets in leftovers_test.go and reclaimsweep_test.go,
// failover.KeepExistingReplicaIncomplete in pkg/failover. What no test can
// reach is whether their callers hand them the right thing, because both
// callers need two libvirt connections to enter. refusalwiring_test.go makes
// the same argument for incompleteReplicaRefusal and names this file as its
// sibling; the file did not exist until the mutations below needed it.
//
// Each assertion here exists because a mutation walked through everything
// else. Both of those mutations restore behaviour this tree shipped, which is
// what makes them the plausible future edit rather than vandalism: somebody
// reads the sweep's marker parameter, decides the read belongs beside the call
// that uses it, and puts it back.
func reclaimWiringFile(t *testing.T, path string) *ast.File {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("this test guards wiring nothing else can guard, so it must not pass by failing to look: parse %s: %v", path, err)
	}
	return file
}

func funcBodyNamed(file *ast.File, name string) *ast.BlockStmt {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == name && fn.Body != nil {
			return fn.Body
		}
	}
	return nil
}

// markerArgIdent is the variable the displaced-set sweep must be handed as its
// replica_incomplete argument, and it is deliberately NOT a fresh read.
//
// runorder_test.go pins WHERE the read happens -- ahead of both armings. That
// is half the property: a read in the right place proves nothing if the value
// the sweep decides on comes from somewhere else. Passing a late expression
// instead keeps the early read in place, satisfies every ordering assertion,
// and restores the defect in full, because what the sweep refuses on is its
// argument and not the line above it.
const markerArgIdent = "replicaIncompleteAtStart"

// sweepMarkerArgPos is which of sweepDisplacedSets' parameters carries it. Its
// signature is (ctx, cfg, runner, replicaIncomplete, markerErr, paths, now),
// so the marker is the fourth.
const sweepMarkerArgPos = 3

func TestTheSweepDecidesOnTheMarkerReadBeforeThisRunArmedIt(t *testing.T) {
	body := funcBodyNamed(reclaimWiringFile(t, runSourceFile), runFuncName)
	if body == nil {
		t.Fatalf("no func %s() with a body in %s", runFuncName, runSourceFile)
	}

	found := 0
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isFuncCallNamed(call, "sweepDisplacedSets") {
			return true
		}
		found++
		if len(call.Args) <= sweepMarkerArgPos {
			t.Errorf("sweepDisplacedSets is called with %d arguments, so argument %d -- the replica_incomplete value it refuses on -- cannot be checked. If the signature changed, move sweepMarkerArgPos with it rather than deleting this test: the value it guards is the one that decides whether a full-size aside set is deleted",
				len(call.Args), sweepMarkerArgPos+1)
			return true
		}
		arg := call.Args[sweepMarkerArgPos]
		id, ok := arg.(*ast.Ident)
		if !ok || id.Name != markerArgIdent {
			t.Errorf("sweepDisplacedSets' replica_incomplete argument is %s, not the identifier %q read at the top of the run lock.\n\nThe field means a PREVIOUS full copy did not finish. Any expression evaluated here instead -- a fresh ReadDomainMetadataFields, a re-parse of the target XML -- is read after this run armed the field for its own copy, so the sweep stands down on every rebuild and the run that renames a full-size set aside becomes the only run that can never reclaim an older one. That is the ENOSPC sweepDisplacedSets sits before the copy to prevent.",
				exprText(arg), markerArgIdent)
		}
		return true
	})
	if found != 1 {
		t.Errorf("found %d calls to sweepDisplacedSets inside %s(), want exactly 1: a second call would sweep on a second answer, and this test can only vouch for the one it checked", found, runFuncName)
	}
}

// The other half: the rule that decides whether an interrupted rebuild's
// pointer to the last complete copy survives.
//
// KeepExistingReplicaIncomplete used to take the stamp this run was about to
// arm and prefer the incoming record whenever it had one. That was wrong in
// the sequence that matters -- a rebuild following an interrupted one sets the
// HALF-WRITTEN disks aside, so its stamp names garbage -- and the pure test
// for it in pkg/failover now pins the corrected rule. Conjoining the old
// condition back on at the call site reinstates the defect exactly, compiles,
// and leaves that pure test green, because the function still answers
// correctly and the caller ignores half of the answer.
func TestTheArmKeepsAnExistingStampWithoutConsultingItsOwn(t *testing.T) {
	body := funcBodyNamed(reclaimWiringFile(t, runSourceFile), "armReplicaIncomplete")
	if body == nil {
		t.Fatalf("no func armReplicaIncomplete() with a body in %s", runSourceFile)
	}

	found := 0
	ast.Inspect(body, func(n ast.Node) bool {
		stmt, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		call, ok := stmt.Cond.(*ast.CallExpr)
		if !ok || !isMethodCallNamed(call, "KeepExistingReplicaIncomplete") {
			// Not this if, or the call is buried in a larger condition --
			// which is the mutation. Checked below by the count.
			return true
		}
		found++
		if len(call.Args) != 1 {
			t.Errorf("KeepExistingReplicaIncomplete is called with %d arguments, want 1. The rule rests on the record already on the target and nothing else; a second argument is how the aside stamp this run is about to write got back into a decision it must not influence", len(call.Args))
		}
		return true
	})
	if found != 1 {
		t.Errorf("armReplicaIncomplete has %d `if` statements whose whole condition is a call to KeepExistingReplicaIncomplete, want exactly 1.\n\nEither the call is gone -- in which case an interrupted rebuild's record is overwritten by the next one and the only thing on the host naming the last complete copy is lost -- or the call is now one term of a larger condition. `&& asideStamp == \"\"` is the specific edit this guards: it restores the rule this tree had, where a second interrupted rebuild aimed the pointer at the half-written disks it had just set aside.", found)
	}
}
