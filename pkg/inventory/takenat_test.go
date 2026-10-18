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
// target said the time was when it published, and publish_seq carries the
// order it was actually published in.
func writeStampedPoint(t *testing.T, dir, tag string, checkpointAt, takenAt, seq int64) {
	t.Helper()
	p := filepath.Join(dir, tag)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", p, err)
	}
	b, err := restorepoint.Status{
		Checkpoint: "vmsync-cpt-000042", CheckpointAt: checkpointAt, TakenAt: takenAt,
		PublishSeq: seq, Source: "src01", Verify: restorepoint.VerifyNotRun,
	}.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := os.WriteFile(filepath.Join(p, restorepoint.StatusName), b, 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
}

// EVERY FIELD REPORTS ITS OWN FACT, and none of them substitutes for another.
//
// taken_at_unix used to fall back to the tag instant when the sidecar did not
// record one -- a different fact, on a different host's clock, under this
// field's name. The two are now separate and either can be absent without the
// other covering for it.
func TestRestorePointsReportEachInstantSeparately(t *testing.T) {
	dir := replicaDir(t)
	// Written while the clock ran 48h fast: the tag says two days hence, the
	// target's own record says it happened first, and the sequence says it
	// was published first of the two.
	writeStampedPoint(t, dir, "1700172800-vmsync-cpt-000001", 1700172800, 1699996400, 1)
	// Written after the correction.
	writeStampedPoint(t, dir, "1700000000-vmsync-cpt-000002", 1700000000, 1700000100, 2)

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
		t.Errorf("taken_at_unix = %d, want the sidecar's taken_at 1699996400", ghost.TakenAtUnix)
	}
	if ghost.TagAtUnix != 1700172800 {
		t.Errorf("tag_at_unix = %d, want the checkpoint instant from the directory name", ghost.TagAtUnix)
	}
	if ghost.PublishSeq != 1 {
		t.Errorf("publish_seq = %d, want 1", ghost.PublishSeq)
	}
	// Three different facts, three different values. Any two of them being
	// equal here would mean one is standing in for another.
	if ghost.TakenAtUnix == ghost.TagAtUnix {
		t.Error("taken_at_unix and tag_at_unix hold the same value, so one of the two is reporting nothing")
	}
}

// A SIDECAR THAT DOES NOT RECORD taken_at REPORTS ZERO, not a substitute.
//
// Reachable deliberately: the target's clock may not be readable at publish,
// in which case vmsync records no publish time rather than one from the wrong
// clock (cmd/vmsync's targetPublishInstant). A reader that filled the gap
// with the tag instant would present that decision as a fact about the
// target.
func TestRestorePointWithNoTakenAtReportsZero(t *testing.T) {
	dir := replicaDir(t)
	writeStampedPoint(t, dir, "1700000000-vmsync-cpt-000004", 1700000000, 0, 3)

	got := readRestorePointDir(dir)
	if len(got) != 1 {
		t.Fatalf("read %d points, want 1", len(got))
	}
	if got[0].TakenAtUnix != 0 {
		t.Errorf("taken_at_unix = %d, want 0 -- the sidecar does not record one, and anything else here is a different fact wearing this name", got[0].TakenAtUnix)
	}
	// The instant the contents are from is still reported, so nothing an
	// operator needs has gone missing.
	if got[0].TagAtUnix != 1700000000 {
		t.Errorf("tag_at_unix = %d, want the checkpoint instant", got[0].TagAtUnix)
	}
	// And it still has a place in the order, which is what the list sorts by.
	if got[0].PublishSeq != 3 {
		t.Errorf("publish_seq = %d, want 3", got[0].PublishSeq)
	}
}

// With no sidecar at all there is nothing but the tag, and Incomplete is what
// tells a caller that is what they are looking at.
func TestRestorePointWithoutASidecarReportsOnlyTheTag(t *testing.T) {
	dir := replicaDir(t)
	writePoint(t, dir, "1700000000-vmsync-cpt-000003", "")

	got := readRestorePointDir(dir)
	if len(got) != 1 {
		t.Fatalf("read %d points, want 1", len(got))
	}
	if !got[0].Incomplete {
		t.Error("a point with no sidecar was not reported as incomplete, so a caller cannot tell an absent record from a recorded zero")
	}
	if got[0].TagAtUnix != 1700000000 {
		t.Errorf("tag_at_unix = %d, want the checkpoint instant, which is all there is", got[0].TagAtUnix)
	}
	if got[0].TakenAtUnix != 0 || got[0].PublishSeq != 0 {
		t.Errorf("a point with no sidecar reported taken_at=%d seq=%d, want both absent", got[0].TakenAtUnix, got[0].PublishSeq)
	}
}

// THE LIST IS ORDERED BY THE PUBLISH SEQUENCE, which no clock can reorder.
//
// The ordering lives in RestorePointsFor, not in readRestorePointDir, and has
// to be asserted through the function that actually does it --
// readRestorePointDir appends in os.ReadDir order.
//
// The fixture makes every clock disagree with the answer: the point published
// SECOND has the older tag and the older taken_at, so only the sequence picks
// it out. Ordering by either instant would put the other one first.
func TestRestorePointsForOrdersByThePublishSequence(t *testing.T) {
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

	// seq 1: the newest tag and the newest taken_at, published FIRST.
	writeStampedPoint(t, root, "1700172800-vmsync-cpt-000001", 1700172800, 1700172900, 1)
	// seq 2: the oldest tag and the oldest taken_at, published SECOND.
	writeStampedPoint(t, root, "1700000000-vmsync-cpt-000002", 1700000000, 1700000100, 2)

	got := RestorePointsFor(d)
	if len(got) != 2 {
		t.Fatalf("read %d points, want 2: %+v", len(got), got)
	}
	if got[0].Tag != "1700000000-vmsync-cpt-000002" {
		t.Errorf("the list starts with %s, want the copy published most recently (publish_seq 2). Ordering by either instant would have put the other one first, which is the mistake a wrong clock produces", got[0].Tag)
	}
}

// A point with no sequence ranks below every point that has one -- the same
// two-tier rule retention uses, so the list and the prune cannot disagree
// about which copy is most recent.
func TestRestorePointsForRanksUnsequencedPointsLast(t *testing.T) {
	parent := replicaDir(t)
	d := domainIn(t, parent, "web01", "prod01:web01")
	store, _ := restorePointStoreFor(d)
	root, _ := store.Path()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// No sequence, and the NEWEST tag of the three, so any instant-based
	// order would put it first.
	writeStampedPoint(t, root, "1700900000-vmsync-cpt-000001", 1700900000, 1700900100, 0)
	writeStampedPoint(t, root, "1700000000-vmsync-cpt-000002", 1700000000, 1700000100, 1)
	writeStampedPoint(t, root, "1700003600-vmsync-cpt-000003", 1700003600, 1700003700, 2)

	got := RestorePointsFor(d)
	if len(got) != 3 {
		t.Fatalf("read %d points, want 3", len(got))
	}
	if got[0].Tag != "1700003600-vmsync-cpt-000003" {
		t.Errorf("the list starts with %s, want the highest sequence", got[0].Tag)
	}
	if got[2].Tag != "1700900000-vmsync-cpt-000001" {
		t.Errorf("the list ends with %s, want the point with no sequence -- an unknown must not outrank a known however it is dated", got[2].Tag)
	}
}
