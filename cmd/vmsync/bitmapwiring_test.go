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
	"testing"
)

// THE WIRING of the dead-bitmap report, not its wording.
//
// attributeDeadBitmaps is pure and tested in deadbitmaps_test.go. What no test
// there can reach is what run() hands it, and run() needs two libvirt
// connections and an SSH client to enter -- the same argument
// refusalwiring_test.go and reclaimwiring_test.go make for their own
// decisions, and the same reason this file exists.
//
// Both assertions below exist because a mutation walked through everything
// else, and both mutations are plausible edits rather than vandalism.

// THE SET THE REPORT DESCRIBES must be the dead one, and nothing else.
//
// orphanBitmaps subtracts the names libvirt holds checkpoints for from the
// names the images carry. Handed the unsubtracted set instead -- which is one
// deleted call away, since both are map[string][]string and it compiles -- the
// report names the LIVE bitmap of the current chain as a dead generation's
// reclaimable leftover. That is the error plan §4.1 is built to avoid: the
// checkpoint list decides death, and the token only labels the dead. An
// operator acting on that report deletes the baseline the next incremental
// needs.
func TestTheDeadBitmapReportDescribesOnlyTheDeadSet(t *testing.T) {
	body := funcBodyNamed(reclaimWiringFile(t, runSourceFile), runFuncName)
	if body == nil {
		t.Fatalf("no func %s() with a body in %s", runFuncName, runSourceFile)
	}

	found := 0
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isFuncCallNamed(call, "attributeDeadBitmaps") {
			return true
		}
		found++
		if len(call.Args) == 0 {
			t.Error("attributeDeadBitmaps is called with no arguments")
			return true
		}
		inner, ok := call.Args[0].(*ast.CallExpr)
		if !ok || !isFuncCallNamed(inner, "orphanBitmaps") {
			t.Errorf("attributeDeadBitmaps' first argument is %s, want a call to orphanBitmaps(...).\n\nThe report must describe the bitmaps libvirt holds NO checkpoint for. Given the whole enumerated set instead, it reports the live chain's own bitmap as a dead generation's leftover -- and the operator who acts on that deletes the baseline the next incremental diffs against.",
				exprText(call.Args[0]))
		}
		return true
	})
	if found != 1 {
		t.Errorf("found %d calls to attributeDeadBitmaps inside %s(), want exactly 1: the report runs once per sync, and this test can only vouch for the call it checked", found, runFuncName)
	}
}

// THE LEFTOVER REPORT MUST NOT REFUSE, and this is the assertion that keeps it
// that way.
//
// It was a refusal until generation tokens made the collision it explained
// impossible, and retiring it was deliberate: a leftover bitmap no longer
// blocks a rebuild, so refusing one is fail-closed with nothing behind it, on
// the single verb an operator reaches for when a pair is already broken.
//
// The edit that undoes it is two lines -- `if r := leftoverBitmapReport(...);
// r != "" { return fmt.Errorf(...) }` -- and it compiles, passes every test in
// bitmaps_test.go (the message is unchanged, only its severity), and silently
// restores the refusal. So the shape is pinned instead: exactly one caller of
// leftoverBitmapReport in the package, the logging wrapper, and run() reaches
// it only through that wrapper.
func TestTheLeftoverBitmapReportIsNeverReturnedAsAnError(t *testing.T) {
	file := reclaimWiringFile(t, runSourceFile)

	// run() must not mention the message builder at all: its only legitimate
	// caller is reportLeftoverBitmaps, which lives in bitmaps.go.
	body := funcBodyNamed(file, runFuncName)
	if body == nil {
		t.Fatalf("no func %s() with a body in %s", runFuncName, runSourceFile)
	}
	direct := 0
	wrapped := 0
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch {
		case isFuncCallNamed(call, "leftoverBitmapReport"):
			direct++
		case isFuncCallNamed(call, "reportLeftoverBitmaps"):
			wrapped++
		}
		return true
	})
	if direct != 0 {
		t.Errorf("%s() calls leftoverBitmapReport directly %d time(s). Its value is a message, and the only thing run() did with it for most of this function's life was return it as an error -- which refuses a rebuild over a bitmap that no longer blocks one. Go through reportLeftoverBitmaps", runFuncName, direct)
	}
	// Three branches reach this state for three different reasons: the
	// source's running state was unreadable, it was shut down and the offline
	// removal did not take, or the online clearing reported success and
	// something survived it. All three must say so.
	if wrapped != 3 {
		t.Errorf("%s() calls reportLeftoverBitmaps %d time(s), want 3 -- one per branch of the reinit path that can be left holding bitmaps it could not clear. A branch that says nothing is a leak nobody is told about, which is what the retired refusal was standing in for", runFuncName, wrapped)
	}
}

// THE CHAIN-MISMATCH DIAGNOSTIC HAS TO BE THE ONE run() USES.
//
// checkpointChainMismatch is pure and tested in chaindecision_test.go, and
// reverting the call site to the message it replaced is one line that
// compiles and leaves every test there green -- the function would still be
// correct, and nothing would reach it. The old message gave two
// thirty-three-character names differing in the middle and left the reader to
// find the difference; the point of the new one is that it says whether the
// chain was rebuilt or merely advanced, because those need different remedies.
func TestRunExplainsAChainMismatchRatherThanPrintingTwoNames(t *testing.T) {
	body := funcBodyNamed(reclaimWiringFile(t, runSourceFile), runFuncName)
	if body == nil {
		t.Fatalf("no func %s() with a body in %s", runFuncName, runSourceFile)
	}
	n := 0
	ast.Inspect(body, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok && isFuncCallNamed(call, "checkpointChainMismatch") {
			n++
		}
		return true
	})
	if n != 1 {
		t.Errorf("%s() calls checkpointChainMismatch %d time(s), want 1. There is exactly one place a chain check fails, and an operator reading its message has to be able to tell a rebuilt chain from one merely out of step", runFuncName, n)
	}
}

// AND THE REWIND'S OVERWRITE DECISION HAS TO BE ASKED.
//
// rewoundOverwriteRefusal is pure and tested. The mutations that survived
// before it existed were all condition flips inside run() -- `if false`, `if
// true` -- which an AST walk cannot see, because it reads positions and never
// the conditions a landmark sits under. Extracting the decision is what made
// it testable; this keeps the extraction wired.
func TestTheRewindAsksWhetherItMayOverwrite(t *testing.T) {
	body := funcBodyNamed(reclaimWiringFile(t, runSourceFile), runFuncName)
	if body == nil {
		t.Fatalf("no func %s() with a body in %s", runFuncName, runSourceFile)
	}
	n := 0
	ast.Inspect(body, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok && isFuncCallNamed(call, "rewoundOverwriteRefusal") {
			n++
		}
		return true
	})
	if n != 1 {
		t.Errorf("%s() calls rewoundOverwriteRefusal %d time(s), want 1: without it a rewind that empties the chain copies base images over whatever replica is on the target, and the preflight that would have refused that was evaluated before the rewind happened", runFuncName, n)
	}
}

// THE GUARD'S CONDITION, not just the presence of what it guards.
//
// This is the one check here that reads a condition rather than a position,
// and it exists because `if false && parent == "" && !cfg.Reinit {` survived
// everything else: the calls inside keep their place in the syntax tree, so a
// walk that counts them cannot tell a live branch from a dead one. That blind
// spot is stated in runorder_test.go's own preamble; this closes it for the
// one branch where the whole protection is the branch.
//
// The condition is pinned verbatim. `parent == ""` is what makes this the
// parentless case -- the rewind having emptied the chain -- and
// `!cfg.Reinit` is what keeps it off the one path that armed the marker with
// its own aside stamp, where overwriting that record would turn a recoverable
// interruption into an unrecoverable one.
func TestTheRewindsParentlessGuardIsTheRealCondition(t *testing.T) {
	const want = `parent == "" && !cfg.Reinit`

	body := funcBodyNamed(reclaimWiringFile(t, runSourceFile), runFuncName)
	if body == nil {
		t.Fatalf("no func %s() with a body in %s", runFuncName, runSourceFile)
	}

	// THE INNERMOST enclosing `if`, because the rewind block is itself one:
	// `else if orphan != ""` contains this branch, so a walk that accepted
	// any enclosing `if` would check the wrong condition and report two.
	// Nesting means the inner one starts later, so the latest Pos is it.
	var guard *ast.IfStmt
	ast.Inspect(body, func(node ast.Node) bool {
		stmt, ok := node.(*ast.IfStmt)
		if !ok || stmt.Body == nil {
			return true
		}
		// The branch is identified by what it CONTAINS, so moving it does not
		// break this and emptying it does.
		guards := false
		ast.Inspect(stmt.Body, func(inner ast.Node) bool {
			if call, ok := inner.(*ast.CallExpr); ok && isFuncCallNamed(call, "rewoundOverwriteRefusal") {
				guards = true
			}
			return true
		})
		if guards && (guard == nil || stmt.Pos() > guard.Pos()) {
			guard = stmt
		}
		return true
	})
	if guard == nil {
		t.Fatal("no `if` in run() guards rewoundOverwriteRefusal: without that branch a rewind that empties the chain copies base images over whatever replica is on the target")
	}
	if got := exprText(guard.Cond); got != want {
		t.Errorf("the post-rewind block is guarded by %q, want %q.\n\nA condition that cannot be true makes every assertion about what is inside it vacuous: the calls keep their place in the syntax tree and the branch never runs, so a rewind that empties the chain copies base images over whatever replica is on the target.", got, want)
	}
}
