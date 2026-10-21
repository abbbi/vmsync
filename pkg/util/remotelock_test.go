/*
	Copyright (C) 2026  Michael Ablassmeier <abi@grinser.de>

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
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeHolder stands in for an SSH client, recording the script it was asked
// to run and replaying a canned outcome.
type fakeHolder struct {
	script string
	ready  string
	// failWith is the marker the remote script would have printed.
	failWith string
	// failHelperOnly limits failWith to the helper invocation, so a test can
	// model a target where the helper is unusable but the shell is fine -- which
	// is the whole point of the fallback and cannot be exercised by a fake that
	// fails both.
	failHelperOnly bool
	closed         *bool
	// handle, when set, is returned instead of a fresh one, so a test can watch
	// the heartbeat and make it fail.
	handle *fakeCloser
}

// fakeCloser is the remote command's stdin plus its lifetime, which is what the
// lock's heartbeat travels over.
type fakeCloser struct {
	closed *bool

	exited chan error
	// said is what the remote command wrote to stderr, which is where a
	// holder's reason for letting go actually lives.
	//
	// Revealed only once the exit has been delivered, through exitWith below,
	// and that is the point of it. The real Said() is complete only after
	// Exited() has fired, because session.Wait drains the stderr copier before
	// returning; a fake that answered the same thing at every instant could
	// not fail if the production code read it too early, which is exactly the
	// mistake worth catching here.
	said      string
	saidReady bool

	mu       sync.Mutex
	beats    int
	writeErr error
	isClose  bool
}

func (f *fakeCloser) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	f.beats++
	return len(p), nil
}

func (f *fakeCloser) Close() error {
	f.mu.Lock()
	f.isClose = true
	f.mu.Unlock()
	if f.closed != nil {
		*f.closed = true
	}
	return nil
}

func (f *fakeCloser) beatCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.beats
}

func (f *fakeCloser) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.isClose
}

// failWrites makes every later heartbeat fail, which is what a partition looks
// like from this side of the link.
func (f *fakeCloser) failWrites() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writeErr = errors.New("ssh: channel closed")
}

func (h *fakeHolder) HoldCommand(_ context.Context, command, readyLine string) (io.WriteCloser, error) {
	h.script, h.ready = command, readyLine
	if h.failWith != "" && (!h.failHelperOnly || strings.Contains(command, "-hold-run-lock")) {
		// Mirrors how remotessh reports a command that answered with
		// something other than the ready line.
		return nil, fmt.Errorf("remote command refused: %s", h.failWith)
	}
	if h.handle != nil {
		return h.handle, nil
	}
	return &fakeCloser{closed: h.closed}, nil
}

func TestAcquireRemoteRunLockScript(t *testing.T) {
	h := &fakeHolder{}
	if _, err := AcquireRemoteRunLock(context.Background(), h, "/run/vmsync-locks", "target-web01", RemoteLockOptions{}); err != nil {
		t.Fatalf("AcquireRemoteRunLock: %v", err)
	}

	// The lock must be held by the SHELL, not by the flock child: an `exec 9>>`
	// in the shell outlives the flock invocation, and the reader then keeps that
	// shell alive. `flock -n 9 -c ...` instead would release the moment the child
	// exited, making the whole thing a no-op that looks like it works.
	//
	// Each of the four is load-bearing on its own:
	//   - `9>>` and not `9>`, because `>` truncates BEFORE flock decides who
	//     wins, so a contender that then loses has blanked the provenance of the
	//     holder it lost to;
	//   - `exec cat`, so exactly one process holds fd 9 -- a forked child
	//     inherits the descriptor and would keep the lock alive after the shell
	//     that took it had gone;
	//   - `>/dev/null`, because nothing drains this command's stdout, so a reader
	//     echoing the heartbeat would fill the SSH channel window and wedge both
	//     ends;
	//   - the `( : >> )` probe, because POSIX lets a redirection failure on
	//     `exec` kill a non-interactive shell, which dash does -- so without the
	//     probe the NO-CREATE marker below is unreachable there and the caller
	//     gets a bare non-zero exit to guess from.
	for _, want := range []string{"exec 9>>", "flock -n 9", "\nexec cat >/dev/null", "( : >>"} {
		if !strings.Contains(h.script, want) {
			t.Errorf("script does not contain %q:\n%s", want, h.script)
		}
	}
	if strings.Contains(h.script, "exec 9>"+RunLockPath("/run/vmsync-locks", "target-web01")) {
		t.Error("the lock file is opened with > rather than >>, truncating it before flock has decided who holds it")
	}
	// Non-blocking: a contended lock has to answer now, not queue behind a
	// multi-hour sync.
	if strings.Contains(h.script, "flock 9") {
		t.Error("script blocks on the lock instead of failing fast")
	}
	// The path has to match the one the LOCAL lock would use, or a sync and
	// a promotion of the same domain would lock different files and both
	// proceed.
	if !strings.Contains(h.script, RunLockPath("/run/vmsync-locks", "target-web01")) {
		t.Errorf("script does not lock %s:\n%s", RunLockPath("/run/vmsync-locks", "target-web01"), h.script)
	}
	if h.ready != RemoteLockReady {
		t.Errorf("ready line = %q, want %q", h.ready, RemoteLockReady)
	}
}

func TestAcquireRemoteRunLockQuotesItsPaths(t *testing.T) {
	// The key reaches this from a domain name, and a domain name is not
	// guaranteed to be shell-safe.
	h := &fakeHolder{}
	if _, err := AcquireRemoteRunLock(context.Background(), h, "/run/vmsync-locks", "target-web01; rm -rf /", RemoteLockOptions{}); err != nil {
		t.Fatalf("AcquireRemoteRunLock: %v", err)
	}
	if strings.Contains(h.script, "; rm -rf /'") == false && strings.Contains(h.script, "rm -rf") {
		// Present but not inside quotes: that is the failure worth catching.
		idx := strings.Index(h.script, "rm -rf")
		before := h.script[:idx]
		if strings.Count(before, "'")%2 == 0 {
			t.Errorf("the key is interpolated unquoted:\n%s", h.script)
		}
	}
}

// TestAcquireRemoteRunLockContentionIsNotAnError distinguishes the one
// outcome callers must treat as a clean skip from every genuine failure.
// Confusing them either turns an ordinary overlap into a paging failure, or
// hides a broken host as routine contention.
func TestAcquireRemoteRunLockContentionIsNotAnError(t *testing.T) {
	for _, tc := range []struct {
		marker      string
		wantHeld    bool
		wantMention string
	}{
		{RemoteLockBusy, true, "already working on"},
		{RemoteLockNoFlock, false, "no flock"},
		{RemoteLockNoDir, false, "lock directory"},
		{RemoteLockNoCreate, false, "lock file"},
		// Not contention: a host that cannot prove which file it locked
		// cannot lock at all, and must read as a misconfiguration rather
		// than as somebody else holding it. The substring is the actionable
		// half -- an operator needs to be told which capability is missing,
		// not merely that something could not be compared.
		{RemoteLockNoSameFile, false, "test -ef"},
	} {
		t.Run(tc.marker, func(t *testing.T) {
			h := &fakeHolder{failWith: tc.marker}
			_, err := AcquireRemoteRunLock(context.Background(), h, "/run/vmsync-locks", "target-web01", RemoteLockOptions{})
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := errors.Is(err, ErrLockHeld); got != tc.wantHeld {
				t.Errorf("errors.Is(err, ErrLockHeld) = %v, want %v (err = %v)", got, tc.wantHeld, err)
			}
			if !strings.Contains(err.Error(), tc.wantMention) {
				t.Errorf("error %q does not mention %q", err, tc.wantMention)
			}
		})
	}
}

func TestAcquireRemoteRunLockReturnsAWorkingCloser(t *testing.T) {
	closed := false
	h := &fakeHolder{closed: &closed}
	lock, err := AcquireRemoteRunLock(context.Background(), h, "/run/vmsync-locks", "target-web01", RemoteLockOptions{})
	if err != nil {
		t.Fatalf("AcquireRemoteRunLock: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if !closed {
		t.Error("closing the lock did not end the remote command holding it")
	}
}

// The helper is preferred, because it is the only holder that releases the lock
// when the driver dies without closing anything.
func TestALeasedLockIsPreferredWhenAHelperIsThere(t *testing.T) {
	h := &fakeHolder{}
	lock, err := AcquireRemoteRunLock(context.Background(), h, "/run/vmsync-locks", "target-web01", RemoteLockOptions{
		HelperPath: "/usr/local/bin/vmsync-bridge-helper",
		Lease:      45 * time.Second,
		Identity:   RunLockIdentity{Kind: "sync", SourceDomain: "web01", RunID: "run-abc"},
	})
	if err != nil {
		t.Fatalf("AcquireRemoteRunLock: %v", err)
	}
	defer lock.Close()

	if !lock.Leased() {
		t.Error("the lock is not leased although a helper path was given; without a lease the target cannot release it when this host dies")
	}
	if lock.Lease() != 45*time.Second {
		t.Errorf("lease = %s, want the configured 45s", lock.Lease())
	}
	for _, want := range []string{
		"'/usr/local/bin/vmsync-bridge-helper'",
		"-hold-run-lock '" + RunLockPath("/run/vmsync-locks", "target-web01") + "'",
		"-lock-lease '45s'",
		`"kind":"sync"`,
		`"run_id":"run-abc"`,
	} {
		if !strings.Contains(h.script, want) {
			t.Errorf("helper command does not contain %q:\n%s", want, h.script)
		}
	}
	// The helper holds the lock and nothing else; a shell script here would mean
	// the fallback ran even though the helper was available.
	if strings.Contains(h.script, "flock -n 9") {
		t.Errorf("the shell fallback was used although a helper path was given:\n%s", h.script)
	}
}

// A helper that cannot hold the lock must not cost the lock: the shell still
// excludes concurrent runs, which is the property a sync cannot proceed without.
// The run is then told, once, that the lock is partition-blind.
func TestAnUnusableHelperFallsBackToTheShell(t *testing.T) {
	h := &fakeHolder{failWith: "flag provided but not defined: -hold-run-lock", failHelperOnly: true}
	lock, err := AcquireRemoteRunLock(context.Background(), h, "/run/vmsync-locks", "target-web01", RemoteLockOptions{
		HelperPath: "/usr/local/bin/vmsync-bridge-helper",
	})
	if err != nil {
		t.Fatalf("an old helper cost the lock entirely: %v -- concurrent runs would no longer be excluded", err)
	}
	defer lock.Close()

	if lock.Leased() {
		t.Error("the lock reports itself leased after falling back to the shell; a caller would then not warn that a dead driver blocks promotion for two hours")
	}
	if lock.FellBackBecause() == nil {
		t.Error("the fallback recorded no reason, so the run cannot say WHY the lock is not leased")
	}
	if !strings.Contains(h.script, "flock -n 9") {
		t.Errorf("the shell fallback did not run:\n%s", h.script)
	}
}

// Contention from the helper must not be retried against the shell: it would
// find the same lock held and only delay the answer by a round trip.
func TestHelperContentionIsReportedWithoutRetryingTheShell(t *testing.T) {
	h := &fakeHolder{failWith: RemoteLockBusy}
	_, err := AcquireRemoteRunLock(context.Background(), h, "/run/vmsync-locks", "target-web01", RemoteLockOptions{
		HelperPath: "/usr/local/bin/vmsync-bridge-helper",
	})
	if !errors.Is(err, ErrLockHeld) {
		t.Fatalf("err = %v, want ErrLockHeld", err)
	}
	if strings.Contains(h.script, "flock -n 9") {
		t.Error("a busy helper was retried with the shell, which would report the same contention one round trip later")
	}
}

// The heartbeat is what a lease is renewed by, so it has to actually be sent --
// and its failure is the only signal this side gets that the lock is gone.
func TestTheHeartbeatRunsAndItsFailureIsReportedAsLockLoss(t *testing.T) {
	beat := &fakeCloser{}
	h := &fakeHolder{handle: beat}
	lock, err := AcquireRemoteRunLock(context.Background(), h, "/run/vmsync-locks", "target-web01", RemoteLockOptions{
		HelperPath: "/usr/local/bin/vmsync-bridge-helper",
		Lease:      90 * time.Millisecond, // beats every 30ms
	})
	if err != nil {
		t.Fatalf("AcquireRemoteRunLock: %v", err)
	}
	if lock.Lost() != nil {
		t.Errorf("a freshly taken lock reports itself lost: %v", lock.Lost())
	}
	time.Sleep(200 * time.Millisecond)
	if beat.beatCount() == 0 {
		t.Fatal("no heartbeat was sent, so a leased lock would be released under a healthy driver")
	}
	if lock.Lost() != nil {
		t.Errorf("a lock whose beats are landing reports itself lost: %v", lock.Lost())
	}

	// The channel goes away, which is what a partition looks like from here.
	beat.failWrites()
	deadline := time.Now().Add(3 * time.Second)
	for lock.Lost() == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	lost := lock.Lost()
	if lost == nil {
		t.Fatal("a failing heartbeat was not reported as lock loss; the gates before commit and before define would let the run write to a replica it no longer holds")
	}
	if !errors.Is(lost, ErrRemoteLockLost) {
		t.Errorf("Lost() = %v, want it to wrap ErrRemoteLockLost so callers can match on it", lost)
	}
	if err := lock.Close(); err != nil {
		t.Errorf("Close after loss: %v", err)
	}
}

// Close must not race an in-flight heartbeat: closing the handle underneath a
// write turns an ordinary release into an error a caller cannot act on.
func TestCloseStopsTheHeartbeatBeforeClosingTheHandle(t *testing.T) {
	beat := &fakeCloser{}
	h := &fakeHolder{handle: beat}
	lock, err := AcquireRemoteRunLock(context.Background(), h, "/run/vmsync-locks", "target-web01", RemoteLockOptions{
		HelperPath: "/usr/local/bin/vmsync-bridge-helper",
		Lease:      15 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("AcquireRemoteRunLock: %v", err)
	}
	time.Sleep(60 * time.Millisecond)
	if err := lock.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Nothing may be written after Close returned.
	after := beat.beatCount()
	time.Sleep(60 * time.Millisecond)
	if beat.beatCount() != after {
		t.Errorf("the heartbeat kept writing after Close returned (%d then %d)", after, beat.beatCount())
	}
	if !beat.isClosed() {
		t.Error("Close did not end the remote command holding the lock")
	}
}

// A holder that exits is the PROMPT signal that the lock is gone, and the reason
// the whole lease mechanism is trustworthy from this side.
//
// Without it this side learns only when a heartbeat write fails -- and a write
// into a socket whose peer has vanished succeeds into the local send buffer and
// keeps succeeding until the kernel gives up its retransmissions, around fifteen
// minutes on Linux. The target hands the lock on at the lease, 90 seconds. The
// gap between those two numbers is a window where this run believes it holds a
// lock a promoted guest is already writing behind.
func TestAHolderThatExitsIsReportedAtOnce(t *testing.T) {
	h := &fakeHolder{handle: &fakeCloser{exited: make(chan error, 1)}}
	lock, err := AcquireRemoteRunLock(context.Background(), h, "/run/vmsync-locks", "target-web01", RemoteLockOptions{
		HelperPath: "/usr/local/bin/vmsync-bridge-helper",
		// Long, so nothing here can pass by way of a heartbeat tick.
		Lease: time.Hour,
	})
	if err != nil {
		t.Fatalf("AcquireRemoteRunLock: %v", err)
	}
	defer lock.Close()
	if lock.Lost() != nil {
		t.Fatalf("a freshly taken lock reports itself lost: %v", lock.Lost())
	}

	// The lease expired on the target: the holder let go and exited cleanly.
	h.handle.exited <- nil

	deadline := time.Now().Add(3 * time.Second)
	for lock.Lost() == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	lost := lock.Lost()
	if lost == nil {
		t.Fatal("the holder exited and the lock still reports itself held; with an hour-long lease nothing else would have noticed for an hour")
	}
	if !errors.Is(lost, ErrRemoteLockLost) {
		t.Errorf("Lost() = %v, want it to wrap ErrRemoteLockLost", lost)
	}
	if !strings.Contains(lost.Error(), "exited") {
		t.Errorf("Lost() = %q, which does not say the holder exited -- the two causes need telling apart in a log", lost)
	}
}

// Closing deliberately must not look like losing the lock, or every clean
// release would log a scary line and the signal would stop meaning anything.
func TestADeliberateCloseIsNotALoss(t *testing.T) {
	h := &fakeHolder{handle: &fakeCloser{exited: make(chan error, 1)}}
	lock, err := AcquireRemoteRunLock(context.Background(), h, "/run/vmsync-locks", "target-web01", RemoteLockOptions{
		HelperPath: "/usr/local/bin/vmsync-bridge-helper",
		Lease:      time.Hour,
	})
	if err != nil {
		t.Fatalf("AcquireRemoteRunLock: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// The remote command ends because we ended it, which is what Close means.
	select {
	case h.handle.exited <- nil:
	default:
	}
	time.Sleep(50 * time.Millisecond)
	if lost := lock.Lost(); lost != nil {
		t.Errorf("a deliberately closed lock reports itself lost: %v", lost)
	}
}

// Exited is the optional hook AcquireRemoteRunLock looks for by shape.
func (f *fakeCloser) Exited() <-chan error { return f.exited }

func (f *fakeCloser) Said() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.saidReady {
		return ""
	}
	return f.said
}

// exitWith delivers the command's exit the way the real handle does: whatever
// it said on stderr becomes readable at the moment the exit is published, not
// before.
func (f *fakeCloser) exitWith(err error) {
	f.mu.Lock()
	f.saidReady = true
	f.mu.Unlock()
	f.exited <- err
}

// THE FINDING. A holder that let go explains itself on stderr, and until that
// reason reached the driver the loss arrived as "exited (Process exited with
// status 1)" -- or, worse, as "exited (<nil>)".
//
// Both cases are here because the exit status and the interesting-ness of the
// case do not agree. vmsync-bridge-helper's own runLockExit maps a lease that
// expired to status ZERO, calling it "the lock doing its job ... the outcome it
// exists to produce", while a dispossessed holder exits 1. So a fix that only
// decorated the non-nil error would have delivered the rarer case and lost the
// ordinary one, which is the question an operator actually arrives with: the
// sync refused to commit, and why.
func TestTheLossSaysWhyTheHolderLetGo(t *testing.T) {
	for _, tc := range []struct {
		name     string
		exit     error
		said     string
		wantSaid string
	}{
		{
			// The lease ran out: the helper prints this and exits 0.
			name:     "a lease that expired, which exits cleanly",
			exit:     nil,
			said:     "vmsync-bridge-helper: run lock lease expired",
			wantSaid: "run lock lease expired",
		},
		{
			// The lock file went away under a live holder: exit 1.
			name:     "a holder dispossessed by something clearing the lock directory",
			exit:     errors.New("Process exited with status 1"),
			said:     "vmsync-bridge-helper: the run lock was removed from under this holder: /run/vmsync-locks/target-web01.lock has been removed",
			wantSaid: "removed from under this holder",
		},
		{
			// Nothing said: the message must not grow an empty clause.
			name:     "a holder that said nothing",
			exit:     errors.New("Process exited with status 1"),
			said:     "",
			wantSaid: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &fakeHolder{handle: &fakeCloser{exited: make(chan error, 1), said: tc.said}}
			lock, err := AcquireRemoteRunLock(context.Background(), h, "/run/vmsync-locks", "target-web01", RemoteLockOptions{
				HelperPath: "/usr/local/bin/vmsync-bridge-helper",
				Lease:      time.Hour,
			})
			if err != nil {
				t.Fatalf("AcquireRemoteRunLock: %v", err)
			}
			defer lock.Close()

			h.handle.exitWith(tc.exit)

			deadline := time.Now().Add(3 * time.Second)
			for lock.Lost() == nil && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			lost := lock.Lost()
			if lost == nil {
				t.Fatal("the holder exited and the lock still reports itself held")
			}
			if !errors.Is(lost, ErrRemoteLockLost) {
				t.Errorf("loss does not wrap ErrRemoteLockLost, so the gates before the commit and the define will not refuse on it: %v", lost)
			}
			if tc.wantSaid == "" {
				if strings.Contains(lost.Error(), "saying:") {
					t.Errorf("a holder that said nothing produced an empty explanation clause: %v", lost)
				}
				return
			}
			if !strings.Contains(lost.Error(), tc.wantSaid) {
				t.Errorf("the loss does not say why the holder let go.\n got: %v\nwant it to contain: %q", lost, tc.wantSaid)
			}
		})
	}
}

// THE RACE. Two goroutines can discover the same loss, and the one that knows
// least can get here first.
//
// The heartbeat only knows its write failed. The exit watcher knows the holder
// ended and why. With first-wins, a pending beat failing a moment before the
// exit arrives meant the operator read "heartbeat to <path> failed: EOF" while
// "run lock lease expired" was discarded behind it -- the symptom kept, the
// cause thrown away, in the one scenario the stderr plumbing exists for.
//
// Driven in that order deliberately: the observed loss is established FIRST,
// and only then does the explanation arrive.
func TestAnExplainedLossReplacesAnObservedOne(t *testing.T) {
	h := &fakeHolder{handle: &fakeCloser{
		exited: make(chan error, 1),
		said:   "vmsync-bridge-helper: run lock lease expired",
	}}
	// Short, so a beat is attempted at once and the heartbeat path wins.
	lock, err := AcquireRemoteRunLock(context.Background(), h, "/run/vmsync-locks", "target-web01", RemoteLockOptions{
		HelperPath: "/usr/local/bin/vmsync-bridge-helper",
		Lease:      90 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("AcquireRemoteRunLock: %v", err)
	}
	defer lock.Close()

	h.handle.failWrites()
	deadline := time.Now().Add(3 * time.Second)
	for lock.Lost() == nil && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	observed := lock.Lost()
	if observed == nil {
		t.Fatal("a failing heartbeat did not report the lock as lost")
	}
	if !strings.Contains(observed.Error(), "heartbeat") {
		t.Fatalf("expected the heartbeat to be the first to notice; got %v", observed)
	}

	// Now the holder's own account of it arrives.
	h.handle.exitWith(nil)

	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(lock.Lost().Error(), "lease expired") {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	final := lock.Lost()
	if !strings.Contains(final.Error(), "lease expired") {
		t.Errorf("the holder's own reason never replaced the heartbeat's observation.\n got: %v\nThe operator is left reading a network fault where the truth is that the target released the lock.", final)
	}
	if strings.Contains(final.Error(), "heartbeat") {
		t.Errorf("the loss still reports the heartbeat failure, which is a consequence of the exit rather than the cause: %v", final)
	}
	if !errors.Is(final, ErrRemoteLockLost) {
		t.Errorf("the replacement no longer wraps ErrRemoteLockLost, so the commit and define gates would stop refusing: %v", final)
	}
}

// AND THE OTHER DIRECTION MUST NOT HAPPEN. A partition is the case where the
// heartbeat is the only thing that can notice: nothing exits, so no explanation
// is ever offered, and the observation is the whole truth.
func TestAPartitionKeepsTheHeartbeatsAccount(t *testing.T) {
	h := &fakeHolder{handle: &fakeCloser{
		exited: make(chan error, 1),
		said:   "this must never appear: nothing exited",
	}}
	lock, err := AcquireRemoteRunLock(context.Background(), h, "/run/vmsync-locks", "target-web01", RemoteLockOptions{
		HelperPath: "/usr/local/bin/vmsync-bridge-helper",
		Lease:      90 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("AcquireRemoteRunLock: %v", err)
	}
	defer lock.Close()

	h.handle.failWrites()
	deadline := time.Now().Add(3 * time.Second)
	for lock.Lost() == nil && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	lost := lock.Lost()
	if lost == nil {
		t.Fatal("a failing heartbeat did not report the lock as lost")
	}
	if !strings.Contains(lost.Error(), "heartbeat") {
		t.Errorf("a partition must be reported as what it is -- the heartbeat failing -- not as something the holder said: %v", lost)
	}
	// Nothing exited, so nothing may be attributed to the holder.
	if strings.Contains(lost.Error(), "this must never appear") {
		t.Errorf("stderr was attributed to a loss no exit explained: %v", lost)
	}
	// And it stays that way.
	time.Sleep(150 * time.Millisecond)
	if !strings.Contains(lock.Lost().Error(), "heartbeat") {
		t.Errorf("the heartbeat's account was replaced although nothing exited: %v", lock.Lost())
	}
}

// A lease that expired exits CLEANLY, and that has to read as something.
func TestACleanExitDoesNotReportItselfAsNil(t *testing.T) {
	h := &fakeHolder{handle: &fakeCloser{
		exited: make(chan error, 1),
		said:   "vmsync-bridge-helper: run lock lease expired",
	}}
	lock, err := AcquireRemoteRunLock(context.Background(), h, "/run/vmsync-locks", "target-web01", RemoteLockOptions{
		HelperPath: "/usr/local/bin/vmsync-bridge-helper",
		Lease:      time.Hour,
	})
	if err != nil {
		t.Fatalf("AcquireRemoteRunLock: %v", err)
	}
	defer lock.Close()

	h.handle.exitWith(nil)
	deadline := time.Now().Add(3 * time.Second)
	for lock.Lost() == nil && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	lost := lock.Lost()
	if lost == nil {
		t.Fatal("the holder exited and the lock still reports itself held")
	}
	if strings.Contains(lost.Error(), "<nil>") {
		t.Errorf("the most ordinary reason for losing a target lock renders as \"exited (<nil>)\": %v", lost)
	}
	if !strings.Contains(lost.Error(), "deliberately") {
		t.Errorf("a clean exit does not say that the holder let go on purpose, which is what distinguishes it from a crash: %v", lost)
	}
}

// AND THE MIRROR ORDER, which the first pair of these tests did not cover and
// a mutation walked straight through.
//
// The exit can arrive first and the next beat fail a moment later -- the holder
// is gone, so of course the write fails. If an observation may overwrite a
// cause, the explanation the driver had just been given is replaced by
// "heartbeat failed: EOF", and the reason is lost again by a different route
// than the one 4.22 fixed.
func TestAnObservationNeverOverwritesACause(t *testing.T) {
	h := &fakeHolder{handle: &fakeCloser{
		exited: make(chan error, 1),
		said:   "vmsync-bridge-helper: run lock lease expired",
	}}
	lock, err := AcquireRemoteRunLock(context.Background(), h, "/run/vmsync-locks", "target-web01", RemoteLockOptions{
		HelperPath: "/usr/local/bin/vmsync-bridge-helper",
		// Short, so beats keep being attempted after the exit.
		Lease: 90 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("AcquireRemoteRunLock: %v", err)
	}
	defer lock.Close()

	// The cause arrives first this time.
	h.handle.exitWith(nil)
	deadline := time.Now().Add(3 * time.Second)
	for lock.Lost() == nil && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if lost := lock.Lost(); lost == nil || !strings.Contains(lost.Error(), "lease expired") {
		t.Fatalf("the holder's reason was not recorded first: %v", lost)
	}

	// Now the writes start failing, which is what a gone holder does to them.
	h.handle.failWrites()
	time.Sleep(200 * time.Millisecond) // several beat intervals

	final := lock.Lost()
	if !strings.Contains(final.Error(), "lease expired") {
		t.Errorf("a failing heartbeat overwrote the holder's own account of why the lock went.\n got: %v\nThe write failing is a consequence of the holder being gone, not a second cause.", final)
	}
	if strings.Contains(final.Error(), "heartbeat") {
		t.Errorf("the loss now reports the heartbeat rather than the reason: %v", final)
	}
}
