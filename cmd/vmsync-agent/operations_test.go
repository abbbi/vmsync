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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var opNow = time.Unix(1_800_000_000, 0)

func promoteOp() Operation {
	return Operation{
		ID: "op-1", Kind: OpPromote, VM: "web01",
		PeerHost: "prod01", PeerVM: "web01",
		Mode: modeForcedLiteral, CreatedAtUnix: opNow.Unix(), CreatedBy: "alice",
	}
}

// modeForcedLiteral avoids importing pkg/failover here just for a string
// the wire format carries verbatim.
const modeForcedLiteral = "forced"

// A Begin whose disk write fails must leave NOTHING behind.
//
// put() records in memory before it writes, so a failed write used to leave
// the operation marked `running` having never started -- and Seen() refuses to
// re-execute any recorded id, so that operation could never run again while
// the UI kept publishing it forever. A later Load() then relabelled the orphan
// "the agent stopped while this operation was in progress", a statement about
// something that never began. A transient ENOSPC was enough.
func TestLedgerBeginLeavesNothingBehindWhenItCannotBeWritten(t *testing.T) {
	// A state dir nested under a regular FILE, so writeJSONAtomic's own
	// os.MkdirAll fails with ENOTDIR. A merely absent directory would not do
	// it -- writeJSONAtomic creates one -- and that is worth knowing: the
	// unwritable cases this guards against are permissions, a full disk and a
	// read-only mount, never a missing path.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("a file, not a directory"), 0o600); err != nil {
		t.Fatalf("set up the blocker file: %v", err)
	}
	l := newOperationLedger(filepath.Join(blocker, "state"))
	op := promoteOp()

	if err := l.Begin(op, opNow); err == nil {
		t.Fatal("Begin reported success with an unwritable ledger path")
	}
	if res, seen := l.Seen(op.ID); seen {
		t.Errorf("the operation is still recorded as %q after a failed Begin -- Seen() will refuse to retry it, so it can never run", res.State)
	}

	// And the id is genuinely reusable: a later Begin against a working path
	// must be able to start the same operation.
	ok := newOperationLedger(t.TempDir())
	if err := ok.Begin(op, opNow); err != nil {
		t.Fatalf("Begin on a working ledger after a failed one: %v", err)
	}
	if _, seen := ok.Seen(op.ID); !seen {
		t.Error("the retried Begin did not record the operation")
	}
}

// TestLedgerRecordsIntentBeforeOutcome is the property the whole replay
// guard rests on. Recording only completion leaves the entire duration of an
// operation -- a graceful shutdown is minutes -- with no trace on disk, so a
// crash there loses all knowledge of a half-performed failover.
func TestLedgerRecordsIntentBeforeOutcome(t *testing.T) {
	l := newOperationLedger(t.TempDir())
	op := promoteOp()

	if _, seen := l.Seen(op.ID); seen {
		t.Fatal("a fresh ledger already knows this operation")
	}
	if err := l.Begin(op, opNow); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	res, seen := l.Seen(op.ID)
	if !seen {
		t.Fatal("Begin did not make the operation known")
	}
	if res.State != OpStateRunning {
		t.Errorf("state after Begin = %q, want %q", res.State, OpStateRunning)
	}

	// A second delivery of the same ID must be refused while it is still
	// running -- that is the concurrent-delivery case, not just the
	// after-the-fact one.
	if _, seen := l.Seen(op.ID); !seen {
		t.Error("an in-progress operation is not recognised as already seen")
	}
}

// TestLedgerSurvivesRestartAsUnknown: a `running` entry can only mean the
// agent died mid-operation. Resuming or retrying it would act on a domain
// whose state nobody has established.
func TestLedgerSurvivesRestartAsUnknown(t *testing.T) {
	dir := t.TempDir()
	op := promoteOp()

	first := newOperationLedger(dir)
	if err := first.Begin(op, opNow); err != nil {
		t.Fatalf("Begin: %v", err)
	}

	// A new process over the same state directory.
	second := newOperationLedger(dir)
	if err := second.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	res, seen := second.Seen(op.ID)
	if !seen {
		t.Fatal("the operation was forgotten across a restart -- it would be executed again")
	}
	if res.State != OpStateUnknown {
		t.Errorf("state after restart = %q, want %q", res.State, OpStateUnknown)
	}
	if res.Error == "" {
		t.Error("an unknown outcome carries no explanation for the operator")
	}
}

// TestLedgerNeverRetriesAFailure: a failover that went wrong once is not
// re-run unattended. A person re-issues it with a fresh ID once they know
// why it failed.
func TestLedgerNeverRetriesAFailure(t *testing.T) {
	l := newOperationLedger(t.TempDir())
	op := promoteOp()

	if err := l.Begin(op, opNow); err != nil {
		t.Fatal(err)
	}
	if err := l.Finish(op, OperationResult{State: OpStateFailed, Error: "exit status 1"}, opNow); err != nil {
		t.Fatal(err)
	}
	res, seen := l.Seen(op.ID)
	if !seen {
		t.Fatal("a failed operation is not remembered; it would be retried")
	}
	if res.State != OpStateFailed {
		t.Errorf("state = %q, want %q", res.State, OpStateFailed)
	}
}

// TestLedgerResultsAreReportedUntilAcknowledged covers the stuck-forever
// case: the result is re-sent on every report, and only dropped once the UI
// stops publishing the operation.
func TestLedgerResultsAreReportedUntilAcknowledged(t *testing.T) {
	l := newOperationLedger(t.TempDir())
	op := promoteOp()

	if err := l.Begin(op, opNow); err != nil {
		t.Fatal(err)
	}
	if err := l.Finish(op, OperationResult{State: OpStateDone}, opNow); err != nil {
		t.Fatal(err)
	}

	// Still published: the result must keep going out, however many reports
	// have already carried it.
	for i := 0; i < 3; i++ {
		l.Forget(map[string]bool{op.ID: true})
		if got := l.Results(); len(got) != 1 || got[0].ID != op.ID {
			t.Fatalf("report %d carries %d results, want the operation still acknowledged", i, len(got))
		}
	}

	// The UI has stopped publishing it, which IS the acknowledgement.
	l.Forget(map[string]bool{})
	if got := l.Results(); len(got) != 0 {
		t.Errorf("results = %v, want none once the UI stopped publishing", got)
	}
	if _, seen := l.Seen(op.ID); seen {
		t.Error("the record outlived its acknowledgement; the ledger would grow without bound")
	}
}

// TestOperationValidatePeerMustMatchLocalMetadata is the trust boundary. The
// UI's claim about the far end is checked against what this host records
// about itself, never used as the endpoint.
func TestOperationValidatePeerMustMatchLocalMetadata(t *testing.T) {
	op := promoteOp()

	if err := op.Validate([]string{"prod01:web01"}, opNow); err != nil {
		t.Fatalf("a matching peer was refused: %v", err)
	}
	// Case-insensitive on the host, like every other host comparison here.
	if err := op.Validate([]string{"PROD01:web01"}, opNow); err != nil {
		t.Errorf("host comparison is case-sensitive: %v", err)
	}
	// A source fanning out to several targets: any one of them is a peer
	// this host legitimately recognises.
	if err := op.Validate([]string{"dr02:web01", "prod01:web01"}, opNow); err != nil {
		t.Errorf("a peer among several was refused: %v", err)
	}

	err := op.Validate([]string{"somewhere-else:web01"}, opNow)
	if err == nil {
		t.Fatal("accepted a peer this host does not record -- the UI could name any hypervisor")
	}
	if !strings.Contains(err.Error(), "somewhere-else:web01") {
		t.Errorf("error = %q, want it to show what local metadata actually says", err)
	}

	if err := op.Validate(nil, opNow); err == nil {
		t.Error("accepted a peer claim against a VM that records no replication relationship")
	}
}

func TestOperationValidateRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Operation)
		want   string
	}{
		{"no id", func(o *Operation) { o.ID = "" }, "no id"},
		{"no vm", func(o *Operation) { o.VM = "" }, "names no vm"},
		{
			// From a newer UI. Refused AND reported, never ignored: silence
			// leaves a failover pending against a healthy agent.
			"unknown kind",
			func(o *Operation) { o.Kind = "teleport" },
			"does not understand",
		},
		{
			// The armed-forever case. A target host that was down when the
			// promote was issued must not execute it days later as a first
			// delivery no replay guard covers.
			"expired",
			func(o *Operation) { o.NotAfterUnix = opNow.Unix() - 1 },
			"expired",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			op := promoteOp()
			tc.mutate(&op)
			err := op.Validate([]string{"prod01:web01"}, opNow)
			if err == nil {
				t.Fatalf("accepted: %+v", op)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}

	// A deadline still in the future is fine.
	op := promoteOp()
	op.NotAfterUnix = opNow.Unix() + 60
	if err := op.Validate([]string{"prod01:web01"}, opNow); err != nil {
		t.Errorf("an unexpired operation was refused: %v", err)
	}

	// Every kind the agent can execute must survive Validate. A kind added to
	// the executor but forgotten here is refused as "does not understand" --
	// which reads to an operator like the agent is too old, on an agent that
	// implements it perfectly well.
	for _, kind := range []string{OpPromote, OpInvert, OpShutdown, OpSetRole, OpReinit, OpForceClean} {
		k := promoteOp()
		k.Kind = kind
		if err := k.Validate([]string{"prod01:web01"}, opNow); err != nil {
			t.Errorf("Validate refused kind %q: %v", kind, err)
		}
	}
}

// TestLoadCacheDropsOperations pins the structural half of the replay guard:
// the schedule survives a restart because it is desired state, an operation
// does not because it is an event.
func TestLoadCacheDropsOperations(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	cfg := DefaultUIConfig()
	cfg.Schedule = []ScheduleEntry{{VM: "web01", IntervalSeconds: 900, Enabled: true}}
	cfg.Operations = []Operation{promoteOp()}

	if err := s.SaveCache(CachedConfig{ETag: `"x"`, Config: cfg}); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}
	got, ok, err := s.LoadCache()
	if err != nil || !ok {
		t.Fatalf("LoadCache: ok=%v err=%v", ok, err)
	}
	if len(got.Config.Operations) != 0 {
		t.Errorf("operations survived a restart (%d); one would be executed from an instruction nobody re-issued", len(got.Config.Operations))
	}
	if len(got.Config.Schedule) != 1 {
		t.Errorf("the schedule did NOT survive (%d entries); replication would stop during a partition", len(got.Config.Schedule))
	}
}

func TestOperationArgs(t *testing.T) {
	cfg := agentConfig{LibvirtURI: "qemu:///system", TargetURIPattern: "qemu+ssh://%s/system"}

	t.Run("promote", func(t *testing.T) {
		op := promoteOp()
		op.StartVM, op.Force = true, true
		args, err := operationArgs(cfg, nil, op)
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(args, " ")
		for _, want := range []string{
			"-promote", "-target-uri qemu:///system", "-target-domain web01",
			"-promote-mode forced", "-start", "-force-promote", "-promoted-by alice",
		} {
			if !strings.Contains(joined, want) {
				t.Errorf("argv %q is missing %q", joined, want)
			}
		}
		// The peer is NOT passed as an endpoint: promote runs locally and
		// deliberately never reaches the other site.
		if strings.Contains(joined, "prod01") {
			t.Errorf("argv %q names the peer host; promote must not contact it", joined)
		}
	})

	t.Run("invert spans both ends", func(t *testing.T) {
		op := promoteOp()
		op.Kind = OpInvert
		args, err := operationArgs(cfg, nil, op)
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(args, " ")
		for _, want := range []string{
			"-invert", "-source-uri qemu:///system", "-source-domain web01",
			"-target-uri qemu+ssh://prod01/system", "-target-domain web01",
		} {
			if !strings.Contains(joined, want) {
				t.Errorf("argv %q is missing %q", joined, want)
			}
		}
	})

	t.Run("invert without a peer is refused", func(t *testing.T) {
		op := promoteOp()
		op.Kind, op.PeerHost = OpInvert, ""
		if _, err := operationArgs(cfg, nil, op); err == nil {
			t.Error("built an invert with no far end")
		}
	})

	t.Run("set-role needs a role", func(t *testing.T) {
		op := promoteOp()
		op.Kind, op.Mode = OpSetRole, ""
		if _, err := operationArgs(cfg, nil, op); err == nil {
			t.Error("built a set-role with no role")
		}
	})
}
