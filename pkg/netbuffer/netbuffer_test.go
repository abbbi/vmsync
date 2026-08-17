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

package netbuffer

import (
	"strings"
	"testing"
)

func TestParseSpec(t *testing.T) {
	tests := []struct {
		name      string
		spec      string
		wantBlock string
		wantSize  string
		wantErr   bool
		errSubstr string
	}{
		{"empty spec disables netbuffer", "", "", "", false, ""},
		{"valid spec with suffixes", "64k,512M", "64k", "512M", false, ""},
		{"valid spec with bare byte counts", "1024,2048", "1024", "2048", false, ""},
		{"valid spec is case-insensitive", "64K,512m", "64K", "512m", false, ""},
		{"missing comma is malformed", "64k512M", "", "", true, "must be of the form"},
		{"empty block half is malformed", ",512M", "", "", true, "must be of the form"},
		{"empty size half is malformed", "64k,", "", "", true, "must be of the form"},
		{"invalid block suffix", "64x,512M", "", "", true, "block size"},
		{"non-numeric block", "abc,512M", "", "", true, "block size"},
		{"invalid buffer suffix", "64k,512x", "", "", true, "buffer size"},
		{"non-numeric buffer", "64k,abc", "", "", true, "buffer size"},
		{"zero buffer size (bare) is rejected", "64k,0", "", "", true, "wrong buffer size"},
		{"zero buffer size (with suffix) is rejected", "64k,0M", "", "", true, "wrong buffer size"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			block, size, err := ParseSpec(tt.spec)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseSpec(%q) error = %v, wantErr %v", tt.spec, err, tt.wantErr)
			}
			if tt.wantErr {
				if tt.errSubstr != "" && !strings.Contains(err.Error(), tt.errSubstr) {
					t.Errorf("ParseSpec(%q) error = %q, want substring %q", tt.spec, err.Error(), tt.errSubstr)
				}
				return
			}
			if block != tt.wantBlock || size != tt.wantSize {
				t.Errorf("ParseSpec(%q) = (%q, %q), want (%q, %q)", tt.spec, block, size, tt.wantBlock, tt.wantSize)
			}
		})
	}
}
