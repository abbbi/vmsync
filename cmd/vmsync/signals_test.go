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
	"syscall"
	"testing"
)

// WHY THESE ARE READ RATHER THAN EXECUTED.
//
// A signal disposition cannot be asserted from inside the process that holds
// it: the only way to observe "an unhandled SIGHUP kills this process" is to
// send one, and a test that does that kills the test binary. The thing that
// actually went wrong was not a disposition anyway -- it was a LIST, copied
// into two places, updated in one. That is a property of the source, so the
// source is what gets checked, the same way runorder_test.go checks run()'s
// internal ordering for the same reason.
//
// The incident: SIGHUP was added to the sync path's handler and not to the
// mode dispatch, so -promote, -fence-domain, -invert, -shutdown-domain,
// -break-target-lock, -clone-restore-point and -restore-restore-point kept
// dying silently on a terminal hangup, leaving libvirt backup jobs open that
// blocked every later sync of the domain. Before that, neither site had SIGHUP
// at all, and four concurrent runs vanished mid-verify without a log line.

// TestStopSignalsCoversTheSilentKillers pins the membership of the list. SIGHUP
// is the one that matters and the one that was missing: unlike SIGINT and
// SIGTERM, which an operator sends deliberately and notices the absence of, a
// hangup arrives on its own, kills with no output under Go's default
// disposition, and is therefore invisible when it is not caught.
func TestStopSignalsCoversTheSilentKillers(t *testing.T) {
	want := map[os.Signal]string{
		os.Interrupt:    "Ctrl+C from an operator watching a run",
		syscall.SIGTERM: "systemd stopping a unit, or the runner's own KillChilds",
		syscall.SIGHUP:  "a dropped SSH session; kills SILENTLY when uncaught, which is how four runs once vanished mid-verify",
	}
	got := make(map[os.Signal]bool, len(stopSignals))
	for _, s := range stopSignals {
		got[s] = true
	}
	for s, why := range want {
		if !got[s] {
			t.Errorf("stopSignals is missing %v -- %s", s, why)
		}
	}

	// SIGPIPE must NOT be here. Catching it means the handler logs, and
	// logging to the broken pipe is what raises it: a caught SIGPIPE re-arms
	// itself on the first line of the cleanup it interrupted. main() ignores
	// it instead, which turns the write into a discarded EPIPE.
	if got[syscall.SIGPIPE] {
		t.Error("SIGPIPE is in stopSignals; it must be IGNORED in main() instead, because a handler that logs re-raises it on its own first line")
	}
}

// TestEverySignalRegistrationUsesTheSharedList is the regression guard for the
// drift itself. A new registration -- or an edit to an existing one -- that
// spells its signals out again reintroduces exactly the bug this file exists
// for, and would otherwise be invisible until a hangup landed on whichever
// verb was forgotten.
func TestEverySignalRegistrationUsesTheSharedList(t *testing.T) {
	const file = "main.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	found := 0
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
		switch sel.Sel.Name {
		case "Notify", "NotifyContext":
		default:
			// Ignore, Stop, Reset: no signal list to get wrong.
			return true
		}
		found++

		// The last argument must be the shared list, spread. Anything else --
		// a literal syscall.SIGTERM, a locally built slice -- is the drift.
		if call.Ellipsis == token.NoPos {
			t.Errorf("%s: signal.%s spells its signals out instead of passing stopSignals... -- that is how SIGHUP came to be registered in one place and not the other",
				fset.Position(call.Pos()), sel.Sel.Name)
			return true
		}
		last, ok := call.Args[len(call.Args)-1].(*ast.Ident)
		if !ok || last.Name != "stopSignals" {
			t.Errorf("%s: signal.%s spreads something other than stopSignals", fset.Position(call.Pos()), sel.Sel.Name)
		}
		return true
	})

	// Both known sites must still be there. A registration that disappears is
	// as bad as one that drifts: it means a whole class of run stops cleaning
	// up after itself, which is what leaves backup jobs behind.
	if found < 2 {
		t.Errorf("found %d signal.Notify/NotifyContext call(s) in %s, want at least 2 (the mode dispatch and the sync path's handler)", found, file)
	}
}

// TestMainIgnoresSIGPIPEBeforeItCanLog pins the ORDER, which is the whole
// value of the call. SIGPIPE only fires on a write to fd 1 or 2, so anything
// that logs before the Ignore is a window in which the agent's closed pipe
// still kills the process -- and the pprof block right after it logs.
func TestMainIgnoresSIGPIPEBeforeItCanLog(t *testing.T) {
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
			t.Errorf("signal.Ignore is statement %d of main(); it must be the FIRST, because every statement before it can write a log line and a log line is what raises SIGPIPE", i+1)
		}
		if len(call.Args) != 1 {
			t.Fatalf("signal.Ignore takes %d arguments, want exactly SIGPIPE", len(call.Args))
		}
		if got := exprText(fset, call.Args[0]); !strings.Contains(got, "SIGPIPE") {
			t.Errorf("main() ignores %s, want syscall.SIGPIPE", got)
		}
		return
	}
	t.Error("main() never calls signal.Ignore(syscall.SIGPIPE); without it, an agent-launched run is killed silently by its own log line once cmd.WaitDelay closes the pipe mid-cleanup")
}

// exprText renders an expression back to source, for error messages.
func exprText(fset *token.FileSet, e ast.Expr) string {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return "an unrecognised expression"
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return sel.Sel.Name
	}
	return pkg.Name + "." + sel.Sel.Name
}
