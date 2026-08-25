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

package restorepoint

import (
	"fmt"
	"path"
	"strconv"
	"strings"
)

// Phase 2: putting a restore point back over the replica it came from.
//
// Everything here is a string builder, like the rest of remote.go, so the
// commands that run against a production replica are ordinary values a test can
// assert on.
//
// The shape of a restore is three steps per disk, and the order is the whole
// safety argument:
//
//  1. STAGE   -- reflink the restore point's copy to a sibling of the replica.
//     Costs nothing, commits nothing, and can be thrown away.
//  2. ASIDE   -- reflink the CURRENT replica to a sibling of its own, so the
//     contents about to be displaced still exist under a name.
//  3. PROMOTE -- rename the staged copy onto the replica's path.
//
// Step 3 is a single rename within one directory, so the replica path never
// stops existing and never holds a half-written file: at every instant it is
// either entirely the old contents or entirely the new ones. That is what makes
// a multi-disk restore recoverable -- a failure between disk 2 and disk 3
// leaves three intact files, two new and one old, and the asides from step 2
// are what put the first two back.
//
// See docs/design/restore-points.md.

// RestoreTempSuffix precedes the stamp on a staged restore copy.
//
// Deliberately NOT of the form <disk>_<something>: pkg/failover treats any
// sibling matching "<disk>_*" as an uncommitted incremental overlay and blocks
// promotion on it (cmd/vmsync/failover.go). A staged restore that looked like
// an overlay would make the replica unpromotable for as long as it existed --
// during exactly the incident the restore was for.
const RestoreTempSuffix = ".vmsync-restoring-"

// RestoreTempPath is where a restore stages one disk before swapping it in.
//
// A sibling of the replica, never inside DirName: ParseListing classifies
// anything it does not recognise under the restore point root as Unknown, and
// prune never deletes an Unknown entry -- so a temp file left there by an
// interrupted restore would be warned about on every sync, forever.
func RestoreTempPath(replicaPath string, stamp int64) string {
	return replicaPath + RestoreTempSuffix + strconv.FormatInt(stamp, 10)
}

// RestoreStageCommand reflinks one of a restore point's disks to tempPath,
// which the caller has already chosen with RestoreTempPath.
//
// The destination is passed in rather than derived so that the plan an operator
// reads in the assessment names the exact paths the restore then writes -- a
// second derivation here could disagree with the first, and the file it named
// would be one nobody was shown.
//
// --reflink=always, not =auto: the destination is the replica's own directory,
// which is the same filesystem the restore point lives on, so sharing extents
// is always possible here. =auto would silently fall back to a full byte copy
// of the whole image and take as long as a restore from tape.
//
// It refuses rather than overwrites an existing destination. cp -n would be the
// obvious way to say that and is the wrong one: GNU cp -n SKIPS silently and
// exits 0, so a collision would look like a successful stage and the promote
// would then rename a stranger's file onto the replica.
func RestoreStageCommand(root string, t Tag, disk, tempPath string) (string, error) {
	// "." and ".." survive a path.Base round trip unchanged, so the obvious
	// check alone would accept them and build a cp whose source is a directory.
	if disk == "" || disk == "." || disk == ".." || disk != path.Base(disk) {
		return "", fmt.Errorf("restore: %q is not a bare disk name", disk)
	}
	src := path.Join(Dir(root, t), disk)
	return fmt.Sprintf("if [ -e %s ]; then echo 'restore staging path already exists' >&2; exit 1; fi; cp --reflink=always -- %s %s",
		shQuote(tempPath), shQuote(src), shQuote(tempPath)), nil
}

// RestoreAsideCommand preserves the replica's CURRENT contents under aside,
// before anything displaces them.
//
// A reflink copy rather than a hard link, though a link would also be free and
// also instant. An incremental sync writes into a sibling overlay and then
// qemu-img commits it back into the base file IN PLACE, so a hard-linked aside
// would silently follow the replica forward and stop being the snapshot of
// pre-restore state it was taken to be. A reflink copy is an independent file
// that shares extents until one of them is written to, which is the property
// actually wanted.
func RestoreAsideCommand(replicaPath, asidePath string) string {
	return fmt.Sprintf("if [ -e %s ]; then echo 'restore aside path already exists' >&2; exit 1; fi; cp --reflink=always -- %s %s",
		shQuote(asidePath), shQuote(replicaPath), shQuote(asidePath))
}

// RestorePromoteCommand renames the staged copy onto the replica's path.
//
// rename(2) over an existing destination, which is atomic: a reader either sees
// the whole old file or the whole new one, and the path is never absent. The
// displaced inode is already preserved by RestoreAsideCommand, so nothing is
// lost by letting the rename replace it.
func RestorePromoteCommand(tempPath, replicaPath string) string {
	return "mv -- " + shQuote(tempPath) + " " + shQuote(replicaPath)
}

// RestoreUndoCommand puts a displaced replica back, for a multi-disk restore
// that failed part-way.
//
// A copy, not a rename: the aside is what the operator keeps afterwards under
// -replaced-disk-action=rename, and undoing a partial restore must not consume
// it. cp -f because the destination exists by definition -- this only runs on a
// path a promote already replaced.
func RestoreUndoCommand(asidePath, replicaPath string) string {
	return "cp -f --reflink=auto -- " + shQuote(asidePath) + " " + shQuote(replicaPath)
}

// RestoreDiscardCommand removes staged copies or asides that will not be used.
//
// rm -f, so discarding something already gone is not an error: this runs on
// cleanup paths where the caller cannot know how far a previous step got.
func RestoreDiscardCommand(paths ...string) string {
	cmd := "rm -f --"
	for _, p := range paths {
		cmd += " " + shQuote(p)
	}
	return cmd
}

// ReplicaPresentCommand asks whether every disk a restore point holds still
// exists at the replica's path.
//
// Answers with a marker per name rather than exiting non-zero, per this file's
// asking-vs-doing convention: a wedged connection must never be readable as
// "the disk is missing", which would otherwise turn a network hiccup into a
// refusal that names the wrong cause.
func ReplicaPresentCommand(replicaDir string, disks []string) string {
	cmd := ""
	for _, d := range disks {
		p := path.Join(replicaDir, d)
		cmd += fmt.Sprintf("if [ -f %s ]; then echo %s; else echo %s; fi; ",
			shQuote(p), shQuote(markerDiskHave+d), shQuote(markerDiskMiss+d))
	}
	return cmd + "exit 0"
}

// The vmsync metadata field names a restore rewrites, duplicated from
// pkg/libvirtsync rather than imported.
//
// Same reason pkg/failover duplicates its own fourteen: libvirtsync needs cgo
// libvirt, and importing it would make this package -- and MetadataPlan below,
// which is the single most consequential decision in a restore -- buildable and
// testable only on a machine with libvirt development headers. The names are
// pinned against libvirtsync's own constants by
// pkg/libvirtsync/duplicated_names_test.go, which lives on that side precisely
// so the check costs no build-time dependency here.
const (
	FieldLastCheckpoint      = "last_checkpoint"
	FieldLastSync            = "last_sync_timestamp"
	FieldFailureCount        = "failure_count"
	FieldCheckpointAt        = "checkpoint_at"
	FieldSourceStoppedAtSync = "source_stopped_at_sync"
	FieldReplicationRole     = "replication_role"
)

// RolePausedValue is libvirtsync.RolePaused, duplicated for the same reason.
const RolePausedValue = "paused"

// MetadataPlan is what a restore writes onto the replica's domain metadata,
// derived from the restore point's own sidecar.
//
// This is the crux of the whole feature, and the reasoning is worth stating in
// full because every field here is a choice with a way to be wrong.
//
// The disks are about to say they are from an earlier instant. Metadata that
// still describes the newer one is not merely stale, it is actively dangerous:
// the next incremental sync compares the target's last_checkpoint against the
// parent it computed from the SOURCE's checkpoint chain, and nothing in that
// comparison looks at disk content. Leave last_checkpoint forward-dated and
// vmsync applies the source's N->N+1 delta onto data that is at M, exits 0, and
// reports green.
//
// So each field is set to describe the RESTORED state:
//
//   - last_checkpoint <- the sidecar's checkpoint. Not cleared: an empty one
//     blocks promotion outright (pkg/failover's evidence check reads it as "no
//     sync has ever completed"), and promoting the restored replica is the
//     entire reason anyone restores. Setting it to the older name is both
//     honest and self-enforcing -- the source deletes each parent checkpoint
//     after the sync that supersedes it, so the named checkpoint no longer
//     exists up there and the next incremental fails its chain-consistency
//     check instead of running. That refusal is the intended end state, not an
//     accident: see the role below.
//
//   - last_sync_timestamp <- when the restore point was taken. Same argument:
//     an empty one is a second promotion blocker, and the true answer to "when
//     was this replica's content last written by a sync" is that instant.
//
//   - checkpoint_at <- the sidecar's checkpoint_at. This is what a later
//     -promote measures data loss FROM, so it is what makes the promotion
//     report say "you are giving up eleven hours" instead of "four minutes".
//     Removed rather than guessed when the sidecar predates the field.
//
//   - source_stopped_at_sync REMOVED, always. It short-circuits the data-loss
//     calculation to a verified zero before any clock is read, and the sidecar
//     does not record whether the source was stopped for the point being
//     restored -- so it cannot be set accurately, and leaving a stale one would
//     make a rolled-back replica report a verified zero-loss promotion.
//
//   - failure_count <- 0. The failures it counted were against the contents
//     just discarded. Leaving it would block promotion on evidence that no
//     longer describes this replica.
//
//   - replication_role <- paused. The one field here that is not a fact about
//     the disks, and the one that makes the rest safe. A restore is done to
//     promote, not to resume replication -- the next sync from the same source
//     would overwrite the restored data with exactly what the operator rolled
//     away from, and under -reinit-after-failures it would do so unattended.
//     Paused is refused by the sync's very first guard with a message naming
//     the cure, so replication stops deliberately rather than silently, and
//     -update-role=target is the operator's explicit decision to resume.
//
// Never touched: replica_source (identifies whose replica this is, and an empty
// one blocks promotion), replica_targets, last_replicated_at/to (they describe
// a life as a SOURCE), and the promotion and fence records (an audit trail of a
// failover that rolling a disk back does not undo).
func MetadataPlan(s Status) (updates map[string]string, removals []string) {
	updates = map[string]string{
		FieldFailureCount:    "0",
		FieldReplicationRole: RolePausedValue,
	}
	// Always: the sidecar cannot attest it, and a stale one is a verified
	// zero-data-loss promotion of rolled-back data.
	removals = []string{FieldSourceStoppedAtSync}

	if s.Checkpoint != "" {
		updates[FieldLastCheckpoint] = s.Checkpoint
	} else {
		removals = append(removals, FieldLastCheckpoint)
	}
	if s.TakenAt > 0 {
		updates[FieldLastSync] = strconv.FormatInt(s.TakenAt, 10)
	} else {
		removals = append(removals, FieldLastSync)
	}
	if s.CheckpointAt > 0 {
		updates[FieldCheckpointAt] = strconv.FormatInt(s.CheckpointAt, 10)
	} else {
		removals = append(removals, FieldCheckpointAt)
	}
	return updates, removals
}

// containsLine reports whether out has want as a whole line, ignoring
// surrounding whitespace.
//
// Whole-line, not strings.Contains: one marker plus a disk name can be a
// prefix of another ("disk.qcow2" inside "disk.qcow2.old"), and a substring
// match would then read the answer for one disk as the answer for another.
func containsLine(out, want string) bool {
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}

const (
	markerDiskHave = "__VMSYNC_RP_HAVE__"
	markerDiskMiss = "__VMSYNC_RP_MISS__"
)

// ParseReplicaPresent reads ReplicaPresentCommand's answer and returns the
// names that are NOT present.
//
// A name the output mentions neither way is reported as an error rather than
// assumed present: the command emits exactly one line per name, so a missing
// answer means it did not run as written.
func ParseReplicaPresent(out string, disks []string) ([]string, error) {
	var missing []string
	for _, d := range disks {
		switch {
		case containsLine(out, markerDiskHave+d):
			// present
		case containsLine(out, markerDiskMiss+d):
			missing = append(missing, d)
		default:
			return nil, fmt.Errorf("could not tell whether the replica still has %q; the check answered neither way", d)
		}
	}
	return missing, nil
}
