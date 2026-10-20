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
	"path"
	"strconv"
	"strings"
	"testing"
	"time"
)

// generatedSpellings is every name reachable by combining what
// strconv.ParseInt accepts in base 10 with the checkpoint shapes checkpointRe
// permits.
//
// GENERATED rather than hand-listed, and that is the point. The first version
// of these tests used a hand corpus, and a reviewer showed the invariant test
// was then strictly subsumed by the removal test below -- every name it could
// catch, the other caught too. A generated space is not: it covers spellings
// nobody thought to write down, which is exactly where a parser's edges are.
func generatedSpellings() []string {
	signs := []string{"", "+"}
	numbers := []string{
		"0", "00", "0000", "7", "07", "0000007",
		"1700000000", "01700000000", "00000000001700000000",
		"9223372036854775807", // math.MaxInt64, the largest ParseInt accepts
	}
	checkpoints := []string{"cpt", "vmsync-cpt-000001", "a.b_c-1", "Z9"}

	var out []string
	for _, s := range signs {
		for _, n := range numbers {
			for _, c := range checkpoints {
				out = append(out, s+n+"-"+c)
			}
		}
	}
	// Shapes that are not tags at all, by rules other than canonicality.
	return append(out,
		"nothyphenhere",
		"-1700000000-cpt",
		"1700000000-",
		"1700000000-.hidden",
		"notanumber-cpt",
		"vm-web01",
		"1700000000-has/slash",
		"9223372036854775808-cpt", // one past MaxInt64: ParseInt refuses
	)
}

// THE INVARIANT, and the defect was its violation: a name that parses as a tag
// must render back to itself.
//
// Point.tagName deliberately addresses a directory by the tag's RENDERING
// rather than by the caller's string, and RemoveCommand builds rm -rf from
// that. Those two are only sound together if parsing round-trips. It did not:
// strconv.ParseInt accepts a leading '+' and leading zeros, so a near-miss
// spelling parsed, rendered as its canonical sibling, and every later command
// addressed the sibling instead.
func TestEveryTagThatParsesRendersBackToItself(t *testing.T) {
	names := generatedSpellings()
	parsed, refused := 0, 0
	for _, name := range names {
		tag, err := ParseTag(name)
		if err != nil {
			refused++
			continue
		}
		parsed++
		if got := tag.String(); got != name {
			t.Errorf("ParseTag(%q) accepted it and renders as %q, so every command built from this tag -- including rm -rf -- addresses %q instead of the directory that was listed",
				name, got, got)
		}
	}
	// Both halves asserted, because a parser that accepts everything and one
	// that accepts nothing would each satisfy the loop above.
	if parsed == 0 {
		t.Error("nothing in the generated space parsed, so this proves only that nothing is a tag")
	}
	if refused == 0 {
		t.Error("everything in the generated space parsed, so the non-canonical spellings in it are still being accepted")
	}
	t.Logf("%d names: %d parsed, %d refused", len(names), parsed, refused)
}

// THE CONSEQUENCE, end to end: for any name accepted as a point, the rm names
// that same directory. This is the assertion 4.25 is about.
func TestTheRemovalNamesTheDirectoryThatWasListed(t *testing.T) {
	store, err := StoreFor("/data/replicas/web01.qcow2", "web01")
	if err != nil {
		t.Fatalf("StoreFor: %v", err)
	}
	root, err := store.Path()
	if err != nil {
		t.Fatalf("store.Path: %v", err)
	}

	checked := 0
	for _, name := range generatedSpellings() {
		tag, err := ParseTag(name)
		if err != nil {
			continue
		}
		cmd, err := RemoveCommand(store.Point(tag))
		if err != nil {
			t.Errorf("%s: RemoveCommand: %v", name, err)
			continue
		}
		checked++
		target := strings.Trim(strings.TrimPrefix(cmd, "rm -rf "), "'")
		if got := path.Base(target); got != name {
			t.Errorf("a point listed as %q would be removed by %q, which names %q -- a different directory, and if a point by that name exists it is one the retention policy decided to KEEP",
				name, cmd, got)
		}
		if path.Dir(target) != root {
			t.Errorf("%s: the removal addresses %q, outside this domain's store %q", name, target, root)
		}
	}
	if checked == 0 {
		t.Fatal("no removal command was built, so this test asserts nothing")
	}
}

// THE REFUSAL HAS TO NAME THE CANONICAL SPELLING, and asserting that needs
// care.
//
// The first version of this test checked Contains(err, canonical) and was
// VACUOUS: the message echoes the input under %q, and the canonical spelling is
// always a SUFFIX of a non-canonical one -- '+' and leading zeros can only
// prefix the canonical digits -- so the echo alone satisfied Contains. A
// reviewer proved it by brute force over 128 spellings and by a mutation that
// stripped the canonical name from the message entirely and survived the whole
// repo. The echo is removed here so only an independent mention counts.
//
// It matters because this message is operator-facing: -restore-restore-point
// and -clone-restore-point both take a tag somebody typed, and both wrap this
// error with %w.
func TestANonCanonicalSpellingIsRefusedAndSaysWhich(t *testing.T) {
	for _, tc := range []struct{ name, canonical string }{
		{"+1700000000-vmsync-cpt-000001", "1700000000-vmsync-cpt-000001"},
		{"01700000000-vmsync-cpt-000001", "1700000000-vmsync-cpt-000001"},
		{"0000-cpt", "0-cpt"},
		{"+0-cpt", "0-cpt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseTag(tc.name)
			if err == nil {
				t.Fatalf("ParseTag(%q) was accepted; it renders as %q, so it addresses a different directory", tc.name, tc.canonical)
			}
			// Strip every occurrence of the input before looking for the
			// canonical spelling, so the message's own echo cannot satisfy it.
			stripped := strings.ReplaceAll(err.Error(), tc.name, "<input>")
			if !strings.Contains(stripped, tc.canonical) {
				t.Errorf("the refusal does not name the canonical spelling %q independently of echoing the input, and that name is the one thing telling somebody what they actually have: %v", tc.canonical, err)
			}
		})
	}
}

// THE CALL SITE, not just the parser.
//
// Nothing detected the fix being reverted one level up: ParseListing could stop
// using ParseTag -- classify by prefix, say -- and the whole repo stayed green,
// because every other test here calls ParseTag directly. This drives the real
// entry point instead.
func TestTheListingRefusesANonCanonicalDirectory(t *testing.T) {
	out := strings.Join([]string{
		markerListing,
		"1700000000-vmsync-cpt-000001/",
		"+1700000000-vmsync-cpt-000002/",
		"01700000000-vmsync-cpt-000003/",
		StagingPrefix + "1700000000-vmsync-cpt-000004/",
		"",
	}, "\n")

	l, err := ParseListing(out)
	if err != nil {
		t.Fatalf("ParseListing: %v", err)
	}
	if len(l.Points) != 1 || l.Points[0].String() != "1700000000-vmsync-cpt-000001" {
		t.Errorf("points = %+v, want only the canonical one -- a non-canonical directory counted as a point can be selected by Prune, and the rm then names its canonical sibling", l.Points)
	}
	for _, want := range []string{"+1700000000-vmsync-cpt-000002", "01700000000-vmsync-cpt-000003"} {
		found := false
		for _, u := range l.Unknown {
			if u == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%q is not in Unknown = %v; it has to be reported somewhere, and Unknown is what cmd/vmsync warns about by name", want, l.Unknown)
		}
	}
	if len(l.Staging) != 1 {
		t.Errorf("staging = %v, want the one staging directory", l.Staging)
	}
}

// THE STAGING SWEEP NEEDS A SHAPE, NOT A SPELLING -- and requiring the spelling
// was a regression this fix introduced and a reviewer caught.
//
// ParseListing buckets anything with StagingPrefix as staging BEFORE it reaches
// ParseTag, and RemoveStagingCommand removes path.Join(root, name): the
// verbatim string, never a re-rendering. So canonicality buys nothing there,
// and demanding it made the listing offer a directory the sweep refused on
// every run, for ever.
func TestTheStagingSweepRemovesWhatTheListingOffered(t *testing.T) {
	store, err := StoreFor("/data/replicas/web01.qcow2", "web01")
	if err != nil {
		t.Fatalf("StoreFor: %v", err)
	}

	for _, name := range []string{
		StagingPrefix + "1700000000-vmsync-cpt-000001",
		// Non-canonical, which the listing still offers as staging.
		StagingPrefix + "+1700000000-vmsync-cpt-000002",
		StagingPrefix + "01700000000-vmsync-cpt-000003",
	} {
		cmd, err := RemoveStagingCommand(store, name)
		if err != nil {
			t.Errorf("RemoveStagingCommand(%q): %v -- ParseListing offers this as staging, so refusing it here leaves it on disk for ever", name, err)
			continue
		}
		if !strings.Contains(cmd, name) {
			t.Errorf("the command for %q does not name it verbatim: %s", name, cmd)
		}
	}

	// The shape is still bounded, which is the thing that site does need:
	// the name reaches rm -rf.
	for _, bad := range []string{
		StagingPrefix + "1700000000-has/slash",
		StagingPrefix + "notanumber-cpt",
		StagingPrefix + "1700000000-.hidden",
	} {
		if _, err := RemoveStagingCommand(store, bad); err == nil {
			t.Errorf("RemoveStagingCommand(%q) was accepted, and that name is interpolated into rm -rf", bad)
		}
	}
}

// The mint side of the same invariant.
//
// ParseTag requires a name to render back to itself; nothing required the same
// of NewTag, and a negative time broke it unrepairably -- "-100-cpt" renders
// fine and ParseTag cuts at the FIRST '-', leaving an empty unix half it then
// refuses. A restore point could be written whose own name could never be read
// back.
func TestATagCannotBeMintedWithANameItsOwnParserRefuses(t *testing.T) {
	for _, at := range []time.Time{
		time.Unix(-1, 0),
		time.Unix(-100, 0),
		time.Date(1969, 7, 20, 20, 17, 0, 0, time.UTC),
	} {
		if _, err := NewTag(at, "cpt"); err == nil {
			t.Errorf("NewTag accepted %s, which renders as %q -- a name ParseTag cannot read back",
				at.UTC().Format(time.RFC3339), strconv.FormatInt(at.Unix(), 10)+"-cpt")
		}
	}

	// And the boundary is kept: the epoch itself is a legal, parseable tag.
	tag, err := NewTag(time.Unix(0, 0), "cpt")
	if err != nil {
		t.Fatalf("NewTag at the epoch was refused: %v", err)
	}
	if _, err := ParseTag(tag.String()); err != nil {
		t.Errorf("a tag minted at the epoch renders as %q, which ParseTag refuses: %v", tag.String(), err)
	}
}
