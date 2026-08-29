package restorepoint

import (
	"strings"
	"testing"
)

const testReplicaDir = "/data/replicas"

// --- staging ------------------------------------------------------------------

func TestRestoreTempPathIsNotMistakableForAnOverlay(t *testing.T) {
	// pkg/failover globs "<disk>_*" to find uncommitted incremental overlays
	// and blocks promotion on a match. A staged restore that matched would
	// make the replica unpromotable for as long as it existed -- during the
	// incident the restore was for.
	got := RestoreTempPath("/data/replicas/vm.qcow2", 1756041600)
	if strings.HasPrefix(got, "/data/replicas/vm.qcow2_") {
		t.Fatalf("staging path %q looks like an incremental overlay (<disk>_*)", got)
	}
	if !strings.HasPrefix(got, "/data/replicas/vm.qcow2") {
		t.Fatalf("staging path %q is not a sibling of the replica", got)
	}
	// A sibling, never inside the restore point root: ParseListing would
	// classify it as Unknown and prune never removes an Unknown entry, so an
	// interrupted restore would be warned about on every sync forever.
	if strings.Contains(got, DirName) {
		t.Errorf("staging path %q is inside %s", got, DirName)
	}
}

func TestRestoreStageCommand(t *testing.T) {
	tag := testTag(t)
	temp := RestoreTempPath(testReplicaDir+"/vm.qcow2", 99)
	cmd, err := RestoreStageCommand(testRoot, tag, "vm.qcow2", temp)
	if err != nil {
		t.Fatalf("RestoreStageCommand: %v", err)
	}
	if !strings.Contains(cmd, "cp --reflink=always") {
		t.Errorf("stage must reflink, got %s", cmd)
	}
	if strings.Contains(cmd, "reflink=auto") {
		t.Errorf("stage must not fall back to a full byte copy: %s", cmd)
	}
	if !strings.Contains(cmd, shQuote(Dir(testRoot, tag)+"/vm.qcow2")) {
		t.Errorf("stage does not read from the restore point: %s", cmd)
	}
	if !strings.Contains(cmd, shQuote(temp)) {
		t.Errorf("stage does not write to the planned temp path: %s", cmd)
	}
	// cp -n would be the obvious way to say "do not clobber" and is the wrong
	// one: GNU cp -n skips silently and exits 0, so a collision would look
	// like a successful stage.
	if strings.Contains(cmd, "cp -n") || strings.Contains(cmd, " -n ") {
		t.Errorf("stage must not use cp -n, which skips silently: %s", cmd)
	}
	if !strings.Contains(cmd, "exit 1") {
		t.Errorf("stage must fail rather than overwrite an existing destination: %s", cmd)
	}
}

func TestRestoreStageCommandRefusesAPathForADiskName(t *testing.T) {
	tag := testTag(t)
	for _, bad := range []string{"", "../../etc/passwd", "sub/dir.qcow2", "/abs.qcow2", ".", "..", "a/"} {
		if _, err := RestoreStageCommand(testRoot, tag, bad, "/tmp/x"); err == nil {
			t.Errorf("RestoreStageCommand accepted %q as a disk name", bad)
		}
	}
}

func TestRestoreStageCommandQuotesHostilePaths(t *testing.T) {
	tag := testTag(t)
	temp := "/data/it's here/vm.qcow2.vmsync-restoring-1"
	cmd, err := RestoreStageCommand(testRoot, tag, "vm.qcow2", temp)
	if err != nil {
		t.Fatalf("RestoreStageCommand: %v", err)
	}
	if !strings.Contains(cmd, `'/data/it'\''s here/vm.qcow2.vmsync-restoring-1'`) {
		t.Errorf("temp path is not shell-quoted: %s", cmd)
	}
}

// --- the swap -----------------------------------------------------------------

func TestRestoreAsideIsAnIndependentCopyNotALink(t *testing.T) {
	cmd := RestoreAsideCommand("/data/vm.qcow2", "/data/vm.qcow2.vmsync-replaced-1")
	// A hard link would be equally free and equally instant, and wrong: an
	// incremental sync qemu-img commits its overlay back into the base file IN
	// PLACE, so a hard-linked aside would silently follow the replica forward
	// and stop being the pre-restore snapshot it was taken to be.
	if strings.Contains(cmd, "ln ") {
		t.Errorf("aside must not be a hard link: %s", cmd)
	}
	if !strings.Contains(cmd, "cp --reflink=always") {
		t.Errorf("aside must be a reflink copy: %s", cmd)
	}
	if !strings.Contains(cmd, "exit 1") {
		t.Errorf("aside must refuse an existing destination rather than clobber it: %s", cmd)
	}
}

func TestRestorePromoteIsASingleAtomicRename(t *testing.T) {
	cmd := RestorePromoteCommand("/data/vm.qcow2.vmsync-restoring-1", "/data/vm.qcow2")
	if !strings.HasPrefix(cmd, "mv -- ") {
		t.Fatalf("promote must be a bare rename, got %s", cmd)
	}
	// mv -n would SKIP silently when the destination exists -- and the
	// destination always exists here, so -n would turn every promote into a
	// no-op that exits 0.
	if strings.Contains(cmd, "-n") {
		t.Errorf("promote must replace the destination, not skip it: %s", cmd)
	}
	if strings.Contains(cmd, "&&") || strings.Contains(cmd, ";") {
		t.Errorf("promote must be one rename so the replica path is never absent: %s", cmd)
	}
}

func TestRestoreUndoDoesNotConsumeTheAside(t *testing.T) {
	cmd := RestoreUndoCommand("/data/vm.qcow2.vmsync-replaced-1", "/data/vm.qcow2")
	// The aside is what the operator keeps under -replaced-disk-action=rename.
	// Undoing a partial restore must not eat it.
	if strings.Contains(cmd, "mv ") {
		t.Errorf("undo must copy, not move, or the kept contents are consumed: %s", cmd)
	}
	if !strings.Contains(cmd, "cp -f") {
		t.Errorf("undo must overwrite the replica it is putting back: %s", cmd)
	}
}

func TestRestoreDiscardTolerctesMissingPaths(t *testing.T) {
	cmd := RestoreDiscardCommand("/a", "/b")
	if !strings.HasPrefix(cmd, "rm -f") {
		t.Fatalf("discard runs on cleanup paths and must tolerate a path already gone: %s", cmd)
	}
	for _, p := range []string{"'/a'", "'/b'"} {
		if !strings.Contains(cmd, p) {
			t.Errorf("discard missed %s: %s", p, cmd)
		}
	}
}

// --- disk set presence ---------------------------------------------------------

func TestParseReplicaPresent(t *testing.T) {
	disks := []string{"a.qcow2", "b.qcow2", "c.qcow2"}
	out := markerDiskHave + "a.qcow2\n" +
		markerDiskMiss + "b.qcow2\n" +
		markerDiskHave + "c.qcow2\n"
	missing, err := ParseReplicaPresent(out, disks)
	if err != nil {
		t.Fatalf("ParseReplicaPresent: %v", err)
	}
	if len(missing) != 1 || missing[0] != "b.qcow2" {
		t.Fatalf("missing = %v, want [b.qcow2]", missing)
	}
}

func TestParseReplicaPresentRefusesAnUnansweredName(t *testing.T) {
	// A name the output mentions neither way means the command did not run as
	// written. Reading that as "present" would let a restore proceed against a
	// disk set it never actually checked.
	if _, err := ParseReplicaPresent(markerDiskHave+"a.qcow2\n", []string{"a.qcow2", "b.qcow2"}); err == nil {
		t.Fatal("an unanswered disk name was accepted")
	}
	if _, err := ParseReplicaPresent("", []string{"a.qcow2"}); err == nil {
		t.Fatal("empty output was accepted")
	}
}

func TestParseReplicaPresentDoesNotMatchOnPrefixes(t *testing.T) {
	// "disk.qcow2" is a prefix of "disk.qcow2.old". A substring match would
	// read one disk's answer as another's.
	disks := []string{"disk.qcow2", "disk.qcow2.old"}
	out := markerDiskMiss + "disk.qcow2\n" + markerDiskHave + "disk.qcow2.old\n"
	missing, err := ParseReplicaPresent(out, disks)
	if err != nil {
		t.Fatalf("ParseReplicaPresent: %v", err)
	}
	if len(missing) != 1 || missing[0] != "disk.qcow2" {
		t.Fatalf("missing = %v, want [disk.qcow2]", missing)
	}
}

func TestReplicaPresentCommandAlwaysExitsZero(t *testing.T) {
	// The asking-vs-doing convention: a non-nil error from the runner must
	// mean exclusively "the question could not be put", never "no".
	cmd := ReplicaPresentCommand(testReplicaDir, []string{"a.qcow2"})
	if !strings.HasSuffix(cmd, "exit 0") {
		t.Errorf("presence check must exit 0 whatever it found: %s", cmd)
	}
	if !strings.Contains(cmd, shQuote(testReplicaDir+"/a.qcow2")) {
		t.Errorf("presence check does not test the replica path: %s", cmd)
	}
}

func TestReplicaPresentCommandQuotesTheMarkerEcho(t *testing.T) {
	cmd := ReplicaPresentCommand(testReplicaDir, []string{"it's.qcow2"})
	if !strings.Contains(cmd, `'`+markerDiskHave+`it'\''s.qcow2'`) {
		t.Errorf("marker echo is not shell-quoted, a disk name with a quote would break the script: %s", cmd)
	}
}

// --- the metadata plan ---------------------------------------------------------
//
// The single most consequential decision in a restore, so it is asserted field
// by field rather than as a whole map: each one has its own reason to be there
// and its own way to be wrong.

func fullStatus() Status {
	return Status{
		Checkpoint:   "vmsync-cpt-000042",
		CheckpointAt: 1756041000,
		TakenAt:      1756041600,
		Source:       "srchost:vm1",
		Verify:       VerifyPassed,
		Disks:        []string{"vm.qcow2"},
	}
}

func TestMetadataPlanDescribesTheRestoredPoint(t *testing.T) {
	updates, removals := MetadataPlan(fullStatus(), testProvenance())

	for field, want := range map[string]string{
		FieldLastCheckpoint:  "vmsync-cpt-000042",
		FieldCheckpointAt:    "1756041000",
		FieldLastSync:        "1756041600",
		FieldFailureCount:    "0",
		FieldReplicationRole: RolePausedValue,
	} {
		if got := updates[field]; got != want {
			t.Errorf("updates[%s] = %q, want %q", field, got, want)
		}
	}
	if !contains(removals, FieldSourceStoppedAtSync) {
		t.Error("source_stopped_at_sync must be removed: it short-circuits the data-loss calculation to a verified zero, and the sidecar records no value to restore accurately")
	}
}

func TestMetadataPlanKeepsTheReplicaPromotable(t *testing.T) {
	// pkg/failover's evidence check blocks promotion on an empty
	// last_checkpoint ("no sync has ever completed"), an empty
	// last_sync_timestamp, and a non-zero failure_count. Promoting the
	// restored replica is the entire reason anyone restores, so a plan that
	// cleared these would make the feature defeat itself.
	updates, removals := MetadataPlan(fullStatus(), testProvenance())
	for _, f := range []string{FieldLastCheckpoint, FieldLastSync} {
		if updates[f] == "" {
			t.Errorf("%s is not set; an empty one blocks promotion", f)
		}
		if contains(removals, f) {
			t.Errorf("%s is removed; an empty one blocks promotion", f)
		}
	}
	if updates[FieldFailureCount] != "0" {
		t.Error("failure_count must be zeroed; a non-zero one blocks promotion on evidence about contents that were just discarded")
	}
}

func TestMetadataPlanPausesReplication(t *testing.T) {
	// The field that makes the rest safe. Without it the next scheduled sync
	// overwrites the restored data with exactly what the operator rolled away
	// from -- and under -reinit-after-failures it does so unattended.
	updates, removals := MetadataPlan(fullStatus(), testProvenance())
	if updates[FieldReplicationRole] != RolePausedValue {
		t.Fatalf("replication_role = %q, want %q", updates[FieldReplicationRole], RolePausedValue)
	}
	if contains(removals, FieldReplicationRole) {
		t.Fatal("replication_role must be set, not removed")
	}
}

func TestMetadataPlanNeverTouchesFieldsThatDoNotDescribeDiskContent(t *testing.T) {
	// replica_source identifies whose replica this is and an empty one blocks
	// promotion; replica_targets and last_replicated_* describe a life as a
	// SOURCE; the promotion and fence records are an audit trail of a failover
	// that rolling a disk back does not undo.
	updates, removals := MetadataPlan(fullStatus(), testProvenance())
	for _, f := range []string{
		"replica_source", "replica_targets", "last_replicated_at", "last_replicated_to",
		"promoted_at", "promoted_by", "promoted_from", "promotion_mode",
		"fence_id", "fence_source", "fence_armed_at", "fence_armed_by",
	} {
		if _, ok := updates[f]; ok {
			t.Errorf("plan writes %s, which says nothing about disk content", f)
		}
		if contains(removals, f) {
			t.Errorf("plan removes %s, which says nothing about disk content", f)
		}
	}
}

func TestMetadataPlanRemovesWhatAnOldSidecarCannotSupply(t *testing.T) {
	// A sidecar written before checkpoint_at existed, or by a run whose
	// checkpoint chain could not advance. Guessing a value would be worse than
	// having none: checkpoint_at is what a promotion measures data loss from.
	updates, removals := MetadataPlan(Status{Disks: []string{"vm.qcow2"}}, testProvenance())
	for _, f := range []string{FieldLastCheckpoint, FieldLastSync, FieldCheckpointAt} {
		if _, ok := updates[f]; ok {
			t.Errorf("%s was invented from an empty sidecar: %q", f, updates[f])
		}
		if !contains(removals, f) {
			t.Errorf("%s must be removed when the sidecar cannot supply it", f)
		}
	}
	// Still pauses, still zeroes: neither depends on the sidecar.
	if updates[FieldReplicationRole] != RolePausedValue || updates[FieldFailureCount] != "0" {
		t.Error("an old sidecar must not stop the restore pausing replication")
	}
}

func TestMetadataPlanNeverSetsAndRemovesTheSameField(t *testing.T) {
	// SetDomainMetadataFields merges updates and removals; a field in both is
	// a plan that does not know what it wants.
	for name, s := range map[string]Status{
		"full":             fullStatus(),
		"empty":            {},
		"no checkpoint_at": {Checkpoint: "c", TakenAt: 1, Disks: []string{"d"}},
		"no checkpoint":    {CheckpointAt: 1, TakenAt: 1, Disks: []string{"d"}},
	} {
		updates, removals := MetadataPlan(s, testProvenance())
		for _, r := range removals {
			if _, ok := updates[r]; ok {
				t.Errorf("%s: %s is both set and removed", name, r)
			}
		}
	}
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

func testProvenance() Provenance {
	return Provenance{Tag: "1756041600-vmsync-cpt-000042", AtUnix: 1756900000, By: "alice"}
}

// The restore record is what makes a rolled-back replica recognisable AFTER it
// has been promoted -- at which point role=paused has been overwritten and
// every other signal is ambiguous with an ordinary lagging replica.
func TestMetadataPlanRecordsItsOwnProvenance(t *testing.T) {
	updates, _ := MetadataPlan(fullStatus(), testProvenance())

	for field, want := range map[string]string{
		FieldRestoredFrom: "1756041600-vmsync-cpt-000042",
		FieldRestoredAt:   "1756900000",
		FieldRestoredBy:   "alice",
	} {
		if got := updates[field]; got != want {
			t.Errorf("updates[%s] = %q, want %q", field, got, want)
		}
	}

	// restored_at is when the ROLLBACK happened, not when the copy was taken.
	// The gap between the two is the thing an incident timeline is trying to
	// establish, so conflating them would defeat the point of recording it.
	if updates[FieldRestoredAt] == updates[FieldCheckpointAt] || updates[FieldRestoredAt] == updates[FieldLastSync] {
		t.Error("restored_at duplicates a field describing the copy; it must record when the rollback was performed")
	}
}

func TestMetadataPlanClearsAStaleAttribution(t *testing.T) {
	// A domain restored twice, the second time by an unattributed run: the
	// first restore's actor must not be left standing as though they asked
	// for the second.
	p := testProvenance()
	p.By = ""
	updates, removals := MetadataPlan(fullStatus(), p)

	if _, ok := updates[FieldRestoredBy]; ok {
		t.Error("an unattributed restore invented a restored_by")
	}
	if !contains(removals, FieldRestoredBy) {
		t.Error("restored_by must be REMOVED when nobody is named, or a previous restore's actor survives")
	}
	// The other two still record what happened.
	if updates[FieldRestoredFrom] == "" || updates[FieldRestoredAt] == "" {
		t.Error("an unattributed restore must still record which point and when")
	}
}

func TestMetadataPlanNeverSetsAndRemovesProvenance(t *testing.T) {
	updates, removals := MetadataPlan(fullStatus(), testProvenance())
	for _, r := range removals {
		if _, ok := updates[r]; ok {
			t.Errorf("%s is both set and removed", r)
		}
	}
}
