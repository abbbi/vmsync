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
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"vmsync/pkg/disk"
	"vmsync/pkg/libvirtsync"
	"vmsync/pkg/remotessh"
	"vmsync/pkg/trace"
)

// An ORPHANED BITMAP is a qcow2 dirty bitmap named like one of vmsync's
// checkpoints, sitting in a source disk with no libvirt checkpoint behind it.
//
// It is invisible from every direction an operator would normally look:
// `virsh checkpoint-list` shows nothing, the domain is healthy, and vmsync's
// own -reinit reports that it discarded the chain. Only `qemu-img info` on the
// disk shows it, and only qemu mentions it -- as "Bitmap already exists:
// vmsync-cpt-000001", naming neither the file it is in nor anything to do
// about it.
//
// It arises when a checkpoint's bitmap outlives its metadata: qemu's
// `transaction` creates the bitmap and libvirt then fails to record the
// checkpoint, or the metadata is deleted without the bitmap. From then on
// every full sync collides, because vmsync restarts its chain at
// vmsync-cpt-000001 and that name is taken.

// leftoverCheckpointBitmaps reports vmsync-named bitmaps still present on the
// source's disks, keyed by disk path.
//
// Meant to be called AFTER the checkpoint chain has been dropped, which is what
// makes the answer unambiguous: everything libvirt knew about has just been
// removed, so whatever is still there is an orphan. Comparing against libvirt's
// checkpoint list would work too and would be more code for the same answer.
//
// Reads the images the same way the rest of the run does -- remotely when the
// source is reached over SSH, locally otherwise -- and both paths pass
// --force-share, which is what lets the read open a disk a running qemu holds.
//
// --force-share buys the lock, not the truth. What comes back is the qcow2
// bitmap directory ON DISK, which is not always what qemu holds: a bitmap
// created during the current qemu process's life need not be in the file yet.
// So an empty answer from a RUNNING source is not proof of absence, and the
// caller must not read it as one -- qemu can still refuse the next sync with
// "Bitmap already exists" for a name this never reported.
//
// Both halves measured on the same host. On 2026-10-02 a checkpoint libvirt had
// made at 16:46 was still not in the directory at 18:10 while the domain ran,
// and qemu refused to re-create that bitmap for already having it. On
// 2026-10-04, after that domain had been restarted, the same read reported both
// bitmaps, flagged in-use. So this read is not blind on a running domain -- it
// reports what was in the file when qemu opened it -- but it cannot be trusted
// to show a bitmap created since. A restart is what moves one into the file.
func leftoverCheckpointBitmaps(ctx context.Context, needsSSH bool, ssh *remotessh.Client, disks []disk.QcowDisk) (map[string][]string, error) {
	out := map[string][]string{}
	for _, d := range disks {
		path := d.Source
		if path == "" {
			continue
		}
		var (
			chain []disk.QemuImgInfo
			err   error
		)
		if needsSSH {
			chain, err = disk.QemuImgInfoChainJSONRemote(ctx, ssh, path)
		} else {
			chain, err = disk.QemuImgInfoChainJSON(path)
		}
		if err != nil {
			return nil, fmt.Errorf("inspect %s for leftover checkpoint bitmaps: %w", path, err)
		}
		if len(chain) == 0 {
			continue
		}
		// chain[0] only: a backing file's bitmaps belong to whatever owns that
		// file, not to this domain's checkpoint chain. Same rule, and the same
		// reason, as dropCheckpointsOffline.
		var mine []string
		for _, name := range disk.BitmapNames(chain[0]) {
			if libvirtsync.IsManagedCheckpointName(name) {
				mine = append(mine, name)
			}
		}
		if len(mine) > 0 {
			sort.Strings(mine)
			out[path] = mine
		}
	}
	return out, nil
}

// leftoverBitmapReport turns leftover bitmaps a rebuild could not clear into
// the message an operator acts on, or "" when there are none.
//
// A REPORT AND NOT A REFUSAL. It was a refusal, because a leftover bitmap used
// to stop the next sync outright: the new chain restarted at the same name and
// qemu answered "Bitmap already exists". Generation tokens remove that
// collision, so the rebuild now succeeds and the leftover is a space leak
// rather than an obstacle -- which is a reason to say so on every run, not a
// reason to refuse the rebuild that was asked for.
//
// Pure, so the message can be tested: it is the entire value of this feature.
// What it replaces is qemu's own error, which told an operator the name of
// something they could not find, on a host it did not identify, with no way to
// clear it. Everything here is chosen so the message alone is enough to act on
// -- the disk, the bitmap, why vmsync will not remove it, and the commands
// that will.
func leftoverBitmapReport(domain string, byDisk map[string][]string) string {
	if len(byDisk) == 0 {
		return ""
	}

	// Sorted, because a map's order would make the same fault read differently
	// on every run and two operators comparing notes would see two messages.
	paths := make([]string, 0, len(byDisk))
	for p := range byDisk {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var found, fix []string
	for _, p := range paths {
		for _, b := range byDisk[p] {
			found = append(found, fmt.Sprintf("%s in %s", b, p))
			// -f qcow2, matching disk.RemoveBitmap: the advice handed to an
			// operator should not skip the format pin the code applies to
			// itself, since probing a guest-writable image is how a raw disk
			// gets treated as something else.
			fix = append(fix, fmt.Sprintf("    qemu-img bitmap --remove -f qcow2 %s %s", p, b))
		}
	}

	return fmt.Sprintf(`reinit discarded %s's checkpoint chain, but its disks still carry %d dirty bitmap(s) that no checkpoint accounts for: %s

These are leftovers from a checkpoint whose bitmap outlived its metadata. The rebuild is NOT blocked by them -- the chain it starts carries a new generation token, so the names it mints cannot collide with these -- but nothing will ever use them either, and nothing removes them on its own: they are pure space in the qcow2 from here on. libvirt cannot see them (virsh checkpoint-list shows nothing) and reinit does not remove them while the domain is running, because qemu holds the image open and qemu-img cannot write it.

Clear them by hand, with the domain shut down:

    virsh shutdown %s
%s
    virsh start %s

then run this command again. Shutting the domain down and re-running vmsync with -reinit also works -- reinit removes bitmaps itself when the source is offline -- but costs a full sync's worth of downtime rather than a reboot's.

There is also a way to do it with NO downtime, which vmsync does not do for you yet: give libvirt back a checkpoint that names the orphaned bitmap, then delete that checkpoint normally so qemu removes the bitmap through its own path. Per bitmap, with TARGET-DEV the disk's target device (vda, sda...):

    cat > /tmp/adopt.xml <<'XML'
    <domaincheckpoint>
      <name>BITMAP-NAME</name>
      <creationTime>SECONDS-SINCE-EPOCH</creationTime>
      <disks>
        <disk name='TARGET-DEV' checkpoint='bitmap' bitmap='BITMAP-NAME'/>
      </disks>
    </domaincheckpoint>
    XML
    virsh checkpoint-create %s --xmlfile /tmp/adopt.xml --redefine
    virsh checkpoint-delete %s --checkpointname BITMAP-NAME

creationTime is required and its value is not checked against anything. Verified against libvirt on a running domain, and on a paused one`,
		domain, len(found), strings.Join(found, ", "),
		domain, strings.Join(fix, "\n"), domain,
		domain, domain)
}

// reportLeftoverBitmaps logs what a rebuild could not clear, once, and says
// nothing when there is nothing to say.
//
// A wrapper rather than three copies of the same two lines, because the three
// call sites in the reinit path reach this state for three different reasons
// -- the source's running state was unreadable, the source was shut down and
// the offline removal did not take, or the online clearing reported success
// and something survived it -- and the message is the same in all three. What
// differs is already logged where the branch is taken.
func reportLeftoverBitmaps(domain string, byDisk map[string][]string) {
	if report := leftoverBitmapReport(domain, byDisk); report != "" {
		trace.Warning(report, "vm", domain, "bitmaps", countBitmaps(byDisk))
	}
}

// orphanBitmaps reports which vmsync-named bitmaps in the images have no libvirt
// checkpoint behind them.
//
// An orphan is the one state that makes a later sync fail with no explanation in
// libvirt's view: `virsh checkpoint-list` shows nothing, so the next chain
// restarts at vmsync-cpt-000001, and qemu refuses because the qcow2 already
// holds a bitmap of that name. Every subsequent sync for the pair fails
// identically.
//
// Which bitmaps are orphaned decides whether a rebuild can proceed, so it is a
// pure set difference over values already in hand: no libvirt
// call, no qemu-img, and therefore testable without either.
// clearOrphanBitmapsOnline removes every vmsync-named bitmap the source still
// carries that libvirt has no checkpoint for, on a RUNNING domain, by adopting
// each one and deleting it.
//
// Called after the chain drop, which is what makes the set unambiguous:
// everything libvirt knew about has just been removed the proper way, so a
// vmsync-named bitmap still in the images is an orphan by definition and there
// is nothing left for it to collide with.
//
// Every orphan is attempted even when one fails, because they are independent
// and clearing three of four is better than clearing none -- the error names
// all the failures and the caller refuses on them. Returns how many it removed,
// so a run that cleared nothing does not read like a run that had nothing to
// clear.
//
// The removal itself arrives as a function so the decisions here -- what to
// attempt, what cannot be attempted at all, what to say when some of it fails
// -- are testable without a libvirt connection, which is the only way they get
// tested at all.
func clearOrphanBitmapsOnline(domainName string, orphans map[string][]string, disks []disk.QcowDisk, clear func(bitmap string, devs []string) error) (cleared int, err error) {
	plan, unmapped := adoptionPlan(orphans, disks)

	var failed []string
	for _, a := range plan {
		if cerr := clear(a.Bitmap, a.Devs); cerr != nil {
			failed = append(failed, fmt.Sprintf("%s on %s: %v", a.Bitmap, strings.Join(a.Devs, ","), cerr))
			continue
		}
		cleared++
		trace.Info("removed an orphaned checkpoint bitmap from a running source by adopting it and deleting the checkpoint",
			"vm", domainName, "bitmap", a.Bitmap, "disks", strings.Join(a.Devs, ","))
	}

	// Named, not swallowed: a bitmap in a file the domain does not list cannot
	// be adopted -- there is no target device to put in the xml -- and reporting
	// success over it would be a clean sweep of something still sitting there.
	for path, names := range unmapped {
		failed = append(failed, fmt.Sprintf("%s in %s, which %s does not list as one of its disks",
			strings.Join(names, ", "), path, domainName))
	}

	if len(failed) > 0 {
		return cleared, fmt.Errorf("could not clear %d orphaned bitmap(s) on %s: %s",
			len(failed), domainName, strings.Join(failed, "; "))
	}
	return cleared, nil
}

// countBitmaps totals the bitmaps across every disk, for a log line that says
// how many there are rather than how many disks have some.
func countBitmaps(byDisk map[string][]string) int {
	n := 0
	for _, names := range byDisk {
		n += len(names)
	}
	return n
}

// adoption is one orphaned bitmap and the disks that carry it, named the way
// libvirt wants them.
type adoption struct {
	Bitmap string
	// Devs are TARGET devices (vda, sda), not paths. libvirt's checkpoint xml
	// addresses disks by target device, and the bitmap read addresses them by
	// file, so something has to translate -- and it has to be the domain's own
	// definition doing it rather than a guess from the filename.
	Devs []string
}

// adoptionPlan turns orphaned bitmaps keyed by disk FILE into one entry per
// bitmap NAME, carrying the target devices that hold it.
//
// Per name rather than per disk, because one adoption covers all of a name's
// disks at once: a vmsync checkpoint puts the same bitmap name on every disk it
// covers, so adopting per disk would ask libvirt to redefine the same checkpoint
// several times and the second call would be refused for already existing.
//
// A bitmap on a file the domain does not list is left out, and the caller is
// expected to notice the count. Adopting it is impossible -- there is no target
// device to name -- and quietly dropping it would report a clean sweep over a
// bitmap still sitting there.
//
// Sorted, so a run's log and its error messages list the same thing in the same
// order twice running.
func adoptionPlan(byDisk map[string][]string, disks []disk.QcowDisk) (plan []adoption, unmapped map[string][]string) {
	dev := make(map[string]string, len(disks))
	for _, d := range disks {
		if d.Source != "" {
			dev[d.Source] = d.TargetDev
		}
	}

	devsFor := map[string][]string{}
	unmapped = map[string][]string{}
	for path, names := range byDisk {
		target, ok := dev[path]
		if !ok || target == "" {
			unmapped[path] = append(unmapped[path], names...)
			continue
		}
		for _, name := range names {
			devsFor[name] = append(devsFor[name], target)
		}
	}

	for name, devs := range devsFor {
		sort.Strings(devs)
		plan = append(plan, adoption{Bitmap: name, Devs: devs})
	}
	sort.Slice(plan, func(i, j int) bool { return plan[i].Bitmap < plan[j].Bitmap })
	for path := range unmapped {
		sort.Strings(unmapped[path])
	}
	return plan, unmapped
}

func orphanBitmaps(byDisk map[string][]string, known []string) map[string][]string {
	k := make(map[string]bool, len(known))
	for _, name := range known {
		k[name] = true
	}
	out := map[string][]string{}
	for path, names := range byDisk {
		for _, name := range names {
			if !k[name] {
				out[path] = append(out[path], name)
			}
		}
	}
	return out
}

// checkpointNames reduces libvirt's checkpoint records to the names the bitmaps
// in an image can be compared against.
//
// The names are the only part of a checkpoint that appears in a qcow2: a bitmap
// carries its checkpoint's name and nothing else, so a comparison against
// anything richer would be comparing fields one side does not have.
func checkpointNames(cps []libvirtsync.Checkpoint) []string {
	out := make([]string, 0, len(cps))
	for _, cp := range cps {
		out = append(out, cp.Name)
	}
	return out
}

// preTokenBitmapRe is the name shape vmsync minted before generation tokens:
// the prefix and a bare counter, nothing identifying the chain it belonged to.
//
// Recognised only so the report can SAY so. No chain is ever extended from one
// -- NextCheckpointName refuses a tip it cannot read a generation from -- but a
// bitmap sitting in a real image has to be nameable, and "we cannot tell you
// anything about this" is a worse answer than "this predates generation
// tokens, so which chain it belonged to is unknowable".
var preTokenBitmapRe = regexp.MustCompile(
	`^` + regexp.QuoteMeta(libvirtsync.CheckpointPrefix) + `-[0-9]+$`)

// deadBitmapGroup is one generation's worth of dead bitmaps, as the report
// renders it.
type deadBitmapGroup struct {
	// token is the generation, "" for the two buckets that have none.
	token string
	// minted is when that generation began, zero when it cannot be read.
	minted time.Time
	// where is "<bitmap> in <disk path>", sorted.
	where []string
}

// attributeDeadBitmaps describes the dead bitmaps a source still carries,
// grouped by the generation each belonged to.
//
// THE SET IS NOT RE-DECIDED HERE. It comes from orphanBitmaps, which subtracts
// the names libvirt holds checkpoints for from the names the images carry, and
// a bitmap is dead because no checkpoint accounts for it -- true whatever its
// name says. This only chooses words for the dead: which generation, and how
// long ago that generation began.
//
// Deciding by token instead would be wrong in both directions. A bitmap with a
// foreign token but a live checkpoint would be reported as reclaimable when it
// is the baseline an incremental needs; and -reinit's post-drop clearing would
// switch off exactly where it is needed, because after a chain drop there is
// no live generation for a token to be compared against.
//
// Pure, and empty when there is nothing to say, so the message is testable the
// way leftoverBitmapRefusal's is.
func attributeDeadBitmaps(orphans map[string][]string, now time.Time) string {
	if len(orphans) == 0 {
		return ""
	}

	// Sorted paths, for the reason leftoverBitmapRefusal sorts them: a map's
	// order would make the same state read differently on every run.
	paths := make([]string, 0, len(orphans))
	for p := range orphans {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	// Three buckets, keyed so the two tokenless ones cannot collide with a
	// token: a token always contains a '-', these keys do not.
	const (
		preToken       = "pretoken"
		unattributable = "unreadable"
	)
	groups := map[string]*deadBitmapGroup{}
	order := []string{}
	add := func(key, token string, minted time.Time, where string) {
		g, seen := groups[key]
		if !seen {
			g = &deadBitmapGroup{token: token, minted: minted}
			groups[key] = g
			order = append(order, key)
		}
		g.where = append(g.where, where)
	}

	for _, path := range paths {
		names := append([]string(nil), orphans[path]...)
		sort.Strings(names)
		for _, name := range names {
			where := fmt.Sprintf("%s in %s", name, path)
			switch token, ok := libvirtsync.GenerationToken(name); {
			case ok:
				// The grammar admits ten digits, not a calendar, so a date
				// that is not a date reaches here. Reported as the generation
				// it is, without an age, rather than refused: this function
				// describes what is there.
				// time.Parse already returns the zero Time on failure, so the
				// branch states the intent rather than effecting it: a date
				// that is not a date leaves minted zero, and the renderer
				// below says so instead of computing an age against year one.
				// Written out because a reader otherwise has to know that
				// about time.Parse to see that the zero case is handled.
				minted, err := time.Parse("0601021504", strings.SplitN(token, "-", 2)[0])
				if err != nil {
					minted = time.Time{}
				}
				add(token, token, minted.UTC(), where)
			case preTokenBitmapRe.MatchString(name):
				add(preToken, "", time.Time{}, where)
			default:
				add(unattributable, "", time.Time{}, where)
			}
		}
	}
	sort.Strings(order)

	var lines []string
	for _, key := range order {
		g := groups[key]
		switch key {
		case preToken:
			lines = append(lines, fmt.Sprintf("  %d bitmap(s) from before generation tokens existed, so which chain they belonged to cannot be recovered: %s",
				len(g.where), strings.Join(g.where, ", ")))
		case unattributable:
			lines = append(lines, fmt.Sprintf("  %d bitmap(s) whose names vmsync did not mint and cannot attribute: %s",
				len(g.where), strings.Join(g.where, ", ")))
		default:
			age := "minted at an unreadable date"
			if !g.minted.IsZero() {
				age = fmt.Sprintf("generation minted %s, %s ago",
					g.minted.Format(time.RFC3339), now.Sub(g.minted).Round(time.Minute))
			}
			lines = append(lines, fmt.Sprintf("  %d bitmap(s) from generation %s (%s): %s",
				len(g.where), g.token, age, strings.Join(g.where, ", ")))
		}
	}

	return fmt.Sprintf(`this source carries %d dirty bitmap(s) that no libvirt checkpoint accounts for, so no incremental can ever use them:
%s
They cost space in the qcow2 and nothing removes them on its own. Clearing one needs the domain shut down (qemu-img bitmap --remove) or an adoption through libvirt, and vmsync does neither unasked -- see the -reinit path, which clears them as part of a rebuild it was told to do.

This read is of the qcow2 bitmap directory ON DISK, which is not always what qemu holds: a bitmap created during the current qemu process's life need not be in the file yet. On a running source this is a floor, not a census. It also reads only each disk's own active image, never down a backing chain, so bitmaps belonging to a shared base are out of scope here and not counted`,
		countBitmaps(orphans), strings.Join(lines, "\n"))
}
