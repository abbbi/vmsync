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
	"errors"
	"strings"
	"testing"
)

// unprovenAbsenceReport exists because an EMPTY bitmap read means two
// different things depending on whether qemu still holds the image, and only
// one of them is "there is no orphan".
//
// Every assertion here is tied to a mutation that otherwise survives, because
// the function is four lines of decision wrapped around a message and the
// decision is the part that can be silently inverted.

// A CLOSED IMAGE HAS ALREADY WRITTEN ITS DIRECTORY OUT, so for a shut-down
// source the empty answer is the whole answer and there is nothing to warn
// about.
//
// This is the assertion that keeps the warning off the one state where it
// would be false. Without it, `return someMessage` unconditionally passes
// every other test in this file: they all look for content, and a function
// that always produces content satisfies them all.
func TestAClosedImageMakesTheEmptyAnswerProof(t *testing.T) {
	if msg := unprovenAbsenceReport(testDomain, false, nil, false); msg != "" {
		t.Fatalf("a shut-down source was warned that its empty bitmap read proves nothing, but a closed image has flushed its bitmap directory into the file -- that read saw everything there was:\n\n%s", msg)
	}
}

// A RUNNING SOURCE CANNOT PROVE ABSENCE, and the message has to say so and say
// what settles it.
//
// The remedies are the point of the whole report. Closing the image is what
// moves a bitmap from qemu's memory into the qcow2 directory this check reads,
// and there are exactly two ways to close it that do not throw the guest away:
// a save followed by a restore, or a reboot. A message that states the limit
// without naming either leaves the operator with a doubt and no way to resolve
// it, which is worse than saying nothing.
func TestARunningSourceCannotProveThereIsNoOrphan(t *testing.T) {
	msg := unprovenAbsenceReport(testDomain, true, nil, false)
	if msg == "" {
		t.Fatal("a running source was told nothing. Its empty bitmap read cannot see a bitmap created during this qemu's life, which is precisely the orphan a failed run leaves behind")
	}

	for _, want := range []string{
		// The remedies, which are the operator's only way out of the doubt.
		"virsh save",
		"virsh restore",
		"reboot",
		// Why the doubt exists at all: a reader who is not told the read was
		// of the FILE has no reason to believe the remedy would change it.
		"ON DISK",
		// And that this is a caveat, not a fault: a generation token means a
		// missed orphan cannot collide with the names the new chain mints, so
		// an operator must not read this as a reason to stop.
		"not blocked",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message never mentions %q, so it states a doubt it does not let the operator resolve:\n\n%s", want, msg)
		}
	}

	if !strings.Contains(msg, testDomain) {
		t.Errorf("the message does not name the domain %q. A warning about one pair on a hypervisor running several is unactionable without it:\n\n%s", testDomain, msg)
	}
	// Twice at least: once in the prose and once in the save/restore command,
	// which is useless as a copy-paste if it carries a placeholder instead.
	if n := strings.Count(msg, testDomain); n < 2 {
		t.Errorf("the domain %q appears %d time(s); the save/restore command has to carry the real name rather than a placeholder:\n\n%s", testDomain, n, msg)
	}
}

// AN UNREADABLE STATE IS NOT A CLOSED IMAGE.
//
// DomainActive returns (false, err) when it cannot tell, so the zero value of
// the bool it returns on failure is the same value a shut-down domain
// produces. Dropping the `stateErr == nil` conjunct from the guard therefore
// compiles, reads naturally -- `if !sourceActive { return "" }` -- and
// silences the report in exactly the case where least is known.
//
// That is the defect class this codebase keeps meeting: a measurement that
// could not be taken reading as a measurement that came back clean.
func TestAnUnreadableStateIsNotTreatedAsAClosedImage(t *testing.T) {
	stateErr := errors.New("libvirt: domain lookup failed")
	msg := unprovenAbsenceReport(testDomain, false, stateErr, false)
	if msg == "" {
		t.Fatal("the source's running state could not be read and the report said nothing, which is the same thing it says for a source it KNOWS is shut down. Not knowing whether the read was conclusive is not the same as knowing it was")
	}
	if !strings.Contains(msg, stateErr.Error()) {
		t.Errorf("the message does not carry the underlying error %q, so nobody can tell why the state was unreadable:\n\n%s", stateErr, msg)
	}
	// The remedies still apply: whatever the state turns out to be, closing
	// the image is what makes the next read conclusive.
	for _, want := range []string{"virsh save", "virsh restore", "reboot"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the unreadable-state message never mentions %q, though the remedy is the same one as for a running source:\n\n%s", want, msg)
		}
	}
}

// THE TWO LEADS MUST NOT BE INTERCHANGEABLE.
//
// Reusing the running-source lead for the unreadable case is one deleted
// branch and changes nothing a content check would notice: both messages still
// carry the remedies, the domain and the explanation. What is lost is the only
// thing distinguishing "I looked and the source is running" from "I could not
// look", and an operator chasing the second one needs to know it was the state
// read that failed and not the bitmap read.
func TestTheRunningAndUnreadableLeadsAreDistinguishable(t *testing.T) {
	running := unprovenAbsenceReport(testDomain, true, nil, false)
	unknown := unprovenAbsenceReport(testDomain, false, errors.New("boom"), false)

	if running == unknown {
		t.Fatal("a running source and a source whose state could not be read produced the identical message; the difference between a conclusion and a failure to reach one is not reported")
	}
	if strings.Contains(running, "could not tell") {
		t.Errorf("the running-source message claims the state could not be read, when it was read and said running:\n\n%s", running)
	}
	if !strings.Contains(unknown, "could not tell") {
		t.Errorf("the unreadable-state message does not say the state could not be read:\n\n%s", unknown)
	}
	// A state that was read and came back running must not be reported as an
	// error, so the message carries no error text to misread as one.
	if strings.Contains(running, "boom") {
		t.Errorf("the running-source message carried an error it was not given:\n\n%s", running)
	}
}

// STABLE ACROSS RUNS, like every other message here: two operators comparing
// notes on the same fault have to be looking at the same text, and a map
// iteration or a clock reading sneaking in is what breaks that.
func TestTheUnprovenAbsenceMessageIsStable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		active bool
		err    error
	}{
		{"running", true, nil},
		{"unreadable", false, errors.New("same error every time")},
	} {
		first := unprovenAbsenceReport(testDomain, tc.active, tc.err, false)
		for i := 0; i < 20; i++ {
			if got := unprovenAbsenceReport(testDomain, tc.active, tc.err, false); got != first {
				t.Fatalf("%s: the message changed between two identical calls:\n\nfirst:\n%s\n\nlater:\n%s", tc.name, first, got)
			}
		}
	}
}
