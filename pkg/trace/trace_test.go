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

package trace

import (
	"testing"
	"time"
)

// WarningCount/ErrorCount are process-lifetime counters with no reset (see
// their own doc comments), so these tests assert the *delta* Warning/Error
// calls produce rather than an absolute value -- keeping them correct
// regardless of test execution order or repeated runs (-count=N) within the
// same process.

func TestWarningCountIncrementsOnWarning(t *testing.T) {
	before := WarningCount()

	Warning("test warning one")
	Warning("test warning two", "key", "value")

	if got := WarningCount() - before; got != 2 {
		t.Errorf("WarningCount() delta = %d, want 2", got)
	}
}

func TestErrorCountIncrementsOnError(t *testing.T) {
	before := ErrorCount()

	Error("test error one")

	if got := ErrorCount() - before; got != 1 {
		t.Errorf("ErrorCount() delta = %d, want 1", got)
	}
}

func TestInfoDebugDoNotAffectWarningOrErrorCount(t *testing.T) {
	warningBefore := WarningCount()
	errorBefore := ErrorCount()

	Info("test info")
	Debug("test debug")
	DebugError("test debug error", nil)

	if got := WarningCount() - warningBefore; got != 0 {
		t.Errorf("WarningCount() delta after Info/Debug = %d, want 0", got)
	}
	if got := ErrorCount() - errorBefore; got != 0 {
		t.Errorf("ErrorCount() delta after Info/Debug = %d, want 0", got)
	}
}

// feed drives a QuietWatch with a scripted sequence of silences, one per
// tick, and reports what it did. No clock, no peer: the whole point of the
// function's shape is that its boundary is decidable from arguments alone.
func feed(t *testing.T, grace, bound time.Duration, quiets ...time.Duration) (gaveUp bool, remarks []time.Duration) {
	t.Helper()
	tick := make(chan time.Time, len(quiets))
	for range quiets {
		tick <- time.Time{}
	}
	close(tick)
	i := 0
	quietFor := func(time.Time) time.Duration {
		q := quiets[i]
		i++
		return q
	}
	gaveUp = QuietWatch(make(chan struct{}), tick, quietFor, grace, bound,
		func(q time.Duration) { remarks = append(remarks, q) })
	return gaveUp, remarks
}

// TestQuietWatchGivesUpExactlyAtTheBound pins the comparison that decides
// whether a bound is a bound.
//
// MUTATION CAUGHT: `>` for `>=`. With a strict comparison a silence sitting
// precisely on the bound is remarked on instead, and the wait runs to the
// next tick -- which turns a 3-minute bound into "3 minutes, or 4, depending
// on where the ticks happen to fall".
func TestQuietWatchGivesUpExactlyAtTheBound(t *testing.T) {
	if gaveUp, _ := feed(t, 90*time.Second, 3*time.Minute, 3*time.Minute); !gaveUp {
		t.Error("a silence exactly at the bound did not trip it")
	}
	if gaveUp, remarks := feed(t, 90*time.Second, 3*time.Minute, 3*time.Minute-time.Nanosecond); gaveUp {
		t.Error("a silence one nanosecond short of the bound tripped it")
	} else if len(remarks) != 1 {
		t.Errorf("a silence past grace but short of the bound produced %d remarks, want 1", len(remarks))
	}
}

// TestQuietWatchStaysSilentForAHealthyPass is the expensive mutation: a watch
// that remarks on, or gives up on, a peer that is reporting normally.
//
// The ladder it protects is ProgressInterval < grace < bound. A peer beating
// once per interval is never quiet for grace, so a healthy pass of any length
// produces no line from this side at all.
func TestQuietWatchStaysSilentForAHealthyPass(t *testing.T) {
	healthy := make([]time.Duration, 0, 120)
	for i := 0; i < 120; i++ {
		healthy = append(healthy, 61*time.Second) // one beat, plus a second of slack
	}
	gaveUp, remarks := feed(t, 90*time.Second, 3*time.Minute, healthy...)
	if gaveUp {
		t.Error("gave up on a peer that was beating every interval")
	}
	if len(remarks) != 0 {
		t.Errorf("remarked %d times about a peer that never went quiet", len(remarks))
	}
}

// TestQuietWatchRemarksBeforeItGivesUp keeps the operator-facing half honest:
// a failure at the bound must be preceded by the waiting having been said out
// loud, so the error is the end of a story rather than the whole of it.
func TestQuietWatchRemarksBeforeItGivesUp(t *testing.T) {
	gaveUp, remarks := feed(t, 90*time.Second, 3*time.Minute,
		time.Minute, 2*time.Minute, 3*time.Minute)
	if !gaveUp {
		t.Fatal("a silence reaching the bound did not trip it")
	}
	if len(remarks) != 1 || remarks[0] != 2*time.Minute {
		t.Errorf("remarks = %v, want exactly one at 2m: below grace must be silent, at the bound must fail rather than remark", remarks)
	}
}

// TestQuietWatchPrefersAStopOverAPendingTick is the race that fails a healthy
// run at the last instant: the answer arrives, the caller closes stop, and a
// tick is already pending. select picks between them at random, so without
// the second check the watch declares a stall on an exchange that already
// succeeded. Looped, because that is a coin flip.
func TestQuietWatchPrefersAStopOverAPendingTick(t *testing.T) {
	for i := 0; i < 200; i++ {
		tick := make(chan time.Time, 1)
		tick <- time.Time{}
		stop := make(chan struct{})
		close(stop)
		remarked := false
		gaveUp := QuietWatch(stop, tick, func(time.Time) time.Duration { return time.Hour },
			time.Second, time.Minute, func(time.Duration) { remarked = true })
		if gaveUp || remarked {
			t.Fatalf("round %d: acted on a tick after the wait had already finished (gaveUp=%v remarked=%v)", i, gaveUp, remarked)
		}
	}
}

// TestQuietWatchToleratesNoRemark keeps the remark optional, so a caller that
// only wants the bound is not forced to invent a log line for it.
func TestQuietWatchToleratesNoRemark(t *testing.T) {
	tick := make(chan time.Time, 1)
	tick <- time.Time{}
	close(tick)
	if !QuietWatch(make(chan struct{}), tick, func(time.Time) time.Duration { return time.Hour }, time.Second, time.Minute, nil) {
		t.Error("a silence past the bound did not trip it when remark was nil")
	}
}
