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

// Which of a source domain's disks a replica does not have.
package libvirtsync

import (
	"sort"
	"strings"
)

// DiskExclusions is the two sets a run was told not to replicate, by
// <target dev=>.
//
// One type rather than two parameters so the two cannot be passed in the wrong
// order: they differ only in what the replica's definition says afterwards,
// which makes them exactly the kind of same-typed pair a positional call site
// swaps silently.
//
//   - Ignored: the <disk> stays in the replica's definition exactly as the
//     source declares it, path included. For storage the target has by other
//     means -- a SAN LUN, an array-replicated volume -- where the replica is
//     meant to keep the device under the name it already has.
//   - Dropped: the <disk> element is removed. For a disk nothing on the target
//     provides, where a definition naming the source's path would point the
//     replica at a file that is not there.
//
// Neither disk is copied either way. An empty DiskExclusions is the ordinary
// case and must behave exactly as the code did before they existed.
type DiskExclusions struct {
	Ignored []string
	Dropped []string
}

// Devices is both sets together, sorted and deduplicated: what gets recorded
// on the replica and what a message lists.
//
// Sorted because this value is compared between runs. A set that reordered
// itself would read as a set that had changed.
func (e DiskExclusions) Devices() []string {
	seen := map[string]bool{}
	var out []string
	for _, dev := range append(append([]string{}, e.Ignored...), e.Dropped...) {
		if dev == "" || seen[dev] {
			continue
		}
		seen[dev] = true
		out = append(out, dev)
	}
	sort.Strings(out)
	return out
}

// Empty is true when this run excludes nothing, which is every ordinary pair.
func (e DiskExclusions) Empty() bool {
	return len(e.Ignored) == 0 && len(e.Dropped) == 0
}

// sameDeviceSet compares two device lists as sets, ignoring order and repeats.
//
// Used to assert that a removal removed exactly what it was asked to. Order
// cannot be relied on: the request is in the operator's order and the removal
// walks the document's.
func sameDeviceSet(a, b []string) bool {
	set := func(in []string) map[string]bool {
		out := map[string]bool{}
		for _, s := range in {
			if s != "" {
				out[s] = true
			}
		}
		return out
	}
	x, y := set(a), set(b)
	if len(x) != len(y) {
		return false
	}
	for k := range x {
		if !y[k] {
			return false
		}
	}
	return true
}

// listOrNoneDevs renders a device list for a message, never as an empty gap.
//
// "yielded (none)" is a sentence; "yielded " is a reader wondering whether the
// message was truncated.
func listOrNoneDevs(devs []string) string {
	if len(devs) == 0 {
		return "(none)"
	}
	return strings.Join(devs, ", ")
}
