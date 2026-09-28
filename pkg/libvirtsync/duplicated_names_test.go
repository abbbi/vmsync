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

package libvirtsync

import (
	"slices"
	"testing"

	"vmsync/pkg/failover"
	"vmsync/pkg/restorepoint"
)

// Two packages duplicate this package's metadata field names rather than
// importing them, because importing this one drags in cgo libvirt and would
// make them buildable only where libvirt's development headers are. Both say so
// in their own comments. The duplication is only safe if it is actually kept in
// step, and nothing but this file checks that.
//
// It lives HERE, in a test file, rather than in either of them: a production
// import in the other direction would recreate exactly the cgo dependency the
// duplication exists to avoid, while a test file's imports cost nothing at
// build time. The cycle risk is nil -- neither package imports this one.
//
// A mismatch here is not a cosmetic drift. pkg/failover's names are what a
// promotion reads to decide whether a replica has evidence of a completed sync;
// pkg/restorepoint's are what a restore writes to stop the next sync applying
// an incremental delta onto rolled-back data. A silent divergence in either
// direction means the write lands on a field nothing reads.

func TestFailoverFieldNamesMatch(t *testing.T) {
	for _, c := range []struct{ mine, theirs, what string }{
		{MetadataFieldReplicationRole, failover.FieldReplicationRole, "replication_role"},
		{MetadataFieldReplicaSource, failover.FieldReplicaSource, "replica_source"},
		{MetadataFieldReplicaTargets, failover.FieldReplicaTargets, "replica_targets"},
		{MetadataFieldLastCheckpoint, failover.FieldLastCheckpoint, "last_checkpoint"},
		{MetadataFieldLastSync, failover.FieldLastSync, "last_sync_timestamp"},
		{MetadataFieldReplicaWrittenAt, failover.FieldReplicaWrittenAt, "replica_written_at"},
		{MetadataFieldPendingCheckpoint, failover.FieldPendingCheckpoint, "pending_checkpoint"},
		{MetadataFieldReplicaIncomplete, failover.FieldReplicaIncomplete, "replica_incomplete"},
		{MetadataFieldVerifyState, failover.FieldVerifyState, "verify_state"},
		{MetadataFieldVerifyFailedAt, failover.FieldVerifyFailedAt, "verify_failed_at"},
		{MetadataFieldFailureCount, failover.FieldFailureCount, "failure_count"},
		{MetadataFieldPromotedAt, failover.FieldPromotedAt, "promoted_at"},
		{MetadataFieldPromotedBy, failover.FieldPromotedBy, "promoted_by"},
		{MetadataFieldPromotedFrom, failover.FieldPromotedFrom, "promoted_from"},
		{MetadataFieldPromotionMode, failover.FieldPromotionMode, "promotion_mode"},
		{MetadataFieldLastPromotedAt, failover.FieldLastPromotedAt, "last_promoted_at"},
		{MetadataFieldFenceID, failover.FieldFenceID, "fence_id"},
		{MetadataFieldFenceSource, failover.FieldFenceSource, "fence_source"},
		{MetadataFieldFenceArmedAt, failover.FieldFenceArmedAt, "fence_armed_at"},
		{MetadataFieldFenceArmedBy, failover.FieldFenceArmedBy, "fence_armed_by"},
	} {
		if c.mine != c.theirs {
			t.Errorf("%s: libvirtsync says %q, pkg/failover says %q", c.what, c.mine, c.theirs)
		}
	}
}

func TestFailoverRoleNamesMatch(t *testing.T) {
	for _, c := range []struct{ mine, theirs, what string }{
		{RoleSource, failover.RoleSource, "source"},
		{RoleTarget, failover.RoleTarget, "target"},
		{RolePromoted, failover.RolePromoted, "promoted"},
		{RolePaused, failover.RolePaused, "paused"},
		{RoleFenced, failover.RoleFenced, "fenced"},
	} {
		if c.mine != c.theirs {
			t.Errorf("role %s: libvirtsync says %q, pkg/failover says %q", c.what, c.mine, c.theirs)
		}
	}
}

func TestRestorePointFieldNamesMatch(t *testing.T) {
	for _, c := range []struct{ mine, theirs, what string }{
		{MetadataFieldLastCheckpoint, restorepoint.FieldLastCheckpoint, "last_checkpoint"},
		{MetadataFieldLastSync, restorepoint.FieldLastSync, "last_sync_timestamp"},
		{MetadataFieldReplicaWrittenAt, restorepoint.FieldReplicaWrittenAt, "replica_written_at"},
		{MetadataFieldPendingCheckpoint, restorepoint.FieldPendingCheckpoint, "pending_checkpoint"},
		{MetadataFieldReplicaIncomplete, restorepoint.FieldReplicaIncomplete, "replica_incomplete"},
		{MetadataFieldVerifyState, restorepoint.FieldVerifyState, "verify_state"},
		{MetadataFieldVerifyFailedAt, restorepoint.FieldVerifyFailedAt, "verify_failed_at"},
		{MetadataFieldFailureCount, restorepoint.FieldFailureCount, "failure_count"},
		{MetadataFieldCheckpointAt, restorepoint.FieldCheckpointAt, "checkpoint_at"},
		{MetadataFieldSourceStoppedAtSync, restorepoint.FieldSourceStoppedAtSync, "source_stopped_at_sync"},
		{MetadataFieldReplicationRole, restorepoint.FieldReplicationRole, "replication_role"},
		{MetadataFieldRestoredFrom, restorepoint.FieldRestoredFrom, "restored_from"},
		{MetadataFieldRestoredAt, restorepoint.FieldRestoredAt, "restored_at"},
		{MetadataFieldRestoredBy, restorepoint.FieldRestoredBy, "restored_by"},
	} {
		if c.mine != c.theirs {
			t.Errorf("%s: libvirtsync says %q, pkg/restorepoint says %q", c.what, c.mine, c.theirs)
		}
	}
	if VerifyStateFailed != failover.VerifyStateFailedValue {
		t.Errorf("VerifyStateFailed = %q but failover.VerifyStateFailedValue = %q", VerifyStateFailed, failover.VerifyStateFailedValue)
	}
	if RolePaused != restorepoint.RolePausedValue {
		t.Errorf("paused role: libvirtsync says %q, pkg/restorepoint says %q", RolePaused, restorepoint.RolePausedValue)
	}
}

// The restore's whole safety argument is that the role it leaves behind is one
// the sync path refuses. If RolePaused ever became something TargetRoleAllowsSync
// permits, a restore would end with replication armed against the replica it
// just rolled back -- and the next scheduled run would overwrite it.
func TestARestoredReplicaIsRefusedBySync(t *testing.T) {
	updates, _ := restorepoint.MetadataPlan(restorepoint.Status{
		Checkpoint: "vmsync-cpt-000042", CheckpointAt: 1, TakenAt: 2, Disks: []string{"d.qcow2"},
	}, restorepoint.Provenance{Tag: "2-vmsync-cpt-000042", AtUnix: 3, By: "alice"})
	role := updates[MetadataFieldReplicationRole]
	if role == "" {
		t.Fatal("a restore leaves no replication_role at all, so the next sync would be allowed")
	}
	if err := TargetRoleAllowsSync(role); err == nil {
		t.Fatalf("a restore leaves replication_role=%q, which TargetRoleAllowsSync permits -- the next scheduled sync would overwrite the restored data", role)
	}
	// ...and one the restore path itself still permits, or a second restore
	// after a first would be refused on the state the first one created.
	if err := TargetRoleAllowsRestore(role); err != nil {
		t.Fatalf("a restore leaves replication_role=%q, which TargetRoleAllowsRestore refuses: %v", role, err)
	}
}

// The two gates differ on exactly one value, deliberately. Asserting the whole
// matrix keeps a later edit to either one from quietly closing or opening the
// other.
func TestRoleGatesDifferOnlyOnPaused(t *testing.T) {
	for _, role := range []string{"", RoleTarget, RoleSource, RolePromoted, RolePaused, "something-newer"} {
		syncOK := TargetRoleAllowsSync(role) == nil
		restoreOK := TargetRoleAllowsRestore(role) == nil
		want := syncOK
		if role == RolePaused {
			want = true
		}
		if restoreOK != want {
			t.Errorf("role %q: sync allowed=%v, restore allowed=%v, wanted restore allowed=%v", role, syncOK, restoreOK, want)
		}
	}
}

// replica_incomplete is a TARGET-only field, and "forgot one of the strip
// lists" is this codebase's documented failure mode for those -- see
// MetadataFieldReplicaWrittenAt, whose own comment has to name all four
// places. So all four are asserted in ONE test rather than each in its own,
// because the defect is never "this list is wrong", it is "this list was not
// updated with the others".
//
// The four, and what each one costs when it is the one that was forgotten:
//
//   - UpdateSyncMetadata: a completed copy never withdraws its own warning,
//     so every healthy replica in the estate becomes force-only.
//   - RecordReplicaTarget: a domain acting as a SOURCE keeps a record of a
//     copy into it from an earlier life as somebody's replica -- and
//     UpdateSyncMetadata then stamps that onto a real target, refusing the
//     promotion of a replica nothing ever interrupted.
//   - AssessInvert, both ends: the same inheritance, reached by reversing a
//     pair instead of by syncing.
//   - restorepoint.MetadataPlan: a domain restored twice keeps the first
//     restore's record for ever.
//
// It lives here, with the name-mirroring tests, because this is the one file
// that can see all four sides at once without any of them taking a build-time
// dependency on the others.
func TestReplicaIncompleteIsClearedEverywhereATargetOnlyFieldMustBe(t *testing.T) {
	const armed = "verb=reinit,at=1758441600,action=9f3c1a2b4d5e6f70,host=hv-a,aside=1758441600"

	t.Run("UpdateSyncMetadata clears it on success", func(t *testing.T) {
		// Asserted through the real round trip rather than by reading the
		// removal list, because what matters is that the field is GONE from
		// the document the target is about to be defined with -- and this is
		// the same single DomainDefineXML that records the success, so
		// acceptance and clearing cannot come apart.
		base := minimalDomainXML("testvm", "12345678-1234-1234-1234-123456789abc", "/var/lib/libvirt/images/x.qcow2")
		withField, err := SetMetadataFields(base, map[string]string{MetadataFieldReplicaIncomplete: armed})
		if err != nil {
			t.Fatalf("SetMetadataFields: %v", err)
		}
		out, err := UpdateSyncMetadata(withField, "vmsync-cpt-000005", "src-host", "srcvm", RoleTarget, 1700000000, false, "", AutostartIntentUnknown)
		if err != nil {
			t.Fatalf("UpdateSyncMetadata: %v", err)
		}
		if got, _ := ParseMetadata(out, MetadataFieldReplicaIncomplete); got != "" {
			t.Errorf("a successful sync left %s=%q behind: every replica it writes is then refused by -promote until somebody clears it by hand",
				MetadataFieldReplicaIncomplete, got)
		}
	})

	t.Run("RecordReplicaTarget strips it from a domain acting as a source", func(t *testing.T) {
		if !slices.Contains(recordReplicaTargetStrips, MetadataFieldReplicaIncomplete) {
			t.Errorf("RecordReplicaTarget does not strip %s, so a source that was once somebody's replica carries it -- and UpdateSyncMetadata inherits it onto a healthy target",
				MetadataFieldReplicaIncomplete)
		}
	})

	t.Run("AssessInvert strips it from both ends", func(t *testing.T) {
		plan, err := failover.AssessInvert(failover.PairState{
			OldSource: failover.DomainEnd{Host: "prod01", Domain: "web01", Role: failover.RoleSource,
				ReplicaTargets: []string{"dr01:web01"}},
			Promoted: failover.DomainEnd{Host: "dr01", Domain: "web01", Role: failover.RolePromoted,
				ReplicaSource: "prod01:web01"},
		})
		if err != nil {
			t.Fatalf("AssessInvert: %v", err)
		}
		for name, list := range map[string][]string{
			"NewTargetRemovals": plan.NewTargetRemovals,
			"NewSourceRemovals": plan.NewSourceRemovals,
		} {
			if !slices.Contains(list, failover.FieldReplicaIncomplete) {
				t.Errorf("AssessInvert's %s does not strip %s", name, failover.FieldReplicaIncomplete)
			}
		}
	})

	t.Run("restorepoint.MetadataPlan sets it and clears it", func(t *testing.T) {
		st := restorepoint.Status{
			Checkpoint: "vmsync-cpt-000042", CheckpointAt: 1, TakenAt: 2, Disks: []string{"d.qcow2"},
		}
		p := restorepoint.Provenance{Tag: "2-vmsync-cpt-000042", AtUnix: 3, By: "alice"}

		// Cleared when the act records none: a domain restored twice must not
		// keep the first restore's record.
		_, removals := restorepoint.MetadataPlan(st, p)
		if !slices.Contains(removals, restorepoint.FieldReplicaIncomplete) {
			t.Errorf("an unarmed restore plan does not remove %s", restorepoint.FieldReplicaIncomplete)
		}

		// Armed in the SAME write that invalidates the replication metadata,
		// so no disk is ever swapped with the refusal not yet in place.
		p.ReplicaIncomplete = armed
		updates, removals := restorepoint.MetadataPlan(st, p)
		if updates[restorepoint.FieldReplicaIncomplete] != armed {
			t.Errorf("an armed restore plan wrote %s=%q, want %q",
				restorepoint.FieldReplicaIncomplete, updates[restorepoint.FieldReplicaIncomplete], armed)
		}
		if slices.Contains(removals, restorepoint.FieldReplicaIncomplete) {
			t.Errorf("%s is both set and removed", restorepoint.FieldReplicaIncomplete)
		}
	})
}
