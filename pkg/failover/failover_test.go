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

// --- replica_incomplete --------------------------------------------------

// The parser is the one piece of this that reads something written by
// another process, possibly by another VERSION, so every shape it can be
// handed is enumerated rather than sampled.
//
// The rule it has to obey is stated once here and asserted everywhere below:
// the PRESENCE of the field is the finding, and parsing only decides the
// wording. Nothing may make a promotion possible that the raw field alone
// would have refused.
func TestParseReplicaIncomplete(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want ReplicaIncomplete
	}{
		{
			name: "well formed, every key",
			raw:  "verb=reinit,at=1758441600,action=9f3c1a2b4d5e6f70,host=hv-a,aside=1758441600",
			want: ReplicaIncomplete{
				Parsed: true, Verb: "reinit", AtUnix: 1758441600,
				ActionID: "9f3c1a2b4d5e6f70", Host: "hv-a", AsideStamp: "1758441600",
			},
		},
		{
			// A full sync renames nothing aside, so its value carries no
			// aside key at all. It must still parse: this is the ordinary
			// shape of that path, not a damaged value.
			name: "no aside stamp, which is a full sync",
			raw:  "verb=full-sync,at=1758441600,action=abc123,host=hv-a",
			want: ReplicaIncomplete{
				Parsed: true, Verb: "full-sync", AtUnix: 1758441600,
				ActionID: "abc123", Host: "hv-a",
			},
		},
		{
			// A key a newer vmsync added. Ignoring it is what lets that
			// engine write one without every older engine on the estate
			// losing the ability to read the field at all -- which would lose
			// the refusal, not merely the extra key.
			name: "an unknown key is ignored, the rest still reads",
			raw:  "verb=restore,at=1758441600,host=hv-b,tier=gold,aside=1758441700",
			want: ReplicaIncomplete{
				Parsed: true, Verb: "restore", AtUnix: 1758441600,
				Host: "hv-b", AsideStamp: "1758441700",
			},
		},
		{
			// A verb this build has never heard of, taken as found. A newer
			// engine's interrupted copy leaves exactly the same wreckage as
			// this one's, so refusing to read the verb would discard a real
			// finding over a vocabulary difference.
			name: "a verb from a newer build is read, not rejected",
			raw:  "verb=rebase-from-cold,at=1758441600,host=hv-a",
			want: ReplicaIncomplete{
				Parsed: true, Verb: "rebase-from-cold", AtUnix: 1758441600, Host: "hv-a",
			},
		},
		{
			// No verb: not enough to word the refusal around, so Parsed is
			// false. What was readable is still filled in, and Raw is kept.
			name: "missing verb",
			raw:  "at=1758441600,host=hv-a",
			want: ReplicaIncomplete{AtUnix: 1758441600, Host: "hv-a"},
		},
		{
			name: "missing time",
			raw:  "verb=reinit,host=hv-a,aside=1758441600",
			want: ReplicaIncomplete{Verb: "reinit", Host: "hv-a", AsideStamp: "1758441600"},
		},
		{
			// A time that will not parse must not become a time. Zero, and
			// Parsed false -- the refusal then simply does not claim to know
			// when the copy started.
			name: "an unreadable time is not a time",
			raw:  "verb=reinit,at=yesterday,host=hv-a",
			want: ReplicaIncomplete{Verb: "reinit", Host: "hv-a"},
		},
		{
			name: "garbage",
			raw:  "not a value at all",
			want: ReplicaIncomplete{},
		},
		{
			// The only case that means "nothing happened". Everything else
			// above still refuses a promotion.
			name: "empty",
			raw:  "",
			want: ReplicaIncomplete{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.want.Raw = tc.raw
			got := ParseReplicaIncomplete(tc.raw)
			if got != tc.want {
				t.Errorf("ParseReplicaIncomplete(%q) =\n%+v\nwant\n%+v", tc.raw, got, tc.want)
			}
			// Raw is kept on every path, readable or not, because an
			// unreadable value is the one somebody has to be shown verbatim
			// to work out what wrote it.
			if got.Raw != tc.raw {
				t.Errorf("Raw = %q, want the value exactly as the domain held it (%q)", got.Raw, tc.raw)
			}
		})
	}
}

// The keep-the-aside rule, enumerated. Every shape the engine can be holding
// when it is about to arm, because the one that goes wrong costs the operator
// the location of the last complete copy of a production machine -- and it
// goes wrong silently, since the promotion stays refused on either record and
// nothing else would ever say the stamp had been dropped.
func TestKeepExistingReplicaIncomplete(t *testing.T) {
	const asideStamp = "1758441600"
	for _, tc := range []struct {
		name     string
		existing string
		newStamp string
		want     bool
		why      string
	}{
		{
			// THE case this exists for: an interrupted reinit's record, and
			// the full sync that follows it, which renames nothing and so
			// names nothing.
			name:     "existing names files aside, incoming names none",
			existing: "verb=reinit,at=1758441600,action=9f3c1a2b4d5e6f70,host=hv-a,aside=" + asideStamp,
			newStamp: "",
			want:     true,
			why:      "the incoming full-sync record carries no stamp, so overwriting loses the only pointer to the .vmsync-replaced files",
		},
		{
			// A second reinit set a complete copy aside of its own, under a
			// stamp of its own. Its record describes the disks that are on
			// the host NOW; the older one describes files the second reinit
			// has just replaced. Keeping the stale one would name the wrong
			// set.
			name:     "existing names files aside, incoming names its own",
			existing: "verb=reinit,at=1758441600,host=hv-a,aside=" + asideStamp,
			newStamp: "1758445200",
			want:     false,
			why:      "the incoming record names a displaced set of its own, so nothing is lost by replacing",
		},
		{
			// An interrupted full sync, then another. Neither record points
			// anywhere, so the fresher one is strictly better: same refusal,
			// but the action id and host of the run that is actually running.
			name:     "existing names nothing aside",
			existing: "verb=full-sync,at=1758441600,action=abc123,host=hv-a",
			newStamp: "",
			want:     false,
			why:      "there is no stamp to protect, so the newer record wins on freshness",
		},
		{
			name:     "no existing record at all",
			existing: "",
			newStamp: "",
			want:     false,
			why:      "an unarmed replica has nothing to keep",
		},
		{
			// DECIDED DELIBERATELY: keep. The value is unreadable as a whole
			// -- no verb, no time, so ParseReplicaIncomplete leaves Parsed
			// false -- yet the aside= field itself is intact. A stamp this
			// build can still read is a stamp an operator can still use, and
			// the rule is built on the stamp's presence rather than on the
			// record parsing, exactly as the refusal is built on the field's
			// presence rather than on its contents.
			name:     "unparsable existing value that still carries an aside",
			existing: "written by something newer,aside=" + asideStamp + ",,,",
			newStamp: "",
			want:     true,
			why:      "the stamp is readable even though the record is not, and it is still the only pointer to the displaced disks",
		},
		{
			// The other half of that: unreadable AND carrying nothing worth
			// keeping. "aside" appears in the text but never as its own
			// field, so there is no stamp here, only the word.
			name:     "unparsable existing value with no aside field",
			existing: "aside from that it is not the grammar at all",
			newStamp: "",
			want:     false,
			why:      "nothing here is a usable stamp, so there is nothing to protect",
		},
		{
			// An empty stamp written out longhand by a newer engine is not a
			// stamp. It names no files, so there is nothing to recover from
			// and nothing to keep the record for.
			name:     "existing carries an empty aside",
			existing: "verb=reinit,at=1758441600,host=hv-a,aside=",
			newStamp: "",
			want:     false,
			why:      "an empty stamp names no files",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := KeepExistingReplicaIncomplete(tc.existing, tc.newStamp); got != tc.want {
				t.Errorf("KeepExistingReplicaIncomplete(%q, %q) = %v, want %v: %s",
					tc.existing, tc.newStamp, got, tc.want, tc.why)
			}
		})
	}
}

// The decision never depends on WHICH verb is already there, and that is
// worth pinning separately: a future reader looking at the call site sees it
// invoked on the full-sync path and could reasonably decide to narrow the
// rule to "keep only a reinit record". That would drop a restore's stamp,
// which names exactly the same kind of displaced-aside files.
func TestKeepExistingReplicaIncompleteIgnoresTheVerb(t *testing.T) {
	for _, verb := range []string{"reinit", "force-clean", "restore", "rebase-from-cold"} {
		if !KeepExistingReplicaIncomplete("verb="+verb+",at=1758441600,host=hv-a,aside=1758441600", "") {
			t.Errorf("a record with verb=%q and an aside stamp was not kept; the stamp is what matters, whatever wrote it", verb)
		}
	}
}

// The gate itself: CI-02 in one test.
//
// An interrupted -reinit leaves a target whose every other field describes
// the replica it REPLACED, so this fixture is deliberately the healthy one
// with nothing else spoiled. Remove the branch and the promotion below
// succeeds silently, reporting a confident data-loss window measured off
// metadata that describes a different set of disks.
func TestAnInterruptedFullCopyRefusesPromotion(t *testing.T) {
	armed := func() TargetState {
		st := healthyTarget()
		st.ReplicaIncomplete = "verb=reinit,at=1758441600,action=9f3c1a2b4d5e6f70,host=hv-a,aside=1758441600"
		return st
	}

	t.Run("it is the only thing wrong, and it is enough", func(t *testing.T) {
		problems := evidenceProblems(armed())
		if len(problems) != 1 {
			t.Fatalf("got %d problems, want exactly 1 (everything else about this replica reads healthy): %v", len(problems), problems)
		}
		p := problems[0]
		// Each of these is in the message because leaving it out costs an
		// operator time in the middle of an outage.
		for _, want := range []string{
			"never recorded as finished",  // what happened
			"reinit",                      // which verb, so they know what to go and look at
			"2025-09-21",                  // when it started
			"hv-a",                        // and from where
			"replaced",                    // that the metadata above describes a DIFFERENT replica
			".vmsync-replaced-1758441600", // the one-line recovery
		} {
			if !strings.Contains(p, want) {
				t.Errorf("problem = %q\nwant it to contain %q", p, want)
			}
		}
	})

	t.Run("it reads FIRST, ahead of the checks it contradicts", func(t *testing.T) {
		// Ordering is load-bearing rather than cosmetic: the other problems
		// are answers about a different set of disks, so an operator has to
		// meet this one before them or they reason from the wrong premise.
		st := armed()
		st.DisksPresent = false
		st.FailureCount = 2
		problems := evidenceProblems(st)
		if len(problems) < 2 {
			t.Fatalf("expected several problems, got %v", problems)
		}
		if !strings.Contains(problems[0], "never recorded as finished") {
			t.Errorf("problems[0] = %q, want the interrupted copy reported first", problems[0])
		}
	})

	t.Run("promotion is refused without the override", func(t *testing.T) {
		_, err := AssessPromote(armed(), PromoteOptions{Mode: ModeForced, NowUnix: nowUnix})
		if err == nil {
			t.Fatal("a replica a full copy was part-way through rewriting was promoted with no override: this is CI-02")
		}
		if !strings.Contains(err.Error(), ReplicaReplacedSuffix) {
			t.Errorf("refusal = %q, want it to name the aside suffix, which is the only record of where the complete copy went", err)
		}
	})

	t.Run("an unreadable value still refuses", func(t *testing.T) {
		// Fail closed. A value written by a newer build, or torn mid-splice,
		// must not read as "no record" -- that is precisely the direction
		// that promotes a half-written image.
		st := armed()
		st.ReplicaIncomplete = "who wrote this"
		problems := evidenceProblems(st)
		if len(problems) != 1 {
			t.Fatalf("an unreadable record produced %d problems, want 1: %v", len(problems), problems)
		}
		if !strings.Contains(problems[0], "could not be read") {
			t.Errorf("problem = %q, want it to admit the record is unreadable rather than inventing details", problems[0])
		}
		if !strings.Contains(problems[0], "never recorded as finished") {
			t.Errorf("problem = %q, want the finding itself to survive the failed parse", problems[0])
		}
		if _, err := AssessPromote(st, PromoteOptions{Mode: ModeForced, NowUnix: nowUnix}); err == nil {
			t.Fatal("an unreadable replica_incomplete was waved through")
		}
	})

	t.Run("a full sync does not send anyone looking for aside files", func(t *testing.T) {
		// That path writes bases where no disk file existed, so it renames
		// nothing aside. Naming a suffix here would cost an operator an hour
		// hunting for files that were never created.
		st := armed()
		st.ReplicaIncomplete = "verb=full-sync,at=1758441600,host=hv-a"
		p := evidenceProblems(st)[0]
		if strings.Contains(p, ReplicaReplacedSuffix) {
			t.Errorf("problem = %q, want it NOT to name aside files a full sync never creates", p)
		}
		if !strings.Contains(p, "re-run the sync") {
			t.Errorf("problem = %q, want it to name the recovery that does exist", p)
		}
	})
}

// It is an EVIDENCE problem, not a role one, and the difference is the whole
// of what an operator is allowed to do during a real outage.
//
// -force-promote must still get through -- a questionable copy beats nothing
// when the other site is gone -- and the window it reports must become
// unknown rather than the confident figure the contradicted metadata would
// have produced. That second half is what makes forcing safe to offer: the
// operator is told the number cannot be trusted instead of being handed one.
func TestForcingPastAnInterruptedCopyReportsNoWindow(t *testing.T) {
	st := healthyTarget()
	st.ReplicaIncomplete = "verb=reinit,at=1758441600,action=9f3c,host=hv-a,aside=1758441600"

	// The control: the same fixture without the field reports a normal
	// window, so the change below can only have come from the field.
	clean, err := AssessPromote(healthyTarget(), PromoteOptions{Mode: ModeForced, Force: true, NowUnix: nowUnix})
	if err != nil {
		t.Fatalf("the healthy control was refused: %v", err)
	}
	if !clean.DataLoss.Known {
		t.Fatalf("the control reported no window (%s); this test cannot tell the two cases apart", clean.DataLoss)
	}

	plan, err := AssessPromote(st, PromoteOptions{Mode: ModeForced, Force: true, NowUnix: nowUnix})
	if err != nil {
		t.Fatalf("force was refused: %v -- during a real outage an operator may knowingly boot a half-written copy rather than nothing", err)
	}
	if plan.DataLoss.Known {
		t.Errorf("a forced promotion past an interrupted copy reported a normal data-loss window of %s -- the metadata that figure comes from describes the replica the copy replaced, not these disks", plan.DataLoss)
	}
	if !plan.WriteMetadata {
		t.Error("a forced promotion produced no promotion record")
	}
	if !strings.Contains(strings.Join(plan.Notes, "; "), "never recorded as finished") {
		t.Errorf("notes = %v, want the interrupted copy named among what was overridden", plan.Notes)
	}
}

// The strip lists: an inversion must not carry this field onto either end.
//
// Both directions matter and they fail differently. Left on the new TARGET it
// refuses the promotion of a replica about to be rebuilt from scratch; left
// on the new SOURCE it is inherited onto every future target by
// UpdateSyncMetadata, refusing promotions of replicas nothing ever
// interrupted.
func TestInversionStripsAnInterruptedCopyFromBothEnds(t *testing.T) {
	plan, err := AssessInvert(PairState{
		OldSource: DomainEnd{Host: "prod01", Domain: "web01", Role: RoleSource,
			ReplicaTargets: []string{"dr01:web01"}},
		Promoted: DomainEnd{Host: "dr01", Domain: "web01", Role: RolePromoted,
			ReplicaSource: "prod01:web01"},
	})
	if err != nil {
		t.Fatalf("AssessInvert: %v", err)
	}
	for name, list := range map[string][]string{
		"NewTargetRemovals": plan.NewTargetRemovals,
		"NewSourceRemovals": plan.NewSourceRemovals,
	} {
		found := false
		for _, f := range list {
			if f == FieldReplicaIncomplete {
				found = true
			}
		}
		if !found {
			t.Errorf("%s does not strip %s", name, FieldReplicaIncomplete)
		}
	}
}
