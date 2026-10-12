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

// WHY THIS IS READ RATHER THAN EXECUTED: a signal disposition cannot be
// asserted from inside the process holding it -- observing "an unhandled
// SIGPIPE kills this" means sending one, which kills the test binary. The
// same reasoning, and the same shape, as cmd/vmsync/signals_test.go.
//
// WHAT IT GUARDS. Go terminates on a write to a broken pipe on fd 1 or 2,
// silently, with nothing deferred. For this binary that is not merely a lost
// log line: the agent supervises vmsync through exec.CommandContext and stops
// it by cancelling that context, so an agent that dies leaves every in-flight
// sync orphaned -- holding a libvirt backup job and a target run lock, with
// nobody to cancel it or record what it did. A domain in that state refuses
// every later sync until an operator clears the job by hand.
//
// The ORDER is half the property: SIGPIPE only fires on a write to those two
// descriptors, so anything that can write before the Ignore is a window in
// which a closed pager still kills the process -- and flag.Parse, which runs
// immediately after, prints usage to stderr on a mistyped flag.
func TestAgentMainIgnoresSIGPIPEFirst(t *testing.T) {
	const file = "main.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	var body []ast.Stmt
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if ok && fn.Name.Name == "main" && fn.Recv == nil {
			body = fn.Body.List
			break
		}
	}
	if body == nil {
		t.Fatal("no func main() in " + file)
	}

	for i, stmt := range body {
		expr, ok := stmt.(*ast.ExprStmt)
		if !ok {
			continue
		}
		call, ok := expr.X.(*ast.CallExpr)
		if !ok {
			continue
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "signal" || sel.Sel.Name != "Ignore" {
			continue
		}
		if i != 0 {
			t.Errorf("signal.Ignore is statement %d of main(); it must be the FIRST, because flag.Parse writes usage to stderr on a bad flag and a write to stderr is what raises SIGPIPE", i+1)
		}
		if len(call.Args) != 1 {
			t.Fatalf("signal.Ignore takes %d arguments, want exactly SIGPIPE", len(call.Args))
		}
		if got := agentExprText(call.Args[0]); !strings.Contains(got, "SIGPIPE") {
			t.Errorf("main() ignores %s, want syscall.SIGPIPE", got)
		}
		return
	}
	t.Error("main() never calls signal.Ignore(syscall.SIGPIPE); without it an agent piped into a pager dies silently when the pager quits, orphaning every sync it was supervising")
}

// TestAgentDoesNotCatchSIGPIPE pins the other half of the decision. Catching it
// would mean a handler, a handler would log, and logging to the broken pipe is
// what raises it -- so a caught SIGPIPE re-arms itself on the first line of
// whatever it interrupted. The three registrations this binary does have are
// deliberately different from each other (INT/TERM shut down, HUP reloads,
// USR1 dumps inventory), so there is no shared list to assert; what must hold
// is simply that SIGPIPE is in none of them.
func TestAgentDoesNotCatchSIGPIPE(t *testing.T) {
	fset := token.NewFileSet()
	for _, file := range []string{"main.go", "standalone.go", "status.go"} {
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "signal" {
				return true
			}
			if sel.Sel.Name != "Notify" && sel.Sel.Name != "NotifyContext" {
				return true
			}
			for _, arg := range call.Args {
				if strings.Contains(agentExprText(arg), "SIGPIPE") {
					t.Errorf("%s: signal.%s registers SIGPIPE; it must be ignored in main() instead, because a handler that logs re-raises it on its own first line",
						fset.Position(call.Pos()), sel.Sel.Name)
				}
			}
			return true
		})
	}
}

// agentExprText renders a selector expression back to source, for messages.
func agentExprText(e ast.Expr) string {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		if id, ok := e.(*ast.Ident); ok {
			return id.Name
		}
		return "an unrecognised expression"
	}
	if pkg, ok := sel.X.(*ast.Ident); ok {
		return pkg.Name + "." + sel.Sel.Name
	}
	return sel.Sel.Name
}
