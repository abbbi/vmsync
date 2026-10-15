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
	"strings"
	"testing"
)

// TestOnlySafeTargetCommandsGetARuntimeLimit is the guard on which commands may be
// abandoned, and it is the one test in this file that protects a replica
// rather than a wait.
//
// A runtime limit means the helper signals the command when it expires. That is
// only ever right where what the command leaves behind can be cleaned up by
// whoever comes next, and for most target commands it cannot:
//
//   - qemu-img commit leaves a base image that is neither its old self nor
//     its new one, and nothing afterwards can tell which parts were written;
//   - the write and verify export starts leave a qemu-nbd that no pidfile
//     names, because --fork daemonizes before the image is open and the
//     pidfile is written only after setup;
//   - creating the overlay leaves a file its cleanup was armed too late to
//     remove.
//
// So the limit is opted into per call site, and this pins the list. A new
// call site that wants one has to be added here, which is the moment to ask
// whether abandoning it is safe.
//
// MUTATION CAUGHT: giving runTargetCommand itself a non-zero default, which
// would silently limit every command in the file, commit included.
func TestOnlySafeTargetCommandsGetARuntimeLimit(t *testing.T) {
	const file = "main.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	// What each limited call site is, by a fragment of its action text.
	// Short enough to survive rewording, specific enough to name one site.
	allowed := []string{"mkdir", "qemu-io", "checksum export"}
	limited := 0

	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		fn, ok := call.Fun.(*ast.Ident)
		if !ok || fn.Name != "runTargetCommandWithin" || len(call.Args) == 0 {
			return true
		}
		arg, ok := call.Args[0].(*ast.Ident)
		if !ok || arg.Name != "targetCommandMaxRuntime" {
			// A literal duration, or anything else, means somebody invented
			// a second number for this. One constant or none.
			if !isZeroLiteral(call.Args[0]) {
				t.Errorf("%s: a target command is limited with something other than targetCommandMaxRuntime", fset.Position(call.Pos()))
			}
			return true
		}
		limited++
		src := renderCall(fset, call)
		matched := false
		for _, want := range allowed {
			if strings.Contains(src, want) {
				matched = true
			}
		}
		if !matched {
			t.Errorf("%s: this target command was given a runtime limit, so the helper will signal it when that runs out. Abandoning it must be safe -- nothing half-done that the next run cannot clean up. If it is, add it to the list in this test; if it is not, drop the limit and let it run under the liveness line alone.\n  %s",
				fset.Position(call.Pos()), src)
		}
		return true
	})

	if limited != len(allowed) {
		t.Errorf("found %d target commands with a runtime limit, want %d -- the list in this test and the call sites have drifted apart", limited, len(allowed))
	}
}

// TestTheCommitNeverGetsARuntimeLimit says the most important half of the rule
// outright, rather than leaving it to be inferred from a list.
func TestTheCommitNeverGetsARuntimeLimit(t *testing.T) {
	const file = "main.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		fn, ok := call.Fun.(*ast.Ident)
		if !ok || fn.Name != "runTargetCommandWithin" {
			return true
		}
		if src := renderCall(fset, call); strings.Contains(src, "commit") {
			t.Errorf("%s: the commit has a runtime limit. A qemu-img commit signalled part-way leaves the base image in a state no later run can describe, and the replica is then neither its old self nor its new one.\n  %s",
				fset.Position(call.Pos()), src)
		}
		return true
	})
}

// isZeroLiteral reports whether e is the literal 0, which is how a call site
// says "no limit" when it passes one explicitly.
func isZeroLiteral(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	return ok && lit.Value == "0"
}

// renderCall is the call's source text, for an error message that shows the
// reader which call site is meant.
//
// Every string literal anywhere under the call is appended, not just the
// top-level arguments: the action text that identifies a call site is
// usually inside a fmt.Sprintf, and a renderer that stopped at the argument
// list would show "fmt.Sprintf(...)" for every one of them.
func renderCall(fset *token.FileSet, call *ast.CallExpr) string {
	var b strings.Builder
	for i, a := range call.Args {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(argText(fset, a))
	}
	ast.Inspect(call, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			b.WriteString(" " + lit.Value)
		}
		return true
	})
	return b.String()
}

// exprText renders one argument well enough to recognise it.
func argText(fset *token.FileSet, e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.BasicLit:
		return v.Value
	case *ast.BinaryExpr:
		return argText(fset, v.X) + " + " + argText(fset, v.Y)
	case *ast.CallExpr:
		return argText(fset, v.Fun) + "(...)"
	case *ast.SelectorExpr:
		return argText(fset, v.X) + "." + v.Sel.Name
	default:
		return "?"
	}
}
