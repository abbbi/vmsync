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
	"strings"
	"sync"
	"testing"
	"time"

	"vmsync/pkg/util"
)

// superviseRig drives superviseLoop one deadline at a time, with no real
// clock and no real process. Every deadline the loop arms is handed back
// here, so a test fires them in whatever order it wants and the whole state
// machine is decidable without spending a second of anybody's time -- the
// shape pkg/remotessh uses to prove its keepalive strikes.
//
// Everything the loop touches is under mu: the loop runs on its own
// goroutine, so a rig that collected into bare slices would be a data race
// in every test that used it.
type superviseRig struct {
	tick chan time.Time
	wait chan error

	mu      sync.Mutex
	armed   []chan time.Time
	signals []string
	beats   []string
	state   string
	stops   int
}

func newSuperviseRig() *superviseRig {
	return &superviseRig{tick: make(chan time.Time, 8), wait: make(chan error, 1), state: "D"}
}

func (r *superviseRig) clock(maxRuntime, grace time.Duration) superviseClock {
	return superviseClock{
		tick: r.tick,
		stop: func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.stops++
		},
		maxRuntime: maxRuntime,
		grace:      grace,
		after: func(time.Duration) <-chan time.Time {
			ch := make(chan time.Time, 1)
			r.mu.Lock()
			defer r.mu.Unlock()
			r.armed = append(r.armed, ch)
			return ch
		},
	}
}

func (r *superviseRig) actions() superviseActions {
	return superviseActions{
		beat: func(phase string, _ time.Duration, _ progressSample, _ int) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.beats = append(r.beats, phase)
		},
		signal: func(sig string) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.signals = append(r.signals, sig)
			return nil
		},
		state: func() string {
			r.mu.Lock()
			defer r.mu.Unlock()
			return r.state
		},
	}
}

// run starts the loop and returns a channel carrying its verdict.
func (r *superviseRig) run(maxRuntime, grace time.Duration) <-chan superviseResult {
	done := make(chan superviseResult, 1)
	go func() { done <- superviseLoop(r.clock(maxRuntime, grace), r.wait, r.actions()) }()
	return done
}

// fire releases the nth deadline the loop armed (0 = the first), waiting for
// it to be armed if the loop has not got there yet.
func (r *superviseRig) fire(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		r.mu.Lock()
		armed := len(r.armed)
		var ch chan time.Time
		if armed > n {
			ch = r.armed[n]
		}
		r.mu.Unlock()
		if ch != nil {
			ch <- time.Time{}
			return
		}
		select {
		case <-time.After(time.Millisecond):
		case <-deadline:
			t.Fatalf("the loop never armed deadline %d (armed %d)", n, armed)
		}
	}
}

func (r *superviseRig) sent() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.signals...)
}

// awaitSignals blocks until the loop has sent n signals.
//
// Needed because fire() only makes a deadline READY: the loop may not have
// acted on it yet, and a test that went straight on to release the child
// would be racing its own setup -- sometimes exercising the case it meant
// to, sometimes a different one. Every assertion about what happens AFTER a
// signal has to wait for the signal.
func (r *superviseRig) awaitSignals(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if len(r.sent()) >= n {
			return
		}
		select {
		case <-time.After(time.Millisecond):
		case <-deadline:
			t.Fatalf("the loop sent %d signals, waiting for %d", len(r.sent()), n)
		}
	}
}

// settle waits for the verdict, failing rather than hanging. A supervisor
// that does not return is the bug this whole mode exists to prevent, so a
// test that hung on it would be reporting the fault as a timeout in CI
// instead of as a failure here.
func settle(t *testing.T, done <-chan superviseResult) superviseResult {
	t.Helper()
	select {
	case res := <-done:
		return res
	case <-time.After(5 * time.Second):
		t.Fatal("superviseLoop never returned")
		return superviseResult{}
	}
}

// TestSuperviseStopsWaitingForAChildThatCannotDie is the reason this mode is
// worth writing at all, and the assertion that distinguishes it from a
// supervisor that has merely moved the hang one level down.
//
// A process in uninterruptible I/O -- which is what a command touching
// wedged storage becomes -- cannot be killed by anything. SIGKILL pends until
// the I/O completes or errors, and waitpid cannot collect a process that is
// still there, so cmd.Wait() never returns. A supervisor that waits for the
// child it just killed therefore inherits exactly the fault it was built to
// bound, and the caller is no better off than before.
//
// So the loop arms a THIRD deadline after the kill and gives up on the wait.
// Here the child never exits at all: the verdict must still arrive, say
// reaped=false, and carry the state letter that explains why.
//
// MUTATION CAUGHT: removing the post-kill deadline, or returning only when
// waitCh fires. Both hang, and settle() fails rather than letting the test
// time out namelessly.
func TestSuperviseStopsWaitingForAChildThatCannotDie(t *testing.T) {
	r := newSuperviseRig()
	done := r.run(30*time.Second, 5*time.Second)

	r.fire(t, 0) // the runtime limit: SIGTERM
	r.awaitSignals(t, 1)
	r.fire(t, 1) // the grace: SIGKILL
	r.fire(t, 2) // the reap deadline: give up

	res := settle(t, done)
	if res.Outcome != superviseTimeout {
		t.Errorf("outcome = %q, want %q", res.Outcome, superviseTimeout)
	}
	if got := strings.Join(res.Signals, ","); got != "TERM,KILL" {
		t.Errorf("signals = %q, want TERM,KILL -- SIGTERM first is what lets qemu-nbd remove its own pidfile", got)
	}
	if res.Reaped {
		t.Error("reported the child as reaped when it never exited")
	}
	if res.State != "D" {
		t.Errorf("child_state = %q, want D: the state letter is what explains an unreaped child to somebody who does not already know", res.State)
	}
	if res.ExitCode != util.ExitSupervisedTimeout {
		t.Errorf("exit hint = %d, want %d", res.ExitCode, util.ExitSupervisedTimeout)
	}
}

// TestSuperviseEscalatesOnlyWhenTermIsIgnored pins the grace. A command that
// honours SIGTERM must never be killed, because the difference is a qemu-nbd
// that removed its pidfile and one that left it behind for a later teardown
// to kill -9 whatever process has since inherited the number.
func TestSuperviseEscalatesOnlyWhenTermIsIgnored(t *testing.T) {
	r := newSuperviseRig()
	done := r.run(30*time.Second, 5*time.Second)

	r.fire(t, 0) // the runtime limit: SIGTERM
	r.awaitSignals(t, 1)
	r.wait <- nil

	res := settle(t, done)
	if got := strings.Join(res.Signals, ","); got != "TERM" {
		t.Errorf("signals = %q, want TERM alone: the child exited inside its grace", got)
	}
	if !res.Reaped {
		t.Error("a child that exited during the grace was not reported as reaped")
	}
	if res.Outcome != superviseTimeout {
		t.Errorf("outcome = %q, want %q: it still outlived its runtime limit, it just died politely", res.Outcome, superviseTimeout)
	}
}

// TestSuperviseNeverSignalsWithoutARuntimeLimit is the qemu-img commit case, and
// the most expensive mutation in this file.
//
// A commit must never be abandoned part-way: the base image is then in a
// state nobody can describe, and the replica is neither its old self nor its
// new one. vmsync runs it with MaxRuntime 0 for exactly that reason, and a loop
// that armed a deadline anyway -- treating zero as "immediately" rather than
// "never" -- would kill the one command in the set that must not be killed.
//
// It still beats, because a commit that hangs must be audible even though it
// must not be interrupted.
func TestSuperviseNeverSignalsWithoutARuntimeLimit(t *testing.T) {
	r := newSuperviseRig()
	done := r.run(0, 5*time.Second)

	for i := 0; i < 20; i++ {
		r.tick <- time.Time{}
	}
	r.wait <- nil
	res := settle(t, done)

	if got := r.sent(); len(got) != 0 {
		t.Errorf("signalled %v a command that was given no runtime limit; a qemu-img commit killed part-way leaves the base image in an undescribable state", got)
	}
	r.mu.Lock()
	beats := len(r.beats)
	r.mu.Unlock()
	if beats == 0 {
		t.Error("a command with no runtime limit produced no liveness lines, so a hang in it would be as invisible as before")
	}
	if res.Outcome != superviseCompleted {
		t.Errorf("outcome = %q, want %q", res.Outcome, superviseCompleted)
	}
}

// TestSuperviseReportsACleanRunWithoutFuss: the overwhelmingly common case
// must add nothing. No signals, no reaped= field in the line, and the
// caller's exit code untouched.
func TestSuperviseReportsACleanRunWithoutFuss(t *testing.T) {
	r := newSuperviseRig()
	done := r.run(30*time.Second, 5*time.Second)
	r.wait <- nil
	res := settle(t, done)

	if res.Outcome != superviseCompleted {
		t.Errorf("outcome = %q, want %q", res.Outcome, superviseCompleted)
	}
	if len(res.Signals) != 0 {
		t.Errorf("signalled %v a command that finished on its own", res.Signals)
	}
	if line := res.Line(); strings.Contains(line, "reaped=") || strings.Contains(line, "signalled=") {
		t.Errorf("a clean run's verdict carries fields that only mean something after a signal: %q", line)
	}
}

// TestSuperviseStopsItsTicker keeps the beat from outliving the command it
// describes. One leaked ticker per supervised command, on a host running a
// sync every five minutes, is a slow leak in a long-lived agent.
func TestSuperviseStopsItsTicker(t *testing.T) {
	r := newSuperviseRig()
	done := r.run(30*time.Second, 5*time.Second)
	r.wait <- nil
	settle(t, done)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stops != 1 {
		t.Errorf("the ticker was stopped %d times, want exactly 1", r.stops)
	}
}

// TestParseProcStateSurvivesAHostileCommand is the classic /proc parser bug,
// pinned. Field 2 of /proc/pid/stat is the executable name in parentheses and
// may contain spaces and parentheses of its own, so splitting the line on
// spaces reads the wrong field -- and reports a process stuck in D as
// something reassuring.
func TestParseProcStateSurvivesAHostileCommand(t *testing.T) {
	for _, tc := range []struct{ name, stat, want string }{
		{"ordinary", "1234 (sh) S 1 1234 1234 0 -1 4194304 123 0", "S"},
		{"spaces in the name", "1234 (my weird cmd) D 1 1234", "D"},
		{"parentheses in the name", "1234 (sh (wrapped)) D 1 1234", "D"},
		{"truncated", "1234 (sh)", ""},
		{"nonsense", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseProcState(tc.stat); got != tc.want {
				t.Errorf("parseProcState(%q) = %q, want %q", tc.stat, got, tc.want)
			}
		})
	}
}

// TestSuperviseVerdictIsParseableAndComplete guards the line vmsync reads.
//
// The verdict is authoritative and the exit code is only a hint, so anything
// missing from this line is a fact the far side cannot recover: a supervised
// command can legitimately exit 124 itself, and only this line distinguishes
// that from a command this process timed out.
func TestSuperviseVerdictIsParseableAndComplete(t *testing.T) {
	timedOut := superviseResult{
		Outcome: superviseTimeout, ExitCode: util.ExitSupervisedTimeout,
		Signals: []string{"TERM", "KILL"}, Reaped: false, State: "D",
		Duration: 35 * time.Second,
	}
	line := timedOut.Line()
	if !strings.HasPrefix(line, superviseVerdictPrefix) {
		t.Fatalf("verdict does not carry the prefix the far side greps for: %q", line)
	}
	for _, want := range []string{
		"outcome=timeout", "exit=124", "duration=35s",
		"signalled=TERM,KILL", "reaped=false", "child_state=D",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("verdict is missing %q: %q", want, line)
		}
	}
	if strings.Contains(line, "\n") {
		t.Errorf("the verdict spans more than one line, so a line-based reader would see only part of it: %q", line)
	}
}

// TestSuperviseBeatNamesWhatAnOperatorNeeds. The beat exists to turn "the
// target is slow" into a diagnosis, and it does that with three facts: which
// command, how long, and what the child is doing. Dropping any of them
// leaves a line that proves only that this process is alive -- which nobody
// was asking about.
func TestSuperviseBeatNamesWhatAnOperatorNeeds(t *testing.T) {
	cfg := superviseConfig{Label: "start target qemu-nbd for vda", MaxRuntime: 30 * time.Second}
	line := formatSuperviseBeat(cfg, 4242, superviseRunning, 2*time.Minute, "D", progressSample{}, 0)
	for _, want := range []string{
		"start target qemu-nbd for vda", "2m0s", "child 4242",
		"state D", "uninterruptible I/O", "max runtime 30s",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("beat is missing %q: %q", want, line)
		}
	}

	// A command with no limit says so, because "no runtime limit" is the fact that
	// explains why a line has been repeating for twenty minutes.
	commit := formatSuperviseBeat(superviseConfig{Label: "committing changes for vda"}, 7, superviseRunning, time.Hour, "", progressSample{}, 0)
	if !strings.Contains(commit, "no runtime limit") {
		t.Errorf("a command with no runtime limit does not say so: %q", commit)
	}
	if strings.Contains(commit, "child 7") {
		t.Errorf("the beat claims a process state it could not read: %q", commit)
	}
}

// TestSuperviseLetsAChildThatFinishedAtTheDeadlineFinish covers the race
// that only appears under load, and it is a coin flip when it appears.
//
// select picks at random when more than one case is ready, so a command that
// exited in the same instant its runtime limit expired is signalled about half the
// time -- and a command that honoured SIGTERM just as its grace ran out is
// killed and then reported unreaped, although it had already gone. Both read
// as a target fault in the log of a run that was fine.
//
// Looped, because a race that happens half the time passes a single run half
// the time.
func TestSuperviseLetsAChildThatFinishedAtTheDeadlineFinish(t *testing.T) {
	t.Run("at the runtime limit", func(t *testing.T) {
		for i := 0; i < 200; i++ {
			r := newSuperviseRig()
			r.wait <- nil
			done := r.run(30*time.Second, 5*time.Second)
			r.fire(t, 0)
			res := settle(t, done)
			if got := r.sent(); len(got) != 0 {
				t.Fatalf("round %d: signalled %v a command that had already exited", i, got)
			}
			if res.Outcome != superviseCompleted {
				t.Fatalf("round %d: outcome = %q, want %q", i, res.Outcome, superviseCompleted)
			}
		}
	})

	t.Run("at the grace", func(t *testing.T) {
		for i := 0; i < 200; i++ {
			r := newSuperviseRig()
			done := r.run(30*time.Second, 5*time.Second)
			r.fire(t, 0) // the runtime limit: SIGTERM goes out
			r.awaitSignals(t, 1)
			r.wait <- nil
			r.fire(t, 1) // the grace fires in the same instant it exits
			res := settle(t, done)
			if got := strings.Join(r.sent(), ","); got != "TERM" {
				t.Fatalf("round %d: signals = %q, want TERM alone -- it honoured the term", i, got)
			}
			if !res.Reaped {
				t.Fatalf("round %d: a child that exited inside its grace was reported unreaped", i)
			}
		}
	})
}
