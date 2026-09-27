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

package actionlog

import (
	"strings"
	"testing"
)

// intentLine and outcomeLine build fixture journals the way the writer does,
// so the reader is tested against what is actually on a target rather than
// against a hand-written approximation of it.
func intentLine(aid string, seq int, at int64, verb string, detail map[string]string) string {
	return string(Record{
		Kind: KindIntent, Action: aid, Seq: seq, At: at, Verb: verb,
		Domain: "web01", Host: "hv-a", PID: 4242, Detail: detail,
	}.Encode())
}

func outcomeLine(aid string, seq int, at int64, verb, result string, exit int, errText string) string {
	return string(Record{
		Kind: KindOutcome, Action: aid, Seq: seq, At: at, Verb: verb,
		Domain: "web01", Host: "hv-a", PID: 4242,
		Result: result, Exit: exit, Err: errText,
	}.Encode())
}

func TestReadJoinsAnIntentToItsOutcome(t *testing.T) {
	live := intentLine("aaa", 1, 1758441600, "sync", map[string]string{"mode": "reinit"}) +
		outcomeLine("aaa", 1, 1758445200, "sync", ResultOK, 0, "")

	got := Read([]byte(live))
	if got.Records != 2 {
		t.Fatalf("read %d records, want 2", got.Records)
	}
	if len(got.Actions) != 1 {
		t.Fatalf("joined into %d actions, want 1", len(got.Actions))
	}
	a := got.Actions[0]
	if !a.HasIntent || !a.HasOutcome || a.Unfinished() || a.Orphaned() {
		t.Fatalf("the pair did not join: %+v", a)
	}
	if a.ActionID != "aaa" || a.Seq != 1 || a.Verb() != "sync" || a.StartedAt() != 1758441600 {
		t.Errorf("wrong action: %+v", a)
	}
}

// The finding the whole journal exists to make possible: a run that recorded
// what it was about to do and never came back.
func TestReadReportsAnIntentWithNoOutcomeAsExactlyThat(t *testing.T) {
	live := intentLine("aaa", 1, 1758441600, "sync", nil) +
		outcomeLine("aaa", 1, 1758445200, "sync", ResultOK, 0, "") +
		intentLine("bbb", 1, 1758448800, "sync", map[string]string{"mode": "reinit"})

	got := Read([]byte(live))
	unfinished := got.Unfinished()
	if len(unfinished) != 1 {
		t.Fatalf("found %d unfinished actions, want 1: %+v", len(unfinished), got.Actions)
	}
	if unfinished[0].ActionID != "bbb" {
		t.Errorf("the wrong action is reported unfinished: %+v", unfinished[0])
	}
	if unfinished[0].HasOutcome {
		t.Error("an unfinished action must not claim an outcome")
	}
	// It must NOT be invented as a failure. The journal says the run stopped
	// without saying how, and that is a different thing from a run that failed.
	if unfinished[0].Outcome.Result != "" {
		t.Errorf("an outcome was invented: %q", unfinished[0].Outcome.Result)
	}
}

// One rotated generation plus the live file, oldest first, so the order the
// reader reports is the order things happened.
func TestReadSpansTheRotatedGenerationInOrder(t *testing.T) {
	rotated := intentLine("old", 1, 1758400000, "sync", nil) +
		outcomeLine("old", 1, 1758400100, "sync", ResultOK, 0, "")
	live := intentLine("new", 1, 1758441600, "promote", nil) +
		outcomeLine("new", 1, 1758441610, "promote", ResultOK, 0, "")

	got := Read([]byte(rotated), []byte(live))
	if len(got.Actions) != 2 {
		t.Fatalf("read %d actions, want 2", len(got.Actions))
	}
	if got.Actions[0].ActionID != "old" || got.Actions[1].ActionID != "new" {
		t.Errorf("out of order: %q then %q", got.Actions[0].ActionID, got.Actions[1].ActionID)
	}
}

// An action that straddled a rotation loses its older half. Ordinary, not
// alarming, and named so a reader is not left wondering whether the intent was
// never written at all.
func TestReadReportsAnOutcomeWhoseIntentIsGone(t *testing.T) {
	live := outcomeLine("aaa", 1, 1758445200, "sync", ResultOK, 0, "")
	got := Read([]byte(live))
	if len(got.Actions) != 1 {
		t.Fatalf("read %d actions, want 1", len(got.Actions))
	}
	a := got.Actions[0]
	if !a.Orphaned() || a.Unfinished() {
		t.Errorf("an outcome with no intent must be orphaned, not unfinished: %+v", a)
	}
	if a.Verb() != "sync" || a.StartedAt() != 1758445200 {
		t.Errorf("an orphaned outcome must still describe itself: %+v", a)
	}
}

// The power cut, exactly: the last append was interrupted, so the final line
// is half a record. It costs that one record and nothing else -- which is the
// property JSON Lines was chosen for.
func TestReadToleratesATornFinalLine(t *testing.T) {
	complete := intentLine("aaa", 1, 1758441600, "sync", nil) +
		outcomeLine("aaa", 1, 1758445200, "sync", ResultOK, 0, "") +
		intentLine("bbb", 1, 1758448800, "sync", nil)
	torn := complete + `{"v":1,"k":"outcome","aid":"bbb","seq":1,"at":175844`

	got := Read([]byte(torn))
	if got.Records != 3 {
		t.Fatalf("read %d whole records, want 3", got.Records)
	}
	if len(got.Torn) != 1 {
		t.Fatalf("reported %d torn lines, want 1: %+v", len(got.Torn), got.Torn)
	}
	if got.Torn[0].Line != 4 || got.Torn[0].Generation != 0 {
		t.Errorf("torn line is reported at generation %d line %d, want 0/4", got.Torn[0].Generation, got.Torn[0].Line)
	}
	// Everything before it still read, and the unfinished intent is still the
	// finding: a torn outcome and a missing one look the same from here, and
	// both mean "this run did not record finishing".
	if len(got.Unfinished()) != 1 || got.Unfinished()[0].ActionID != "bbb" {
		t.Errorf("a torn final line lost the records before it: %+v", got.Actions)
	}
}

// A torn line in the MIDDLE is tolerated too -- two writers interleaving, or a
// filesystem that returned garbage for one block.
func TestReadToleratesATornLineInTheMiddle(t *testing.T) {
	content := intentLine("aaa", 1, 1758441600, "sync", nil) +
		"{\"v\":1,\"k\":\"outc\n" +
		outcomeLine("aaa", 1, 1758445200, "sync", ResultOK, 0, "")
	got := Read([]byte(content))
	if got.Records != 2 || len(got.Torn) != 1 {
		t.Fatalf("read %d records and %d torn, want 2 and 1", got.Records, len(got.Torn))
	}
	if !got.Actions[0].HasIntent || !got.Actions[0].HasOutcome {
		t.Errorf("a torn line in the middle broke the join: %+v", got.Actions[0])
	}
}

// A journal directory somebody pointed at a binary would otherwise produce one
// torn entry per line. The count that matters is "a lot"; the first few say
// what kind.
func TestReadBoundsWhatItReportsAboutAnUnreadableFile(t *testing.T) {
	var b strings.Builder
	for i := 0; i < maxTornReported*4; i++ {
		b.WriteString("this is not a record at all\n")
	}
	got := Read([]byte(b.String()))
	if got.Records != 0 {
		t.Errorf("read %d records out of garbage", got.Records)
	}
	if len(got.Torn) != maxTornReported {
		t.Errorf("reported %d torn lines, want the bound of %d", len(got.Torn), maxTornReported)
	}
}

// A torn line is untrusted content from a file anybody with write access to
// the replica directory can put anything in, and it gets printed.
func TestReadClipsWhatATornLineContributes(t *testing.T) {
	got := Read([]byte(strings.Repeat("x", 5000) + "\n"))
	if len(got.Torn) != 1 {
		t.Fatalf("want one torn line, got %d", len(got.Torn))
	}
	if len(got.Torn[0].Text) > tornTextMax {
		t.Errorf("a torn line contributed %d bytes to the report", len(got.Torn[0].Text))
	}
}

// Blank lines and the empty field after the final newline are not torn.
func TestReadIgnoresBlankLines(t *testing.T) {
	content := "\n" + intentLine("aaa", 1, 1758441600, "sync", nil) + "\n   \n" +
		outcomeLine("aaa", 1, 1758445200, "sync", ResultOK, 0, "") + "\n"
	got := Read([]byte(content))
	if len(got.Torn) != 0 {
		t.Errorf("blank lines were reported as torn: %+v", got.Torn)
	}
	if got.Records != 2 || len(got.Actions) != 1 {
		t.Errorf("read %d records into %d actions, want 2 into 1", got.Records, len(got.Actions))
	}
}

func TestReadOfNothingIsEmptyRatherThanAnError(t *testing.T) {
	got := Read(nil, []byte{})
	if got.Records != 0 || len(got.Actions) != 0 || len(got.Torn) != 0 {
		t.Errorf("an absent journal read as %+v", got)
	}
	if len(got.Unfinished()) != 0 {
		t.Error("an absent journal reported unfinished actions")
	}
}

// A step record from a newer vmsync is counted but joins nothing, so it can
// never turn into an action nobody performed.
func TestReadCountsButDoesNotActOnAKindItDoesNotKnow(t *testing.T) {
	content := intentLine("aaa", 1, 1758441600, "sync", nil) +
		`{"v":1,"k":"step","aid":"aaa","seq":1,"at":1758441700,"verb":"sync","step":"copy-disk","res":"","exit":0}` + "\n" +
		outcomeLine("aaa", 1, 1758445200, "sync", ResultOK, 0, "")
	got := Read([]byte(content))
	if got.Records != 3 {
		t.Errorf("read %d records, want 3", got.Records)
	}
	if len(got.Actions) != 1 {
		t.Fatalf("a step record created %d extra actions", len(got.Actions)-1)
	}
	if !got.Actions[0].HasIntent || !got.Actions[0].HasOutcome {
		t.Errorf("a step record disturbed the join: %+v", got.Actions[0])
	}
}

// Two actions of one process share an id and are told apart by seq. Joining on
// the id alone would pair one run's intent with another run's outcome, which
// is the one way this reader could actively mislead.
func TestReadNeverJoinsAcrossSeq(t *testing.T) {
	content := intentLine("aaa", 1, 1758441600, "sync", nil) +
		outcomeLine("aaa", 1, 1758441700, "sync", ResultFailed, 1, "verify mismatch") +
		intentLine("aaa", 2, 1758441800, "sync", map[string]string{"mode": "repair"})

	got := Read([]byte(content))
	if len(got.Actions) != 2 {
		t.Fatalf("folded two actions into %d", len(got.Actions))
	}
	if got.Actions[0].Outcome.Result != ResultFailed {
		t.Errorf("the first action lost its outcome: %+v", got.Actions[0])
	}
	unfinished := got.Unfinished()
	if len(unfinished) != 1 || unfinished[0].Seq != 2 {
		t.Errorf("the repair pass is not reported unfinished: %+v", got.Actions)
	}
}

// A record with no action id is still readable, and still joins to its own
// outcome. Not every caller has a correlation id, and a record with none is
// far better than no record.
func TestReadJoinsRecordsWithNoActionID(t *testing.T) {
	content := intentLine("", 1, 1758441600, "sync", nil) +
		outcomeLine("", 1, 1758445200, "sync", ResultOK, 0, "")
	got := Read([]byte(content))
	if len(got.Actions) != 1 || !got.Actions[0].HasIntent || !got.Actions[0].HasOutcome {
		t.Errorf("records with no action id did not join: %+v", got.Actions)
	}
}
