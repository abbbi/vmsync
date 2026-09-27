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
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// runSourceFile is the file run() lives in, relative to this package's
// directory, which is where `go test` puts the working directory.
const runSourceFile = "main.go"

// runFuncName is the function whose interior order is the subject here.
const runFuncName = "run"

// WHY THIS IS READ RATHER THAN EXECUTED.
//
// run() opens libvirt connections to two hypervisors, dials SSH to both, and
// writes to a replica's disks. It cannot be called from a unit test on any
// machine, ever -- not here, not in CI, not on a developer's laptop. So the
// ordinary way of pinning a sequence, running it and watching what happens,
// is not available for the one function where the sequence IS the safety
// property. Everything downstream can be tested and is: the journal sink is
// synchronous, the sweep displaces what it says it displaces, the marker
// parses, the promotion refuses. None of that proves the calls are still made
// in the right place, or made at all.
//
// This closes that: it parses the real source of run() and asserts where the
// arming happens relative to the destruction it is supposed to survive.
//
// WHAT WOULD MAKE THIS A FALSE POSITIVE, stated plainly because a check on
// source text earns its keep only if its blind spots are known:
//
//   - Source position is not execution order. A landmark moved inside a
//     func literal that runs later -- a defer, a goroutine, an errgroup --
//     would keep its place in the text while moving in time, and this test
//     would not notice. Five of the six landmarks are plain statements in
//     run()'s own straight-line flow. The sixth is not: the `qemu-img create`
//     that writes a base sits inside the copyAndStage closure, which the disk
//     workers invoke later. That is sound in this direction and only this
//     one -- its position in the text is a LOWER bound on when it runs, so
//     "the arming precedes it here" implies "the arming precedes it in time",
//     while the reverse would not follow.
//   - Moving a landmark into a helper that run() calls makes it vanish from
//     this walk, and the test then fails with "not found" although behaviour
//     may be perfectly correct. That failure is deliberate rather than
//     tolerated: the ordering guarantee has left the place this test can see
//     it, and somebody has to decide where it now lives.
//   - Two landmarks are recognised by the command text they build ("mv -n",
//     "rm -f", "qemu-img create"). Assemble any of them from pieces, or move
//     one to a constant outside run(), and it is no longer found. Same
//     failure, same remedy.
//
// WHAT WOULD MAKE THIS A FALSE NEGATIVE -- the direction that matters more,
// since a test that passes wrongly is worse than one that fails wrongly:
//
//   - This walk reads POSITIONS, never the conditions a landmark sits under.
//     Flip the guard on either arming -- `if parent == "" && !cfg.Reinit` to
//     something never true, say -- and every check here still passes while
//     no run arms anything. Only the bench stage, which runs a real rebuild
//     and then reads the domain, can catch that.
//   - A landmark whose enclosing branch has changed meaning is found, in
//     order, and executed on a different path than the one being reasoned
//     about.
//   - The armings are judged on their LAST occurrence precisely so a
//     duplicate left in an unreachable branch cannot vouch for one that
//     moved below the damage (see orderedStep.latest). Before that, a decoy
//     arming earlier in the function satisfied this test on behalf of a real
//     one that had been deleted.
//
// Renaming variables, reflowing the code, or reformatting it changes nothing
// here: the walk is over the parsed syntax tree, matching on function and
// identifier names rather than on the shape of the text.

// orderedStep is one landmark inside run(), and where it sits in the source.
type orderedStep struct {
	// what it is, in the words a failure message uses.
	what string
	// how it was recognised, so a message can say what was matched on rather
	// than leaving a reader to guess why the test thinks it found something.
	how   string
	pos   token.Pos
	line  int
	found bool
	// latest inverts which occurrence is kept, and which way round it goes is
	// decided by what the landmark has to PROVE rather than by what reads
	// naturally.
	//
	// A landmark that must come LAST of its pair -- a destructive step -- is
	// kept at its earliest occurrence: the first byte that displaces the
	// replica is the deadline everything else has to beat, and a second
	// rename further down says nothing about when the damage started.
	//
	// A landmark that must come FIRST -- the intent, the two armings -- is
	// kept at its LATEST occurrence, and that asymmetry is load-bearing.
	// Keeping the earliest would let a second, later call to the same
	// function satisfy the check on behalf of one that had been moved below
	// the destruction, or deleted: the walk would find an arming in the right
	// place, in a branch that never runs on this path, and report order while
	// the replica was armed by nothing. Taking the latest asks the stricter
	// question -- is EVERY arming ahead of the damage -- which is the property
	// the record actually depends on.
	latest bool
}

// note records this landmark, keeping whichever occurrence its direction
// makes the safe one to judge on. See orderedStep.latest.
func (s *orderedStep) note(fset *token.FileSet, pos token.Pos, how string) {
	if s.found {
		if s.latest && pos <= s.pos {
			return
		}
		if !s.latest && s.pos <= pos {
			return
		}
	}
	s.found, s.pos, s.line, s.how = true, pos, fset.Position(pos).Line, how
}

// runSteps are the landmarks whose relative order is the mechanism.
type runSteps struct {
	intent      orderedStep
	armReinit   orderedStep
	armFullSync orderedStep
	sweep       orderedStep
	displace    orderedStep
	createBase  orderedStep
}

func (s *runSteps) all() []*orderedStep {
	return []*orderedStep{&s.intent, &s.armReinit, &s.armFullSync, &s.sweep, &s.displace, &s.createBase}
}

// findRunSteps walks run()'s body and reports where each landmark sits.
//
// The two arming calls are told apart by the VERB they pass, not by which
// comes first in the file: reinitVerb(cfg) for the rebuild path and the
// full-sync constant for the other. So the pair can be reordered, renamed or
// have a third arming site added between them without this test quietly
// starting to compare the wrong one against the wrong destruction.
func findRunSteps(path string) (runSteps, error) {
	var steps runSteps
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return steps, fmt.Errorf("parse %s: %w", path, err)
	}
	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv == nil && fn.Name.Name == runFuncName && fn.Body != nil {
			body = fn.Body
			break
		}
	}
	if body == nil {
		return steps, fmt.Errorf("no func %s() with a body in %s", runFuncName, path)
	}

	steps.intent.what = "the sync intent reaching the target"
	steps.armReinit.what = "arming replica_incomplete for the reinit path"
	steps.armFullSync.what = "arming replica_incomplete for the non-reinit full sync"
	steps.sweep.what = "the restore-point sweep"
	steps.displace.what = "displacing the replica's disks"
	steps.createBase.what = "the first qemu-img create of a base"

	// The three that must precede something are judged on their LAST
	// occurrence, so an arming left behind in a branch cannot vouch for one
	// that moved below the damage. See orderedStep.latest.
	steps.intent.latest = true
	steps.armReinit.latest = true
	steps.armFullSync.latest = true

	ast.Inspect(body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			switch {
			case isMethodCallNamed(node, "Intent") && hasIdentArg(node, "journalVerbSync"):
				steps.intent.note(fset, node.Pos(), "a call to .Intent(..., journalVerbSync, ...)")
			case isFuncCallNamed(node, "armReplicaIncomplete"):
				switch {
				case hasCallArgNamed(node, "reinitVerb"):
					steps.armReinit.note(fset, node.Pos(), "armReplicaIncomplete(..., reinitVerb(...), ...)")
				case hasSelectorArgNamed(node, "ReplicaIncompleteVerbFullSync"):
					steps.armFullSync.note(fset, node.Pos(), "armReplicaIncomplete(..., ReplicaIncompleteVerbFullSync, ...)")
				}
			case isFuncCallNamed(node, "sweepRestorePointsForReinit"):
				steps.sweep.note(fset, node.Pos(), "a call to sweepRestorePointsForReinit")
			}
		case *ast.Ident:
			// The suffix the good disks are renamed to. Named as well as the
			// mv command itself so that either one alone still marks the spot.
			if node.Name == "replacedDiskSuffix" {
				steps.displace.note(fset, node.Pos(), "the replacedDiskSuffix constant")
			}
		case *ast.BasicLit:
			if node.Kind != token.STRING {
				return true
			}
			text, err := strconv.Unquote(node.Value)
			if err != nil {
				return true
			}
			// HasPrefix, not Contains: a log line that merely mentions one of
			// these commands is not the command, and matching one would put
			// the landmark earlier than the moment it stands for.
			switch {
			case strings.HasPrefix(text, "mv -n"):
				steps.displace.note(fset, node.Pos(), "the `mv -n` that moves a replica disk aside")
			case strings.HasPrefix(text, "rm -f"):
				// The other half of the same switch, and the more
				// destructive one: -replaced-disk-action=delete removes the
				// replica outright rather than renaming it, so there is no
				// aside copy to go back to. Matched explicitly rather than
				// left to sit a few lines below its sibling, because a
				// refactor that split the two branches apart would otherwise
				// leave the deleting one pinned by nothing at all.
				steps.displace.note(fset, node.Pos(), "the `rm -f` that deletes a replica disk outright")
			case strings.HasPrefix(text, "qemu-img create"):
				steps.createBase.note(fset, node.Pos(), "the `qemu-img create` that writes a base")
			}
		}
		return true
	})
	return steps, nil
}

func isFuncCallNamed(call *ast.CallExpr, name string) bool {
	id, ok := call.Fun.(*ast.Ident)
	return ok && id.Name == name
}

// isMethodCallNamed matches x.Name(...) whatever x is, so the recorder can be
// renamed or replaced by a field without this stopping to find it.
func isMethodCallNamed(call *ast.CallExpr, name string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel != nil && sel.Sel.Name == name
}

func hasIdentArg(call *ast.CallExpr, name string) bool {
	for _, arg := range call.Args {
		if id, ok := arg.(*ast.Ident); ok && id.Name == name {
			return true
		}
	}
	return false
}

func hasCallArgNamed(call *ast.CallExpr, name string) bool {
	for _, arg := range call.Args {
		if inner, ok := arg.(*ast.CallExpr); ok && isFuncCallNamed(inner, name) {
			return true
		}
	}
	return false
}

// hasSelectorArgNamed matches pkg.Name, ignoring which package: the import
// could be aliased, and the constant's own name is what identifies it.
func hasSelectorArgNamed(call *ast.CallExpr, name string) bool {
	for _, arg := range call.Args {
		if sel, ok := arg.(*ast.SelectorExpr); ok && sel.Sel != nil && sel.Sel.Name == name {
			return true
		}
	}
	return false
}

// THE ORDERING THE WHOLE MECHANISM RESTS ON, pinned where it actually lives.
//
// Every one of these pairs says the same thing in a different place: a record
// written AFTER the disks move is a record that cannot protect them. Between
// the first displacement and the redefine that clears the marker, the target
// domain describes a replica that is no longer underneath it -- last
// checkpoint, last sync, replica source, all still reading healthy over a
// half-written image. A run that dies in that window is what CI-02 was: the
// marker exists to make the window visible, and it can only do that if it is
// already on the domain when the window opens.
func TestRunArmsTheReplicaBeforeItDisplacesAnything(t *testing.T) {
	steps, err := findRunSteps(runSourceFile)
	if err != nil {
		t.Fatalf("this test guards an ordering nothing else can guard, so it must not pass by failing to look: %v", err)
	}
	for _, step := range steps.all() {
		if !step.found {
			t.Fatalf("%s was not found anywhere inside %s(). Either the call is gone -- in which case the replica is being rebuilt with nothing recorded to say so, and an interrupted rebuild promotes as a healthy replica -- or it has moved somewhere this test cannot see it, and the ordering guarantee has to be re-pinned wherever it now lives. This test looks for it in %s",
				step.what, runFuncName, runSourceFile)
		}
	}

	mustComeBefore(t, steps.intent, steps.armReinit,
		"The journal entry is the only evidence that survives the machine running this sync dying mid-flight, and it is written to the target over the same link the rebuild uses. Written after the rebuild has begun, it records an event that has already happened to disks nobody can account for -- and if the link is what failed, it is never written at all.")

	mustComeBefore(t, steps.armReinit, steps.sweep,
		"The sweep is the first thing a reinit does that touches the target's filesystem. Once it has moved the restore point set aside, the replica no longer matches its own metadata; the marker written after that point describes a state the domain was already in.")

	mustComeBefore(t, steps.armReinit, steps.displace,
		"The rename is the moment the last complete copy of this replica stops being what the domain points at. The marker carries the stamp those files were renamed with, so arming after the rename means an interrupted run leaves disks moved aside and nothing on the domain naming where they went -- the exact recovery the stamp exists to make possible, lost.")

	mustComeBefore(t, steps.armFullSync, steps.createBase,
		"A full sync with no checkpoint parent writes base images directly, with no overlay to throw away, while the domain keeps the metadata of the replica it is overwriting. The qemu-img create is the first byte of that. Arming after it means every earlier failure leaves a half-written base behind a clean bill of health.")

	// The non-reinit path deserves the same guard on the journal as the
	// reinit path gets above: it displaces nothing by renaming, so the arming
	// call is not between the intent and the first write.
	mustComeBefore(t, steps.intent, steps.createBase,
		"On a run that never touches the reinit block, the qemu-img create is the first thing that writes to the replica at all. An intent recorded after it cannot mark the start of anything.")
}

func mustComeBefore(t *testing.T, first, second orderedStep, why string) {
	t.Helper()
	if first.pos < second.pos {
		return
	}
	t.Errorf("%s (%s:%d, found as %s) must come BEFORE %s (%s:%d, found as %s) inside %s(), and it does not.\n\n%s",
		first.what, runSourceFile, first.line, first.how,
		second.what, runSourceFile, second.line, second.how,
		runFuncName, why)
}
