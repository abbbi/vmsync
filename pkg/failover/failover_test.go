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

package failover

import (
	"strings"
	"testing"
	"time"
)

const nowUnix = 1_800_000_000

// healthyTarget is a replica a sync has demonstrably landed on: every piece
// of corroborating evidence present and consistent.
func healthyTarget() TargetState {
	return TargetState{
		Role:             RoleTarget,
		LastCheckpoint:   "vmsync-cpt-000012",
		LastSyncUnix:     nowUnix - 300,
		CheckpointAtUnix: nowUnix - 360,
		ReplicaSource:    "prod01:web01",
		DisksPresent:     true,
	}
}

func TestAssessPromoteRoleTable(t *testing.T) {
	for _, tc := range []struct {
		role        string
		wantErr     bool
		wantAlready bool
	}{
		{RoleTarget, false, false},
		// paused must be promotable: pausing replication and then failing
		// over is ordinary, and refusing would make paused a trap.
		{RolePaused, false, false},
		{"", false, false},
		{RolePromoted, false, true},
		{RoleSource, true, false},
		{"invented-by-a-newer-build", true, false},
	} {
		t.Run("role="+tc.role, func(t *testing.T) {
			st := healthyTarget()
			st.Role = tc.role
			plan, err := AssessPromote(st, PromoteOptions{Mode: ModeForced, NowUnix: nowUnix})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("role %q was accepted, want refusal", tc.role)
				}
				return
			}
			if err != nil {
				t.Fatalf("role %q refused: %v", tc.role, err)
			}
			if plan.AlreadyPromoted != tc.wantAlready {
				t.Errorf("AlreadyPromoted = %v, want %v", plan.AlreadyPromoted, tc.wantAlready)
			}
		})
	}
}

// TestPromoteRefusesWithoutEvidenceOfARealReplica is the gap a role-only
// check leaves: -reinit deletes a target's disks but deliberately leaves
// its definition alone, so role, last_checkpoint and last_sync_timestamp
// all survive with nothing behind them. Trusting the role alone promotes an
// empty image and reports a confident, fictional data-loss window.
func TestPromoteRefusesWithoutEvidenceOfARealReplica(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*TargetState)
		want   string
	}{
		{"disks deleted by reinit", func(s *TargetState) { s.DisksPresent = false }, "disk files are missing"},
		{"never synced", func(s *TargetState) { s.LastCheckpoint = "" }, "no sync has ever completed"},
		{"no sync timestamp", func(s *TargetState) { s.LastSyncUnix = 0 }, "no last_sync_timestamp"},
		{"not known to be a replica", func(s *TargetState) { s.ReplicaSource = "" }, "not known to be a replica"},
		{"interrupted copy", func(s *TargetState) { s.OverlayPresent = true }, "interrupted"},
		{"last attempt failed", func(s *TargetState) { s.FailureCount = 3 }, "did not succeed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := healthyTarget()
			tc.mutate(&st)

			_, err := AssessPromote(st, PromoteOptions{Mode: ModeForced, NowUnix: nowUnix})
			if err == nil {
				t.Fatal("promoted a target with no usable replica")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}

			// Force is the operator knowingly accepting a questionable
			// copy. It must go through -- and it must NOT then report a
			// data-loss figure derived from metadata it just overrode.
			plan, err := AssessPromote(st, PromoteOptions{Mode: ModeForced, Force: true, NowUnix: nowUnix})
			if err != nil {
				t.Fatalf("force was refused: %v", err)
			}
			if plan.DataLoss.Known {
				t.Errorf("data loss reported as %s after overriding the evidence checks; it must be unknown", plan.DataLoss)
			}
			if len(plan.Notes) == 0 {
				t.Error("a forced promotion recorded no note saying what was overridden")
			}
		})
	}
}

// TestPromoteNeverOverridesAnInFlightSync: there is no version of booting a
// guest on disks another process is still writing that an operator can
// usefully consent to, so Force must not reach it.
func TestPromoteNeverOverridesAnInFlightSync(t *testing.T) {
	st := healthyTarget()
	st.SyncInFlight = true
	for _, force := range []bool{false, true} {
		_, err := AssessPromote(st, PromoteOptions{Mode: ModeForced, Force: force, NowUnix: nowUnix})
		if err == nil {
			t.Fatalf("force=%v: promoted a target with a sync writing it", force)
		}
		if !strings.Contains(err.Error(), "currently writing") {
			t.Errorf("force=%v: error = %q, want it to name the in-flight sync", force, err)
		}
	}
}

// TestAlreadyPromotedStillStarts covers the state the design deliberately
// creates: metadata is written before the domain is booted, so a promotion
// that fails in between leaves it promoted-but-down. Re-issuing must be
// able to finish the job, or that state is unrecoverable through the only
// control an operator has.
func TestAlreadyPromotedStillStarts(t *testing.T) {
	st := healthyTarget()
	st.Role = RolePromoted

	st.Active = false
	plan, err := AssessPromote(st, PromoteOptions{Mode: ModeForced, Start: true, NowUnix: nowUnix})
	if err != nil {
		t.Fatalf("re-promoting a promoted domain errored: %v", err)
	}
	if plan.WriteMetadata {
		t.Error("rewrote the promotion record; the original promotion's time and actor must stand")
	}
	if !plan.StartDomain {
		t.Error("did not start a promoted-but-down domain -- this is exactly the recovery case")
	}

	// Already running: nothing to do at all.
	st.Active = true
	plan, err = AssessPromote(st, PromoteOptions{Mode: ModeForced, Start: true, NowUnix: nowUnix})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if plan.StartDomain {
		t.Error("tried to start an already-running domain")
	}
}

func TestDataLossWindow(t *testing.T) {
	t.Run("a stopped source at checkpoint time is a verified zero", func(t *testing.T) {
		// The only honest basis for claiming nothing was lost: the source
		// could not write after it stopped, so the replica is complete.
		st := healthyTarget()
		st.SourceStoppedAtSync = true
		plan, err := AssessPromote(st, PromoteOptions{Mode: ModePlanned, NowUnix: nowUnix})
		if err != nil {
			t.Fatal(err)
		}
		if !plan.DataLoss.Known || plan.DataLoss.Seconds != 0 || !plan.DataLoss.Verified {
			t.Errorf("data loss = %+v, want a verified 0", plan.DataLoss)
		}
		if len(plan.Notes) != 0 {
			t.Errorf("a correctly executed planned failover produced warnings: %v", plan.Notes)
		}
	})

	// TestDataLossWindow/planned mode alone is not evidence is the bug this
	// replaced: -promote-mode=planned is a string anybody can pass, and the
	// old code returned a hard zero for it. A shutdown with no final sync
	// after it -- or the flag with no shutdown at all -- then reported "0s
	// lost" while discarding everything written since the last scheduled
	// run.
	t.Run("planned mode alone is not evidence", func(t *testing.T) {
		st := healthyTarget() // source was still running at checkpoint time
		plan, err := AssessPromote(st, PromoteOptions{Mode: ModePlanned, NowUnix: nowUnix})
		if err != nil {
			t.Fatal(err)
		}
		if plan.DataLoss.Seconds == 0 {
			t.Error("claimed zero data loss from the mode label alone")
		}
		if plan.DataLoss.Verified {
			t.Error("marked an unverified figure as verified")
		}
		if plan.DataLoss.Seconds != 360 {
			t.Errorf("data loss = %ds, want the real 360s window to the checkpoint", plan.DataLoss.Seconds)
		}
		// And it must say so, not just quietly report a different number.
		if len(plan.Notes) == 0 {
			t.Error("no note explaining that the planned sequence was not completed")
		}
	})

	t.Run("a stopped source is a verified zero in forced mode too", func(t *testing.T) {
		// The evidence is about the DATA, not the procedure, so the label
		// the caller passed is irrelevant to it.
		st := healthyTarget()
		st.SourceStoppedAtSync = true
		plan, err := AssessPromote(st, PromoteOptions{Mode: ModeForced, NowUnix: nowUnix})
		if err != nil {
			t.Fatal(err)
		}
		if !plan.DataLoss.Verified || plan.DataLoss.Seconds != 0 {
			t.Errorf("data loss = %+v, want a verified 0", plan.DataLoss)
		}
	})

	t.Run("measured from the checkpoint, not the end of the copy", func(t *testing.T) {
		// The checkpoint is taken before any data moves, so that is the
		// moment the replica's contents are frozen at. last_sync is written
		// when the copy finishes, which understates the loss by its whole
		// duration.
		plan, err := AssessPromote(healthyTarget(), PromoteOptions{Mode: ModeForced, NowUnix: nowUnix})
		if err != nil {
			t.Fatal(err)
		}
		if plan.DataLoss.Seconds != 360 {
			t.Errorf("data loss = %ds, want 360 (to the checkpoint) not 300 (to the end of the copy)", plan.DataLoss.Seconds)
		}
		if plan.DataLoss.LowerBoundOnly {
			t.Error("marked a checkpoint-derived figure as a lower bound")
		}
	})

	t.Run("older target is labelled a lower bound", func(t *testing.T) {
		st := healthyTarget()
		st.CheckpointAtUnix = 0 // written by a vmsync too old to record it
		plan, err := AssessPromote(st, PromoteOptions{Mode: ModeForced, NowUnix: nowUnix})
		if err != nil {
			t.Fatal(err)
		}
		if !plan.DataLoss.LowerBoundOnly {
			t.Error("reported a last_sync-derived figure as exact; it understates by the copy duration")
		}
		if !strings.Contains(plan.DataLoss.String(), "at least") {
			t.Errorf("rendered %q, want it to read as a lower bound", plan.DataLoss)
		}
	})

	t.Run("clock skew is surfaced, not clamped away", func(t *testing.T) {
		// The source's clock ran ahead, so the target's timestamp is in
		// this host's future. Silently clamping to 0 would report a stale
		// replica as perfectly current.
		st := healthyTarget()
		st.CheckpointAtUnix = nowUnix + 1200
		plan, err := AssessPromote(st, PromoteOptions{Mode: ModeForced, NowUnix: nowUnix})
		if err != nil {
			t.Fatal(err)
		}
		if !plan.DataLoss.ClockSkew || plan.DataLoss.Seconds != 0 {
			t.Errorf("data loss = %+v, want 0 with the skew flagged", plan.DataLoss)
		}
		if !strings.Contains(plan.DataLoss.String(), "clocks disagree") {
			t.Errorf("rendered %q without warning about the clocks", plan.DataLoss)
		}
	})

	t.Run("unknown never renders as a number", func(t *testing.T) {
		d := DataLoss{Reason: "the target records no sync time"}
		if got := d.String(); !strings.HasPrefix(got, "unknown") {
			t.Errorf("rendered %q; an unmeasured window must never look like a measurement", got)
		}
	})
}

// --- inversion -----------------------------------------------------------

func invertible() PairState {
	return PairState{
		OldSource: DomainEnd{
			Host: "prod01", Domain: "web01", Role: RoleSource, Active: false,
			ReplicaTargets: []string{"dr01:web01"}, HasCheckpoints: true,
		},
		Promoted: DomainEnd{
			Host: "dr01", Domain: "web01", Role: RolePromoted, Active: true,
			ReplicaSource: "prod01:web01",
		},
	}
}

func TestAssessInvertSwapsBothEnds(t *testing.T) {
	plan, err := AssessInvert(invertible())
	if err != nil {
		t.Fatalf("AssessInvert: %v", err)
	}
	if plan.AlreadyInverted {
		t.Fatal("reported a pre-inversion pair as already inverted")
	}
	if plan.NewSourceUpdates[FieldReplicationRole] != RoleSource ||
		plan.NewSourceUpdates[FieldReplicaTargets] != "prod01:web01" {
		t.Errorf("new source updates = %v", plan.NewSourceUpdates)
	}
	if plan.NewTargetUpdates[FieldReplicationRole] != RoleTarget ||
		plan.NewTargetUpdates[FieldReplicaSource] != "dr01:web01" {
		t.Errorf("new target updates = %v", plan.NewTargetUpdates)
	}
	// The promotion record must not survive: this domain is simply the
	// primary now, which is what role=source says.
	for _, f := range []string{FieldPromotedAt, FieldPromotedBy, FieldPromotedFrom, FieldPromotionMode, FieldReplicaSource} {
		if !contains(plan.NewSourceRemovals, f) {
			t.Errorf("%s is not stripped from the new source", f)
		}
	}
	// The old source's checkpoint bookkeeping described a chain running the
	// other way.
	for _, f := range []string{FieldReplicaTargets, FieldLastCheckpoint, FieldLastSync, FieldFailureCount} {
		if !contains(plan.NewTargetRemovals, f) {
			t.Errorf("%s is not stripped from the new target", f)
		}
	}
	// The real libvirt checkpoint objects are a separate thing from the
	// metadata strings, and they are what a later sync would try to chain
	// onto.
	if !plan.DropCheckpointsOnOldSource {
		t.Error("did not schedule deletion of the old source's real checkpoint objects")
	}
}

// TestAssessInvertConverges: an inversion that completed but whose result
// never reached the control plane WILL be re-issued. Reporting a hard
// failure for work that actually succeeded would leave a correct pair
// recorded as broken, with its schedule never migrated.
func TestAssessInvertConverges(t *testing.T) {
	st := invertible()
	st.OldSource.Role = RoleTarget
	st.OldSource.ReplicaTargets = nil
	st.OldSource.ReplicaSource = "dr01:web01"
	st.Promoted.Role = RoleSource
	st.Promoted.ReplicaSource = ""
	st.Promoted.ReplicaTargets = []string{"prod01:web01"}

	plan, err := AssessInvert(st)
	if err != nil {
		t.Fatalf("an already-inverted pair was reported as an error: %v", err)
	}
	if !plan.AlreadyInverted {
		t.Error("did not recognise the post-inversion arrangement")
	}
}

func TestAssessInvertRefusals(t *testing.T) {
	t.Run("target end was never promoted", func(t *testing.T) {
		st := invertible()
		st.Promoted.Role = RoleTarget
		if _, err := AssessInvert(st); err == nil {
			t.Fatal("inverted a pair that never failed over")
		}
	})

	t.Run("old source still running", func(t *testing.T) {
		// It is about to become a replication target, and a running target
		// is one scheduled sync away from being overwritten live.
		st := invertible()
		st.OldSource.Active = true
		_, err := AssessInvert(st)
		if err == nil || !strings.Contains(err.Error(), "still running") {
			t.Fatalf("err = %v, want a refusal naming the running domain", err)
		}
	})

	t.Run("source fans out to other targets", func(t *testing.T) {
		// A domain cannot be both a replication target and the live source
		// of a fan-out. Silently picking one reading either orphans the
		// other targets or leaves a target replicating onward.
		st := invertible()
		st.OldSource.ReplicaTargets = []string{"dr01:web01", "dr02:web01"}
		_, err := AssessInvert(st)
		if err == nil || !strings.Contains(err.Error(), "dr02:web01") {
			t.Fatalf("err = %v, want a refusal naming the other target", err)
		}
	})

	t.Run("pair not recorded on the source", func(t *testing.T) {
		st := invertible()
		st.OldSource.ReplicaTargets = []string{"somewhere-else:web01"}
		if _, err := AssessInvert(st); err == nil {
			t.Fatal("inverted a pair the source does not record")
		}
	})
}

// TestRemoveRefKeepsTheRestOfTheFanOut is the specific data-destroying
// mistake: stripping replica_targets wholesale erases the record of every
// OTHER target that source replicated to, and nothing anywhere remembers
// they existed.
func TestRemoveRefKeepsTheRestOfTheFanOut(t *testing.T) {
	remaining, removed := removeRef([]string{"dr01:web01", "dr02:web01", "dr03:web01"}, "dr02:web01")
	if !removed {
		t.Fatal("did not remove the named peer")
	}
	if len(remaining) != 2 || remaining[0] != "dr01:web01" || remaining[1] != "dr03:web01" {
		t.Errorf("remaining = %v, want the other two preserved", remaining)
	}

	if _, removed := removeRef([]string{"DR01:web01"}, "dr01:web01"); !removed {
		t.Error("host comparison is case-sensitive; it must not be")
	}
}

func contains(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}

// A recorded verification failure must block a promotion.
//
// This is the single most valuable effect of persisting the verdict at all.
// Everything else evidenceProblems checks says the replica may be STALE or
// incomplete -- no checkpoint, no timestamp, an uncommitted overlay, a
// non-zero failure_count. This one says an attempt finished and the
// resulting replica did not match its source: the replica may be WRONG.
// Without it, a replica that failed verify last night promoted with a clean
// bill of health today, which is the worst possible moment to find out.
func TestVerifyFailureBlocksPromotion(t *testing.T) {
	t.Run("a healthy replica has no problems", func(t *testing.T) {
		if problems := evidenceProblems(healthyTarget()); len(problems) != 0 {
			t.Fatalf("healthy target reported problems: %v", problems)
		}
	})

	t.Run("a recorded failure is a problem", func(t *testing.T) {
		st := healthyTarget()
		st.VerifyState = VerifyStateFailedValue
		st.VerifyFailedAt = nowUnix - 3600

		problems := evidenceProblems(st)
		if len(problems) != 1 {
			t.Fatalf("got %d problems, want exactly 1: %v", len(problems), problems)
		}
		// The message has to distinguish "wrong" from "stale", or an
		// operator reads it as another way of saying the sync is behind.
		if !strings.Contains(problems[0], "differing from its source") {
			t.Errorf("problem = %q, want it to say the contents differ", problems[0])
		}
		// And point at where the diagnosis actually is, since this metadata
		// deliberately records only the verdict.
		if !strings.Contains(problems[0], "log") {
			t.Errorf("problem = %q, want it to point at the log for which blocks differed", problems[0])
		}
		// The date matters: "failed 20 minutes ago" and "failed in March"
		// are different decisions.
		if !strings.Contains(problems[0], time.Unix(nowUnix-3600, 0).UTC().Format("2006-01-02")) {
			t.Errorf("problem = %q, want it to name when the failure was recorded", problems[0])
		}
	})

	t.Run("a failure with no date still blocks", func(t *testing.T) {
		st := healthyTarget()
		st.VerifyState = VerifyStateFailedValue
		// VerifyFailedAt deliberately left zero: a replica written by a
		// vmsync that recorded the state but not the date must still be
		// distrusted, not waved through for lack of a timestamp.
		problems := evidenceProblems(st)
		if len(problems) != 1 {
			t.Fatalf("got %d problems, want 1: %v", len(problems), problems)
		}
		if !strings.Contains(problems[0], "unrecorded time") {
			t.Errorf("problem = %q, want it to admit the time is unknown", problems[0])
		}
	})

	t.Run("it is independent of failure_count", func(t *testing.T) {
		// Different facts, and both must be reported. failure_count says
		// the last attempt did not finish; this says one did and produced a
		// replica that does not match.
		st := healthyTarget()
		st.VerifyState = VerifyStateFailedValue
		st.FailureCount = 3
		if problems := evidenceProblems(st); len(problems) != 2 {
			t.Errorf("got %d problems, want both reported: %v", len(problems), problems)
		}
	})
}
