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

package remotessh

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"sync"
	"testing"
)

// TestHoldStderrKeepsWhatTheCommandSaid is the fix for a message that
// described its own blindness.
//
// A mis-pathed helper makes the remote shell print "No such file or
// directory" on STDERR. holdCommand attached a writer to stdout only, so
// x/crypto/ssh discarded that, the first stdout line came back empty, and the
// refusal reported "it exited without saying why" -- over the top of a
// perfectly good explanation. It was observed in the field on a bench run
// whose helper path did not exist, where it also hid the reason the target
// lock had silently fallen back to unleased.
func TestHoldStderrKeepsWhatTheCommandSaid(t *testing.T) {
	var s holdStderr
	if got := s.String(); got != "" {
		t.Errorf("a command that said nothing reads as %q, want empty", got)
	}
	if _, err := s.Write([]byte("/nonexistent/vmsync-bridge-helper: No such file or directory\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := s.String(); got != "/nonexistent/vmsync-bridge-helper: No such file or directory" {
		t.Errorf("stderr came back as %q", got)
	}
}

// TestHoldStderrIsBounded keeps a command that fails by printing without end
// from growing this without limit, and keeps the FIRST thing it said.
//
// The first words are the diagnosis: a missing binary or a permission denial
// names itself immediately and then usually says nothing else. A sink that
// kept the tail instead would discard exactly the sentence worth having.
func TestHoldStderrIsBounded(t *testing.T) {
	var s holdStderr
	if _, err := s.Write([]byte("the reason\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	flood := strings.Repeat("x", maxHoldStderrTail*4)
	n, err := s.Write([]byte(flood))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	// The whole write must be reported as accepted even when nothing was
	// kept: a short write makes x/crypto/ssh treat a full buffer as a broken
	// stream and tear down a session that is working.
	if n != len(flood) {
		t.Errorf("Write reported %d of %d bytes; a short write here tears down a healthy session", n, len(flood))
	}
	got := s.String()
	if len(got) > maxHoldStderrTail {
		t.Errorf("kept %d bytes, want at most %d", len(got), maxHoldStderrTail)
	}
	if !strings.HasPrefix(got, "the reason") {
		t.Errorf("the first thing the command said was dropped: %q", got[:min(40, len(got))])
	}
}

// TestHoldStderrIsSafeUnderConcurrentWrites pins the lock. x/crypto/ssh
// copies the channel's stderr in from its own goroutine while the refusal
// branches read it, so this type is written to and read from at once by
// construction -- an unguarded buffer is a data race on a path only taken
// when something has already gone wrong.
func TestHoldStderrIsSafeUnderConcurrentWrites(t *testing.T) {
	var s holdStderr
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_, _ = s.Write([]byte("boom "))
				_ = s.String()
			}
		}()
	}
	wg.Wait()
}

// TestHoldStderrSuffixOnlySpeaksWhenItHasSomething keeps the quiet case
// quiet: an error that ended in a bare ": " would read as truncated.
func TestHoldStderrSuffixOnlySpeaksWhenItHasSomething(t *testing.T) {
	if got := holdStderrSuffix(""); got != "" {
		t.Errorf("holdStderrSuffix(\"\") = %q, want empty", got)
	}
	if got := holdStderrSuffix("No such file or directory"); got != ": No such file or directory" {
		t.Errorf("holdStderrSuffix = %q", got)
	}
}

// TestHoldAttachesStderrBeforeStarting guards the wiring, which no behavioural
// test in this package can reach: holdCommand needs a live SSH session.
//
// session.Stderr has to be set BEFORE session.Start -- afterwards the channel
// is already running and x/crypto/ssh has nowhere to put what it reads, so
// the remote command's explanation is discarded exactly as it was before this
// existed.
//
// MUTATION CAUGHT: moving the assignment below Start, or deleting it. Both
// compile, and every other test here passes, because the only symptom is an
// error message that says the command was silent when it was not.
func TestHoldAttachesStderrBeforeStarting(t *testing.T) {
	const file = "hold.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	attached, started := 0, 0
	ast.Inspect(f, func(n ast.Node) bool {
		if assign, ok := n.(*ast.AssignStmt); ok && len(assign.Lhs) == 1 {
			if sel, ok := assign.Lhs[0].(*ast.SelectorExpr); ok && sel.Sel.Name == "Stderr" {
				attached = fset.Position(assign.Pos()).Line
			}
		}
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Start" {
				started = fset.Position(call.Pos()).Line
			}
		}
		return true
	})

	switch {
	case attached == 0:
		t.Error("hold.go never assigns session.Stderr, so x/crypto/ssh discards the remote command's stderr and a refusal can only report that it said nothing")
	case started == 0:
		t.Fatal("hold.go no longer calls session.Start; this test needs rewriting")
	case attached > started:
		t.Errorf("session.Stderr is attached at line %d, after Start at line %d -- too late to catch anything the command says", attached, started)
	}
}

// Said is the whole of 4.22's fix on this side: without it the buffer the
// session keeps filling is unreachable the moment HoldCommand returns.
func TestHeldCommandReportsWhatTheCommandSaid(t *testing.T) {
	var buf holdStderr
	h := &heldCommand{said: &buf}
	if got := h.Said(); got != "" {
		t.Errorf("Said() = %q before anything was written, want empty", got)
	}
	if _, err := buf.Write([]byte("vmsync-bridge-helper: run lock lease expired\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got, want := h.Said(), "vmsync-bridge-helper: run lock lease expired"; got != want {
		t.Errorf("Said() = %q, want %q", got, want)
	}

	// A handle built without one must not panic: Exited is found by shape in
	// pkg/util and so is this, and a nil buffer is what a future constructor
	// that forgets it would leave.
	if got := (&heldCommand{}).Said(); got != "" {
		t.Errorf("Said() on a handle with no buffer = %q, want empty", got)
	}
}

// WHAT MAKES Said COMPLETE, asserted structurally because it is a property of
// the order two statements appear in and nothing observable distinguishes them
// here without a live SSH server.
//
// session.Wait drains the session's copy goroutines -- the stderr copier among
// them -- before it returns. So publishing the exit only AFTER Wait returns is
// what lets a caller read Said() on receiving from Exited() and see everything
// the command said. Publish it first and a consumer can win the race against
// the last line of the explanation, which is the line that usually matters.
//
// MUTATION CAUGHT: moving the send above the Wait, which compiles and leaves
// every other test here passing.
func TestTheExitIsPublishedOnlyAfterWaitReturns(t *testing.T) {
	const file = "hold.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	waited, published := 0, 0
	ast.Inspect(f, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Wait" {
				if x, ok := sel.X.(*ast.Ident); ok && x.Name == "session" {
					waited = fset.Position(call.Pos()).Line
				}
			}
		}
		if send, ok := n.(*ast.SendStmt); ok {
			if sel, ok := send.Chan.(*ast.SelectorExpr); ok && sel.Sel.Name == "exited" {
				published = fset.Position(send.Pos()).Line
			}
		}
		return true
	})

	switch {
	case waited == 0:
		t.Fatal("hold.go no longer calls session.Wait; this test needs rewriting")
	case published == 0:
		t.Fatal("hold.go no longer publishes the exit on h.exited; this test needs rewriting")
	case published < waited:
		t.Errorf("the exit is published at line %d, before session.Wait at line %d -- so a caller can read Said() before the stderr copier has finished and miss the end of the command's explanation", published, waited)
	}
}

// AND THE WIRING, which is the half that has caught this audit out five times
// now: a decision tested while nothing checks it is reached.
//
// Said() can be perfect and the handle can still be built with no buffer in it.
// There is no SSH server in this package's tests, so HoldCommand cannot be
// called to find out -- what can be checked is that the handle is given the
// SAME buffer the session was told to write into. Two separate `holdStderr`
// values, or a missing field, both compile and both leave Said() permanently
// empty while every other test here passes.
func TestTheHandleIsGivenTheBufferTheSessionWritesInto(t *testing.T) {
	const file = "hold.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	// Which variable the session's stderr is pointed at.
	attachedTo := ""
	// Which variable the handle was given.
	handedTo := ""
	handleLit := false

	ast.Inspect(f, func(n ast.Node) bool {
		if assign, ok := n.(*ast.AssignStmt); ok && len(assign.Lhs) == 1 && len(assign.Rhs) == 1 {
			if sel, ok := assign.Lhs[0].(*ast.SelectorExpr); ok && sel.Sel.Name == "Stderr" {
				if u, ok := assign.Rhs[0].(*ast.UnaryExpr); ok {
					if id, ok := u.X.(*ast.Ident); ok {
						attachedTo = id.Name
					}
				}
			}
		}
		if lit, ok := n.(*ast.CompositeLit); ok {
			if id, ok := lit.Type.(*ast.Ident); ok && id.Name == "heldCommand" {
				handleLit = true
				for _, el := range lit.Elts {
					kv, ok := el.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					key, ok := kv.Key.(*ast.Ident)
					if !ok || key.Name != "said" {
						continue
					}
					if u, ok := kv.Value.(*ast.UnaryExpr); ok {
						if id, ok := u.X.(*ast.Ident); ok {
							handedTo = id.Name
						}
					}
				}
			}
		}
		return true
	})

	switch {
	case !handleLit:
		t.Fatal("hold.go no longer builds a heldCommand literal; this test needs rewriting")
	case attachedTo == "":
		t.Fatal("hold.go does not point session.Stderr at a variable; TestHoldAttachesStderrBeforeStarting covers that and should have failed first")
	case handedTo == "":
		t.Error("the heldCommand is built without a `said` buffer, so Said() is permanently empty and a holder's reason for letting go is unreachable again -- which is the whole of 4.22")
	case handedTo != attachedTo:
		t.Errorf("the session writes stderr into %q but the handle was given %q, so Said() reports a buffer nothing fills", attachedTo, handedTo)
	}
}
