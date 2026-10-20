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
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// proc is a process started for a test, with a channel that closes when it has
// actually been reaped. Polling kill(pid, 0) is NOT enough to decide a child
// died: a SIGKILLed child of this test binary stays a zombie until someone
// waits for it, and kill(pid, 0) succeeds against a zombie. A test that polled
// for ESRCH would therefore report a successful kill as a survival.
type proc struct {
	pid  int
	done chan struct{}
}

// spawn starts a long-lived process with marker as one of its own argv words.
//
// The shape matters. These commands check /proc/<pid>/cmdline for a WHOLE argv
// word equal to the pidfile path, because that is how the real processes carry
// it: vmsync-bridge-helper is started with "-pidfile <path>" and qemu-nbd with
// "--pid-file <path>", so the path is its own NUL-separated word. Passing it as
// an operand to sh -c reproduces that exactly -- it lands in $0 and stays a
// separate word -- whereas interpolating it into the script text would make it
// part of one long word and match nothing.
//
// Cleanup kills by pid AND by group: a failing group case is precisely one
// where the group signal did not land, so group-only cleanup would leak these
// loops onto the host running the suite.
func spawn(t *testing.T, marker string, ownGroup bool) *proc {
	t.Helper()
	cmd := exec.Command("sh", "-c", "while :; do sleep 0.2; done", marker)
	if ownGroup {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start a test process: %v", err)
	}
	p := &proc{pid: cmd.Process.Pid, done: make(chan struct{})}
	go func() { _, _ = cmd.Process.Wait(); close(p.done) }()
	t.Cleanup(func() {
		_ = syscall.Kill(p.pid, syscall.SIGKILL)
		_ = syscall.Kill(-p.pid, syscall.SIGKILL)
		<-p.done
	})

	// Wait until it is in /proc with its argv readable, so a check that runs
	// immediately is not racing the exec.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile("/proc/" + strconv.Itoa(p.pid) + "/cmdline"); err == nil && len(b) > 0 {
			return p
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("test process %d never appeared in /proc", p.pid)
	return nil
}

func (p *proc) died(d time.Duration) bool {
	select {
	case <-p.done:
		return true
	case <-time.After(d):
		return false
	}
}

func (p *proc) stillRunning() bool {
	select {
	case <-p.done:
		return false
	default:
		return syscall.Kill(p.pid, 0) == nil
	}
}

// runCmd runs the command and requires it to be SILENT as well as successful.
//
// Silence is part of the contract, not tidiness. Callers hand this command's
// combined output to the operator, so anything it writes is read as a fault in
// the teardown. It is also where a real defect hid: with the redirections in
// the other order, `tr ... < /proc/<gone>/cmdline 2>/dev/null` let the shell's
// own "No such file or directory" through, on the most common path there is --
// a pidfile naming a process that has already exited. A runCmd that only
// checked the exit status could not see it.
func runCmd(t *testing.T, sh string) {
	t.Helper()
	out, err := exec.Command("sh", "-c", sh).CombinedOutput()
	if err != nil {
		t.Fatalf("running the kill command failed: %v\n%s", err, out)
	}
	if len(out) > 0 {
		t.Errorf("the command was not silent, and its output reaches the operator as a teardown fault:\n%s\ncommand: %s", out, sh)
	}
}

// vk builds the command for a test target. Comm is "sh" because spawn starts
// its targets as `sh -c ...`, which is what /proc/<pid>/comm then holds; the
// real callers pass "qemu-nbd" and the installed helper's own binary name.
func vk(pidFile string, group bool, remove ...string) string {
	return VerifiedKill{PidFile: pidFile, Comm: "sh", Group: group, Remove: remove}.Command()
}

func writePidFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write pidfile: %v", err)
	}
}

func mustBeGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("%s survived: %v -- a leftover pidfile is what makes a replay dangerous and the next reclaim a no-op", filepath.Base(path), err)
	}
}

// It does kill what the pidfile names, when that really is the process. The
// check is worthless if it also refuses the true case.
func TestVerifiedKillKillsTheRecordedProcess(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "vmsync-qemu-nbd-web01-vda.pid")
	p := spawn(t, pidFile, false)
	writePidFile(t, pidFile, strconv.Itoa(p.pid)+"\n")

	runCmd(t, vk(pidFile, false))

	if !p.died(5 * time.Second) {
		t.Error("the process the pidfile named is still running, so a teardown that must release the image did nothing")
	}
	mustBeGone(t, pidFile)
}

// THE DEFECT, as a live process.
//
// `kill -9 $(cat pidfile) || true` cannot tell this from the case above: kill -9
// succeeds against any valid pid, recycled or not, and || true hides it. Here a
// live, unrelated process holds the number the pidfile claims -- which is what
// happens when the recorded process exited and the OS handed its number on.
func TestVerifiedKillLeavesARecycledPidAlone(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "vmsync-qemu-nbd-web01-vda.pid")

	stranger := spawn(t, "an-unrelated-process", false)
	writePidFile(t, pidFile, strconv.Itoa(stranger.pid)+"\n")

	runCmd(t, vk(pidFile, false))

	if !stranger.stillRunning() {
		t.Error("an unrelated process holding the recorded pid was SIGKILLed -- as root on a hypervisor, that is the whole finding")
	}
	// The files still go: a pidfile naming something that is not ours is
	// exactly what should not be left behind.
	mustBeGone(t, pidFile)
}

// The group variant takes the children with it, which is why the bridge helper
// needs it -- and why the check in front of it matters more, not less: a group
// kill against a recycled pid takes out a whole group of strangers.
func TestVerifiedKillGroupTakesTheChildrenToo(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "vmsync-bridge-web01-20901.pid")
	childFile := filepath.Join(dir, "child")

	script := "sh -c 'while :; do sleep 0.2; done' & echo $! > " +
		ShQuote(childFile) + "; while :; do sleep 0.2; done"
	cmd := exec.Command("sh", "-c", script, pidFile)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	leader := &proc{pid: cmd.Process.Pid, done: make(chan struct{})}
	go func() { _, _ = cmd.Process.Wait(); close(leader.done) }()
	t.Cleanup(func() {
		_ = syscall.Kill(leader.pid, syscall.SIGKILL)
		_ = syscall.Kill(-leader.pid, syscall.SIGKILL)
		<-leader.done
	})

	var child int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && child == 0 {
		if b, err := os.ReadFile(childFile); err == nil {
			child, _ = strconv.Atoi(string(bytesTrimSpace(b)))
		}
		if child == 0 {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if child == 0 {
		t.Fatal("the child never recorded its pid")
	}
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })

	writePidFile(t, pidFile, strconv.Itoa(leader.pid)+"\n")
	runCmd(t, vk(pidFile, true))

	if !leader.died(5 * time.Second) {
		t.Error("the group leader survived")
	}
	// The child is a grandchild of this test, so once the leader is gone init
	// reaps it and kill(pid, 0) is a sound liveness test for it.
	gone := false
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(child, 0) != nil {
			gone = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !gone {
		t.Error("a child of the group survived, so a relay keeps holding the port and the image after its helper was stopped")
	}
	mustBeGone(t, pidFile)
}

func bytesTrimSpace(b []byte) []byte {
	i, j := 0, len(b)
	for i < j && (b[i] == ' ' || b[i] == '\n' || b[i] == '\r' || b[i] == '\t') {
		i++
	}
	for j > i && (b[j-1] == ' ' || b[j-1] == '\n' || b[j-1] == '\r' || b[j-1] == '\t') {
		j--
	}
	return b[i:j]
}

// Everything a pidfile can hold that is not a pid we can prove is ours.
//
// These are asserted against a RECORDER, not a victim: the command runs in a
// shell where `kill` is a function that appends its arguments to a file instead
// of signalling anything. Two reasons, and the second is the important one.
//
//  1. It asserts intent rather than an outcome -- that no kill was ATTEMPTED,
//     which is stronger than observing that one particular process happened to
//     survive.
//  2. Several of these inputs are catastrophic if the guard is ever removed:
//     "-1\n" would become `kill -9 -1` and take out every process of the
//     invoking user -- as root, the machine -- and "0\n" would become
//     `kill -9 0`, killing the caller's own process group. Mutation testing
//     this function means deliberately deleting that guard, so the case that
//     proves the guard exists must not be the case that destroys the host it
//     runs on. Recorded, a lost guard shows up as a failed assertion naming
//     the exact command that would have run.
func TestVerifiedKillNeverSignalsWhatThePidfileCannotProve(t *testing.T) {
	for _, tc := range []struct{ name, content, why string }{
		{"empty", "", "an empty pidfile is a crashed or racing writer"},
		{"whitespace", "   \n", "likewise, and ${p:-x} must not let it through"},
		{"not a number", "notanumber\n", "garbage, or a different format entirely"},
		{"two numbers", "123 456\n", "two writers, or a half-overwritten file"},
		{"negative one", "-1\n", "kill -9 -1 is every process this user owns"},
		{"zero", "0\n", "kill -9 0 is this shell's own process group"},
		{"pid one", "1\n", "a truncated write naming init"},
		{"a number with a suffix", "123x\n", "a partial line from a longer file"},
		{"leading plus", "+42\n", "accepted by some readers, not a bare pid"},
		{"leading space", " 42\n", "a formatted write, not the bare number this expects"},
		{"a huge number", "99999999999999999999\n", "beyond any pid, and beyond strtol"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			pidFile := filepath.Join(dir, "x.pid")
			rec := filepath.Join(dir, "signals")
			writePidFile(t, pidFile, tc.content)

			stub := "kill() { printf '%s\\n' \"kill $*\" >> " + ShQuote(rec) + "; }; "
			runCmd(t, stub+vk(pidFile, false))

			if b, err := os.ReadFile(rec); err == nil && len(b) > 0 {
				t.Errorf("a pidfile holding %q led to: %s(%s)", tc.content, b, tc.why)
			}
			mustBeGone(t, pidFile)
		})
	}
}

// A live pid whose argv does not carry the pidfile path: the same refusal, but
// recorded, so it asserts that nothing was signalled rather than that something
// happened to live. The group form too, since that is the dangerous one.
func TestVerifiedKillAttemptsNoSignalForAnUnprovenPid(t *testing.T) {
	for _, group := range []bool{false, true} {
		name := "single"
		if group {
			name = "group"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			pidFile := filepath.Join(dir, "x.pid")
			rec := filepath.Join(dir, "signals")
			stranger := spawn(t, "an-unrelated-process", false)
			writePidFile(t, pidFile, strconv.Itoa(stranger.pid)+"\n")

			stub := "kill() { printf '%s\\n' \"kill $*\" >> " + ShQuote(rec) + "; }; "
			runCmd(t, stub+vk(pidFile, group))

			if b, err := os.ReadFile(rec); err == nil && len(b) > 0 {
				t.Errorf("a live pid whose argv does not carry %s was signalled: %s", filepath.Base(pidFile), b)
			}
			if !stranger.stillRunning() {
				t.Error("and it died")
			}
			mustBeGone(t, pidFile)
		})
	}
}

// A pidfile that is not there at all is the ordinary case after a clean stop,
// and must not be an error: these commands are replayed from the
// interrupt-cleanup path after the inline teardown has already run. The extra
// files go too, and go whether or not anything was killed.
func TestVerifiedKillIsSafeToReplay(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "gone.pid")
	logFile := filepath.Join(dir, "gone.log")
	writePidFile(t, logFile, "some output\n")

	cmd := vk(pidFile, true, logFile)
	runCmd(t, cmd)
	runCmd(t, cmd)

	mustBeGone(t, pidFile)
	mustBeGone(t, logFile)
}

// Paths with shell metacharacters are quoted on every use, including inside the
// /proc check and the grep pattern. A domain name is not arbitrary, but the
// pidfile path is built from one and this must not be the place that assumes.
func TestVerifiedKillQuotesAwkwardPaths(t *testing.T) {
	dir := t.TempDir()
	for _, base := range []string{"a b.pid", "it's.pid", "a$x.pid", "a;rm -rf x.pid", "a`x`.pid", "a*.pid"} {
		t.Run(base, func(t *testing.T) {
			pidFile := filepath.Join(dir, base)
			p := spawn(t, pidFile, false)
			writePidFile(t, pidFile, strconv.Itoa(p.pid)+"\n")

			runCmd(t, vk(pidFile, false))

			if !p.died(5 * time.Second) {
				t.Errorf("a process whose argv carries %q was not killed, so the path did not survive quoting", base)
			}
			mustBeGone(t, pidFile)
		})
	}
}

// THE WHOLE-WORD MATCH IS LOAD-BEARING, and this is the process that proves it.
//
// grep -qxF requires an argv word equal to the path. Dropping the -x -- a
// substring match -- looks harmless and is not, because the stop command ITSELF
// is `sh -c "p=$(cat /run/....pid); ..."`, one long argv word with the path
// inside it. Two concurrent runs, or an operator's own shell, put such a word in
// /proc. A substring match would find that shell, decide the pid in the pidfile
// is "ours", and SIGKILL it -- or, in the group form, its whole process group,
// which for an interactive shell is the session.
func TestVerifiedKillDoesNotMatchAPathInsideALongerArgvWord(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "vmsync-qemu-nbd-web01-vda.pid")
	rec := filepath.Join(dir, "signals")

	// The shape of a sibling stop command: the path, inside a longer word.
	word := "p=$(cat " + pidFile + " 2>/dev/null); echo \"$p\""
	stranger := spawn(t, word, false)
	writePidFile(t, pidFile, strconv.Itoa(stranger.pid)+"\n")

	stub := "kill() { printf '%s\\n' \"kill $*\" >> " + ShQuote(rec) + "; }; "
	runCmd(t, stub+vk(pidFile, false))

	if b, err := os.ReadFile(rec); err == nil && len(b) > 0 {
		t.Errorf("a process that merely MENTIONS the pidfile path was signalled: %s", b)
	}
	if !stranger.stillRunning() {
		t.Error("and it died -- that process shape is another vmsync's own stop command")
	}
	mustBeGone(t, pidFile)
}

// It waits for the process to actually go, and the wait is bounded.
//
// A caller that goes on to commit into the image, or rm it on a -reinit, must
// not race a process that still has it open -- SIGKILL returns as soon as the
// signal is queued, not when the address space is torn down. Asserted with the
// same kill recorder, which makes it deterministic: the stub reports the target
// as alive forever, so the loop runs to its own bound instead of depending on
// how fast a real process dies. Removing the loop drops the elapsed time to
// nothing and the kill -0 count to zero; removing the "-lt 50" bound hangs,
// which the context deadline reports rather than the suite timing out.
func TestVerifiedKillWaitsForTheProcessToGoAndIsBounded(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "vmsync-qemu-nbd-web01-vda.pid")
	rec := filepath.Join(dir, "signals")
	p := spawn(t, pidFile, false)
	writePidFile(t, pidFile, strconv.Itoa(p.pid)+"\n")

	// Records and always SUCCEEDS, so "kill -0" says the target is still
	// there and the loop has to decide for itself when to stop.
	stub := "kill() { printf '%s\\n' \"kill $*\" >> " + ShQuote(rec) + "; return 0; }; "

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	start := time.Now()
	out, err := exec.CommandContext(ctx, "sh", "-c", stub+vk(pidFile, false)).CombinedOutput()
	elapsed := time.Since(start)
	if ctx.Err() != nil {
		t.Fatalf("the wait never ended: still running after %s, so nothing bounds it -- a teardown that cannot finish blocks the run it is cleaning up after", elapsed)
	}
	if err != nil {
		t.Fatalf("command failed: %v\n%s", err, out)
	}

	b, readErr := os.ReadFile(rec)
	if readErr != nil {
		t.Fatalf("nothing was recorded at all: %v", readErr)
	}
	if n := strings.Count(string(b), "kill -9 "); n != 1 {
		t.Errorf("want exactly one kill -9, got %d:\n%s", n, b)
	}
	// 50 iterations is the implementation's bound; anything in that order
	// proves the loop ran rather than the signal being fired and forgotten.
	if n := strings.Count(string(b), "kill -0 "); n < 5 {
		t.Errorf("only %d liveness checks after the kill, so it did not wait for the process to go:\n%s", n, b)
	}
	if elapsed < time.Second {
		t.Errorf("returned after %s against a target that never dies, so it did not wait", elapsed)
	}

	// AND IT HAS TO FIT INSIDE ITS CALLER'S BUDGET, which is the other half of
	// the bound and the half that is easy to lose.
	//
	// Every stop command registered for the interrupt path runs under its own
	// five-second context in cmd/vmsync's cleanupTargetNBD, inside a polling
	// loop that keeps a five-second window open for commands workers register
	// late. A wait that can itself reach five seconds spends the whole of
	// both: the command is cancelled before its `rm -f` runs, so the pidfile
	// survives to mislead the next run, and the straggler window is gone --
	// which is how a late-registered export ends up with nothing left to stop
	// it. Four seconds is the ceiling that leaves the rest of the command room
	// to finish and still be reported.
	if elapsed > 4*time.Second {
		t.Errorf("took %s against a target that never dies, which does not fit inside the five-second context cleanupTargetNBD gives each stop command: the command is cancelled before it removes the pidfile, and the polling window for late-registered exports is spent", elapsed)
	}
}

// shellsPresent returns the distinct POSIX shells installed here, resolved so
// the usual /bin/sh -> bash symlink is not probed twice.
func shellsPresent(t *testing.T) map[string][]string {
	t.Helper()
	// resolved path -> the argv prefix that makes it read a command.
	found := map[string][]string{}
	for _, c := range []struct{ path, sub string }{
		{"/bin/sh", ""}, {"/bin/bash", ""}, {"/bin/dash", ""},
		{"/bin/ash", ""}, {"/bin/ksh", ""}, {"/usr/bin/dash", ""},
		// busybox is a multi-call binary: `busybox -c cmd` is not a shell
		// invocation, it is busybox complaining about an unknown applet. The
		// shell is `busybox sh -c cmd`. The first version of this list had
		// bare busybox in it, which simply never ran here -- Fedora has no
		// busybox -- and would have failed for the wrong reason on a host
		// that does.
		{"/usr/bin/busybox", "sh"}, {"/bin/busybox", "sh"},
	} {
		st, err := os.Stat(c.path)
		if err != nil || st.IsDir() {
			continue
		}
		real, err := filepath.EvalSymlinks(c.path)
		if err != nil {
			real = c.path
		}
		if _, seen := found[real]; seen {
			continue
		}
		argv := []string{c.path}
		if c.sub != "" {
			argv = append(argv, c.sub)
		}
		found[real] = append(argv, "-c")
	}
	return found
}

// THE GROUP FORM HAS TO WORK IN THE SHELL THE TARGET HAS, which is not the
// shell this suite runs in. The command goes over ssh to a hypervisor and is
// interpreted there.
//
// This is where a `--` separator was caught: `kill -9 -- "-$p"` is rejected
// outright by dash's kill builtin, which has no `--` and reads it as the pid,
// so the group is never signalled and the helper leaks. bash accepts it, so a
// suite on a Fedora or RHEL box sees nothing wrong.
//
// Every shell installed here is exercised, and the ones that are NOT installed
// are named in the log, because a green run on a box with only bash proves only
// that bash is happy. The text assertion below covers what this cannot.
func TestVerifiedKillGroupFormWorksInEveryShellPresent(t *testing.T) {
	shells := shellsPresent(t)
	if len(shells) == 0 {
		t.Fatal("no POSIX shell found at all, which cannot be right")
	}

	var names []string
	for real, argv := range shells {
		names = append(names, strings.Join(argv[:len(argv)-1], " ")+" -> "+real)
	}
	sort.Strings(names)
	t.Logf("exercising the group kill under: %s", strings.Join(names, ", "))
	if _, ok := shells["/usr/bin/dash"]; !ok {
		if _, ok := shells["/bin/dash"]; !ok {
			t.Log("NOTE: dash is not installed here, so this test cannot " +
				"prove the command works on a Debian or Ubuntu target. " +
				"TestVerifiedKillGroupUsesABareNegativePid is what guards that.")
		}
	}

	for real, argv := range shells {
		as := strings.Join(argv[:len(argv)-1], " ")
		argv := argv
		t.Run(filepath.Base(real), func(t *testing.T) {
			dir := t.TempDir()
			pidFile := filepath.Join(dir, "vmsync-bridge-web01-20901.pid")
			childFile := filepath.Join(dir, "child")

			script := "sh -c 'while :; do sleep 0.2; done' & echo $! > " +
				ShQuote(childFile) + "; while :; do sleep 0.2; done"
			cmd := exec.Command("sh", "-c", script, pidFile)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			if err := cmd.Start(); err != nil {
				t.Fatalf("start: %v", err)
			}
			leader := &proc{pid: cmd.Process.Pid, done: make(chan struct{})}
			go func() { _, _ = cmd.Process.Wait(); close(leader.done) }()
			t.Cleanup(func() {
				_ = syscall.Kill(leader.pid, syscall.SIGKILL)
				_ = syscall.Kill(-leader.pid, syscall.SIGKILL)
				<-leader.done
			})

			var child int
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) && child == 0 {
				if b, err := os.ReadFile(childFile); err == nil {
					child, _ = strconv.Atoi(string(bytesTrimSpace(b)))
				}
				if child == 0 {
					time.Sleep(20 * time.Millisecond)
				}
			}
			if child == 0 {
				t.Fatal("the child never recorded its pid")
			}
			t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })

			writePidFile(t, pidFile, strconv.Itoa(leader.pid)+"\n")

			// Interpreted by THIS shell, which is the whole point.
			out, err := exec.Command(argv[0], append(argv[1:], vk(pidFile, true))...).CombinedOutput()
			if err != nil {
				t.Fatalf("%s rejected the command: %v\n%s", as, err, out)
			}
			if len(out) > 0 {
				t.Errorf("%s wrote something, which a clean teardown should not: %s", as, out)
			}

			if !leader.died(5 * time.Second) {
				t.Errorf("under %s the group leader survived", as)
			}
			gone := false
			deadline = time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if syscall.Kill(child, 0) != nil {
					gone = true
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			if !gone {
				t.Errorf("under %s a child of the group survived -- on a target with this shell, stopping the helper leaves its relays running", as)
			}
		})
	}
}

// A property this host cannot exhibit, asserted on the text because it has to
// be asserted somewhere.
//
// Shape assertions are usually the weaker instrument and this suite prefers
// behaviour everywhere else. Not here: whether dash accepts `--` cannot be
// observed on a box without dash, and the boxes that run this suite are not
// the boxes that run the command. Measured in debian:stable-slim --
// `kill -9 -- "-$pgid"` gives "Illegal number: -" and the group survives.
func TestVerifiedKillGroupUsesABareNegativePid(t *testing.T) {
	got := vk("/run/vmsync-bridge-web01-20901.pid", true)

	if !strings.Contains(got, `kill -9 "-$p"`) {
		t.Errorf("the group kill is not a bare negative pid:\n%s", got)
	}
	// `kill -9 --` in any spelling. dash reads the separator as the pid.
	if strings.Contains(got, "kill -9 --") {
		t.Errorf("a `--` separator is back in the group kill:\n%s\ndash's kill builtin has no `--`; it reads it as the pid operand, fails with \"Illegal number: -\", and never signals the group. The helper and its relay children then keep running, keep the port bound and keep the image open. bash accepts it, so this will not show up in the suite on a Fedora or RHEL host.", got)
	}

	// And the single form must not acquire a negative pid, which would turn
	// one process into its whole group.
	if single := vk("/run/x.pid", false); !strings.Contains(single, `kill -9 "$p"`) {
		t.Errorf("the single kill is not aimed at the bare pid:\n%s", single)
	}
}

// The ordinary case, and the one that has to be quiet: a pidfile left behind
// by a process that is already gone. Nothing to kill, nothing to say.
//
// Both forms, because the group form is built separately, and a pid that is
// merely absent rather than malformed, because that is the one the digits
// guard does not catch and the /proc lookup does.
func TestVerifiedKillIsSilentForAPidThatIsGone(t *testing.T) {
	for _, group := range []bool{false, true} {
		name := "single"
		if group {
			name = "group"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			pidFile := filepath.Join(dir, "vmsync-qemu-nbd-web01-vda.pid")

			// A pid that is syntactically fine and simply does not exist.
			// 4194303 is above the usual pid_max, so nothing can hold it.
			writePidFile(t, pidFile, "4194303\n")

			// runCmd asserts silence.
			runCmd(t, vk(pidFile, group))
			mustBeGone(t, pidFile)
		})
	}
}

// COMM IS THE OTHER HALF OF THE PROOF, and this is the process that shows why
// the argv word alone is not enough.
//
// The premise behind matching argv is that the pidfile's path carries a domain
// and a device or port, so "an argv containing it verbatim cannot belong to
// anything else". That is false, and vmsync itself is the counterexample: its
// own stop command runs `cat '<pidfile>'`, `grep -qxF '<pidfile>'` and
// `rm -f '<pidfile>'`, and in each of those the path is a whole argv word of a
// short-lived process. A pid recycled onto one of them during a concurrent
// teardown matches on argv -- and in the group form that signals whatever
// process group the transient helper is sitting in.
//
// Comm separates a qemu-nbd from a cat. Here the target's argv matches
// perfectly and only its comm is wrong, which is exactly the shape of that
// false positive, and nothing may be signalled.
func TestVerifiedKillRefusesAProcessWhoseCommIsWrong(t *testing.T) {
	for _, group := range []bool{false, true} {
		name := "single"
		if group {
			name = "group"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			pidFile := filepath.Join(dir, "vmsync-qemu-nbd-web01-vda.pid")
			rec := filepath.Join(dir, "signals")

			// Its argv carries the pidfile path as its own word; its comm is
			// "sh", not "qemu-nbd".
			p := spawn(t, pidFile, false)
			writePidFile(t, pidFile, strconv.Itoa(p.pid)+"\n")

			cmd := VerifiedKill{PidFile: pidFile, Comm: "qemu-nbd", Group: group}.Command()
			stub := "kill() { printf '%s\\n' \"kill $*\" >> " + ShQuote(rec) + "; }; "
			runCmd(t, stub+cmd)

			if b, err := os.ReadFile(rec); err == nil && len(b) > 0 {
				t.Errorf("a process whose argv matches but whose comm does not was signalled: %s", b)
			}
			if !p.stillRunning() {
				t.Error("and it died -- that shape is vmsync's own `cat <pidfile>`, and in the group form the signal reaches its whole process group")
			}
			mustBeGone(t, pidFile)
		})
	}
}

// And comm must not be so strict that it refuses the real thing. The kernel
// stores 15 characters of it, so a longer binary name is truncated there --
// "vmsync-bridge-helper" becomes "vmsync-bridge-h". Comparing the full name
// would refuse every kill against the one process the group form exists for.
//
// Exercised with a real long-named binary rather than asserted on the text: a
// copy of /bin/sh under a 20-character name, so the kernel does the truncating
// and the test depends on its behaviour instead of on my arithmetic.
func TestVerifiedKillComparesTheCommTheKernelKeeps(t *testing.T) {
	const longName = "vmsync-bridge-helper" // 20 characters
	if len(longName) <= commLimit {
		t.Fatalf("%q is not longer than the %d the kernel keeps, so this test proves nothing", longName, commLimit)
	}

	dir := t.TempDir()
	bin := filepath.Join(dir, longName)
	src, err := os.ReadFile("/bin/sh")
	if err != nil {
		t.Skipf("cannot read /bin/sh to make a long-named copy: %v", err)
	}
	if err := os.WriteFile(bin, src, 0o755); err != nil {
		t.Fatalf("write the long-named shell: %v", err)
	}

	pidFile := filepath.Join(dir, "vmsync-bridge-web01-20901.pid")
	cmd := exec.Command(bin, "-c", "while :; do sleep 0.2; done", pidFile)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the long-named shell: %v", err)
	}
	p := &proc{pid: cmd.Process.Pid, done: make(chan struct{})}
	go func() { _, _ = cmd.Process.Wait(); close(p.done) }()
	t.Cleanup(func() {
		_ = syscall.Kill(p.pid, syscall.SIGKILL)
		_ = syscall.Kill(-p.pid, syscall.SIGKILL)
		<-p.done
	})

	// What the kernel actually kept, so a failure says which half was wrong.
	comm, err := os.ReadFile("/proc/" + strconv.Itoa(p.pid) + "/comm")
	if err != nil {
		t.Fatalf("read comm: %v", err)
	}
	kept := string(bytesTrimSpace(comm))
	if kept == longName {
		t.Skipf("this kernel kept the whole of %q, so there is no truncation to compare against", longName)
	}
	t.Logf("the kernel kept %q of %q", kept, longName)

	writePidFile(t, pidFile, strconv.Itoa(p.pid)+"\n")
	runCmd(t, VerifiedKill{PidFile: pidFile, Comm: longName}.Command())

	if !p.died(5 * time.Second) {
		t.Errorf("a process whose comm is the kernel's truncation of %q was NOT killed, so the comparison uses a name /proc can never hold and every real kill is refused", longName)
	}
	mustBeGone(t, pidFile)
}

// THE DOCUMENTED ESCAPE HATCH, which until now had no test at all.
//
// Comm may be empty, and the field says so: "Empty means the check is skipped,
// which only an argv that is unique by construction should ever ask for." No
// caller in the tree asks for it, which is exactly how an option ends up
// unexercised -- and mutation testing found the consequence. With a comm set,
// the comm read is the first thing that touches /proc, so it short-circuits
// every case where the pid is gone; a mutation putting the ARGV read's
// `2>/dev/null` back after its input redirect therefore produced no stderr at
// all and survived the suite. The ordering is still load-bearing, just only on
// this path, and the only way to keep it honest is to exercise this path.
//
// So: no comm check, and all three things still hold -- silent for a pid that
// is gone, refuses one it cannot match on argv, and kills the one it can.
func TestVerifiedKillWithNoCommCheckStillProvesAndStaysSilent(t *testing.T) {
	t.Run("a pid that is gone says nothing", func(t *testing.T) {
		dir := t.TempDir()
		pidFile := filepath.Join(dir, "vmsync-qemu-nbd-web01-vda.pid")
		writePidFile(t, pidFile, "4194303\n")

		// runCmd asserts silence as well as success.
		runCmd(t, VerifiedKill{PidFile: pidFile}.Command())
		mustBeGone(t, pidFile)
	})

	t.Run("an unproven pid is not signalled", func(t *testing.T) {
		dir := t.TempDir()
		pidFile := filepath.Join(dir, "vmsync-qemu-nbd-web01-vda.pid")
		rec := filepath.Join(dir, "signals")
		stranger := spawn(t, "an-unrelated-process", false)
		writePidFile(t, pidFile, strconv.Itoa(stranger.pid)+"\n")

		stub := "kill() { printf '%s\\n' \"kill $*\" >> " + ShQuote(rec) + "; }; "
		runCmd(t, stub+VerifiedKill{PidFile: pidFile}.Command())

		if b, err := os.ReadFile(rec); err == nil && len(b) > 0 {
			t.Errorf("signalled a pid whose argv does not carry %s: %s", filepath.Base(pidFile), b)
		}
		if !stranger.stillRunning() {
			t.Error("and it died")
		}
		mustBeGone(t, pidFile)
	})

	t.Run("the real target is still killed", func(t *testing.T) {
		dir := t.TempDir()
		pidFile := filepath.Join(dir, "vmsync-qemu-nbd-web01-vda.pid")
		p := spawn(t, pidFile, false)
		writePidFile(t, pidFile, strconv.Itoa(p.pid)+"\n")

		runCmd(t, VerifiedKill{PidFile: pidFile}.Command())

		if !p.died(5 * time.Second) {
			t.Error("a process whose argv carries its pidfile path was not killed, so skipping the comm check refuses everything instead of only narrowing the proof")
		}
		mustBeGone(t, pidFile)
	})
}
