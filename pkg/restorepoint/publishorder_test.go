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

	seq := map[string]int64{}
	var store []Tag
	// The ghosts: published for real in the three hours before the clock was
	// corrected, and so labelled two days hence. Their SEQUENCES are 1, 2, 3
	// -- they were published first, whatever any clock says.
	for i, ahead := range []time.Duration{48 * time.Hour, 47 * time.Hour, 46 * time.Hour} {
		g := Tag{At: start.Add(ahead), Checkpoint: fmt.Sprintf("ghost-%03d", i+1)}
		store = append(store, g)
		seq[g.String()] = int64(i + 1)
	}
	next := int64(len(store))

	const runs = 8
	for i := 1; i <= runs; i++ {
		now := start.Add(time.Duration(i) * time.Hour)
		fresh := Tag{At: now, Checkpoint: fmt.Sprintf("run-%03d", i)}
		next++
		seq[fresh.String()] = next

		plan := Prune(append(append([]Tag{}, store...), fresh), fresh, seq, policy)
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
	seq := map[string]int64{
		older.String(): 1,
		fresh.String(): 2,
	}

	plan := Prune([]Tag{ghost, older, fresh}, fresh, seq, Policy{Count: 2, Interval: time.Hour})

	if !inRemove(plan, ghost) {
		t.Errorf("the point with no publish sequence was kept over one that had it; Keep=%v Remove=%v", tagNames(plan.Keep), tagNames(plan.Remove))
	}
	if inRemove(plan, older) {
		t.Error("a point with a publish sequence was dropped in favour of one dated in the future by a clock")
	}
}

// With no sequences known, the order falls back to the tag instant -- a store
// this run learned nothing about is not a store to start reordering.
func TestPruneWithNoSequencesOrdersByTagInstant(t *testing.T) {
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
	seq := map[string]int64{
		a.String(): 1,
		b.String(): 2,
		// fresh deliberately absent
	}

	plan := Prune([]Tag{a, b, fresh}, fresh, seq, Policy{Count: 2, Interval: time.Hour})

	if inRemove(plan, fresh) {
		t.Error("the point this run just published was deleted because its sidecar was not readable yet")
	}
	if !inRemove(plan, a) {
		t.Errorf("the oldest point was not the one dropped; Remove=%v", tagNames(plan.Remove))
	}
}

// tagsOf parses directory names into the tags a caller would have got from a
// listing, so a fixture asks about exactly what it then answers with.
func tagsOf(t *testing.T, names ...string) []Tag {
	t.Helper()
	out := make([]Tag, 0, len(names))
	for _, n := range names {
		tag, err := ParseTag(n)
		if err != nil {
			t.Fatalf("ParseTag(%q): %v", n, err)
		}
		out = append(out, tag)
	}
	return out
}

func TestReadAllStatusCommandIsOneRoundTripAndSurvivesAMissingSidecar(t *testing.T) {
	s, err := StoreForDir("/data/replicas", "web01")
	if err != nil {
		t.Fatalf("StoreForDir: %v", err)
	}
	asked := tagsOf(t, "1700000000-vmsync-cpt-000001", "1700003600-vmsync-cpt-000002")
	cmd, err := ReadAllStatusCommand(s, asked)
	if err != nil {
		t.Fatalf("ReadAllStatusCommand: %v", err)
	}
	root, _ := s.Path()
	if !strings.Contains(cmd, root) {
		t.Errorf("the command does not name the store root %q: %s", root, cmd)
	}
	for _, tag := range asked {
		if !strings.Contains(cmd, tag.String()) {
			t.Errorf("the command does not ask about %s: %s", tag, cmd)
		}
	}
	// A point with no sidecar must not fail the whole read: the caller needs
	// the other points' instants, and "this one has none" is an answer.
	if !strings.Contains(cmd, "2>/dev/null") {
		t.Errorf("an unreadable sidecar would fail the command: %s", cmd)
	}
	// One round trip, and NO GLOB. The glob is what put a filesystem path on
	// the record header; see ReadAllStatusCommand.
	if strings.Contains(cmd, "for d in") || strings.Contains(cmd, "/*/") {
		t.Errorf("the command still iterates the directory itself: %s", cmd)
	}
	if n := strings.Count(cmd, markerStatusPoint); n != len(asked)*2 {
		// Once in the printf argument and once in its shQuote'd form per
		// point -- the count is a cheap way to notice a record going missing.
		t.Logf("marker occurrences: %d (command: %s)", n, cmd)
	}
	// The tag goes through printf as an argument, never inside the format.
	if !strings.Contains(cmd, `printf '%s %s\n' `) {
		t.Errorf("the record header is not emitted through printf with the value as an argument: %s", cmd)
	}
}

// A STORE PATH CANNOT BREAK THE RECORD FRAMING.
//
// The framing is newline-delimited, so whatever sits on a header line decides
// where one record ends and the next begins. An earlier version globbed the
// directory and put the filesystem path there -- and nothing rejects a
// newline in -target-disk-path or -target-domain (store.go's SafeKey says so
// outright). One newline split every header, every record was dropped, and
// ParseAllStatus had still seen its opening marker: empty map, no error, the
// caller read it as "no point has a publish instant", and retention fell back
// to the checkpoint instant. That is audit item 4.9 restored in full with
// every number healthy.
//
// Naming the points is what fixes it: a Tag is digits, a hyphen and a
// checkpoint name checkpointRe has constrained to [A-Za-z0-9._-].
func TestAHostileStorePathCannotBreakTheFraming(t *testing.T) {
	for _, dir := range []string{
		"/data/rep\nlicas",
		"/data/replicas\n" + markerStatusPoint + " 1700009999-vmsync-cpt-000099",
		"/data/rep'licas",
		"/data/rep%slicas",
		"/data/rep\\nlicas",
	} {
		s, err := StoreForDir(dir, "web01")
		if err != nil {
			continue // refused upstream, which is also a fine answer
		}
		asked := tagsOf(t, "1700000000-vmsync-cpt-000001")
		cmd, err := ReadAllStatusCommand(s, asked)
		if err != nil {
			continue
		}
		// Exactly one record header, whatever the path contains.
		if n := strings.Count(cmd, "printf '%s %s\\n' "); n != 1 {
			t.Errorf("store dir %q produced %d record headers, want 1: %s", dir, n, cmd)
		}
		// And the header's value is the tag, not anything from the path.
		if !strings.Contains(cmd, shQuote("1700000000-vmsync-cpt-000001")) {
			t.Errorf("store dir %q: the header does not carry the tag: %s", dir, cmd)
		}
	}
}

// The two readers must not accept each other's output.
func TestTheListingAndTheSidecarReadRejectEachOther(t *testing.T) {
	s, err := StoreForDir("/data/replicas", "web01")
	if err != nil {
		t.Fatalf("StoreForDir: %v", err)
	}
	asked := tagsOf(t, "1700000000-vmsync-cpt-000001")
	listCmd, _ := ListCommand(s)
	scCmd, _ := ReadAllStatusCommand(s, asked)
	if strings.Contains(scCmd, markerListing) || strings.Contains(scCmd, markerListingNone) {
		t.Errorf("the sidecar read is bracketed by the listing's markers: %s", scCmd)
	}
	if strings.Contains(listCmd, markerStatus) {
		t.Errorf("the listing is bracketed by the sidecar read's marker: %s", listCmd)
	}

	listOut := markerListing + "\n1700000000-vmsync-cpt-000001/\n"
	if got, err := ParseAllStatus(listOut, asked); err == nil {
		t.Errorf("ParseAllStatus accepted a listing and returned %v; a crossed wire must be an error, not a store with no publish instants", got)
	}

	scOut := markerStatus + "\n" + markerStatusPoint + " 1700000000-vmsync-cpt-000001\n{\"taken_at\":1}\n" + markerStatusEnd + "\n"
	if l, err := ParseListing(scOut); err == nil {
		t.Errorf("ParseListing accepted a sidecar dump and returned %+v", l)
	}
	if l, err := ParseListing(markerListing + "\n1700000000-vmsync-cpt-000001/\n"); err != nil || len(l.Points) != 1 {
		t.Errorf("ParseListing rejected its own output: %+v %v", l, err)
	}
	if _, err := ParseListing(markerListingNone + "\n"); err != nil {
		t.Errorf("an absent store was reported as an error: %v", err)
	}
}

func TestParseAllStatusReadsEachPointsOwnInstant(t *testing.T) {
	asked := tagsOf(t, "1700000000-vmsync-cpt-000001", "1700003600-vmsync-cpt-000002")
	out := strings.Join([]string{
		markerStatus,
		markerStatusPoint + " 1700000000-vmsync-cpt-000001",
		`{"checkpoint":"vmsync-cpt-000001","checkpoint_at":1700000000,"taken_at":1700000120,"publish_seq":7}`,
		markerStatusEnd,
		markerStatusPoint + " 1700003600-vmsync-cpt-000002",
		`{"checkpoint":"vmsync-cpt-000002","checkpoint_at":1700003600,"taken_at":1700003700,"publish_seq":8}`,
		markerStatusEnd,
	}, "\n")

	got, err := ParseAllStatus(out, asked)
	if err != nil {
		t.Fatalf("ParseAllStatus: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("parsed %d sidecars, want 2: %v", len(got), got)
	}
	// Each point's own sidecar, whole: the publish path wants the sequence
	// and the prune path wants it too, so one read serves both.
	if st := got["1700000000-vmsync-cpt-000001"]; st.PublishSeq != 7 || st.TakenAt != 1700000120 {
		t.Errorf("first point = %+v, want publish_seq 7 and taken_at 1700000120", st)
	}
	if st := got["1700003600-vmsync-cpt-000002"]; st.PublishSeq != 8 || st.TakenAt != 1700003700 {
		t.Errorf("second point = %+v, want publish_seq 8 and taken_at 1700003700", st)
	}
}

// A SIDECAR CANNOT SPEAK FOR A POINT THAT IS NOT ITS OWN.
//
// The framing is in-band, so a line inside a body that begins with the point
// marker opens a new record. Position decides which point a record is about,
// and an injected header shifts every record after it -- so the honest answer
// is to refuse the whole read, which makes the caller withhold the prune
// rather than delete by an order it cannot account for.
//
// Both directions, because only one of them used to be caught. An earlier
// version kept a set of asked-for names and took the FIRST record per point,
// which stops a forgery that follows the genuine record but not one that
// precedes it. Preceding is the reachable direction: the command asks in
// listing order and a tag starts with unix seconds, so the oldest point is
// read first and its body can speak for every point after it.
func TestParseAllStatusRefusesAForgedRecord(t *testing.T) {
	asked := tagsOf(t, "1700000000-vmsync-cpt-000001", "1700003600-vmsync-cpt-000002")
	genuine1 := []string{markerStatusPoint + " 1700000000-vmsync-cpt-000001", `{"taken_at":1700000120,"publish_seq":1}`}
	genuine2 := []string{markerStatusPoint + " 1700003600-vmsync-cpt-000002", `{"taken_at":1700003700,"publish_seq":2}`}
	forgery := []string{markerStatusPoint + " 1700003600-vmsync-cpt-000002", `{"taken_at":4000000000,"publish_seq":9999}`}

	for _, c := range []struct {
		name  string
		lines []string
	}{
		{
			// The oldest point's body speaks for its neighbour, BEFORE that
			// neighbour's own record is read.
			"forged record precedes the genuine one",
			append(append(append([]string{markerStatus}, genuine1...), append(forgery, markerStatusEnd)...), append(genuine2, markerStatusEnd)...),
		},
		{
			"forged record follows the genuine one",
			append(append(append([]string{markerStatus}, genuine1...), markerStatusEnd), append(append(genuine2, forgery...), markerStatusEnd)...),
		},
		{
			"a record for a point nobody listed",
			append(append(append(append([]string{markerStatus}, genuine1...), markerStatusEnd),
				append(genuine2, markerStatusEnd)...),
				markerStatusPoint+" 1700009999-vmsync-cpt-000099", `{"taken_at":4000000001,"publish_seq":9999}`, markerStatusEnd),
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseAllStatus(strings.Join(c.lines, "\n"), asked)
			if err == nil {
				t.Fatalf("ParseAllStatus accepted a stream carrying a forged record and returned %v. The caller would then prune by an order a sidecar had chosen for it", got)
			}
			if got != nil {
				t.Errorf("a refused read still returned %v", got)
			}
		})
	}
}

// The ordinary stream is still accepted, so the refusal above is not simply
// refusing everything.
func TestParseAllStatusAcceptsTheAnswerItAskedFor(t *testing.T) {
	asked := tagsOf(t, "1700000000-vmsync-cpt-000001", "1700003600-vmsync-cpt-000002")
	out := strings.Join([]string{
		markerStatus,
		markerStatusPoint + " 1700000000-vmsync-cpt-000001",
		`{"taken_at":1700000120,"publish_seq":1}`,
		markerStatusEnd,
		markerStatusPoint + " 1700003600-vmsync-cpt-000002",
		`{"taken_at":1700003700,"publish_seq":2}`,
		markerStatusEnd,
	}, "\n")
	got, err := ParseAllStatus(out, asked)
	if err != nil {
		t.Fatalf("ParseAllStatus: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("parsed %v, want both points", got)
	}
}

// Records must line up with what was asked, in order -- an answer that names
// the right points in the wrong order is an answer to a different question.
func TestParseAllStatusRefusesRecordsOutOfOrder(t *testing.T) {
	asked := tagsOf(t, "1700000000-vmsync-cpt-000001", "1700003600-vmsync-cpt-000002")
	out := strings.Join([]string{
		markerStatus,
		markerStatusPoint + " 1700003600-vmsync-cpt-000002",
		`{"taken_at":1700003700}`,
		markerStatusEnd,
		markerStatusPoint + " 1700000000-vmsync-cpt-000001",
		`{"taken_at":1700000120,"publish_seq":1}`,
		markerStatusEnd,
	}, "\n")
	if got, err := ParseAllStatus(out, asked); err == nil {
		t.Errorf("ParseAllStatus accepted records in a different order from the question and returned %v", got)
	}
}

// A RECORD THIS FUNCTION CANNOT READ IS ABSENT, never present with zeroes.
//
// Absent and "present, saying nothing" are different facts and the callers
// treat them differently: the prune path omits a point with no sequence from
// the order, and the publish path takes the maximum over whatever it got. A
// record defaulted to zeroes would be indistinguishable from a real sidecar
// whose fields are genuinely absent, and a zero sequence is not a place in
// the order.
func TestParseAllStatusOmitsRecordsItCannotRead(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"sidecar missing entirely", ""},
		{"sidecar not json", "cat: no such file"},
		{"sidecar truncated mid-json", `{"publish_seq":4`},
	}
	asked := tagsOf(t, "1700000000-vmsync-cpt-000001")
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := strings.Join([]string{
				markerStatus,
				markerStatusPoint + " 1700000000-vmsync-cpt-000001",
				c.body,
				markerStatusEnd,
			}, "\n")
			got, err := ParseAllStatus(out, asked)
			if err != nil {
				t.Fatalf("ParseAllStatus: %v", err)
			}
			if len(got) != 0 {
				t.Errorf("parsed %v, want nothing -- a record that could not be read must be absent", got)
			}
		})
	}
}

// A sidecar written before publish_seq existed reads back as a sequence of
// zero, which is what the prune path filters on. Present, not absent: the
// record WAS readable, and its other fields are good.
func TestParseAllStatusKeepsASidecarWithNoSequence(t *testing.T) {
	asked := tagsOf(t, "1700000000-vmsync-cpt-000001")
	out := strings.Join([]string{
		markerStatus,
		markerStatusPoint + " 1700000000-vmsync-cpt-000001",
		`{"checkpoint":"vmsync-cpt-000001","taken_at":1700000120}`,
		markerStatusEnd,
	}, "\n")
	got, err := ParseAllStatus(out, asked)
	if err != nil {
		t.Fatalf("ParseAllStatus: %v", err)
	}
	st, ok := got["1700000000-vmsync-cpt-000001"]
	if !ok {
		t.Fatalf("a readable sidecar with no sequence was dropped: %v", got)
	}
	if st.PublishSeq != 0 {
		t.Errorf("publish_seq = %d, want 0 for a sidecar that predates the field", st.PublishSeq)
	}
	if st.TakenAt != 1700000120 {
		t.Errorf("the rest of the sidecar was lost: %+v", st)
	}
}

// MaxPublishSeq is what a publishing run adds one to.
func TestMaxPublishSeq(t *testing.T) {
	if got := MaxPublishSeq(nil); got != 0 {
		t.Errorf("MaxPublishSeq(nil) = %d, want 0 so the first point published is 1", got)
	}
	if got := MaxPublishSeq(map[string]Status{"a": {}, "b": {}}); got != 0 {
		t.Errorf("a store whose points predate the field = %d, want 0", got)
	}
	// The maximum over the whole store, not the last one seen: map iteration
	// has no order, and the newest BY SEQUENCE is the highest by definition.
	got := MaxPublishSeq(map[string]Status{
		"a": {PublishSeq: 3},
		"b": {PublishSeq: 41},
		"c": {PublishSeq: 7},
		"d": {},
	})
	if got != 41 {
		t.Errorf("MaxPublishSeq = %d, want 41", got)
	}
}

// One bad record must not take the others down with it, in EITHER direction.
func TestParseAllStatusKeepsTheGoodRecordsBesideABadOne(t *testing.T) {
	bad := []string{markerStatusPoint + " 1700000000-vmsync-cpt-000001", "not json at all", markerStatusEnd}
	good := []string{markerStatusPoint + " 1700003600-vmsync-cpt-000002", `{"taken_at":1700003700,"publish_seq":2}`, markerStatusEnd}

	// Each ordering asks in the order it answers: records are correlated by
	// position now, so a stream whose records are in a different order from
	// the question is a different -- and refused -- case, covered by
	// TestParseAllStatusRefusesRecordsOutOfOrder.
	for _, c := range []struct {
		name  string
		asked []Tag
		lines []string
	}{
		{"bad record first",
			tagsOf(t, "1700000000-vmsync-cpt-000001", "1700003600-vmsync-cpt-000002"),
			append(append([]string{markerStatus}, bad...), good...)},
		{"good record first",
			tagsOf(t, "1700003600-vmsync-cpt-000002", "1700000000-vmsync-cpt-000001"),
			append(append([]string{markerStatus}, good...), bad...)},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseAllStatus(strings.Join(c.lines, "\n"), c.asked)
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
func TestParseAllStatusRecoversFromAnUnterminatedRecord(t *testing.T) {
	asked := tagsOf(t, "1700000000-vmsync-cpt-000001", "1700003600-vmsync-cpt-000002")
	out := strings.Join([]string{
		markerStatus,
		markerStatusPoint + " 1700000000-vmsync-cpt-000001",
		`{"taken_at":1700000`, // cut mid-JSON, no end marker
		markerStatusPoint + " 1700003600-vmsync-cpt-000002",
		`{"taken_at":1700003700}`,
		markerStatusEnd,
	}, "\n")

	got, err := ParseAllStatus(out, asked)
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
	asked := tagsOf(t, "1700003600-vmsync-cpt-000002")
	out := strings.Join([]string{
		markerStatus,
		markerStatusPoint + " 1700003600-vmsync-cpt-000002",
		`{"taken_at":1700003700}`,
	}, "\n")

	got, err := ParseAllStatus(out, asked)
	if err != nil {
		t.Fatalf("ParseAllStatus: %v", err)
	}
	if _, ok := got["1700003600-vmsync-cpt-000002"]; !ok {
		t.Errorf("the last record was dropped because the stream ended without an end marker: %v", got)
	}
}
