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

// THE WIRING, not the logic.
//
// sizeMismatchIsExplained and its truth table are covered by ordinary tests.
// What those cannot reach is whether the run actually CONSULTS it, and
// whether res.stagedDelta is derived from the right thing -- both live inside
// closures in the sync's run function, which needs a source, a target and an
// SSH session to enter. Mutation testing proved the gap: replacing the call
// with `if false`, or the assignment with `= false`, left the entire suite
// green while silently reverting the behaviour.
//
// So this asserts the shape of the wiring instead, the way runorder_test.go
// and stamporder_test.go already do for orderings that cannot be unit
// tested. It is a weaker kind of test and it is the one available.
func parseMainFile(t *testing.T) (*token.FileSet, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	return fset, f
}

// The predicate has to be consulted. An unused predicate is the behaviour
// removed, with the tests that cover it still passing.
func TestTheSizeMismatchPolicyIsActuallyConsulted(t *testing.T) {
	_, f := parseMainFile(t)

	calls := 0
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "sizeMismatchIsExplained" {
			calls++
		}
		return true
	})
	if calls == 0 {
		t.Error("nothing in main.go calls sizeMismatchIsExplained, so a resized source fails every sync that has nothing to copy -- and every test of the predicate still passes")
	}
	if calls > 1 {
		t.Errorf("sizeMismatchIsExplained is called %d times; the decision is meant to be taken once, where the verify result is handled", calls)
	}
}

// res.stagedDelta must be DERIVED, not asserted.
//
// It records whether this run created an image for the disk, which is the
// same condition that decides whether to skip the copy. Assigning a literal
// would let the two drift: a run that staged a delta claiming it did not
// excuses a real size fault, and the reverse fails a legitimate resize.
func TestStagedDeltaIsDerivedFromTheSkipDecision(t *testing.T) {
	fset, f := parseMainFile(t)

	assignments := 0
	ast.Inspect(f, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, lhs := range as.Lhs {
			sel, ok := lhs.(*ast.SelectorExpr)
			if !ok || sel.Sel == nil || sel.Sel.Name != "stagedDelta" {
				continue
			}
			assignments++
			if i >= len(as.Rhs) {
				t.Errorf("%s: stagedDelta assigned with no matching right-hand side", fset.Position(as.Pos()))
				continue
			}
			// A negation of something, never a literal.
			unary, ok := as.Rhs[i].(*ast.UnaryExpr)
			if !ok || unary.Op != token.NOT {
				t.Errorf("%s: stagedDelta is assigned %T rather than the negation of the skip decision. A literal here lets it drift from the condition that actually decides whether anything was staged",
					fset.Position(as.Pos()), as.Rhs[i])
				continue
			}
			if _, ok := unary.X.(*ast.Ident); !ok {
				t.Errorf("%s: stagedDelta is assigned the negation of %T, want the named skip decision", fset.Position(as.Pos()), unary.X)
			}
		}
		return true
	})

	if assignments != 1 {
		t.Errorf("res.stagedDelta is assigned %d times in main.go, want exactly once -- a second assignment is a second opinion about what this run staged", assignments)
	}
}
