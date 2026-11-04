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
	"fmt"
	"sort"
	"strings"
	"time"

	"libvirt.org/go/libvirt"

	"vmsync/pkg/disk"
	"vmsync/pkg/libvirtsync"
	"vmsync/pkg/unsupportedqmp"
)

// THE WHOLE OF THE UNSUPPORTED ROUTE'S INTEGRATION, and the only file in
// cmd/vmsync that names pkg/unsupportedqmp.
//
// Deleting the route when libvirt grows a supported bitmap enumeration is:
// this file, pkg/unsupportedqmp, the bitmapCensusFor closure in main.go, and
// the qemuAnswered parameter of unprovenAbsenceReport. Nothing else learns
// that a second reader exists -- adoptionPlan, clearOrphanBitmapsOnline and
// orphanBitmaps all keep taking a map keyed by disk path, which is what the
// census produces.
//
// WHY THERE IS NO ERROR ANYWHERE BELOW. The owner's requirement is that a QMP
// failure warns and never fails a run. Carried by the types rather than by
// discipline: bitmapCensus returns a result struct with no error field, so
// there is no value a future caller can accidentally propagate into a failed
// run, and no `if err != nil` for somebody to add one to. The only error that
// reaches run() from this path is leftoverCheckpointBitmaps', which already
// has handled arms in the post-drop switch.

// censusLevel is how loudly a note is said. The census's own observations are
// Info; anything an operator may need to act on is Warning.
//
// Not collapsed into "always warn": trace.WarningCount() is read into the
// metrics textfile and the bench greps warning prose, so a routine
// observation logged as a warning shows up as a fault in both.
type censusLevel int

const (
	censusInfo censusLevel = iota
	censusWarn
)

type censusNote struct {
	Level censusLevel
	Text  string
}

// censusInput is what the combiner needs to decide everything. A struct rather
// than seven positional parameters, because the call is made once and read
// often.
type censusInput struct {
	Domain string
	Disks  []disk.QcowDisk

	// FromFile and FileErr are leftoverCheckpointBitmaps' answer. FileErr is
	// used for WORDING ONLY: a difference against a set that was never read is
	// not a difference, so the two disagreement notes stay silent when the
	// image read failed. No control flow depends on it.
	FromFile map[string][]string
	FileErr  error

	// Active and ActiveErr come from DomainActive. A shut-down source is not
	// asked: its image is closed, so the file read already saw everything, and
	// a monitor command against it would fail anyway.
	Active    bool
	ActiveErr error

	// Query is injected so every branch below is reachable from a unit test
	// without a libvirt connection, which is the only way any of this gets
	// tested at all.
	Query func() (string, error)
}

// censusResult carries no error, deliberately. See the file comment.
type censusResult struct {
	// Union is what the caller acts on: the image read widened by whatever
	// qemu reported that the file could not yet show. Always a superset of
	// FromFile, so deleting this route can never make vmsync act on less.
	Union map[string][]string

	// FromQemu is qemu's answer alone, for the one caller that must not look
	// at the file: after a clearing, a bitmap qemu has let go of stays in the
	// qcow2 until the image closes, so the file would report a cleared orphan
	// as a survivor.
	FromQemu map[string][]string

	// OnlyQemu is what this route made newly visible -- in qemu's answer and
	// not in the image read.
	//
	// Computed on every success, not only when the disagreement notes fire,
	// because it decides a CLEARING FAILURE'S SEVERITY and not just a message.
	// A bitmap the image read also found is one vmsync could already see and
	// already refused a rebuild over, so that refusal stands; one only qemu
	// reported was invisible until this route existed, and a run that used to
	// succeed must not start failing because vmsync can now see more.
	//
	// Empty whenever the census did not run, which makes every failure fatal
	// again -- exactly the behaviour before this route existed.
	OnlyQemu map[string][]string

	// Proven is unsupportedqmp.Census.Proven, or false whenever qemu was not
	// asked or did not answer completely. The caller's only use for it is to
	// stop saying that an empty answer proves nothing -- so "qemu was not
	// asked" and "qemu's answer was partial" must both read as not proven.
	Proven bool

	Notes []censusNote
}

// bitmapCensus widens the image read with qemu's own list of dirty bitmaps.
//
// Pure, because the query is injected: every failure mode has a unit test with
// a stub, which matters more here than anywhere else in this feature, since
// none of these branches can be reached from a test through a real libvirt
// connection.
//
// On every failure the Union is the image read unchanged and Proven is false.
// There is no path on which this function makes vmsync act on fewer bitmaps
// than the file read found.
func bitmapCensus(in censusInput) censusResult {
	res := censusResult{Union: in.FromFile, Proven: false}

	switch {
	case in.ActiveErr != nil:
		// Not asked, and said so. Asking would mean deciding what an
		// unreadable state means, and the answer is that vmsync does not know.
		res.Notes = append(res.Notes, censusNote{censusWarn, fmt.Sprintf(
			"did not ask qemu which dirty bitmaps %s holds, because vmsync could not tell whether the source is running (%v); this run has only the qcow2 directory to go on, which cannot show a bitmap created during the running qemu's life",
			in.Domain, in.ActiveErr)})
		return res
	case !in.Active:
		// Silent on purpose. A closed image has written its bitmap directory
		// out, so the file read is already a complete answer and there is
		// nothing for a census to add -- and nothing worth tainting a domain
		// for. Proven stays false because it means "qemu answered", and qemu
		// was not asked; the caller's own shut-down arm already handles this
		// state without needing a census.
		return res
	}

	reply, err := in.Query()
	if err != nil {
		// Quoted verbatim: a read-only connection, a polkit denial and a
		// libvirt built without the qemu driver all land here and read quite
		// differently, and paraphrasing them would send an operator to the
		// wrong component.
		res.Notes = append(res.Notes, censusNote{censusWarn, fmt.Sprintf(
			"could not ask qemu which dirty bitmaps %s holds, so this run has only the qcow2 directory to go on: %v",
			in.Domain, err)})
		return res
	}

	found, nodes, perr := unsupportedqmp.Parse(reply)
	if perr != nil {
		res.Notes = append(res.Notes, censusNote{censusWarn, fmt.Sprintf(
			"qemu answered the dirty-bitmap query for %s with something vmsync could not read, so this run has only the qcow2 directory to go on: %v",
			in.Domain, perr)})
		return res
	}

	c := unsupportedqmp.Map(found, nodes, in.Disks, libvirtsync.IsManagedCheckpointName)

	for _, p := range c.Problems {
		res.Notes = append(res.Notes, censusNote{censusWarn, fmt.Sprintf(
			"qemu's dirty-bitmap answer for %s cannot be trusted: %s", in.Domain, p)})
	}
	if r := unclaimedReport(in.Domain, c.Unclaimed); r != "" {
		res.Notes = append(res.Notes, censusNote{censusWarn, r})
	}
	if r := busyReport(in.Domain, c.Busy); r != "" {
		res.Notes = append(res.Notes, censusNote{censusWarn, r})
	}
	if r := uncoveredReport(in.Domain, c.Uncovered); r != "" {
		res.Notes = append(res.Notes, censusNote{censusWarn, r})
	}

	res.FromQemu = c.ByDisk
	res.Union = unsupportedqmp.Union(in.FromFile, c.ByDisk)
	res.Proven = c.Proven()

	// UNGATED, because this one is a decision and not a message. Which
	// bitmaps this route made newly visible is what keeps a clearing failure
	// over one of them from refusing a rebuild that used to succeed, and that
	// has to hold even on a run whose image read failed or whose census came
	// back partial.
	res.OnlyQemu = unsupportedqmp.OnlyIn(c.ByDisk, in.FromFile)

	// The two disagreement notes, and only when BOTH readers answered. A
	// difference against a set that was never read is not a difference: with
	// the image read failed, every bitmap would read as qemu-only, and with a
	// partial census every bitmap would read as file-only.
	if in.FileErr == nil && res.Proven {
		if r := qmpOnlyNote(in.Domain, res.OnlyQemu); r != "" {
			res.Notes = append(res.Notes, censusNote{censusInfo, r})
		}
		if r := fileOnlyReport(in.Domain, unsupportedqmp.OnlyIn(in.FromFile, c.ByDisk)); r != "" {
			res.Notes = append(res.Notes, censusNote{censusWarn, r})
		}
	}

	return res
}

// qmpOnlyNote is the EXPECTED steady state on a running source, which is why
// it is Info and why it carries no remedy.
//
// Deliberately contains no `virsh shutdown` and no `qemu-img bitmap --remove`:
// there is nothing for an operator to do. vmsync found these and is about to
// clear them through libvirt, and offering a downtime recipe for a state the
// run is already fixing is how a report stops being read.
func qmpOnlyNote(domain string, onlyQemu map[string][]string) string {
	names, total := flattenBitmaps(onlyQemu)
	if total == 0 {
		return ""
	}
	return fmt.Sprintf("qemu holds %d checkpoint bitmap(s) on %s that its qcow2 files do not carry yet: %s. That is the ordinary state for a bitmap made during the running qemu's life -- qemu does not write one into the image until it closes it -- and it is why vmsync asks qemu as well as the image. These are in the set this run acts on.",
		total, domain, strings.Join(names, ", "))
}

// fileOnlyReport is the other direction, and it is a WARNING because
// adopt-and-delete cannot fix it.
//
// A bitmap in the image that qemu does not hold is one qemu never loaded --
// what a save/restore leaves behind once the checkpoint metadata is gone.
// libvirt's delete removes a bitmap qemu HOLDS, so the online route has no
// purchase on this one and the remedy really is the offline one.
func fileOnlyReport(domain string, onlyFile map[string][]string) string {
	if len(onlyFile) == 0 {
		return ""
	}
	paths := sortedKeys(onlyFile)
	var found, fix []string
	for _, p := range paths {
		for _, b := range onlyFile[p] {
			found = append(found, fmt.Sprintf("%s in %s", b, p))
			fix = append(fix, fmt.Sprintf("    qemu-img bitmap --remove -f qcow2 %s %s", p, b))
		}
	}
	return fmt.Sprintf(`%s's qcow2 files carry %d checkpoint bitmap(s) that qemu does not hold: %s

qemu never loaded them, which is what a save and restore leaves behind once the checkpoint metadata has gone. vmsync's online clearing cannot remove these: it works by giving libvirt back a checkpoint naming an existing bitmap and deleting it, and libvirt's delete removes a bitmap qemu HOLDS. The rebuild is not blocked by them -- the chain it starts cannot collide with these names -- but nothing will ever use them and nothing removes them on its own.

Clear them with the domain shut down:

    virsh shutdown %s
%s
    virsh start %s`,
		domain, len(found), strings.Join(found, ", "), domain, strings.Join(fix, "\n"), domain)
}

// unclaimedReport names every bitmap qemu holds that the census would not put
// in its result, and says why.
//
// NEVER SILENT, which is the whole point. A bitmap qemu holds that vmsync has
// decided not to touch is exactly what an operator needs told: the alternative
// is a run that reports a clean sweep over something still sitting there,
// which is the defect class this codebase keeps finding.
func unclaimedReport(domain string, unclaimed []unsupportedqmp.UnclaimedNode) string {
	if len(unclaimed) == 0 {
		return ""
	}
	var lines []string
	for _, u := range unclaimed {
		lines = append(lines, fmt.Sprintf("  %s on node %s (%s): %s",
			strings.Join(u.Bitmaps, ", "), u.NodeName, u.File, unclaimedWhy(u.Reason, domain)))
	}
	return fmt.Sprintf("qemu holds %d checkpoint bitmap set(s) on %s that this run will not touch:\n%s",
		len(unclaimed), domain, strings.Join(lines, "\n"))
}

func unclaimedWhy(reason, domain string) string {
	switch reason {
	case unsupportedqmp.ReasonBacking:
		return "this file is a backing file of one of the disks, not the disk itself. A backing file can be shared with another domain, so its bitmaps belong to whatever owns that file and removing one could take away another pair's baseline."
	case unsupportedqmp.ReasonNoTarget:
		return "the domain XML gives this disk no <target dev>, so no adoption XML can name it and vmsync's online clearing has no way to address it. Add a target device to the disk, or clear the bitmap with qemu-img while the domain is shut down."
	case unsupportedqmp.ReasonNoFile:
		return "qemu reported no file for this node, so there is nothing to match it against " + domain + "'s disks."
	default:
		return "this file is not one of " + domain + "'s disks. Another domain, or another tool, may own it; vmsync will not remove a bitmap from an image it does not manage."
	}
}

// busyReport exists because QMP offers a check the image read never could.
//
// A busy bitmap is one an in-flight backup is using. Adopting and deleting it
// would pull the bitmap out from under a running job, so it is set aside --
// and said, because the next run will meet it again and an operator comparing
// two runs needs to know why it was skipped.
func busyReport(domain string, busy map[string][]string) string {
	names, total := flattenBitmaps(busy)
	if total == 0 {
		return ""
	}
	return fmt.Sprintf("qemu reports %d checkpoint bitmap(s) on %s as busy, so this run left them alone: %s. A busy bitmap is one a backup job is using, and removing it from under that job would leave the job describing something that no longer exists. Re-run once no backup is in flight.",
		total, domain, strings.Join(names, ", "))
}

// uncoveredReport is the symlink case: a disk vmsync manages that no block node
// qemu described matched.
//
// Said rather than assumed empty, because the census is then incomplete for
// that disk and the reason is almost always a path vmsync and qemu spell
// differently.
func uncoveredReport(domain string, uncovered []string) string {
	if len(uncovered) == 0 {
		return ""
	}
	return fmt.Sprintf("qemu described no block node for %d of %s's disks, so this run cannot say whether they carry leftover bitmaps:\n  %s\n\nThe usual cause is the domain XML naming a path qemu opened under a different spelling -- a symlink, or a relative <source file>. The qcow2 directory was still read for these disks, so an older leftover would still have been found; a bitmap made during the running qemu's life would not.",
		len(uncovered), domain, strings.Join(uncovered, "\n  "))
}

// censusQuery is the bounded call, and the only place a libvirt handle meets
// this feature.
//
// virDomainQemuMonitorCommand takes a libvirt MODIFY job and has no timeout of
// its own, so an unresponsive qemu would park the run here holding the source's
// run lock. Wrapped in the caller's own bounded-call helper, which on a timeout
// marks the handle as possibly still in use and lets the run proceed -- the
// trade ErrCallTimedOut's own doc comment already blesses.
func censusQuery(dom *libvirt.Domain, domainName string, bounded func(string, time.Duration, func() error) error, timeout time.Duration) (string, error) {
	var reply string
	err := bounded(fmt.Sprintf("ask qemu which dirty bitmaps %s holds", domainName), timeout, func() error {
		var qerr error
		reply, qerr = unsupportedqmp.Query(dom, domainName)
		return qerr
	})
	if err != nil {
		return "", err
	}
	return reply, nil
}

func flattenBitmaps(byDisk map[string][]string) (names []string, total int) {
	for _, p := range sortedKeys(byDisk) {
		for _, b := range byDisk[p] {
			names = append(names, fmt.Sprintf("%s in %s", b, p))
			total++
		}
	}
	return names, total
}

func sortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
