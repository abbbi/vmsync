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
// source is reached over SSH, locally otherwise -- and both paths already pass
// --force-share, which is what lets this read a disk a running qemu has open.
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
			fix = append(fix, fmt.Sprintf("    qemu-img bitmap --remove %s %s", p, b))
		}
	}

	return fmt.Errorf(`reinit discarded %s's checkpoint chain, but its disks still carry %d dirty bitmap(s) that no checkpoint accounts for: %s

These are leftovers from a checkpoint whose bitmap outlived its metadata, and they are why the next step would fail with "Bitmap already exists". libvirt cannot see them (virsh checkpoint-list shows nothing) and reinit cannot remove them while the domain is running: qemu holds the image open, so the only live route is a raw QMP block-dirty-bitmap-remove behind libvirt's back, which vmsync will not do to a running guest.

Clear them by hand, with the domain shut down:

    virsh shutdown %s
%s
    virsh start %s

then run this command again. Shutting the domain down and re-running vmsync with -reinit also works -- reinit removes bitmaps itself when the source is offline -- but costs a full sync's worth of downtime rather than a reboot's`,
		domain, len(found), strings.Join(found, ", "),
		domain, strings.Join(fix, "\n"), domain)
}
