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
	"strings"
	"testing"
	"time"
)

func tagNames(tags []Tag) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		out = append(out, t.Checkpoint)
	}
	return out
}

// HISTORY MUST ACCUMULATE ACROSS RUNS, which is the one property every other
// test in this package was structurally unable to see.
//
// Protecting the tag a run has just published fixes the FIRST cycle and
// nothing after it: from the second run onward the lowest-ranked unprotected
// tag is the real-dated point the PREVIOUS run published, so that is what
// gets deleted, while two future-dated ghosts sit permanently at the top of
// the order. Every single-shot fixture passes against that, because the
// defect is a steady state and only appears on iteration two.
//
// Eight cycles, a source clock that was 48h ahead and has been corrected, and
// a store already holding three ghosts from before the correction. What the
// store must converge on is the last three points this loop published.
func TestPruneAccumulatesHistoryAcrossRuns(t *testing.T) {
	start := time.Unix(1_700_000_000, 0).UTC()
	policy := Policy{Count: 3, Interval: time.Hour}

	publishedAt := map[string]time.Time{}
	var store []Tag
	// The ghosts: published for real in the three hours before the clock was
	// corrected, and so labelled two days hence.
	for i, ahead := range []time.Duration{48 * time.Hour, 47 * time.Hour, 46 * time.Hour} {
		g := Tag{At: start.Add(ahead), Checkpoint: fmt.Sprintf("ghost-%03d", i+1)}
		store = append(store, g)
		publishedAt[g.String()] = start.Add(-time.Duration(3-i) * time.Hour)
	}

	const runs = 8
	for i := 1; i <= runs; i++ {
		now := start.Add(time.Duration(i) * time.Hour)
		fresh := Tag{At: now, Checkpoint: fmt.Sprintf("run-%03d", i)}
		publishedAt[fresh.String()] = now

		plan := Prune(append(append([]Tag{}, store...), fresh), fresh, publishedAt, policy)
		if inRemove(plan, fresh) {
			t.Fatalf("run %d deleted the point it had just published", i)
		}
		// The point the PREVIOUS run published must not be deleted by this one
		// while the store has older points to give up. This is the defect.
		if i > 1 {
			prev := Tag{At: start.Add(time.Duration(i-1) * time.Hour), Checkpoint: fmt.Sprintf("run-%03d", i-1)}
			if inRemove(plan, prev) {
				t.Fatalf("run %d deleted the point run %d published, while the store still held %v -- history cannot accumulate", i, i-1, tagNames(store))
			}
		}
		store = plan.Keep
	}

	want := []string{"run-008", "run-007", "run-006"}
	got := tagNames(store)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("after %d runs the store holds %v, want %v", runs, got, want)
	}
}

// A point whose publish time is unknown must not outrank one that is known.
//
// The sidecar is the only thing that says when THIS host made a copy. A point
// without a readable one could be dated anything at all by a source clock, so
// it is placed below every point that can account for itself -- rather than
// being trusted at face value, which is the whole defect, or defaulted to
// zero, which would make an unreadable sidecar a deletion order.
func TestPruneRanksUnknownPublishTimesLast(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	ghost := Tag{At: now.Add(48 * time.Hour), Checkpoint: "vmsync-cpt-000001"} // no sidecar
	older := Tag{At: now.Add(-2 * time.Hour), Checkpoint: "vmsync-cpt-000002"}
	fresh := Tag{At: now, Checkpoint: "vmsync-cpt-000003"}
	publishedAt := map[string]time.Time{
		older.String(): now.Add(-2 * time.Hour),
		fresh.String(): now,
	}

	plan := Prune([]Tag{ghost, older, fresh}, fresh, publishedAt, Policy{Count: 2, Interval: time.Hour})

	if !inRemove(plan, ghost) {
		t.Errorf("the point with no recorded publish time was kept over one that had it; Keep=%v Remove=%v", tagNames(plan.Keep), tagNames(plan.Remove))
	}
	if inRemove(plan, older) {
		t.Error("a point with a recorded publish time was dropped in favour of one dated in the future by its source")
	}
}

// With nothing known, the order must be exactly what it was before -- a store
// this run learned nothing about is not a store to start reordering.
func TestPruneWithNoPublishTimesOrdersByTagInstant(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	tags := []Tag{
		{At: now.Add(-2 * time.Hour), Checkpoint: "vmsync-cpt-000001"},
		{At: now, Checkpoint: "vmsync-cpt-000003"},
		{At: now.Add(-time.Hour), Checkpoint: "vmsync-cpt-000002"},
	}
	plan := Prune(tags, Tag{}, nil, Policy{Count: 2, Interval: time.Hour})
	if got := tagNames(plan.Keep); strings.Join(got, ",") != "vmsync-cpt-000003,vmsync-cpt-000002" {
		t.Errorf("Keep=%v, want the two newest by tag instant", got)
	}
	if len(plan.Remove) != 1 || plan.Remove[0].Checkpoint != "vmsync-cpt-000001" {
		t.Errorf("Remove=%v, want the oldest", tagNames(plan.Remove))
	}
}

// The keep protection is NOT made redundant by ordering on publish time.
//
// A point published moments ago may not have a readable sidecar yet -- it
// lands in the unknown tier, below ghosts that do have one. The one copy a
// run must never delete is the one it just made, whatever the order says.
func TestPruneProtectsANewPointWhoseSidecarIsNotReadableYet(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	a := Tag{At: now.Add(-3 * time.Hour), Checkpoint: "vmsync-cpt-000001"}
	b := Tag{At: now.Add(-2 * time.Hour), Checkpoint: "vmsync-cpt-000002"}
	fresh := Tag{At: now, Checkpoint: "vmsync-cpt-000003"}
	publishedAt := map[string]time.Time{
		a.String(): now.Add(-3 * time.Hour),
		b.String(): now.Add(-2 * time.Hour),
		// fresh deliberately absent
	}

	plan := Prune([]Tag{a, b, fresh}, fresh, publishedAt, Policy{Count: 2, Interval: time.Hour})

	if inRemove(plan, fresh) {
		t.Error("the point this run just published was deleted because its sidecar was not readable yet")
	}
	if !inRemove(plan, a) {
		t.Errorf("the oldest point was not the one dropped; Remove=%v", tagNames(plan.Remove))
	}
}

func TestReadAllStatusCommandIsOneRoundTripAndSurvivesAMissingSidecar(t *testing.T) {
	s, err := StoreForDir("/data/replicas", "web01")
	if err != nil {
		t.Fatalf("StoreForDir: %v", err)
	}
	cmd, err := ReadAllStatusCommand(s)
	if err != nil {
		t.Fatalf("ReadAllStatusCommand: %v", err)
	}
	root, _ := s.Path()
	if !strings.Contains(cmd, root) {
		t.Errorf("the command does not name the store root %q: %s", root, cmd)
	}
	if !strings.Contains(cmd, StatusName) {
		t.Errorf("the command does not read %s: %s", StatusName, cmd)
	}
	// A point with no sidecar must not fail the whole read: the caller needs
	// the other points' instants, and "this one has none" is an answer.
	if !strings.Contains(cmd, "2>/dev/null") {
		t.Errorf("an unreadable sidecar would fail the command: %s", cmd)
	}
	// One round trip. A per-point command would put ReadStatusCommand's shape
	// here instead, and a store at -retention=24 would pay 24 of them.
	if n := strings.Count(cmd, "for d in"); n != 1 {
		t.Errorf("want a single loop over the store, found %d: %s", n, cmd)
	}
	// The record's identity goes through printf with the path as an ARGUMENT.
	// echo with a variable argument is the one construct POSIX leaves
	// unspecified -- under dash it expands backslashes in the value, and the
	// store root comes from an operator-supplied -target-disk-path. This is
	// the rule StatusCommand states for the same reason.
	if strings.Contains(cmd, `echo "${d}"`) || strings.Contains(cmd, "echo "+markerStatusPoint+` "${d}"`) {
		t.Errorf("the record path is emitted with echo rather than printf: %s", cmd)
	}
	if !strings.Contains(cmd, `printf '%s %s\n' `) {
		t.Errorf("the record path is not emitted through printf with the value as an argument: %s", cmd)
	}
}

// The two readers must not accept each other's output.
//
// They bracket their answers with different markers precisely so that a
// crossed wire is an error rather than a plausible answer: a sidecar dump
// read as a listing would be a store with no points, and a listing read as a
// sidecar dump would be a store whose points have no publish instants. Both
// are wrong, and neither announces itself. An earlier version of this code
// reused the listing's markers for both and claimed in a comment that this
// could not happen.
func TestTheListingAndTheSidecarReadRejectEachOther(t *testing.T) {
	s, err := StoreForDir("/data/replicas", "web01")
	if err != nil {
		t.Fatalf("StoreForDir: %v", err)
	}
	listCmd, _ := ListCommand(s)
	scCmd, _ := ReadAllStatusCommand(s)
	if strings.Contains(scCmd, markerListing) || strings.Contains(scCmd, markerListingNone) {
		t.Errorf("the sidecar read is bracketed by the listing's markers: %s", scCmd)
	}
	if strings.Contains(listCmd, markerStatus) || strings.Contains(listCmd, markerStatusNone) {
		t.Errorf("the listing is bracketed by the sidecar read's markers: %s", listCmd)
	}

	// A listing's output offered to the sidecar parser.
	listOut := markerListing + "\n1700000000-vmsync-cpt-000001/\n"
	if got, err := ParseAllStatus(listOut); err == nil {
		t.Errorf("ParseAllStatus accepted a listing and returned %v; it must refuse output that is not its own, or a crossed wire reads as a store with no publish instants and retention silently orders by the checkpoint instant", got)
	}

	// A sidecar dump offered to the listing parser. It must be an ERROR and
	// not an empty store: those two were the same answer here until this
	// change, so a crossed wire read as "nothing to prune" rather than as a
	// command that had gone to the wrong parser.
	scOut := markerStatus + "\n" + markerStatusPoint + " /x/1700000000-vmsync-cpt-000001/\n{\"taken_at\":1}\n" + markerStatusEnd + "\n"
	if l, err := ParseListing(scOut); err == nil {
		t.Errorf("ParseListing accepted a sidecar dump and returned %+v; it must refuse output that is not its own", l)
	}

	// The real listing still parses, so the refusal above is not simply
	// refusing everything.
	if l, err := ParseListing(markerListing + "\n1700000000-vmsync-cpt-000001/\n"); err != nil || len(l.Points) != 1 {
		t.Errorf("ParseListing rejected its own output: %+v %v", l, err)
	}
	// And an absent store is still an answer rather than an error.
	if _, err := ParseListing(markerListingNone + "\n"); err != nil {
		t.Errorf("an absent store was reported as an error: %v", err)
	}
}

func TestParseAllStatusReadsEachPointsOwnInstant(t *testing.T) {
	out := strings.Join([]string{
		markerStatus,
		markerStatusPoint + " /data/replicas/.rp/web01/1700000000-vmsync-cpt-000001/",
		`{"checkpoint":"vmsync-cpt-000001","checkpoint_at":1700000000,"taken_at":1700000120}`,
		markerStatusEnd,
		markerStatusPoint + " /data/replicas/.rp/web01/1700003600-vmsync-cpt-000002/",
		`{"checkpoint":"vmsync-cpt-000002","checkpoint_at":1700003600,"taken_at":1700003700}`,
		markerStatusEnd,
	}, "\n")

	got, err := ParseAllStatus(out)
	if err != nil {
		t.Fatalf("ParseAllStatus: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("parsed %d sidecars, want 2: %v", len(got), got)
	}
	// taken_at, not checkpoint_at. They are close together on a healthy pair
	// -- the copy takes as long as it takes -- so a fixture that made them
	// far apart would be asserting against a store the system cannot produce.
	if at := got["1700000000-vmsync-cpt-000001"]; !at.Equal(time.Unix(1700000120, 0).UTC()) {
		t.Errorf("first point published at %v, want the sidecar's taken_at and not its checkpoint_at", at)
	}
	if at := got["1700003600-vmsync-cpt-000002"]; !at.Equal(time.Unix(1700003700, 0).UTC()) {
		t.Errorf("second point published at %v", at)
	}
}

// An absent store is an answer, not a failure: a pair that has never used
// -retention is the ordinary case, and it must not stop the prune.
func TestParseAllStatusTreatsAnAbsentStoreAsEmptyRatherThanBroken(t *testing.T) {
	got, err := ParseAllStatus(markerStatusNone + "\n")
	if err != nil {
		t.Errorf("an absent store was reported as an error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("parsed %v from an absent store", got)
	}
}

// Every way a record can be useless must leave it OUT of the map, not in it
// with a zero. A zero would read as "published in 1970" and make that point
// the first thing the next prune deletes.
func TestParseAllStatusOmitsRatherThanDefaults(t *testing.T) {
	cases := []struct {
		name string
		body string
		dir  string
	}{
		{"sidecar missing entirely", "", "1700000000-vmsync-cpt-000001"},
		{"sidecar not json", "cat: no such file", "1700000000-vmsync-cpt-000001"},
		{"taken_at absent", `{"checkpoint":"vmsync-cpt-000001","checkpoint_at":1700000000}`, "1700000000-vmsync-cpt-000001"},
		{"taken_at zero", `{"taken_at":0}`, "1700000000-vmsync-cpt-000001"},
		{"directory is not a tag", `{"taken_at":1699999000}`, "not-a-restore-point"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := strings.Join([]string{
				markerStatus,
				markerStatusPoint + " /data/replicas/.rp/web01/" + c.dir + "/",
				c.body,
				markerStatusEnd,
			}, "\n")
			got, err := ParseAllStatus(out)
			if err != nil {
				t.Fatalf("ParseAllStatus: %v", err)
			}
			if len(got) != 0 {
				t.Errorf("parsed %v, want nothing -- an unusable record must be absent, not zero", got)
			}
		})
	}
}

// One bad record must not take the others down with it, in EITHER direction.
//
// Both orderings, because they fail differently: a bad record last can only
// corrupt what is still to come, while a bad record first can only corrupt an
// empty map -- so a fixture with one ordering cannot see the other's bug.
func TestParseAllStatusKeepsTheGoodRecordsBesideABadOne(t *testing.T) {
	bad := []string{markerStatusPoint + " /x/1700000000-vmsync-cpt-000001/", "not json at all", markerStatusEnd}
	good := []string{markerStatusPoint + " /x/1700003600-vmsync-cpt-000002/", `{"taken_at":1700003700}`, markerStatusEnd}

	for _, c := range []struct {
		name  string
		lines []string
	}{
		{"bad record first", append(append([]string{markerStatus}, bad...), good...)},
		{"good record first", append(append([]string{markerStatus}, good...), bad...)},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseAllStatus(strings.Join(c.lines, "\n"))
			if err != nil {
				t.Fatalf("ParseAllStatus: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("parsed %v, want only the readable one", got)
			}
			if _, ok := got["1700003600-vmsync-cpt-000002"]; !ok {
				t.Errorf("the readable sidecar was lost alongside the unreadable one: %v", got)
			}
		})
	}
}

// A record that never gets its end marker must not swallow the next one.
//
// A truncated transfer is the ordinary way this happens, and without the
// flush at the start of each record the two bodies merge into one blob that
// decodes as neither -- so one truncation costs two publish instants and
// drops both points into the unknown tier.
func TestParseAllStatusRecoversFromAnUnterminatedRecord(t *testing.T) {
	out := strings.Join([]string{
		markerStatus,
		markerStatusPoint + " /x/1700000000-vmsync-cpt-000001/",
		`{"taken_at":1700000`, // cut mid-JSON, no end marker
		markerStatusPoint + " /x/1700003600-vmsync-cpt-000002/",
		`{"taken_at":1700003700}`,
		markerStatusEnd,
	}, "\n")

	got, err := ParseAllStatus(out)
	if err != nil {
		t.Fatalf("ParseAllStatus: %v", err)
	}
	if _, ok := got["1700003600-vmsync-cpt-000002"]; !ok {
		t.Errorf("the record after an unterminated one was lost: %v", got)
	}
	if _, ok := got["1700000000-vmsync-cpt-000001"]; ok {
		t.Errorf("a truncated record was accepted: %v", got)
	}
}

// A stream that ends without its final end marker must still yield the last
// record, which is the other flush() -- the one after the loop.
func TestParseAllStatusKeepsALastRecordThatEndsAtEOF(t *testing.T) {
	out := strings.Join([]string{
		markerStatus,
		markerStatusPoint + " /x/1700003600-vmsync-cpt-000002/",
		`{"taken_at":1700003700}`,
	}, "\n")

	got, err := ParseAllStatus(out)
	if err != nil {
		t.Fatalf("ParseAllStatus: %v", err)
	}
	if _, ok := got["1700003600-vmsync-cpt-000002"]; !ok {
		t.Errorf("the last record was dropped because the stream ended without an end marker: %v", got)
	}
}

func TestParseTargetNowReadsTheTargetsClock(t *testing.T) {
	at, err := ParseTargetNow(markerNow + "\n1700000000\n")
	if err != nil {
		t.Fatalf("ParseTargetNow: %v", err)
	}
	if !at.Equal(time.Unix(1700000000, 0).UTC()) {
		t.Errorf("read %v, want the unix time the target printed", at)
	}

	// Anything else is an error rather than a plausible instant: silently
	// returning the zero time, or this host's time, is how the stamp goes
	// back to sharing a clock with the checkpoint instant.
	for _, bad := range []string{"", "1700000000\n", markerNow + "\n", markerNow + "\ndate: command not found\n"} {
		if got, err := ParseTargetNow(bad); err == nil {
			t.Errorf("ParseTargetNow(%q) = %v, want an error", bad, got)
		}
	}
}
