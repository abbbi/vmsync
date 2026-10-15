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

import "testing"

// TestChooseHelperModeRoutesEveryInvocation is the whole routing table, and
// it is worth a test because supervising is now what happens when nothing
// matches: a mistake in the precedence no longer produces a usage error, it
// produces a process reading stdin for a command nobody is going to send.
func TestChooseHelperModeRoutesEveryInvocation(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  helperModeRequest
		want helperMode
	}{
		{
			name: "nothing but a command on a pipe is a supervision",
			req:  helperModeRequest{},
			want: modeSupervise,
		},
		{
			name: "the same invocation on a terminal is a usage error",
			req:  helperModeRequest{StdinIsTerminal: true},
			want: modeUsage,
		},
		{
			// A supervision arrives over SSH, where stdin is always a pipe,
			// so the terminal check can never swallow a real one.
			name: "supervision flags on a pipe are still a supervision",
			req:  helperModeRequest{},
			want: modeSupervise,
		},
		{
			name: "a relay flag selects the relay",
			req:  helperModeRequest{Relay: true},
			want: modeRelay,
		},
		{
			// Half a relay must reach the relay, which names the missing
			// flag. Falling through to a supervision would read the
			// operator's terminal instead of saying -connect is required.
			name: "a half-specified relay on a terminal is still the relay",
			req:  helperModeRequest{Relay: true, StdinIsTerminal: true},
			want: modeRelay,
		},
		{
			name: "checksum outranks the relay",
			req:  helperModeRequest{Checksum: true, Relay: true},
			want: modeChecksum,
		},
		{
			name: "the lock outranks everything but the version",
			req:  helperModeRequest{HoldRunLock: true, Checksum: true, Relay: true},
			want: modeLock,
		},
		{
			name: "the version is answered whatever else was asked",
			req:  helperModeRequest{Version: true, HoldRunLock: true, Checksum: true, Relay: true},
			want: modeVersion,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := chooseHelperMode(tc.req); got != tc.want {
				t.Errorf("chooseHelperMode(%+v) = %q, want %q", tc.req, got, tc.want)
			}
		})
	}
}

// TestSupervisionIsTheDefaultMode states the design decision as an assertion
// rather than leaving it implied by the table above.
//
// MUTATION CAUGHT: a default branch that returns modeRelay or modeUsage,
// which would make every target command vmsync issues either relay nonsense
// or a usage error -- and vmsync passes no mode flag at all, so nothing on
// its side would notice until a run failed.
func TestSupervisionIsTheDefaultMode(t *testing.T) {
	if got := chooseHelperMode(helperModeRequest{}); got != modeSupervise {
		t.Errorf("an invocation asking for nothing in particular chose %q; vmsync names no mode when it runs a target command, so this must be %q", got, modeSupervise)
	}
}

// TestModeOnlyFlagListsDoNotOverlap keeps the two refusal sets disjoint. A
// flag in both lists would make a relay invocation refuse itself for
// carrying its own flag, which is the kind of mistake that only shows up on
// a host nobody is watching.
func TestModeOnlyFlagListsDoNotOverlap(t *testing.T) {
	seen := map[string]string{}
	for _, n := range relayOnlyFlags {
		seen[n] = "relay"
	}
	for _, n := range superviseOnlyFlags {
		if owner, dup := seen[n]; dup {
			t.Errorf("flag %q is listed as belonging only to %s and only to supervision", n, owner)
		}
	}
}
