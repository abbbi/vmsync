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
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"testing"
	"time"
)

func skipWithoutProc(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("/proc progress accounting is Linux-only")
	}
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc on this host")
	}
}

// THE REASON THE SUM IS OVER THE GROUP, proven against real processes
// rather than asserted in a comment.
//
// The helper runs `/bin/sh -c`, and sh forks for any compound command, so
// the pid it holds is a shell asleep in wait(). Reading that pid alone
// reports a command going flat out as perfectly still, and every compound
// command would be killed the moment its stall bound elapsed. This starts
// exactly that shape and requires the group sum to move while the leader's
// own counters do not.
func TestGroupProgressSeesTheWorkerUnderACompoundShell(t *testing.T) {
	skipWithoutProc(t)

	// Compound (`; true`) so sh forks instead of exec'ing in place, which
	// is what makes the leader a shell rather than the worker.
	cmd := exec.Command("/bin/sh", "-c", "dd if=/dev/zero of=/dev/null bs=1M count=400000 >/dev/null 2>&1; true")
	startInOwnProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Skipf("could not start the probe command: %v", err)
	}
	pgid := cmd.Process.Pid
	defer func() {
		_ = signalProcessGroup(pgid, "KILL")
		_, _ = cmd.Process.Wait()
	}()

	// Let the shell fork and the worker get going.
	time.Sleep(500 * time.Millisecond)

	leaderFirst, ok := procProgress(pgid)
	if !ok {
		t.Skip("the probe command exited before it could be sampled")
	}
	groupFirst, ok := groupProgress(pgid, "")
	if !ok {
		t.Fatal("groupProgress could read nothing for a running process group")
	}

	time.Sleep(1500 * time.Millisecond)

	leaderSecond, _ := procProgress(pgid)
	groupSecond, ok := groupProgress(pgid, "")
	if !ok {
		t.Skip("the probe command exited mid-test")
	}

	if !groupSecond.moved(groupFirst) {
		t.Errorf("the group sum did not move while a worker was running flat out: %+v then %+v",
			groupFirst, groupSecond)
	}
	// Not an assertion about sh's implementation so much as the fact that
	// makes this test worth having: if the leader DID move, the group sum
	// would be unnecessary and this test would be proving nothing.
	if leaderSecond.moved(leaderFirst) {
		t.Logf("NOTE: the leader's own counters moved (%+v then %+v) -- this sh exec'd rather than forked, so this run did not exercise the case the group sum exists for",
			leaderFirst, leaderSecond)
	}
}

// A process that is genuinely doing nothing must read as still, or the
// bound can never fire at all. The mirror of the test above.
func TestGroupProgressIsStillForAnIdleProcess(t *testing.T) {
	skipWithoutProc(t)

	cmd := exec.Command("/bin/sh", "-c", "sleep 30; true")
	startInOwnProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Skipf("could not start the probe command: %v", err)
	}
	pgid := cmd.Process.Pid
	defer func() {
		_ = signalProcessGroup(pgid, "KILL")
		_, _ = cmd.Process.Wait()
	}()

	time.Sleep(500 * time.Millisecond)
	first, ok := groupProgress(pgid, "")
	if !ok {
		t.Skip("the probe command exited before it could be sampled")
	}
	time.Sleep(1500 * time.Millisecond)
	second, ok := groupProgress(pgid, "")
	if !ok {
		t.Skip("the probe command exited mid-test")
	}
	if second.moved(first) {
		t.Errorf("a sleeping process group reported progress: %+v then %+v", first, second)
	}
}

// procPgid must survive an executable name containing spaces and
// parentheses, which is the classic way a /proc/pid/stat parser reads the
// wrong field -- and reading the wrong field here means the group sum
// silently collects the wrong processes.
func TestProcPgidParsesAnAwkwardCommName(t *testing.T) {
	skipWithoutProc(t)

	dir := t.TempDir()
	// The name is the point: ") (" inside it defeats a naive split, and a
	// parser anchored on the FIRST ')' reads the state letter as the pgid.
	awkward := dir + "/sh (a) (b) c"
	data, err := os.ReadFile("/bin/sh")
	if err != nil {
		t.Skipf("cannot read /bin/sh to copy it: %v", err)
	}
	if err := os.WriteFile(awkward, data, 0o755); err != nil {
		t.Skipf("cannot create the awkwardly named binary: %v", err)
	}

	cmd := exec.Command(awkward, "-c", "sleep 30; true")
	startInOwnProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Skipf("could not start the awkwardly named probe: %v", err)
	}
	pid := cmd.Process.Pid
	defer func() {
		_ = signalProcessGroup(pid, "KILL")
		_, _ = cmd.Process.Wait()
	}()
	time.Sleep(300 * time.Millisecond)

	// Setpgid makes the child its own group leader, so its pgid is its pid.
	if got := procPgid(pid); got != pid {
		t.Errorf("procPgid(%d) = %d for a process whose comm contains spaces and parentheses, want %d",
			pid, got, pid)
	}
}

// matchesArgv is how a daemon that left the process group is found again,
// so it must match on a distinctive argument and must never match on an
// empty pattern -- which would adopt every process on the host.
func TestMatchesArgv(t *testing.T) {
	skipWithoutProc(t)

	marker := "vmsync-argv-marker-" + strconv.Itoa(os.Getpid())
	// The marker rides in a trailing comment so the command stays VALID:
	// an argument sleep rejects makes the shell exit at once, and a
	// zombie's /proc/pid/cmdline reads empty, so the test would fail
	// against a process that no longer has a command line rather than
	// against matchesArgv.
	cmd := exec.Command("/bin/sh", "-c", "sleep 30; true # "+marker)
	startInOwnProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Skipf("could not start the probe command: %v", err)
	}
	pid := cmd.Process.Pid
	defer func() {
		_ = signalProcessGroup(pid, "KILL")
		_, _ = cmd.Process.Wait()
	}()
	time.Sleep(300 * time.Millisecond)

	if !matchesArgv(pid, marker) {
		t.Errorf("matchesArgv did not find %q in the command line of pid %d", marker, pid)
	}
	if matchesArgv(pid, "") {
		t.Error("an empty pattern matched, which would adopt every process on the host")
	}
	if matchesArgv(pid, "a-string-no-process-has-"+marker) {
		t.Error("matchesArgv matched a string that is not in the command line")
	}
}
