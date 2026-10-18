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

// The markers are restorepoint's, repeated here because they are unexported.
// Repeating them is safe in the direction that matters: if they ever diverge
// this runner stops recognising the sidecar read, ParseAllStatus rejects the
// answer, the prune is skipped, and the assertions below fail loudly rather
// than quietly stopping to test anything.
const (
	testMarkerStatusPoint = "__VMSYNC_RP_SC__"
	testMarkerStatusEnd   = "__VMSYNC_RP_SC_END__"
	testMarkerStatusList  = "__VMSYNC_RP_SC_LIST__"
	testMarkerListing     = "__VMSYNC_RP_LIST__"
	testMarkerNow         = "__VMSYNC_RP_NOW__"
)

// sidecarRunner answers every command the retention path issues -- the target
// clock read, the directory listing, the batched sidecar read, the sidecar
// write, the commit rename and the removals -- and records them all.
type sidecarRunner struct {
	dir      string
	entries  []string
	sidecars map[string]restorepoint.Status // keyed by tag directory name
	targetAt time.Time                      // what the target says its clock is

	// Failure injection, one per branch the production code has.
	failSidecarRead bool
	garbleSidecar   bool // exit 0 with output that is not ours
	failClock       bool
	garbleClock     bool // exit 0, but the answer is not a unix time

	ran       *[]string
	readCount *int
}

func (r sidecarRunner) Run(_ context.Context, cmd string) (string, error) {
	*r.ran = append(*r.ran, cmd)

	switch {
	case strings.Contains(cmd, testMarkerNow):
		if r.failClock {
			return "", fmt.Errorf("ssh: connection closed")
		}
		if r.garbleClock {
			// The command ran and exited 0; date was not there to answer it.
			// Distinct from the branch above, and reached by different code.
			return testMarkerNow + "\nsh: date: command not found\n", nil
		}
		return testMarkerNow + "\n" + fmt.Sprintf("%d", r.targetAt.Unix()) + "\n", nil

	// Checked before the listing: the sidecar read names the same directory.
	case strings.Contains(cmd, testMarkerStatusPoint):
		*r.readCount++
		if r.failSidecarRead {
			return "cat: permission denied", fmt.Errorf("exit status 1")
		}
		if r.garbleSidecar {
			// Exit 0, but it is the LISTING's answer. A parser that only
			// looked for records would read this as "no point has a publish
			// instant" and prune by the checkpoint instant believing it had
			// looked.
			return testMarkerListing + "\n" + strings.Join(r.entries, "/\n") + "/\n", nil
		}
		var b strings.Builder
		b.WriteString(testMarkerStatusList + "\n")
		for _, name := range r.entries {
			st, ok := r.sidecars[name]
			if !ok {
				continue
			}
			body, err := st.Encode()
			if err != nil {
				return "", err
			}
			b.WriteString(testMarkerStatusPoint + " " + r.dir + "/" + name + "/\n")
			b.Write(body)
			b.WriteString(testMarkerStatusEnd + "\n")
		}
		return b.String(), nil

	case strings.Contains(cmd, "'"+r.dir+"'"):
		out := testMarkerListing + "\n"
		for _, e := range r.entries {
			out += e + "/\n"
		}
		return out, nil
	}
	return "", nil
}

type rpFixture struct {
	store  restorepoint.Store
	dir    string
	tags   []restorepoint.Tag
	status map[string]restorepoint.Status
	names  []string
}

// Builds a store whose tags and sidecars DISAGREE in the way a wrong clock
// on the host running vmsync produces: that host stamps checkpoint_at two
// days hence, while the target stamps taken_at with the real time.
//
// taken_at BELOW checkpoint_at is exactly what that looks like, and it is
// reachable only because the two now come from different clocks -- when both
// were time.Now() in one process, taken_at was always at or after
// checkpoint_at by the duration of the copy, and a fixture like this one
// described a store the system could not produce.
func skewedStore(t *testing.T, T time.Time) rpFixture {
	t.Helper()
	store := rpTestStore(t)
	dir, _ := store.Path()
	f := rpFixture{store: store, dir: dir, status: map[string]restorepoint.Status{}}

	add := func(at time.Time, cpt string, taken time.Time) restorepoint.Tag {
		tag, err := restorepoint.NewTag(at, cpt)
		if err != nil {
			t.Fatalf("NewTag: %v", err)
		}
		f.tags = append(f.tags, tag)
		f.names = append(f.names, tag.String())
		f.status[tag.String()] = restorepoint.Status{
			Checkpoint: cpt, CheckpointAt: at.Unix(), TakenAt: taken.Unix(), Source: "src01",
		}
		return tag
	}
	// Published while the clock ran 48h fast, so the tag is dated then and
	// the target's own record is not.
	add(T.Add(48*time.Hour), "vmsync-cpt-000001", T.Add(-3*time.Hour))
	add(T.Add(47*time.Hour), "vmsync-cpt-000002", T.Add(-2*time.Hour))
	// Published after the correction.
	add(T.Add(-time.Hour), "vmsync-cpt-000003", T.Add(-time.Hour))
	add(T, "vmsync-cpt-000004", T)
	return f
}

func (f rpFixture) removalsAmong(ran []string) []string {
	var out []string
	for _, cmd := range ran {
		if strings.Contains(cmd, testMarkerStatusPoint) {
			continue // the sidecar read names every point by design
		}
		for _, name := range f.names {
			if strings.Contains(cmd, name) {
				out = append(out, name)
			}
		}
	}
	return out
}

// prune must order by what the SIDECARS say, not by the tag instant.
//
// This is the test that catches the call site forgetting to pass the map --
// Prune's own tests cannot see a caller that hands it nil.
func TestPruneOrdersByTheSidecarsNotTheTagInstant(t *testing.T) {
	T := time.Unix(1_700_000_000, 0).UTC()
	f := skewedStore(t, T)

	var ran []string
	reads := 0
	r := &restorePoints{
		runner: sidecarRunner{
			dir: f.dir, entries: f.names, sidecars: f.status,
			ran: &ran, readCount: &reads,
		},
		store:  f.store,
		point:  f.store.Point(f.tags[3]),
		policy: restorepoint.Policy{Count: 3, Interval: time.Hour},
		armed:  true,
	}
	r.prune(context.Background())

	if reads != 1 {
		t.Fatalf("prune read the sidecars %d times; want exactly one round trip for the whole store", reads)
	}
	got := f.removalsAmong(ran)
	want := []string{f.tags[0].String()} // the one the target published longest ago
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("removed %v, want %v -- ordering by the tag instant would have taken %s, the copy from an hour ago",
			got, want, f.tags[2])
	}
}

// A sidecar read that FAILS must stop the prune, not fall back.
//
// The two outcomes are not comparable. Ordering by the tag instant under skew
// deletes the point the previous run published on every cycle, and a deletion
// cannot be taken back; not pruning costs disk and announces itself, because
// Over stays positive and that is the documented signal. Growth is
// recoverable and visible, the wrong deletion is neither.
func TestPruneDeletesNothingWhenTheSidecarReadFails(t *testing.T) {
	T := time.Unix(1_700_000_000, 0).UTC()
	f := skewedStore(t, T)

	var ran []string
	reads := 0
	r := &restorePoints{
		runner: sidecarRunner{
			dir: f.dir, entries: f.names, sidecars: f.status,
			failSidecarRead: true,
			ran:             &ran, readCount: &reads,
		},
		store:  f.store,
		point:  f.store.Point(f.tags[3]),
		policy: restorepoint.Policy{Count: 3, Interval: time.Hour},
		armed:  true,
	}
	r.prune(context.Background())

	if reads == 0 {
		t.Fatal("the sidecar read was never attempted")
	}
	if got := f.removalsAmong(ran); len(got) != 0 {
		t.Errorf("the sidecar read failed and prune deleted %v anyway, by an order it had just failed to establish", got)
	}
}

// Exit 0 with output that is not ours is a FAILURE, not an empty store.
//
// If those two were the same answer, a mismatched marker or an SSH banner
// would silently put retention back on the checkpoint instant while the run
// reported nothing at all -- the original defect, reached by a different road.
func TestPruneDeletesNothingWhenTheTargetAnswersSomethingElse(t *testing.T) {
	T := time.Unix(1_700_000_000, 0).UTC()
	f := skewedStore(t, T)

	var ran []string
	reads := 0
	r := &restorePoints{
		runner: sidecarRunner{
			dir: f.dir, entries: f.names, sidecars: f.status,
			garbleSidecar: true,
			ran:           &ran, readCount: &reads,
		},
		store:  f.store,
		point:  f.store.Point(f.tags[3]),
		policy: restorepoint.Policy{Count: 3, Interval: time.Hour},
		armed:  true,
	}
	r.prune(context.Background())

	if got := f.removalsAmong(ran); len(got) != 0 {
		t.Errorf("the target answered the sidecar read with a listing and prune deleted %v on the strength of it", got)
	}
}

// A store whose points have no sidecars is an ANSWER, and the prune proceeds.
//
// Distinct from the two tests above: the read succeeded and the store really
// has nothing to say about when its points were published. Refusing to prune
// here would mean such a store grew without bound forever.
func TestPruneStillRunsWhenTheStoreHasNoSidecars(t *testing.T) {
	T := time.Unix(1_700_000_000, 0).UTC()
	store := rpTestStore(t)
	dir, _ := store.Path()

	var tags []restorepoint.Tag
	var names []string
	for i, at := range []time.Time{T, T.Add(-time.Hour), T.Add(-2 * time.Hour), T.Add(-3 * time.Hour)} {
		tag, err := restorepoint.NewTag(at, fmt.Sprintf("vmsync-cpt-%06d", 4-i))
		if err != nil {
			t.Fatalf("NewTag: %v", err)
		}
		tags = append(tags, tag)
		names = append(names, tag.String())
	}

	var ran []string
	reads := 0
	r := &restorePoints{
		runner: sidecarRunner{
			dir: dir, entries: names,
			sidecars: map[string]restorepoint.Status{}, // read succeeds, says nothing
			ran:      &ran, readCount: &reads,
		},
		store:  store,
		point:  store.Point(tags[0]),
		policy: restorepoint.Policy{Count: 3, Interval: time.Hour},
		armed:  true,
	}
	r.prune(context.Background())

	removedOldest, removedFresh := false, false
	for _, cmd := range ran {
		if strings.Contains(cmd, testMarkerStatusPoint) {
			continue
		}
		if strings.Contains(cmd, tags[3].String()) {
			removedOldest = true
		}
		if strings.Contains(cmd, tags[0].String()) {
			removedFresh = true
		}
	}
	if !removedOldest {
		t.Errorf("a store with no sidecars was not pruned at all, so it would grow without bound; commands %q", ran)
	}
	if removedFresh {
		t.Error("the point this run just published was removed")
	}
}

// taken_at MUST be the target's clock.
//
// This is the invariant the whole ordering rests on, and the one an earlier
// version of this change got wrong: it stamped taken_at with vmsync's own
// time.Now(), which is the same clock that produced the checkpoint instant.
// Both values then moved together under a skew and the ordering was inert --
// correct-looking code that changed nothing. The target's clock here is set
// to 2023 precisely so that this process's own clock cannot pass the test.
func TestCommitStampsTheSidecarWithTheTargetsClock(t *testing.T) {
	store := rpTestStore(t)
	dir, _ := store.Path()
	targetAt := time.Unix(1_700_000_500, 0).UTC()
	checkpointAt := time.Unix(1_700_000_000, 0).UTC()

	tag, err := restorepoint.NewTag(checkpointAt, "vmsync-cpt-000007")
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
		wantDisks: 1,
		disks:     []string{"/data/replicas/web01.qcow2"},
	}
	if err := r.commit(context.Background(), "ok", "src01", checkpointAt, "vmsync-cpt-000007"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var sidecar string
	for _, cmd := range ran {
		if strings.Contains(cmd, restorepoint.StatusName) && strings.Contains(cmd, "taken_at") {
			sidecar = cmd
		}
	}
	if sidecar == "" {
		t.Fatalf("commit never wrote a sidecar; commands were %q", ran)
	}
	if want := fmt.Sprintf(`"taken_at": %d`, targetAt.Unix()); !strings.Contains(sidecar, want) {
		t.Errorf("the sidecar does not carry the target's clock (%s): %s", want, sidecar)
	}
	// Specifically not this process's clock, which is years away from the
	// fixture's. Without this the assertion above would pass on a run that
	// happened to be stamped locally in 2023.
	if local := fmt.Sprintf(`"taken_at": %d`, time.Now().Unix()); strings.Contains(sidecar, local) {
		t.Errorf("the sidecar carries this host's clock, which is the clock that also produced the checkpoint instant: %s", sidecar)
	}
	if want := fmt.Sprintf(`"checkpoint_at": %d`, checkpointAt.Unix()); !strings.Contains(sidecar, want) {
		t.Errorf("checkpoint_at was not preserved: %s", sidecar)
	}
}

// The clock read failing must not fail a copy that has already been made,
// and it must fall back to the LOCAL clock -- not to the zero time, which
// would stamp the point 1970 and send the next prune straight at it.
//
// Both ways the read can fail, because they are different branches:
// the command erroring, and the command succeeding with an answer that is
// not a unix time. Asserting only that a sidecar was written would pass
// against either of them stamping anything at all.
func TestCommitFallsBackToTheLocalClockWhenTheTargetsCannotBeRead(t *testing.T) {
	for _, c := range []struct {
		name   string
		fail   bool
		garble bool
	}{
		{"the read errors", true, false},
		{"the read succeeds with a non-answer", false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			store := rpTestStore(t)
			dir, _ := store.Path()
			checkpointAt := time.Unix(1_700_000_000, 0).UTC()
			tag, err := restorepoint.NewTag(checkpointAt, "vmsync-cpt-000008")
			if err != nil {
				t.Fatalf("NewTag: %v", err)
			}

			var ran []string
			reads := 0
			before := time.Now().Unix()
			r := &restorePoints{
				runner: sidecarRunner{
					dir: dir, entries: []string{tag.String()},
					sidecars:    map[string]restorepoint.Status{},
					failClock:   c.fail,
					garbleClock: c.garble,
					ran:         &ran, readCount: &reads,
				},
				store:     store,
				point:     store.Point(tag),
				policy:    restorepoint.Policy{Count: 3, Interval: time.Hour},
				armed:     true,
				wantDisks: 1,
				disks:     []string{"/data/replicas/web01.qcow2"},
			}
			if err := r.commit(context.Background(), "ok", "src01", checkpointAt, "vmsync-cpt-000008"); err != nil {
				t.Fatalf("commit refused to publish a copy that was already made: %v", err)
			}
			after := time.Now().Unix()

			var sidecar string
			for _, cmd := range ran {
				if strings.Contains(cmd, restorepoint.StatusName) && strings.Contains(cmd, "taken_at") {
					sidecar = cmd
				}
			}
			if sidecar == "" {
				t.Fatalf("no sidecar was written after the clock read failed; commands were %q", ran)
			}
			m := regexp.MustCompile(`"taken_at": (-?\d+)`).FindStringSubmatch(sidecar)
			if m == nil {
				t.Fatalf("the sidecar carries no taken_at: %s", sidecar)
			}
			got, err := strconv.ParseInt(m[1], 10, 64)
			if err != nil {
				t.Fatalf("taken_at %q: %v", m[1], err)
			}
			if got < before || got > after {
				t.Errorf("taken_at = %d, want this host's clock between %d and %d. A zero or a stale value here would stamp the point far in the past and make it the first thing the next prune deletes", got, before, after)
			}
		})
	}
}
