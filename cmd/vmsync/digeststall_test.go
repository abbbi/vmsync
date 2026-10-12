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

	"vmsync/pkg/trace"
)

// TestTargetDigestLadderHoldsTogether pins the three durations that decide
// what the target digest wait does against each other rather than against
// their values, the way pkg/remotessh pins its keepalive trio against the
// lock lease.
//
// THE LADDER. The helper beats once per trace.ProgressInterval while it
// hashes -- progress when a block completed, liveness when none did. The
// grace is how long vmsync listens to nothing before SAYING so; the bound is
// how long before it STOPS. Each has to outlive the one below it:
//
//   - grace <= interval would remark about a peer beating normally, one line
//     a minute per disk for the whole of a healthy pass: the double-logging
//     targetDigestQuietGrace exists to prevent.
//   - bound <= grace would fail the run without ever having said it was
//     waiting, so the first an operator hears of a stall is the failure.
//   - a bound that is not a whole number of intervals trips at a moment
//     unrelated to how many beats were missed, which is the only thing it is
//     measuring.
//
// MUTATION CAUGHT: any of the three edited alone. The tempting one is
// shrinking the bound towards its neighbours at targetLockTimeout (30s) and
// the 15s skew probe, which look like peers and are not: those bound a
// control-plane round trip, this bounds silence inside a data pass.
func TestTargetDigestLadderHoldsTogether(t *testing.T) {
	if targetDigestQuietGrace <= trace.ProgressInterval {
		t.Errorf("grace %s does not outlive one beat (%s): a healthy pass would be remarked on every tick",
			targetDigestQuietGrace, trace.ProgressInterval)
	}
	if targetDigestStallBound <= targetDigestQuietGrace {
		t.Errorf("bound %s does not outlive the grace %s: a stall would fail before it was ever reported as a wait",
			targetDigestStallBound, targetDigestQuietGrace)
	}
	if targetDigestStallBound%trace.ProgressInterval != 0 {
		t.Errorf("bound %s is not a whole number of beats (%s), so what trips it is not a count of missed beats",
			targetDigestStallBound, trace.ProgressInterval)
	}
	if got := targetDigestStallBound / trace.ProgressInterval; got != 3 {
		t.Errorf("bound is %d beats, want 3 -- one missed beat is a loaded host, three is the peer being gone, which is the call pkg/remotessh's keepaliveStrikes already makes", got)
	}
}

// TestTargetDigestWaitRunsOnADerivedContext is the half of the bound that
// reading constants cannot check.
//
// THE PROPERTY. askTargetDigests cancels a context to abandon a stalled
// digest, and that context must be one it derived for this single call --
// never the run's own. What happens immediately after a trip is the
// target-side export being killed and the run lock released, both over the
// same SSH connection and both through Client.Run, which answers a cancelled
// context before it even opens a session. A bound that cancelled the run's
// context would trade an audible hang for a silent leak: a live export on
// the DR host with an abandoned helper still attached, and a lock nobody
// released.
//
// WHY READ RATHER THAN EXECUTED: askTargetDigests is a closure inside run(),
// which opens libvirt connections to two hypervisors. What this walk cannot
// see is whether the derived context is the one the watcher cancels -- it
// checks the call, not the goroutine.
//
// MUTATION CAUGHT: passing ctx to RunWithInputProgress, which is exactly the
// simplification the extra variable invites once its purpose is forgotten.
func TestTargetDigestWaitRunsOnADerivedContext(t *testing.T) {
	const file = "main.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	calls := 0
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "RunWithInputProgress" {
			return true
		}
		calls++
		if len(call.Args) == 0 {
			t.Errorf("%s: RunWithInputProgress called with no context at all", fset.Position(call.Pos()))
			return true
		}
		arg, ok := call.Args[0].(*ast.Ident)
		if !ok || arg.Name != "digestCtx" {
			t.Errorf("%s: the target digest runs on %s rather than the context askTargetDigests derived for it; cancelling the run's own context takes the export teardown and the lock release down with the wait",
				fset.Position(call.Pos()), exprName(call.Args[0]))
		}
		return true
	})
	if calls != 1 {
		t.Errorf("found %d RunWithInputProgress call sites, want 1 -- a second would need a bound of its own, and this test would not notice it missing", calls)
	}
}

// exprName renders an expression for an error message, falling back to a
// description when it is not a plain identifier.
func exprName(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return "a non-identifier expression"
}

// TestVerifyVerdictDoesNotCallAFailedCompareAMismatch guards the sentence an
// operator acts on.
//
// Every phase-two failure used to be announced as "disk vda does not match",
// including the ones where no comparison took place: a target that stopped
// reporting, a plan that could not be built, a source read that failed. The
// metric was right all along (verification_state 2, "could not check") and
// the sentence was wrong -- and the sentence is what gets read at 3am. "Does
// not match" means reinitialise the replica. "Could not be compared" means
// go and look at the target host, which is where the fault actually is.
//
// A source-level check because runVerify needs two hypervisors and a
// replica. It asserts the wording is chosen by isVerifyMismatch, the
// classifier that already decides the metric, so the two cannot drift apart
// again.
func TestVerifyVerdictDoesNotCallAFailedCompareAMismatch(t *testing.T) {
	const file = "main.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	defaulted, chosen := false, false
	ast.Inspect(f, func(n ast.Node) bool {
		if assign, ok := n.(*ast.AssignStmt); ok && len(assign.Lhs) == 1 && len(assign.Rhs) == 1 {
			if lhs, ok := assign.Lhs[0].(*ast.Ident); ok && lhs.Name == "verdict" {
				if lit, ok := assign.Rhs[0].(*ast.BasicLit); ok && strings.Contains(lit.Value, "could not be compared") {
					defaulted = true
				}
			}
		}
		stmt, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		call, ok := stmt.Cond.(*ast.CallExpr)
		if !ok {
			return true
		}
		fn, ok := call.Fun.(*ast.Ident)
		if !ok || fn.Name != "isVerifyMismatch" {
			return true
		}
		for _, s := range stmt.Body.List {
			assign, ok := s.(*ast.AssignStmt)
			if !ok || len(assign.Lhs) != 1 {
				continue
			}
			if id, ok := assign.Lhs[0].(*ast.Ident); ok && id.Name == "verdict" {
				chosen = true
			}
		}
		return true
	})
	if !defaulted {
		t.Error("the verify verdict no longer defaults to \"could not be compared\": a stalled target, an unbuildable plan and a failed source read would again be announced as a data mismatch, which is the sentence that gets a healthy replica reinitialised")
	}
	if !chosen {
		t.Error("the verdict wording is not chosen by isVerifyMismatch, so the sentence an operator reads and the verification_state a dashboard shows can disagree again")
	}
}
