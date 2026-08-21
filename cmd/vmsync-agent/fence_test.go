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
	"strconv"
	"testing"
)

func TestParseFenceReportReadsAnArmedToken(t *testing.T) {
	out := []byte(`{"reachable":true,"target_ref":"dr01:web01","target_role":"promoted","target_active":true,` +
		`"fence":{"id":"abc123","source":"prod01:web01","armed_at":1755000000,"armed_by":"alice"}}`)
	rep, err := parseFenceReport(out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !rep.Reachable || !rep.TargetActive {
		t.Errorf("reachable/active lost: %+v", rep)
	}
	if rep.Fence.ID != "abc123" || rep.Fence.Source != "prod01:web01" {
		t.Errorf("fence token lost: %+v", rep.Fence)
	}
	if rep.Fence.ArmedAt != 1755000000 || rep.Fence.ArmedBy != "alice" {
		t.Errorf("attribution lost: %+v", rep.Fence)
	}
}

// vmsync logs to stderr, so this should not arise today. It is pinned
// because the two binaries are separately versioned: a future build writing
// one extra line to stdout would otherwise disable fencing silently, which
// is the failure mode with no symptom until the day it matters.
func TestParseFenceReportIgnoresLogLinesAroundIt(t *testing.T) {
	out := []byte("level=info msg=\"connecting\"\n" +
		"some other noise\n" +
		`{"reachable":true,"target_ref":"dr01:web01","target_role":"promoted","target_active":true,"fence":{"id":"xyz","source":"prod01:web01"}}` + "\n")
	rep, err := parseFenceReport(out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if rep.Fence.ID != "xyz" {
		t.Errorf("did not find the report after log output: %+v", rep)
	}
}

// An unreachable peer is a normal, well-formed answer -- and it must never
// be confused with "no fence is armed". One means keep serving because
// nothing says otherwise; the other means keep serving because we could not
// ask. Only the second should ever be retried on the next sweep.
func TestParseFenceReportKeepsUnreachableDistinctFromUnfenced(t *testing.T) {
	unreachable, err := parseFenceReport([]byte(`{"reachable":false,"error":"dial tcp: no route to host","target_ref":"dr01:web01","fence":{}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if unreachable.Reachable {
		t.Fatal("an unreachable peer must not report as reachable")
	}
	if unreachable.Error == "" {
		t.Error("an unreachable peer should carry the reason")
	}

	unfenced, err := parseFenceReport([]byte(`{"reachable":true,"target_ref":"dr01:web01","target_role":"target","target_active":false,"fence":{}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !unfenced.Reachable {
		t.Fatal("a peer that answered must report as reachable")
	}
	if unfenced.Fence.Armed() {
		t.Error("an empty fence object must not read as armed")
	}
}

// Garbage must be an ERROR, never an empty report. An empty report parses as
// "reachable=false", which the caller treats as "could not ask" -- benign.
// But silently turning a broken vmsync into a benign answer would hide the
// breakage indefinitely, and the fence would never fire.
func TestParseFenceReportRejectsOutputItCannotUnderstand(t *testing.T) {
	for _, tc := range []struct{ name, out string }{
		{"empty", ""},
		{"only whitespace", "   \n\t\n"},
		{"not json at all", "vmsync: error: could not connect\n"},
		{"a json array rather than the report object", "[1,2,3]\n"},
		{"truncated json", `{"reachable":true,`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseFenceReport([]byte(tc.out)); err == nil {
				t.Error("output that is not a fence report must be an error, not a benign empty answer")
			}
		})
	}
}

func TestFenceLedgerLatchesAcrossAReload(t *testing.T) {
	dir := t.TempDir()

	l := newFenceLedger(dir)
	if err := l.Load(); err != nil {
		t.Fatalf("load a ledger that does not exist yet: %v", err)
	}
	if l.Acted("fence-1") {
		t.Fatal("a fresh ledger cannot have acted on anything")
	}

	rec := fenceRecord{FenceID: "fence-1", VM: "web01", PeerRef: "dr01:web01", AtUnix: 1755000000}
	if err := l.Begin(rec); err != nil {
		t.Fatalf("begin: %v", err)
	}
	// Latched from the moment intent is recorded, NOT from completion. The
	// window between the two is a whole guest shutdown, and a crash inside
	// it must not leave a token looking untouched.
	if !l.Acted("fence-1") {
		t.Fatal("recording intent must latch the fence immediately")
	}

	rec.State = OpStateDone
	if err := l.Finish(rec); err != nil {
		t.Fatalf("finish: %v", err)
	}

	// The property that makes this durable rather than merely in-memory: a
	// restarted agent must still refuse the token.
	reloaded := newFenceLedger(dir)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !reloaded.Acted("fence-1") {
		t.Fatal("a fence acted on before a restart must still be latched after one -- otherwise every restart re-arms every fence")
	}
	if reloaded.Acted("fence-2") {
		t.Error("an unrelated fence must not be latched")
	}
}

// A failed fence stays latched. This is the deliberate decision: the
// realistic failure is a guest ignoring ACPI, and retrying that on a timer
// means either an unbounded queue of shutdowns or an escalation to
// destroying a running VM. Neither is something an unattended agent should
// arrive at by repetition.
func TestFenceLedgerLatchesFailuresToo(t *testing.T) {
	dir := t.TempDir()
	l := newFenceLedger(dir)
	if err := l.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := l.Finish(fenceRecord{
		FenceID: "fence-boom", VM: "web01", AtUnix: 1755000000,
		State: OpStateFailed, Error: "the guest did not shut down in time",
	}); err != nil {
		t.Fatalf("finish: %v", err)
	}

	reloaded := newFenceLedger(dir)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !reloaded.Acted("fence-boom") {
		t.Fatal("a FAILED fence must stay latched: the retry is a person's decision, not a timer's")
	}
}

func TestFenceLedgerEvictsTheOldestWhenFull(t *testing.T) {
	dir := t.TempDir()
	l := newFenceLedger(dir)
	if err := l.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}

	// One past the cap, oldest first, so exactly one eviction is due.
	for i := 0; i <= fenceLedgerKept; i++ {
		if err := l.Finish(fenceRecord{
			FenceID: "fence-" + strconv.Itoa(i), VM: "web01",
			AtUnix: int64(1_000_000 + i), State: OpStateDone,
		}); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}
	if got := len(l.Records()); got != fenceLedgerKept {
		t.Fatalf("ledger holds %d records, want it capped at %d", got, fenceLedgerKept)
	}
	if l.Acted("fence-0") {
		t.Error("the OLDEST record should have been evicted")
	}
	if !l.Acted("fence-" + strconv.Itoa(fenceLedgerKept)) {
		t.Error("the newest record must survive: evicting it would un-latch the fence most likely to still matter")
	}
}

func TestFenceReportRoundTripsThroughTheWireFormat(t *testing.T) {
	// The agent and vmsync are deliberately separable binaries and may be
	// different builds, so the report is a wire format. This pins the field
	// names: renaming one on either side silently breaks fencing, and the
	// symptom would be a fence that never fires rather than an error.
	const wire = `{"reachable":true,"target_ref":"dr01:web01","target_role":"promoted",` +
		`"target_active":true,"fence":{"id":"i","source":"s:v","armed_at":7,"armed_by":"b"}}`
	rep, err := parseFenceReport([]byte(wire))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for name, ok := range map[string]bool{
		"reachable":      rep.Reachable,
		"target_ref":     rep.TargetRef == "dr01:web01",
		"target_role":    rep.TargetRole == "promoted",
		"target_active":  rep.TargetActive,
		"fence.id":       rep.Fence.ID == "i",
		"fence.source":   rep.Fence.Source == "s:v",
		"fence.armed_at": rep.Fence.ArmedAt == 7,
		"fence.armed_by": rep.Fence.ArmedBy == "b",
	} {
		if !ok {
			t.Errorf("wire field %s did not survive decoding: %+v", name, rep)
		}
	}
}
