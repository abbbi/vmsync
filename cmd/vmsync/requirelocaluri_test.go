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

package main

import (
	"strings"
	"testing"
)

// requireLocalURI is the gate on every DR verb -- promote, invert, the two
// shutdowns, break-target-lock, explain-domain -- and what it guarantees is
// not "no ssh" but "this command acts on the machine it is typed on".
//
// Those two are different questions, and answering the second with the first
// let every non-ssh remote transport through. The verbs behind it then mixed
// a REMOTE libvirt with LOCAL filesystem and lock state: runPromote taking
// its run lock on the wrong host (so a sync of that replica contends with
// nothing at all), disk-presence evidence read from the wrong machine's
// files, and -invert running qemu-img bitmap --remove here against the
// disk paths it read from the REMOTE domain's definition -- a path
// collision, not a name one, and a symmetric DR pair makes it likely.
//
// Pure, so every transport libvirt offers can simply be asked.
func TestRequireLocalURIAcceptsOnlyWhatIsActuallyLocal(t *testing.T) {
	cases := []struct {
		uri   string
		local bool
		why   string
	}{
		// Genuinely this machine.
		{"qemu:///system", true, "the ordinary local URI"},
		{"qemu:///session", true, "the session URI the agent and the bench use"},
		{"qemu+unix:///system", true, "an explicit unix socket is still this machine"},
		{"", true, "no URI at all means libvirt's own default, which is local"},
		{"qemu+tcp://127.0.0.1/system", true, "loopback is this machine, however it is dialled"},
		{"qemu+tcp://localhost:16509/system", true, "and so is localhost, with or without a port"},

		// ssh: refused before this change and still refused.
		{"qemu+ssh://hyper01p/system", false, "ssh names another machine"},
		{"qemu+libssh://hyper01p/system", false, "a different ssh implementation is still ssh"},
		{"qemu+libssh2://hyper01p/system", false, "and so is this one"},
		{"qemu+ssh://localhost/system", false, "deliberately refused even at localhost: the operator named a user and a key, and running as whoever invoked vmsync instead would change which account owns the files"},

		// THE FIVE THIS GATE USED TO LET THROUGH. Every one of them is a
		// remote hypervisor reached without ssh.
		{"qemu+tcp://hyper01p/system", false, "tcp to another host is not local"},
		{"qemu+tls://hyper01p/system", false, "nor is tls"},
		{"qemu+tls://hyper01p:16514/system", false, "nor is tls on its usual port"},
		{"qemu://hyper01p/system", false, "a bare scheme with a host is still a host"},
		{"qemu+ext://hyper01p/system", false, "an external transport is a transport"},
	}

	for _, tc := range cases {
		t.Run(tc.uri, func(t *testing.T) {
			err := requireLocalURI(tc.uri, "-target-uri")
			if tc.local && err != nil {
				t.Errorf("requireLocalURI(%q) refused a local URI: %v -- %s", tc.uri, err, tc.why)
			}
			if !tc.local && err == nil {
				t.Errorf("requireLocalURI(%q) accepted a REMOTE URI as local -- %s. The verb behind this gate will take its run lock, read its disk evidence and write its journal on the wrong machine", tc.uri, tc.why)
			}
			if !tc.local && err != nil && !strings.Contains(err.Error(), "-target-uri") {
				t.Errorf("the refusal does not name the flag: %v", err)
			}
		})
	}
}

// The gate and the restore-point runner must answer the same question the
// same way. They are the two places that decide "is this URI this machine",
// and a disagreement means one verb acts locally while another refuses to --
// on the same URI, in the same incident.
func TestTheLocalityGateAndTheRestorePointRunnerAgree(t *testing.T) {
	for _, uri := range []string{
		"qemu:///system", "qemu:///session", "qemu+unix:///system", "",
		"qemu+tcp://127.0.0.1/system", "qemu+tcp://localhost:16509/system",
		"qemu+ssh://hyper01p/system", "qemu+ssh://localhost/system",
		"qemu+libssh://hyper01p/system", "qemu+libssh2://hyper01p/system",
		"qemu+tcp://hyper01p/system", "qemu+tls://hyper01p/system",
		"qemu+tls://hyper01p:16514/system", "qemu://hyper01p/system",
		"qemu+ext://hyper01p/system",
	} {
		gateSaysLocal := requireLocalURI(uri, "-target-uri") == nil
		runnerSaysLocal := uriRunsCommandsLocally(uri)
		if gateSaysLocal != runnerSaysLocal {
			t.Errorf("for %q the DR gate says local=%v but the restore-point runner says local=%v; one of them is acting on the wrong machine",
				uri, gateSaysLocal, runnerSaysLocal)
		}
	}
}
