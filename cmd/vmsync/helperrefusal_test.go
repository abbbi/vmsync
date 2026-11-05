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

// The wording IS the feature here, so every clause that carries a decision
// has an assertion holding it down. The defect these were written for was a
// message that refused correctly and explained itself incompletely, which no
// exit-code test anywhere can see.
const (
	refusalDomain = "web01"
	refusalReason = "not found"
	refusalHelper = "/usr/local/bin/vmsync-bridge-helper"
	refusalHost   = "hyperB"
)

func helperRefusalText(t *testing.T) string {
	t.Helper()
	err := helperMissingRefusal(refusalDomain, refusalReason, refusalHelper, refusalHost)
	if err == nil {
		t.Fatal("helperMissingRefusal returned no error, so the run it was called from carries on with no usable helper and no integrity check")
	}
	return err.Error()
}

// The one the defect was. An operator who wants a sync rather than a digest
// has to be able to learn the flag's name from the message that stopped them,
// because that message is the only thing they are looking at.
func TestTheHelperRefusalNamesTheFlagThatGetsPastIt(t *testing.T) {
	got := helperRefusalText(t)
	if !strings.Contains(got, "-no-checksum") {
		t.Errorf("the refusal does not name -no-checksum, so an operator who only wanted a sync is sent to deploy a binary with no way to learn the flag exists -- which is how this message was read as \"vmsync cannot be used without the helper\":\n%s", got)
	}
}

// The other way on, which the old wording did carry. Losing it while keeping
// the flag would be the same defect inverted: an estate that wants the check
// told only how to turn it off.
func TestTheHelperRefusalStillNamesTheHelperToDeploy(t *testing.T) {
	got := helperRefusalText(t)
	for _, want := range []string{refusalHelper, refusalHost} {
		if !strings.Contains(got, want) {
			t.Errorf("the refusal does not say %q, so an estate that wants the integrity check is not told what to deploy or where:\n%s", want, got)
		}
	}
}

// Both costs, named where the flag is offered rather than two warnings later.
// Neither is about checksums, and an operator who reaches for -no-checksum to
// get past a digest check has not thereby agreed to an unsupervised run.
func TestTheHelperRefusalNamesWhatTheFlagCosts(t *testing.T) {
	got := helperRefusalText(t)
	for _, want := range []string{"unsupervised", "unleased"} {
		if !strings.Contains(got, want) {
			t.Errorf("the refusal offers -no-checksum without naming the %q cost, so the trade it asks the operator to make is not the one they can see:\n%s", want, got)
		}
	}
	// The cost the first two wordings both missed, and the only one that is
	// about correctness rather than convenience: only the leased hold rechecks
	// on every beat that the lock it holds still protects the path, and that
	// recheck is what the pre-commit and pre-redefine gates refuse on. An
	// unleased run cannot reach those gates, so a lock removed under it is
	// invisible and the commit goes through.
	if !strings.Contains(got, "removed under a live run") {
		t.Errorf("the refusal names the unleased lock without saying what the lease is FOR, so it reads as a two-hour availability window when it is also the run's only way to notice it has been dispossessed:\n%s", got)
	}
}

// What THIS run needed the helper for. The refusal is reachable only from the
// unbridged arm of the switch, so the integrity check is the whole of its own
// cause -- and a message that did not say so would be describing the binary
// instead of explaining the refusal.
func TestTheHelperRefusalSaysWhatThisRunNeededItFor(t *testing.T) {
	got := helperRefusalText(t)
	if !strings.Contains(got, "pre-commit integrity check") {
		t.Errorf("the refusal does not name the thing that required the helper on this run:\n%s", got)
	}
}

// The correction that prompted the rewrite. An operator handed a refusal whose
// only named remedy is -no-checksum will reasonably conclude the helper is a
// checksum feature, pass the flag, and be refused all over again by
// nbdbridge.CheckRemote -- because `case cfg.NoChecksum` is evaluated BEFORE
// `case bridgeCfg.Enabled()` in run()'s switch, so the flag never gets a
// bridged run past anything.
func TestTheHelperRefusalRulesTheFlagOutForBridging(t *testing.T) {
	got := helperRefusalText(t)
	for _, want := range []string{"-compress", "-netbuffer"} {
		if !strings.Contains(got, want) {
			t.Errorf("the refusal does not mention %s, so it reads as though the helper were a checksum feature and -no-checksum the way past every requirement:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "does not cover") {
		t.Errorf("the refusal names -compress/-netbuffer but does not say -no-checksum fails to cover them, which is the one thing an operator about to pass that flag needs:\n%s", got)
	}
}

// Version skew is the likelier way an estate meets this refusal than a host
// that never had the helper: upgrade vmsync and every target is one build
// behind, with no compatibility range to save it.
func TestTheHelperRefusalWarnsThatTheVersionMustMatchExactly(t *testing.T) {
	got := helperRefusalText(t)
	if !strings.Contains(got, "matching exactly") {
		t.Errorf("the refusal says to deploy a matching helper without saying that matching is exact, so an operator with a helper already deployed reads it as being about a missing file:\n%s", got)
	}
}

// What was refused, and why the probe said so. Without the reason, "deploy a
// matching one" cannot be acted on: missing and version-mismatched take
// different work, and the probe is the only thing that knows which.
func TestTheHelperRefusalSaysWhatItRefusedAndWhy(t *testing.T) {
	got := helperRefusalText(t)
	if !strings.Contains(got, refusalDomain) {
		t.Errorf("the refusal does not name the pair it refused, so a log holding several says nothing about which:\n%s", got)
	}
	if !strings.Contains(got, refusalReason) {
		t.Errorf("the refusal drops the probe's reason, so a missing helper and a version-mismatched one read identically although the work to fix them differs:\n%s", got)
	}
}

// -use-ssh without a bridge. The table is the whole claim: refused in exactly
// one combination, and permitted in every other, because a predicate that
// refused too widely would block the transport it exists to protect.
func TestUseSSHIsRefusedOnlyWhenThereIsNoBridgeToTunnel(t *testing.T) {
	cases := []struct {
		name   string
		cfg    syncConfig
		refuse bool
	}{
		{"-use-ssh alone", syncConfig{UseSSH: true}, true},
		{"-use-ssh with compression", syncConfig{UseSSH: true, Compress: "s2"}, false},
		{"-use-ssh with buffering", syncConfig{UseSSH: true, NetBuffer: "128k,1G"}, false},
		{"-use-ssh with both", syncConfig{UseSSH: true, Compress: "zstd", NetBuffer: "128k,1G"}, false},
		{"no -use-ssh at all", syncConfig{}, false},
		{"a bridge without -use-ssh", syncConfig{Compress: "s2"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := useSSHWithoutBridgeRefusal(c.cfg)
			if c.refuse && err == nil {
				t.Error("-use-ssh with no bridge flag was permitted, so the flag is a silent no-op again and the NBD traffic an operator believes is tunnelled crosses the network in the clear")
			}
			if !c.refuse && err != nil {
				t.Errorf("refused a combination that works: %v", err)
			}
		})
	}
}

// And the refusal has to say which flags make it go away, for the same reason
// every other refusal in this file does.
func TestTheUseSSHRefusalNamesTheFlagsThatSatisfyIt(t *testing.T) {
	err := useSSHWithoutBridgeRefusal(syncConfig{UseSSH: true})
	if err == nil {
		t.Fatal("no refusal for -use-ssh without a bridge")
	}
	for _, want := range []string{"-compress", "-netbuffer", "unencrypted"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q, so it states a rule without either the way to satisfy it or the reason it matters:\n%s", want, err.Error())
		}
	}
}

// THE WIRING for that one, which no test above can reach: main() exits the
// process on every path around it.
//
// The mutation it exists for compiles, keeps every test in this file green,
// and restores the exact defect: delete the call and useSSHWithoutBridgeRefusal
// becomes an unused function, which Go permits, while -use-ssh goes back to
// being silently ignored.
func TestMainConsultsTheUseSSHRefusal(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	var mainFn *ast.FuncDecl
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if ok && fn.Recv == nil && fn.Name != nil && fn.Name.Name == "main" {
			mainFn = fn
			break
		}
	}
	if mainFn == nil {
		t.Fatal("main() is not in main.go any more; this test guards what it does with the -use-ssh refusal and has to move with it")
	}

	var guard *ast.IfStmt
	ast.Inspect(mainFn.Body, func(n ast.Node) bool {
		is, ok := n.(*ast.IfStmt)
		if !ok || guard != nil || is.Init == nil {
			return true
		}
		ast.Inspect(is.Init, func(x ast.Node) bool {
			c, ok := x.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := c.Fun.(*ast.Ident); ok && id.Name == "useSSHWithoutBridgeRefusal" {
				guard = is
			}
			return true
		})
		return true
	})
	if guard == nil {
		t.Fatal("main() does not consult useSSHWithoutBridgeRefusal in an if-statement guard, so -use-ssh without a bridge is silently ignored again and an operator's NBD traffic crosses the network in the clear while they believe it is tunnelled")
	}

	// And the run must STOP. Logging the refusal and carrying on is the no-op
	// with a line of reassurance on top.
	exits := false
	ast.Inspect(guard.Body, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := c.Fun.(*ast.SelectorExpr)
		if ok && sel.Sel != nil && sel.Sel.Name == "Exit" {
			exits = true
		}
		return true
	})
	if !exits {
		t.Errorf("%s: main() builds the -use-ssh refusal but does not exit on it, so the run proceeds with the flag ignored and the reason merely recorded", fset.Position(guard.Pos()))
	}
}

// THE WIRING, not the wording.
//
// run() cannot be entered by a test: it needs two libvirt connections and an
// SSH client. So the shape is asserted the way refusalwiring_test.go and
// censuswiring_test.go do -- a weaker kind of test, and the one available.
//
// Two mutations it exists for, both of which compile and both of which keep
// every test above green:
//
//   - the refusal is RETURNED. Logging it and carrying on is the whole
//     "refused, not degraded" decision reversed, and the run then commits a
//     replica nothing compared against its source while reporting success.
//   - the message comes from helperMissingRefusal. An inline fmt.Errorf here
//     is how the old wording got away with naming one remedy for as long as
//     it did: there was nothing for a test to hold.
func TestRunReturnsTheHelperRefusalRatherThanWarningAboutIt(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	var runFn *ast.FuncDecl
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if ok && fn.Recv == nil && fn.Name != nil && fn.Name.Name == "run" {
			runFn = fn
			break
		}
	}
	if runFn == nil {
		t.Fatal("run() is not in main.go any more; this test guards what it does with the helper refusal and has to move with it")
	}

	calls := false
	returned := false
	ast.Inspect(runFn.Body, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if ok {
			if id, ok := c.Fun.(*ast.Ident); ok && id.Name == "helperMissingRefusal" {
				calls = true
			}
			return true
		}
		ret, ok := n.(*ast.ReturnStmt)
		if !ok {
			return true
		}
		for _, res := range ret.Results {
			ast.Inspect(res, func(x ast.Node) bool {
				c, ok := x.(*ast.CallExpr)
				if !ok {
					return true
				}
				if id, ok := c.Fun.(*ast.Ident); ok && id.Name == "helperMissingRefusal" {
					returned = true
				}
				return true
			})
		}
		return true
	})

	if !calls {
		t.Fatal("run() does not call helperMissingRefusal: either the gate on a target with no usable helper is gone -- and a run that asked for the integrity check gets a replica nobody compared against its source -- or the wording was inlined again, which is how it came to name only one of its two remedies")
	}
	if !returned {
		t.Error("run() builds the helper refusal but does not return it, so a target with no usable helper is merely remarked upon and the sync proceeds with no integrity check on a run that never asked to go without one")
	}
}
