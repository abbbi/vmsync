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

package util

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The shell hold, RUN rather than read.
//
// Every other test of this script greps its text, which can only say that the
// right words are present. What matters is what the shell does with them:
// whether VMSYNC-LOCK-ACQUIRED is withheld when the lock cannot be vouched
// for, whether the loop goes round, which marker comes out. So these start a
// real shell on the real script.
//
// The open-then-flock race is made DETERMINISTIC by shadowing flock(1) with a
// stub that replaces the lock file before reporting success. That is the same
// move pkg/util makes in Go, where RunLockIsAtPath is a separate function and
// the helper indirects it through a variable, "because the window it guards is
// between this function's own OpenFile and its own Flock, and nothing outside
// can be scheduled there". A test that tried to win the race by timing would
// be a test that passes when it loses.

func needFlock(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("no flock(1) here, so the shell hold cannot be exercised: " + err.Error())
	}
}

// lockPaths returns a scratch lock directory and the lock file inside it.
func lockPaths(t *testing.T) (dir, path string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "locks")
	return dir, RunLockPath(dir, "target-web01")
}

// stubFlock writes a flock(1) replacement onto a directory it returns for
// PATH, counting its calls in a file. body runs before it exits 0.
func stubFlock(t *testing.T, body string) (binDir, countFile string) {
	t.Helper()
	binDir = filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", binDir, err)
	}
	countFile = filepath.Join(binDir, "calls")
	script := "#!/bin/sh\n" +
		"n=$(cat " + ShQuote(countFile) + " 2>/dev/null || echo 0)\n" +
		"n=$((n+1))\n" +
		"printf '%s\\n' \"$n\" > " + ShQuote(countFile) + "\n" +
		body + "\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "flock"), []byte(script), 0o755); err != nil {
		t.Fatalf("write the flock stub: %v", err)
	}
	return binDir, countFile
}

func calls(t *testing.T, countFile string) int {
	t.Helper()
	b, err := os.ReadFile(countFile)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("call count %q: %v", b, err)
	}
	return n
}

// runHold runs the script to completion with stdin already at EOF, so the
// final `exec cat` returns at once instead of parking. Returns the output and
// the exit status.
func runHold(t *testing.T, path, dir string, extraPath string) (string, int) {
	t.Helper()
	// Bounded, because one of the things worth asserting is that the retry
	// loop ENDS. Without a deadline here an unbounded loop would spin until
	// the whole package timed out, which reports no verdict about the loop and
	// loses every other test's result with it.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", shellHoldScript(path, dir))
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	defer devNull.Close()
	cmd.Stdin = devNull
	if extraPath != "" {
		cmd.Env = append(os.Environ(), "PATH="+extraPath+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("the hold script never finished: the retry loop does not end, so a lock file being replaced leaves the shell spinning instead of refusing.\n%s", out)
	}
	status := 0
	if ee, ok := err.(*exec.ExitError); ok {
		status = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run the hold script: %v\n%s", err, out)
	}
	return string(out), status
}

// It takes the lock, says so, and excludes a second one while it holds it.
func TestShellHoldTakesTheLockAndExcludesASecond(t *testing.T) {
	needFlock(t)
	dir, path := lockPaths(t)

	// The first holder has to stay alive, so its stdin stays open: the script
	// ends in `exec cat`, which is what keeps the shell -- and therefore fd 9
	// and the lock -- in place.
	first := exec.Command("sh", "-c", shellHoldScript(path, dir))
	stdin, err := first.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := first.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := first.Start(); err != nil {
		t.Fatalf("start the first holder: %v", err)
	}
	// Reaped in exactly one place, and armed BEFORE anything can call Fatalf:
	// started after the assertions below, the cleanup's wait would stall its
	// full timeout on every failure and then call Wait() concurrently with
	// this goroutine, which is a race on the same *exec.Cmd.
	waited := make(chan error, 1)
	go func() { waited <- first.Wait() }()
	t.Cleanup(func() {
		_ = stdin.Close()
		select {
		case <-waited:
		case <-time.After(5 * time.Second):
			_ = first.Process.Kill()
			<-waited
		}
	})

	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("the first holder said nothing: %v", err)
	}
	if strings.TrimSpace(line) != RemoteLockReady {
		t.Fatalf("the first holder said %q, want %q", strings.TrimSpace(line), RemoteLockReady)
	}

	// And now the lock is really held, which is the only thing that makes the
	// announcement worth anything.
	out, status := runHold(t, path, dir, "")
	if !strings.Contains(out, RemoteLockBusy) {
		t.Errorf("a second hold against a held lock said %q, want %s", out, RemoteLockBusy)
	}
	if status != RemoteLockExitBusy {
		t.Errorf("second hold exited %d, want %d", status, RemoteLockExitBusy)
	}
	if strings.Contains(out, RemoteLockReady) {
		t.Errorf("the second hold announced the lock it did not get:\n%s", out)
	}
}

// THE FINDING. The lock file is replaced between the open and the lock on
// every attempt, so every flock lands on an orphaned inode that excludes
// nobody. READY must never be printed, and the refusal must come after a
// bounded number of tries rather than spinning.
func TestShellHoldWithholdsReadyWhileTheFileKeepsBeingReplaced(t *testing.T) {
	// No needFlock: this case SHADOWS flock(1) entirely, so a host without
	// util-linux can still run it -- and it is the headline regression test
	// for 4.21, so gating it on a binary it does not use would skip the one
	// assertion that matters on exactly the hosts the shell hold exists for.
	dir, path := lockPaths(t)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Replaces the file the shell has just opened, every single time, which is
	// what a tmpfiles.d rule clearing the directory looks like from here.
	binDir, countFile := stubFlock(t, "rm -f "+ShQuote(path)+"\n: >> "+ShQuote(path))

	out, status := runHold(t, path, dir, binDir)

	if strings.Contains(out, RemoteLockReady) {
		t.Errorf("announced the lock while holding an orphaned inode:\n%s\nA second vmsync would then take the file that replaced it and both would run against one replica.", out)
	}
	if !strings.Contains(out, RemoteLockNoCreate) {
		t.Errorf("output %q does not name %s", out, RemoteLockNoCreate)
	}
	if status != RemoteLockExitNoCreate {
		t.Errorf("exited %d, want %d", status, RemoteLockExitNoCreate)
	}
	if n := calls(t, countFile); n != shellHoldAttempts {
		t.Errorf("flock was called %d time(s); the loop is bounded at %d, so a file being replaced forever must cost exactly that and then stop", n, shellHoldAttempts)
	}
}

// And the loop is a RETRY, not just a refusal: one replacement costs one turn
// and the lock is then taken on the file that is actually there.
func TestShellHoldRetriesOnceAndThenAnnounces(t *testing.T) {
	// No needFlock, for the reason above: flock is shadowed here.
	dir, path := lockPaths(t)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	binDir, countFile := stubFlock(t,
		"if [ \"$n\" -eq 1 ]; then rm -f "+ShQuote(path)+"; : >> "+ShQuote(path)+"; fi")

	out, status := runHold(t, path, dir, binDir)

	if !strings.Contains(out, RemoteLockReady) {
		t.Errorf("never announced the lock even though the second attempt was clean:\n%s", out)
	}
	if status != 0 {
		t.Errorf("exited %d, want 0", status)
	}
	if n := calls(t, countFile); n != 2 {
		t.Errorf("flock was called %d time(s), want 2: one attempt whose file was replaced, one that stood", n)
	}
}

// A host with no flock(1) is a misconfiguration, and has to read as one rather
// than as contention.
func TestShellHoldReportsNoFlockWhenThereIsNone(t *testing.T) {
	dir, path := lockPaths(t)
	cmd := exec.Command("sh", "-c", shellHoldScript(path, dir))
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	defer devNull.Close()
	cmd.Stdin = devNull
	// An empty PATH, so `command -v flock` finds nothing. /usr/bin is not on
	// it either, which is the point: this is the "cannot lock at all" case.
	cmd.Env = append(os.Environ(), "PATH=")
	out, err := cmd.CombinedOutput()
	status := 0
	if ee, ok := err.(*exec.ExitError); ok {
		status = ee.ExitCode()
	}
	if !strings.Contains(string(out), RemoteLockNoFlock) {
		t.Errorf("output %q does not name %s", out, RemoteLockNoFlock)
	}
	if status != RemoteLockExitNoFlock {
		t.Errorf("exited %d, want %d", status, RemoteLockExitNoFlock)
	}
}

// The capability probes, and the one thing they must never do.
//
// Structural, and deliberately so: neither condition they guard -- a shell with
// no `test -ef`, a host whose /proc does not resolve descriptor paths -- can be
// produced on a Linux host with any shell a target might have. All of dash,
// bash, busybox ash and Alpine's busybox were measured and all four have both.
// So what is asserted is the shape, and specifically the property whose absence
// was a real defect here.
//
// THE REGRESSION THIS GUARDS. The first version of the probe asked the
// same-file question about the LOCK FILE. That made one observation carry two
// meanings -- "this host cannot compare files" and "the lock file was just
// replaced" -- and it reported the permanent one for both, so a concurrent
// `-break-target-lock` turned a retryable race into a refusal that sent the
// operator to inspect a working /proc. The probes must therefore ask only about
// things that cannot be unlinked underneath them: the root directory, and a
// descriptor path.
//
// An earlier version of this test also contained an assertion that could not
// fail -- it compared two strings the test itself had just built from one
// template, rather than comparing either against the script. Everything below
// is asserted against the script.
func TestShellHoldProbesOnlyWhatCannotBeUnlinked(t *testing.T) {
	_, path := lockPaths(t)
	script := shellHoldScript(path, "/run/vmsync-locks")
	exit := strconv.Itoa(RemoteLockExitNoSameFile)

	// Each probe, with its whole failure branch: deleting the `|| { ... }`
	// would leave the probe running and its answer ignored, which is the
	// mutation a bare "is the expression present" assertion cannot see.
	for _, want := range []string{
		"[ / -ef / ] || { echo " + RemoteLockNoSameFile + "; exit " + exit + "; }",
		"[ -e /proc/$$/fd/8 ] || { echo " + RemoteLockNoSameFile + "; exit " + exit + "; }",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the script is missing a capability probe, or its refusal:\n  want: %s\n%s", want, script)
		}
	}

	// THE REGRESSION: no probe may ask the same-file question about the lock
	// file, because a replacement of it is retryable and the probes are not.
	if strings.Contains(script, ShQuote(path)+" -ef /proc/$$/fd/8") {
		t.Errorf("a capability probe compares the LOCK FILE against a descriptor, so a lock file replaced in that window -- which this tree's own `-break-target-lock` does -- reads as a host that cannot compare files, and is refused instead of retried:\n%s", script)
	}

	// And the loop's comparison is assembled from exactly the two ingredients
	// the probes vouched for.
	check := "[ " + ShQuote(path) + " -ef /proc/$$/fd/9 ]"
	if !strings.Contains(script, check) {
		t.Errorf("the script does not compare fd 9 against the lock path:\n%s", script)
	}

	// Ordering: both probes before the loop, and the comparison between the
	// lock and the announcement. Before the flock it would prove nothing;
	// after the echo it would be too late, because a driver that reads READY
	// starts writing a replica.
	iEf := strings.Index(script, "[ / -ef / ]")
	iProc := strings.Index(script, "[ -e /proc/$$/fd/8 ]")
	iFlock := strings.Index(script, "flock -n 9")
	iCheck := strings.Index(script, check)
	iReady := strings.Index(script, "echo "+RemoteLockReady)
	if !(iEf < iFlock && iProc < iFlock) {
		t.Errorf("a capability probe runs after the first lock attempt (-ef at %d, /proc at %d, flock at %d)", iEf, iProc, iFlock)
	}
	if !(iFlock < iCheck && iCheck < iReady) {
		t.Errorf("the comparison is not between the lock and the announcement (flock at %d, check at %d, ready at %d):\n%s", iFlock, iCheck, iReady, script)
	}
}

// The retry has to end holding the real lock on the real file, and until now
// nothing proved it did.
//
// The two stubbed cases above replace flock(1) outright, so no lock is ever
// taken in them: they prove the loop goes round and that READY is withheld, but
// a loop that announced the lock while holding nothing at all would satisfy
// both. This one stubs only the FIRST attempt -- replacing the file under it --
// and then hands every later attempt to the real flock(1). So the shell ends up
// where production would: past one lost attempt, holding a genuine lock on the
// file the path names. A second hold then has to be excluded by it, which is
// the only evidence that the announcement was worth anything.
func TestShellHoldRetriesOntoARealLock(t *testing.T) {
	needFlock(t)
	real, err := exec.LookPath("flock")
	if err != nil {
		t.Skipf("no flock(1): %v", err)
	}
	dir, path := lockPaths(t)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// `exec` the real flock, so fd 9 -- and therefore the lock -- belongs to
	// the same process the shell handed it to.
	binDir, countFile := stubFlock(t,
		"if [ \"$n\" -eq 1 ]; then rm -f "+ShQuote(path)+"; : >> "+ShQuote(path)+"; exit 0; fi\n"+
			"exec "+ShQuote(real)+" \"$@\"")

	first := exec.Command("sh", "-c", shellHoldScript(path, dir))
	first.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	stdin, err := first.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := first.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := first.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	waited := make(chan error, 1)
	go func() { waited <- first.Wait() }()
	t.Cleanup(func() {
		_ = stdin.Close()
		select {
		case <-waited:
		case <-time.After(5 * time.Second):
			_ = first.Process.Kill()
			<-waited
		}
	})

	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("the holder said nothing: %v", err)
	}
	if strings.TrimSpace(line) != RemoteLockReady {
		t.Fatalf("the holder said %q, want %q", strings.TrimSpace(line), RemoteLockReady)
	}
	if n := calls(t, countFile); n != 2 {
		t.Errorf("flock was called %d time(s), want 2: one attempt whose file was replaced, one real one", n)
	}

	// The lock it ended on is a real one, held on the file the path names.
	out, status := runHold(t, path, dir, "")
	if !strings.Contains(out, RemoteLockBusy) || status != RemoteLockExitBusy {
		t.Errorf("a second hold was not excluded after the retry: output %q, exit %d -- so the loop announced a lock it does not actually hold on the file the path names", out, status)
	}
}
