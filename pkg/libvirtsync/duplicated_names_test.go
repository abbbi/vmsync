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
		{MetadataFieldFailureCount, failover.FieldFailureCount, "failure_count"},
		{MetadataFieldPromotedAt, failover.FieldPromotedAt, "promoted_at"},
		{MetadataFieldPromotedBy, failover.FieldPromotedBy, "promoted_by"},
		{MetadataFieldPromotedFrom, failover.FieldPromotedFrom, "promoted_from"},
		{MetadataFieldPromotionMode, failover.FieldPromotionMode, "promotion_mode"},
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
		{MetadataFieldFailureCount, restorepoint.FieldFailureCount, "failure_count"},
		{MetadataFieldCheckpointAt, restorepoint.FieldCheckpointAt, "checkpoint_at"},
		{MetadataFieldSourceStoppedAtSync, restorepoint.FieldSourceStoppedAtSync, "source_stopped_at_sync"},
		{MetadataFieldReplicationRole, restorepoint.FieldReplicationRole, "replication_role"},
	} {
		if c.mine != c.theirs {
			t.Errorf("%s: libvirtsync says %q, pkg/restorepoint says %q", c.what, c.mine, c.theirs)
		}
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
	})
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
