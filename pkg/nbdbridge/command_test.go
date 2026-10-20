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
			// from streamrelay's).
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

// BuildStopCommand's job is a CHOICE now, not a string: which verified kill to
// ask for. How that command behaves -- that it refuses a pid it cannot prove is
// still the helper, that it ends the process GROUP rather than the leader alone,
// that it waits for the process to actually go -- is asserted against real
// processes in pkg/util/verifiedkill_test.go, which is where a shell command
// belongs under test. What belongs here is that this package asks for the right
// one, and that what it used to emit cannot come back.
//
// This test previously pinned the old command as its contract, substring by
// substring, "|| true" included. It passed for as long as the defect existed
// and would have failed the day somebody fixed it -- which is what it just did.
// A test that describes the implementation cannot notice the implementation is
// wrong.
func TestBuildStopCommand(t *testing.T) {
	const pidFile = "/run/vmsync-bridge/vmsync-bridge-20200.pid"
	const logFile = "/run/vmsync-bridge/vmsync-bridge-20200.log"

	const helperPath = "/usr/local/bin/vmsync-bridge-helper"

	got := BuildStopCommand(pidFile, logFile, helperPath)

	// GROUP form, because the helper setsid()s out of its parent's group and
	// its relay children have to go with it; the log removed alongside the
	// pidfile, because a log naming a dead helper is what makes the next
	// run's diagnosis wrong; and comm taken from the INSTALLED helper's name,
	// because the operator chooses where that binary lives and a constant
	// would refuse every kill on a target that put it somewhere else.
	want := util.VerifiedKill{
		PidFile: pidFile,
		Comm:    "vmsync-bridge-helper",
		Group:   true,
		Remove:  []string{logFile},
	}.Command()
	if got != want {
		t.Errorf("BuildStopCommand(%q, %q, %q) does not ask for the verified group kill.\n got: %s\nwant: %s", pidFile, logFile, helperPath, got, want)
	}

	// And the comm it compares has to be what /proc actually holds.
	// "vmsync-bridge-helper" is twenty characters; the kernel stores fifteen.
	// Comparing the full name would refuse every kill against the very
	// process this command exists to stop.
	if !strings.Contains(got, `[ "$c" = 'vmsync-bridge-h' ]`) {
		t.Errorf("the comm comparison is not the kernel's truncation of %q, so it can never match:\n%s", "vmsync-bridge-helper", got)
	}

	// The defect, named in the three spellings it had here, so that this
	// function going back to building its own string is caught with a message
	// that says what is wrong rather than printing two long commands side by
	// side. kill -9 succeeds against any live pid, recycled or not, and
	// "|| true" hides that; aimed at a GROUP it takes out everything that now
	// answers to a number read out of a stale file.
	//
	// It does NOT guard the shared builder: a gutted VerifiedKill that still
	// produced one of these three strings would be caught, and one gutted any
	// other way would satisfy both this and the equality above, since the
	// equality compares against that same builder. What guards the builder is
	// its own behavioural suite in pkg/util, run against real processes.
	for _, forbidden := range []string{
		"kill -9 -$(cat",
		"kill -9 $(cat",
		"kill -9 -" + util.ShQuote(pidFile),
	} {
		if strings.Contains(got, forbidden) {
			t.Errorf("the unverified stop command is back: %q contains %q", got, forbidden)
		}
	}
}

// TestBuildReadinessCheckCommand pins down the exact shell string
// waitForRemoteListening polls with -- previously built inline, with no
// direct test coverage at all, despite mixing single- and double-quoting
// (a nested $(cat '...') substitution inside a double-quoted grep pattern)
// that's easy to get subtly wrong on a future edit.
func TestBuildReadinessCheckCommand(t *testing.T) {
	tests := []struct {
		name       string
		bridgePort int
		pidFile    string
	}{
		{"typical port and pidfile", 20200, "/run/vmsync-bridge/vmsync-bridge-20200.pid"},
		{"different port", 30301, "/run/vmsync-bridge/vmsync-bridge-30301.pid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildReadinessCheckCommand(tt.bridgePort, tt.pidFile)

			assertContainsAll(t, got,
				"ss -Htlnp",
				util.ShQuote(fmt.Sprintf("( sport = :%d )", tt.bridgePort)),
				"| grep -q",
				"$(cat "+util.ShQuote(tt.pidFile)+")",
				// The trailing comma is what disambiguates pid 123 from a
				// same-prefixed pid like 1234 or 12345 in ss's own output
				// (see BuildReadinessCheckCommand's own doc comment) --
				// losing it silently would be exactly the kind of subtle
				// regression this test exists to catch.
				"),\"",
			)
		})
	}
}

// TestBuildReadinessCheckCommandQuotesPidFileSafely confirms a pidFile
// containing a shell metacharacter (a single quote) still produces a
// syntactically safe command -- pidFile is always a deterministic path this
// package itself constructs in practice (see bridgeStateDir in remote.go),
// never user-controlled, but the nested quoting here is exactly the kind of
// thing worth pinning down explicitly rather than trusting by inspection.
func TestBuildReadinessCheckCommandQuotesPidFileSafely(t *testing.T) {
	const pidFile = "/run/vmsync-bridge/vm's-bridge.pid"
	got := BuildReadinessCheckCommand(20200, pidFile)

	wantCat := "$(cat " + util.ShQuote(pidFile) + ")"
	if !strings.Contains(got, wantCat) {
		t.Errorf("BuildReadinessCheckCommand(20200, %q) = %q, missing safely-quoted %q", pidFile, got, wantCat)
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

// The bridge helper runs on the far host and dials the real qemu-nbd
// there. That address was hardcoded to loopback, which is right for the
// default wildcard bind and wrong for a narrowed one: qemu-nbd is then not
// listening on loopback, so -source-nbd-bind/-target-nbd-bind narrowed to
// one interface made every bridged transport (--compress, --netbuffer)
// fail -- with the only trace in the helper's own log file on the far host.
func TestBuildStartCommandDialsTheBoundAddress(t *testing.T) {
	base := Config{HelperPath: "/usr/local/bin/vmsync-bridge-helper"}

	t.Run("unset means loopback, as before", func(t *testing.T) {
		got := BuildStartCommand(base, 10809, 10800, "/run/p.pid", "/run/l.log")
		if !strings.Contains(got, "127.0.0.1:10800") {
			t.Errorf("command does not dial loopback by default: %s", got)
		}
	})

	t.Run("a narrowed bind is dialled", func(t *testing.T) {
		cfg := base
		cfg.ConnectHost = "10.0.0.5"
		got := BuildStartCommand(cfg, 10809, 10800, "/run/p.pid", "/run/l.log")
		if !strings.Contains(got, "10.0.0.5:10800") {
			t.Errorf("command still dials loopback though qemu-nbd is bound elsewhere: %s", got)
		}
		if strings.Contains(got, "127.0.0.1:10800") {
			t.Errorf("command dials loopback as well as the bound address: %s", got)
		}
	})

	// An IPv6 address has to arrive bracketed or the helper cannot parse
	// its own -connect argument, which is why this uses net.JoinHostPort
	// rather than Sprintf.
	t.Run("an IPv6 bind is bracketed", func(t *testing.T) {
		cfg := base
		cfg.ConnectHost = "2001:db8::5"
		got := BuildStartCommand(cfg, 10809, 10800, "/run/p.pid", "/run/l.log")
		if !strings.Contains(got, "[2001:db8::5]:10800") {
			t.Errorf("an IPv6 connect address is not bracketed, so the helper cannot parse it: %s", got)
		}
	})

	// The LISTEN side keeps its own rule -- wildcard, or loopback under
	// -use-ssh -- and must not pick up the connect address.
	t.Run("the listen address is unaffected", func(t *testing.T) {
		cfg := base
		cfg.ConnectHost = "10.0.0.5"
		got := BuildStartCommand(cfg, 10809, 10800, "/run/p.pid", "/run/l.log")
		if !strings.Contains(got, "0.0.0.0:10809") {
			t.Errorf("the listen address changed with the connect address: %s", got)
		}
		cfg.UseSSH = true
		got = BuildStartCommand(cfg, 10809, 10800, "/run/p.pid", "/run/l.log")
		if !strings.Contains(got, "127.0.0.1:10809") {
			t.Errorf("-use-ssh no longer listens on loopback only: %s", got)
		}
	})
}
