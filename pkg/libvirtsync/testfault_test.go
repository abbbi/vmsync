/*
	Copyright (C) 2026  Orsiris de Jong <ozy@netpower.fr>
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

package libvirtsync

import (
	"strings"
	"testing"
)

// TestValidateTestFaultAcceptsEveryListedFault is the guard for the mistake
// that is easy to make here: declaring a TestFault constant and forgetting to
// add it to TestFaults. The flag would then refuse the value the code is
// written to handle, and the branch would be unreachable -- a fault nobody can
// inject, which looks exactly like a fault that does not work.
func TestValidateTestFaultAcceptsEveryListedFault(t *testing.T) {
	for _, f := range TestFaults {
		if f == "" {
			t.Error("TestFaults contains an empty name, which would make -test= with no value valid")
			continue
		}
		if err := ValidateTestFault(f); err != nil {
			t.Errorf("ValidateTestFault(%q) rejected a fault that is in TestFaults: %v", f, err)
		}
	}
}

// TestEveryTestFaultConstantIsListed pins each constant against the list by
// name. Spelled out rather than derived, because there is nothing to derive
// from: a constant that is not in TestFaults is invisible to reflection over
// the package, so only an explicit roster catches the omission.
func TestEveryTestFaultConstantIsListed(t *testing.T) {
	for _, f := range []string{
		TestFaultFailureDefine,
		TestFaultCorruptBeforeChecksum,
		TestFaultCorruptAfterCommit,
		TestFaultFailLastDisk,
		TestFaultDieWritingBase,
		TestFaultStallAfterLock,
	} {
		if err := ValidateTestFault(f); err != nil {
			t.Errorf("fault %q is declared as a constant but not accepted by ValidateTestFault -- add it to TestFaults: %v", f, err)
		}
	}
}

// TestValidateTestFaultAcceptsNoInjection: "" is every real run, so rejecting
// it would refuse every sync anybody actually wants.
func TestValidateTestFaultAcceptsNoInjection(t *testing.T) {
	if err := ValidateTestFault(""); err != nil {
		t.Errorf(`ValidateTestFault("") must mean "inject nothing" and be valid: %v`, err)
	}
}

// TestValidateTestFaultRefusesUnknownAndSaysWhatIsValid. The message matters
// more than the refusal: -test is a developer-facing flag whose values are not
// guessable, so an error that does not list them sends the reader to the
// source.
func TestValidateTestFaultRefusesUnknownAndSaysWhatIsValid(t *testing.T) {
	for _, name := range []string{
		"stall",                 // a prefix of a real one
		"stall-after-lock ",     // trailing space, as a shell might leave
		"STALL-AFTER-LOCK",      // wrong case
		"die_writing_base",      // underscores instead of dashes
		"corrupt-before-commit", // plausible blend of two real names
	} {
		err := ValidateTestFault(name)
		if err == nil {
			t.Errorf("ValidateTestFault(%q) accepted a name that is not a fault", name)
			continue
		}
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the error for %q does not quote what was passed: %v", name, err)
		}
		for _, f := range TestFaults {
			if !strings.Contains(err.Error(), f) {
				t.Errorf("the error for %q does not list the accepted fault %q: %v", name, f, err)
			}
		}
	}
}

// TestTestFaultsHasNoDuplicates keeps the error message readable -- it joins
// the list verbatim -- and keeps a copy-pasted constant from looking accepted
// twice.
func TestTestFaultsHasNoDuplicates(t *testing.T) {
	seen := make(map[string]bool, len(TestFaults))
	for _, f := range TestFaults {
		if seen[f] {
			t.Errorf("TestFaults lists %q more than once", f)
		}
		seen[f] = true
	}
}

// TestStallAfterLockIsDistinctFromTheFailureFaults records the one property of
// the stall fault that a reader needs and the type system cannot carry: it is
// the only fault that does not make the run fail. Every other value here
// injects an error and the run ends; this one holds the target lock and waits
// to be signalled, so anything that treats "-test is set" as "this run will
// exit non-zero" is wrong for exactly this value.
func TestStallAfterLockIsDistinctFromTheFailureFaults(t *testing.T) {
	for _, f := range []string{
		TestFaultFailureDefine,
		TestFaultCorruptBeforeChecksum,
		TestFaultCorruptAfterCommit,
		TestFaultFailLastDisk,
		TestFaultDieWritingBase,
	} {
		if f == TestFaultStallAfterLock {
			t.Errorf("%q collides with the stall fault's name; the stall fault must stay distinguishable from the ones that fail the run", f)
		}
	}
}
