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

package actionlog

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSink is a journal that lives in memory, plus the ordering log every
// test here asserts on.
type fakeSink struct {
	mu    sync.Mutex
	lines [][]byte
	// order records journal writes AND whatever else a test interleaves with
	// them, so "the intent landed before the destructive step" is a fact about
	// one shared sequence rather than two that have to be correlated.
	order []string
	fail  error
}

func (f *fakeSink) sink(_ context.Context, line []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		f.order = append(f.order, "journal-write-failed")
		return f.fail
	}
	f.lines = append(f.lines, append([]byte(nil), line...))
	f.order = append(f.order, "journal")
	return nil
}

func (f *fakeSink) note(what string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.order = append(f.order, what)
}

func (f *fakeSink) records(t *testing.T) []Record {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Record, 0, len(f.lines))
	for i, l := range f.lines {
		r, err := DecodeLine(l)
		if err != nil {
			t.Fatalf("record %d does not decode: %v (%s)", i, err, l)
		}
		out = append(out, r)
	}
	return out
}

func testIdentity() Identity {
	return Identity{
		ActionID: "9f3c1a2b4d5e6f70",
		Domain:   "web01",
		Host:     "hv-a",
		PID:      4242,
		RunID:    "run-7",
		OpID:     "op-123",
		By:       "alice",
	}
}

func TestRecorderWritesOneIntentAndOneOutcomePerAction(t *testing.T) {
	f := &fakeSink{}
	rec := New(f.sink, testIdentity(), nil)

	rec.Intent(context.Background(), "sync", map[string]string{"mode": "reinit"})
	rec.Outcome(context.Background(), ResultOK, 0, nil, map[string]string{"checkpoint": "vmsync-cpt-000007"})

	got := f.records(t)
	if len(got) != 2 {
		t.Fatalf("wrote %d records, want exactly one intent and one outcome", len(got))
	}
	if got[0].Kind != KindIntent || got[1].Kind != KindOutcome {
		t.Fatalf("wrong kinds: %s then %s", got[0].Kind, got[1].Kind)
	}
	// The join.
	if got[0].Action != got[1].Action || got[0].Seq != got[1].Seq {
		t.Errorf("the intent and its outcome do not share (aid, seq): %q/%d vs %q/%d",
			got[0].Action, got[0].Seq, got[1].Action, got[1].Seq)
	}
	if got[0].Seq != 1 {
		t.Errorf("first action has seq %d, want 1", got[0].Seq)
	}
	// The verb is carried onto the outcome from the intent, so a reader that
	// only has the outcome still knows what ended.
	if got[1].Verb != "sync" {
		t.Errorf("outcome verb = %q, want sync", got[1].Verb)
	}
	if got[1].Result != ResultOK || got[1].Exit != 0 || got[1].Err != "" {
		t.Errorf("outcome does not describe a success: %+v", got[1])
	}
	if got[0].Detail["mode"] != "reinit" || got[1].Detail["checkpoint"] != "vmsync-cpt-000007" {
		t.Errorf("details did not reach the records: %v / %v", got[0].Detail, got[1].Detail)
	}
}

// The identity is stamped on every record, so a journal read on the DR host
// says who ran the action and from where -- which is the only way to find the
// log of a run that died on another machine.
func TestRecorderStampsTheIdentityOnEveryRecord(t *testing.T) {
	f := &fakeSink{}
	rec := New(f.sink, testIdentity(), nil)
	rec.Intent(context.Background(), "sync", nil)
	rec.Outcome(context.Background(), ResultFailed, 1, errors.New("boom"), nil)

	for i, r := range f.records(t) {
		if r.Action != "9f3c1a2b4d5e6f70" || r.Domain != "web01" || r.Host != "hv-a" ||
			r.PID != 4242 || r.RunID != "run-7" || r.OpID != "op-123" || r.By != "alice" {
			t.Errorf("record %d lost part of the identity: %+v", i, r)
		}
	}
}

// The ordering the whole feature rests on: the intent is ON the target before
// the first step that can displace anything. Asserted against one shared
// sequence, with a stand-in for the destructive step, because a recorder that
// buffered would pass every other test here and fail the only run that matters
// -- the one that never gets to flush.
func TestTheIntentIsWrittenBeforeTheDestructiveStep(t *testing.T) {
	f := &fakeSink{}
	rec := New(f.sink, testIdentity(), nil)

	rec.Intent(context.Background(), "sync", map[string]string{"mode": "reinit"})
	f.note("mv -n disk aside") // stands in for the first thing that displaces a replica
	rec.Outcome(context.Background(), ResultOK, 0, nil, nil)

	want := []string{"journal", "mv -n disk aside", "journal"}
	if len(f.order) != len(want) {
		t.Fatalf("sequence %v, want %v", f.order, want)
	}
	for i := range want {
		if f.order[i] != want[i] {
			t.Fatalf("sequence %v, want %v", f.order, want)
		}
	}
}

// A journal write failure must never fail the action it describes: counted,
// reported, and carried on past. A diagnostic that can stop a replication is
// a diagnostic that causes outages.
func TestAJournalWriteFailureIsCountedAndNeverFatal(t *testing.T) {
	f := &fakeSink{fail: errors.New("read-only filesystem")}
	var reported []string
	rec := New(f.sink, testIdentity(), func(err error) { reported = append(reported, err.Error()) })

	rec.Intent(context.Background(), "sync", nil)
	rec.Outcome(context.Background(), ResultOK, 0, nil, nil)

	if rec.Failures() != 2 {
		t.Errorf("counted %d failures, want 2", rec.Failures())
	}
	if len(reported) != 2 {
		t.Fatalf("reported %d failures, want 2: %v", len(reported), reported)
	}
	for _, msg := range reported {
		if !strings.Contains(msg, "read-only filesystem") {
			t.Errorf("the report does not name the cause: %q", msg)
		}
	}
	if !strings.Contains(reported[0], KindIntent) || !strings.Contains(reported[1], KindOutcome) {
		t.Errorf("the report does not say which record was lost: %v", reported)
	}
	// Both attempts were made; the first failure did not disarm the second.
	if len(f.order) != 2 {
		t.Errorf("stopped trying after the first failure: %v", f.order)
	}
}

// -journal=off, and "there is nowhere to write", are the same object
// downstream. Every method on it does nothing, so no call site needs a
// conditional -- a conditional is how a caller ends up journaling in one
// branch and not the other.
func TestANilRecorderIsSafeEverywhere(t *testing.T) {
	var rec *Recorder
	rec.Intent(context.Background(), "sync", map[string]string{"mode": "reinit"})
	rec.Outcome(context.Background(), ResultFailed, 1, errors.New("boom"), nil)
	if rec.Failures() != 0 {
		t.Errorf("a disabled journal reported %d failures", rec.Failures())
	}
}

func TestNewWithNoSinkIsADisabledRecorder(t *testing.T) {
	if rec := New(nil, testIdentity(), nil); rec != nil {
		t.Error("a recorder with nowhere to write must be indistinguishable from -journal=off")
	}
}

// One process can perform several actions under one id -- a sync and the
// verify-failure repair that follows it. Joining on the id alone would fold
// them into one action whose intent and outcome describe different runs.
func TestSeqTellsTwoActionsOfOneProcessApart(t *testing.T) {
	f := &fakeSink{}
	rec := New(f.sink, testIdentity(), nil)

	rec.Intent(context.Background(), "sync", nil)
	rec.Outcome(context.Background(), ResultFailed, 1, errors.New("verify mismatch"), nil)
	rec.Intent(context.Background(), "sync", map[string]string{"mode": "repair"})
	rec.Outcome(context.Background(), ResultOK, 0, nil, nil)

	got := f.records(t)
	if len(got) != 4 {
		t.Fatalf("wrote %d records, want 4", len(got))
	}
	if got[0].Seq != 1 || got[1].Seq != 1 || got[2].Seq != 2 || got[3].Seq != 2 {
		t.Fatalf("seq numbers %d/%d/%d/%d, want 1/1/2/2", got[0].Seq, got[1].Seq, got[2].Seq, got[3].Seq)
	}
	reading := Read(joinLines(f.lines))
	if len(reading.Actions) != 2 {
		t.Fatalf("the reader folded %d actions into %d", 2, len(reading.Actions))
	}
	for _, a := range reading.Actions {
		if !a.HasIntent || !a.HasOutcome {
			t.Errorf("action seq %d did not pair up: %+v", a.Seq, a)
		}
	}
}

// An outcome with no intent before it is a caller that got the order wrong.
// Recorded rather than dropped: that mistake is itself worth seeing.
func TestAnOutcomeWithNoIntentIsStillRecorded(t *testing.T) {
	f := &fakeSink{}
	rec := New(f.sink, testIdentity(), nil)
	rec.Outcome(context.Background(), ResultOK, 0, nil, nil)
	got := f.records(t)
	if len(got) != 1 || got[0].Kind != KindOutcome || got[0].Seq != 1 {
		t.Fatalf("want one outcome at seq 1, got %+v", got)
	}
}

func TestOutcomeCarriesTheError(t *testing.T) {
	f := &fakeSink{}
	rec := New(f.sink, testIdentity(), nil)
	rec.Intent(context.Background(), "promote", nil)
	rec.Outcome(context.Background(), ResultFailed, 1, errors.New("refusing to promote web01: replica_incomplete"), nil)
	got := f.records(t)
	if !strings.Contains(got[1].Err, "replica_incomplete") {
		t.Errorf("the outcome does not carry the error: %q", got[1].Err)
	}
}

// Standing down on a held lock is not a failure, and recording it as one would
// make every scheduled overlap read as a broken replica in the history.
func TestBusyIsItsOwnResult(t *testing.T) {
	f := &fakeSink{}
	rec := New(f.sink, testIdentity(), nil)
	rec.Intent(context.Background(), "sync", nil)
	rec.Outcome(context.Background(), ResultBusy, 75, nil, nil)
	got := f.records(t)
	if got[1].Result != ResultBusy || got[1].Exit != 75 {
		t.Errorf("a stand-down was not recorded as busy: %+v", got[1])
	}
}

// The clock is read at the moment each record is written, not once per
// process: the interval between an intent and its outcome is how long the
// action took, and it is the number an operator compares against a
// replica_incomplete marker.
func TestRecordsCarryTheirOwnTime(t *testing.T) {
	f := &fakeSink{}
	rec := New(f.sink, testIdentity(), nil)
	ticks := []time.Time{time.Unix(1758441600, 0), time.Unix(1758445200, 0)}
	i := 0
	rec.now = func() time.Time {
		v := ticks[i]
		i++
		return v
	}
	rec.Intent(context.Background(), "sync", nil)
	rec.Outcome(context.Background(), ResultOK, 0, nil, nil)
	got := f.records(t)
	if got[0].At != 1758441600 || got[1].At != 1758445200 {
		t.Errorf("times %d and %d, want 1758441600 and 1758445200", got[0].At, got[1].At)
	}
}

// Two vmsync processes can journal the same domain at once (two sources
// replicating into one target host). Nothing here may race.
func TestRecorderIsSafeUnderConcurrentUse(t *testing.T) {
	f := &fakeSink{}
	rec := New(f.sink, testIdentity(), nil)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec.Intent(context.Background(), "sync", nil)
			rec.Outcome(context.Background(), ResultOK, 0, nil, nil)
		}()
	}
	wg.Wait()
	if got := len(f.records(t)); got != 32 {
		t.Errorf("wrote %d records, want 32", got)
	}
	if rec.Failures() != 0 {
		t.Errorf("unexpected failures: %d", rec.Failures())
	}
}

func joinLines(lines [][]byte) []byte {
	var out []byte
	for _, l := range lines {
		out = append(out, l...)
	}
	return out
}
