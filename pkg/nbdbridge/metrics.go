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

package nbdbridge

import (
	"fmt"

	"vmsync/pkg/nbdsync"
)

// SumLogicalDirtyBytes returns the total logical (uncompressed) bytes that
// will be copied for a set of extents, for comparison against the actual
// wire bytes a bridge reports.
func SumLogicalDirtyBytes(extents []nbdsync.Extent) uint64 {
	var total uint64
	for _, e := range extents {
		if e.Dirty {
			total += e.Length
		}
	}
	return total
}

// FormatSavings renders a human-readable compression summary given the
// logical byte count and the actual bytes sent over the wire.
func FormatSavings(logicalBytes, wireBytes uint64) string {
	if logicalBytes == 0 {
		return "n/a (no data copied)"
	}
	ratio := float64(wireBytes) / float64(logicalBytes)
	saved := (1 - ratio) * 100
	return fmt.Sprintf("%.1f%% smaller on the wire (%d -> %d bytes)", saved, logicalBytes, wireBytes)
}
