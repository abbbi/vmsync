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

package inventory

import (
	"os"
	"path/filepath"
	"sort"
	"syscall"
)

// DiskInfo is one disk file as it sits on this host's storage.
//
// Two sizes, because they answer different questions and confusing them
// gives the wrong answer to both. ApparentBytes is the file's nominal
// length; AllocatedBytes is what it actually occupies. A sparse or
// thin-provisioned qcow2 routinely reports 500 GB apparent against 40 GB
// allocated, and it is the allocated figure that a copy consumes.
type DiskInfo struct {
	Path           string `json:"path"`
	ApparentBytes  int64  `json:"apparent_bytes"`
	AllocatedBytes int64  `json:"allocated_bytes"`
	// Missing is true when the file the domain definition names is not
	// there. Reported rather than silently skipped: a target whose disks
	// have vanished looks identical to a healthy one from metadata alone.
	Missing bool `json:"missing,omitempty"`
}

// Filesystem is the storage a set of disks lives on.
//
// Keyed by the DIRECTORY the disks are in rather than by the host, because
// the question being answered is per-location: a hypervisor commonly has
// VMs spread across several pools, and "the host has 2 TB free" is useless
// if the VM being inverted lives on the full one.
type Filesystem struct {
	Path       string `json:"path"`
	TotalBytes int64  `json:"total_bytes"`
	FreeBytes  int64  `json:"free_bytes"`
	// UsedBytes is Total minus what is free to a non-privileged writer, so
	// it includes the reserved blocks. It is what a df-reading operator
	// expects to see.
	UsedBytes int64 `json:"used_bytes"`
}

// AllocatedBytes totals what a domain's disks actually occupy.
//
// This is the figure that answers "can I afford to keep the old copy?" --
// a rename-aside on inversion consumes exactly this much again, on the same
// filesystem, for as long as the aside files are kept.
func (d Domain) AllocatedBytes() int64 {
	var total int64
	for _, disk := range d.Disks {
		total += disk.AllocatedBytes
	}
	return total
}

// inspectDisks stats every disk path a domain refers to.
//
// Deliberately stat-only: no qemu-img, no reading image headers. This runs
// for every domain on every report, and shelling out per disk to learn a
// virtual size nobody acts on would turn a cheap inventory into a slow one.
// What a copy costs is what is allocated, and stat knows that.
func inspectDisks(paths []string) []DiskInfo {
	out := make([]DiskInfo, 0, len(paths))
	for _, p := range paths {
		info := DiskInfo{Path: p}
		st, err := os.Stat(p)
		if err != nil {
			info.Missing = true
			out = append(out, info)
			continue
		}
		info.ApparentBytes = st.Size()
		// Blocks are always 512-byte units in stat(2), regardless of the
		// filesystem's own block size -- see statx(2). Falls back to the
		// apparent size on a platform that does not expose it rather than
		// reporting zero, which would read as "costs nothing to copy".
		if sys, ok := st.Sys().(*syscall.Stat_t); ok {
			info.AllocatedBytes = sys.Blocks * 512
		} else {
			info.AllocatedBytes = st.Size()
		}
		out = append(out, info)
	}
	return out
}

// FilesystemsFor reports the storage behind a set of domains, one entry per
// distinct directory.
//
// Deduplicated by directory: several VMs normally share a pool, and
// statfs-ing once per disk would produce a dozen identical rows and a dozen
// syscalls for one answer.
func FilesystemsFor(domains []Domain) []Filesystem {
	seen := map[string]bool{}
	var out []Filesystem

	for _, d := range domains {
		for _, disk := range d.Disks {
			dir := filepath.Dir(disk.Path)
			if dir == "" || seen[dir] {
				continue
			}
			seen[dir] = true
			fs, ok := statFilesystem(dir)
			if !ok {
				continue
			}
			out = append(out, fs)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func statFilesystem(dir string) (Filesystem, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return Filesystem{}, false
	}
	bsize := int64(st.Bsize)
	total := int64(st.Blocks) * bsize
	// Bavail, not Bfree: the reserved blocks are not available to the
	// process that would be writing the copy, so counting them would
	// promise space that a rename-aside cannot actually use.
	free := int64(st.Bavail) * bsize
	return Filesystem{
		Path:       dir,
		TotalBytes: total,
		FreeBytes:  free,
		UsedBytes:  total - free,
	}, true
}
