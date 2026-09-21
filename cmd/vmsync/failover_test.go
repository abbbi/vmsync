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
	"reflect"
	"strings"
	"testing"

	"vmsync/pkg/failover"
	"vmsync/pkg/libvirtsync"
)

// promoteTestNowUnix is the promoting host's clock for these tests, fixed so
// the data-loss arithmetic is repeatable.
const promoteTestNowUnix int64 = 1700000000

// promotableState is a replica with nothing wrong with it: a sync has
// completed, the source is recorded, the disks are where they should be.
// Each test spoils exactly one thing about it, so that whatever refusal
// follows can only have come from the thing that was spoiled.
func promotableState() libvirtsync.FailoverState {
	return libvirtsync.FailoverState{
		Exists:           true,
		Role:             libvirtsync.RoleTarget,
		LastCheckpoint:   "vmsync-cpt-000012",
		LastSyncUnix:     promoteTestNowUnix - 300,
		CheckpointAtUnix: promoteTestNowUnix - 360,
		ReplicaSource:    "prod01:web01",
	}
}

// The wiring, and only the wiring: a verify failure recorded on the domain
// has to reach the decision that refuses it.
//
// This test exists because that link was missing and nothing noticed. The
// rule lives in pkg/failover and was tested there against a TargetState
// built by hand, so the gate passed its own tests while no promotion ever
// carried the field to it: a replica a -verify run had proven did not match
// its source was promoted silently, without -force-promote being asked for.
// Which is why the real AssessPromote is called here rather than the rule
// being restated -- restating it would pass just as happily with the field
// dropped again, which is the one thing this must not do.
func TestPromoteCarriesAVerifyFailureIntoTheDecision(t *testing.T) {
	opts := func(force bool) failover.PromoteOptions {
		return failover.PromoteOptions{
			Mode:    failover.ModePlanned,
			Start:   true,
			Force:   force,
			NowUnix: promoteTestNowUnix,
		}
	}

	// The control, and it is not a formality: without it, a fixture broken
	// in some unrelated way would refuse every promotion and the refusal
	// below would prove nothing at all.
	t.Run("a replica with no finding against it promotes", func(t *testing.T) {
		plan, err := failover.AssessPromote(promoteTargetState(promotableState(), true, false), opts(false))
		if err != nil {
			t.Fatalf("a healthy replica was refused: %v", err)
		}
		if !plan.WriteMetadata {
			t.Error("a healthy replica produced no promotion record")
		}
	})

	t.Run("the field reaches the decision input", func(t *testing.T) {
		st := promotableState()
		st.VerifyState = libvirtsync.VerifyStateFailed
		st.VerifyFailedAt = promoteTestNowUnix - 3600

		tgt := promoteTargetState(st, true, false)
		if tgt.VerifyState != libvirtsync.VerifyStateFailed {
			t.Errorf("verify_state = %q, want %q -- the promotion path is not passing the finding to the gate that refuses it",
				tgt.VerifyState, libvirtsync.VerifyStateFailed)
		}
		if tgt.VerifyFailedAt != promoteTestNowUnix-3600 {
			t.Errorf("verify_failed_at = %d, want %d -- an operator cannot tell a finding from this morning from one from March without it",
				tgt.VerifyFailedAt, promoteTestNowUnix-3600)
		}
	})

	t.Run("it is refused without the override", func(t *testing.T) {
		st := promotableState()
		st.VerifyState = libvirtsync.VerifyStateFailed
		st.VerifyFailedAt = promoteTestNowUnix - 3600

		_, err := failover.AssessPromote(promoteTargetState(st, true, false), opts(false))
		if err == nil {
			t.Fatal("a replica carrying a recorded verification failure was promoted without the override")
		}
		// The message has to say the replica is WRONG rather than merely
		// behind, or the operator reads it as another way of saying the
		// sync is lagging and forces past it without thinking.
		if !strings.Contains(err.Error(), "differing from its source") {
			t.Errorf("refusal = %q, want it to say the contents differ from the source", err)
		}
	})

	t.Run("the override still gets through, and says what it overrode", func(t *testing.T) {
		st := promotableState()
		st.VerifyState = libvirtsync.VerifyStateFailed
		st.VerifyFailedAt = promoteTestNowUnix - 3600

		plan, err := failover.AssessPromote(promoteTargetState(st, true, false), opts(true))
		if err != nil {
			t.Fatalf("the override was refused: %v -- during a real outage an operator may knowingly choose a questionable copy over nothing", err)
		}
		if !plan.WriteMetadata {
			t.Error("a forced promotion produced no promotion record")
		}
		if plan.DataLoss.Known {
			t.Error("a forced promotion reported a known data-loss window; a replica that does not match its source cannot have one measured from its metadata")
		}
		if len(plan.Notes) == 0 {
			t.Fatal("a forced promotion past a verification failure said nothing about what it overrode")
		}
		if !strings.Contains(strings.Join(plan.Notes, "; "), "differing from its source") {
			t.Errorf("notes = %v, want the verification failure named among what was overridden", plan.Notes)
		}
	})

	t.Run("a finding with no date still refuses", func(t *testing.T) {
		// The two fields are written by one metadata call, but a record
		// from an older vmsync -- or one whose date will not parse -- has
		// only the verdict. Presence is the state, so this must refuse
		// exactly as above; the missing date changes only the wording.
		st := promotableState()
		st.VerifyState = libvirtsync.VerifyStateFailed

		_, err := failover.AssessPromote(promoteTargetState(st, true, false), opts(false))
		if err == nil {
			t.Fatal("a verification failure with no recorded date was waved through")
		}
		if !strings.Contains(err.Error(), "unrecorded time") {
			t.Errorf("refusal = %q, want it to admit the time is unknown rather than inventing one", err)
		}
	})
}

// Every other field the promotion decision reads must arrive too.
//
// The verify fields are the ones that were missing, but the failure was of
// the mapping as a whole -- an unchecked struct literal between two packages
// -- so the whole mapping is pinned rather than the field of the day.
func TestPromoteTargetStateMapsEveryField(t *testing.T) {
	st := libvirtsync.FailoverState{
		Exists:              true,
		Role:                libvirtsync.RoleTarget,
		Active:              true,
		LastCheckpoint:      "vmsync-cpt-000012",
		LastSyncUnix:        promoteTestNowUnix - 300,
		CheckpointAtUnix:    promoteTestNowUnix - 360,
		ReplicaSource:       "prod01:web01",
		FailureCount:        2,
		SourceStoppedAtSync: true,
		RestoredFrom:        "1756041600-vmsync-cpt-000042",
		VerifyState:         libvirtsync.VerifyStateFailed,
		VerifyFailedAt:      promoteTestNowUnix - 3600,
	}

	got := promoteTargetState(st, true, true)
	want := failover.TargetState{
		Role:                libvirtsync.RoleTarget,
		LastCheckpoint:      "vmsync-cpt-000012",
		LastSyncUnix:        promoteTestNowUnix - 300,
		CheckpointAtUnix:    promoteTestNowUnix - 360,
		ReplicaSource:       "prod01:web01",
		FailureCount:        2,
		DisksPresent:        true,
		OverlayPresent:      true,
		Active:              true,
		SourceStoppedAtSync: true,
		RestoredFrom:        "1756041600-vmsync-cpt-000042",
		VerifyState:         libvirtsync.VerifyStateFailed,
		VerifyFailedAt:      promoteTestNowUnix - 3600,
		// SyncInFlight stays false whatever was observed: the promotion
		// path holds this target's run lock, which a sync could not have
		// let it take.
		SyncInFlight: false,
	}
	if got != want {
		t.Errorf("promoteTargetState() =\n%+v\nwant\n%+v", got, want)
	}

	// The comparison above catches a field being taken OUT of the mapping.
	// It cannot catch one being put into failover.TargetState and never
	// added here, because an unmapped field is the zero value on both sides
	// and compares equal -- and that is not a hypothetical oversight, it is
	// precisely how the verify verdict came to be consumed by the promotion
	// gate and never carried to it.
	//
	// So the fixture above deliberately gives every field a non-zero value,
	// and this walk insists on finding one. A field added to
	// failover.TargetState without a line in promoteTargetState arrives here
	// as its zero value and fails, naming itself.
	//
	// SyncInFlight is the one exemption, and it is exempt because false IS
	// its mapped value rather than an omission: the promotion path holds
	// this target's run lock, so no sync can be writing it.
	v := reflect.ValueOf(got)
	for i := 0; i < v.NumField(); i++ {
		name := v.Type().Field(i).Name
		if name == "SyncInFlight" {
			continue
		}
		if v.Field(i).IsZero() {
			t.Errorf("failover.TargetState.%s came back zero: promoteTargetState does not map it, so the promotion gate never sees it", name)
		}
	}
}
