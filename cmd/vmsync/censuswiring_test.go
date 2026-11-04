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

// THE WIRING of the qemu bitmap census, not its behaviour.
//
// bitmapCensus and every report it produces are pure and tested in
// unsupportedcensus_test.go; clearFailureIsFatal in clearfailure_test.go. What
// no test there can reach is run(), which needs two libvirt connections and an
// SSH client to enter -- the same argument bitmapwiring_test.go and
// absencewiring_test.go make for their own decisions.
//
// Every assertion below is tied to a one-line edit that compiles, leaves every
// behaviour test green, and quietly undoes the feature. One of them survived a
// mutation round with no test in the tree able to see it.

func runBody(t *testing.T) *ast.BlockStmt {
	t.Helper()
	body := funcBodyNamed(reclaimWiringFile(t, runSourceFile), runFuncName)
	if body == nil {
		t.Fatalf("no func %s() with a body in %s", runFuncName, runSourceFile)
	}
	return body
}

func countCalls(body ast.Node, name string) int {
	n := 0
	ast.Inspect(body, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok && isFuncCallNamed(call, name) {
			n++
		}
		return true
	})
	return n
}

// THE CENSUS IS WIRED WHERE IT WAS DECIDED TO BE, AND NOWHERE ELSE.
//
// Two call sites, both inside the reinit path: the read after the chain drop,
// and the re-read after a clearing. The count pins both halves of the owner's
// decision at once. Adding a third wires it somewhere it was deliberately kept
// out of -- the pre-drop read, which would mark a domain tainted on a run that
// may still refuse having changed nothing, or the per-sync dead-generation
// report, which runs on every sync of every pair. Removing one leaves that
// reader blind to a bitmap made during the running qemu's life, which is the
// whole defect.
func TestTheCensusIsWiredToTwoReadersOnly(t *testing.T) {
	body := runBody(t)

	if n := countCalls(body, "bitmapCensusFor"); n != 2 {
		t.Errorf("%s() calls bitmapCensusFor %d time(s), want exactly 2.\n\nTwo is the owner's decision: the post-drop read and the post-clear re-read. A third taints a domain on a path that was deliberately left alone -- the pre-drop read, or the per-sync report that runs for every pair on every sync. A first or second missing leaves that reader unable to see a bitmap made during the running qemu's life.", runFuncName, n)
	}

	// The combiner itself belongs in the closure and nowhere else, so there is
	// one place that decides what a census failure means.
	if n := countCalls(body, "bitmapCensus"); n != 1 {
		t.Errorf("%s() calls bitmapCensus %d time(s), want exactly 1 -- inside the closure. A second call site is a second place deciding what a monitor failure means, and they will drift", runFuncName, n)
	}

	// And the image read keeps its two direct callers, the ones the census was
	// deliberately kept away from, plus the closure's own.
	if n := countCalls(body, "leftoverCheckpointBitmaps"); n != 3 {
		t.Errorf("%s() calls leftoverCheckpointBitmaps %d time(s), want 3: the pre-drop read, the per-sync report, and the closure's own. Fewer means a reader lost the image read entirely -- and neither reader is a superset of the other, so dropping it hides a bitmap left in a file qemu never reloaded", runFuncName, n)
	}
}

// A FAILED IMAGE READ MUST NOT END THE MATTER ONCE QEMU HAS ANSWERED.
//
// The first arm used to be `case err != nil:`, which was right when the image
// was the only reader. Left that way, a run whose qcow2 read failed would warn
// and skip the clearing even though qemu had just listed every bitmap on every
// disk -- the orphan would survive a rebuild that had everything it needed to
// remove it. Reverting the arm is a four-character edit that compiles and
// leaves every behaviour test green.
func TestAFailedImageReadDoesNotOutrankAProvenCensus(t *testing.T) {
	const want = "err != nil && !leftoverProven"

	body := runBody(t)

	// Identified by what it CONTAINS, so moving the arm does not break this.
	var arm *ast.CaseClause
	ast.Inspect(body, func(n ast.Node) bool {
		cc, ok := n.(*ast.CaseClause)
		if !ok || len(cc.List) != 1 {
			return true
		}
		if caseConditionText(cc) == want {
			arm = cc
		}
		return true
	})
	if arm == nil {
		var conditions []string
		ast.Inspect(body, func(n ast.Node) bool {
			if cc, ok := n.(*ast.CaseClause); ok && len(cc.List) == 1 {
				if c := caseConditionText(cc); strings.Contains(c, "leftoverProven") || strings.Contains(c, "err != nil") {
					conditions = append(conditions, c)
				}
			}
			return true
		})
		t.Fatalf("no switch arm in %s() is guarded by `%s`.\n\nArms mentioning err or the census: %v\n\nWithout that second clause a failed qcow2 read skips the clearing even when qemu listed every bitmap on every disk, and the orphan survives a rebuild that had everything it needed to remove it.",
			runFuncName, want, conditions)
	}
}

// THE POST-CLEAR CHECK MUST ASK QEMU, NOT THE FILE.
//
// This is the one place the union is the WRONG answer, and reading the file
// here is a false positive that predates the census: a bitmap qemu has just
// released stays in the qcow2 directory until the image closes, so the file
// reports the orphan that was successfully removed as a survivor. Passing
// afterCensus.Union instead of afterCensus.FromQemu is one word, compiles, and
// restores that false positive.
func TestThePostClearCheckAsksQemuAndNotTheFile(t *testing.T) {
	const want = "afterCensus.FromQemu"

	body := runBody(t)

	var args []string
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isFuncCallNamed(call, "reportLeftoverBitmaps") || len(call.Args) < 2 {
			return true
		}
		args = append(args, exprText(call.Args[1]))
		return true
	})
	if len(args) == 0 {
		t.Fatalf("no call to reportLeftoverBitmaps in %s() takes a second argument", runFuncName)
	}
	found := false
	for _, a := range args {
		if a == want {
			found = true
		}
		if a == "afterCensus.Union" {
			t.Errorf("reportLeftoverBitmaps is handed %s after a clearing. A bitmap qemu has just released stays in the qcow2 until the image closes, so the union reports the orphan this run removed as one that survived -- the false positive the census was supposed to end", a)
		}
	}
	if !found {
		t.Errorf("no call to reportLeftoverBitmaps is handed %s; its second arguments are %v.\n\nAfter a clearing, qemu's own list is the only question that means anything: it is the component that was asked to drop the bitmap.", want, args)
	}
}

// THE FATALITY DECISION HAS TO BE ASKED, AND ASKED WITH THE REAL PROVENANCE.
//
// The owner's decision was that a clearing failure over a bitmap only qemu
// could see reports rather than refuses. Reverting to the unconditional
// `return fmt.Errorf("refusing to rebuild...")` is one deleted `if` and makes
// a newly-visible orphan start failing runs that used to succeed. Handing it a
// literal nil for the provenance is subtler and worse: it compiles, the
// function stays correct and tested, and every failure becomes fatal again.
func TestTheFatalityDecisionIsAskedWithTheRealProvenance(t *testing.T) {
	want := []string{"failedBitmaps", "leftoverOnlyQemu"}

	body := runBody(t)

	var got []string
	n := 0
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || !isFuncCallNamed(call, "clearFailureIsFatal") {
			return true
		}
		n++
		got = got[:0]
		for _, a := range call.Args {
			got = append(got, exprText(a))
		}
		return true
	})
	if n != 1 {
		t.Fatalf("%s() calls clearFailureIsFatal %d time(s), want exactly 1. Zero means the rebuild refuses over every clearing failure again, including one over a bitmap that was invisible until vmsync started asking qemu", runFuncName, n)
	}
	if strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Errorf("clearFailureIsFatal is called with (%s), want (%s).\n\nA literal nil for the provenance compiles and leaves the function correct and tested, and makes every clearing failure fatal again -- including the newly visible ones the owner decided should report instead.",
			strings.Join(got, ", "), strings.Join(want, ", "))
	}
}

// AND THE RUN THAT DID NOT REFUSE HAS TO SAY SO.
//
// THIS IS THE ONE THAT SURVIVED A MUTATION ROUND. Deleting the trace.Warning
// leaves the report function built, tested and never called: the run exits 0
// with an orphan still in place and nothing in the log admitting the clearing
// failed. That is the exact silence this whole feature exists to remove, and
// no behaviour test can see it because the decision lives inside run().
func TestTheForgivenClearingFailureIsStillReported(t *testing.T) {
	body := runBody(t)

	built, logged := 0, 0
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if isFuncCallNamed(call, "newlyVisibleClearFailureReport") {
			built++
		}
		if !isMethodCallNamed(call, "Warning") {
			return true
		}
		// A Warning whose first argument is the report: built AND emitted.
		if len(call.Args) > 0 {
			if inner, ok := call.Args[0].(*ast.CallExpr); ok && isFuncCallNamed(inner, "newlyVisibleClearFailureReport") {
				logged++
			}
		}
		return true
	})

	if built != 1 {
		t.Errorf("%s() builds newlyVisibleClearFailureReport %d time(s), want 1", runFuncName, built)
	}
	if logged != 1 {
		t.Errorf("newlyVisibleClearFailureReport is built but not passed to trace.Warning (%d emitted).\n\nA report that is computed and thrown away is the same as no report: the run exits 0 having failed to clear an orphan, and nothing in the log says so. Info is not enough either -- WarningCount feeds the metrics textfile and the bench greps warning prose.", logged)
	}
}

// THE RUN ACTS ON THE UNION, NOT ON EITHER READER ALONE.
//
// `census.FromQemu` compiles here, types identically, and is defensible out
// loud -- "the census is for reporting, act on what qemu holds". It drops
// every bitmap the image read found that qemu does not hold, which is a real
// set: a bitmap left in a file that qemu never reloaded, which is what a
// save/restore leaves once the metadata has gone. Neither reader is a superset
// of the other, so acting on one is acting on less than vmsync knows.
func TestTheRunActsOnTheUnionOfBothReaders(t *testing.T) {
	const want = "census.Union"

	body := runBody(t)

	got := ""
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) == 0 || len(as.Rhs) == 0 {
			return true
		}
		if id, ok := as.Lhs[0].(*ast.Ident); !ok || id.Name != "leftover" {
			return true
		}
		got = exprText(as.Rhs[0])
		return true
	})
	if got == "" {
		t.Fatalf("nothing in %s() assigns to `leftover`, so this test cannot vouch for what the clearing acts on", runFuncName)
	}
	if got != want {
		t.Errorf("`leftover` is assigned %s, want %s.\n\nThe set the clearing acts on must be both readers unioned. qemu's list alone drops a bitmap sitting in a file qemu never reloaded -- a real orphan after a save and restore with its metadata gone -- and the image read alone is the blindness this whole feature removes.", got, want)
	}
}

// AND THE CENSUS'S OWN NOTES HAVE TO REACH THE LOG, AT TWO LEVELS.
//
// bitmapCensus returns its findings rather than logging them, which is what
// makes every one of them unit-testable -- and which means the closure is the
// only thing that emits them. Deleting that loop leaves every census warning
// computed and discarded: a busy bitmap set aside, a disk qemu never
// described, a monitor refused, all silent. Collapsing the two levels into
// Info is quieter and worse, because WarningCount feeds the metrics textfile
// and the bench greps warning prose, so an Info-only census cannot be seen by
// either.
func TestTheCensusNotesAreEmittedAtBothLevels(t *testing.T) {
	body := runBody(t)

	// The closure, found by the name it is assigned to.
	var closure *ast.FuncLit
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		if id, ok := as.Lhs[0].(*ast.Ident); !ok || id.Name != "bitmapCensusFor" {
			return true
		}
		if fl, ok := as.Rhs[0].(*ast.FuncLit); ok {
			closure = fl
		}
		return true
	})
	if closure == nil {
		t.Fatalf("no func literal is assigned to bitmapCensusFor in %s()", runFuncName)
	}

	info, warn := 0, 0
	ast.Inspect(closure.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		if exprText(call.Args[0]) != "n.Text" {
			return true
		}
		switch {
		case isMethodCallNamed(call, "Info"):
			info++
		case isMethodCallNamed(call, "Warning"):
			warn++
		}
		return true
	})

	if warn != 1 {
		t.Errorf("the closure emits %d Warning(s) carrying a census note, want 1.\n\nWithout it a refused monitor, a busy bitmap set aside, and a disk qemu never described are all computed and thrown away -- and WarningCount, which the metrics textfile and the bench both read, never moves.", warn)
	}
	if info != 1 {
		t.Errorf("the closure emits %d Info call(s) carrying a census note, want 1.\n\nThe expected steady state on a running source -- qemu holding bitmaps the image does not carry yet -- is an observation, not a fault. Logging it as a warning puts a routine state into WarningCount, where the metrics and the bench read it as something wrong.", info)
	}
}

// THE DOOMED PACKAGE IS NOT A DEPENDENCY OF THE FILE THAT USES IT.
//
// Putting the route behind one adapter is what makes deleting it a bounded
// change: the only places that reach it are the package itself, the adapter,
// and the closure. main.go is 8,900 lines, so once it depends on that package
// directly a maintainer removing the route has to read all of it.
//
// The DEPENDENCY is what matters, not the spelling -- a comment naming the
// package is documentation and is welcome. So this reads the import list and
// the qualified identifiers, which is what a compiler would break on, rather
// than grepping for the word.
func TestTheDoomedPackageIsNotADependencyOfRun(t *testing.T) {
	const pkg = "unsupportedqmp"

	file := reclaimWiringFile(t, runSourceFile)

	for _, imp := range file.Imports {
		if imp.Path != nil && strings.Contains(imp.Path.Value, pkg) {
			t.Errorf("%s imports %s. Everything is meant to go through the adapter in unsupportedcensus.go, which is what keeps deleting the route a three-file change instead of an audit of this file", runSourceFile, imp.Path.Value)
		}
	}

	qualified := 0
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == pkg {
			qualified++
		}
		return true
	})
	if qualified != 0 {
		t.Errorf("%s names %d identifier(s) from %s directly. The adapter exists so this file never has to", runSourceFile, qualified, pkg)
	}
}
