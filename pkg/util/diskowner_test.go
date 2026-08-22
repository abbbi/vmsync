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

package util

import (
	"context"
	"strings"
	"testing"
)

func TestParseDiskOwner(t *testing.T) {
	for _, tc := range []struct {
		name, spec        string
		wantUser, wantGrp string
		auto, off, bad    bool
	}{
		{name: "unset means auto, so never setting the flag is safe", spec: "", auto: true},
		{name: "auto", spec: "auto", auto: true},
		{name: "off restores the old behaviour", spec: "off", off: true},
		{name: "RHEL", spec: "qemu:qemu", wantUser: "qemu", wantGrp: "qemu"},
		{name: "Debian", spec: "libvirt-qemu:kvm", wantUser: "libvirt-qemu", wantGrp: "kvm"},
		{name: "user only leaves the group alone", spec: "qemu", wantUser: "qemu"},
		{name: "group only", spec: ":kvm", wantGrp: "kvm"},
		{name: "surrounding space is an operator typo, not an error", spec: "  qemu:qemu  ", wantUser: "qemu", wantGrp: "qemu"},
		{name: "a machine account", spec: "svc_vm$:kvm", wantUser: "svc_vm$", wantGrp: "kvm"},

		// This value is interpolated into a chown running as root on
		// another machine. ShQuote makes that safe; refusing outright means
		// a typo fails loudly here rather than confusingly over there.
		{name: "a shell metacharacter", spec: "qemu;rm -rf /", bad: true},
		{name: "a substitution attempt", spec: "$(id -u)", bad: true},
		{name: "a backtick", spec: "qemu:`id`", bad: true},
		{name: "a leading dash could be read as a chown flag", spec: "-R", bad: true},
		{name: "neither half given", spec: ":", bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseDiskOwner(tc.spec)
			if tc.bad {
				if err == nil {
					t.Fatalf("ParseDiskOwner(%q) was accepted, want refused", tc.spec)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseDiskOwner(%q) = %v", tc.spec, err)
			}
			if got.IsAuto() != tc.auto {
				t.Errorf("IsAuto() = %v, want %v", got.IsAuto(), tc.auto)
			}
			if got.IsOff() != tc.off {
				t.Errorf("IsOff() = %v, want %v", got.IsOff(), tc.off)
			}
			if got.User != tc.wantUser || got.Group != tc.wantGrp {
				t.Errorf("got %q:%q, want %q:%q", got.User, got.Group, tc.wantUser, tc.wantGrp)
			}
		})
	}
}

func TestDiskOwnerSpec(t *testing.T) {
	for _, tc := range []struct {
		owner DiskOwner
		want  string
	}{
		{DiskOwner{User: "qemu", Group: "qemu"}, "qemu:qemu"},
		{DiskOwner{User: "qemu"}, "qemu"},
		{DiskOwner{Group: "kvm"}, ":kvm"},
	} {
		if got := tc.owner.Spec(); got != tc.want {
			t.Errorf("Spec() = %q, want %q", got, tc.want)
		}
	}
}

// Preserving what owned the file before is the deterministic half of the
// fix: -reinit renames the correctly-owned disk aside and creates a fresh
// root-owned one, so without this a replica that WAS bootable silently
// stops being so.
func TestParseStatOwner(t *testing.T) {
	for _, tc := range []struct {
		name, out         string
		wantUser, wantGrp string
	}{
		{name: "the ordinary case", out: "qemu:qemu\n", wantUser: "qemu", wantGrp: "qemu"},
		{name: "Debian", out: "libvirt-qemu:kvm\n", wantUser: "libvirt-qemu", wantGrp: "kvm"},
		{name: "root, which is what the bug produces", out: "root:root\n", wantUser: "root", wantGrp: "root"},
		{
			name:     "numeric when the host has no passwd entry, still chown-able",
			out:      "107:107\n",
			wantUser: "107", wantGrp: "107",
		},
		{
			// `stat ... || true` leaves the error text on the stream and
			// exits 0, so the absence of a file arrives looking like this.
			name: "no such file", out: "stat: cannot statx '/x.qcow2': No such file or directory\n",
		},
		{name: "empty", out: ""},
		{name: "whitespace only", out: "  \n\t\n"},
		{name: "not an owner at all", out: "something went wrong\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseStatOwner(tc.out)
			if got.User != tc.wantUser || got.Group != tc.wantGrp {
				t.Errorf("got %q:%q, want %q:%q", got.User, got.Group, tc.wantUser, tc.wantGrp)
			}
			if tc.wantUser == "" && !got.Empty() {
				t.Error("unparseable output must yield an empty owner, not a partial one")
			}
		})
	}
}

// The rule that keeps this honest: a COMMENTED setting is not in force, and
// the value behind the comment differs by distribution -- `qemu` on RHEL,
// `libvirt-qemu` on Debian. Reading one as though it applied would produce a
// confident chown to possibly the wrong user, which is worse than saying
// nothing because it looks like the problem was handled.
func TestParseQemuConfOwner(t *testing.T) {
	const rhelDefault = `
# The user for QEMU processes run by the system instance.
#user = "qemu"
#group = "qemu"
`
	if o := ParseQemuConfOwner(rhelDefault); !o.Empty() {
		t.Errorf("a fully commented qemu.conf yielded %q -- a commented default is not a setting in force", o.Spec())
	}

	const configured = `
#user = "root"
user = "qemu"
group = "qemu"
`
	o := ParseQemuConfOwner(configured)
	if o.User != "qemu" || o.Group != "qemu" {
		t.Errorf("got %q:%q, want qemu:qemu", o.User, o.Group)
	}
	if o.Source == "" {
		t.Error("a determined owner should say where it came from")
	}

	t.Run("last assignment wins, as libvirt itself parses it", func(t *testing.T) {
		got := ParseQemuConfOwner("user = \"first\"\nuser = \"second\"\n")
		if got.User != "second" {
			t.Errorf("got %q, want second", got.User)
		}
	})

	t.Run("leading whitespace is still in force", func(t *testing.T) {
		got := ParseQemuConfOwner("   user = \"qemu\"\n")
		if got.User != "qemu" {
			t.Errorf("got %q, want qemu", got.User)
		}
	})

	t.Run("a commented line later in the file does not undo a real one", func(t *testing.T) {
		got := ParseQemuConfOwner("user = \"qemu\"\n#user = \"root\"\n")
		if got.User != "qemu" {
			t.Errorf("got %q, want qemu", got.User)
		}
	})

	t.Run("only a group configured", func(t *testing.T) {
		got := ParseQemuConfOwner("group = \"kvm\"\n")
		if got.User != "" || got.Group != "kvm" {
			t.Errorf("got %q:%q, want :kvm", got.User, got.Group)
		}
		if got.Empty() {
			t.Error("a group on its own is still an answer")
		}
	})

	t.Run("empty file", func(t *testing.T) {
		if !ParseQemuConfOwner("").Empty() {
			t.Error("an empty qemu.conf cannot determine anything")
		}
	})
}

// The value reaches a root shell on another machine, so quoting is not
// optional even though the parser already refuses metacharacters.
func TestChownCommandQuotes(t *testing.T) {
	cmd := ChownCommand(DiskOwner{User: "qemu", Group: "qemu"}, "/var/lib/libvirt/images/a b.qcow2")
	if !strings.Contains(cmd, "chown ") {
		t.Fatalf("not a chown: %q", cmd)
	}
	if !strings.Contains(cmd, `'/var/lib/libvirt/images/a b.qcow2'`) {
		t.Errorf("a path with a space is not quoted: %q", cmd)
	}
	if !strings.Contains(cmd, "'qemu:qemu'") {
		t.Errorf("the owner is not quoted: %q", cmd)
	}
}

// stat must never fail the caller: "there is no file here" is an ordinary
// answer on a first-ever sync, not an error.
func TestStatOwnerCommandToleratesAMissingFile(t *testing.T) {
	cmd := StatOwnerCommand("/var/lib/libvirt/images/x.qcow2")
	if !strings.Contains(cmd, "|| true") {
		t.Errorf("stat should not be able to fail the run: %q", cmd)
	}
	if !strings.Contains(cmd, "%U:%G") {
		t.Errorf("stat should ask for names, not numeric ids: %q", cmd)
	}
}

func TestReadQemuConfOwner(t *testing.T) {
	t.Run("finds a configured owner", func(t *testing.T) {
		r := &fakeRunner{out: "user = \"qemu\"\ngroup = \"qemu\"\n"}
		got := ReadQemuConfOwner(context.Background(), r)
		if got.User != "qemu" || got.Group != "qemu" {
			t.Errorf("got %q:%q, want qemu:qemu", got.User, got.Group)
		}
	})

	t.Run("an unreadable host is not an error", func(t *testing.T) {
		r := &fakeRunner{err: context.DeadlineExceeded}
		if got := ReadQemuConfOwner(context.Background(), r); !got.Empty() {
			t.Errorf("got %q, want empty -- this is best-effort and must not fail a sync", got.Spec())
		}
	})

	t.Run("a fully commented qemu.conf determines nothing", func(t *testing.T) {
		r := &fakeRunner{out: "#user = \"qemu\"\n"}
		if got := ReadQemuConfOwner(context.Background(), r); !got.Empty() {
			t.Errorf("got %q, want empty", got.Spec())
		}
	})
}

// scriptedRunner answers each command from a table keyed by a substring of
// it, so a test can say "this host has a qemu account but no libvirt-qemu"
// without caring about the exact getent spelling.
type scriptedRunner struct {
	replies map[string]string
	seen    []string
}

func (s *scriptedRunner) Run(_ context.Context, cmd string) (string, error) {
	s.seen = append(s.seen, cmd)
	for frag, out := range s.replies {
		if strings.Contains(cmd, frag) {
			return out, nil
		}
	}
	return "no\n", nil
}

// The last-resort detection, which is what a FIRST-EVER sync relies on: every
// distribution ships qemu.conf with the setting commented out, so a first
// sync onto a fresh target reaches this and nothing else.
func TestDetectQemuAccount(t *testing.T) {
	t.Run("RHEL: a qemu account and a qemu group", func(t *testing.T) {
		r := &scriptedRunner{replies: map[string]string{
			"passwd 'qemu'": "yes\n",
			"group 'qemu'":  "yes\n",
		}}
		got, found := DetectQemuAccount(context.Background(), r)
		if got.User != "qemu" || got.Group != "qemu" {
			t.Errorf("got %q:%q, want qemu:qemu", got.User, got.Group)
		}
		if len(found) != 1 {
			t.Errorf("found %v, want exactly one candidate", found)
		}
		if got.Source == "" {
			t.Error("an inferred owner must say it was inferred")
		}
	})

	t.Run("Debian: libvirt-qemu in the kvm group", func(t *testing.T) {
		r := &scriptedRunner{replies: map[string]string{
			"passwd 'libvirt-qemu'": "yes\n",
			"group 'kvm'":           "yes\n",
		}}
		got, _ := DetectQemuAccount(context.Background(), r)
		if got.User != "libvirt-qemu" || got.Group != "kvm" {
			t.Errorf("got %q:%q, want libvirt-qemu:kvm", got.User, got.Group)
		}
	})

	t.Run("the user exists but its group does not", func(t *testing.T) {
		// Naming an absent group would make the chown fail outright, throwing
		// away a correct answer for the user half.
		r := &scriptedRunner{replies: map[string]string{"passwd 'qemu'": "yes\n"}}
		got, _ := DetectQemuAccount(context.Background(), r)
		if got.User != "qemu" {
			t.Errorf("got user %q, want qemu", got.User)
		}
		if got.Group != "" {
			t.Errorf("got group %q, want it omitted since it does not exist", got.Group)
		}
	})

	t.Run("no known account at all", func(t *testing.T) {
		r := &scriptedRunner{}
		got, found := DetectQemuAccount(context.Background(), r)
		if !got.Empty() || len(found) != 0 {
			t.Errorf("got %q / %v, want nothing determined", got.Spec(), found)
		}
	})

	t.Run("both accounts exist, which must not be resolved silently", func(t *testing.T) {
		r := &scriptedRunner{replies: map[string]string{
			"passwd 'qemu'":         "yes\n",
			"passwd 'libvirt-qemu'": "yes\n",
			"group":                 "yes\n",
		}}
		got, found := DetectQemuAccount(context.Background(), r)
		if !got.Empty() {
			t.Errorf("got %q -- an ambiguous host must be reported, not guessed at", got.Spec())
		}
		if len(found) != 2 {
			t.Errorf("found %v, want both candidates reported so the warning can name them", found)
		}
	})
}

func TestParseAccountExists(t *testing.T) {
	for out, want := range map[string]bool{
		"yes\n": true, "yes": true, " yes \n": true,
		"no\n": false, "": false, "getent: command not found\n": false,
	} {
		if got := ParseAccountExists(out); got != want {
			t.Errorf("ParseAccountExists(%q) = %v, want %v", out, got, want)
		}
	}
}

func TestAccountExistsCommandUsesNSS(t *testing.T) {
	cmd := AccountExistsCommand("passwd", "qemu")
	if !strings.Contains(cmd, "getent") {
		t.Errorf("must consult NSS so an LDAP/SSSD account is found too: %q", cmd)
	}
	if !strings.Contains(cmd, "'qemu'") {
		t.Errorf("the name is not quoted: %q", cmd)
	}
}
