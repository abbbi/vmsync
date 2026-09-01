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
