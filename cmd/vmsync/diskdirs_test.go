/*
	Copyright (C) 2026  Orsiris de Jong <ozy@netpower.fr>
	Copyright (C) 2026  Michael Ablassmeier <abi@grinser.de>

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
	"path"
	"strings"
	"testing"

	"vmsync/pkg/disk"
	"vmsync/pkg/util"
)

// diskAt builds a QcowDisk whose RootSource is the given POSIX path. Only
// RootSource and TargetDev matter here -- the directory under test is derived
// from RootSource alone.
func diskAt(dev, rootSource string) disk.QcowDisk {
	return disk.QcowDisk{TargetDev: dev, RootSource: rootSource}
}

// Every case here leaves -target-disk-path EMPTY and puts each disk's
// RootSource at the path the replica's disk should land on, which is what
// util.SetTargetPath returns unchanged in that mode.
//
// That is deliberate, and it is a scoping decision rather than a convenience.
// With a non-empty -target-disk-path, SetTargetPath goes through
// filepath.Join, so the directory this function derives is spelled with the
// host platform's separator -- and path.Dir, which is POSIX-only, then reports
// "." for a Windows-joined path. Exercising that combination would be testing
// filepath's behaviour (already covered, and already a known cross-platform
// difference in TestSetTargetPath) while silently collapsing every case below
// to the same directory, so the de-duplication assertions would pass without
// de-duplicating anything.
//
// wantDirs recomputes the expected directories with the same two calls the
// function under test uses, so the assertions that carry weight are the COUNT,
// the de-duplication, and whether a chown is issued -- not the spelling of a
// path both sides would be deriving identically.
func wantDirs(targetDiskPath string, disks []disk.QcowDisk) []string {
	var out []string
	seen := map[string]bool{}
	for _, d := range disks {
		dir := path.Dir(util.SetTargetPath(targetDiskPath, d.RootSource))
		if seen[dir] {
			continue
		}
		seen[dir] = true
		out = append(out, dir)
	}
	return out
}

// TestEnsureTargetDiskDirsOwnsWhatItCreates is the regression guard for the
// defect this function exists to close: the replica's disks were chowned to the
// qemu account but the directory above them was created by the action journal's
// plain `mkdir -p`, as root, and MkdirOwnedCommand then refused to re-own an
// existing directory. Under a restrictive umask that leaves a 0700 root-owned
// parent, so qemu cannot traverse to disks it demonstrably owns, and the sync
// reports success.
//
// The command must therefore CHOWN, not merely create.
func TestEnsureTargetDiskDirsOwnsWhatItCreates(t *testing.T) {
	r := &recordingRunner{}
	cfg := syncConfig{TargetDiskPath: "", TargetDiskOwner: "qemu:qemu"}
	disks := []disk.QcowDisk{diskAt("vda", "/data/replicas/vm-disk0.qcow2")}

	if err := ensureTargetDiskDirs(context.Background(), r, cfg, disks); err != nil {
		t.Fatalf("ensureTargetDiskDirs: %v", err)
	}
	if len(r.commands) != 1 {
		t.Fatalf("expected 1 command for 1 directory, got %d: %q", len(r.commands), r.commands)
	}
	cmd := r.commands[0]
	if !strings.Contains(cmd, "mkdir") {
		t.Errorf("command does not create the directory: %q", cmd)
	}
	if !strings.Contains(cmd, "chown") {
		t.Errorf("command creates the directory but never chowns it, which is the whole defect: %q", cmd)
	}
	if !strings.Contains(cmd, "qemu:qemu") {
		t.Errorf("command does not name the owner the disks will get: %q", cmd)
	}
}

// TestEnsureTargetDiskDirsIsOneCommandPerDirectory pins the de-duplication: the
// ordinary layout puts every disk of a domain in one directory, and a mkdir per
// DISK would be an SSH round trip per disk to create the same path.
func TestEnsureTargetDiskDirsIsOneCommandPerDirectory(t *testing.T) {
	for _, tc := range []struct {
		name     string
		diskPath string
		disks    []disk.QcowDisk
	}{
		{
			name:     "four disks, one directory",
			diskPath: "",
			disks: []disk.QcowDisk{
				diskAt("vda", "/data/replicas/vm-disk0.qcow2"),
				diskAt("vdb", "/data/replicas/vm-disk1.qcow2"),
				diskAt("vdc", "/data/replicas/vm-disk2.qcow2"),
				diskAt("vdd", "/data/replicas/vm-disk3.qcow2"),
			},
		},
		{
			// No -target-disk-path, so each disk keeps the source's own path
			// and the directories genuinely differ.
			name:     "two disks, two directories",
			diskPath: "",
			disks: []disk.QcowDisk{
				diskAt("vda", "/pool-a/vm-disk0.qcow2"),
				diskAt("vdb", "/pool-b/vm-disk1.qcow2"),
			},
		},
		{
			name:     "two directories, interleaved and repeated",
			diskPath: "",
			disks: []disk.QcowDisk{
				diskAt("vda", "/pool-a/vm-disk0.qcow2"),
				diskAt("vdb", "/pool-b/vm-disk1.qcow2"),
				diskAt("vdc", "/pool-a/vm-disk2.qcow2"),
				diskAt("vdd", "/pool-b/vm-disk3.qcow2"),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &recordingRunner{}
			cfg := syncConfig{TargetDiskPath: tc.diskPath, TargetDiskOwner: "qemu:qemu"}

			if err := ensureTargetDiskDirs(context.Background(), r, cfg, tc.disks); err != nil {
				t.Fatalf("ensureTargetDiskDirs: %v", err)
			}
			want := wantDirs(tc.diskPath, tc.disks)
			if len(r.commands) != len(want) {
				t.Fatalf("expected %d command(s) for %d distinct director(ies) across %d disks, got %d: %q",
					len(want), len(want), len(tc.disks), len(r.commands), r.commands)
			}
			for i, dir := range want {
				if !strings.Contains(r.commands[i], dir) {
					t.Errorf("command %d does not name directory %q: %q", i, dir, r.commands[i])
				}
			}
		})
	}
}

// TestEnsureTargetDiskDirsRespectsOwnerOff asserts that -target-disk-owner=off
// still creates the directories and simply does not chown them. Off means "do
// not touch ownership", not "do not prepare the target" -- a run that skipped
// the mkdir entirely would fail later at qemu-img create for a reason that
// names the disk rather than the flag.
func TestEnsureTargetDiskDirsRespectsOwnerOff(t *testing.T) {
	r := &recordingRunner{}
	cfg := syncConfig{TargetDiskPath: "", TargetDiskOwner: util.DiskOwnerOff}
	disks := []disk.QcowDisk{diskAt("vda", "/data/replicas/vm-disk0.qcow2")}

	if err := ensureTargetDiskDirs(context.Background(), r, cfg, disks); err != nil {
		t.Fatalf("ensureTargetDiskDirs: %v", err)
	}
	if len(r.commands) != 1 {
		t.Fatalf("expected the directory to still be created, got %d command(s): %q", len(r.commands), r.commands)
	}
	if strings.Contains(r.commands[0], "chown") {
		t.Errorf("-target-disk-owner=%s must not chown anything: %q", util.DiskOwnerOff, r.commands[0])
	}
	if !strings.Contains(r.commands[0], "mkdir") {
		t.Errorf("-target-disk-owner=%s must still create the directory: %q", util.DiskOwnerOff, r.commands[0])
	}
}

// TestEnsureTargetDiskDirsReportsWhichDirectoryFailed keeps the failure
// actionable. This runs before anything has been written to the target, so the
// error is the operator's only clue, and "create remote target dir" without the
// path would send them looking at the disk instead -- the same wrong place the
// original defect sent everyone.
func TestEnsureTargetDiskDirsReportsWhichDirectoryFailed(t *testing.T) {
	cfg := syncConfig{TargetDiskPath: "", TargetDiskOwner: "qemu:qemu"}
	disks := []disk.QcowDisk{
		diskAt("vda", "/pool-a/vm-disk0.qcow2"),
		diskAt("vdb", "/pool-b/vm-disk1.qcow2"),
	}
	failing := path.Dir(util.SetTargetPath("", disks[1].RootSource))

	r := &recordingRunner{failOn: failing}
	err := ensureTargetDiskDirs(context.Background(), r, cfg, disks)
	if err == nil {
		t.Fatal("expected an error when the mkdir fails")
	}
	if !strings.Contains(err.Error(), failing) {
		t.Errorf("error does not name the directory that failed (%q): %v", failing, err)
	}
	// Stops at the failure rather than carrying on: the run is over, and a
	// second mkdir against a target that just refused one is noise.
	if len(r.commands) != 2 {
		t.Errorf("expected to stop at the failing directory (2 commands), got %d: %q", len(r.commands), r.commands)
	}
}

// TestEnsureTargetDiskDirsHandlesNoDisks guards the degenerate input. A domain
// with no qcow2 disks is refused long before this point, so the only way here
// is a future caller moving the call earlier -- which should be a no-op, not a
// panic on disks[0].
func TestEnsureTargetDiskDirsHandlesNoDisks(t *testing.T) {
	r := &recordingRunner{}
	cfg := syncConfig{TargetDiskPath: "", TargetDiskOwner: "qemu:qemu"}

	if err := ensureTargetDiskDirs(context.Background(), r, cfg, nil); err != nil {
		t.Fatalf("ensureTargetDiskDirs with no disks: %v", err)
	}
	if len(r.commands) != 0 {
		t.Errorf("expected no commands for no disks, got %q", r.commands)
	}
}
