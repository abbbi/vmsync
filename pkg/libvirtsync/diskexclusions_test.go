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

// What the replica's definition says about a disk it does not have.
//
// The two flags differ ONLY here, so these are the tests that give them
// separate meanings: an ignored disk keeps its element and its original path,
// a dropped one is gone. Everything else about them is identical.
package libvirtsync

import (
	"strings"
	"testing"
)

const exclusionDomainXML = `<domain type='kvm'>
  <name>web01</name>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='/src/web01-vda.qcow2'/>
      <target dev='vda' bus='virtio'/>
      <boot order='1'/>
    </disk>
    <disk type='file' device='disk'>
      <driver name='qemu' type='raw'/>
      <source file='/src/web01-vdb.raw'/>
      <target dev='vdb' bus='virtio'/>
    </disk>
    <disk type='file' device='cdrom'>
      <driver name='qemu' type='raw'/>
      <source file='/isos/debian.iso'/>
      <target dev='sr0' bus='sata'/>
    </disk>
  </devices>
</domain>`

func TestDiskExclusionsDevices(t *testing.T) {
	e := DiskExclusions{Ignored: []string{"vdc", "vdb"}, Dropped: []string{"sdd", "vdb"}}
	if got := strings.Join(e.Devices(), ","); got != "sdd,vdb,vdc" {
		t.Errorf("Devices() = %q, want sdd,vdb,vdc -- sorted, because this value is compared between runs", got)
	}
	if (DiskExclusions{}).Empty() != true {
		t.Error("an empty DiskExclusions does not report itself empty")
	}
	if e.Empty() {
		t.Error("a populated DiskExclusions reports itself empty")
	}
}

// The assertion that earns the right to move the configuration-loss baseline.
func TestSameDeviceSetIgnoresOrderAndRepeats(t *testing.T) {
	cases := []struct {
		a, b []string
		want bool
	}{
		{[]string{"vdb"}, []string{"vdb"}, true},
		{[]string{"vdb", "sdc"}, []string{"sdc", "vdb"}, true},
		{[]string{"vdb", "vdb"}, []string{"vdb"}, true},
		{[]string{"vdb"}, []string{"vdc"}, false},
		{[]string{"vdb"}, []string{"vdb", "sdc"}, false},
		{[]string{"vdb", "sdc"}, []string{"vdb"}, false},
		{nil, nil, true},
		{[]string{"vdb"}, nil, false},
	}
	for _, c := range cases {
		if got := sameDeviceSet(c.a, c.b); got != c.want {
			t.Errorf("sameDeviceSet(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestRemoveDomainDisksRemovesTheNamedDiskAndReportsIt(t *testing.T) {
	out, removed, err := removeDomainDisks(exclusionDomainXML, []string{"vdb"})
	if err != nil {
		t.Fatalf("removeDomainDisks: %v", err)
	}
	if strings.Join(removed, ",") != "vdb" {
		t.Errorf("removed = %v, want [vdb] -- the caller asserts this against what it asked for", removed)
	}
	if strings.Contains(out, "web01-vdb.raw") {
		t.Errorf("the dropped disk's source survived in the definition:\n%s", out)
	}
	if strings.Contains(out, `dev="vdb"`) {
		t.Errorf("the dropped disk's <target> survived in the definition:\n%s", out)
	}
	// Everything else must be untouched, including the cdrom and the boot
	// order on the disk that stays.
	for _, want := range []string{"web01-vda.qcow2", `dev="vda"`, "debian.iso", `boot order="1"`} {
		if !strings.Contains(out, want) {
			t.Errorf("removing vdb also removed %q:\n%s", want, out)
		}
	}
}

// A name the document does not carry comes back absent from the removed set,
// which is what lets the caller refuse rather than define a replica that still
// has the disk.
func TestRemoveDomainDisksReportsOnlyWhatItActuallyRemoved(t *testing.T) {
	_, removed, err := removeDomainDisks(exclusionDomainXML, []string{"vdz"})
	if err != nil {
		t.Fatalf("removeDomainDisks: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("removed = %v for a device the domain does not have", removed)
	}
	if sameDeviceSet(removed, []string{"vdz"}) {
		t.Error("the mismatch is not detectable, so a caller could not refuse on it")
	}
}

func TestRemoveDomainDisksIsANoOpForAnEmptyList(t *testing.T) {
	out, removed, err := removeDomainDisks(exclusionDomainXML, nil)
	if err != nil {
		t.Fatalf("removeDomainDisks: %v", err)
	}
	if out != exclusionDomainXML {
		t.Error("an empty list rewrote the document; every ordinary pair takes this path")
	}
	if removed != nil {
		t.Errorf("removed = %v for an empty request", removed)
	}
}

func TestRemoveDomainDisksRefusesADocumentWithNoDevices(t *testing.T) {
	if _, _, err := removeDomainDisks(`<domain type='kvm'><name>x</name></domain>`, []string{"vdb"}); err == nil {
		t.Error("a domain with no <devices> accepted a removal; the caller would then define a replica believing the disk was gone")
	}
}

// THE DIFFERENCE BETWEEN THE TWO FLAGS. An ignored disk keeps its element AND
// its original path: the storage is provided on the target by other means,
// under the name the source already uses.
func TestAnIgnoredDiskKeepsItsElementAndItsPath(t *testing.T) {
	out, err := replaceDomainDiskPath(exclusionDomainXML, "/replicas",
		map[string]string{"/src/web01-vda.qcow2": "/src/web01-vda.qcow2"}, []string{"vdb"})
	if err != nil {
		t.Fatalf("replaceDomainDiskPath: %v", err)
	}
	if !strings.Contains(out, "/src/web01-vdb.raw") {
		t.Errorf("the ignored disk's own path was rewritten or removed; -ignore-disk means vmsync touches it in no way:\n%s", out)
	}
	// And the replicated disk IS repathed, so the test cannot pass by the
	// function doing nothing at all.
	if !strings.Contains(out, "/replicas/web01-vda.qcow2") {
		t.Errorf("the replicated disk was not repathed, so this test proves nothing:\n%s", out)
	}
}

// An ignored QCOW2 disk is the case that breaks a naive implementation: it is
// not caught by the unreplicable branch, so without the ignore set it reaches
// the root-source lookup, finds nothing -- the map is built only from disks
// that were copied -- and fails the whole sync at its last step.
func TestAnIgnoredQcow2DiskDoesNotFailTheDefine(t *testing.T) {
	const twoQcow = `<domain type='kvm'>
  <name>web01</name>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='/src/a.qcow2'/>
      <target dev='vda'/>
    </disk>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='/src/b.qcow2'/>
      <target dev='vdb'/>
    </disk>
  </devices>
</domain>`
	rootMap := map[string]string{"/src/a.qcow2": "/src/a.qcow2"} // vdb was not copied
	out, err := replaceDomainDiskPath(twoQcow, "/replicas", rootMap, []string{"vdb"})
	if err != nil {
		t.Fatalf("an ignored qcow2 disk failed the define: %v", err)
	}
	if !strings.Contains(out, "/src/b.qcow2") {
		t.Errorf("the ignored qcow2 disk's path was rewritten:\n%s", out)
	}
	// The control: without the ignore set, the same document must still be
	// refused, or the backstop below has been lost.
	if _, err := replaceDomainDiskPath(twoQcow, "/replicas", rootMap, nil); err == nil {
		t.Error("a qcow2 disk with no copied path was accepted; the replica would name a file nobody wrote")
	}
}

// THE BACKSTOP. It is never supposed to fire -- the gate in cmd/vmsync refuses
// long before any copy -- but a definition naming a disk nobody copied is the
// one thing this package must not be able to produce.
func TestAnUnaccountedNonQcow2DiskIsRefusedAtTheDefine(t *testing.T) {
	_, err := replaceDomainDiskPath(exclusionDomainXML, "/replicas",
		map[string]string{"/src/web01-vda.qcow2": "/src/web01-vda.qcow2"}, nil)
	if err == nil {
		t.Fatal("a raw disk named in neither flag produced a definition; on a target where that path resolves the replica would open a file nobody replicated")
	}
	for _, want := range []string{"vdb", "-ignore-disk", "-drop-disk"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// A cdrom stays verbatim, ISO path included. Pre-existing and deliberate: it
// is a decision about empty trays versus dangling paths, not about which disks
// get replicated. Pinned so a later change to the classifier cannot alter it
// by accident.
func TestACdromIsLeftVerbatim(t *testing.T) {
	out, err := replaceDomainDiskPath(exclusionDomainXML, "/replicas",
		map[string]string{"/src/web01-vda.qcow2": "/src/web01-vda.qcow2"}, []string{"vdb"})
	if err != nil {
		t.Fatalf("replaceDomainDiskPath: %v", err)
	}
	if !strings.Contains(out, "/isos/debian.iso") {
		t.Errorf("the cdrom's source was rewritten or removed:\n%s", out)
	}
}

func TestDispositionOfSplitsTheThreeCases(t *testing.T) {
	doc, err := parseDomainDoc(exclusionDomainXML)
	if err != nil {
		t.Fatalf("parseDomainDoc: %v", err)
	}
	want := map[string]diskDisposition{
		"vda": dispositionReplicate,
		"vdb": dispositionUnreplicable,
		"sr0": dispositionCdrom,
	}
	seen := 0
	for _, d := range doc.Root().FindElements("./devices/disk") {
		dev := diskTargetDev(d)
		if got := dispositionOf(d); got != want[dev] {
			t.Errorf("%s classified %d, want %d", dev, got, want[dev])
		}
		seen++
	}
	if seen != 3 {
		t.Fatalf("walked %d disks, want 3", seen)
	}
}

// A <disk> with no <target> has no name any flag can use. It must not be
// matched by a removal, or an operator's -drop-disk=vdb could take out a
// different, unnamed device.
func TestADiskWithNoTargetDevIsNeverRemovedByName(t *testing.T) {
	const xml = `<domain type='kvm'>
  <name>web01</name>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='raw'/>
      <source file='/src/orphan.raw'/>
    </disk>
  </devices>
</domain>`
	out, removed, err := removeDomainDisks(xml, []string{"vdb"})
	if err != nil {
		t.Fatalf("removeDomainDisks: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("removed = %v; a disk with no <target dev=> has no name to match", removed)
	}
	if !strings.Contains(out, "orphan.raw") {
		t.Errorf("the unnamed disk was removed anyway:\n%s", out)
	}
	doc, err := parseDomainDoc(xml)
	if err != nil {
		t.Fatalf("parseDomainDoc: %v", err)
	}
	if dev := diskTargetDev(doc.Root().FindElements("./devices/disk")[0]); dev != "" {
		t.Errorf("diskTargetDev invented the name %q for a disk with no <target>", dev)
	}

	// AND AN EMPTY REQUEST MUST MATCH NOTHING, which is the case the dev == ""
	// guard exists for and the only one that can reach it. DefineDomain is
	// exported and takes DiskExclusions from any caller; vmsync's own flags and
	// the agent's schedule validation both reject an empty device name, so
	// without this the guard looks redundant -- and removing it would silently
	// strip every <disk> that declares no <target> from the replica.
	out, removed, err = removeDomainDisks(xml, []string{""})
	if err != nil {
		t.Fatalf("removeDomainDisks with an empty name: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("removed = %v for an empty device name", removed)
	}
	if !strings.Contains(out, "orphan.raw") {
		t.Errorf("an empty device name removed a disk that has no <target dev=>:\n%s", out)
	}
}

// Both lists reach the replica's metadata, and a run that excludes nothing
// actively REMOVES them -- the set-or-remove branch, without which a source
// that was once somebody's replica stamps its own device letters onto a
// replica of a different domain.
func TestTheExcludedListsAreRecordedAndCleared(t *testing.T) {
	base := minimalDomainXML("testvm", "12345678-1234-1234-1234-123456789abc", "/var/lib/libvirt/images/x.qcow2")

	withLists, err := UpdateSyncMetadata(base, "vmsync-cpt-000001", "src-host", "testvm", RoleTarget,
		1700000000, false, "", AutostartIntentUnknown,
		DiskExclusions{Ignored: []string{"vdb"}, Dropped: []string{"sdc", "sdd"}})
	if err != nil {
		t.Fatalf("UpdateSyncMetadata: %v", err)
	}
	if got, _ := ParseMetadataField(withLists, MetadataFieldIgnoredDisks); got != "vdb" {
		t.Errorf("%s = %q, want vdb", MetadataFieldIgnoredDisks, got)
	}
	if got, _ := ParseMetadataField(withLists, MetadataFieldDroppedDisks); got != "sdc,sdd" {
		t.Errorf("%s = %q, want sdc,sdd", MetadataFieldDroppedDisks, got)
	}

	// The next run excludes nothing: both must go, not merely stop being
	// updated. A replica that is whole must not keep reporting a missing disk.
	cleared, err := UpdateSyncMetadata(withLists, "vmsync-cpt-000002", "src-host", "testvm", RoleTarget,
		1700000100, false, "", AutostartIntentUnknown, DiskExclusions{})
	if err != nil {
		t.Fatalf("UpdateSyncMetadata: %v", err)
	}
	for _, f := range []string{MetadataFieldIgnoredDisks, MetadataFieldDroppedDisks} {
		if got, _ := ParseMetadataField(cleared, f); got != "" {
			t.Errorf("%s = %q after a run that excluded nothing; it must be removed, not left behind", f, got)
		}
	}

	// And a dev moved from one flag to the other must move in the record too.
	moved, err := UpdateSyncMetadata(withLists, "vmsync-cpt-000003", "src-host", "testvm", RoleTarget,
		1700000200, false, "", AutostartIntentUnknown, DiskExclusions{Dropped: []string{"vdb"}})
	if err != nil {
		t.Fatalf("UpdateSyncMetadata: %v", err)
	}
	if got, _ := ParseMetadataField(moved, MetadataFieldIgnoredDisks); got != "" {
		t.Errorf("%s = %q after vdb moved to -drop-disk", MetadataFieldIgnoredDisks, got)
	}
	if got, _ := ParseMetadataField(moved, MetadataFieldDroppedDisks); got != "vdb" {
		t.Errorf("%s = %q after vdb moved to -drop-disk, want vdb", MetadataFieldDroppedDisks, got)
	}
}
