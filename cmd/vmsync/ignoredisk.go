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

// Which of a source domain's disks a run replicates, and what it refuses.
//
// THE DEFAULT IS A REFUSAL. A domain carrying a disk vmsync cannot copy -- one
// that is not qcow2, or declares no driver at all -- is a domain whose replica
// is missing data, and until this existed the run said so in one INFO line and
// carried on. The replica then looked complete: it had a definition, a
// checkpoint chain and a green exit code, and it was a VM somebody believed
// was protected.
//
// Two flags say it was meant, and they differ only in what the REPLICA's own
// definition ends up saying about the disk:
//
//   - -ignore-disk=vdb leaves the <disk> exactly as the source declares it.
//     For storage that exists on the target by other means -- a SAN LUN, an
//     array-replicated volume, an LVM device provisioned separately -- that is
//     the whole point: the promoted replica still has the disk, and vmsync
//     neither copies it nor rewrites its path.
//   - -drop-disk=vdb removes the <disk> from the replica's definition. For a
//     scratch or local-only disk, where nothing on the target provides it and
//     a definition naming the source's path would point at a file that is not
//     there.
//
// Both accept any disk by name, qcow2 or not: "do not replicate the scratch
// volume" is the same operation as "cannot replicate the raw one", and one
// rule is easier to reason about than two.
//
// Naming a qcow2 disk in -ignore-disk is what makes the target-XML writer's
// knowledge of these sets load-bearing rather than cosmetic:
// replaceDomainDiskPath refuses to write a definition for a disk it has no
// copied path for, so an ignored qcow2 disk would fail the define unless that
// writer skips it the way it skips a cdrom.
package main

import (
	"fmt"
	"sort"
	"strings"
	"vmsync/pkg/disk"
)

// diskSet is the flag.Value behind -ignore-disk and -drop-disk: repeatable,
// and accepting a comma-separated list in one go.
//
// Both spellings, because an operator writing one of these is working from a
// `virsh dumpxml` and will reach for whichever matches how they think about
// it -- and a flag that accepts only one of the two forms silently takes
// "vdb,vdc" as a single device name nothing matches.
type diskSet struct {
	order []string
	seen  map[string]bool
}

func (s *diskSet) String() string { return strings.Join(s.order, ",") }

// Set splits on commas and refuses anything that is not a bare device name.
//
// The rejection matters more than the acceptance: -ignore-disk=/data/vm.qcow2
// is the mistake an operator makes when the flag's argument is a disk, and a
// path accepted here would match no device, so the run would refuse for the
// very disk the operator had just excluded.
func (s *diskSet) Set(v string) error {
	for _, raw := range strings.Split(v, ",") {
		name := strings.TrimSpace(raw)
		if name == "" {
			return fmt.Errorf("empty device name in %q: the argument is a target device such as vdb, or several separated by commas", v)
		}
		if strings.ContainsAny(name, "/\\ \t") {
			return fmt.Errorf("%q is not a target device name: pass the <target dev=> from the domain's XML, such as vdb, not a path", name)
		}
		if s.seen == nil {
			s.seen = map[string]bool{}
		}
		// Repeating a name is accepted, not refused: the flag is a set, and a
		// config generator emitting vdb twice has said nothing contradictory.
		if s.seen[name] {
			continue
		}
		s.seen[name] = true
		s.order = append(s.order, name)
	}
	return nil
}

func (s *diskSet) Has(dev string) bool { return s.seen[dev] }

func (s *diskSet) Empty() bool { return len(s.order) == 0 }

// Names is the set in the order the operator gave it, so a message quoting it
// back matches what they typed.
func (s *diskSet) Names() []string { return s.order }

// diskSelection is one run's answer to "which disks does this replicate".
type diskSelection struct {
	domain string
	// all is every <target dev=> the domain declares, in XML order.
	all []string
	// unreplicable is everything util.IgnoreDevice drops: cdroms included,
	// since -drop-disk is a legitimate thing to do to a cdrom whose ISO does
	// not exist on the target.
	unreplicable []disk.UnreplicableDisk
	ignore       *diskSet
	drop         *diskSet
}

// check returns the first reason this selection cannot be acted on, or nil.
//
// Ordered deliberately, cheapest mistake first: a typo is reported before the
// refusal it was meant to prevent, so an operator who misspelled vdb is told
// that rather than being told again about the disk they thought they had just
// excluded.
func (s diskSelection) check() error {
	if err := s.unknownDevices(); err != nil {
		return err
	}
	if err := s.conflictingDevices(); err != nil {
		return err
	}
	if err := s.unreplicatedDisks(); err != nil {
		return err
	}
	return s.nothingLeftToReplicate()
}

// unknownDevices refuses a name the domain does not have.
//
// A silently-ignored exclusion is the failure this whole file exists to close:
// -ignore-disk=vbd (transposed) would leave vdb unaccounted for, and on a
// domain with no other unreplicable disk the run would then proceed and
// replicate exactly what the operator had asked it not to.
func (s diskSelection) unknownDevices() error {
	have := map[string]bool{}
	for _, dev := range s.all {
		have[dev] = true
	}
	var bad []string
	for _, flagged := range []struct {
		flag string
		set  *diskSet
	}{{"-ignore-disk", s.ignore}, {"-drop-disk", s.drop}} {
		for _, name := range flagged.set.Names() {
			if !have[name] {
				bad = append(bad, fmt.Sprintf("%s=%s", flagged.flag, name))
			}
		}
	}
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("%s has no such disk: %s. Its disks are %s -- take the names from <target dev=> in `virsh dumpxml %s`",
		s.domain, strings.Join(bad, ", "), listOrNone(s.all), s.domain)
}

// conflictingDevices refuses a disk named in both flags.
//
// They ask for opposite things about the replica's definition -- keep the disk
// as declared, or remove it -- so there is no sensible precedence to pick.
// Choosing one silently would mean the operator's second flag did nothing.
func (s diskSelection) conflictingDevices() error {
	var both []string
	for _, name := range s.ignore.Names() {
		if s.drop.Has(name) {
			both = append(both, name)
		}
	}
	if len(both) == 0 {
		return nil
	}
	return fmt.Errorf("%s is named in both -ignore-disk and -drop-disk on %s: the first keeps the disk in the replica's definition exactly as the source declares it, the second removes it. Pick one",
		strings.Join(both, ", "), s.domain)
}

// unreplicatedDisks is the refusal itself: a disk vmsync cannot copy that
// nobody has said to leave out.
//
// A cdrom is exempt. It carries no replicated data on any domain, it is
// present on nearly all of them, and refusing every VM with one would make
// the check something operators disable rather than read.
func (s diskSelection) unreplicatedDisks() error {
	var unaccounted []disk.UnreplicableDisk
	var unnameable int
	for _, d := range s.unreplicable {
		if d.Device == "cdrom" {
			continue
		}
		if d.TargetDev == "" {
			unnameable++
			unaccounted = append(unaccounted, d)
			continue
		}
		if s.ignore.Has(d.TargetDev) || s.drop.Has(d.TargetDev) {
			continue
		}
		unaccounted = append(unaccounted, d)
	}
	if len(unaccounted) == 0 {
		return nil
	}

	var described []string
	for _, d := range unaccounted {
		name := d.TargetDev
		if name == "" {
			name = "a disk with no <target dev=>"
		}
		where := ""
		if d.Source != "" {
			where = " at " + d.Source
		}
		described = append(described, fmt.Sprintf("%s (%s, format %s%s)", name, d.Device, d.Format, where))
	}

	msg := fmt.Sprintf("refusing to sync %s: it has %s vmsync cannot replicate, and the replica would be missing data while reporting success. %s. "+
		"Pass -ignore-disk=<dev> to leave the disk out of the copy but keep it in the replica's definition exactly as declared -- for storage the target already has by other means -- or -drop-disk=<dev> to leave it out and remove it from that definition. Both take several devices, comma-separated",
		s.domain, plural(len(unaccounted), "a disk", "disks"), strings.Join(described, "; "))
	if unnameable > 0 {
		msg += ". One of them declares no <target dev=>, so no flag can name it: give it one in the domain XML, or remove the device"
	}
	return fmt.Errorf("%s", msg)
}

// nothingLeftToReplicate refuses a run that would copy no disk at all.
//
// It would otherwise succeed: every stage iterates the disk list, an empty
// list iterates nothing, and the run would record a checkpoint, define a
// replica and exit 0 having transferred no data. That is the same false green
// as the silent skip, reached from the other direction.
func (s diskSelection) nothingLeftToReplicate() error {
	replicable := 0
	for _, dev := range s.all {
		if s.ignore.Has(dev) || s.drop.Has(dev) {
			continue
		}
		if unreplicableByDev(s.unreplicable, dev) {
			continue
		}
		replicable++
	}
	if replicable > 0 {
		return nil
	}
	return fmt.Errorf("refusing to sync %s: every disk it has is either unreplicable or named in -ignore-disk/-drop-disk, so this run would copy nothing and still record a replica. Sync a domain with at least one qcow2 disk, or stop replicating this one -- the exclude metadata field takes a whole domain out of the schedule",
		s.domain)
}

// replicatedDisks is the parsed disk list with the excluded devices removed:
// what every stage of a run iterates.
//
// One function rather than a filter at each consumer, because the stages are
// not independent -- the checkpoint, the exports, the copy, the integrity
// check and the target definition must all act on the SAME set, and a stage
// that filtered differently would produce a replica whose checkpoint claims
// disks its definition does not have.
//
// Returns a new slice, never the input: the caller's full list is still what
// the refusal and the per-run warning describe.
func replicatedDisks(all []disk.QcowDisk, ignore, drop *diskSet) []disk.QcowDisk {
	out := make([]disk.QcowDisk, 0, len(all))
	for _, d := range all {
		if ignore.Has(d.TargetDev) || drop.Has(d.TargetDev) {
			continue
		}
		out = append(out, d)
	}
	return out
}

// unreplicableByDev answers whether a device is in the unreplicable set.
func unreplicableByDev(set []disk.UnreplicableDisk, dev string) bool {
	for _, d := range set {
		if d.TargetDev == dev {
			return true
		}
	}
	return false
}

// excludedDevices is the two sets together, sorted: what is recorded on the
// replica and what the per-run warning lists.
//
// Sorted rather than in flag order, because this one is compared between runs
// -- a set that reorders itself would read as a set that changed.
func (s diskSelection) excludedDevices() []string {
	out := append([]string{}, s.ignore.Names()...)
	out = append(out, s.drop.Names()...)
	sort.Strings(out)
	return out
}

func listOrNone(names []string) string {
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(names, ", ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return fmt.Sprintf("%d %s", n, many)
}
