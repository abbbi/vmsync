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

package unsupportedqmp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vmsync/pkg/disk"
)

// The fixtures in testdata/ are REAL replies, captured from libvirt 12.0.0 /
// qemu 10.2.2 on a two-disk domain where one disk has a backing chain and both
// carry one vmsync-named bitmap. A parser tested against JSON somebody wrote by
// hand passes for the wrong reason: the measured reply contains
// `image.format-specific.data.bitmaps` as well, present and EMPTY, and reading
// that instead is both the likeliest mistake and invisible to a hand-written
// fixture.
//
// The paths are the capture host's and are used verbatim below, because the
// mapping under test is string equality against QcowDisk.Source.
const (
	fxVda  = "/home/claude/vmsync-bench/probe/vmsync-fixture-probe-1.qcow2" // libvirt-2-format
	fxVdb  = "/home/claude/vmsync-bench/probe/vmsync-fixture-probe-2.qcow2" // libvirt-1-format
	fxBase = "/home/claude/vmsync-bench/probe/vmsync-fixture-probe-b.qcow2" // vda's backing file
	fxName = "vmsync-cpt-2611040000-beef-000001"
)

func vmsyncNamed(name string) bool { return strings.HasPrefix(name, "vmsync-cpt-") }

// fixtureDisks mirrors the domain the fixture was captured from: vda is the
// disk WITH a backing chain, vdb the single-layer one.
func fixtureDisks() []disk.QcowDisk {
	return []disk.QcowDisk{
		{TargetDev: "vda", Source: fxVda, RootSource: fxBase},
		{TargetDev: "vdb", Source: fxVdb, RootSource: fxVdb},
	}
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("this test is only meaningful against the real reply: %v", err)
	}
	return string(b)
}

// PARSE READS WHAT QEMU HOLDS, not what the file holds.
//
// The same reply carries `image.format-specific.data.bitmaps`, which is
// qemu-img's path and is EMPTY here -- these bitmaps were made during this
// qemu's life and are not in the file yet, which is the entire reason this
// package exists. Reading that key instead yields a census that finds nothing
// new on every host, agrees with the image read always, and fixes nothing.
func TestParseReadsTheBitmapsQemuHoldsNotTheOnesTheFileHolds(t *testing.T) {
	found, nodes, err := Parse(readFixture(t, "nodes-two-disks-orphan.json"))
	if err != nil {
		t.Fatalf("the real reply did not parse: %v", err)
	}
	if nodes != 6 {
		t.Errorf("counted %d block nodes, want 6 (a -format and a -storage node for each of two disks plus the backing file). The count is what tells an empty census from an unanswered query", nodes)
	}
	if len(found) != 2 {
		t.Fatalf("found %d bitmaps, want 2 -- one per disk. Zero means the parser is reading image.format-specific.data.bitmaps, which is qemu-img's view and is empty for a bitmap this qemu has not written out yet", len(found))
	}
	for _, b := range found {
		if b.Name != fxName {
			t.Errorf("bitmap named %q, want %q", b.Name, fxName)
		}
		if !b.Persistent {
			t.Errorf("%s on %s reported non-persistent; a libvirt checkpoint bitmap is persistent, and treating it as transient would set it aside as nothing to clear", b.Name, b.NodeName)
		}
		if b.Busy {
			t.Errorf("%s on %s reported busy with no backup running", b.Name, b.NodeName)
		}
		if b.File == "" {
			t.Errorf("%s on %s carries no file path, which is the only key that maps it to a disk", b.Name, b.NodeName)
		}
	}
}

// AN ABSENT ANSWER IS NOT AN EMPTY ONE, and the two must not share a value.
func TestParseTellsAMissingReturnFromAnEmptyOne(t *testing.T) {
	if _, _, err := Parse(`{}`); err == nil {
		t.Error("a reply with no `return` key parsed as success. That is a failed read, and reporting it as an empty one says the source holds no bitmaps")
	}
	if _, _, err := Parse(`{"return":"not an array"}`); err == nil {
		t.Error("a `return` that is not an array parsed as success")
	}
	found, nodes, err := Parse(`{"return":[]}`)
	if err != nil {
		t.Errorf("an empty array is a legitimate answer from a domain with no block nodes: %v", err)
	}
	if len(found) != 0 || nodes != 0 {
		t.Errorf("got %d bitmaps / %d nodes from an empty array, want 0/0", len(found), nodes)
	}
}

func TestParseRefusesWhatItCannotRead(t *testing.T) {
	for _, tc := range []struct{ name, reply string }{
		{"not json", `this is not json`},
		{"empty", ``},
		{"qemu error object", `{"id":"libvirt-19","error":{"class":"CommandNotFound","desc":"The command query-named-block-nodes has not been found"}}`},
	} {
		if _, _, err := Parse(tc.reply); err == nil {
			t.Errorf("%s: parsed as success, so a census that never happened would read as a clean source", tc.name)
		}
	}
}

// A NODE WITH NO BITMAPS IS NORMAL. The real reply is full of them -- every
// -storage child, every backing node, every cdrom -- and treating a missing
// key as malformed would switch the census off on every host.
func TestParseToleratesANodeWithNoBitmaps(t *testing.T) {
	found, nodes, err := Parse(`{"return":[{"node-name":"libvirt-1-storage","file":"/x.qcow2","drv":"file"}]}`)
	if err != nil {
		t.Fatalf("a node with no dirty-bitmaps key is an ordinary node: %v", err)
	}
	if nodes != 1 || len(found) != 0 {
		t.Errorf("got %d nodes / %d bitmaps, want 1/0", nodes, len(found))
	}
}

// THE MEASURED TRAP: NODE NUMBERS DO NOT FOLLOW DISK ORDER.
//
// In the real reply libvirt-1-format is vdb and libvirt-2-format is vda. Any
// mapping that reads the node's number, or its position in the array, puts one
// disk's bitmaps on the other -- and since both disks carry the same checkpoint
// name here, a cross-map is invisible in the result and only shows up when
// adoptionPlan builds an adoption XML naming the wrong target device.
func TestMapKeysOnTheFilePathNotTheNodeNumber(t *testing.T) {
	found, nodes, err := Parse(readFixture(t, "nodes-two-disks-orphan.json"))
	if err != nil {
		t.Fatal(err)
	}
	// vda is DELIBERATELY given a bitmap name the other disk does not have, by
	// renaming the one the fixture put on libvirt-2-format (vda's node). If the
	// mapping went by node number, this name would land on vdb.
	for i := range found {
		if found[i].File == fxVda {
			found[i].Name = "vmsync-cpt-2611040000-beef-000099"
		}
	}

	c := Map(found, nodes, fixtureDisks(), vmsyncNamed)

	if got := c.ByDisk[fxVda]; len(got) != 1 || got[0] != "vmsync-cpt-2611040000-beef-000099" {
		t.Errorf("vda (%s) got %v, want only the name that was on ITS node. libvirt-2-format is vda and libvirt-1-format is vdb; reading the node number swaps them", fxVda, got)
	}
	if got := c.ByDisk[fxVdb]; len(got) != 1 || got[0] != fxName {
		t.Errorf("vdb (%s) got %v, want only %q", fxVdb, got, fxName)
	}
}

// A BACKING FILE'S BITMAP IS NOT THIS CHAIN'S.
//
// Same rule, same reason, as the image read's top-layer-only restriction: the
// base may be shared with another domain, and adopting a bitmap on it would
// delete something a different pair is diffing against.
func TestMapRefusesToClaimABackingFilesBitmap(t *testing.T) {
	found := []Bitmap{{Name: fxName, NodeName: "libvirt-3-format", File: fxBase, Persistent: true}}
	c := Map(found, 6, fixtureDisks(), vmsyncNamed)

	if len(c.ByDisk) != 0 {
		t.Errorf("claimed a bitmap on the backing file: %v. The base can be shared, and deleting a bitmap on it takes away another pair's baseline", c.ByDisk)
	}
	if !hasUnclaimed(c, ReasonBacking, fxName) {
		t.Errorf("the backing-file bitmap was neither claimed nor reported; Unclaimed=%+v. Dropping it silently is the defect this codebase keeps finding", c.Unclaimed)
	}
}

// THE HOLE ALL THREE DESIGNS MISSED, and the one thing that could make this
// package fail a run.
//
// parseQcowDisks sets TargetDev to "" for a qcow2 <disk> with no <target>, and
// util.IgnoreDevice does not filter it out. adoptionPlan routes an empty target
// to its unmapped set, clearOrphanBitmapsOnline turns that into an error, and
// the reinit block returns "refusing to rebuild". Today a bitmap on such a disk
// is invisible on a running source and the run succeeds; admitting one here
// would make the census able to refuse a rebuild, which is precisely what it
// must never do.
func TestMapWillNotClaimABitmapOnADiskWithNoTargetDev(t *testing.T) {
	disks := []disk.QcowDisk{{TargetDev: "", Source: fxVda, RootSource: fxVda}}
	found := []Bitmap{{Name: fxName, NodeName: "libvirt-2-format", File: fxVda, Persistent: true}}

	c := Map(found, 2, disks, vmsyncNamed)

	if len(c.ByDisk) != 0 {
		t.Errorf("claimed %v on a disk the domain gives no <target dev>. adoptionPlan cannot name it in an adoption XML, so clearOrphanBitmapsOnline fails and the whole rebuild is refused -- for a bitmap that is invisible, and harmless, today", c.ByDisk)
	}
	if !hasUnclaimed(c, ReasonNoTarget, fxName) {
		t.Errorf("not reported either; Unclaimed=%+v. An operator needs to know qemu holds it and why vmsync will not touch it", c.Unclaimed)
	}
	if c.Proven() {
		t.Error("Proven() is true with something unclaimed, so the caller would stop warning that its answer is incomplete")
	}
}

func TestMapSetsAsideWhatMustNotBeAdopted(t *testing.T) {
	for _, tc := range []struct {
		name   string
		bitmap Bitmap
		where  func(Census) map[string][]string
		why    string
	}{
		{
			name:   "busy",
			bitmap: Bitmap{Name: fxName, NodeName: "n", File: fxVdb, Persistent: true, Busy: true},
			where:  func(c Census) map[string][]string { return c.Busy },
			why:    "a backup is using it; adopting and deleting it now pulls the bitmap out from under a running job",
		},
		{
			name:   "transient",
			bitmap: Bitmap{Name: fxName, NodeName: "n", File: fxVdb, Persistent: false},
			where:  func(c Census) map[string][]string { return c.Transient },
			why:    "it was never written to the image, so there is nothing in the qcow2 to clear",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Map([]Bitmap{tc.bitmap}, 2, fixtureDisks(), vmsyncNamed)
			if len(c.ByDisk) != 0 {
				t.Errorf("admitted a %s bitmap into the clearing set: %v -- %s", tc.name, c.ByDisk, tc.why)
			}
			if len(tc.where(c)) == 0 {
				t.Errorf("a %s bitmap was dropped instead of set aside", tc.name)
			}
			if c.Proven() {
				t.Errorf("Proven() is true with a %s bitmap set aside, so the caller would claim a complete answer it does not have", tc.name)
			}
		})
	}
}

// A DISABLED BITMAP IS STILL AN ORPHAN.
//
// recording=false means it has stopped tracking writes, not that it has gone:
// it still occupies the qcow2 and still has no checkpoint behind it, and
// adopt-and-delete still removes it. This is also the case REDEFINE_VALIDATE
// wrongly conflates with absent, measured -- which is why validation is not
// used to gate clearing.
func TestMapIncludesADisabledBitmap(t *testing.T) {
	found, nodes, err := Parse(readFixture(t, "nodes-bitmap-disabled.json"))
	if err != nil {
		t.Fatal(err)
	}
	var sawDisabled bool
	for _, b := range found {
		if !b.Recording {
			sawDisabled = true
		}
	}
	if !sawDisabled {
		t.Fatal("the disabled-bitmap fixture carries no bitmap with recording=false; it is not exercising the case")
	}
	c := Map(found, nodes, fixtureDisks(), vmsyncNamed)
	total := 0
	for _, names := range c.ByDisk {
		total += len(names)
	}
	if total != 2 {
		t.Errorf("claimed %d bitmaps, want 2: a disabled bitmap still takes space in the qcow2 with no checkpoint behind it, and is still removable", total)
	}
}

// AN EMPTY REPLY FROM A DOMAIN THAT HAS DISKS IS A FAILED READ.
func TestMapReportsNoNodesAsAFailedRead(t *testing.T) {
	c := Map(nil, 0, fixtureDisks(), vmsyncNamed)
	if len(c.Problems) == 0 {
		t.Error("qemu described no block nodes at all for a two-disk domain and nothing complained; that is a failed read wearing the shape of a clean one")
	}
	if c.Proven() {
		t.Error("Proven() is true for a reply that described nothing")
	}
	// And the legitimate case: no disks, no nodes, no complaint.
	if quiet := Map(nil, 0, nil, vmsyncNamed); len(quiet.Problems) != 0 {
		t.Errorf("complained about an empty reply for a domain with no disks: %v", quiet.Problems)
	}
}

// ONLY VMSYNC'S OWN NAMES, because the clearing route deletes what it is
// pointed at. libvirt makes its own bitmaps for an in-flight backup, and
// vmsync's verify window is deliberately unprefixed so it is invisible here.
func TestMapIgnoresNamesThatAreNotVmsyncs(t *testing.T) {
	found := []Bitmap{
		{Name: "libvirt-tmp-activewrite", NodeName: "n", File: fxVdb, Persistent: true},
		{Name: "vmsync-verify-window", NodeName: "n", File: fxVdb, Persistent: true},
		{Name: fxName, NodeName: "n", File: fxVdb, Persistent: true},
	}
	c := Map(found, 2, fixtureDisks(), vmsyncNamed)
	got := c.ByDisk[fxVdb]
	if len(got) != 1 || got[0] != fxName {
		t.Errorf("claimed %v, want only %q. Adopting and deleting a bitmap libvirt is using for a backup, or vmsync's own verify window, destroys something in use", got, fxName)
	}
	if len(c.Unclaimed) != 0 {
		t.Errorf("reported a non-vmsync bitmap as unclaimed: %+v. They are not ours and not noise to report; a warning per foreign bitmap trains the reader to filter the one that matters", c.Unclaimed)
	}
}

// ONE DISK, ONE ENTRY. The -format and -storage nodes of a disk report the
// same file, so without a dedupe adoptionPlan builds a devs slice with the
// target device twice and buildAdoptionXML emits two <disk> elements for it.
func TestMapDedupesTheNodesOfOneDisk(t *testing.T) {
	found := []Bitmap{
		{Name: fxName, NodeName: "libvirt-1-format", File: fxVdb, Persistent: true},
		{Name: fxName, NodeName: "libvirt-1-storage", File: fxVdb, Persistent: true},
	}
	c := Map(found, 2, fixtureDisks(), vmsyncNamed)
	if got := c.ByDisk[fxVdb]; len(got) != 1 {
		t.Errorf("got %v for one disk's two nodes, want one entry", got)
	}
}

func TestMapNamesAFileItCannotPlace(t *testing.T) {
	found := []Bitmap{{Name: fxName, NodeName: "libvirt-9-format", File: "/somewhere/else.qcow2", Persistent: true}}
	c := Map(found, 7, fixtureDisks(), vmsyncNamed)
	if len(c.ByDisk) != 0 {
		t.Errorf("claimed a bitmap on a file this domain does not manage: %v", c.ByDisk)
	}
	if !hasUnclaimed(c, ReasonUnknown, fxName) {
		t.Errorf("dropped it instead of naming it; Unclaimed=%+v", c.Unclaimed)
	}
}

// A MANAGED DISK NO NODE COVERED is the symlink case, and it has to be said:
// the census is then incomplete for a reason only the operator can resolve.
func TestMapReportsADiskQemuNeverMentioned(t *testing.T) {
	disks := append(fixtureDisks(), disk.QcowDisk{TargetDev: "vdc", Source: "/not/in/the/reply.qcow2", RootSource: "/not/in/the/reply.qcow2"})
	found, nodes, err := Parse(readFixture(t, "nodes-two-disks-orphan.json"))
	if err != nil {
		t.Fatal(err)
	}
	c := Map(found, nodes, disks, vmsyncNamed)
	if len(c.Uncovered) != 1 {
		t.Fatalf("Uncovered=%v, want the one disk no node's file matched", c.Uncovered)
	}
	if !strings.Contains(c.Uncovered[0], "/not/in/the/reply.qcow2") || !strings.Contains(c.Uncovered[0], "vdc") {
		t.Errorf("the line does not name the disk and its device: %q", c.Uncovered[0])
	}
	if !strings.Contains(c.Uncovered[0], fxVda) {
		t.Errorf("the line does not say which files qemu DID describe, which is what makes a symlinked <source file> diagnosable: %q", c.Uncovered[0])
	}
	if c.Proven() {
		t.Error("Proven() is true with a disk uncovered")
	}
}

// PROVEN IS THE ONLY SUBTRACTIVE POWER HERE, so every partial census must make
// it false. Each row is a one-line mutation of Proven's condition.
func TestProvenIsFalseForEveryPartialCensus(t *testing.T) {
	full := Census{ByDisk: map[string][]string{fxVdb: {fxName}}, Nodes: 6,
		Busy: map[string][]string{}, Transient: map[string][]string{}}
	if !full.Proven() {
		t.Fatal("a census with nothing wrong is not Proven; every row below is then vacuous")
	}
	for _, tc := range []struct {
		name string
		mut  func(*Census)
	}{
		{"a problem", func(c *Census) { c.Problems = []string{"x"} }},
		{"no nodes", func(c *Census) { c.Nodes = 0 }},
		{"a disk uncovered", func(c *Census) { c.Uncovered = []string{"x"} }},
		{"something unclaimed", func(c *Census) { c.Unclaimed = []UnclaimedNode{{NodeName: "n"}} }},
		{"a busy bitmap", func(c *Census) { c.Busy = map[string][]string{fxVdb: {"b"}} }},
		{"a transient bitmap", func(c *Census) { c.Transient = map[string][]string{fxVdb: {"b"}} }},
	} {
		c := full
		c.Problems, c.Uncovered, c.Unclaimed = nil, nil, nil
		c.Busy, c.Transient = map[string][]string{}, map[string][]string{}
		tc.mut(&c)
		if c.Proven() {
			t.Errorf("%s: Proven() is still true. The caller uses it to stop saying its answer is incomplete, so a partial census that claims proof silences a warning it did not earn", tc.name)
		}
	}
}

// THE UNION ONLY EVER ADDS. If a name the image read found could be removed
// here, the supported path's behaviour would depend on the unsupported one and
// deleting this package could make vmsync act on less.
func TestUnionOnlyEverAdds(t *testing.T) {
	// DELIBERATELY NOT IN SORTED ORDER, on either side. With inputs that
	// happen to arrive sorted, dropping the sort leaves the result sorted and
	// the ordering assertion below passes over the mutation that removed it.
	fromFile := map[string][]string{fxVda: {"vmsync-cpt-z-0009", "vmsync-cpt-a-0001"}, fxVdb: {"vmsync-cpt-m-0005"}}
	for _, tc := range []struct {
		name     string
		fromQemu map[string][]string
	}{
		{"nothing", nil},
		{"empty", map[string][]string{}},
		{"a name sorting before the file's", map[string][]string{fxVda: {"vmsync-cpt-b-0002"}}},
		{"the same names", fromFile},
		{"a disk the file read did not mention", map[string][]string{"/third.qcow2": {"vmsync-cpt-y-0008", "vmsync-cpt-c-0003"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := Union(fromFile, tc.fromQemu)
			for p, names := range fromFile {
				for _, n := range names {
					if !contains(out[p], n) {
						t.Errorf("%s/%s from the image read is missing from the union %v", p, n, out)
					}
				}
			}
			for p, names := range out {
				if len(names) == 0 {
					t.Errorf("%s maps to an empty list; an empty key reads as a disk with bitmaps on it", p)
				}
				for i := 1; i < len(names); i++ {
					if names[i-1] >= names[i] {
						t.Errorf("%s is not sorted and deduped: %v. Two operators comparing notes must see one message", p, names)
					}
				}
			}
		})
	}
}

// ONLYIN IS WHAT DECIDES A CLEARING FAILURE'S SEVERITY: a bitmap the image read
// also found is one vmsync could already see and already refused over, while
// one only qemu reported is newly visible, and a run that used to succeed must
// not start failing because vmsync can now see more.
func TestOnlyInNamesWhatOnlyQemuSaw(t *testing.T) {
	fromFile := map[string][]string{fxVda: {"shared"}}
	fromQemu := map[string][]string{fxVda: {"shared", "qemu-only"}, fxVdb: {"vdb-only"}}

	only := OnlyIn(fromQemu, fromFile)
	if contains(only[fxVda], "shared") {
		t.Error("a name both readers found was reported as qemu-only, which would downgrade a refusal vmsync already makes today")
	}
	if !contains(only[fxVda], "qemu-only") || !contains(only[fxVdb], "vdb-only") {
		t.Errorf("missed a name only qemu reported: %v", only)
	}
	if len(OnlyIn(fromFile, fromFile)) != 0 {
		t.Error("a set differenced against itself is not empty")
	}
}

// SAID ONCE PER DOMAIN, not once per process and not once per call. A reinit
// asks twice; a second pair in the same run must still be announced.
func TestTheTaintNoticeIsSaidOncePerDomain(t *testing.T) {
	ResetTaintNoticeForTest()
	t.Cleanup(ResetTaintNoticeForTest)

	first := taintNotice("web01")
	if first == "" {
		t.Fatal("the first ask for a domain said nothing; an operator would find the taint in libvirt's log with nothing to explain it")
	}
	for _, want := range []string{"custom-monitor", "query-named-block-nodes", "read-only", "restarting"} {
		if !strings.Contains(first, want) {
			t.Errorf("the notice never mentions %q:\n\n%s", want, first)
		}
	}
	if !strings.Contains(first, "web01") {
		t.Error("the notice does not name the domain being tainted")
	}
	if again := taintNotice("web01"); again != "" {
		t.Errorf("said twice for one domain; a reinit asks twice and two copies train the reader to skip it:\n\n%s", again)
	}
	if other := taintNotice("db02"); other == "" {
		t.Error("a second domain went unannounced, so one pair's taint would be the only one on record")
	}
}

func hasUnclaimed(c Census, reason, bitmap string) bool {
	for _, u := range c.Unclaimed {
		if u.Reason == reason && contains(u.Bitmaps, bitmap) {
			return true
		}
	}
	return false
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
