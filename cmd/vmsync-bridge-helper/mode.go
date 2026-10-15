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
	"flag"
	"os"
)

// Flags that belong to one mode only. Grouped here beside the chooser
// because "which flags mean which mode" and "which mode is this" are one
// question, and answering it in two places is how a flag ends up silently
// ignored by a mode that did not expect it.
var (
	relayOnlyFlags     = []string{"listen", "connect", "compress", "compress-level", "netbuffer"}
	superviseOnlyFlags = []string{"max-runtime", "kill-grace", "label"}
)

// flagsGiven reports whether any of names appeared on the command line.
//
// flag.Visit rather than comparing values against defaults: -kill-grace has
// a non-zero default, so its value cannot say whether a caller set it, and a
// mode that refuses another's flags has to know the difference between
// "passed" and "left alone".
func flagsGiven(names ...string) bool {
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	given := false
	flag.Visit(func(f *flag.Flag) {
		if want[f.Name] {
			given = true
		}
	})
	return given
}

// relayFlagsGiven reports whether this invocation named anything only the
// relay uses. A half-specified relay must reach the relay, which says which
// flag is missing, rather than falling through to the default mode.
func relayFlagsGiven() bool { return flagsGiven(relayOnlyFlags...) }

// supervisionFlagsGiven reports whether this invocation named anything only
// a supervised command uses, so the other modes can refuse them the way they
// refuse each other's mode flags.
func supervisionFlagsGiven() bool { return flagsGiven(superviseOnlyFlags...) }

// stdinIsTerminal reports whether stdin is a tty, which separates a command
// arriving on a pipe from a person who typed this binary's name.
//
// Failing closed on an error: a stat this process cannot do is not evidence
// that somebody is sitting there, and treating it as such would refuse a
// real supervision.
func stdinIsTerminal() bool {
	st, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

// helperMode is which of this binary's jobs one invocation is asking for.
type helperMode string

const (
	modeVersion   helperMode = "version"
	modeLock      helperMode = "hold-run-lock"
	modeChecksum  helperMode = "checksum"
	modeRelay     helperMode = "relay"
	modeSupervise helperMode = "supervise"
	modeUsage     helperMode = "usage"
)

// helperModeRequest is what the flags say, reduced to the facts the choice
// turns on. A struct rather than the flag pointers themselves so the decision
// is decidable in a table test, with no flag set to parse and no process to
// start.
type helperModeRequest struct {
	Version     bool
	HoldRunLock bool
	Checksum    bool
	// Relay is any flag that only the relay uses: -listen, -connect,
	// -compress, -compress-level, -netbuffer. Grouped because the relay's own
	// required-flag checks already say which one is missing, and because a
	// half-specified relay must reach the relay to be told so rather than
	// falling through to another mode.
	Relay bool
	// StdinIsTerminal distinguishes a command arriving on a pipe from a
	// person who typed the binary's name and nothing else.
	StdinIsTerminal bool
}

// chooseHelperMode decides what this invocation does.
//
// SUPERVISING IS THE DEFAULT, and that is the point of this function. Running
// one command under a runtime limit is what vmsync asks this binary for on every
// sync, several times per disk, and a flag whose value is always the same
// carries no information -- it is a word every caller has to pass and no
// reader learns anything from. The relay, the digest pass and the run lock
// are the special cases, and each already names itself.
//
// The precedence is the dispatch order in main(), and the two must stay the
// same: lock, digest, relay, then supervise. Each of the first three refuses
// the others' flags rather than quietly doing one of the other things.
//
// A bare invocation on a TERMINAL is a usage error, never a supervision. A
// person who typed the name of this binary to see what it does would
// otherwise get a process that sits there reading their keyboard, which is
// indistinguishable from a hang -- the single least helpful response a tool
// can give. Over SSH, where every real supervision arrives, stdin is a pipe
// and this never fires.
func chooseHelperMode(r helperModeRequest) helperMode {
	switch {
	case r.Version:
		return modeVersion
	case r.HoldRunLock:
		return modeLock
	case r.Checksum:
		return modeChecksum
	case r.Relay:
		return modeRelay
	case r.StdinIsTerminal:
		return modeUsage
	default:
		return modeSupervise
	}
}
