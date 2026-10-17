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
	"os"
	"strings"
	"testing"
)

// mentions reports whether an expression's source text contains name.
// Crude on purpose: the conditions being matched are short and this test
// is about which guard exists, not about evaluating it.
func mentions(fset *token.FileSet, src []byte, e ast.Expr, name string) bool {
	if e == nil {
		return false
	}
	a := fset.Position(e.Pos()).Offset
	b := fset.Position(e.End()).Offset
	if a < 0 || b > len(src) || a >= b {
		return false
	}
	return strings.Contains(string(src[a:b]), name)
}

// DETECTING IS NOT ACTING, AND MONITOR MODE MUST DO THE FIRST.
//
// fenceLoop used to be started only when cfg.Mode.actsOnItsOwn(), so a
// monitor-mode agent never swept at all. The gauge was still emitted --
// setSplitBrain is never called, so len() of an empty map is published --
// which made vmsync_agent_split_brain_vms read 0 for the life of the
// process. Not absent: an affirmative "no VM here is running in two places"
// about a question the agent never asked, on the one metric the README
// calls "the one to alert on", on a mode whose entire job is to report.
func TestTheFenceSweepRunsInEveryMode(t *testing.T) {
	const file = "main.go"
	fset := token.NewFileSet()
	src, err := parseSource(file)
	if err != nil {
		t.Fatal(err)
	}
	f, err := parser.ParseFile(fset, file, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	// Every region of source gated on the mode.
	type span struct{ from, to token.Pos }
	var modeGated []span
	ast.Inspect(f, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok || !mentions(fset, src, ifs.Cond, "actsOnItsOwn") {
			return true
		}
		modeGated = append(modeGated, span{ifs.Body.Pos(), ifs.Body.End()})
		return true
	})
	if len(modeGated) == 0 {
		t.Fatal("no mode-gated blocks found in main.go at all; this test locates the hazard by them, so it is now checking nothing")
	}

	var startPos token.Pos
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "fenceLoop" {
			startPos = call.Pos()
		}
		return true
	})
	if startPos == 0 {
		t.Fatal("run() does not start fenceLoop at all")
	}

	for _, g := range modeGated {
		if startPos > g.from && startPos < g.to {
			t.Errorf("fenceLoop is started inside a block gated on cfg.Mode.actsOnItsOwn() (line %d).\n"+
				"  A monitor-mode agent then never sweeps, and vmsync_agent_split_brain_vms is published as 0 rather than being absent -- an affirmative all-clear for a question nobody asked.\n"+
				"  Monitor mode must not FENCE; checkOneFence is where that is enforced. Starting the sweep is what makes the finding reportable.",
				fset.Position(startPos).Line)
		}
	}
}

// ...and the other half of the same decision: the sweep may report in
// monitor mode, but it must never shut a guest down there.
//
// Enforced positionally, because the branch itself cannot be reached in a
// test -- getting to it needs a peer host answering a fence query.
func TestMonitorModeReportsButNeverFences(t *testing.T) {
	const file = "fence.go"
	fset := token.NewFileSet()
	src, err := parseSource(file)
	if err != nil {
		t.Fatal(err)
	}
	f, err := parser.ParseFile(fset, file, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "checkOneFence" {
			fn = fd
			break
		}
	}
	if fn == nil {
		t.Fatal("no func checkOneFence in " + file)
	}

	// Where the shutdown is issued.
	var actPos token.Pos
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "fenceOneDomain" {
			actPos = call.Pos()
		}
		return true
	})
	if actPos == 0 {
		t.Fatal("checkOneFence no longer calls fenceOneDomain; this test locates the action by that call, so rename it here rather than deleting the guard that keeps monitor mode from stopping guests")
	}

	// A mode guard that returns, before the action.
	guarded := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok || ifs.Pos() >= actPos || !mentions(fset, src, ifs.Cond, "actsOnItsOwn") {
			return true
		}
		for _, stmt := range ifs.Body.List {
			if _, isReturn := stmt.(*ast.ReturnStmt); isReturn {
				guarded = true
			}
		}
		return true
	})
	if !guarded {
		t.Error("checkOneFence has no monitor-mode guard returning before fenceOneDomain. An operator who adds --monitor to watch a cron-driven site would get unattended guest shutdowns they did not ask for, because autofence is on by default")
	}
}

// parseSource reads a source file for the position-based walks above.
func parseSource(name string) ([]byte, error) {
	return os.ReadFile(name)
}
