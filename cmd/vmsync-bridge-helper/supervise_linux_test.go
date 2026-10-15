//go:build linux

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
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Linux-only, and that is the point of the file rather than an apology for
// it. Everything here needs a real process group, a real kill(2) with a
// negative pid and a real /proc -- the three things the state machine's
// clockless tests deliberately fake, and the three things that decide whether
// this mode works on the only platform it ships to.

// alive reports whether pid still exists. Signal 0 performs the permission
// checks and nothing else, which is how you ask the kernel the question.
func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// goneWithin waits for pid to disappear, so the assertion does not race the
// kernel reaping it.
func goneWithin(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return !alive(pid)
}

// TestSuperviseKillsTheWholeProcessGroup is the one assertion no portable
// test can make, and the reason startInOwnProcessGroup and the negative pid
// exist at all.
//
// Every command vmsync runs on a target is a shell line, so the supervised
// child is /bin/sh and the thing that matters -- a qemu-nbd blocked opening
// the replica image -- is its child. Signalling the shell alone reaps the
// shell and leaves that process running, unsupervised and unsignalled, which
// is the exact state this mode exists to prevent.
//
// MUTATION CAUGHT: signalling cmd.Process instead of the group (kill(pid)
// rather than kill(-pid)), or dropping Setpgid so there is no group of its
// own to signal. Both leave the backgrounded process alive here, and both
// pass every other test in this package.
func TestSuperviseKillsTheWholeProcessGroup(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "grandchild.pid")
	var out bytes.Buffer
	var progress safeProgress

	res := runSupervise(superviseConfig{
		MaxRuntime: 300 * time.Millisecond,
		Grace:      300 * time.Millisecond,
		Label:      "a command that backgrounds a child",
		Interval:   40 * time.Millisecond,
		Progress:   &progress,
	}, "sleep 300 & echo $! > "+pidfile+"; sleep 300", &out)

	if res.Outcome != superviseTimeout {
		t.Fatalf("outcome = %q, want %q (progress: %s)", res.Outcome, superviseTimeout, progress.String())
	}

	raw, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatalf("the command never recorded its background child: %v", err)
	}
	grandchild, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("unreadable pid %q: %v", raw, err)
	}
	if !goneWithin(grandchild, 2*time.Second) {
		// Clean up after a FAILING build, by pid and then by group. The pid
		// first, deliberately: every way this test fails is a way in which
		// the group signal did not reach that process -- because the group
		// was never created, or because nothing ever signalled it -- so a
		// cleanup that only signalled the group would leak a sleeping
		// process onto the host on exactly the runs that fail.
		_ = syscall.Kill(grandchild, syscall.SIGKILL)
		_ = syscall.Kill(-grandchild, syscall.SIGKILL)
		t.Errorf("the backgrounded process %d survived the signal, so it went to the shell alone and not to its process group -- on a target that means a qemu-nbd left holding the replica image with nobody able to signal it", grandchild)
	}
}

// TestSuperviseEscalatesToKillAgainstARealChildThatIgnoresTerm pairs with the
// state machine's scripted version: here the SIGTERM is genuinely delivered
// and genuinely ignored, by a shell that trapped it.
func TestSuperviseEscalatesToKillAgainstARealChildThatIgnoresTerm(t *testing.T) {
	var out bytes.Buffer
	var progress safeProgress
	res := runSupervise(superviseConfig{
		MaxRuntime: 200 * time.Millisecond,
		Grace:      300 * time.Millisecond,
		Label:      "a command that ignores SIGTERM",
		Interval:   40 * time.Millisecond,
		Progress:   &progress,
	}, "trap '' TERM; sleep 300", &out)

	if got := strings.Join(res.Signals, ","); got != "TERM,KILL" {
		t.Errorf("signals = %q, want TERM,KILL -- a child that ignores the term must be killed", got)
	}
	if !res.Reaped {
		t.Errorf("a killable child was reported unreaped (progress: %s)", progress.String())
	}
	if res.ExitCode != 124 {
		t.Errorf("exit hint = %d, want 124", res.ExitCode)
	}
}

// TestSuperviseLetsARealChildHonourTheTerm is the other half: no SIGKILL at
// all when the command goes quietly.
func TestSuperviseLetsARealChildHonourTheTerm(t *testing.T) {
	var out bytes.Buffer
	res := runSupervise(superviseConfig{
		MaxRuntime: 200 * time.Millisecond,
		Grace:      2 * time.Second,
		Label:      "a command that exits on SIGTERM",
		Interval:   40 * time.Millisecond,
		Progress:   &bytes.Buffer{},
	}, "trap 'exit 0' TERM; sleep 300", &out)

	if got := strings.Join(res.Signals, ","); got != "TERM" {
		t.Errorf("signals = %q, want TERM alone", got)
	}
	if !res.Reaped {
		t.Error("a child that exited inside its grace was reported unreaped")
	}
}

// TestSuperviseReportsARealChildsOutputAndStatus keeps the ordinary case
// honest: the command's own bytes reach the caller unchanged, and its exit
// status is its own.
//
// This is what vmsync builds every target-command error from, so anything
// this process added to the stream -- a beat, a verdict, a prefix -- would
// end up quoted inside that error as if the command had said it.
func TestSuperviseReportsARealChildsOutputAndStatus(t *testing.T) {
	var out bytes.Buffer
	var progress safeProgress
	res := runSupervise(superviseConfig{
		Label:    "a command that prints and fails",
		Interval: time.Hour,
		Progress: &progress,
	}, "echo to-stdout; echo to-stderr >&2; exit 3", &out)

	if res.Outcome != superviseCompleted {
		t.Fatalf("outcome = %q, want %q", res.Outcome, superviseCompleted)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit = %d, want the command's own 3", res.ExitCode)
	}
	if got := out.String(); !strings.Contains(got, "to-stdout") || !strings.Contains(got, "to-stderr") {
		t.Errorf("the command's combined output did not reach the caller verbatim: %q", got)
	}
	if got := out.String(); strings.Contains(got, "supervise") {
		t.Errorf("this process wrote its own words into the command's output stream: %q", got)
	}
	if got := progress.String(); !strings.Contains(got, "outcome=completed") {
		t.Errorf("no verdict on the progress stream: %q", got)
	}
}

// TestSuperviseBeatReadsARealProcState proves the field that turns "the
// target is slow" into a diagnosis. A sleeping child reads as S; what must
// never happen is an empty state on a host that has /proc, because then the
// beat says only that this process is alive -- which nobody was asking.
func TestSuperviseBeatReadsARealProcState(t *testing.T) {
	var out bytes.Buffer
	var progress safeProgress
	go func() {
		_ = runSupervise(superviseConfig{
			MaxRuntime: 600 * time.Millisecond,
			Grace:      300 * time.Millisecond,
			Label:      "a sleeping command",
			Interval:   30 * time.Millisecond,
			Progress:   &progress,
		}, "sleep 300", &out)
	}()

	deadline := time.After(5 * time.Second)
	for {
		if line := progress.String(); strings.Contains(line, "in state S") || strings.Contains(line, "in state R") {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("no liveness line carrying a real /proc state: %q", progress.String())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// TestProcStateReadsThisProcess is parseProcState's counterpart against the
// real file rather than a fixture, so a change to how the path is built
// cannot pass on the fixture alone.
func TestProcStateReadsThisProcess(t *testing.T) {
	if got := procState(os.Getpid()); got != "R" && got != "S" {
		t.Errorf("procState(self) = %q, want R or S", got)
	}
	// A pid that cannot exist must read as unknown rather than as something
	// reassuring: the beat prints the state only when it has one.
	if got := procState(-1); got != "" {
		t.Errorf("procState(-1) = %q, want empty", got)
	}
}
