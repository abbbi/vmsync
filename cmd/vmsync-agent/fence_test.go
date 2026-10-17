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
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"

	"vmsync/pkg/libvirtsync"
	"vmsync/pkg/util"
)

// replicaSelfRef must produce the SAME string vmsync stamps into replication
// metadata, or every fence token naming this host fails to match and split
// brain protection is silently off.
//
// The two are derived from the same inputs by the same function on purpose:
// the agent runs vmsync with -source-uri cfg.LibvirtURI and -local-host-name
// cfg.Hostname (see scheduler.go), and vmsync stamps
// util.ReplicaHost(-source-uri, -local-host-name). This asserts the agent's
// side agrees, for a remote URI as well as a local one -- the local case is
// the one that always worked and hid the bug.
func TestReplicaSelfRefMatchesWhatVmsyncStamps(t *testing.T) {
	for _, tc := range []struct {
		name, uri, hostname, want string
	}{
		{
			// The default, and why this was invisible: with no host in the
			// URI, ReplicaHost falls through to the local name, so comparing
			// cfg.Hostname directly happened to be right.
			"a local uri uses the configured hostname",
			"qemu:///system", "prod01", "prod01:web01",
		},
		{
			// The broken case. ReplicaHost prefers the URI's host and ignores
			// the local name entirely, so the old code compared "mgmt01:web01"
			// against a stamp of "prod01:web01" -- forever.
			"a remote uri uses the uri's host, not the configured hostname",
			"qemu+ssh://prod01/system", "mgmt01", "prod01:web01",
		},
		{
			"a remote uri with a port and a user still uses the host",
			"qemu+ssh://root@prod01:2222/system", "mgmt01", "prod01:web01",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := agentConfig{LibvirtURI: tc.uri, Hostname: tc.hostname}
			got := replicaSelfRef(cfg, "web01")
			if got != tc.want {
				t.Errorf("replicaSelfRef = %q, want %q", got, tc.want)
			}
			// The property that actually matters, stated directly: this is
			// the same derivation vmsync applies to the same two values.
			stamped := util.ReplicaHost(tc.uri, tc.hostname) + ":web01"
			if got != stamped {
				t.Errorf("replicaSelfRef = %q but vmsync stamps %q -- a fence token naming this host would never match", got, stamped)
			}
		})
	}
}

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

// The state a fence that did not take leaves behind, and the one thing that
// recognises it.
//
// role=fenced is written by a fence whether or not the ACPI shutdown worked --
// deliberately, because that role is the only thing stopping replication resuming
// into the split brain. So the role alone says nothing about whether the domain
// stopped; the role AND still running says the fence suspended replication and
// left the guest serving beside the copy that displaced it.
//
// Before this predicate existed, that state was the one state in which every
// signal read clear: the sweep skipped the domain precisely BECAUSE it was fenced,
// so it left the split-brain set and the gauge fell back to zero about a minute
// after the failure, with only an in-memory failure counter -- reset on restart --
// recording that anything had happened.
func TestFenceFailedOpen(t *testing.T) {
	if !fenceFailedOpen(libvirtsync.RoleFenced, true) {
		t.Error("fenced and running is a fence that did not stop the domain; it is the whole condition this exists to report")
	}
	// Stopped is a fence that worked. The ordinary resting state of every fenced
	// domain in an estate, and it must never alarm -- nor must the condition
	// latch, which is what lets it clear when somebody finally stops the guest.
	if fenceFailedOpen(libvirtsync.RoleFenced, false) {
		t.Error("a fenced domain that is stopped is a fence that worked")
	}

	// Deliberately NOT generalised to paused. `paused` claims nothing about a
	// peer, so a running domain whose replication was paused by hand is one live
	// copy and an ordinary state; alarming on it would teach an operator to
	// ignore the series that matters. This is also why a pause may not overwrite
	// a fence -- see roleToRecord in cmd/vmsync.
	for _, role := range []string{
		libvirtsync.RolePaused, libvirtsync.RoleSource, libvirtsync.RoleTarget,
		libvirtsync.RolePromoted, libvirtsync.RoleNone, "", "something-newer-wrote",
	} {
		if fenceFailedOpen(role, true) {
			t.Errorf("role %q while running is not a failed fence", role)
		}
	}
}

// The gauge and the sweep's warning must name the same VMs, so the console and
// the metrics cannot disagree about which machines are live in two places.
func TestFencedRunningVMsUsesTheSamePredicateAsTheSweep(t *testing.T) {
	got := fencedRunningVMs([]ReportDomain{
		{Name: "web01", Role: libvirtsync.RoleFenced, Active: true},
		{Name: "db01", Role: libvirtsync.RoleFenced, Active: false},
		{Name: "app01", Role: libvirtsync.RolePaused, Active: true},
		{Name: "mail01", Role: libvirtsync.RoleSource, Active: true},
	})
	if len(got) != 1 || !got["web01"] {
		t.Errorf("fencedRunningVMs = %v, want only web01 -- db01's fence worked, app01 is an administrative pause with one live copy, mail01 is an ordinary source", got)
	}
	if n := len(fencedRunningVMs(nil)); n != 0 {
		t.Errorf("fencedRunningVMs(nil) = %d entries", n)
	}
}

// THE RULE AN ALARM CLEARS ON.
//
// checkOneFence used to return a bool, and both "the peer says it is not
// promoted" and "the peer did not answer" were false. sweepFences rebuilds
// the gauge from each pass and publishes it wholesale, so a VM already
// known to be in split brain dropped out of that gauge the moment its peer
// stopped answering -- which is exactly when the partition that caused the
// failover sets in. Two live copies taking writes, and the alert resolving
// itself.
//
// sweepFences already stated the rule at the HOST level and followed it
// there: local libvirt unreachable returns without touching the metric,
// because "failing to look is not evidence that the problem went away".
// This is that rule per VM.
func TestSilenceNeitherRaisesAnAlarmNorClearsOne(t *testing.T) {
	cases := []struct {
		name        string
		verdicts    []fenceVerdict
		wasSplit    bool
		wantSplit   bool
		wantUnknown bool
	}{
		{"a peer that answers 'not me' clears it",
			[]fenceVerdict{fenceClear}, true, false, false},
		{"a peer that answers 'I took over' raises it",
			[]fenceVerdict{fenceSplit}, false, true, false},

		// THE DEFECT. Silence used to read as "not split".
		{"a peer that goes quiet does NOT clear a known split brain",
			[]fenceVerdict{fenceUnknown}, true, true, true},
		{"...and does not invent one either",
			[]fenceVerdict{fenceUnknown}, false, false, true},

		// Fan-out: a source with several targets.
		{"one peer displacing settles it however many others are quiet",
			[]fenceVerdict{fenceUnknown, fenceSplit, fenceUnknown}, false, true, false},
		{"a single unreachable peer makes the whole answer unknown",
			[]fenceVerdict{fenceClear, fenceUnknown}, true, true, true},
		{"only when EVERY peer answers is it cleared",
			[]fenceVerdict{fenceClear, fenceClear}, true, false, false},

		// A VM with no targets never reaches here; an empty verdict list
		// is the degenerate case and must not carry anything forward.
		{"no peers to ask is not the same as a peer that did not answer",
			nil, true, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			split, unknown := splitStateFor(tc.verdicts, tc.wasSplit)
			if split != tc.wantSplit {
				t.Errorf("split = %v, want %v -- %s", split, tc.wantSplit, tc.name)
			}
			if unknown != tc.wantUnknown {
				t.Errorf("unknown = %v, want %v -- %s", unknown, tc.wantUnknown, tc.name)
			}
			// A finding and an inability to look are different states, and
			// a VM reported as both would make the unknown gauge an alarm
			// of its own rather than a reason the other one did not move.
			if split && unknown && len(tc.verdicts) > 0 {
				hasSplit := false
				for _, v := range tc.verdicts {
					if v == fenceSplit {
						hasSplit = true
					}
				}
				if hasSplit {
					t.Error("a positively observed split brain is also reported as unknown")
				}
			}
		})
	}
}

// The anti-latch property the carry-forward must not break.
//
// sweepFences' own comment explains why the gauge is rebuilt wholesale: the
// ordinary way a split brain ends is the domain being shut down, after
// which the sweep skips it as inactive and an incrementally maintained
// gauge would latch at 1 for ever -- "an alert nobody could clear, which
// trains people to ignore the one metric that must never be ignored".
//
// Carrying a previous answer forward is the one thing that could
// reintroduce that, so the boundary is worth stating: carry-forward happens
// only for a VM that is still running AND still being asked about. A VM
// that stops is never passed to splitStateFor at all, which this asserts by
// construction -- the only way out of the gauge is not appearing in a pass.
func TestAClearedPeerStillClearsTheGauge(t *testing.T) {
	// However long it was split, one complete round of answers clears it.
	split, unknown := splitStateFor([]fenceVerdict{fenceClear}, true)
	if split || unknown {
		t.Errorf("split=%v unknown=%v after every peer answered 'not me'; the gauge would latch and nobody could clear the alert", split, unknown)
	}
}

// NOTHING MAY BE CONCLUDED BEFORE THE PEER ANSWERS.
//
// splitStateFor above pins what the verdicts MEAN; this pins that the
// conditions map onto them correctly, which no table test can reach --
// checkOneFence needs a peer host to query, so the branches that matter are
// exactly the ones a unit test cannot drive.
//
// The invariant is positional and simple: failover.AssessFence is the point
// at which this function first has an answer to reason from. Every return
// before it is a return taken because the question could not be put, and
// must therefore be fenceUnknown. A fenceClear there is the original defect
// -- silence reported as "no split brain", clearing the gauge for a VM
// whose peer had just gone quiet.
func TestCheckOneFenceConcludesNothingBeforeThePeerAnswers(t *testing.T) {
	const file = "fence.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "checkOneFence" {
			fn = fd
			break
		}
	}
	if fn == nil {
		t.Fatal("no func checkOneFence in " + file)
	}

	// Where the peer's answer first exists.
	var assessPos token.Pos
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "AssessFence" {
			return true
		}
		if assessPos == 0 {
			assessPos = call.Pos()
		}
		return true
	})
	if assessPos == 0 {
		t.Fatal("checkOneFence no longer calls failover.AssessFence; this test located the boundary by that call, so rename it here rather than deleting the check that keeps silence out of the gauge")
	}

	var early int
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		ret, ok := n.(*ast.ReturnStmt)
		if !ok || ret.Pos() >= assessPos || len(ret.Results) != 1 {
			return true
		}
		early++
		id, ok := ret.Results[0].(*ast.Ident)
		if !ok || id.Name != "fenceUnknown" {
			got := "a non-identifier expression"
			if ok {
				got = id.Name
			}
			t.Errorf("checkOneFence returns %s at line %d, before the peer has answered. Everything before failover.AssessFence is a path taken because the question could not be put, so it must return fenceUnknown -- anything else lets silence clear the split-brain gauge for a VM whose peer just went quiet",
				got, fset.Position(ret.Pos()).Line)
		}
		return true
	})
	// The three early returns: an unparsable peer ref, a failed query, and
	// an unreachable peer. A count of zero would mean this walk found
	// nothing and passed for that reason.
	if early < 3 {
		t.Errorf("found only %d early returns in checkOneFence, expected at least 3 (bad peer ref, query error, peer unreachable); this audit is no longer covering them", early)
	}
}
