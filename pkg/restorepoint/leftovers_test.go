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
	"strings"
	"testing"
)

// scanOut builds what LeftoverScanCommand's output looks like.
func scanOut(lines ...string) string {
	return markerScan + "\n" + strings.Join(lines, "\n") + "\n"
}

func TestLeftoverScanCommandRefusesWhatWouldBecomeARelativePath(t *testing.T) {
	if _, err := LeftoverScanCommand([]string{"var/lib/libvirt/images"}, ""); err == nil {
		t.Error("a relative directory was accepted; the output of this scan decides what rm -rf is pointed at, and a relative path names whatever directory the login lands in")
	}
	if _, err := LeftoverScanCommand(nil, ""); err == nil {
		t.Error("a scan with nowhere to look was accepted")
	}
}

// One find per directory, deduplicated, sorted, and every path quoted.
//
// Pinned because the shape is load-bearing in two ways: a find per directory is
// what makes a missing directory one skipped entry rather than a failed scan,
// and the quoting is the only thing between a disk directory with a space or a
// quote in its name and a command that means something else.
func TestLeftoverScanCommandShape(t *testing.T) {
	got, err := LeftoverScanCommand(
		[]string{"/data/replicas", "/data/replicas", "/other/dir"},
		"/data/replicas/.vmsync-rp",
	)
	if err != nil {
		t.Fatalf("LeftoverScanCommand: %v", err)
	}
	if n := strings.Count(got, "find "); n != 3 {
		t.Errorf("got %d find invocations, want 3 (the duplicate directory collapsed): %s", n, got)
	}
	for _, want := range []string{
		"echo " + markerScan,
		"'/data/replicas'",
		"'/data/replicas/.vmsync-rp'",
		"'/other/dir'",
		"-mindepth 1 -maxdepth 1",
		`-printf '%y\t%T@\t%p\n'`,
		"exit 0",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("command is missing %q: %s", want, got)
		}
	}
	// Quoting, on the one input that would otherwise end the argument.
	q, err := LeftoverScanCommand([]string{"/data/it's here"}, "")
	if err != nil {
		t.Fatalf("LeftoverScanCommand: %v", err)
	}
	if !strings.Contains(q, `'/data/it'\''s here'`) {
		t.Errorf("a directory containing a quote was not escaped: %s", q)
	}
}

func TestParseLeftoverScanReadsTheLinesAndNoticesNoOutput(t *testing.T) {
	if _, err := ParseLeftoverScan("find: /nope: No such file or directory\n"); err == nil {
		t.Error("output with no marker was accepted; that is a command that never ran, and reading it as an empty directory would mean a scan failure looks exactly like a clean host")
	}
	got, err := ParseLeftoverScan(scanOut(
		"f\t1758441600.0000000000\t/data/replicas/web01.qcow2",
		"d\t1758441500.0000000000\t/data/replicas/.vmsync-rp",
		"this line has no tabs at all",
	))
	if err != nil {
		t.Fatalf("ParseLeftoverScan: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3: %+v", len(got), got)
	}
	if got[0].Dir || got[0].MtimeUnix != 1758441600 || got[0].Path != "/data/replicas/web01.qcow2" {
		t.Errorf("entries[0] = %+v", got[0])
	}
	if !got[1].Dir {
		t.Errorf("entries[1] lost its directory flag: %+v", got[1])
	}
	// Kept rather than dropped: the only way to produce it is a path with a tab
	// or a newline in it, and an operator should hear that such a file is there
	// -- while it must never become a removal candidate.
	if got[2].Unparsed == "" || got[2].Path != "" {
		t.Errorf("an unreadable line was not held as unparsed: %+v", got[2])
	}
}

// AttributeLeftovers is where a shared directory is made per-domain, and this is
// the test that matters most in this file.
//
// Co-locating replicas in one images directory is the default, so the scan hands
// this function a directory holding several machines' disks and a stores parent
// holding several machines' asides. Every negative case below is a file that
// would otherwise be charged to web01: another domain's aside, another domain's
// store, and the aside of a domain whose name merely STARTS with web01.
func TestAttributeLeftoversChargesNothingToTheWrongDomain(t *testing.T) {
	entries, err := ParseLeftoverScan(scanOut(
		// web01's, all three kinds.
		"f\t1\t/data/web01.qcow2"+ReplicaReplacedSuffix+"1758441600",
		"f\t1\t/data/web01-disk1.qcow2"+ReplicaReplacedSuffix+"1758441600",
		"f\t1\t/data/web01.qcow2"+RestoreTempSuffix+"1758441700",
		"d\t1\t/data/.vmsync-rp/"+AsidePrefix+DomainPrefix+"web01-1758441500",
		// Not web01's.
		"f\t1\t/data/web01.qcow2",
		"f\t1\t/data/db01.qcow2"+ReplicaReplacedSuffix+"1758441600",
		"f\t1\t/data/web01.qcow2.bak",
		"d\t1\t/data/.vmsync-rp/"+DomainPrefix+"web01",
		"d\t1\t/data/.vmsync-rp/"+AsidePrefix+DomainPrefix+"db01-1758441500",
		"d\t1\t/data/.vmsync-rp/"+AsidePrefix+DomainPrefix+"web01-old-1758441500",
		// A hand-renamed aside: missed on purpose. A set this misses still shows
		// in df; one it misattributes sends somebody to delete another machine's
		// restore points.
		"d\t1\t/data/.vmsync-rp/"+AsidePrefix+DomainPrefix+"web01-keepme",
	))
	if err != nil {
		t.Fatalf("ParseLeftoverScan: %v", err)
	}

	got, unparsed := AttributeLeftovers(entries, []string{"/data/web01.qcow2", "/data/web01-disk1.qcow2"}, "web01")
	if len(unparsed) != 0 {
		t.Errorf("unparsed = %v, want none", unparsed)
	}
	kinds := map[string]int{}
	for _, l := range got {
		kinds[l.Kind]++
		if strings.Contains(l.Path, "db01") || strings.Contains(l.Path, "web01-old") {
			t.Errorf("%s was attributed to web01; the bytes land on the wrong machine and the path offered as this one's reclaimable space is another one's data", l.Path)
		}
		if l.Path == "/data/web01.qcow2" {
			t.Error("the LIVE replica disk was reported as a leftover -- the working set offered as reclaimable space")
		}
		if strings.HasSuffix(l.Path, ".bak") {
			t.Errorf("%s is not vmsync's file and must not be touched", l.Path)
		}
		if strings.HasSuffix(l.Path, "keepme") {
			t.Errorf("%s has no stamp, so its age is unknown; it must not be a candidate", l.Path)
		}
	}
	if kinds[LeftoverReplacedDisk] != 2 {
		t.Errorf("%s = %d, want web01's 2 disks: %+v", LeftoverReplacedDisk, kinds[LeftoverReplacedDisk], got)
	}
	if kinds[LeftoverRestoreStaging] != 1 {
		t.Errorf("%s = %d, want 1: %+v", LeftoverRestoreStaging, kinds[LeftoverRestoreStaging], got)
	}
	if kinds[LeftoverAsideStore] != 1 {
		t.Errorf("%s = %d, want 1 (the live store and two other domains' asides must stay out): %+v", LeftoverAsideStore, kinds[LeftoverAsideStore], got)
	}
	if len(got) != 4 {
		t.Errorf("got %d leftovers, want 4: %+v", len(got), got)
	}
	// Newest first, so a truncated listing truncates the safe end.
	for i := 1; i < len(got); i++ {
		if got[i-1].AtUnix < got[i].AtUnix {
			t.Errorf("came back oldest-first: %+v", got)
		}
	}
}

// The stamp in the name is preferred over mtime, and mtime is the fallback.
//
// Not interchangeable: a directory's mtime changes when anything inside it is
// touched, so an aside store's mtime can be today while the store was displaced
// in March -- and the sweep's whole decision is "how old is this".
func TestAttributeLeftoversPrefersTheStampInTheNameOverMtime(t *testing.T) {
	entries, err := ParseLeftoverScan(scanOut(
		"f\t1799999999\t/data/web01.qcow2"+ReplicaReplacedSuffix+"1758441600",
		"f\t1799999999\t/data/web01.qcow2"+RestoreTempSuffix+"notastamp",
	))
	if err != nil {
		t.Fatalf("ParseLeftoverScan: %v", err)
	}
	got, _ := AttributeLeftovers(entries, []string{"/data/web01.qcow2"}, "web01")
	if len(got) != 2 {
		t.Fatalf("got %d, want 2: %+v", len(got), got)
	}
	var stamped, unstamped Leftover
	for _, l := range got {
		if l.StampFromName {
			stamped = l
		} else {
			unstamped = l
		}
	}
	if stamped.AtUnix != 1758441600 {
		t.Errorf("a name carrying a stamp was aged by mtime (%d): an aside store's mtime moves every time anything inside it is touched, so it would read as new for ever", stamped.AtUnix)
	}
	if unstamped.AtUnix != 1799999999 || unstamped.StampFromName {
		t.Errorf("a name with no stamp did not fall back to mtime: %+v", unstamped)
	}
}

// An unknown domain name (one DomainSegment refuses) must not turn every aside
// in the parent into this domain's.
func TestAttributeLeftoversReportsNoAsideStoreForAnUnnameableDomain(t *testing.T) {
	entries, err := ParseLeftoverScan(scanOut(
		"d\t1\t/data/.vmsync-rp/" + AsidePrefix + DomainPrefix + "web01-1758441500",
	))
	if err != nil {
		t.Fatalf("ParseLeftoverScan: %v", err)
	}
	for _, domain := range []string{"", "..", "."} {
		if got, _ := AttributeLeftovers(entries, []string{"/data/web01.qcow2"}, domain); len(got) != 0 {
			t.Errorf("domain %q was attributed %+v; with no usable segment there is nothing to match against, and matching everything would point the sweep at every co-located machine's store", domain, got)
		}
	}
}

// LeftoverRemoveCommand re-checks the NAMING rather than the caller's say-so,
// which is the same contract RemoveStagingCommand has and for the same reason:
// the value is about to be interpolated into rm -rf.
func TestLeftoverRemoveCommandRefusesAnythingItDidNotRecognise(t *testing.T) {
	ok := Leftover{Path: "/data/web01.qcow2" + ReplicaReplacedSuffix + "1758441600", Kind: LeftoverReplacedDisk}
	cmd, err := LeftoverRemoveCommand(ok)
	if err != nil {
		t.Fatalf("a well-formed displaced disk was refused: %v", err)
	}
	if cmd != "rm -rf -- '/data/web01.qcow2"+ReplicaReplacedSuffix+"1758441600'" {
		t.Errorf("command = %q", cmd)
	}

	for _, tc := range []struct {
		why string
		l   Leftover
	}{
		{"the live replica disk, mislabelled", Leftover{Path: "/data/web01.qcow2", Kind: LeftoverReplacedDisk}},
		{"a relative path", Leftover{Path: "data/web01.qcow2" + ReplicaReplacedSuffix + "1", Kind: LeftoverReplacedDisk}},
		{"an unclean path", Leftover{Path: "/data/../web01.qcow2" + ReplicaReplacedSuffix + "1", Kind: LeftoverReplacedDisk}},
		{"a path with a newline", Leftover{Path: "/data/a\nb" + ReplicaReplacedSuffix + "1", Kind: LeftoverReplacedDisk}},
		{"an empty path", Leftover{Path: "", Kind: LeftoverReplacedDisk}},
		{"a kind vmsync does not make", Leftover{Path: "/data/web01.qcow2" + ReplicaReplacedSuffix + "1", Kind: "something-else"}},
		{"no stamp after the suffix", Leftover{Path: "/data/web01.qcow2" + ReplicaReplacedSuffix + "yesterday", Kind: LeftoverReplacedDisk}},
		{"an aside store not named like one", Leftover{Path: "/data/.vmsync-rp/" + DomainPrefix + "web01", Kind: LeftoverAsideStore}},
		{"the store root itself", Leftover{Path: "/data/.vmsync-rp", Kind: LeftoverAsideStore}},
		{"a directory reference", Leftover{Path: "/data/..", Kind: LeftoverReplacedDisk}},
	} {
		if cmd, err := LeftoverRemoveCommand(tc.l); err == nil {
			t.Errorf("%s was accepted and would have produced %q", tc.why, cmd)
		}
	}

	// A well-formed aside store, so the negatives above are not passing because
	// the whole kind is refused.
	store := Leftover{Path: "/data/.vmsync-rp/" + AsidePrefix + DomainPrefix + "web01-1758441500", Kind: LeftoverAsideStore}
	if _, err := LeftoverRemoveCommand(store); err != nil {
		t.Errorf("a well-formed aside store was refused: %v", err)
	}
}

func TestLeftoverSizeCommandAndParse(t *testing.T) {
	if _, err := LeftoverSizeCommand(nil); err == nil {
		t.Error("measuring nothing was accepted")
	}
	if _, err := LeftoverSizeCommand([]string{"relative/path"}); err == nil {
		t.Error("a relative path was accepted into a du command")
	}
	cmd, err := LeftoverSizeCommand([]string{"/data/a", "/data/b"})
	if err != nil {
		t.Fatalf("LeftoverSizeCommand: %v", err)
	}
	// --block-size=1 so the answer is bytes, and du rather than stat so an aside
	// store's whole tree is measured.
	if !strings.Contains(cmd, "du -s --block-size=1 -- '/data/a'") || !strings.Contains(cmd, "'/data/b'") {
		t.Errorf("command = %q", cmd)
	}

	if _, err := ParseLeftoverSizes("du: cannot access\n"); err == nil {
		t.Error("output with no marker was accepted as an answer")
	}
	sizes, err := ParseLeftoverSizes(scanOut(
		"32212254720\t/data/a",
		"4096\t/data/b",
		"not a size\t/data/c",
	))
	if err != nil {
		t.Fatalf("ParseLeftoverSizes: %v", err)
	}
	if sizes["/data/a"] != 32212254720 || sizes["/data/b"] != 4096 {
		t.Errorf("sizes = %+v", sizes)
	}
	if _, ok := sizes["/data/c"]; ok {
		t.Error("an unreadable size was recorded; 0 is the right answer and inventing one would put a made-up number in an operator's decision")
	}
}

func TestStampAfterAndAllDigits(t *testing.T) {
	for _, tc := range []struct {
		name, sep string
		want      int64
		ok        bool
	}{
		{"/d/web01.qcow2" + ReplicaReplacedSuffix + "1758441600", ReplicaReplacedSuffix, 1758441600, true},
		{"/d/web01.qcow2" + ReplicaReplacedSuffix, ReplicaReplacedSuffix, 0, false},
		{"/d/web01.qcow2" + ReplicaReplacedSuffix + "yesterday", ReplicaReplacedSuffix, 0, false},
		{"/d/web01.qcow2" + ReplicaReplacedSuffix + "-1", ReplicaReplacedSuffix, 0, false},
		{"/d/web01.qcow2", ReplicaReplacedSuffix, 0, false},
		// Two occurrences: the LAST one wins, because the first belongs to a
		// name that was already displaced once.
		{"/d/a" + ReplicaReplacedSuffix + "1" + ReplicaReplacedSuffix + "2", ReplicaReplacedSuffix, 2, true},
	} {
		got, ok := stampAfter(tc.name, tc.sep)
		if got != tc.want || ok != tc.ok {
			t.Errorf("stampAfter(%q) = %d,%v want %d,%v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

// A name carrying the suffix twice must be reported by the agent's scan AND
// removable by this sweep, or it is reported for ever and reclaimed by nothing.
//
// It can only come from a hand rename -- vmsync renames a live disk aside, never
// an aside -- so what matters is not which answer is "right" but that the two
// readers agree. inventory.displacedFrom anchors on the FIRST occurrence; this
// pins the same choice here, and pins that the STAMP still comes from the last,
// because that question ("when was this displaced") has a different answer.
func TestADoublyDisplacedNameIsStillThisDomainsAndStillRemovable(t *testing.T) {
	name := "/data/web01.qcow2" + ReplicaReplacedSuffix + "1758441600" + ReplicaReplacedSuffix + "1758441700"
	entries, err := ParseLeftoverScan(scanOut("f\t1\t" + name))
	if err != nil {
		t.Fatalf("ParseLeftoverScan: %v", err)
	}
	got, _ := AttributeLeftovers(entries, []string{"/data/web01.qcow2"}, "web01")
	if len(got) != 1 {
		t.Fatalf("got %d, want 1 -- the agent's scan reports this file, so a sweep that cannot see it leaves a permanent unreclaimable report: %+v", len(got), got)
	}
	if got[0].AtUnix != 1758441700 {
		t.Errorf("AtUnix = %d, want the LAST stamp: the question is when this was displaced", got[0].AtUnix)
	}
	if _, err := LeftoverRemoveCommand(got[0]); err != nil {
		t.Errorf("the sweep attributed this file and then refused to remove it, which is the same dead end: %v", err)
	}
}
