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
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"vmsync/pkg/restorepoint"
)

// A STORE BIGGER THAN ONE CHUNK MUST BE READ IN FULL.
//
// The sidecar read names every point it asks about, so the command's LENGTH
// grows with the store, and it reaches the remote shell as a single argument
// -- which Linux caps at MAX_ARG_STRLEN whatever room the rest of argv has.
// publishInstants therefore asks in batches. Every fixture before this one
// held four points, so a change that read only the first batch and dropped
// the rest passed the whole suite: the points it never asked about would have
// no publish instant, land in Prune's unknown tier below every point it did
// ask about, and be the first things deleted -- the newest copies going first
// if they happened to fall in a later batch.
//
// The store that triggers this is the one that has already grown, which is
// also the one where a wrong prune costs most.
func TestAStoreLargerThanOneChunkIsReadInFull(t *testing.T) {
	store := rpTestStore(t)
	dir, _ := store.Path()
	base := time.Unix(1_700_000_000, 0).UTC()

	const points = readStatusChunk*2 + 7 // spans three batches, last one partial
	var names []string
	status := map[string]restorepoint.Status{}
	for i := 0; i < points; i++ {
		at := base.Add(time.Duration(i) * time.Minute)
		tag, err := restorepoint.NewTag(at, fmt.Sprintf("vmsync-cpt-%06d", i+1))
		if err != nil {
			t.Fatalf("NewTag: %v", err)
		}
		names = append(names, tag.String())
		status[tag.String()] = restorepoint.Status{
			Checkpoint: tag.Checkpoint, CheckpointAt: at.Unix(), TakenAt: at.Unix(),
			PublishSeq: int64(i + 1),
		}
	}

	var ran []string
	reads := 0
	r := &restorePoints{
		runner: sidecarRunner{
			dir: dir, entries: names, sidecars: status,
			ran: &ran, readCount: &reads,
		},
		store: store,
	}
	tags := make([]restorepoint.Tag, 0, len(names))
	for _, n := range names {
		tag, err := restorepoint.ParseTag(n)
		if err != nil {
			t.Fatalf("ParseTag: %v", err)
		}
		tags = append(tags, tag)
	}

	got, ok := r.publishOrder(context.Background(), tags)
	if !ok {
		t.Fatalf("the read failed over %d points; commands issued: %d", points, reads)
	}
	if len(got) != points {
		t.Errorf("read %d publish sequences from a store of %d. The points missing from the map rank below every point that was read, so a prune would delete them first", len(got), points)
	}
	// Specifically the ones past the first batch, which is what a
	// first-chunk-only read would lose.
	for _, i := range []int{readStatusChunk, readStatusChunk + 1, points - 1} {
		if _, found := got[names[i]]; !found {
			t.Errorf("point %d of %d (%s) was never asked about", i+1, points, names[i])
		}
	}
	if reads < 3 {
		t.Errorf("the store was read in %d batches; %d points at a chunk of %d needs at least 3", reads, points, readStatusChunk)
	}

	// And no single command is anywhere near the kernel's per-argument limit.
	for _, cmd := range ran {
		if !strings.Contains(cmd, testMarkerStatusPoint) {
			continue
		}
		if len(cmd) > 100*1024 {
			t.Errorf("a sidecar read command is %d bytes, within reach of MAX_ARG_STRLEN (128 KiB)", len(cmd))
		}
	}
}

// If any batch fails the whole read fails, because a partial map is
// indistinguishable from a store whose later points have no sidecars -- and
// Prune would then rank every unread point below every read one.
func TestAFailedBatchFailsTheWholeRead(t *testing.T) {
	store := rpTestStore(t)
	dir, _ := store.Path()
	base := time.Unix(1_700_000_000, 0).UTC()

	const points = readStatusChunk + 5
	var names []string
	status := map[string]restorepoint.Status{}
	var tags []restorepoint.Tag
	for i := 0; i < points; i++ {
		at := base.Add(time.Duration(i) * time.Minute)
		tag, err := restorepoint.NewTag(at, fmt.Sprintf("vmsync-cpt-%06d", i+1))
		if err != nil {
			t.Fatalf("NewTag: %v", err)
		}
		names = append(names, tag.String())
		tags = append(tags, tag)
		status[tag.String()] = restorepoint.Status{TakenAt: at.Unix(), PublishSeq: int64(i + 1)}
	}

	var ran []string
	reads := 0
	dead := false
	r := &restorePoints{
		runner: sidecarRunner{
			dir: dir, entries: names, sidecars: status,
			failSidecarRead: true, dead: &dead,
			ran: &ran, readCount: &reads,
		},
		store: store,
	}

	if got, ok := r.publishOrder(context.Background(), tags); ok {
		t.Errorf("a failed batch still reported success with %d sequences", len(got))
	}
}
