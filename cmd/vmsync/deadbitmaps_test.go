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
	"strings"
	"testing"
	"time"
)

// The clock these tests reason against: the generation below was minted at
// 2026-10-21 14:30 UTC, and "now" is ninety minutes later.
var (
	deadNow      = time.Date(2026, 10, 21, 16, 0, 0, 0, time.UTC)
	deadGenA     = "2610211430-a1b2"
	deadGenB     = "2610200900-9f3c"
	deadBitmapA1 = "vmsync-cpt-" + deadGenA + "-000001"
	deadBitmapA2 = "vmsync-cpt-" + deadGenA + "-000002"
	deadBitmapB1 = "vmsync-cpt-" + deadGenB + "-000007"
)

// Nothing dead means nothing said. This report runs on EVERY sync, so a
// message on a healthy pair would train an operator to ignore it.
func TestNoDeadBitmapsSaysNothing(t *testing.T) {
	if got := attributeDeadBitmaps(nil, deadNow); got != "" {
		t.Errorf("an empty set produced a report:\n%s", got)
	}
	if got := attributeDeadBitmaps(map[string][]string{}, deadNow); got != "" {
		t.Errorf("an empty map produced a report:\n%s", got)
	}
}

// One dead generation: the report has to name the generation, how long ago it
// was minted, how many bitmaps carry it, and where they are. A count alone
// would tell an operator that something is wrong and nothing about what.
func TestADeadGenerationIsNamedWithItsAge(t *testing.T) {
	got := attributeDeadBitmaps(map[string][]string{
		"/vm/a.qcow2": {deadBitmapA1, deadBitmapA2},
	}, deadNow)

	for _, want := range []struct{ what, s string }{
		{"the generation", deadGenA},
		{"when it was minted", "2026-10-21T14:30:00Z"},
		{"how long ago that was", "1h30m0s ago"},
		{"how many bitmaps carry it", "2 bitmap(s) from generation"},
		{"the total", "2 dirty bitmap(s)"},
		{"the first bitmap and its disk", deadBitmapA1 + " in /vm/a.qcow2"},
		{"the second", deadBitmapA2 + " in /vm/a.qcow2"},
		{"that no incremental can use them", "no incremental can ever use them"},
	} {
		if !strings.Contains(got, want.s) {
			t.Errorf("the report does not state %s (%q):\n%s", want.what, want.s, got)
		}
	}
}

// THE HONESTY REQUIREMENT, and it is not decoration.
//
// The read is of the qcow2 bitmap directory on disk, which is not what qemu
// holds: measured on this project's own hardware, a checkpoint made at 16:46
// was still absent from the directory at 18:10 on a running domain and
// appeared only after a restart. A report that omitted the caveat would let an
// absent measurement read as health, which is the defect class this audit
// keeps finding.
//
// The backing-chain scope is the same kind of claim: the enumeration reads
// chain[0] only, so a shared base's bitmaps are outside what this counted.
func TestTheReportSaysWhatItCannotSee(t *testing.T) {
	got := attributeDeadBitmaps(map[string][]string{"/vm/a.qcow2": {deadBitmapA1}}, deadNow)
	for _, want := range []string{
		"floor, not a census",
		"need not be in the file yet",
		"never down a backing chain",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the report omits %q, so it would read as a complete census of what is there:\n%s", want, got)
		}
	}
}

// Two generations must not be merged. Merging them would report one number an
// operator cannot act on: the generations have different ages, and age is the
// only thing that distinguishes a leak worth chasing from one made minutes ago
// by a rebuild that is still running.
func TestGenerationsAreReportedSeparately(t *testing.T) {
	got := attributeDeadBitmaps(map[string][]string{
		"/vm/a.qcow2": {deadBitmapA1, deadBitmapB1},
		"/vm/b.qcow2": {deadBitmapA2},
	}, deadNow)

	if !strings.Contains(got, "2 bitmap(s) from generation "+deadGenA) {
		t.Errorf("generation %s is not reported with its own count:\n%s", deadGenA, got)
	}
	if !strings.Contains(got, "1 bitmap(s) from generation "+deadGenB) {
		t.Errorf("generation %s is not reported with its own count:\n%s", deadGenB, got)
	}
	if !strings.Contains(got, "3 dirty bitmap(s)") {
		t.Errorf("the total is wrong:\n%s", got)
	}
	// Ordered, so the same state reads the same way twice. B was minted
	// earlier and sorts first by token.
	if a, b := strings.Index(got, deadGenA), strings.Index(got, deadGenB); b > a {
		t.Errorf("the generations are not in a stable order:\n%s", got)
	}
}

// The three buckets, and the point of having three: "we cannot attribute this"
// and "this predates attribution" are different answers, and so is a name
// vmsync never minted. An operator deciding whether a file is safe to remove
// needs to know which of the three they are looking at.
func TestTheBucketsAreDistinguishable(t *testing.T) {
	got := attributeDeadBitmaps(map[string][]string{
		"/vm/a.qcow2": {
			deadBitmapA1,           // a readable generation
			"vmsync-cpt-000042",    // the shape that predates tokens
			"vmsync-cpt-hand-made", // prefixed, so enumerated, but not ours
		},
	}, deadNow)

	for _, want := range []struct{ what, s string }{
		{"the readable generation", "from generation " + deadGenA},
		{"the pre-token shape", "from before generation tokens existed"},
		{"the unattributable name", "vmsync did not mint and cannot attribute"},
	} {
		if !strings.Contains(got, want.s) {
			t.Errorf("the report does not distinguish %s (%q):\n%s", want.what, want.s, got)
		}
	}
	if !strings.Contains(got, "3 dirty bitmap(s)") {
		t.Errorf("the total does not count all three buckets:\n%s", got)
	}
}

// A token whose date is not a date. The grammar admits ten digits, not a
// calendar, so this is reachable from any name that satisfies the parser --
// and the report must say the date is unreadable rather than render an age
// computed from a zero time, which would claim a bitmap from the year 1.
func TestAnUnreadableDateIsSaidSoRatherThanComputed(t *testing.T) {
	got := attributeDeadBitmaps(map[string][]string{
		"/vm/a.qcow2": {"vmsync-cpt-9999999999-abcd-000001"},
	}, deadNow)

	if !strings.Contains(got, "minted at an unreadable date") {
		t.Errorf("an unparseable date is not reported as one:\n%s", got)
	}
	if strings.Contains(got, "ago") {
		t.Errorf("an age was computed from a date that could not be read:\n%s", got)
	}
	// The generation is still named: it is what distinguishes these bitmaps
	// from any other, whatever the date half says.
	if !strings.Contains(got, "9999999999-abcd") {
		t.Errorf("the generation is not named:\n%s", got)
	}
}

// Map iteration order is random, and the same state must read identically
// every time or two operators comparing notes see two different reports.
func TestTheDeadBitmapReportIsStable(t *testing.T) {
	in := map[string][]string{
		"/vm/z.qcow2": {deadBitmapB1, deadBitmapA1},
		"/vm/a.qcow2": {deadBitmapA2},
		"/vm/m.qcow2": {"vmsync-cpt-000009"},
	}
	first := attributeDeadBitmaps(in, deadNow)
	for i := 0; i < 50; i++ {
		if got := attributeDeadBitmaps(in, deadNow); got != first {
			t.Fatalf("the report varies between runs:\n%s\n---\n%s", first, got)
		}
	}
	// Sorted rather than merely consistent, and the ordering is two-level:
	// generations in token order, and WITHIN each generation the disks in path
	// order. Checked inside one generation's line, because across the whole
	// report a disk appears once per generation it carries bitmaps for -- here
	// /vm/z.qcow2 appears in both, so a global comparison would be asserting
	// the group order instead and would pass or fail for the wrong reason.
	var genALine string
	for _, line := range strings.Split(first, "\n") {
		if strings.Contains(line, "from generation "+deadGenA) {
			genALine = line
		}
	}
	if genALine == "" {
		t.Fatalf("generation %s is not reported at all:\n%s", deadGenA, first)
	}
	if a, z := strings.Index(genALine, "/vm/a.qcow2"), strings.Index(genALine, "/vm/z.qcow2"); a < 0 || z < 0 || a > z {
		t.Errorf("within one generation the disks are not in path order: %q", genALine)
	}
	// And the generations themselves are in token order, which puts the older
	// mint date first.
	if b, a := strings.Index(first, "from generation "+deadGenB), strings.Index(first, "from generation "+deadGenA); b > a {
		t.Errorf("the generations are not in token order:\n%s", first)
	}
}
