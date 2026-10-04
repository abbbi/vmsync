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
	"sort"
	"strings"

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

// leftoverBitmapRefusal turns leftover bitmaps into the error that stops the
// run, or nil when there are none.
//
// Pure, so the message can be tested: it is the entire value of this feature.
// The failure it replaces is qemu's "Bitmap already exists: vmsync-cpt-000001",
// which tells an operator the name of something they cannot find, on a host it
// does not identify, with no way to clear it. Everything here is chosen so that
// the message alone is enough to fix the problem -- the disk, the bitmap, why
// vmsync will not remove it, and the three commands that will.
func leftoverBitmapRefusal(domain string, byDisk map[string][]string) error {
	if len(byDisk) == 0 {
		return nil
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

	return fmt.Errorf(`reinit discarded %s's checkpoint chain, but its disks still carry %d dirty bitmap(s) that no checkpoint accounts for: %s

These are leftovers from a checkpoint whose bitmap outlived its metadata, and they are why the next step would fail with "Bitmap already exists". libvirt cannot see them (virsh checkpoint-list shows nothing) and reinit does not remove them while the domain is running: qemu holds the image open, so qemu-img cannot write it.

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
