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
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
	"vmsync/pkg/disk"
	"vmsync/pkg/trace"
	"vmsync/pkg/util"
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
	// sourceByDev is every disk's own <source file=> on the SOURCE, replicable
	// or not. Kept because an excluded disk is removed from the list every
	// later stage iterates, and this is then the only remaining record of what
	// its replica file was called -- see candidateReplicaPaths.
	sourceByDev map[string]string
	ignore      *diskSet
	drop        *diskSet
}

// candidateReplicaPaths is where the replica files of the excluded disks would
// be, under this run's -target-disk-path.
//
// FOR ATTRIBUTION, not for acting on. The displaced-set sweep recognises a set
// only as `<base>.vmsync-replaced-<unix>` where base is a file this replica
// owns, which is what keeps a shared directory's answer per-domain. Once the
// aside has been made, the base is no longer anywhere in the replica's
// definition -- a dropped disk is gone from it, an ignored one names the
// SOURCE's path -- so without these the set would be listed on every later run
// and attributed to nobody, and `-reclaim-leftovers-after` could never reap it.
// Reporting it once and then losing it would have turned one invisible orphan
// into another with a different name.
//
// Derived from the source's own declared path, which is how every replica file
// is named, and available on every run without touching the disk. A path that
// does not exist costs nothing here: it is only ever used as a base name to
// match against.
//
// The one case it cannot name is a disk whose source sits on a backing chain,
// where the replica was named after the chain's ROOT rather than the overlay.
// The run that makes the aside gets that name exactly, from the replica's own
// definition; later runs would compute the overlay's. That leaves such a set
// unattributed, which is where it was before any of this -- not worse.
func (s diskSelection) candidateReplicaPaths(targetDiskPath string) []string {
	var out []string
	for _, dev := range s.excludedDevices() {
		if src := s.sourceByDev[dev]; src != "" {
			out = append(out, util.SetTargetPath(targetDiskPath, src))
		}
	}
	return out
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

// asideExcludedReplicas renames aside the replica files of disks this run has
// stopped copying, and returns the ordinary paths it displaced.
//
// WHAT IT IS FOR. A pair that replicated vdb for months and now excludes it
// leaves a full-size file on the DR host that nothing ever touches again: the
// -reinit rename loop, the full-sync preflight and the displaced-set sweep all
// iterate the disks this run copies, so none of them asks about it -- and the
// leftover reporting could not name it anyway, because it recognises a
// displaced set only by the `<base>.vmsync-replaced-<unix>` shape and this file
// still has the ordinary disk name. It is unreferenced under either flag: a
// dropped disk is gone from the definition, and an ignored one points at the
// SOURCE host's path.
//
// Renaming it aside turns it into an ordinary displaced set, which the sweep
// already reports on every run and -reclaim-leftovers-after already removes
// once an operator has said how old is old enough.
//
// Never deleted, whatever -replaced-disk-action says. That flag's own wording
// is about the disk a -reinit replaces, and this is the last replica of a disk
// somebody has just said they manage another way -- so it goes where an
// operator can still find it, and leaves by the route they chose.
//
// THE PATH COMES FROM THE REPLICA'S OWN PREVIOUS DEFINITION, which is the only
// place it is recorded exactly. Deriving it from the source's disk today would
// be wrong wherever an external snapshot or a linked clone is in play, because
// the replica was named after the backing chain's ROOT; and resolving that
// chain here would mean running qemu-img against a disk -ignore-disk has just
// promised to touch in no way.
//
// Only a path inside the replica's own disk directory is acted on, and that
// guard is load-bearing rather than tidy: after the first excluded run an
// IGNORED disk's entry in the definition names the SOURCE host's path, so
// without it a second run could rename a file on the target that merely
// happens to sit at the same path as something on the source.
func asideExcludedReplicas(ctx context.Context, runner remoteRunner, targetXML string, excluded []string, replicaDir, stamp string) ([]string, error) {
	if len(excluded) == 0 || targetXML == "" {
		return nil, nil
	}
	byDev, err := disk.SourceByTargetDev(targetXML)
	if err != nil {
		// Not fatal: an unreadable target definition is already reported by
		// everything that depends on it, and failing the sync over an orphan
		// nobody is about to read would be the wrong trade.
		trace.Warning("could not read the replica's definition to look for the disks this run stopped copying; a previously replicated disk may be left as an unreported file on the target",
			"error", err)
		return nil, nil
	}

	var displaced []string
	for _, dev := range excluded {
		p := byDev[dev]
		if p == "" {
			continue
		}
		if path.Dir(p) != replicaDir {
			// The definition names a path outside the replica's own disk
			// directory, which is what an ignored disk's entry looks like
			// once this flag has been in force for a run. Nothing of this
			// replica's is there to displace.
			continue
		}
		exists, err := util.RemotePathExists(ctx, runner, p)
		if err != nil {
			return nil, fmt.Errorf("check whether the replica still has a file for the excluded disk %s at %s: %w", dev, p, err)
		}
		if !exists {
			continue
		}
		aside := p + replacedDiskSuffix + stamp
		trace.Warning("this disk is no longer replicated, so its replica file is being renamed aside rather than left as an unreported file on the target",
			"device", dev, "path", p, "renamed_to", aside,
			"note", "it is now an ordinary displaced set: every run reports it, and -reclaim-leftovers-after removes it once it is older than the duration you set")
		// -n for the reason the reinit aside uses it: an existing file at the
		// destination is never clobbered, so a set somebody kept deliberately
		// survives and the run fails instead.
		if out, err := runner.Run(ctx, "mv -n "+util.ShQuote(p)+" "+util.ShQuote(aside)); err != nil {
			return nil, fmt.Errorf("rename aside the replica file of the excluded disk %s (%s -> %s): %w: %s", dev, p, aside, err, out)
		}
		displaced = append(displaced, p)
	}
	return displaced, nil
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
