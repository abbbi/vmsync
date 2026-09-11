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
	"errors"
	"fmt"
	"testing"
	"time"

	// Embeds the IANA timezone database so the multi-zone calendar tests below
	// run on any host, including one without tzdata installed.
	_ "time/tzdata"

	"vmsync/pkg/runresult"
)

// vmNames is a spread of plausible domain names. Enough of them that a
// function which genuinely distributes will visibly fill the interval, and
// few enough that the assertions below stay honest about what that proves.
func vmNames(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("vm%02d.example.internal", i))
	}
	return out
}

// stagger exists to keep every entry from becoming due at once on agent
// start. It spent its whole life not doing that: interval is a Duration, so
// it is in NANOSECONDS, and a 32-bit hash tops out at 4294967295 -- 4.29
// seconds. Modulo an interval longer than that returned the hash untouched,
// identically for a 30-second cadence and a 24-hour one.
//
// The assertion is deliberately about SPREAD rather than about any particular
// value: what the scheduler needs is that offsets fill the interval, not that
// a given VM lands anywhere specific.
func TestStaggerSpreadsAcrossTheWholeInterval(t *testing.T) {
	for _, interval := range []time.Duration{
		30 * time.Second,
		5 * time.Minute,
		time.Hour,
		24 * time.Hour,
	} {
		t.Run(interval.String(), func(t *testing.T) {
			var low, high int
			for _, vm := range vmNames(40) {
				got := stagger(vm, interval)
				if got < 0 || got >= interval {
					t.Fatalf("stagger(%q, %v) = %v, out of range [0, %v)", vm, interval, got, interval)
				}
				if got < interval/2 {
					low++
				} else {
					high++
				}
			}
			// Both halves populated. With the 32-bit version every offset
			// for any interval above ~4.3s landed in the first fraction of
			// a percent, so high would be 0 and this fails loudly.
			if low == 0 || high == 0 {
				t.Errorf("interval %v: offsets do not span it (%d below the midpoint, %d above) -- stagger is not distributing, so every entry becomes due at once on agent start",
					interval, low, high)
			}
		})
	}
}

// The specific regression, stated as itself: two intervals that differ by
// three orders of magnitude must not produce identical offsets. This is the
// cheapest possible statement of the bug, and the one that reads clearly in a
// failure report.
func TestStaggerDependsOnTheInterval(t *testing.T) {
	same := 0
	names := vmNames(20)
	for _, vm := range names {
		if stagger(vm, 30*time.Second) == stagger(vm, 24*time.Hour) {
			same++
		}
	}
	// A coincidental collision is possible; twenty of them is the bug.
	if same == len(names) {
		t.Errorf("every VM got the same offset for a 30s and a 24h interval -- the interval is not reaching the calculation (a 32-bit hash caps at 4.29s, so the modulo is a no-op for any realistic cadence)")
	}
}

// Stable per VM, because the doc comment on due() promises a given VM keeps
// its slot across agent restarts. Nothing persists nextRun, so that promise
// rests entirely on this function being pure.
func TestStaggerIsDeterministic(t *testing.T) {
	for _, vm := range vmNames(10) {
		first := stagger(vm, time.Hour)
		for i := 0; i < 5; i++ {
			if got := stagger(vm, time.Hour); got != first {
				t.Fatalf("stagger(%q, 1h) returned %v then %v -- it must be pure, or a VM's slot moves on every restart", vm, first, got)
			}
		}
	}
}

// A zero or negative interval is not a caller error to reject here: launchDue
// already skips entries with IntervalSeconds <= 0 before due() is reached.
// This guard exists so the modulo below it cannot divide by zero, and the
// only sane answer is no offset at all.
func TestStaggerHandlesANonPositiveInterval(t *testing.T) {
	for _, interval := range []time.Duration{0, -time.Second} {
		if got := stagger("web01", interval); got != 0 {
			t.Errorf("stagger(web01, %v) = %v, want 0", interval, got)
		}
	}
}

// effectiveMaxConcurrent is already covered by TestEffectiveMaxConcurrent in
// standalone_test.go, which is where the --standalone max_concurrent_syncs
// setting is tested alongside it. Not duplicated here.

// tail bounds what a chatty failure loop can put in a report, and cuts at a
// line boundary so the result does not start mid-word.
func TestTail(t *testing.T) {
	if got := tail("short", 100); got != "short" {
		t.Errorf("tail of a string under the limit = %q, want it unchanged", got)
	}
	got := tail("aaaa\nbbbb\ncccc\ndddd", 10)
	if len(got) > 10 {
		t.Errorf("tail returned %d bytes, over the 10-byte limit: %q", len(got), got)
	}
	if got != "cccc\ndddd" {
		t.Errorf("tail = %q, want it cut at a line boundary", got)
	}
}

// The precedence rules in classifyRunResult, each of which exists because the
// alternative is a specific wrong thing told to an operator.
func TestClassifyRunResult(t *testing.T) {
	const mine = "run-aaa"
	frozen := runresult.Result{VM: "db01", RunID: mine, FSThawFailed: true}

	for _, tc := range []struct {
		name     string
		rr       runresult.Result
		err      error
		wantKind string
	}{
		{"a clean run", runresult.Result{VM: "db01", RunID: mine}, nil, resultClean},
		{"a degraded run", frozen, nil, resultDegraded},
		{
			// An unreadable file might have said the guest is frozen. Reading
			// it as clean is the one answer that is certainly wrong.
			"an unreadable file outranks everything",
			frozen, errors.New("unexpected end of JSON input"), resultUnreadable,
		},
		{
			// A crash can leave one behind. Blaming this run for another's
			// frozen guest sends an operator to the wrong VM.
			"a file from another run is ignored even when it reports a degradation",
			runresult.Result{VM: "web01", RunID: "run-bbb", FSThawFailed: true}, nil, resultStale,
		},
		{
			// vmsync writes whatever -run-id it was given, and a hand-run
			// vmsync is given none. Refusing those would drop the report from
			// every run an operator started themselves.
			"an empty run id is accepted, not treated as stale",
			runresult.Result{VM: "db01", FSThawFailed: true}, nil, resultDegraded,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyRunResult(tc.rr, tc.err, mine)
			if got.kind != tc.wantKind {
				t.Errorf("kind = %q, want %q", got.kind, tc.wantKind)
			}
			// Only a degradation carries a reason, and it must never be empty:
			// a warning pill with nothing to act on is worse than none.
			if (got.reason != "") != (tc.wantKind == resultDegraded) {
				t.Errorf("reason = %q for a %s verdict", got.reason, got.kind)
			}
		})
	}
}

// verifyDue decides which syncs also carry -verify, and the cases below are
// the ones that make it worth having rather than a plain interval.
func TestVerifyDue(t *testing.T) {
	s := &Scheduler{nextVerify: map[string]time.Time{}}
	now := time.Unix(1_800_000_000, 0)

	t.Run("no verify mode means never, whatever the cadence says", func(t *testing.T) {
		// A cadence for something the profile does not do. The UI refuses this
		// combination, but the agent must not depend on that: a standalone
		// schedule file has no UI in front of it.
		e := ScheduleEntry{VM: "web01", VerifyIntervalSeconds: 3600}
		if s.verifyDue(e, now) {
			t.Error("verifyDue = true with no verify mode; there is nothing to run")
		}
	})

	t.Run("no cadence means every sync, which is the old behaviour", func(t *testing.T) {
		// A profile naming a verify mode and no cadence is asking to be
		// verified, and "how often: unspecified" reads as "every time". The
		// other reading is a check somebody believes is running and is not.
		e := ScheduleEntry{VM: "web01", Profile: SyncProfile{Verify: "fast"}}
		for i := 0; i < 3; i++ {
			if !s.verifyDue(e, now.Add(time.Duration(i)*time.Minute)) {
				t.Errorf("run %d did not verify; interval 0 must mean every sync", i)
			}
		}
	})

	t.Run("the first sighting does not verify, then the cadence holds", func(t *testing.T) {
		s := &Scheduler{nextVerify: map[string]time.Time{}}
		e := ScheduleEntry{VM: "db01", Profile: SyncProfile{Verify: "fast"}, VerifyIntervalSeconds: 3600}

		// Not on first sight: an agent restart would otherwise verify every
		// VM it manages at once, which is the burst the stagger exists to
		// avoid. Skipping one cycle costs at most one interval.
		if s.verifyDue(e, now) {
			t.Error("verified on the first sighting; a restart would verify the whole estate at once")
		}
		// Still not due a minute later.
		if s.verifyDue(e, now.Add(time.Minute)) {
			t.Error("verified a minute after being scheduled an hour out")
		}
		// Due once the interval has passed. The stagger is under an hour by
		// construction (it is interval-modulo), so two hours is past it.
		if !s.verifyDue(e, now.Add(2*time.Hour)) {
			t.Fatal("did not verify two hours into a one-hour cadence")
		}
		// And having verified, not again immediately.
		if s.verifyDue(e, now.Add(2*time.Hour+time.Minute)) {
			t.Error("verified twice within one interval")
		}
	})

	t.Run("a long outage does not owe a backlog of verifies", func(t *testing.T) {
		// Same rule markRunning follows for syncs: next is now+interval, not
		// previous+interval. An agent down for a week must come back and
		// verify once, not seven times.
		s := &Scheduler{nextVerify: map[string]time.Time{}}
		e := ScheduleEntry{VM: "db01", Profile: SyncProfile{Verify: "fast"}, VerifyIntervalSeconds: 86400}
		s.verifyDue(e, now) // first sighting, schedules it
		if !s.verifyDue(e, now.Add(7*24*time.Hour)) {
			t.Fatal("did not verify after a week")
		}
		if s.verifyDue(e, now.Add(7*24*time.Hour+time.Hour)) {
			t.Error("verified again an hour later; the missed days were owed as a backlog")
		}
	})

	t.Run("different VMs are staggered rather than all due together", func(t *testing.T) {
		// The point of the stagger. A 24h verify cadence across an estate
		// would otherwise fall due within the same minute of the same night,
		// queue against max_concurrent_syncs and the target's slots, and turn
		// one pass into a multi-hour backlog.
		s := &Scheduler{nextVerify: map[string]time.Time{}}
		const interval = 24 * time.Hour
		for _, vm := range []string{"web01", "db01", "mail01", "app01", "dns01", "ldap01"} {
			s.verifyDue(ScheduleEntry{VM: vm, Profile: SyncProfile{Verify: "fast"},
				VerifyIntervalSeconds: int(interval.Seconds())}, now)
		}
		seen := map[time.Time]bool{}
		for _, at := range s.nextVerify {
			seen[at] = true
			if at.Before(now) || !at.Before(now.Add(interval)) {
				t.Errorf("scheduled at %v, outside [now, now+interval)", at)
			}
		}
		if len(seen) < 4 {
			t.Errorf("6 VMs got %d distinct verify times; they are not being spread", len(seen))
		}
	})
}

// calVM is an entry whose verify cadence is a calendar rather than an
// interval: the first Sunday of the month, 02:00 to 12:00.
func calVM(vm string) ScheduleEntry {
	return ScheduleEntry{
		VM:           vm,
		Profile:      SyncProfile{Verify: "fast"},
		VerifyDays:   "Sun *-*-01..07",
		VerifyWindow: "02:00-12:00",
	}
}

// newSched is a Scheduler with only the maps verifyDue touches, for the
// calendar cases.
func newSched() *Scheduler {
	return &Scheduler{
		nextVerify:   map[string]time.Time{},
		lastVerified: map[string]time.Time{},
	}
}

func schedTime(t *testing.T, s string) time.Time {
	t.Helper()
	// Local, because that is what verifyDueByCalendar is given: the window
	// means the quiet hours where the disks are, so the agent reads its own
	// clock and the console displays the zone it is told.
	ts, err := time.ParseInLocation("2006-01-02 15:04", s, time.Local)
	if err != nil {
		t.Fatalf("bad test time %q: %v", s, err)
	}
	return ts
}

// The calendar form of the verify cadence. Separate from the interval cases
// above because the behaviour deliberately DIFFERS in two ways -- no stagger,
// no first-sighting skip -- and a missed window is dropped rather than owed.
func TestVerifyDueByCalendar(t *testing.T) {
	t.Run("a sync inside the window verifies, and only the first one does", func(t *testing.T) {
		s := newSched()
		e := calVM("web01")
		// 1 March 2026 is a Sunday, and day 1 is inside 01..07.
		if !s.verifyDue(e, schedTime(t, "2026-03-01 02:30")) {
			t.Fatal("the first sync inside the window did not verify")
		}
		// A ten-hour window and a short sync cadence means dozens of syncs
		// land in it. Verifying on each would turn a monthly read-back into a
		// morning of them.
		for _, at := range []string{"2026-03-01 02:31", "2026-03-01 06:00", "2026-03-01 11:59"} {
			if s.verifyDue(e, schedTime(t, at)) {
				t.Errorf("verified again at %s; one occurrence must fire once", at)
			}
		}
	})

	t.Run("the first sighting DOES verify, unlike the interval form", func(t *testing.T) {
		// The opposite of the interval path on purpose. There, skipping the
		// first sighting costs one cycle and breaks up a restart burst. Here
		// the window IS the burst control -- the operator already said where
		// the load may land -- and skipping a cycle would cost a whole month.
		s := newSched()
		if !s.verifyDue(calVM("db01"), schedTime(t, "2026-03-01 02:00")) {
			t.Error("did not verify on first sight; with a monthly window that defers the verify by a month")
		}
	})

	t.Run("syncs outside the window do not verify", func(t *testing.T) {
		s := newSched()
		e := calVM("web01")
		for _, tc := range []struct{ at, why string }{
			{"2026-03-01 01:59", "before the window opens"},
			{"2026-03-01 12:00", "the end is excluded"},
			{"2026-03-01 18:00", "after the window closes"},
			{"2026-03-08 03:00", "the SECOND Sunday, not the first"},
			{"2026-03-02 03:00", "a Monday inside 01..07"},
		} {
			if s.verifyDue(e, schedTime(t, tc.at)) {
				t.Errorf("verified at %s (%s)", tc.at, tc.why)
			}
		}
	})

	t.Run("the next month's window fires again", func(t *testing.T) {
		s := newSched()
		e := calVM("web01")
		if !s.verifyDue(e, schedTime(t, "2026-03-01 03:00")) {
			t.Fatal("March did not verify")
		}
		// 5 April 2026 is the first Sunday. A stale lastVerified would make
		// this the last verify the VM ever got.
		if !s.verifyDue(e, schedTime(t, "2026-04-05 03:00")) {
			t.Error("April did not verify; the occurrence start is not advancing")
		}
	})

	t.Run("a missed window is dropped, not owed", func(t *testing.T) {
		// The rule the operator asked for: an agent down for its window has
		// not created a debt for the scheduler to repay at the worst possible
		// moment. It is a person's problem to notice, which is what the
		// report is for.
		s := newSched()
		e := calVM("web01")
		// From a cold start, with no verify ever recorded.
		if s.verifyDue(e, schedTime(t, "2026-03-02 09:00")) {
			t.Error("verified the morning after a missed window; a missed occurrence must not back-fire")
		}
		// And it does not verify every sync for the rest of the month either.
		for _, at := range []string{"2026-03-10 09:00", "2026-03-25 22:00"} {
			if s.verifyDue(e, schedTime(t, at)) {
				t.Errorf("verified at %s, outside any window", at)
			}
		}
		// April's window still fires normally.
		if !s.verifyDue(e, schedTime(t, "2026-04-05 04:00")) {
			t.Error("April did not verify after March was missed")
		}
	})

	t.Run("a window missed AFTER a successful one is still not owed", func(t *testing.T) {
		// The case the cold-start version above cannot reach, and the one an
		// implementation is most likely to get wrong: an overdue VM is exactly
		// what tempts a "well, it has been five weeks" fallback. That fallback
		// is what the operator refused -- it drags a full-image read on both
		// sides across a working day, at the moment the estate is least able
		// to absorb it, to repay a window a person is better placed to notice.
		s := newSched()
		e := calVM("web01")
		// 1 February 2026 is a Sunday: February's window, verified.
		if !s.verifyDue(e, schedTime(t, "2026-02-01 03:00")) {
			t.Fatal("February did not verify")
		}
		// The agent is down for the whole of March's window. It comes back
		// having gone more than a month without a verify, and must still wait.
		for _, tc := range []struct{ at, why string }{
			{"2026-03-02 09:00", "the morning after the missed window"},
			{"2026-03-20 14:00", "three weeks overdue"},
			{"2026-04-04 23:00", "an hour before April's Sunday, and five weeks overdue"},
			{"2026-04-05 01:59", "a minute before April's window opens"},
		} {
			if s.verifyDue(e, schedTime(t, tc.at)) {
				t.Errorf("verified at %s (%s); a missed window must not be owed", tc.at, tc.why)
			}
		}
		// And April's own window still fires, so waiting is not the same as
		// giving up.
		if !s.verifyDue(e, schedTime(t, "2026-04-05 02:00")) {
			t.Error("April did not verify; skipping a window must not disable the schedule")
		}
	})

	t.Run("VMs are not staggered: the window already spreads them", func(t *testing.T) {
		// Deliberately unlike the interval path. Staggering inside a window
		// the operator chose would push some VMs past its end and skip them
		// for the month.
		s := newSched()
		for _, vm := range []string{"web01", "db01", "mail01", "app01"} {
			if !s.verifyDue(calVM(vm), schedTime(t, "2026-03-01 02:00")) {
				t.Errorf("%s did not verify at the window's open", vm)
			}
		}
	})

	t.Run("no verify mode means never, whatever the calendar says", func(t *testing.T) {
		s := newSched()
		e := calVM("web01")
		e.Profile.Verify = ""
		if s.verifyDue(e, schedTime(t, "2026-03-01 03:00")) {
			t.Error("verified with no verify mode; there is nothing to run")
		}
	})

	t.Run("a window with no days means every day", func(t *testing.T) {
		s := newSched()
		e := ScheduleEntry{VM: "web01", Profile: SyncProfile{Verify: "fast"},
			VerifyWindow: "02:00-04:00"}
		if !s.verifyDue(e, schedTime(t, "2026-03-04 03:00")) {
			t.Fatal("a Wednesday did not verify under a days-less window")
		}
		if !s.verifyDue(e, schedTime(t, "2026-03-05 03:00")) {
			t.Error("the next day did not verify; a daily window must fire daily")
		}
	})

	t.Run("days with no window means once that day", func(t *testing.T) {
		s := newSched()
		e := ScheduleEntry{VM: "web01", Profile: SyncProfile{Verify: "fast"},
			VerifyDays: "Sun"}
		if !s.verifyDue(e, schedTime(t, "2026-03-01 09:00")) {
			t.Fatal("Sunday did not verify")
		}
		if s.verifyDue(e, schedTime(t, "2026-03-01 20:00")) {
			t.Error("verified twice on one Sunday")
		}
		if s.verifyDue(e, schedTime(t, "2026-03-04 09:00")) {
			t.Error("verified on a Wednesday")
		}
		if !s.verifyDue(e, schedTime(t, "2026-03-08 09:00")) {
			t.Error("the next Sunday did not verify")
		}
	})

	t.Run("an unparsable calendar does not verify, and does not panic", func(t *testing.T) {
		// The remaining readings are both bad: verifying every sync turns a
		// typo into a full-image read on both sides forever. This takes the
		// recoverable one and logs it on every run.
		s := newSched()
		e := calVM("web01")
		e.VerifyDays = "Frunday"
		if s.verifyDue(e, schedTime(t, "2026-03-01 03:00")) {
			t.Error("verified on a calendar that does not parse")
		}
	})

	t.Run("a nil lastVerified map does not panic the scheduler goroutine", func(t *testing.T) {
		// A Scheduler built as a literal, which every test above does.
		// Panicking here would stop replicating the whole estate.
		s := &Scheduler{nextVerify: map[string]time.Time{}}
		if !s.verifyDue(calVM("web01"), schedTime(t, "2026-03-01 03:00")) {
			t.Error("did not verify with a lazily-created map")
		}
	})

	t.Run("the interval form is untouched when no calendar is set", func(t *testing.T) {
		// The calendar branch must be reached only by entries that asked for
		// it. An entry with no verify_days anywhere keeps the interval
		// behaviour exactly, stagger and first-sighting skip included.
		s := newSched()
		e := ScheduleEntry{VM: "db01", Profile: SyncProfile{Verify: "fast"}, VerifyIntervalSeconds: 3600}
		now := time.Unix(1_800_000_000, 0)
		if s.verifyDue(e, now) {
			t.Error("verified on first sight; the interval path skips it")
		}
		if !s.verifyDue(e, now.Add(2*time.Hour)) {
			t.Error("the interval path stopped working")
		}
	})
}

// verifyDueByCalendar claims the occurrence at the LAUNCH decision, because
// otherwise the 10s tick would set -verify on every sync across a ten-hour
// window. That claim is a promise the run has not kept yet, and a run that
// fails or stands down keeps none of it.
//
// Since a missed window is deliberately never made up, one transient failure
// at 02:05 used to cost the VM its whole MONTH of verification.
func TestFailedRunGivesBackTheVerifyWindow(t *testing.T) {
	verifying := syncPlan{SyncRequest: SyncRequest{Profile: SyncProfile{Verify: "fast"}}}

	t.Run("a failed run re-arms the window for the next sync inside it", func(t *testing.T) {
		s := newSched()
		e := calVM("web01")
		if !s.verifyDue(e, schedTime(t, "2026-03-01 02:00")) {
			t.Fatal("the first sync in the window did not verify")
		}
		// Without the release, this second sync would not verify and the
		// month's occurrence would be spent on a run that failed.
		s.releaseVerifyOccurrence(e, verifying)
		if !s.verifyDue(e, schedTime(t, "2026-03-01 02:15")) {
			t.Error("the next sync in the window did not verify after the first failed")
		}
	})

	t.Run("giving it back does not reopen a window that has closed", func(t *testing.T) {
		// Re-arming must not become the interval backstop the operator
		// refused: outside the window nothing fires, however overdue.
		s := newSched()
		e := calVM("web01")
		s.verifyDue(e, schedTime(t, "2026-03-01 11:59"))
		s.releaseVerifyOccurrence(e, verifying)
		for _, at := range []string{"2026-03-01 12:00", "2026-03-02 09:00", "2026-03-20 03:00"} {
			if s.verifyDue(e, schedTime(t, at)) {
				t.Errorf("verified at %s, outside the window", at)
			}
		}
	})

	t.Run("a run that was not going to verify releases nothing", func(t *testing.T) {
		// The plan is the authoritative record of what the command line
		// carried. A non-verifying run must not clear a claim made by the
		// verifying run that is still in flight beside it.
		s := newSched()
		e := calVM("web01")
		if !s.verifyDue(e, schedTime(t, "2026-03-01 02:00")) {
			t.Fatal("the first sync did not verify")
		}
		s.releaseVerifyOccurrence(e, syncPlan{}) // Profile.Verify == ""
		if s.verifyDue(e, schedTime(t, "2026-03-01 03:00")) {
			t.Error("a non-verifying run gave back another run's claim")
		}
	})

	t.Run("the interval form is not touched", func(t *testing.T) {
		// releaseVerifyOccurrence is calendar-only. The interval path comes
		// round again in an hour, so there is nothing worth undoing, and
		// clearing nextVerify would hand it a retry it never had.
		s := newSched()
		e := ScheduleEntry{VM: "db01", Profile: SyncProfile{Verify: "fast"}, VerifyIntervalSeconds: 3600}
		now := time.Unix(1_800_000_000, 0)
		s.verifyDue(e, now)
		if !s.verifyDue(e, now.Add(2*time.Hour)) {
			t.Fatal("the interval path did not come due")
		}
		s.releaseVerifyOccurrence(e, verifying)
		if s.verifyDue(e, now.Add(2*time.Hour+time.Minute)) {
			t.Error("releasing reset an interval cadence it has no business touching")
		}
	})
}

// agentZones are the zones the verify calendar is exercised against.
//
// Driven IN-PROCESS rather than by a CI job per TZ. The scheduler takes `now`
// as a parameter and never reads the clock itself, so a time already in the
// target zone exercises exactly the same code -- and doing it in one process
// costs one test run instead of six, needs no tzdata on the runner, and makes
// adding a zone a one-line change rather than a workflow edit.
//
// The TZ matrix in .github/workflows/ci.yml is not a duplicate of this: it
// covers the other half, where production gets its time from time.Now() in
// the host's zone rather than from a caller.
var agentZones = []string{
	"UTC",
	"Europe/Paris",        // 1h DST shift, transition falls ON a Sunday
	"Australia/Lord_Howe", // 30-MINUTE DST shift
	"Pacific/Chatham",     // :45 base offset
	"America/St_Johns",    // :30 base offset
	"Asia/Kolkata",        // :30 base offset, no DST at all
}

func zoneAt(t *testing.T, loc *time.Location, s string) time.Time {
	t.Helper()
	ts, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
	if err != nil {
		t.Fatalf("bad test time %q: %v", s, err)
	}
	return ts
}

// The verify window must behave identically whatever zone the agent's host is
// in, because the host zone is not a setting -- it is whatever the datacentre
// happens to be, and every one of these is somebody's production machine.
//
// Stated as properties over a whole year rather than fixed instants: the point
// is that no zone is special-cased.
func TestVerifyCalendarHoldsInEveryZone(t *testing.T) {
	for _, name := range agentZones {
		t.Run(name, func(t *testing.T) {
			loc, err := time.LoadLocation(name)
			if err != nil {
				t.Fatalf("loading %s failed even with tzdata embedded: %v", name, err)
			}
			s := newSched()
			e := calVM("web01")

			// Walk 2026 at five-minute steps and count the syncs that verify.
			// "First Sunday of the month, 02:00-12:00" is twelve occurrences a
			// year, and a DST day must neither drop one nor double it.
			fires := 0
			for ts := time.Date(2026, 1, 1, 0, 0, 0, 0, loc); ts.Year() == 2026; ts = ts.Add(5 * time.Minute) {
				if s.verifyDue(e, ts) {
					fires++
				}
			}
			if fires != 12 {
				t.Errorf("verified %d times in 2026, want 12 -- one per first Sunday", fires)
			}
		})
	}
}

// A window that crosses midnight belongs to the day it OPENED on, in every
// zone. This is the branch that needs calendar arithmetic rather than
// duration arithmetic, so it is the one a DST bug hides in.
func TestCrossingWindowHoldsInEveryZone(t *testing.T) {
	for _, name := range agentZones {
		t.Run(name, func(t *testing.T) {
			loc, err := time.LoadLocation(name)
			if err != nil {
				t.Fatal(err)
			}
			s := newSched()
			e := ScheduleEntry{
				VM: "db01", Profile: SyncProfile{Verify: "fast"},
				VerifyDays: "Sun", VerifyWindow: "22:00-04:00",
			}
			fires := 0
			for ts := time.Date(2026, 1, 1, 0, 0, 0, 0, loc); ts.Year() == 2026; ts = ts.Add(5 * time.Minute) {
				if s.verifyDue(e, ts) {
					fires++
				}
			}
			// 52 Sundays in 2026. The window opens on each of them and runs
			// into Monday; the Monday tail must not count as its own
			// occurrence, and the spring-forward night must not lose one.
			if fires != 52 {
				t.Errorf("verified %d times in 2026, want 52 -- one per Sunday night", fires)
			}
		})
	}
}
