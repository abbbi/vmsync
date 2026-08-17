/*
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

package disk

import "testing"

// TestResolveRootSource pins down QcowDisk.RootSource's actual resolution
// logic -- previously inline in cmd/vmsync/main.go's disk-info loop, with no
// test coverage at all despite it deciding what every target file is named
// (see ResolveRootSource's and QcowDisk.RootSource's own doc comments).
func TestResolveRootSource(t *testing.T) {
	tests := []struct {
		name   string
		chain  []QemuImgInfo
		source string
		want   string
	}{
		{
			name:   "no backing file: single-element chain resolves to itself, matching source",
			chain:  []QemuImgInfo{{Filename: "/var/lib/libvirt/images/vm.qcow2"}},
			source: "/var/lib/libvirt/images/vm.qcow2",
			want:   "/var/lib/libvirt/images/vm.qcow2",
		},
		{
			// The external-snapshot/linked-clone case: chain is ordered top
			// (source, the domain's currently active file) to base, so the
			// disk's stable target-side name comes from the LAST element,
			// not source itself.
			name: "external snapshot or linked clone: two-element chain resolves to the base, not the active source",
			chain: []QemuImgInfo{
				{Filename: "/var/lib/libvirt/images/vm.qcow2.snap1"},
				{Filename: "/var/lib/libvirt/images/vm.qcow2"},
			},
			source: "/var/lib/libvirt/images/vm.qcow2.snap1",
			want:   "/var/lib/libvirt/images/vm.qcow2",
		},
		{
			name: "deeper chain (multiple stacked overlays) still resolves to the final base",
			chain: []QemuImgInfo{
				{Filename: "/var/lib/libvirt/images/vm.qcow2.snap2"},
				{Filename: "/var/lib/libvirt/images/vm.qcow2.snap1"},
				{Filename: "/var/lib/libvirt/images/vm-base.qcow2"},
			},
			source: "/var/lib/libvirt/images/vm.qcow2.snap2",
			want:   "/var/lib/libvirt/images/vm-base.qcow2",
		},
		{
			// Defensive-only path (see ResolveRootSource's own doc comment):
			// the real callers never actually produce an empty chain without
			// also returning an error first.
			name:   "empty chain falls back to source instead of panicking",
			chain:  nil,
			source: "/var/lib/libvirt/images/vm.qcow2",
			want:   "/var/lib/libvirt/images/vm.qcow2",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolveRootSource(tt.chain, tt.source); got != tt.want {
				t.Errorf("ResolveRootSource(%v, %q) = %q, want %q", tt.chain, tt.source, got, tt.want)
			}
		})
	}
}
