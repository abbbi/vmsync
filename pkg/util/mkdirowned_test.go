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
	"reflect"
	"strings"
	"testing"
)

func TestAncestorPaths(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"/data/replicas/web01", []string{"/data", "/data/replicas", "/data/replicas/web01"}},
		{"/data", []string{"/data"}},
		// The root is never a component. A chown of / that somehow got
		// through would be catastrophic rather than merely wrong.
		{"/", nil},
		{"", nil},
		// Cleaned first, so a non-canonical path does not produce a component
		// per ".." that would then be chowned individually.
		{"/data/../data/replicas", []string{"/data", "/data/replicas"}},
		{"/data/replicas/", []string{"/data", "/data/replicas"}},
	} {
		t.Run(tc.in, func(t *testing.T) {
			if got := ancestorPaths(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ancestorPaths(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// With no owner to apply, the command must be exactly what it always was.
// This path is reached whenever -target-disk-owner is off, or auto resolved
// to nothing, and it must not start doing something new on those hosts.
func TestMkdirOwnedCommandWithoutAnOwnerIsUnchanged(t *testing.T) {
	got := MkdirOwnedCommand(DiskOwner{}, "/data/replicas/web01")
	if got != "mkdir -p '/data/replicas/web01'" {
		t.Errorf("got %q, want a plain mkdir -p", got)
	}
	if strings.Contains(got, "chown") {
		t.Error("a command with no owner still chowns something")
	}
}

// One mkdir+chown pair per component, shallowest first, and a final mkdir -p
// that is NOT silenced so a genuine failure still reports its own message.
func TestMkdirOwnedCommandChownsEachComponent(t *testing.T) {
	o := DiskOwner{User: "qemu", Group: "qemu"}
	got := MkdirOwnedCommand(o, "/data/replicas/web01")

	for _, want := range []string{
		"mkdir '/data' 2>/dev/null && chown 'qemu:qemu' '/data';",
		"mkdir '/data/replicas' 2>/dev/null && chown 'qemu:qemu' '/data/replicas';",
		"mkdir '/data/replicas/web01' 2>/dev/null && chown 'qemu:qemu' '/data/replicas/web01';",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("command is missing %q\ngot: %s", want, got)
		}
	}
	if !strings.HasSuffix(got, "mkdir -p '/data/replicas/web01'") {
		t.Errorf("command does not end with an un-silenced mkdir -p, so a real failure would be swallowed\ngot: %s", got)
	}
	// Shallowest first: creating /data/replicas before /data would fail.
	if strings.Index(got, "'/data'") > strings.Index(got, "'/data/replicas'") {
		t.Errorf("components are not ordered shallowest-first:\n%s", got)
	}
}

// mkdir WITHOUT -p per component is the whole mechanism: it succeeds exactly
// when it created the directory, so `&& chown` runs only for new ones. A -p
// here would make every mkdir succeed and chown the operator's existing
// directories -- the precise thing this must never do.
func TestMkdirOwnedCommandNeverUsesDashPPerComponent(t *testing.T) {
	got := MkdirOwnedCommand(DiskOwner{User: "qemu"}, "/data/replicas/web01")
	for _, line := range strings.Split(got, ";") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "chown") && strings.Contains(line, "mkdir -p") {
			t.Errorf("a component uses `mkdir -p` before chown, which would re-own an existing directory: %q", line)
		}
	}
}

// Everything interpolated is quoted, because these run as root on another
// machine.
func TestMkdirOwnedCommandQuotesEverything(t *testing.T) {
	got := MkdirOwnedCommand(DiskOwner{User: "qemu", Group: "qemu"}, "/data/dir with space/web01")
	if !strings.Contains(got, `'/data/dir with space'`) {
		t.Errorf("a path containing a space is not quoted as one argument:\n%s", got)
	}
	// A path carrying a single quote must come back with it escaped, not
	// closing the quoted word. ShQuote's form is '\'' -- so every apostrophe
	// in the output has to be part of that sequence, never a bare one that
	// would end the argument and let the rest run as commands.
	nasty := MkdirOwnedCommand(DiskOwner{User: "qemu"}, "/data/x'y")
	if strings.Contains(nasty, `'/data/x'y`) {
		t.Errorf("a path with a quote escaped out of its argument:\n%s", nasty)
	}
	if !strings.Contains(nasty, `x'\''y`) {
		t.Errorf("a path with a quote was not run through ShQuote:\n%s", nasty)
	}
}

// A group-only owner is a real configuration -- qemu.conf can set group
// without user -- and chown accepts ":group".
func TestMkdirOwnedCommandHandlesAGroupOnlyOwner(t *testing.T) {
	got := MkdirOwnedCommand(DiskOwner{Group: "kvm"}, "/data/web01")
	if !strings.Contains(got, `chown ':kvm'`) {
		t.Errorf("a group-only owner was not applied:\n%s", got)
	}
}
