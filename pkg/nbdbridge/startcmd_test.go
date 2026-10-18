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

package nbdbridge

import (
	"strings"
	"testing"
)

// THE HELPER RECORDS ITS OWN PID; THIS COMMAND MUST NOT.
//
// It used to end in `& echo $! > pidFile`, which reads as equivalent and is
// not. The redirection runs AFTER the fork, in a shell belonging to the SSH
// session, while setsid has already carried the helper out of that session.
// A driver killed in that window takes the recording shell with it and leaves
// a helper running under no name at all -- invisible to BuildStopCommand, to
// killOrphanedRemoteBridge and to any sweep, because every one of them starts
// by reading this file. Exactly one such helper was found on a production
// target, still listening two hours after the run that started it was killed,
// with no pid file anywhere naming it.
//
// The pre-existing test for this command asserts only
// strings.Contains(got, pidFile), which passes for `-pidfile <path>` just as
// well as for `echo $! > <path>` -- so it cannot tell which of the two is
// happening, and did not notice when this changed. That is what this test is
// for.
func TestBuildStartCommandMakesTheHelperRecordItsOwnPID(t *testing.T) {
	const (
		bridgePort = 20200
		realPort   = 20100
		pidFile    = "/run/vmsync-bridge/vmsync-bridge-20200.pid"
		logFile    = "/run/vmsync-bridge/vmsync-bridge-20200.log"
	)
	got := BuildStartCommand(Config{HelperPath: "/usr/bin/vmsync-bridge-helper"}, bridgePort, realPort, pidFile, logFile)

	// The helper is told where to record itself, as an adjacent
	// flag-and-value pair rather than merely somewhere in the command.
	//
	// Derived from the same two primitives the builder uses rather than
	// written out: the helper's argv is quoted by quoteArgs and then the
	// whole of it is quoted again by util.ShQuote for `sh -c`, so the pair
	// appears doubly quoted. Hardcoding that spelling would make this test
	// fail the next time either primitive changes, for no reason.
	escaped := strings.ReplaceAll(quoteArgs("-pidfile", pidFile), "'", `'\''`)
	if !strings.Contains(got, escaped) {
		t.Errorf("command does not pass -pidfile %s to the helper.\nwant fragment: %s\ngot: %s", pidFile, escaped, got)
	}

	// And the shell does not do it. Checked as the specific construct rather
	// than by looking for "echo", which the marker echoes legitimately use.
	if strings.Contains(got, "echo $!") {
		t.Errorf("the launching shell still records the pid, which it cannot be relied on to survive long enough to do:\n%s", got)
	}
	if strings.Contains(got, "$!") {
		t.Errorf("the command still refers to $!, the pid of the backgrounded setsid rather than of the helper:\n%s", got)
	}

	// Still detached, which is the property that makes all of this necessary
	// and must not be "fixed" by attaching it to the SSH session instead: the
	// helper has to outlive the channel that starts it.
	if !strings.HasPrefix(got, "setsid sh -c ") {
		t.Errorf("the helper is no longer detached:\n%s", got)
	}
	if !strings.HasSuffix(strings.TrimSpace(got), "&") {
		t.Errorf("the command no longer backgrounds the helper, so StartRemote would block on it:\n%s", got)
	}
}

// The pid file path reaches the helper quoted, because it is interpolated
// into a shell command inside another shell command.
func TestBuildStartCommandQuotesThePidFilePath(t *testing.T) {
	const awkward = "/run/vmsync-bridge/vm's bridge.pid"
	got := BuildStartCommand(Config{HelperPath: "/usr/bin/vmsync-bridge-helper"}, 20200, 20100, awkward, "/run/vmsync-bridge/x.log")

	// Raw, the apostrophe would end the quoting of the helper command and the
	// space would split the argument.
	if strings.Contains(got, awkward) {
		t.Errorf("the pid file path is interpolated raw:\n%s", got)
	}
	if !strings.Contains(got, "-pidfile") {
		t.Fatalf("no -pidfile argument at all:\n%s", got)
	}
}
