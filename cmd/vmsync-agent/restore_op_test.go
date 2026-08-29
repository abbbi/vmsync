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
	"testing"
	"time"

	"vmsync/pkg/util"
)

func restoreOp() Operation {
	return Operation{
		ID:   "op-1",
		Kind: OpRestore,
		VM:   "web01",
		Tag:  "1756041600-vmsync-cpt-000042",
	}
}

// A malformed tag must be caught in Validate, NOT in operationArgs.
//
// The distinction is not stylistic. A kind that passes Validate and then fails
// argv-building is recorded OpStateRefused, and Seen() then refuses that ID in
// any state forever -- so the operator loses the operation rather than the
// attempt, and cannot retry it. Validate's refusals are reported the same way
// but are the intended path for a bad parameter.
func TestValidateRefusesAnUnusableRestoreTag(t *testing.T) {
	now := time.Now()
	peers := []string{"hyper01p:web01"}

	if err := restoreOp().Validate(peers, now); err != nil {
		t.Fatalf("a well-formed restore was refused: %v", err)
	}

	for name, tag := range map[string]string{
		"empty":                 "",
		"a path":                "../../etc/passwd",
		"a path separator":      "1756041600-vmsync/cpt",
		"shell metacharacters":  "1756041600-$(rm -rf /)",
		"no separator":          "notatag",
		"a non-numeric instant": "abc-vmsync-cpt-1",
	} {
		t.Run(name, func(t *testing.T) {
			op := restoreOp()
			op.Tag = tag
			if err := op.Validate(peers, now); err == nil {
				t.Errorf("Validate accepted %q as a restore point; this value is interpolated into shell commands including rm -rf", tag)
			}
		})
	}
}

func TestRestoreArgvIsLocalAndForced(t *testing.T) {
	cfg := agentConfig{LibvirtURI: "qemu:///system", TargetURIPattern: "qemu+ssh://%s/system"}
	args, err := operationArgs(cfg, nil, restoreOp())
	if err != nil {
		t.Fatalf("operationArgs: %v", err)
	}
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"-restore-restore-point 1756041600-vmsync-cpt-000042",
		// Without this the run only prints an assessment and changes nothing
		// -- an operation reporting success having done nothing at all.
		"-force-restore",
		// Local, like promote and shutdown-domain: a restore acts on the host
		// it runs on, so it needs no credentials to reach anywhere.
		"-target-uri qemu:///system",
		"-target-domain web01",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv %q is missing %q", joined, want)
		}
	}

	// -target-disk-path is deliberately absent: the engine derives the
	// directory from the target domain's own definition, which is the same
	// rule the sync used to place the restore points. A configured path
	// agrees only when it happens to name the directory the disks are really
	// in.
	if strings.Contains(joined, "-target-disk-path") {
		t.Errorf("argv passes -target-disk-path; the engine derives it more correctly: %q", joined)
	}
	// No SSH credentials for a local operation, the property that makes a
	// failover work when the other site is unreachable.
	if strings.Contains(joined, "-ssh-") {
		t.Errorf("argv carries SSH credentials for a local operation: %q", joined)
	}
}

// A reinit is the only kind here that is a SYNC, so it needs the pair's whole
// transport configuration -- which lives on the schedule, not on the
// operation. Refusing when there is no entry is what stops it running a
// half-configured sync against a production pair.
func TestReinitRefusesAVMWithNoScheduleEntry(t *testing.T) {
	cfg := agentConfig{LibvirtURI: "qemu:///system"}
	op := Operation{ID: "op-2", Kind: OpReinit, VM: "web01"}

	_, err := operationArgs(cfg, nil, op)
	if err == nil {
		t.Fatal("a reinit with no schedule entry was accepted")
	}
	// The message has to say WHERE it should have been issued: a reinit runs
	// on the source's agent, unlike promote and restore, and issuing it to
	// the target is the obvious mistake.
	if !strings.Contains(err.Error(), "SOURCE") {
		t.Errorf("refusal %q does not say a reinit runs on the source's agent", err.Error())
	}
}

func TestScheduleEntryFor(t *testing.T) {
	schedule := []ScheduleEntry{
		{VM: "db01"},
		{VM: "web01", Profile: SyncProfile{Retention: "24,3h"}},
	}
	got, ok := scheduleEntryFor(schedule, "web01")
	if !ok || got.Profile.Retention != "24,3h" {
		t.Fatalf("scheduleEntryFor = %+v ok:%v, want the web01 entry", got, ok)
	}
	if _, ok := scheduleEntryFor(schedule, "WEB01"); ok {
		t.Error("matching must be case-sensitive like libvirt; a loose match resolves to another pair's transport settings")
	}
	if _, ok := scheduleEntryFor(nil, "web01"); ok {
		t.Error("an empty schedule matched")
	}
}

// -retention has to reach the argv or an estate run entirely through the
// control plane takes no restore points at all -- and a restore operation has
// nothing to restore. This is the join that made the whole feature reachable
// from the UI.
func TestRetentionReachesTheSyncArgv(t *testing.T) {
	req := SyncRequest{
		SourceURI:    "qemu:///system",
		SourceDomain: "web01",
		TargetURI:    "qemu+ssh://hyper02p/system",
		TargetDomain: "web01",
		Profile:      SyncProfile{Retention: "24,3h", TargetDiskPath: "/data/replicas"},
	}
	joined := strings.Join(req.CommandArgs(), " ")
	if !strings.Contains(joined, "-retention 24,3h") {
		t.Fatalf("argv %q does not carry -retention", joined)
	}

	req.Profile.Retention = ""
	if strings.Contains(strings.Join(req.CommandArgs(), " "), "-retention") {
		t.Error("an empty retention must pass no flag at all, not an empty one")
	}
}

func TestProfileValidateChecksRetention(t *testing.T) {
	// The engine's own parser, so a value this agent accepts and vmsync then
	// rejects cannot become a schedule entry that looks configured and never
	// runs.
	if err := (SyncProfile{Retention: "24,3h"}).Validate(); err != nil {
		t.Fatalf("a valid retention was refused: %v", err)
	}
	if err := (SyncProfile{}).Validate(); err != nil {
		t.Fatalf("an absent retention must be fine: %v", err)
	}
	for _, bad := range []string{"24", "0,3h", "-1,3h", "24,", "24,banana", "abc,3h"} {
		if err := (SyncProfile{Retention: bad}).Validate(); err == nil {
			t.Errorf("Validate accepted retention %q", bad)
		}
	}
}

// Abandon is the one hole in "recorded once, never retried", so what it does
// and what it does not do are both worth pinning.
func TestAbandonMakesAnOperationRetryable(t *testing.T) {
	l := newOperationLedger(t.TempDir())
	op := restoreOp()

	if err := l.Begin(op, time.Now()); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, seen := l.Seen(op.ID); !seen {
		t.Fatal("Begin did not record the intent")
	}

	if err := l.Abandon(op.ID); err != nil {
		t.Fatalf("Abandon: %v", err)
	}
	// Seen reports ANY recorded id as seen, whatever its state, so leaving a
	// `running` entry behind would make the operation permanently
	// un-retryable -- which is exactly the damage this exists to undo.
	if _, seen := l.Seen(op.ID); seen {
		t.Error("the abandoned operation is still recorded, so the next tick will skip it forever")
	}
	// And it must not be reported to the UI as an outcome, because none
	// happened.
	for _, r := range l.Results() {
		if r.ID == op.ID {
			t.Errorf("the abandoned operation is still reported as a result: %+v", r)
		}
	}

	// Retrying really works: a second Begin with the same id succeeds.
	if err := l.Begin(op, time.Now()); err != nil {
		t.Fatalf("could not re-Begin an abandoned operation: %v", err)
	}
	if _, seen := l.Seen(op.ID); !seen {
		t.Error("the retry was not recorded")
	}
}

func TestAbandonSurvivesARestart(t *testing.T) {
	// The removal has to reach disk, or an agent restarted between the
	// deferral and the retry would load a `running` entry and refuse the
	// operation forever.
	dir := t.TempDir()
	op := restoreOp()

	first := newOperationLedger(dir)
	if err := first.Begin(op, time.Now()); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := first.Abandon(op.ID); err != nil {
		t.Fatalf("Abandon: %v", err)
	}

	second := newOperationLedger(dir)
	if err := second.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, seen := second.Seen(op.ID); seen {
		t.Error("the abandoned entry came back after a restart; the removal did not reach disk")
	}
}

func TestAbandonAnUnknownIDIsNotAnError(t *testing.T) {
	// It runs on a path where the caller cannot be certain Begin landed.
	l := newOperationLedger(t.TempDir())
	if err := l.Abandon("never-seen"); err != nil {
		t.Fatalf("Abandon of an unknown id: %v", err)
	}
}

// The exit status is a contract between two binaries: vmsync exits with it and
// the agent reads it off a finished process. Pinned because a change on one
// side alone turns "I did nothing, retry me" into "I failed" -- which burns
// the operation's id permanently.
func TestExitBusyIsDistinctFromEveryOrdinaryStatus(t *testing.T) {
	if util.ExitBusy != 75 {
		t.Errorf("ExitBusy = %d, want 75 (sysexits.h EX_TEMPFAIL)", util.ExitBusy)
	}
	for _, ordinary := range []int{0, 1, 2} {
		if util.ExitBusy == ordinary {
			t.Errorf("ExitBusy collides with the ordinary status %d", ordinary)
		}
	}
}
