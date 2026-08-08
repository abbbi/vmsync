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

	"vmsync/pkg/util"
)

// assertContainsAll fails the test if got is missing any of want as a
// substring.
func assertContainsAll(t *testing.T, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("command %q does not contain expected substring %q", got, w)
		}
	}
}

func TestBuildStartCommand(t *testing.T) {
	const (
		bridgePort = 20200
		realPort   = 10809
		pidFile    = "/run/vmsync-bridge/vmsync-bridge-20200.pid"
		logFile    = "/run/vmsync-bridge/vmsync-bridge-20200.log"
		helperPath = "/opt/vmsync/vmsync-bridge-helper"
	)

	tests := []struct {
		name    string
		cfg     Config
		want    []string
		notWant []string
	}{
		{
			name: "no compress, no netbuffer",
			cfg:  Config{HelperPath: helperPath},
			want: []string{
				"setsid sh -c",
				helperPath,
				"-listen",
				"0.0.0.0:20200",
				"-connect",
				"127.0.0.1:10809",
			},
			notWant: []string{"-compress", "-netbuffer"},
		},
		{
			name: "compress zstd with explicit level",
			cfg:  Config{HelperPath: helperPath, Compress: true, CompressAlgo: "zstd", CompressLevel: "7"},
			want: []string{
				"-compress=zstd", "-compress-level", "7",
			},
			notWant: []string{"-netbuffer"},
		},
		{
			name: "compress s2 with explicit level",
			cfg:  Config{HelperPath: helperPath, Compress: true, CompressAlgo: "s2", CompressLevel: "better"},
			want: []string{
				"-compress=s2", "-compress-level", "better",
			},
			notWant: []string{"-netbuffer"},
		},
		{
			// BuildStartCommand's own fallback: an empty CompressAlgo
			// defaults to "zstd" (see the "algo := cfg.CompressAlgo; if
			// algo == "" { algo = "zstd" }" block).
			name: "compress with empty algo defaults to zstd",
			cfg:  Config{HelperPath: helperPath, Compress: true, CompressAlgo: "", CompressLevel: "9"},
			want: []string{
				"-compress=zstd", "-compress-level", "9",
			},
		},
		{
			// BuildStartCommand's own fallback: an empty CompressLevel
			// defaults to "3" for zstd (its own hardcoded default, distinct
			// from zstdrelay's).
			name: "compress with empty level defaults to 3 for zstd",
			cfg:  Config{HelperPath: helperPath, Compress: true, CompressAlgo: "zstd", CompressLevel: ""},
			want: []string{
				"-compress=zstd", "-compress-level", "3",
			},
		},
		{
			// Same fallback, but the empty-algo default of "zstd" feeds
			// into the empty-level default too, ending up at "3" rather
			// than s2's "better".
			name: "compress with both empty defaults to zstd level 3",
			cfg:  Config{HelperPath: helperPath, Compress: true, CompressAlgo: "", CompressLevel: ""},
			want: []string{
				"-compress=zstd", "-compress-level", "3",
			},
		},
		{
			// BuildStartCommand's own fallback: an empty CompressLevel
			// defaults to "better" when the algo is s2, matching
			// cmd/vmsync's own -compress-level auto-default policy.
			name: "compress s2 with empty level defaults to better",
			cfg:  Config{HelperPath: helperPath, Compress: true, CompressAlgo: "s2", CompressLevel: ""},
			want: []string{
				"-compress=s2", "-compress-level", "better",
			},
		},
		{
			name: "netbuffer only",
			cfg:  Config{HelperPath: helperPath, NetBufferBlock: "64k", NetBufferSize: "512M"},
			want: []string{
				"-netbuffer=64k,512M",
			},
			notWant: []string{"-compress"},
		},
		{
			name: "compress and netbuffer together",
			cfg: Config{
				HelperPath: helperPath, Compress: true, CompressAlgo: "zstd", CompressLevel: "5",
				NetBufferBlock: "64k", NetBufferSize: "512M",
			},
			want: []string{
				"-compress=zstd", "-compress-level", "5", "-netbuffer=64k,512M",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildStartCommand(tt.cfg, bridgePort, realPort, pidFile, logFile)

			assertContainsAll(t, got, tt.want...)
			for _, nw := range tt.notWant {
				if strings.Contains(got, nw) {
					t.Errorf("command %q unexpectedly contains %q", got, nw)
				}
			}
			if !strings.Contains(got, logFile) {
				t.Errorf("command %q does not reference log file %q", got, logFile)
			}
			if !strings.Contains(got, pidFile) {
				t.Errorf("command %q does not reference pid file %q", got, pidFile)
			}
			if !strings.HasPrefix(got, "setsid sh -c ") {
				t.Errorf("command %q does not start with the expected backgrounding prefix", got)
			}
		})
	}
}

// TestBuildStartCommandUseSSHOnlyChangesListenHost pins down Config.UseSSH's
// documented effect precisely: it must change nothing about the produced
// command except which host the helper's listener binds to (0.0.0.0 by
// default, 127.0.0.1 when UseSSH is set) -- not the connect target, not the
// compress/netbuffer arguments, not the pidfile/logfile handling.
func TestBuildStartCommandUseSSHOnlyChangesListenHost(t *testing.T) {
	base := Config{
		HelperPath:     "/opt/vmsync/vmsync-bridge-helper",
		Compress:       true,
		CompressAlgo:   "zstd",
		CompressLevel:  "5",
		NetBufferBlock: "64k",
		NetBufferSize:  "512M",
	}
	const bridgePort = 20200
	const realPort = 10809
	const pidFile = "/run/vmsync-bridge/vmsync-bridge-20200.pid"
	const logFile = "/run/vmsync-bridge/vmsync-bridge-20200.log"

	withoutSSH := base
	withoutSSH.UseSSH = false
	withSSH := base
	withSSH.UseSSH = true

	gotWithoutSSH := BuildStartCommand(withoutSSH, bridgePort, realPort, pidFile, logFile)
	gotWithSSH := BuildStartCommand(withSSH, bridgePort, realPort, pidFile, logFile)

	if !strings.Contains(gotWithoutSSH, "0.0.0.0:20200") {
		t.Fatalf("UseSSH=false command %q does not listen on 0.0.0.0:20200", gotWithoutSSH)
	}
	if !strings.Contains(gotWithSSH, "127.0.0.1:20200") {
		t.Fatalf("UseSSH=true command %q does not listen on 127.0.0.1:20200", gotWithSSH)
	}
	// The connect target is always the real, local NBD export -- unaffected
	// by UseSSH either way.
	assertContainsAll(t, gotWithoutSSH, "127.0.0.1:10809")
	assertContainsAll(t, gotWithSSH, "127.0.0.1:10809")

	// Patching the listen host back in the UseSSH=true command should
	// reproduce the UseSSH=false command exactly -- proving that's the ONLY
	// difference between them. realPort (10809) differs from bridgePort
	// (20200) here specifically so this substring replacement can't
	// accidentally also touch the (unrelated) connect address.
	patched := strings.Replace(gotWithSSH, "127.0.0.1:20200", "0.0.0.0:20200", 1)
	if patched != gotWithoutSSH {
		t.Errorf("UseSSH changed more than just the listen host:\n  UseSSH=false: %q\n  UseSSH=true (patched back): %q", gotWithoutSSH, patched)
	}
}

func TestBuildStopCommand(t *testing.T) {
	const pidFile = "/run/vmsync-bridge/vmsync-bridge-20200.pid"
	const logFile = "/run/vmsync-bridge/vmsync-bridge-20200.log"

	got := BuildStopCommand(pidFile, logFile)

	wantSubstrings := []string{
		"kill -9 -$(cat " + util.ShQuote(pidFile) + ")",
		"|| true",
		"rm -f " + util.ShQuote(pidFile) + " " + util.ShQuote(logFile),
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(got, want) {
			t.Errorf("BuildStopCommand(%q, %q) = %q, missing expected substring %q", pidFile, logFile, got, want)
		}
	}
}

func TestQuoteArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no args", nil, ""},
		{"single simple arg", []string{"a"}, "'a'"},
		{"multiple simple args", []string{"a", "b", "c"}, "'a' 'b' 'c'"},
		{"arg with a space", []string{"hello world"}, "'hello world'"},
		{"arg with a single quote", []string{"it's"}, `'it'\''s'`},
		{"empty arg", []string{""}, "''"},
		{"mixed args", []string{"-listen", "0.0.0.0:20200"}, "'-listen' '0.0.0.0:20200'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := quoteArgs(tt.args...); got != tt.want {
				t.Errorf("quoteArgs(%v) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}
