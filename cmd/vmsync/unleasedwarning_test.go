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

	"vmsync/pkg/util"
)

const (
	testHelperPath = "/usr/local/bin/vmsync-bridge-helper"
	testTargetHost = "hyper-b"
)

// fields renders the warning's key/value pairs into one searchable string, the
// way a log line reads to the operator who finds it.
func warningText(t *testing.T) string {
	t.Helper()
	msg, kv := unleasedLockWarning("web01", testTargetHost, testHelperPath,
		"no vmsync-bridge-helper path is configured for this run")
	if len(kv)%2 != 0 {
		t.Fatalf("the warning has %d key/value items, which is not pairs: %v", len(kv), kv)
	}
	var b strings.Builder
	b.WriteString(msg)
	for i := 0; i < len(kv); i += 2 {
		b.WriteString("\n")
		b.WriteString(kv[i].(string))
		b.WriteString("=")
		if s, ok := kv[i+1].(string); ok {
			b.WriteString(s)
		}
	}
	return b.String()
}

// THE FINDING. The warning named -break-target-lock as "the way past a stale
// lock", and that verb refuses every lock this warning is about.
func TestTheUnleasedWarningDoesNotNameARemedyThatRefuses(t *testing.T) {
	got := warningText(t)

	// The old claim, in the shapes it could come back in.
	for _, forbidden := range []string{
		"is the way past",
		"until then vmsync -break-target-lock",
	} {
		if strings.Contains(got, forbidden) {
			t.Errorf("the warning offers -break-target-lock as the remedy, and that verb refuses every unleased lock:\n%s", got)
			break
		}
	}

	// Naming the verb is fine -- an operator will reach for it anyway. Saying
	// it refuses is the point.
	if strings.Contains(got, "-break-target-lock") && !strings.Contains(got, "REFUSE") {
		t.Errorf("the warning names -break-target-lock without saying it will refuse this lock:\n%s", got)
	}

	// And it has to say where the verb runs, because it is not where this
	// warning is printed: the verb requires a local target URI.
	if !strings.Contains(got, "ON the target") {
		t.Errorf("the warning does not say that -break-target-lock has to run on the target rather than on this host:\n%s", got)
	}
}

// The ways past, in the order that keeps a replica safe.
//
// The wait comes first deliberately. This is the §4.10 lesson applied here: an
// earlier correction of mine in this audit issued a bare "find and end whatever
// holds the lock" and dropped the two-writer clause, which is the composition
// the fix existed to remove, moved one layer earlier.
func TestTheUnleasedWarningOffersTheWaitBeforeTheKill(t *testing.T) {
	got := warningText(t)

	wait := strings.Index(got, "Wait for")
	end := strings.Index(got, "end the process holding it")
	switch {
	case wait < 0:
		t.Errorf("the warning never offers waiting for sshd to expire the session:\n%s", got)
	case end < 0:
		t.Errorf("the warning never names the lever for when waiting is not acceptable:\n%s", got)
	case end < wait:
		t.Errorf("the warning tells an operator to end the session before it tells them they can wait, which is the dangerous order:\n%s", got)
	}

	// The clause that makes ending it safe to read: the lock is the only thing
	// keeping two writers off the replica.
	if !strings.Contains(got, "two writers") {
		t.Errorf("the warning tells an operator to end a session without saying what ending a LIVE one costs:\n%s", got)
	}
	if !strings.Contains(got, "certain") {
		t.Errorf("the warning does not condition ending the session on being sure the run is gone:\n%s", got)
	}
}

// The warning and the refusal cannot drift, because both render the same text.
//
// They did drift: the warning promised a verb that the refusal then declined to
// perform, and the refusal's own advice -- which was correct -- was reachable
// only by running the verb.
func TestTheWarningAndTheRefusalShareTheirAdvice(t *testing.T) {
	warning := warningText(t)
	refusal := unattributedLockRefusal("web01",
		`the lock for "web01" records no holder, so there is no evidence that whoever holds it has stopped`,
		testHelperPath).Error()

	// Each from the shared renderer, from its own point of view: the warning
	// runs on the driver and talks about "the target"; the refusal runs on the
	// target and talks about "this host".
	if want := util.UnleasedLockEscape("the target", util.RunLockPath(runLockDir, targetLockKey("web01")), ""); !strings.Contains(warning, want) {
		t.Errorf("the warning does not use util.UnleasedLockEscape, so it can drift from the refusal again.\nwant: %s\ngot:\n%s", want, warning)
	}
	if want := util.UnleasedLockEscape("this host", util.RunLockPath(runLockDir, targetLockKey("web01")), testHelperPath); !strings.Contains(refusal, want) {
		t.Errorf("the refusal does not use util.UnleasedLockEscape.\nwant: %s\ngot:\n%s", want, refusal)
	}

	// The refusal still has to be identifiable as one.
	if !strings.Contains(refusal, "refusing to break the run lock") {
		t.Errorf("the refusal no longer says it is refusing: %s", refusal)
	}
	if !strings.Contains(refusal, "ErrLockUnattributed") && !strings.Contains(refusal, "names no holder") && !strings.Contains(refusal, "records no") {
		t.Errorf("the refusal does not say that nothing identifies the holder: %s", refusal)
	}
}

// The durable fix has to be actionable, which means naming the path this run
// was actually configured with rather than the binary in the abstract.
func TestTheUnleasedWarningNamesTheHelperPathItWasGiven(t *testing.T) {
	got := warningText(t)
	if !strings.Contains(got, testHelperPath) {
		t.Errorf("the warning does not name the helper path this run expects, so an operator cannot tell where to put the binary:\n%s", got)
	}
	if !strings.Contains(got, testTargetHost) {
		t.Errorf("the warning does not name the host it is about:\n%s", got)
	}
}

// UnleasedLockEscape mentions the helper only when it has a path to name, so
// the warning -- whose own remedy field already names it -- does not say it
// twice.
func TestTheEscapeMentionsTheHelperOnlyWhenItCanNameAPath(t *testing.T) {
	const lock = "/run/vmsync-locks/web01.lock"
	with := util.UnleasedLockEscape("this host", lock, testHelperPath)
	without := util.UnleasedLockEscape("this host", lock, "")

	if !strings.Contains(with, testHelperPath) {
		t.Errorf("given a path, the escape does not name it: %s", with)
	}
	if strings.Contains(without, "vmsync-bridge-helper at") {
		t.Errorf("given no path, the escape still points at one: %s", without)
	}
	// The ways past are the same either way; only the durable fix is added.
	if !strings.HasPrefix(with, without) {
		t.Errorf("the two renderings do not share their advice:\n with: %s\n without: %s", with, without)
	}
}

// THE HINT HAS TO NAME A COMMAND THAT FINDS THE HOLDER, and the obvious one
// does not.
//
// Measured on a real parked hold: the process holding the lock has the argv
// `cat`, because flock(1) locks the descriptor it inherited and exits while the
// lock lives on the shell's open file description, and the shell then execs its
// reader. `ps -ef | grep -- 'flock -n 9'` -- which docs/RUNBOOK.md instructed an
// operator to run -- finds nothing at all. `fuser -v <lockfile>` and
// `lsof <lockfile>` both name the pid.
func TestTheEscapeNamesACommandThatActuallyFindsTheHolder(t *testing.T) {
	const lock = "/run/vmsync-locks/web01.lock"
	got := util.UnleasedLockEscape("this host", lock, "")

	if !strings.Contains(got, "fuser") && !strings.Contains(got, "lsof") {
		t.Errorf("the advice does not name a command that asks the kernel who holds the file:\n%s", got)
	}
	if !strings.Contains(got, lock) {
		t.Errorf("the advice does not name the lock file, so the command cannot be run as written:\n%s", got)
	}
	// Looking for flock is the trap: it exits as soon as it has locked.
	if strings.Contains(got, "grep -- 'flock") || strings.Contains(got, "grep flock") {
		t.Errorf("the advice sends an operator looking for a flock process, which has already exited:\n%s", got)
	}
	// And it has to prepare them for the argv they will actually see.
	if !strings.Contains(got, "`cat`") {
		t.Errorf("the advice does not warn that the holder's argv is just `cat`, so an operator who finds it will think they have the wrong process:\n%s", got)
	}
}

// The wait is still honest about the case where it never ends.
func TestTheEscapeSaysWhenWaitingWillNotWork(t *testing.T) {
	got := util.UnleasedLockEscape("this host", "/run/vmsync-locks/web01.lock", "")
	if !strings.Contains(got, "TCPKeepAlive no") {
		t.Errorf("the advice offers waiting without saying that under `TCPKeepAlive no` the session never expires, which makes waiting the wrong answer there:\n%s", got)
	}
}

// AND THE CALL SITE, which is the sixth time in this audit that a decision was
// tested and nothing checked it was reached.
//
// Every assertion above is about what unleasedLockWarning RETURNS. All of them
// pass if main.go never calls it, or calls it and drops the result on the
// floor -- and the thing being guarded is a warning an operator reads during a
// disaster, so its absence is silent by construction.
//
// Asserted on the source because the alternative is a sync: run() takes a
// libvirt connection, two SSH clients and a replica.
func TestTheWarningIsActuallyPrinted(t *testing.T) {
	const file = "main.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	built := 0   // where unleasedLockWarning's result is taken
	printed := 0 // where trace.Warning is called with it
	var msgName, fieldsName string

	ast.Inspect(f, func(n ast.Node) bool {
		if assign, ok := n.(*ast.AssignStmt); ok && len(assign.Rhs) == 1 && len(assign.Lhs) == 2 {
			if call, ok := assign.Rhs[0].(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "unleasedLockWarning" {
					built = fset.Position(assign.Pos()).Line
					if a, ok := assign.Lhs[0].(*ast.Ident); ok {
						msgName = a.Name
					}
					if b, ok := assign.Lhs[1].(*ast.Ident); ok {
						fieldsName = b.Name
					}
				}
			}
		}
		if call, ok := n.(*ast.CallExpr); ok {
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Warning" || len(call.Args) != 2 || !call.Ellipsis.IsValid() {
				return true
			}
			first, ok1 := call.Args[0].(*ast.Ident)
			second, ok2 := call.Args[1].(*ast.Ident)
			if ok1 && ok2 && msgName != "" && first.Name == msgName && second.Name == fieldsName {
				printed = fset.Position(call.Pos()).Line
			}
		}
		return true
	})

	switch {
	case built == 0:
		t.Error("main.go never calls unleasedLockWarning, so the warning every unleased run is supposed to print is not printed by anything")
	case printed == 0:
		t.Errorf("unleasedLockWarning's result is taken at line %d but never passed to trace.Warning, so the text is built and dropped", built)
	case printed < built:
		t.Errorf("trace.Warning is called at line %d with values assigned at line %d", printed, built)
	}
}
