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
	"testing"
	"time"

	"vmsync/pkg/restorepoint"
)

func listTag(t *testing.T, at time.Time, cpt string) restorepoint.Tag {
	t.Helper()
	tag, err := restorepoint.NewTag(at, cpt)
	if err != nil {
		t.Fatalf("NewTag: %v", err)
	}
	return tag
}

// -list-restore-points is what an operator reads during an incident, and it
// must agree with retention about which copy is the most recent.
//
// The table is oldest-first, so the LAST row is what a person reaches for.
// Ordering by the tag puts a copy whose clock was two days fast there, and
// presents a two-day-old image as the newest thing available -- while the
// prune, the agent console and the sidecars all rank it last. The sidecar
// was already being read in that loop for the VERIFY column; its taken_at
// was discarded.
func TestTheRestorePointTableIsOrderedByWhenCopiesWereMade(t *testing.T) {
	T := time.Unix(1_700_000_000, 0).UTC()

	ghost := listTag(t, T.Add(48*time.Hour), "vmsync-cpt-000001") // tag says two days hence
	honest := listTag(t, T.Add(-time.Hour), "vmsync-cpt-000002")

	// Built in the WRONG order deliberately. The fixture used to be in the
	// order it then asserted, so replacing the comparator body with
	// `return false` passed -- the test caught the two wrong orderings it
	// was written for and not the deletion of the sort, which is the
	// mutation a refactor actually produces.
	rows := []restorePointRow{
		restorePointRowFor(honest, restorepoint.Status{TakenAt: T.Add(-time.Hour).Unix(), Verify: "ok"}, true),
		restorePointRowFor(ghost, restorepoint.Status{TakenAt: T.Add(-3 * time.Hour).Unix(), Verify: "ok"}, true),
	}
	sortRestorePointRows(rows)

	if rows[0].tag.String() != ghost.String() {
		t.Errorf("the table starts with %s; oldest first means the copy published three hours ago, not the one whose tag is dated furthest ahead", rows[0].tag)
	}
	if rows[1].tag.String() != honest.String() {
		t.Errorf("the table ends with %s; the last row is what an operator reaches for, and it must be the copy made most recently", rows[1].tag)
	}
}

// The TAKEN column shows the sidecar's instant, which is the whole point of
// having it beside DATA: the two differing is the diagnosis.
func TestTheTakenColumnComesFromTheSidecar(t *testing.T) {
	T := time.Unix(1_700_000_000, 0).UTC()
	tag := listTag(t, T.Add(48*time.Hour), "vmsync-cpt-000001")
	published := T.Add(-3 * time.Hour)

	r := restorePointRowFor(tag, restorepoint.Status{TakenAt: published.Unix(), Verify: "ok", Checkpoint: "vmsync-cpt-000001"}, true)

	if !r.takenAt.Equal(published) {
		t.Errorf("TAKEN = %v, want the sidecar's taken_at %v -- the tag instant belongs in the DATA column", r.takenAt, published)
	}
	if r.takenAt.Equal(tag.At) {
		t.Error("TAKEN and DATA carry the same instant, so one of the two columns is reporting nothing")
	}
	if r.verify != "ok" {
		t.Errorf("verify = %q, want the sidecar's", r.verify)
	}
}

// Without a readable sidecar there is no publish instant, so the tag's is all
// there is -- and it must be used rather than a zero, which would sort an
// unreadable point to 1970 and put it at the top of an oldest-first table.
func TestAnUnreadableSidecarFallsBackToTheTagInstant(t *testing.T) {
	T := time.Unix(1_700_000_000, 0).UTC()
	tag := listTag(t, T, "vmsync-cpt-000003")

	r := restorePointRowFor(tag, restorepoint.Status{}, false)

	if !r.takenAt.Equal(tag.At.UTC()) {
		t.Errorf("TAKEN = %v, want the tag instant %v as the only one available", r.takenAt, tag.At.UTC())
	}
	if r.verify != "unknown" {
		t.Errorf("verify = %q, want \"unknown\" -- inventing one defeats the point of recording it", r.verify)
	}

	// Same when the sidecar is readable but carries no taken_at, which is
	// what a point written before the target stamped it looks like.
	r2 := restorePointRowFor(tag, restorepoint.Status{Verify: "ok"}, true)
	if !r2.takenAt.Equal(tag.At.UTC()) {
		t.Errorf("TAKEN = %v for a sidecar with no taken_at, want the tag instant", r2.takenAt)
	}
}
