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

// How a scheduled sync says which disks it does not replicate.
//
// vmsync refuses a domain carrying a disk it cannot copy, so without these two
// entry fields an agent-driven pair whose vdb is raw is refused on every
// interval, for ever, with nothing an operator can set from the console to say
// it was meant.
package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestAnEntrysDiskExclusionsReachTheCommandLine(t *testing.T) {
	req := SyncRequest{
		SourceURI:    "qemu:///system",
		SourceDomain: "web01",
		TargetURI:    "qemu+ssh://hv02/system",
		TargetDomain: "web01",
		IgnoreDisks:  []string{"vdb"},
		DropDisks:    []string{"sdc", "sdd"},
	}
	args := strings.Join(req.CommandArgs(), " ")
	for _, want := range []string{"-ignore-disk=vdb", "-drop-disk=sdc", "-drop-disk=sdd"} {
		if !strings.Contains(args, want) {
			t.Errorf("CommandArgs omits %q:\n%s", want, args)
		}
	}
}

// THE "=" FORM, and it is not a style choice. A space-separated value is only
// safe for a flag Go does not treat as boolean; getting that wrong leaves the
// value as a positional argument vmsync rejects outright, which stops the pair
// replicating rather than failing visibly in the agent.
func TestTheDiskFlagsUseTheEqualsForm(t *testing.T) {
	req := SyncRequest{IgnoreDisks: []string{"vdb"}, DropDisks: []string{"sdc"}}
	for _, arg := range req.CommandArgs() {
		if arg == "-ignore-disk" || arg == "-drop-disk" {
			t.Fatalf("%s was emitted as a bare flag with its value in the next argument; vmsync would take that value as a positional argument and refuse the run", arg)
		}
	}
}

// One flag per device, even though the engine's flag accepts a comma list, so
// its refusal can quote back a single bad device name.
func TestEachDeviceGetsItsOwnFlag(t *testing.T) {
	req := SyncRequest{IgnoreDisks: []string{"vdb", "vdc"}}
	n := 0
	for _, arg := range req.CommandArgs() {
		if strings.HasPrefix(arg, "-ignore-disk=") {
			n++
		}
		if strings.Contains(arg, ",") && strings.HasPrefix(arg, "-ignore-disk") {
			t.Errorf("the devices were comma-joined into %q", arg)
		}
	}
	if n != 2 {
		t.Errorf("got %d -ignore-disk flags for two devices", n)
	}
}

// An ordinary pair must emit neither flag: every estate is mostly these, and a
// flag appearing where nothing was configured would change what every existing
// scheduled run executes.
func TestAnOrdinaryEntryEmitsNeitherFlag(t *testing.T) {
	req := SyncRequest{SourceDomain: "web01"}
	args := strings.Join(req.CommandArgs(), " ")
	if strings.Contains(args, "ignore-disk") || strings.Contains(args, "drop-disk") {
		t.Errorf("a request with no exclusions emitted one:\n%s", args)
	}
}

// Shape, checked where a schedule is SAVED rather than where it runs: a
// transposed letter caught here costs a correction, and the same letter caught
// at the target costs a pair that stops replicating at 02:00.
func TestAnEntryValidatesTheShapeOfItsDiskNames(t *testing.T) {
	cases := []struct {
		name  string
		entry ScheduleEntry
		ok    bool
	}{
		{"an ordinary device", ScheduleEntry{IgnoreDisks: []string{"vdb"}}, true},
		{"several", ScheduleEntry{IgnoreDisks: []string{"vdb", "sdc"}}, true},
		{"an nvme namespace", ScheduleEntry{DropDisks: []string{"nvme0n1"}}, true},
		{"nothing at all", ScheduleEntry{}, true},
		{"a path", ScheduleEntry{IgnoreDisks: []string{"/dev/mapper/lun0"}}, false},
		{"a path in drop_disks", ScheduleEntry{DropDisks: []string{"/data/x.raw"}}, false},
		{"an empty name", ScheduleEntry{IgnoreDisks: []string{""}}, false},
		{"two names in one string", ScheduleEntry{IgnoreDisks: []string{"vdb vdc"}}, false},
		{"a comma list, which belongs in separate entries", ScheduleEntry{IgnoreDisks: []string{"vdb,vdc"}}, false},
		{"an upper-case name", ScheduleEntry{IgnoreDisks: []string{"VDB"}}, false},
		{"the same device twice", ScheduleEntry{IgnoreDisks: []string{"vdb", "vdb"}}, false},
		// The two flags ask for opposite things about the replica's
		// definition, so there is no sensible precedence to pick.
		{"the same device in both fields", ScheduleEntry{IgnoreDisks: []string{"vdb"}, DropDisks: []string{"vdb"}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.entry.Validate()
			if c.ok && err != nil {
				t.Errorf("rejected: %v", err)
			}
			if !c.ok && err == nil {
				t.Error("accepted; the engine would then refuse the sync for a device it cannot find, which reads as the feature not working")
			}
		})
	}
}

// Validate must still carry the profile's own checks, or swapping the call
// sites from Profile.Validate to entry.Validate would have silently dropped
// every one of them.
func TestAnEntryStillValidatesItsProfile(t *testing.T) {
	bad := ScheduleEntry{Profile: SyncProfile{Compress: "lzop"}}
	if err := bad.Validate(); err == nil {
		t.Fatal("an entry with an unknown compress algorithm was accepted; the profile's own validation is no longer reached")
	}
}

// A device letter belongs to ONE machine. If the lists were ever moved onto
// SyncProfile they would be template-inherited, and an operator who excluded
// vdb on one database VM would silently stop replicating a different disk on
// every other VM the template covered -- with every one of those syncs still
// succeeding. This pins where the fields live.
func TestTheExclusionsAreOnTheEntryNotTheProfile(t *testing.T) {
	entry := ScheduleEntry{IgnoreDisks: []string{"vdb"}}
	if len(entry.IgnoreDisks) != 1 {
		t.Fatal("ScheduleEntry has no IgnoreDisks field")
	}
	// A profile carrying the same name would be the regression, so this
	// asserts the template-inheritable type has not grown one. By reflection
	// rather than by reading the source, so it holds whatever the field is
	// called next door.
	profile := reflect.TypeOf(SyncProfile{})
	for i := 0; i < profile.NumField(); i++ {
		name := profile.Field(i).Name
		if strings.Contains(name, "IgnoreDisk") || strings.Contains(name, "DropDisk") {
			t.Errorf("SyncProfile has gained %s. resolveProfile inherits a profile to every VM its template covers, and a device letter means a different disk on each of them -- so the exclusion would silently stop replicating a healthy disk estate-wide, with every one of those syncs still reporting success", name)
		}
	}
}
