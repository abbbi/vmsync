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
	"strings"
	"testing"
)

// THE WIRING of the unproven-absence warning, not its wording.
//
// unprovenAbsenceReport is pure and tested in unprovenabsence_test.go. What no
// test there can reach is the branch it hangs off: run() needs two libvirt
// connections and an SSH client to enter, which is the same argument
// bitmapwiring_test.go and reclaimwiring_test.go make for their own decisions.
//
// The branch this guards is the empty-set arm of the post-drop bitmap switch,
// and the mutation it exists to catch is the one with no syntax to it at all:
// an arm with nothing in its body. `case len(leftover) == 0:` followed
// immediately by `default:` compiles, vets clean, and passes every test in
// unprovenabsence_test.go, because those test a function and this is about
// whether anything calls it.

// wiredAbsenceCase finds the switch arm the warning belongs to, identified by
// what it CONTAINS rather than by its position, so moving the arm does not
// break this and emptying it does.
func wiredAbsenceCase(t *testing.T, body *ast.BlockStmt) (clause *ast.CaseClause, total int) {
	t.Helper()
	ast.Inspect(body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && isFuncCallNamed(call, "unprovenAbsenceReport") {
			total++
		}
		cc, ok := n.(*ast.CaseClause)
		if !ok {
			return true
		}
		found := false
		ast.Inspect(cc, func(inner ast.Node) bool {
			if call, ok := inner.(*ast.CallExpr); ok && isFuncCallNamed(call, "unprovenAbsenceReport") {
				found = true
			}
			return true
		})
		if found && clause == nil {
			clause = cc
		}
		return true
	})
	return clause, total
}

// caseConditionText renders a switch arm's single condition, spelling out the
// arguments the shared exprText drops.
//
// `len(leftover) == 0` and `len(after) == 0` both render as "len(...) == 0"
// through exprText, and telling those two apart is the entire point of the
// assertion below: the second is the re-read after a clearing, where an empty
// answer means the clearing worked and is not a doubt at all.
func caseConditionText(clause *ast.CaseClause) string {
	if len(clause.List) != 1 {
		return "a condition this test does not recognise"
	}
	bin, ok := clause.List[0].(*ast.BinaryExpr)
	if !ok {
		return exprText(clause.List[0])
	}
	return conditionSide(bin.X) + " " + bin.Op.String() + " " + conditionSide(bin.Y)
}

func conditionSide(e ast.Expr) string {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return exprText(e)
	}
	args := make([]string, 0, len(call.Args))
	for _, a := range call.Args {
		args = append(args, exprText(a))
	}
	return exprText(call.Fun) + "(" + strings.Join(args, ", ") + ")"
}

// AN EMPTY READ MUST SAY SO, and it must say so from the arm that handles an
// empty read.
//
// The post-drop switch has three arms. The error arm already warns that the
// check could not run. The default arm clears what it found. The arm in the
// middle is the one that found nothing -- and on a running source "found
// nothing" is not "there is nothing", because the read returns the qcow2
// bitmap directory as the file holds it and qemu has not written this qemu's
// bitmaps into the file yet.
//
// Pinned to that arm specifically. Hung off the default arm instead the
// warning still compiles and still produces its message, but it would then
// fire only when bitmaps WERE found -- telling an operator that an absence is
// unproven at the one moment the presence is certain.
func TestTheEmptyBitmapReadIsReportedAsUnproven(t *testing.T) {
	const wantCond = "len(leftover) == 0"

	body := funcBodyNamed(reclaimWiringFile(t, runSourceFile), runFuncName)
	if body == nil {
		t.Fatalf("no func %s() with a body in %s", runFuncName, runSourceFile)
	}

	clause, total := wiredAbsenceCase(t, body)
	if clause == nil {
		t.Fatalf("no switch arm inside %s() calls unprovenAbsenceReport. The arm that handles an empty post-drop bitmap read then draws no conclusion out loud, and an orphan made during the running qemu's life -- the one a failed run leaves behind -- is reported as nothing at all", runFuncName)
	}
	if total != 1 {
		t.Errorf("%s() calls unprovenAbsenceReport %d time(s), want exactly 1: there is one arm where the read came back empty, and this test can only vouch for the call it checked", runFuncName, total)
	}
	if got := caseConditionText(clause); got != wantCond {
		t.Errorf("the warning hangs off the arm `case %s:`, want `case %s:`.\n\nOn any other arm the message is false: the error arm did not get an answer to doubt, the default arm found bitmaps and is clearing them, and the re-read after a clearing reports whether the clearing took. Only the empty post-drop read is an absence that cannot be proven.", got, wantCond)
	}
}

// AND IT MUST BE A WARNING THAT IS ACTUALLY EMITTED.
//
// Two mutations, both one line and both leaving every other assertion here
// green. Dropping the trace call keeps the function call, its arguments and
// its position in the tree, so a walk that counts calls still finds it --
// run() would compute the message and throw it away. Downgrading Warning to
// Info keeps it emitted but puts it below the level an operator reads, and
// WarningCount is what the harness and the bench scenarios count, so the
// difference is also the difference between a bench row that records this and
// one that does not.
func TestTheUnprovenAbsenceIsEmittedAsAWarning(t *testing.T) {
	body := funcBodyNamed(reclaimWiringFile(t, runSourceFile), runFuncName)
	if body == nil {
		t.Fatalf("no func %s() with a body in %s", runFuncName, runSourceFile)
	}
	clause, _ := wiredAbsenceCase(t, body)
	if clause == nil {
		t.Fatalf("no switch arm inside %s() calls unprovenAbsenceReport", runFuncName)
	}

	// The `if` whose INIT builds the report: the message is only emitted when
	// it is non-empty, and that guard is what keeps a shut-down source silent.
	var guard *ast.IfStmt
	ast.Inspect(clause, func(n ast.Node) bool {
		stmt, ok := n.(*ast.IfStmt)
		if !ok || stmt.Init == nil {
			return true
		}
		ast.Inspect(stmt.Init, func(inner ast.Node) bool {
			if call, ok := inner.(*ast.CallExpr); ok && isFuncCallNamed(call, "unprovenAbsenceReport") {
				guard = stmt
			}
			return true
		})
		return true
	})
	if guard == nil {
		t.Fatalf("unprovenAbsenceReport is called in %s() but not as the init of an `if`. Its empty return is how a shut-down source stays silent, so a caller that does not test the result either warns about a source whose read WAS conclusive or drops the message entirely", runFuncName)
	}

	warnings, infos := 0, 0
	ast.Inspect(guard.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch {
		case isMethodCallNamed(call, "Warning"):
			warnings++
		case isMethodCallNamed(call, "Info"):
			infos++
		}
		return true
	})
	if warnings != 1 {
		t.Errorf("the guarded block emits %d Warning(s), want 1. A message that is built and not logged is the same as no message, and this one is the only thing that tells an operator the bitmap check could not see a bitmap made during the running qemu's life", warnings)
	}
	if infos != 0 {
		t.Errorf("the guarded block emits %d Info call(s). An unproven absence is a doubt the operator has to act on and it is counted by WarningCount, which both the harness and the bench read; Info is below where either looks", infos)
	}
}

// THE STATE IT JUDGES HAS TO BE THE SOURCE'S OWN, READ HERE.
//
// unprovenAbsenceReport returns "" for a shut-down source and a message for
// everything else, so both of its decision arguments are inversion points that
// a pure test cannot see from inside run(). Handed a literal -- `true`, or
// `nil` for the error -- the function is still correct and still tested, and
// the branch reports a conclusive read as doubtful or an unreadable state as
// conclusive. The arguments are therefore pinned verbatim.
func TestTheUnprovenAbsenceJudgesTheSourcesRealState(t *testing.T) {
	want := []string{"cfg.SourceDomain", "srcActive", "activeErr", "leftoverProven"}

	body := funcBodyNamed(reclaimWiringFile(t, runSourceFile), runFuncName)
	if body == nil {
		t.Fatalf("no func %s() with a body in %s", runFuncName, runSourceFile)
	}
	clause, _ := wiredAbsenceCase(t, body)
	if clause == nil {
		t.Fatalf("no switch arm inside %s() calls unprovenAbsenceReport", runFuncName)
	}

	var got []string
	ast.Inspect(clause, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isFuncCallNamed(call, "unprovenAbsenceReport") {
			return true
		}
		got = got[:0]
		for _, a := range call.Args {
			got = append(got, exprText(a))
		}
		return true
	})
	if strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Errorf("unprovenAbsenceReport is called with (%s), want (%s).\n\nA literal in either decision argument makes the warning unconditional or unreachable while leaving the function itself correct: `true` warns even about a shut-down source whose read was complete, and `nil` reports an unreadable state as a conclusive one.",
			strings.Join(got, ", "), strings.Join(want, ", "))
	}

	// The state must be read in this arm rather than borrowed, because the
	// default arm has its own srcActive from its own DomainActive call and the
	// two arms are mutually exclusive.
	reads := 0
	ast.Inspect(clause, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && isMethodCallNamed(call, "DomainActive") {
			reads++
		}
		return true
	})
	if reads != 1 {
		t.Errorf("the empty-read arm calls DomainActive %d time(s), want 1: it has to establish the source's state itself, since the only other reading of it lives in a sibling arm that cannot have run", reads)
	}
}
