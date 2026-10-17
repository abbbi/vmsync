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
// reasons runorder_test.go and stamporder_test.go give at length for the
// same function. run() opens libvirt connections to two hypervisors, so no
// unit test can call it.
//
// But the specific thing being checked could not be caught by running it
// either, and that is the point. The race needs a signal to land inside a
// window of a few libvirt RPCs while a disk goroutine is mid-call; the
// detector would see it perhaps once in thousands of interrupted runs, and
// the losing interleaving is a use-after-free in C, which can corrupt
// quietly rather than crash. A property of the source is therefore what
// gets checked.
//
// THE INVARIANT. libvirtsync.Manager has no lock, Conn is a public field
// callers dereference directly, and Reconnect replaces it in place. So the
// rule is about callers: everything touching run()'s target Manager stays
// on run()'s own goroutine. The signal handler is the one goroutine in this
// file that is deliberately NOT synchronised with run() -- it does not wait
// for wg.Wait(), because a disk goroutine blocked in a cgo libnbd call can
// never be made to return -- so it must own any libvirt connection it uses.
//
// HOW IT BROKE, and why a comment was not enough: every other object the
// handler shares with run() has a mutex and a written rationale --
// sshClientMu, interruptedMu, srcDomMu, defineDomainMu, stampMu,
// checkpointMu. tgtMgr never got one, not because anybody decided against
// it, but because it is a CAPTURED variable: sharing it took no declaration,
// no new field, and no line of code that looks like sharing. The stamp
// closure simply referred to it, and the handler called the closure.
//
// So the check is for the name itself inside the handler's own body, which
// is the shape the next instance would take too.
func TestTheSignalHandlerDoesNotTouchRunsTargetConnection(t *testing.T) {
	const file = "main.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	var runFn *ast.FuncDecl
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if ok && fn.Name.Name == "run" && fn.Recv == nil {
			runFn = fn
			break
		}
	}
	if runFn == nil {
		t.Fatal("no func run() in " + file)
	}

	// The handler is the goroutine that selects on the signal channel. Found
	// by that shape rather than by line number so it survives edits above
	// it, and asserted to exist so this test cannot pass by failing to find
	// anything -- which is how a source-reading test rots into a no-op.
	var handler *ast.FuncLit
	ast.Inspect(runFn.Body, func(n ast.Node) bool {
		gos, ok := n.(*ast.GoStmt)
		if !ok {
			return true
		}
		lit, ok := gos.Call.Fun.(*ast.FuncLit)
		if !ok {
			return true
		}
		found := false
		ast.Inspect(lit.Body, func(in ast.Node) bool {
			if id, ok := in.(*ast.Ident); ok && id.Name == "sigCh" {
				found = true
			}
			return !found
		})
		if found && handler == nil {
			handler = lit
		}
		return true
	})
	if handler == nil {
		t.Fatal("could not find the signal-handler goroutine in run() -- it selects on sigCh; if that channel was renamed, rename it here too rather than deleting this test, which is the only thing keeping the handler off run()'s libvirt connection")
	}

	var offenders []token.Pos
	ast.Inspect(handler.Body, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == "tgtMgr" {
			offenders = append(offenders, id.Pos())
		}
		return true
	})
	if len(offenders) > 0 {
		t.Errorf("the signal handler refers to tgtMgr at line(s) %s. That Manager belongs to run()'s goroutine, which this handler deliberately does not wait for: libvirtsync.Manager has no lock, and a reconnect through it replaces a connection another goroutine may be inside -- a use-after-free in C, not a Go panic, on the path whose job is to leave the stamp that stops the next sync being refused. Open a connection of this goroutine's own instead, as the replica-stamp call does",
			lines(fset, offenders))
	}
}

// The counterpart: the stamp closure must not capture tgtMgr either, or the
// check above is trivially satisfied while the handler still reaches run()'s
// connection one call deeper.
//
// Asserted via the signature, because that is what makes the capture
// impossible rather than merely absent: a closure taking the Manager as a
// parameter cannot quietly fall back on the captured one.
func TestTheReplicaStampTakesItsConnectionAsAParameter(t *testing.T) {
	const file = "main.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	var lit *ast.FuncLit
	ast.Inspect(f, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		id, ok := as.Lhs[0].(*ast.Ident)
		if !ok || id.Name != "recordReplicaWrittenAt" {
			return true
		}
		if fl, ok := as.Rhs[0].(*ast.FuncLit); ok {
			lit = fl
		}
		return true
	})
	if lit == nil {
		t.Fatal("could not find the recordReplicaWrittenAt closure in " + file)
	}

	hasMgrParam := false
	for _, field := range lit.Type.Params.List {
		star, ok := field.Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		sel, ok := star.X.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Manager" {
			continue
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "libvirtsync" {
			hasMgrParam = true
		}
	}
	if !hasMgrParam {
		t.Error("recordReplicaWrittenAt does not take a *libvirtsync.Manager parameter, so it is capturing one. The signal handler calls this closure without waiting for run(), and the closure reconnects on failure -- a captured Manager makes that a cross-goroutine connection swap, which is the race this signature exists to prevent")
	}
}
