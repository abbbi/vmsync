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

package remotessh

import (
	"errors"
	"testing"
	"time"

	"vmsync/pkg/util"
)

// A run of failures gives up; a success in the middle of one does not.
//
// Both directions cost something real. Giving up too readily closes a healthy
// connection and fails a running sync over one lost probe on a busy link; never
// giving up is the defect this prober exists to fix.
func TestKeepaliveGivesUpOnlyOnAnUnbrokenRun(t *testing.T) {
	for _, tc := range []struct {
		name     string
		answers  []bool
		strikes  int
		wantGive bool
	}{
		{"every probe answered", []bool{true, true, true, true}, 3, false},
		{"three in a row missed", []bool{false, false, false}, 3, true},
		{"a success resets the count", []bool{false, false, true, false, false}, 3, false},
		{"and the run after a reset still counts", []bool{false, true, false, false, false}, 3, true},
		{"one miss is not a dead peer", []bool{false}, 3, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tick := make(chan time.Time, len(tc.answers))
			for range tc.answers {
				tick <- time.Time{}
			}
			close(tick)

			i := 0
			probe := func() bool {
				v := tc.answers[i]
				i++
				return v
			}
			if got := keepaliveLoop(make(chan struct{}), tick, probe, tc.strikes); got != tc.wantGive {
				t.Errorf("keepaliveLoop = %v, want %v after %v", got, tc.wantGive, tc.answers)
			}
		})
	}
}

// Stopping must not read as a dead peer: closing a client deliberately would
// otherwise close it again and log that the peer stopped answering.

// A tick is left ready alongside the stop on purpose. That is the case the
// implementation got wrong: select picks between two ready cases at random, so
// this failed about half the time it ran -- which is also why it needs -count
// to be trusted.
func TestKeepaliveStopIsNotAFailure(t *testing.T) {
	for i := 0; i < 200; i++ {
		stop := make(chan struct{})
		close(stop)
		tick := make(chan time.Time, 1)
		tick <- time.Time{}
		if keepaliveLoop(stop, tick, func() bool { return false }, 1) {
			t.Fatalf("a stopped prober reported that the peer should be given up on (attempt %d)", i+1)
		}
	}
}

// A stop must also mean "do not probe at all", which suppressing the verdict
// alone does not give: stopKeepalive WAITS for this goroutine, so a probe begun
// after the stop can hold a deliberate Close for the full keepalive timeout --
// ten seconds, and most likely on exactly the dead peer that is being closed
// because it is dead. Looped because the race it guards is decided by select.
func TestKeepaliveStopSkipsThePendingProbe(t *testing.T) {
	for i := 0; i < 200; i++ {
		stop := make(chan struct{})
		close(stop)
		tick := make(chan time.Time, 1)
		tick <- time.Time{}
		probed := 0
		keepaliveLoop(stop, tick, func() bool { probed++; return true }, 1)
		if probed != 0 {
			t.Fatalf("a stopped prober still probed %d time(s) (attempt %d)", probed, i+1)
		}
	}
}

// And a stop that arrives WHILE a probe is in flight, which the tests above
// cannot reach: the verdict is only reached after the probe returns, so without
// the second check the loop still reports a peer it was told to stop watching.
func TestKeepaliveStopDuringAProbeIsNotAFailure(t *testing.T) {
	stop := make(chan struct{})
	tick := make(chan time.Time, 1)
	tick <- time.Time{}
	probe := func() bool {
		close(stop)
		return false
	}
	if keepaliveLoop(stop, tick, probe, 1) {
		t.Error("a peer was given up on by a prober that was stopped mid-probe")
	}
}

// The bound is the whole point: SendRequest has no deadline, so a peer that has
// vanished blocks it for as long as the kernel retransmits -- about fifteen
// minutes on Linux, which is far past the lease at which the target hands the
// lock to somebody else.
func TestProbeIsBoundedByItsOwnTimeout(t *testing.T) {
	if !probeWithin(func() error { return nil }, time.Second) {
		t.Error("a reply that arrived was treated as a miss")
	}
	if probeWithin(func() error { return errors.New("ssh: connection lost") }, time.Second) {
		t.Error("a transport error was treated as a reply")
	}

	start := time.Now()
	got := probeWithin(func() error {
		// A peer that will never answer.
		time.Sleep(10 * time.Second)
		return nil
	}, 80*time.Millisecond)
	elapsed := time.Since(start)
	if got {
		t.Error("a send that never returned was treated as a reply")
	}
	if elapsed > 3*time.Second {
		t.Errorf("the probe took %s to give up on an 80ms timeout, so nothing bounds it", elapsed)
	}
}

// THE PAIRING RULE, as a test rather than as a comment.
//
// This side must notice an unreachable peer BEFORE the target hands the lock to
// somebody else, or a run commits into disks a promoted guest is already
// writing. Tuning either number without the other is exactly the mistake this
// catches, and the two live in different packages, so nothing else would.
func TestDetectionIsFasterThanTheTargetGivesTheLockAway(t *testing.T) {
	worst := keepaliveInterval*keepaliveStrikes + keepaliveTimeout
	if worst >= util.DefaultRemoteLockLease {
		t.Errorf("worst-case detection is %s but the target releases the lock after %s: a driver that notices after the hand-over commits into disks that are no longer its own",
			worst, util.DefaultRemoteLockLease)
	}
	// And it must not be so eager that an ordinary stall trips it. The lease's
	// own floor is the project's statement of how long a healthy driver may go
	// quiet, so detection must not be quicker than that.
	if worst <= util.MinRemoteLockLease {
		t.Errorf("worst-case detection is %s, inside the %s a healthy driver is allowed to stall: a hypervisor pausing the guest would close a working connection",
			worst, util.MinRemoteLockLease)
	}
}
