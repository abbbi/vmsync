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

package inventory

import (
	"os"
	"path/filepath"
	"testing"

	"vmsync/pkg/restorepoint"
)

// writeStampedPoint materialises a point whose tag and sidecar DISAGREE, which
// is what a wrong clock on the host running vmsync produces: the directory
// name carries the checkpoint instant it stamped, the sidecar carries what the
// target said the time was when it published.
func writeStampedPoint(t *testing.T, dir, tag string, checkpointAt, takenAt int64) {
	t.Helper()
	p := filepath.Join(dir, tag)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", p, err)
	}
	b, err := restorepoint.Status{
		Checkpoint: "vmsync-cpt-000042", CheckpointAt: checkpointAt, TakenAt: takenAt,
		Source: "src01", Verify: restorepoint.VerifyNotRun,
	}.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := os.WriteFile(filepath.Join(p, restorepoint.StatusName), b, 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
}

// taken_at_unix must be the sidecar's taken_at, not the tag's instant.
//
// The field is named for, and documented as, when the copy was made. It was
// filled from the tag -- the CHECKPOINT instant -- which made it a duplicate
// of checkpoint_at_unix and left st.TakenAt unread two lines after it was
// decoded. That is not cosmetic: this list is sorted by it, and the agent
// derives vmsync_agent_restore_point_newest_timestamp_seconds from it, so a
// host whose clock ran ahead put its mislabelled copies at the top of the
// operator's rollback list and called them the newest.
func TestRestorePointsReportTheInstantTheTargetPublishedThem(t *testing.T) {
	dir := replicaDir(t)
	// Written while the clock ran 48h fast: the tag says two days hence, the
	// target's own record says it happened first.
	writeStampedPoint(t, dir, "1700172800-vmsync-cpt-000001", 1700172800, 1699996400)
	// Written after the correction, and genuinely the newest copy.
	writeStampedPoint(t, dir, "1700000000-vmsync-cpt-000002", 1700000000, 1700000100)

	got := readRestorePointDir(dir)
	if len(got) != 2 {
		t.Fatalf("read %d points, want 2: %+v", len(got), got)
	}

	by := map[string]RestorePointInfo{}
	for _, p := range got {
		by[p.Tag] = p
	}
	ghost := by["1700172800-vmsync-cpt-000001"]
	if ghost.TakenAtUnix != 1699996400 {
		t.Errorf("taken_at_unix = %d, want the sidecar's taken_at 1699996400 -- %d is the tag's checkpoint instant, which is a different fact about a different clock",
			ghost.TakenAtUnix, ghost.CheckpointAtUnix)
	}
	if ghost.TakenAtUnix == ghost.CheckpointAtUnix {
		t.Error("taken_at_unix and checkpoint_at_unix are the same value, so one of the two fields is reporting nothing")
	}

	// The tag instant is kept beside it, and they must NOT be the same
	// field: the gauges report how old the data is, the list is ordered by
	// which copy is most recent, and under a wrong clock those differ.
	if ghost.TagAtUnix != 1700172800 {
		t.Errorf("tag_at_unix = %d, want the checkpoint instant from the directory name", ghost.TagAtUnix)
	}
}

// The ordering lives in RestorePointsFor, not in readRestorePointDir, and has
// to be asserted through the function that actually does it.
//
// readRestorePointDir appends in os.ReadDir order, which is lexical -- and
// lexical order over these fixtures happens to match the answer, so asserting
// the sort against it passes whatever the sort does, including not existing.
func TestRestorePointsForOrdersByWhenTheTargetPublishedThem(t *testing.T) {
	parent := replicaDir(t)
	d := domainIn(t, parent, "web01", "prod01:web01")
	store, ok := restorePointStoreFor(d)
	if !ok {
		t.Fatal("no store for the domain")
	}
	root, err := store.Path()
	if err != nil {
		t.Fatalf("store path: %v", err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Lexically, the ghost's directory sorts AFTER the honest one, so a
	// reader that did not sort at all would also put it last. It is the
	// publish instant that has to decide, and here it disagrees with both
	// the tag instant and the name.
	writeStampedPoint(t, root, "1700172800-vmsync-cpt-000001", 1700172800, 1699996400)
	writeStampedPoint(t, root, "1700000000-vmsync-cpt-000002", 1700000000, 1700000100)

	got := RestorePointsFor(d)
	if len(got) != 2 {
		t.Fatalf("read %d points, want 2: %+v", len(got), got)
	}
	if got[0].Tag != "1700000000-vmsync-cpt-000002" {
		t.Errorf("the list starts with %s, want the copy the target published most recently. This list is what an operator reads down during an incident, and ordering it by the tag puts a mislabelled copy at the top", got[0].Tag)
	}
}

// With no sidecar there is no target-side instant, so the tag's is all there
// is -- and Incomplete is what tells a caller which of the two they got.
func TestRestorePointWithoutASidecarFallsBackToTheTagInstant(t *testing.T) {
	dir := replicaDir(t)
	writePoint(t, dir, "1700000000-vmsync-cpt-000003", "")

	got := readRestorePointDir(dir)
	if len(got) != 1 {
		t.Fatalf("read %d points, want 1", len(got))
	}
	if got[0].TakenAtUnix != 1700000000 {
		t.Errorf("taken_at_unix = %d, want the tag instant 1700000000 as the only thing available", got[0].TakenAtUnix)
	}
	if !got[0].Incomplete {
		t.Error("a point with no sidecar was not reported as incomplete, so a caller cannot tell the fallback instant from a recorded one")
	}
}

// A sidecar that is present but carries no taken_at -- written by a vmsync
// from before the target stamped it -- must also fall back rather than
// reporting 1970, which would put it at the bottom of every list.
func TestRestorePointWithASidecarMissingTakenAtFallsBack(t *testing.T) {
	dir := replicaDir(t)
	writeStampedPoint(t, dir, "1700000000-vmsync-cpt-000004", 1700000000, 0)

	got := readRestorePointDir(dir)
	if len(got) != 1 {
		t.Fatalf("read %d points, want 1", len(got))
	}
	if got[0].TakenAtUnix != 1700000000 {
		t.Errorf("taken_at_unix = %d, want the tag instant; a zero here reads as 1970 and sorts the point below everything", got[0].TakenAtUnix)
	}
}
