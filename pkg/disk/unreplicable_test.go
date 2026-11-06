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

// The other half of ParseQcowDisks: the devices it drops.
//
// What has to hold is that the two functions PARTITION a domain's disks. A
// disk missing from both is one vmsync neither copies nor mentions, which is
// the exact state an operator cannot discover -- so the partition is asserted
// directly, on every fixture below, rather than left to the two functions
// happening to apply the same rule.
package disk

import (
	"strings"
	"testing"
)

const mixedDomainXML = `<domain type='kvm'>
  <name>vm01</name>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='/data/vm01-vda.qcow2'/>
      <target dev='vda'/>
    </disk>
    <disk type='file' device='disk'>
      <driver name='qemu' type='raw'/>
      <source file='/data/vm01-vdb.raw'/>
      <target dev='vdb'/>
    </disk>
    <disk type='block' device='lun'>
      <driver name='qemu' type='raw'/>
      <source dev='/dev/mapper/san-lun0'/>
      <target dev='sdc'/>
    </disk>
    <disk type='file' device='disk'>
      <target dev='vdd'/>
    </disk>
    <disk type='file' device='cdrom'>
      <driver name='qemu' type='raw'/>
      <source file='/isos/debian.iso'/>
      <target dev='sr0'/>
    </disk>
  </devices>
</domain>`

// devsOf is the set of target devs in a result, for the partition assertions.
func devsOf(disks []UnreplicableDisk) map[string]UnreplicableDisk {
	out := map[string]UnreplicableDisk{}
	for _, d := range disks {
		out[d.TargetDev] = d
	}
	return out
}

func TestUnreplicableDisksReportsEveryDeviceParseQcowDisksDrops(t *testing.T) {
	skipped, err := UnreplicableDisks(mixedDomainXML)
	if err != nil {
		t.Fatalf("UnreplicableDisks: %v", err)
	}
	got := devsOf(skipped)
	for _, want := range []string{"vdb", "sdc", "vdd", "sr0"} {
		if _, ok := got[want]; !ok {
			t.Errorf("%s is not reported as unreplicable; it is not replicated either, so nothing would name it", want)
		}
	}
	if _, ok := got["vda"]; ok {
		t.Errorf("vda is qcow2 and IS replicated, but it was reported as unreplicable")
	}
	if len(skipped) != 4 {
		t.Fatalf("got %d unreplicable disks, want 4: %+v", len(skipped), skipped)
	}
}

// THE PROPERTY THE FEATURE RESTS ON. Every disk the domain declares is in
// exactly one of the two sets, so "replicated" and "reported" together cover
// the domain with no gap and no overlap.
func TestTheTwoSetsPartitionTheDomainsDisks(t *testing.T) {
	replicated, err := ParseQcowDisks(mixedDomainXML)
	if err != nil {
		t.Fatalf("ParseQcowDisks: %v", err)
	}
	skipped, err := UnreplicableDisks(mixedDomainXML)
	if err != nil {
		t.Fatalf("UnreplicableDisks: %v", err)
	}
	all, err := TargetDevs(mixedDomainXML)
	if err != nil {
		t.Fatalf("TargetDevs: %v", err)
	}

	seen := map[string]int{}
	for _, d := range replicated {
		seen[d.TargetDev]++
	}
	for _, d := range skipped {
		seen[d.TargetDev]++
	}
	for _, dev := range all {
		switch seen[dev] {
		case 1:
		case 0:
			t.Errorf("%s is in neither set: vmsync would neither replicate it nor say it had not", dev)
		default:
			t.Errorf("%s is in both sets %d times; a disk cannot be both replicated and skipped", dev, seen[dev])
		}
	}
	if len(seen) != len(all) {
		t.Errorf("the two sets name %d devices, the domain declares %d", len(seen), len(all))
	}
}

func TestUnreplicableDiskCarriesWhatARefusalHasToSay(t *testing.T) {
	skipped, err := UnreplicableDisks(mixedDomainXML)
	if err != nil {
		t.Fatalf("UnreplicableDisks: %v", err)
	}
	got := devsOf(skipped)

	// The format, because "vdb cannot be replicated" without it reads as a
	// vmsync defect rather than as a fact about the domain.
	if got["vdb"].Format != "raw" {
		t.Errorf("vdb reports format %q, want raw", got["vdb"].Format)
	}
	// A disk declaring no <driver> at all: the schema allows it and it cannot
	// be confirmed qcow2. "none" rather than "" so the message never says
	// `format ""`.
	if got["vdd"].Format != "none" {
		t.Errorf("a disk with no <driver> reports format %q, want none", got["vdd"].Format)
	}
	// The device kind, because a cdrom is expected and a lun is not, and the
	// caller decides which of those is a refusal.
	if got["sr0"].Device != "cdrom" {
		t.Errorf("sr0 reports device %q, want cdrom", got["sr0"].Device)
	}
	if got["sdc"].Device != "lun" {
		t.Errorf("sdc reports device %q, want lun", got["sdc"].Device)
	}
	// The path, so a refusal on a domain with several raw disks says which
	// volume it means.
	if got["vdb"].Source != "/data/vm01-vdb.raw" {
		t.Errorf("vdb reports source %q, want the declared file", got["vdb"].Source)
	}
	// A block disk has <source dev=>, not <source file=>: empty rather than
	// wrong, and the device name still identifies it.
	if got["sdc"].Source != "" {
		t.Errorf("a block-backed disk reports source %q; only <source file=> is carried", got["sdc"].Source)
	}
}

// A <disk> with no <target> cannot be named by any flag. It is still reported,
// with an empty dev, because a disk nobody can opt out of is one a refusal has
// to describe some other way -- silently dropping it would leave a domain that
// can never be synced and no message saying why.
func TestADiskWithNoTargetIsStillReported(t *testing.T) {
	const xml = `<domain type='kvm'>
  <name>vm03</name>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='raw'/>
      <source file='/data/orphan.raw'/>
    </disk>
  </devices>
</domain>`
	skipped, err := UnreplicableDisks(xml)
	if err != nil {
		t.Fatalf("UnreplicableDisks: %v", err)
	}
	if len(skipped) != 1 {
		t.Fatalf("got %d unreplicable disks, want 1", len(skipped))
	}
	if skipped[0].TargetDev != "" {
		t.Errorf("TargetDev is %q, want empty for a disk with no <target>", skipped[0].TargetDev)
	}
	devs, err := TargetDevs(xml)
	if err != nil {
		t.Fatalf("TargetDevs: %v", err)
	}
	if len(devs) != 0 {
		t.Errorf("TargetDevs returned %v; a disk with no <target> has no name to return", devs)
	}
}

func TestADomainWithOnlyReplicableDisksReportsNothingUnreplicable(t *testing.T) {
	const xml = `<domain type='kvm'>
  <name>vm04</name>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='/data/a.qcow2'/>
      <target dev='vda'/>
    </disk>
  </devices>
</domain>`
	skipped, err := UnreplicableDisks(xml)
	if err != nil {
		t.Fatalf("UnreplicableDisks: %v", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("got %+v, want nothing: every disk here is qcow2", skipped)
	}
}

func TestTargetDevsListsEveryDeclaredDiskInOrder(t *testing.T) {
	devs, err := TargetDevs(mixedDomainXML)
	if err != nil {
		t.Fatalf("TargetDevs: %v", err)
	}
	want := "vda vdb sdc vdd sr0"
	if got := strings.Join(devs, " "); got != want {
		t.Errorf("TargetDevs = %q, want %q -- declaration order, so a message listing them matches the XML a reader is looking at", got, want)
	}
}

func TestMalformedXMLIsAnErrorRatherThanAnEmptySet(t *testing.T) {
	// An empty set reads as "this domain has no unreplicable disks", which
	// would turn a parse failure into permission to sync.
	if _, err := UnreplicableDisks("<domain"); err == nil {
		t.Error("UnreplicableDisks accepted malformed XML and returned no disks")
	}
	if _, err := TargetDevs("<domain"); err == nil {
		t.Error("TargetDevs accepted malformed XML and returned no devices")
	}
}
