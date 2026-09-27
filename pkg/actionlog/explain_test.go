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
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// -update rewrites the golden files. Present because the alternative is
// hand-editing column-aligned text, which is how a golden test ends up being
// changed to match a regression instead of catching one. Review the DIFF, not
// the regeneration.
var updateGolden = flag.Bool("update", false, "rewrite the golden files in testdata")

// interruptedJournal is the CI-02 shape, as a real journal on a real target:
// one rotated generation holding a sync that completed, then a live file
// holding the reinit that armed replica_incomplete and never came back, a
// promotion that was refused because of it, and -- last -- a half-written line
// from an append a power cut interrupted.
func interruptedJournal() (rotated, live []byte) {
	rotated = []byte(
		intentLine("7a1c9e40b2d35f61", 1, 1758400000, "sync", map[string]string{"mode": "incremental"}) +
			outcomeLine("7a1c9e40b2d35f61", 1, 1758400240, "sync", ResultOK, 0, ""))
	live = []byte(
		intentLine("9f3c1a2b4d5e6f70", 1, 1758441600, "sync", map[string]string{"mode": "reinit"}) +
			intentLine("c0ffee1234567890", 1, 1758445200, "promote", nil) +
			outcomeLine("c0ffee1234567890", 1, 1758445205, "promote", ResultFailed, 1,
				"refusing to promote web01: a full copy was interrupted") +
			`{"v":1,"k":"outcome","aid":"c0ffee1234567890","seq":1,"at":17584`)
	return rotated, live
}

func interruptedExplanation() Explanation {
	rotated, live := interruptedJournal()
	return Explanation{
		Domain:            "web01",
		URI:               "qemu:///system",
		JournalPath:       "/data/replicas/.vmsync-journal/web01.jsonl",
		ReplicaIncomplete: "verb=reinit,at=1758441600,action=9f3c1a2b4d5e6f70,host=hv-a,aside=1758441600",
		ReplicaIncompleteNote: "a reinit started on hv-a at 2026-09-21 08:00:00 UTC and never recorded finishing; " +
			"the metadata above these disks describes the replica that copy REPLACED, and the complete copy " +
			"is in the .vmsync-replaced-1758441600 files beside them",
		Reading: Read(rotated, live),
		Limit:   10,
	}
}

// The exact bytes an operator reads during an incident. Pinned, because this
// is the one output in vmsync whose audience is a person under time pressure
// who has never read the code: a column that silently shifts or a sentence
// that silently goes missing is a real cost, and nothing else would catch it.
func TestExplainGoldenInterruptedCopy(t *testing.T) {
	assertGolden(t, "explain-interrupted.golden", render(interruptedExplanation()))
}

// The ordinary case, which is most of them: a healthy replica with nothing to
// report. It has to be readable too, and it has to say plainly that there is
// no marker rather than leaving a blank where one would be.
func TestExplainGoldenNothingToReport(t *testing.T) {
	e := Explanation{
		Domain:      "db02",
		URI:         "qemu:///system",
		JournalPath: "/data/replicas/.vmsync-journal/db02.jsonl",
		JournalNote: "no journal has been written for this domain yet",
	}
	assertGolden(t, "explain-empty.golden", render(e))
}

// THE WORST CASE: -force-clean undefined the target and then died part-way
// through replacing its disks, so there is no domain, no metadata and
// therefore no marker -- and the journal is the only artefact left. "(none)"
// would read as a clean bill of health for exactly the replica that is not
// one, so the note takes that line over.
func TestExplainGoldenNoDomainLeft(t *testing.T) {
	live := []byte(intentLine("9f3c1a2b4d5e6f70", 1, 1758441600, "sync", map[string]string{"mode": "force-clean"}))
	e := Explanation{
		Domain:      "web01",
		URI:         "qemu:///system",
		JournalPath: "/data/replicas/.vmsync-journal/web01.jsonl",
		DomainNote: "There is NO DOMAIN by this name on this host, so there is no metadata to carry a marker " +
			"-- which is not a clean bill of health. A -force-clean undefines the target before it replaces " +
			"the disks, so a run killed in that window leaves the disks and this journal with nothing above them.",
		Reading: Read(nil, live),
		Limit:   10,
	}
	assertGolden(t, "explain-no-domain.golden", render(e))
}

// The note must displace "(none)" rather than sit beside it, because the two
// say opposite things about the same replica.
func TestExplainDoesNotClaimANoneMarkerWhenThereIsNoDomain(t *testing.T) {
	out := render(Explanation{Domain: "web01", DomainNote: "there is no domain here"})
	if strings.Contains(out, "carries no record of an interrupted full copy") {
		t.Errorf("a missing domain was reported as a domain with a clean marker:\n%s", out)
	}
	if !strings.Contains(out, "there is no domain here") {
		t.Errorf("the note is missing:\n%s", out)
	}
}

// The two things a reader must be able to find without knowing the format.
func TestExplainNamesTheFindingInWords(t *testing.T) {
	out := render(interruptedExplanation())
	for _, want := range []string{
		"UNFINISHED",
		"recorded an intent and NEVER an outcome",
		"replica_incomplete",
		".vmsync-replaced-1758441600",
		"evidence, never an input to a decision",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not say %q:\n%s", want, out)
		}
	}
}

// Printing must change nothing, and the report has to say so: an operator
// running this during an incident needs to know it is safe to run.
func TestExplainSaysItChangedNothing(t *testing.T) {
	if !strings.Contains(render(interruptedExplanation()), "Nothing was changed by printing it") {
		t.Error("the report does not say that reading it changed nothing")
	}
}

// Deterministic: two runs over one journal produce identical bytes, so an
// operator can diff them and a support case can quote them.
func TestExplainIsDeterministic(t *testing.T) {
	first := render(interruptedExplanation())
	for i := 0; i < 10; i++ {
		if got := render(interruptedExplanation()); got != first {
			t.Fatal("the report is not deterministic")
		}
	}
}

// Limit keeps the NEWEST actions, because during an incident the question is
// what happened last. Dropping from the wrong end would hide the run that
// armed the marker being asked about.
func TestExplainLimitKeepsTheNewestActions(t *testing.T) {
	var live string
	for i := 1; i <= 5; i++ {
		live += intentLine("aid", i, int64(1758400000+i), "sync", nil)
		live += outcomeLine("aid", i, int64(1758400000+i+1), "sync", ResultOK, 0, "")
	}
	e := Explanation{Domain: "web01", Reading: Read([]byte(live)), Limit: 2}
	out := render(e)
	if !strings.Contains(out, "(2 of 5)") {
		t.Errorf("the report does not say how much it is showing:\n%s", out)
	}
	// The oldest two are the ones that must be gone.
	if strings.Contains(out, stamp(1758400001)) || strings.Contains(out, stamp(1758400002)) {
		t.Errorf("the limit dropped the newest actions instead of the oldest:\n%s", out)
	}
	if !strings.Contains(out, stamp(1758400005)) {
		t.Errorf("the newest action is missing:\n%s", out)
	}
}

func TestExplainWithNoLimitShowsEverything(t *testing.T) {
	var live string
	for i := 1; i <= 4; i++ {
		live += intentLine("aid", i, int64(1758400000+i), "sync", nil)
	}
	out := render(Explanation{Domain: "web01", Reading: Read([]byte(live))})
	if !strings.Contains(out, "(4 of 4)") {
		t.Errorf("a zero limit must show everything:\n%s", out)
	}
}

// A detail map renders in sorted key order, or two runs of this command
// disagree about a journal that did not change.
func TestExplainRendersDetailsInSortedOrder(t *testing.T) {
	got := detailSuffix(map[string]string{"zz": "1", "aa": "2", "mm": "3"})
	if got != " {aa=2 mm=3 zz=1}" {
		t.Errorf("detailSuffix = %q, want sorted", got)
	}
	if detailSuffix(nil) != "" {
		t.Error("an absent detail map must render as nothing at all")
	}
}

// UTC always. This is read next to timestamps written by the OTHER host, and
// shifting one of them into the reader's timezone would make two clocks that
// agree look like they do not.
func TestStampIsUTC(t *testing.T) {
	if got := stamp(1758441600); got != "2025-09-21 08:00:00" {
		t.Errorf("stamp = %q, want the UTC rendering", got)
	}
	if got := stamp(0); got != "(no time)" {
		t.Errorf("a record with no time rendered as %q", got)
	}
}

func render(e Explanation) string {
	var b strings.Builder
	e.Render(&b)
	return b.String()
}

func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if *updateGolden {
		if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		t.Logf("rewrote %s", p)
		return
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v -- run go test ./pkg/actionlog -update to create it, then READ the result before committing it", p, err)
	}
	// Normalised, because git on a Windows checkout may have rewritten the
	// golden file's line endings and the report itself never emits a CR.
	if normalise(got) != normalise(string(want)) {
		t.Errorf("the report changed.\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
}

func normalise(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }
