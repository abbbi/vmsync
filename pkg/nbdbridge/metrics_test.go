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
	"testing"

	"vmsync/pkg/nbdsync"
)

func TestSumLogicalDirtyBytes(t *testing.T) {
	tests := []struct {
		name    string
		extents []nbdsync.Extent
		want    uint64
	}{
		{"nil slice", nil, 0},
		{"empty slice", []nbdsync.Extent{}, 0},
		{"single dirty extent", []nbdsync.Extent{
			{Offset: 0, Length: 4096, Dirty: true},
		}, 4096},
		{"single clean extent contributes nothing", []nbdsync.Extent{
			{Offset: 0, Length: 4096, Dirty: false},
		}, 0},
		{"mixed dirty and clean extents", []nbdsync.Extent{
			{Offset: 0, Length: 100, Dirty: true},
			{Offset: 100, Length: 5000, Dirty: false},
			{Offset: 5100, Length: 50, Dirty: true},
		}, 150},
		{"all dirty extents sum together", []nbdsync.Extent{
			{Offset: 0, Length: 100, Dirty: true},
			{Offset: 100, Length: 200, Dirty: true},
			{Offset: 300, Length: 300, Dirty: true},
		}, 600},
		{"zero-length dirty extent contributes nothing", []nbdsync.Extent{
			{Offset: 0, Length: 0, Dirty: true},
		}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SumLogicalDirtyBytes(tt.extents); got != tt.want {
				t.Errorf("SumLogicalDirtyBytes(%+v) = %d, want %d", tt.extents, got, tt.want)
			}
		})
	}
}

func TestFormatSavings(t *testing.T) {
	tests := []struct {
		name         string
		logicalBytes uint64
		wireBytes    uint64
		want         string
	}{
		{"no data copied at all", 0, 0, "n/a (no data copied)"},
		{"no data copied even with a nonzero wire count", 0, 500, "n/a (no data copied)"},
		{"fifty percent smaller", 1000, 500, "50.0% smaller on the wire (1000 -> 500 bytes)"},
		{"no savings at all", 1000, 1000, "0.0% smaller on the wire (1000 -> 1000 bytes)"},
		{"wire bigger than logical is a negative saving", 1000, 1200, "-20.0% smaller on the wire (1000 -> 1200 bytes)"},
		{"fully compressed away", 1000, 0, "100.0% smaller on the wire (1000 -> 0 bytes)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FormatSavings(tt.logicalBytes, tt.wireBytes); got != tt.want {
				t.Errorf("FormatSavings(%d, %d) = %q, want %q", tt.logicalBytes, tt.wireBytes, got, tt.want)
			}
		})
	}
}
