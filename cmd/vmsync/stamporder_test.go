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

// WHY THIS IS READ RATHER THAN EXECUTED, and what it is blind to: the same
// reasons runorder_test.go gives at length for the same function. run() opens
// libvirt connections to two hypervisors and writes to a replica's disks, so
// no unit test can call it; and this walk reads POSITIONS, never the
// conditions a landmark sits under, so a guard flipped to never-true would
// pass here. That blind spot is documented there and not repeated.
//
// THE PROPERTY. A base image's mtime moves when the delta is committed into
// it. The next run's preflight refuses a replica whose mtime is ahead of
// everything on record, and recordReplicaWrittenAt is that record. So the
// stamp has to be taken while those two facts are still adjacent -- between
// the commit and anything long.
//
// THE INCIDENT. It used to be taken only after phase two, which contains
// -verify. On a terabyte disk that is tens of minutes, against the seconds a
// commit takes. Four runs were killed inside that window: every one had
// committed its data, none had recorded it, and every subsequent sync of those
// domains refused with "something wrote to the replica between syncs" -- true,
// and the something was the interrupted run. Unwedging each needed a hand-
// edited metadata field or a tolerance wide enough to hide a real finding.
//
// WHY A TEST RATHER THAN A COMMENT. The fix is one call in the right place,
// and nothing about the code resists moving it: the later stamp still exists
// and is still correct, so deleting the early one leaves a tree that compiles,
// passes every other test, syncs perfectly in every normal run, and reproduces
// the wedge only when a verify is interrupted on a big disk -- i.e. in
// production, weeks later, with no way to connect cause to effect. That is
// exactly the shape of bug worth spending a source-reading test on.
func TestReplicaIsStampedBetweenTheCommitAndPhaseTwo(t *testing.T) {
	const file = "main.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	var body *ast.FuncDecl
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if ok && fn.Name.Name == "run" && fn.Recv == nil {
			body = fn
			break
		}
	}
	if body == nil {
		t.Fatal("no func run() in " + file)
	}

	// Earliest commit, earliest phase-two step, and every stamp. Earliest for
	// the two bounds because they are the deadline: a second commit further
	// down says nothing about when the first one moved an mtime, and a second
	// finishDisk says nothing about when the long step began.
	var commitPos, phaseTwoPos token.Pos
	var stamps []token.Pos
	ast.Inspect(body.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok {
			return true
		}
		switch id.Name {
		case "commitStaged":
			if !commitPos.IsValid() {
				commitPos = call.Pos()
			}
		case "finishDisk":
			if !phaseTwoPos.IsValid() {
				phaseTwoPos = call.Pos()
			}
		case "recordReplicaWrittenAt":
			stamps = append(stamps, call.Pos())
		}
		return true
	})

	// A landmark that has moved into a helper vanishes from this walk, and
	// that is reported rather than tolerated: the guarantee has left the place
	// this test can see it, and somebody has to decide where it now lives.
	switch {
	case !commitPos.IsValid():
		t.Fatal("no call to commitStaged in run(); the landmark this ordering is measured from has moved or been renamed")
	case !phaseTwoPos.IsValid():
		t.Fatal("no call to finishDisk in run(); the landmark marking the start of the long phase has moved or been renamed")
	case len(stamps) == 0:
		t.Fatal("run() never calls recordReplicaWrittenAt; nothing records that this run wrote the replica, so the NEXT run's preflight will refuse it")
	}

	// finishDisk is invoked from a goroutine, so its source position is a
	// LOWER bound on when it runs. That is sound in this direction and only
	// this one: a stamp ahead of it in the text is ahead of it in time,
	// because the stamp is a plain statement in run()'s straight-line flow
	// and the goroutine has not been launched yet.
	for _, s := range stamps {
		if s > commitPos && s < phaseTwoPos {
			return
		}
	}

	t.Errorf("run() takes no replica stamp between commitStaged (line %d) and finishDisk (line %d) -- the %d stamp(s) it does take are at line(s) %s, all of them on the far side of the verify. A run interrupted during a verify will commit its data, record nothing, and wedge every later sync of that domain",
		fset.Position(commitPos).Line, fset.Position(phaseTwoPos).Line, len(stamps), lines(fset, stamps))
}

func lines(fset *token.FileSet, pos []token.Pos) string {
	out := ""
	for i, p := range pos {
		if i > 0 {
			out += ", "
		}
		out += itoa(fset.Position(p).Line)
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
