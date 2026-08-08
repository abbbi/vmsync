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
	"strings"
	"testing"
)

func TestConfigEnabled(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"zero value is disabled", Config{}, false},
		{"compress alone enables it", Config{Compress: true}, true},
		{"netbuffer block alone enables it", Config{NetBufferBlock: "64k"}, true},
		{"netbuffer size alone enables it", Config{NetBufferSize: "512M"}, true},
		{"compress and netbuffer together", Config{Compress: true, NetBufferBlock: "64k", NetBufferSize: "512M"}, true},
		{"UseSSH alone does not enable it", Config{UseSSH: true}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.Enabled(); got != tt.want {
				t.Errorf("Config{%+v}.Enabled() = %v, want %v", tt.cfg, got, tt.want)
			}
		})
	}
}

func TestConfigNetBufferEnabled(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"zero value is disabled", Config{}, false},
		{"block set alone", Config{NetBufferBlock: "64k"}, true},
		{"size set alone", Config{NetBufferSize: "512M"}, true},
		{"both set", Config{NetBufferBlock: "64k", NetBufferSize: "512M"}, true},
		{"compress alone does not count as netbuffer", Config{Compress: true}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.NetBufferEnabled(); got != tt.want {
				t.Errorf("Config{%+v}.NetBufferEnabled() = %v, want %v", tt.cfg, got, tt.want)
			}
		})
	}
}

func TestValidateCompressAlgo(t *testing.T) {
	tests := []struct {
		name    string
		algo    string
		wantErr bool
	}{
		{"empty defaults to zstd, so it's valid", "", false},
		{"zstd is valid", "zstd", false},
		{"s2 is valid", "s2", false},
		{"unknown algo is invalid", "gzip", true},
		{"case matters, so uppercase is invalid", "ZSTD", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateCompressAlgo(tt.algo)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateCompressAlgo(%q) error = %v, wantErr %v", tt.algo, err, tt.wantErr)
			}
		})
	}
}

func TestValidateCompressLevel(t *testing.T) {
	tests := []struct {
		name    string
		algo    string
		level   string
		wantErr bool
	}{
		{"zstd lower bound valid", "zstd", "1", false},
		{"zstd upper bound valid", "zstd", "19", false},
		{"zstd mid-range valid", "zstd", "10", false},
		{"zstd zero is out of range", "zstd", "0", true},
		{"zstd 20 is out of range", "zstd", "20", true},
		{"zstd negative is out of range", "zstd", "-1", true},
		{"zstd non-numeric is invalid", "zstd", "fast", true},
		{"zstd empty is invalid", "zstd", "", true},
		{"empty algo defaults to zstd's numeric rules", "", "5", false},
		{"empty algo still rejects s2-style levels", "", "better", true},
		{"s2 default is valid", "s2", "default", false},
		{"s2 better is valid", "s2", "better", false},
		{"s2 best is valid", "s2", "best", false},
		{"s2 numeric level is invalid", "s2", "3", true},
		{"s2 empty is invalid", "s2", "", true},
		{"s2 unknown mode is invalid", "s2", "fastest", true},
		{"unknown algo surfaces the algo error", "gzip", "3", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateCompressLevel(tt.algo, tt.level)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateCompressLevel(%q, %q) error = %v, wantErr %v", tt.algo, tt.level, err, tt.wantErr)
			}
		})
	}
}

func TestParseNetBufferSpec(t *testing.T) {
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
			block, size, err := ParseNetBufferSpec(tt.spec)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseNetBufferSpec(%q) error = %v, wantErr %v", tt.spec, err, tt.wantErr)
			}
			if tt.wantErr {
				if tt.errSubstr != "" && !strings.Contains(err.Error(), tt.errSubstr) {
					t.Errorf("ParseNetBufferSpec(%q) error = %q, want substring %q", tt.spec, err.Error(), tt.errSubstr)
				}
				return
			}
			if block != tt.wantBlock || size != tt.wantSize {
				t.Errorf("ParseNetBufferSpec(%q) = (%q, %q), want (%q, %q)", tt.spec, block, size, tt.wantBlock, tt.wantSize)
			}
		})
	}
}
