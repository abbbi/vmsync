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

// WHETHER A CLEARING FAILURE REFUSES THE REBUILD is the one place asking qemu
// is allowed to change what a run DOES rather than what it says, and it is
// allowed to change it in one direction only: a rebuild that used to succeed
// must not start failing because vmsync can now see more.
//
// Everything here is a table over a pure function, because the decision is two
// lines inside a branch of run() that no test can reach.

const (
	seenByBoth   = "vmsync-cpt-2510010000-aaaa-000001" // the image read found it too
	newlyVisible = "vmsync-cpt-2510010000-bbbb-000002" // only qemu reported it
)

// FATAL IS THE DEFAULT, and no census at all must leave the old contract
// exactly as it was.
//
// This is the row that matters most: on a host where the monitor is refused,
// or a libvirt without the qemu driver, OnlyQemu is empty and every failure
// must refuse, which is what vmsync did before this route existed. Reached
// without a special case, which is why the empty map is the no-census value
// rather than a nil check somewhere.
func TestWithNoCensusEveryClearingFailureStillRefuses(t *testing.T) {
	for _, onlyQemu := range []map[string][]string{nil, {}} {
		if !clearFailureIsFatal([]string{newlyVisible}, onlyQemu) {
			t.Errorf("onlyQemu=%v: a clearing failure did not refuse the rebuild. With no census nothing is newly visible, so this is the pre-existing contract and it must not have moved", onlyQemu)
		}
	}
}

func TestOnlyANewlyVisibleFailureIsForgiven(t *testing.T) {
	onlyQemu := map[string][]string{testDiskPath: {newlyVisible}}

	for _, tc := range []struct {
		name   string
		failed []string
		fatal  bool
		why    string
	}{
		{
			name:   "a bitmap the image read also found",
			failed: []string{seenByBoth},
			fatal:  true,
			why:    "vmsync could already see it and has always refused when it could not clear one; asking qemu changes nothing about what that failure means",
		},
		{
			name:   "a bitmap only qemu reported",
			failed: []string{newlyVisible},
			fatal:  false,
			why:    "it was invisible on a running source until this version, the rebuild succeeded over it, and refusing now takes away the repair verb over a leak that was always there",
		},
		{
			name:   "one of each",
			failed: []string{newlyVisible, seenByBoth},
			fatal:  true,
			why:    "the file-visible one alone is enough to refuse; forgiving the pair because most of it was newly visible would silently relax the old contract",
		},
		{
			name:   "several, all newly visible",
			failed: []string{newlyVisible, "vmsync-cpt-2510010000-cccc-000003"},
			fatal:  true,
			why:    "the second name is not in onlyQemu, so it is treated as one vmsync could already see -- fatality is decided per bitmap, never by how many there are",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := clearFailureIsFatal(tc.failed, onlyQemu); got != tc.fatal {
				t.Errorf("clearFailureIsFatal(%v) = %v, want %v.\n\n%s", tc.failed, got, tc.fatal, tc.why)
			}
		})
	}
}

// AN UNKNOWN NAME IS TREATED AS ONE THE IMAGE SAW, which is the safe
// direction: it refuses. The alternative -- forgiving anything not positively
// identified -- would turn a census that came back partial into a blanket
// amnesty on refusals.
func TestABitmapOfUnknownProvenanceRefuses(t *testing.T) {
	onlyQemu := map[string][]string{testDiskPath: {newlyVisible}}
	if !clearFailureIsFatal([]string{"vmsync-cpt-2510010000-zzzz-000099"}, onlyQemu) {
		t.Error("a bitmap in neither set was forgiven. Not knowing where a bitmap came from is not evidence that it was newly visible, and the safe reading of an unknown is the one that keeps the old refusal")
	}
}

// THE PROVENANCE IS THE NAME, NOT THE DISK. A name newly visible on one disk
// is newly visible, which matters because vmsync checkpoints every disk it
// manages under one name, so the same name legitimately appears on several.
func TestProvenanceIsPerNameAcrossDisks(t *testing.T) {
	onlyQemu := map[string][]string{"/vm/disk1.qcow2": {newlyVisible}}
	if clearFailureIsFatal([]string{newlyVisible}, onlyQemu) {
		t.Error("a name reported as newly visible on one disk was treated as file-visible when it failed on another. vmsync gives every disk of a domain the same checkpoint name, so a per-disk reading would make the decision depend on which disk happened to fail first")
	}
}

// THE REPORT HAS TO EXPLAIN THE CONTRADICTION. An operator reading "could not
// clear" next to a successful exit will otherwise assume one of them is wrong.
func TestTheNewlyVisibleReportSaysWhyItDidNotRefuse(t *testing.T) {
	clearErr := errors.New("adopt orphaned bitmap: checkpoint inconsistent")
	report := newlyVisibleClearFailureReport(testDomain, []string{newlyVisible}, clearErr)
	if report == "" {
		t.Fatal("carried on past a clearing failure and said nothing. The run exits 0 with an orphan still in place, which is the silence this whole feature exists to remove")
	}
	for _, want := range []string{
		newlyVisible,              // which bitmap
		testDomain,                // on which domain
		"checkpoint inconsistent", // the underlying failure, not paraphrased
		"did NOT refuse",          // that the choice was deliberate
		"rolled back",             // that nothing is half-done
		"space in the qcow2",      // what it actually costs
	} {
		if !strings.Contains(report, want) {
			t.Errorf("the report never mentions %q:\n\n%s", want, report)
		}
	}
	if strings.Contains(report, "refusing to rebuild") {
		t.Errorf("the report uses the refusal's own wording for a run that did not refuse:\n\n%s", report)
	}
}

func TestTheNewlyVisibleReportIsSilentWithNothingToSay(t *testing.T) {
	if r := newlyVisibleClearFailureReport(testDomain, nil, errors.New("x")); r != "" {
		t.Errorf("spoke with no failed bitmaps:\n\n%s", r)
	}
	if r := newlyVisibleClearFailureReport(testDomain, []string{newlyVisible}, nil); r != "" {
		t.Errorf("spoke with no error:\n\n%s", r)
	}
}

// THE CLEARING MUST SAY WHICH BITMAPS FAILED, or the decision above has
// nothing to decide on. Both sources of failure count: a delete libvirt
// refused, and a bitmap with no target device that was never attempted.
func TestClearingReportsWhichBitmapsItCouldNotClear(t *testing.T) {
	// /vm/a.qcow2 is one of testDisks()' own paths, so these bitmaps are
	// MAPPABLE and the clear closure is really reached. Using a path testDisks
	// does not claim would send them down the unmapped branch instead and the
	// refused-delete case would pass without a delete ever being attempted.
	const mappable = "/vm/a.qcow2"

	t.Run("a refused delete", func(t *testing.T) {
		attempted := false
		_, failed, err := clearOrphanBitmapsOnline(testDomain,
			map[string][]string{mappable: {testBitmap}}, testDisks(),
			func(bitmap string, devs []string) error {
				attempted = true
				return errors.New("libvirt said no")
			})
		if !attempted {
			t.Fatal("the clear closure was never called, so this is testing the unmapped branch rather than a refused delete")
		}
		if err == nil {
			t.Fatal("a refused delete reported success")
		}
		if len(failed) != 1 || failed[0] != testBitmap {
			t.Errorf("failed=%v, want exactly [%s]. Without the name the caller cannot tell a newly visible failure from one it has always refused over", failed, testBitmap)
		}
	})
	t.Run("a bitmap with no target device", func(t *testing.T) {
		_, failed, err := clearOrphanBitmapsOnline(testDomain,
			map[string][]string{"/vm/stranger.qcow2": {"vmsync-cpt-000009"}}, testDisks(),
			func(bitmap string, devs []string) error {
				t.Errorf("attempted a bitmap with no target device: %s", bitmap)
				return nil
			})
		if err == nil {
			t.Fatal("an unaddressable bitmap reported success")
		}
		if len(failed) != 1 || failed[0] != "vmsync-cpt-000009" {
			t.Errorf("failed=%v, want [vmsync-cpt-000009]. A bitmap that was never attempted is still one that was not cleared", failed)
		}
	})
	t.Run("nothing failed", func(t *testing.T) {
		cleared, failed, err := clearOrphanBitmapsOnline(testDomain,
			map[string][]string{mappable: {testBitmap}}, testDisks(),
			func(bitmap string, devs []string) error { return nil })
		if err != nil || cleared != 1 {
			t.Fatalf("cleared=%d err=%v, want 1 and nil", cleared, err)
		}
		if len(failed) != 0 {
			t.Errorf("failed=%v on a clean sweep. A non-empty list here would make the caller weigh a decision it does not have to make", failed)
		}
	})
}

// QEMU'S COMPLETE ANSWER SETTLES AN ABSENCE THE FILE NEVER COULD.
//
// Without this the warning fires on precisely the runs where the absence has
// just been proven, telling an operator to save/restore a production guest to
// settle a question qemu answered a moment earlier -- which is how a warning
// stops being read.
func TestQemusCompleteAnswerSettlesTheAbsence(t *testing.T) {
	if msg := unprovenAbsenceReport(testDomain, true, nil, true); msg != "" {
		t.Errorf("still said the absence was unproven after qemu answered completely:\n\n%s", msg)
	}
	// And the other way: a census that did not answer leaves the doubt intact.
	if msg := unprovenAbsenceReport(testDomain, true, nil, false); msg == "" {
		t.Error("said nothing on a running source whose census did not answer; the image read alone cannot see a bitmap made during this qemu's life")
	}
	// An unreadable state is not settled by a census that never ran either.
	if msg := unprovenAbsenceReport(testDomain, false, errors.New("boom"), false); msg == "" {
		t.Error("said nothing when the source's state could not be read")
	}
}
