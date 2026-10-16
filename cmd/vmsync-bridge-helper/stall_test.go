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

// stallRig drives superviseLoop a beat at a time with a progress sampler a
// test controls, which is the only way the "N beats with nothing" rule is
// provable without spending N real minutes on each case.
type stallRig struct {
	tick chan time.Time
	wait chan error

	mu      sync.Mutex
	armed   []chan time.Time
	signals []string
	sample  progressSample
	ok      bool
	beats   []int // stalledBeats as the beat line saw it
}

func newStallRig() *stallRig {
	return &stallRig{tick: make(chan time.Time, 8), wait: make(chan error, 1), ok: true}
}

func (r *stallRig) setSample(p progressSample) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sample = p
}

func (r *stallRig) setReadable(ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ok = ok
}

func (r *stallRig) sent() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.signals...)
}

func (r *stallRig) seen() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.beats...)
}

func (r *stallRig) run(stallBeats int, grace time.Duration) <-chan superviseResult {
	return r.runMode(stallBeats, grace, false)
}

func (r *stallRig) runMode(stallBeats int, grace time.Duration, warnOnly bool) <-chan superviseResult {
	clock := superviseClock{
		tick:          r.tick,
		stop:          func() {},
		grace:         grace,
		stallBeats:    stallBeats,
		stallWarnOnly: warnOnly,
		after: func(time.Duration) <-chan time.Time {
			ch := make(chan time.Time, 1)
			r.mu.Lock()
			defer r.mu.Unlock()
			r.armed = append(r.armed, ch)
			return ch
		},
	}
	act := superviseActions{
		beat: func(_ string, _ time.Duration, _ progressSample, stalled int) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.beats = append(r.beats, stalled)
		},
		signal: func(sig string) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.signals = append(r.signals, sig)
			return nil
		},
		state: func() string { return "D" },
		progress: func() (progressSample, bool) {
			r.mu.Lock()
			defer r.mu.Unlock()
			return r.sample, r.ok
		},
	}
	done := make(chan superviseResult, 1)
	go func() { done <- superviseLoop(clock, r.wait, act) }()
	return done
}

// beat fires one tick and waits for the loop to have processed it, so the
// assertions below are about a settled state and not a race.
func (r *stallRig) beat(t *testing.T, want int) {
	t.Helper()
	before := len(r.seen())
	r.tick <- time.Now()
	deadline := time.After(2 * time.Second)
	for {
		if len(r.seen()) > before {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("the loop never processed beat %d", want)
		case <-time.After(time.Millisecond):
		}
	}
}

// THE CENTRAL GUARANTEE: a command that is advancing is never signalled, no
// matter how long it takes. This is the whole reason the bound is a
// progress rule and not a longer clock -- the clock would eventually fire
// on this command, and this must not.
func TestSlowButAdvancingCommandIsNeverSignalled(t *testing.T) {
	r := newStallRig()
	done := r.run(3, time.Second)

	// Twenty beats -- far past any stall bound -- each moving one counter
	// only, because a command may well be advancing on one and not others.
	for i := 1; i <= 20; i++ {
		switch i % 3 {
		case 0:
			r.setSample(progressSample{cpu: uint64(i)})
		case 1:
			r.setSample(progressSample{rw: uint64(i) * 4096})
		default:
			r.setSample(progressSample{blk: uint64(i) * 512})
		}
		r.beat(t, i)
	}
	if sigs := r.sent(); len(sigs) != 0 {
		t.Fatalf("an advancing command was signalled %v after 20 beats", sigs)
	}

	r.wait <- nil
	res := <-done
	if res.Outcome != superviseCompleted {
		t.Errorf("outcome = %q, want %q", res.Outcome, superviseCompleted)
	}
}

// A command doing nothing at all is signalled, but only on the Nth
// consecutive still beat -- not the first, and not before.
func TestStalledCommandIsSignalledOnTheNthBeat(t *testing.T) {
	r := newStallRig()
	done := r.run(3, time.Second)
	r.setSample(progressSample{cpu: 10, rw: 2048, blk: 512})

	// Beat 1 establishes the baseline: there is no previous sample to
	// compare against, so it cannot be a strike.
	r.beat(t, 1)
	// Beats 2, 3 are the first two strikes.
	r.beat(t, 2)
	r.beat(t, 3)
	if sigs := r.sent(); len(sigs) != 0 {
		t.Fatalf("signalled %v before the bound was reached", sigs)
	}
	// Beat 4 is the third consecutive still beat, and the bound.
	r.beat(t, 4)
	if sigs := r.sent(); len(sigs) != 1 || sigs[0] != "TERM" {
		t.Fatalf("signals after the bound = %v, want exactly [TERM]", sigs)
	}
	if seen := r.seen(); seen[0] != 0 || seen[1] != 1 || seen[2] != 2 || seen[3] != 3 {
		t.Errorf("stall counter as the beat line saw it = %v, want 0,1,2,3", seen)
	}

	r.wait <- nil
	res := <-done
	if res.Outcome != superviseStalled {
		t.Errorf("outcome = %q, want %q", res.Outcome, superviseStalled)
	}
	if res.ExitCode != util.ExitSupervisedTimeout {
		t.Errorf("exit = %d, want %d", res.ExitCode, util.ExitSupervisedTimeout)
	}
	if res.StalledBeats != 3 {
		t.Errorf("StalledBeats = %d, want 3", res.StalledBeats)
	}
	if line := res.Line(); !strings.Contains(line, "outcome=stalled") || !strings.Contains(line, "stalled_beats=3") {
		t.Errorf("verdict line does not say what happened: %q", line)
	}
}

// One moving beat resets the count. A command that pauses and resumes is
// doing exactly what a loaded array makes it do.
func TestProgressResetsTheStallCount(t *testing.T) {
	r := newStallRig()
	done := r.run(3, time.Second)
	r.setSample(progressSample{cpu: 1})

	r.beat(t, 1) // baseline
	r.beat(t, 2) // strike 1
	r.beat(t, 3) // strike 2
	r.setSample(progressSample{cpu: 2})
	r.beat(t, 4) // moved: back to zero
	r.setSample(progressSample{cpu: 2})
	r.beat(t, 5) // strike 1 again
	r.beat(t, 6) // strike 2
	if sigs := r.sent(); len(sigs) != 0 {
		t.Fatalf("signalled %v although progress reset the count", sigs)
	}

	r.wait <- nil
	<-done
}

// An unreadable /proc must never be the reason something is killed. A host
// without CONFIG_TASK_IO_ACCOUNTING, or one where the process is gone, is
// a host where this bound simply does not apply.
func TestUnreadableProgressNeverSignals(t *testing.T) {
	r := newStallRig()
	done := r.run(3, time.Second)
	r.setReadable(false)

	for i := 1; i <= 10; i++ {
		r.beat(t, i)
	}
	if sigs := r.sent(); len(sigs) != 0 {
		t.Fatalf("signalled %v on a host where progress cannot be measured", sigs)
	}

	r.wait <- nil
	<-done
}

// stallBeats == 0 means the bound is off, which is how every command that
// must never be abandoned part-way is configured.
func TestStallBoundOffNeverSignals(t *testing.T) {
	r := newStallRig()
	done := r.run(0, time.Second)
	r.setSample(progressSample{cpu: 7})

	for i := 1; i <= 10; i++ {
		r.beat(t, i)
	}
	if sigs := r.sent(); len(sigs) != 0 {
		t.Fatalf("signalled %v with the stall bound disabled", sigs)
	}

	r.wait <- nil
	<-done
}

// A stall escalates exactly as a timeout does -- TERM, grace, KILL -- which
// is the point of routing it through the same state machine rather than
// giving it one of its own.
func TestStallEscalatesToKill(t *testing.T) {
	r := newStallRig()
	done := r.run(1, time.Second)
	r.setSample(progressSample{cpu: 3})

	r.beat(t, 1) // baseline
	r.beat(t, 2) // one still beat, and the bound

	// The grace deadline the TERM armed.
	deadline := time.After(2 * time.Second)
	for {
		r.mu.Lock()
		n := len(r.armed)
		var ch chan time.Time
		if n > 0 {
			ch = r.armed[0]
		}
		r.mu.Unlock()
		if ch != nil {
			ch <- time.Now()
			break
		}
		select {
		case <-deadline:
			t.Fatal("a stall armed no grace deadline, so it can never escalate")
		case <-time.After(time.Millisecond):
		}
	}

	want := []string{"TERM", "KILL"}
	for {
		got := r.sent()
		if len(got) >= 2 {
			if got[0] != want[0] || got[1] != want[1] {
				t.Fatalf("signals = %v, want %v", got, want)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatalf("a stalled command was never escalated past %v", got)
		case <-time.After(time.Millisecond):
		}
	}

	r.wait <- nil
	<-done
}

// WARN-ONLY: measured exactly the same, signalled never. This is what the
// commit gets, and the guarantee has two halves -- no signal, and a count
// that still reaches the verdict. Dropping either makes it useless: one
// kills a command that must not be killed, the other watches in silence.
func TestWarnOnlyStallReportsButNeverSignals(t *testing.T) {
	r := newStallRig()
	done := r.runMode(2, time.Second, true)
	r.setSample(progressSample{cpu: 5, rw: 1024})

	for i := 1; i <= 8; i++ {
		r.beat(t, i)
	}
	if sigs := r.sent(); len(sigs) != 0 {
		t.Fatalf("a warn-only command was signalled %v -- for the commit this corrupts the base image", sigs)
	}
	// The count kept climbing rather than being reset or capped.
	if seen := r.seen(); seen[len(seen)-1] < 7 {
		t.Errorf("stall count stopped climbing: %v", seen)
	}

	r.wait <- nil
	res := <-done
	// It COMPLETED -- nothing interfered with it -- and still says it
	// spent most of that time doing nothing.
	if res.Outcome != superviseCompleted {
		t.Errorf("outcome = %q, want %q: warn-only must not change the outcome", res.Outcome, superviseCompleted)
	}
	if res.StalledBeats < 2 {
		t.Errorf("StalledBeats = %d, want at least the bound (2): a stall nobody was told about is a stall nobody can act on", res.StalledBeats)
	}
	if line := res.Line(); !strings.Contains(line, "stalled_beats=") {
		t.Errorf("verdict does not mention the stall: %q", line)
	}
}

// A warn-only command that never stalls says nothing about stalling, so the
// field stays out of the line it would otherwise clutter.
func TestWarnOnlyQuietWhenAdvancing(t *testing.T) {
	r := newStallRig()
	done := r.runMode(2, time.Second, true)
	for i := 1; i <= 6; i++ {
		r.setSample(progressSample{blk: uint64(i) * 4096})
		r.beat(t, i)
	}
	r.wait <- nil
	res := <-done
	if res.StalledBeats != 0 {
		t.Errorf("StalledBeats = %d for a command that advanced every beat", res.StalledBeats)
	}
	if line := res.Line(); strings.Contains(line, "stalled_beats=") {
		t.Errorf("verdict mentions a stall that never happened: %q", line)
	}
}

// moved() compares for inequality, not growth, because the set of processes
// being summed can shrink -- and a sum that fell is still a sign of life.
func TestProgressSampleMovedDetectsAFallingSum(t *testing.T) {
	big := progressSample{cpu: 100, rw: 1 << 20, blk: 1 << 20}
	small := progressSample{cpu: 10, rw: 4096, blk: 0}
	if !small.moved(big) {
		t.Error("a sum that fell was not treated as movement")
	}
	if big.moved(big) {
		t.Error("an unchanged sample was treated as movement")
	}
}
