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
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"vmsync/pkg/restorepoint"
)

// nextPublishSeq is where the monotonicity comes from: one above the highest
// already in the store, so the sequence cannot go backwards whatever any
// clock on either host does.
func TestNextPublishSeqIsOneAboveTheHighest(t *testing.T) {
	store := rpTestStore(t)
	dir, _ := store.Path()
	base := time.Unix(1_700_000_000, 0).UTC()

	mk := func(i int, seq int64) (restorepoint.Tag, restorepoint.Status) {
		tag, err := restorepoint.NewTag(base.Add(time.Duration(i)*time.Hour), fmt.Sprintf("vmsync-cpt-%06d", i))
		if err != nil {
			t.Fatalf("NewTag: %v", err)
		}
		return tag, restorepoint.Status{Checkpoint: tag.Checkpoint, PublishSeq: seq}
	}

	// Deliberately not in sequence order, and the highest is not the newest
	// by tag: the maximum is over the whole store.
	t1, s1 := mk(1, 9)
	t2, s2 := mk(2, 4)
	t3, s3 := mk(3, 7)

	var ran []string
	reads := 0
	runner := sidecarRunner{
		dir:      dir,
		entries:  []string{t1.String(), t2.String(), t3.String()},
		sidecars: map[string]restorepoint.Status{t1.String(): s1, t2.String(): s2, t3.String(): s3},
		ran:      &ran, readCount: &reads,
	}

	got, err := nextPublishSeq(context.Background(), runner, store, []restorepoint.Tag{t1, t2, t3})
	if err != nil {
		t.Fatalf("nextPublishSeq: %v", err)
	}
	if got != 10 {
		t.Errorf("next sequence = %d, want 10 (one above the highest, 9). Anything else can repeat or go backwards, and two points sharing a place in the order is the defect this field exists to prevent", got)
	}
}

// A store whose points predate the field starts the sequence at 1, which puts
// every new point above all of them -- correct, because they were all
// published before any of these.
func TestNextPublishSeqStartsAtOneOnAnUnsequencedStore(t *testing.T) {
	store := rpTestStore(t)
	dir, _ := store.Path()
	base := time.Unix(1_700_000_000, 0).UTC()

	tag, err := restorepoint.NewTag(base, "vmsync-cpt-000001")
	if err != nil {
		t.Fatalf("NewTag: %v", err)
	}

	var ran []string
	reads := 0
	runner := sidecarRunner{
		dir:     dir,
		entries: []string{tag.String()},
		// A readable sidecar with no publish_seq, which is what every point
		// written before the field looks like.
		sidecars:  map[string]restorepoint.Status{tag.String(): {Checkpoint: tag.Checkpoint, TakenAt: base.Unix()}},
		ran:       &ran,
		readCount: &reads,
	}

	got, err := nextPublishSeq(context.Background(), runner, store, []restorepoint.Tag{tag})
	if err != nil {
		t.Fatalf("nextPublishSeq: %v", err)
	}
	if got != 1 {
		t.Errorf("next sequence = %d, want 1", got)
	}
}

// An empty store likewise.
func TestNextPublishSeqStartsAtOneOnAnEmptyStore(t *testing.T) {
	store := rpTestStore(t)
	dir, _ := store.Path()
	var ran []string
	reads := 0
	runner := sidecarRunner{dir: dir, sidecars: map[string]restorepoint.Status{}, ran: &ran, readCount: &reads}

	got, err := nextPublishSeq(context.Background(), runner, store, nil)
	if err != nil {
		t.Fatalf("nextPublishSeq: %v", err)
	}
	if got != 1 {
		t.Errorf("next sequence = %d on an empty store, want 1", got)
	}
}

// A store that cannot be read yields an ERROR, not a guess.
//
// The caller disarms the restore point on this, before anything is copied. A
// sequence guessed here would either repeat one already in the store -- two
// points claiming one place in the order -- or be zero, which is no place at
// all and would make the new point the first thing the next prune deleted.
func TestNextPublishSeqRefusesToGuess(t *testing.T) {
	store := rpTestStore(t)
	dir, _ := store.Path()
	base := time.Unix(1_700_000_000, 0).UTC()
	tag, err := restorepoint.NewTag(base, "vmsync-cpt-000001")
	if err != nil {
		t.Fatalf("NewTag: %v", err)
	}

	var ran []string
	reads := 0
	dead := false
	runner := sidecarRunner{
		dir: dir, entries: []string{tag.String()},
		sidecars:        map[string]restorepoint.Status{},
		failSidecarRead: true, dead: &dead,
		ran: &ran, readCount: &reads,
	}

	if got, err := nextPublishSeq(context.Background(), runner, store, []restorepoint.Tag{tag}); err == nil {
		t.Errorf("nextPublishSeq returned %d for a store it could not read; a guessed sequence either repeats an existing place in the order or is no place at all", got)
	}
}

// commit stamps the sequence the run was given, and it reaches the sidecar.
func TestCommitStampsThePublishSequence(t *testing.T) {
	store := rpTestStore(t)
	dir, _ := store.Path()
	checkpointAt := time.Unix(1_700_000_000, 0).UTC()
	targetAt := time.Unix(1_700_000_500, 0).UTC()

	tag, err := restorepoint.NewTag(checkpointAt, "vmsync-cpt-000042")
	if err != nil {
		t.Fatalf("NewTag: %v", err)
	}

	var ran []string
	reads := 0
	r := &restorePoints{
		runner: sidecarRunner{
			dir: dir, entries: []string{tag.String()},
			sidecars: map[string]restorepoint.Status{},
			targetAt: targetAt,
			ran:      &ran, readCount: &reads,
		},
		store:     store,
		point:     store.Point(tag),
		policy:    restorepoint.Policy{Count: 3, Interval: time.Hour},
		armed:     true,
		nextSeq:   41,
		wantDisks: 1,
		disks:     []string{"/data/replicas/web01.qcow2"},
	}
	if err := r.commit(context.Background(), "ok", "src01", checkpointAt, "vmsync-cpt-000042"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var sidecar string
	for _, cmd := range ran {
		if strings.Contains(cmd, restorepoint.StatusName) && strings.Contains(cmd, "publish_seq") {
			sidecar = cmd
		}
	}
	if sidecar == "" {
		t.Fatalf("commit wrote no sidecar carrying a publish_seq; commands were %q", ran)
	}
	m := regexp.MustCompile(`"publish_seq": (-?\d+)`).FindStringSubmatch(sidecar)
	if m == nil {
		t.Fatalf("no publish_seq in the sidecar: %s", sidecar)
	}
	got, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		t.Fatalf("publish_seq %q: %v", m[1], err)
	}
	if got != 41 {
		t.Errorf("publish_seq = %d, want the sequence this run was assigned (41)", got)
	}
	// And the two instants are still their own selves, which is the whole
	// reason the sequence is a separate field.
	if !strings.Contains(sidecar, fmt.Sprintf(`"checkpoint_at": %d`, checkpointAt.Unix())) {
		t.Errorf("checkpoint_at was disturbed: %s", sidecar)
	}
	if !strings.Contains(sidecar, fmt.Sprintf(`"taken_at": %d`, targetAt.Unix())) {
		t.Errorf("taken_at is not the target's clock reading any more: %s", sidecar)
	}
}

// publishOrder omits a point whose sidecar predates the field, rather than
// handing Prune a zero.
func TestPublishOrderOmitsPointsWithNoSequence(t *testing.T) {
	store := rpTestStore(t)
	dir, _ := store.Path()
	base := time.Unix(1_700_000_000, 0).UTC()

	var tags []restorepoint.Tag
	var names []string
	status := map[string]restorepoint.Status{}
	for i := 1; i <= 3; i++ {
		tag, err := restorepoint.NewTag(base.Add(time.Duration(i)*time.Hour), fmt.Sprintf("vmsync-cpt-%06d", i))
		if err != nil {
			t.Fatalf("NewTag: %v", err)
		}
		tags = append(tags, tag)
		names = append(names, tag.String())
		st := restorepoint.Status{Checkpoint: tag.Checkpoint, TakenAt: base.Unix()}
		if i != 2 { // the middle one predates the field
			st.PublishSeq = int64(i)
		}
		status[tag.String()] = st
	}

	var ran []string
	reads := 0
	r := &restorePoints{
		runner: sidecarRunner{dir: dir, entries: names, sidecars: status, ran: &ran, readCount: &reads},
		store:  store,
	}
	got, ok := r.publishOrder(context.Background(), tags)
	if !ok {
		t.Fatal("publishOrder failed on a readable store")
	}
	if len(got) != 2 {
		t.Errorf("publishOrder returned %v, want only the two points that carry a sequence", got)
	}
	if _, present := got[tags[1].String()]; present {
		t.Error("a point with publish_seq 0 was given a place in the order; zero is not a place, and Prune would rank it above points that have one")
	}
}

// probingSidecarRunner answers the reflink probe as well, so a test can drive
// newRestorePoints end to end.
type probingSidecarRunner struct {
	sidecarRunner
}

func (r probingSidecarRunner) Run(ctx context.Context, cmd string) (string, error) {
	if strings.Contains(cmd, ".vmsync-reflink-probe") {
		*r.ran = append(*r.ran, cmd)
		return "__VMSYNC_RP_REFLINK_OK__\n", nil
	}
	return r.sidecarRunner.Run(ctx, cmd)
}

// A STORE WHOSE SEQUENCES CANNOT BE READ TAKES NO RESTORE POINT, and gives up
// BEFORE anything is copied.
//
// A point published without a place in the order would rank below every point
// that has one and be the first thing the next prune deleted -- so publishing
// one means paying for a reflink copy of every disk in order to throw it
// away. Skipping costs this cycle's point and nothing else; the sync is
// unaffected and the next cycle retries.
//
// Found by mutation testing: disabling the check left the whole suite green.
func TestNewRestorePointsDisarmsWhenTheSequencesCannotBeRead(t *testing.T) {
	store := rpTestStore(t)
	dir, _ := store.Path()
	base := time.Unix(1_700_000_000, 0).UTC()

	existing, err := restorepoint.NewTag(base.Add(-6*time.Hour), "vmsync-cpt-000001")
	if err != nil {
		t.Fatalf("NewTag: %v", err)
	}

	var ran []string
	reads := 0
	dead := false
	runner := probingSidecarRunner{sidecarRunner{
		dir: dir, entries: []string{existing.String()},
		sidecars:        map[string]restorepoint.Status{},
		failSidecarRead: true, dead: &dead,
		ran: &ran, readCount: &reads,
	}}

	rp, err := newRestorePoints(context.Background(), restorepoint.Policy{Count: 3, Interval: time.Hour},
		runner, []string{"/data/replicas/web01.qcow2"}, "web01", "vmsync-cpt-000002", base)
	if err != nil {
		t.Fatalf("newRestorePoints returned an error (%v); a store it cannot read must disarm the restore point, not fail the sync", err)
	}
	if rp.armed {
		t.Error("the run is still armed, so it will copy every disk and publish a point with no place in the publish order")
	}
	if rp.stats.Outcome != rpOutcomeFailed {
		t.Errorf("outcome = %q, want %q -- a point was due and none will appear, which is what that outcome is for", rp.stats.Outcome, rpOutcomeFailed)
	}
	// And nothing was staged: giving up has to happen before any copying.
	for _, cmd := range ran {
		if strings.Contains(cmd, ".incomplete-") || strings.Contains(cmd, "mkdir -p") && strings.Contains(cmd, "incomplete") {
			t.Errorf("a staging directory was created before giving up: %q", cmd)
		}
	}
}
