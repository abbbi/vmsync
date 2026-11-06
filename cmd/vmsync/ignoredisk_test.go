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

// The refusal, and the four ways an operator can get the flags wrong.
//
// Every case here is about a message: the checks themselves are a few lines,
// and what decides whether they help is whether the reader is told which disk,
// in what format, and which of the two flags does what. So the assertions are
// on the words, not only on the error being non-nil.
package main

import (
	"strings"
	"testing"
	"vmsync/pkg/disk"
)

func sets(ignore, drop string) (*diskSet, *diskSet) {
	i, d := &diskSet{}, &diskSet{}
	if ignore != "" {
		if err := i.Set(ignore); err != nil {
			panic(err)
		}
	}
	if drop != "" {
		if err := d.Set(drop); err != nil {
			panic(err)
		}
	}
	return i, d
}

// A domain with a qcow2 vda, a raw vdb, and a cdrom.
func mixedSelection(ignore, drop string) diskSelection {
	i, d := sets(ignore, drop)
	return diskSelection{
		domain: "web01",
		all:    []string{"vda", "vdb", "sr0"},
		unreplicable: []disk.UnreplicableDisk{
			{TargetDev: "vdb", Device: "disk", Format: "raw", Source: "/data/web01-vdb.raw"},
			{TargetDev: "sr0", Device: "cdrom", Format: "raw", Source: "/isos/x.iso"},
		},
		ignore: i,
		drop:   d,
	}
}

func TestADiskSetTakesCommasAndRepeats(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want string
	}{
		{"one device", []string{"vdb"}, "vdb"},
		{"a comma-separated list in one flag", []string{"vdb,vdc"}, "vdb,vdc"},
		{"the flag repeated", []string{"vdb", "vdc"}, "vdb,vdc"},
		{"both at once", []string{"vdb,vdc", "sdd"}, "vdb,vdc,sdd"},
		{"spaces around the names", []string{" vdb , vdc "}, "vdb,vdc"},
		{"a repeated name is a set, not an error", []string{"vdb", "vdb"}, "vdb"},
		{"a repeat inside one list too", []string{"vdb,vdb,vdc"}, "vdb,vdc"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &diskSet{}
			for _, v := range c.in {
				if err := s.Set(v); err != nil {
					t.Fatalf("Set(%q): %v", v, err)
				}
			}
			if got := s.String(); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// THE MISTAKE WORTH REFUSING. The flag's argument is a disk, so a path is what
// an operator reaches for -- and a path would match no device, leaving the run
// refusing for the very disk they had just excluded.
func TestADiskSetRefusesAnythingThatIsNotADeviceName(t *testing.T) {
	for _, bad := range []string{
		"/data/web01-vdb.raw",
		"/dev/mapper/san-lun0",
		"vdb vdc",
		"vdb\tvdc",
		`C:\disks\vdb`,
		"",
		"vdb,",
		",",
	} {
		t.Run(bad, func(t *testing.T) {
			s := &diskSet{}
			if err := s.Set(bad); err == nil {
				t.Errorf("Set(%q) was accepted; it names no device and would silently exclude nothing", bad)
			}
		})
	}
}

func TestTheRefusalFiresForAnUnaccountedRawDisk(t *testing.T) {
	err := mixedSelection("", "").check()
	if err == nil {
		t.Fatal("a domain with an unignored raw disk was accepted; its replica would be missing data and report success")
	}
	for _, want := range []string{
		"web01",               // which domain
		"vdb",                 // which disk
		"format raw",          // why it cannot be replicated
		"/data/web01-vdb.raw", // which volume, for a domain with several
		"-ignore-disk",
		"-drop-disk",
		"missing data", // what is at stake
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, err)
		}
	}
}

// The difference between the two flags is the whole reason there are two, so
// the message that offers them has to say what it is.
func TestTheRefusalSaysWhatEachFlagDoesDifferently(t *testing.T) {
	err := mixedSelection("", "").check()
	if err == nil {
		t.Fatal("expected a refusal")
	}
	msg := err.Error()
	if !strings.Contains(msg, "keep it in the replica's definition") {
		t.Errorf("the refusal does not say that -ignore-disk keeps the disk in the definition:\n%s", msg)
	}
	if !strings.Contains(msg, "remove it from that definition") {
		t.Errorf("the refusal does not say that -drop-disk removes it:\n%s", msg)
	}
}

func TestEitherFlagSatisfiesTheRefusal(t *testing.T) {
	if err := mixedSelection("vdb", "").check(); err != nil {
		t.Errorf("-ignore-disk=vdb did not account for vdb: %v", err)
	}
	if err := mixedSelection("", "vdb").check(); err != nil {
		t.Errorf("-drop-disk=vdb did not account for vdb: %v", err)
	}
}

// A cdrom is on nearly every domain and carries no replicated data. Refusing
// over one would make this a check operators switch off rather than read.
func TestACdromIsNotARefusal(t *testing.T) {
	s := diskSelection{
		domain:       "web01",
		all:          []string{"vda", "sr0"},
		unreplicable: []disk.UnreplicableDisk{{TargetDev: "sr0", Device: "cdrom", Format: "raw"}},
	}
	s.ignore, s.drop = sets("", "")
	if err := s.check(); err != nil {
		t.Errorf("a domain whose only unreplicable device is a cdrom was refused: %v", err)
	}
}

// Any disk by name, qcow2 or not: "do not replicate the scratch volume" is the
// same operation as "cannot replicate the raw one".
func TestAQcow2DiskCanBeExcludedToo(t *testing.T) {
	s := diskSelection{domain: "web01", all: []string{"vda", "vdb"}}
	s.ignore, s.drop = sets("vdb", "")
	if err := s.check(); err != nil {
		t.Errorf("-ignore-disk on a replicable qcow2 disk was refused: %v", err)
	}
}

func TestATypoIsRefusedAndNamesTheDisksTheDomainHas(t *testing.T) {
	s := mixedSelection("vbd", "") // vdb, transposed
	err := s.check()
	if err == nil {
		t.Fatal("a device the domain does not have was accepted; vdb would then be unaccounted for and the run would refuse for it instead")
	}
	for _, want := range []string{"vbd", "vda, vdb, sr0", "dumpxml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, err)
		}
	}
}

// Order matters: the operator who misspelled vdb must be told THAT, not told
// again about the disk they thought they had just excluded.
func TestATypoIsReportedBeforeTheDiskItFailedToExclude(t *testing.T) {
	err := mixedSelection("vbd", "").check()
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "no such disk") {
		t.Errorf("the typo was not the first thing reported:\n%s", err)
	}
}

func TestADiskInBothFlagsIsRefused(t *testing.T) {
	err := mixedSelection("vdb", "vdb").check()
	if err == nil {
		t.Fatal("vdb in both flags was accepted; one of the two flags would have done nothing")
	}
	for _, want := range []string{"vdb", "-ignore-disk", "-drop-disk", "Pick one"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, err)
		}
	}
}

// Excluding everything would otherwise SUCCEED: every stage iterates the disk
// list, and an empty list iterates nothing. The run would record a checkpoint,
// define a replica, and exit 0 having copied nothing.
func TestARunWithNothingLeftToCopyIsRefused(t *testing.T) {
	s := diskSelection{domain: "web01", all: []string{"vda", "vdb"}}
	s.ignore, s.drop = sets("vda,vdb", "")
	err := s.check()
	if err == nil {
		t.Fatal("a run excluding every disk was accepted; it would copy nothing and still record a replica")
	}
	for _, want := range []string{"copy nothing", "exclude"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, err)
		}
	}
}

func TestOneReplicableDiskLeftIsEnough(t *testing.T) {
	s := diskSelection{domain: "web01", all: []string{"vda", "vdb"}}
	s.ignore, s.drop = sets("vdb", "")
	if err := s.check(); err != nil {
		t.Errorf("a run with vda still to copy was refused: %v", err)
	}
}

// A domain whose only disk is raw and excluded: nothing to copy, by the other
// route. The two refusals must not disagree about it.
func TestADomainOfOnlyExcludedRawDisksIsRefused(t *testing.T) {
	s := diskSelection{
		domain:       "web01",
		all:          []string{"vdb"},
		unreplicable: []disk.UnreplicableDisk{{TargetDev: "vdb", Device: "disk", Format: "raw"}},
	}
	s.ignore, s.drop = sets("vdb", "")
	if err := s.check(); err == nil {
		t.Error("a domain with one raw disk, excluded, was accepted: the run would copy nothing")
	}
}

// A <disk> with no <target dev=> cannot be named by either flag, so the
// refusal has to say that rather than offering a flag that cannot be written.
func TestADiskWithNoTargetDevSaysNoFlagCanNameIt(t *testing.T) {
	s := diskSelection{
		domain:       "web01",
		all:          []string{"vda"},
		unreplicable: []disk.UnreplicableDisk{{TargetDev: "", Device: "disk", Format: "raw", Source: "/data/orphan.raw"}},
	}
	s.ignore, s.drop = sets("", "")
	err := s.check()
	if err == nil {
		t.Fatal("a disk with no <target dev=> was accepted")
	}
	for _, want := range []string{"no <target dev=>", "no flag can name it"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, err)
		}
	}
}

func TestAHealthyDomainIsNotRefused(t *testing.T) {
	s := diskSelection{domain: "web01", all: []string{"vda", "vdc"}}
	s.ignore, s.drop = sets("", "")
	if err := s.check(); err != nil {
		t.Errorf("a domain whose every disk is qcow2 was refused: %v", err)
	}
}

// Every stage of a run has to iterate the SAME set, or the replica's
// checkpoint claims disks its definition does not have.
func TestReplicatedDisksRemovesExactlyWhatWasExcluded(t *testing.T) {
	all := []disk.QcowDisk{
		{TargetDev: "vda", Source: "/data/a.qcow2"},
		{TargetDev: "vdb", Source: "/data/b.qcow2"},
		{TargetDev: "vdc", Source: "/data/c.qcow2"},
	}
	i, d := sets("vdb", "vdc")
	got := replicatedDisks(all, i, d)
	if len(got) != 1 || got[0].TargetDev != "vda" {
		t.Fatalf("replicatedDisks returned %+v, want vda only", got)
	}
	// The caller's own list must survive: the refusal and the per-run warning
	// describe the full set, and a filter that aliased the input would leave
	// them describing the filtered one.
	if len(all) != 3 {
		t.Errorf("the input slice was modified: now %+v", all)
	}
}

func TestReplicatedDisksKeepsEverythingWhenNothingIsExcluded(t *testing.T) {
	all := []disk.QcowDisk{{TargetDev: "vda"}, {TargetDev: "vdb"}}
	i, d := sets("", "")
	if got := replicatedDisks(all, i, d); len(got) != 2 {
		t.Errorf("replicatedDisks dropped a disk with both sets empty: %+v", got)
	}
}

// Recorded on the replica and compared between runs, so it must not reorder
// itself: a set that changes spelling reads as a set that changed.
func TestTheExcludedSetIsSortedAndMergesBothFlags(t *testing.T) {
	s := diskSelection{domain: "web01", all: []string{"vda", "vdb", "vdc", "sdd"}}
	s.ignore, s.drop = sets("vdc,vdb", "sdd")
	got := strings.Join(s.excludedDevices(), ",")
	if got != "sdd,vdb,vdc" {
		t.Errorf("excludedDevices = %q, want sdd,vdb,vdc", got)
	}
}
