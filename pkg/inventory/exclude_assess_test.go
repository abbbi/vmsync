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

package inventory

import (
	"strings"
	"testing"
	"time"

	"vmsync/pkg/libvirtsync"
)

// THE WHOLE POINT OF THE FIELD, and the thing it got wrong when first
// written: an excluded domain stops being synced, so its last_sync age
// grows for ever. Without this short-circuit it reports `warning` the
// moment it passes its cadence and `critical` soon after -- paging
// somebody about a domain they deliberately set aside, which is the
// outcome the field exists to prevent.
func TestExcludedDomainIsNotReportedStale(t *testing.T) {
	now := time.Unix(1800000000, 0)
	// Synced a week ago against an hourly cadence: unambiguously stale by
	// every rule in assessReplication.
	// ReplicaSource, not just Role: IsTarget() keys on it, and without it
	// assessReplication returns early as "a source, reported from the
	// other side" and never reaches a freshness check at all.
	d := Domain{
		Name:           "web01",
		Role:           libvirtsync.RoleTarget,
		ReplicaSource:  "hyper01p:web01",
		LastCheckpoint: "vmsync-cpt-000042",
		LastSyncUnix:   now.Add(-7 * 24 * time.Hour).Unix(),
		Exclude:        libvirtsync.ExcludeReportOnly,
	}

	got := Assess(d, now, time.Hour)
	if got.Status != StatusExcluded {
		t.Errorf("Status = %s, want %s: a week past an hourly cadence, an excluded domain must still read as a decision rather than as a fault",
			got.Status, StatusExcluded)
	}
	if joined := strings.Join(got.Reasons, "; "); !strings.Contains(joined, "exclude") {
		t.Errorf("the reasons do not mention the exclusion: %q -- an operator seeing a non-OK status needs to know it is theirs", joined)
	}

	// The same domain WITHOUT the field is the control: if this does not
	// come back alarming, the test above proves nothing.
	d.Exclude = ""
	if plain := Assess(d, now, time.Hour); plain.Status == StatusExcluded || plain.Status < StatusWarning {
		t.Errorf("the unexcluded control came back %s; it must be at least %s or the test above is not measuring the exclusion",
			plain.Status, StatusWarning)
	}
}

// A domain nobody ever replicated, excluded anyway, reads as excluded
// rather than unreplicated. Both mean "nothing happening here"; only one
// says a person decided it.
func TestExcludedOutranksUnreplicated(t *testing.T) {
	now := time.Unix(1800000000, 0)
	d := Domain{Name: "someone-elses-vm", Exclude: libvirtsync.ExcludeReportOnly}

	got := Assess(d, now, 0)
	if got.Status != StatusExcluded {
		t.Errorf("Status = %s, want %s", got.Status, StatusExcluded)
	}

	// Control: the same domain with no exclusion is unreplicated.
	if plain := Assess(Domain{Name: "someone-elses-vm"}, now, 0); plain.Status != StatusUnreplicated {
		t.Errorf("the control came back %s, want %s", plain.Status, StatusUnreplicated)
	}
}

// An unrecognised value still excludes, and the assessment is the one
// place an operator will see that their value was not understood -- the
// engine's refusal only appears if something tries to sync it, which by
// then it will not.
func TestExcludedUnrecognisedValueSaysSo(t *testing.T) {
	now := time.Unix(1800000000, 0)
	d := Domain{Name: "web01", Role: libvirtsync.RoleTarget, ReplicaSource: "hyper01p:web01", Exclude: "ful"}

	got := Assess(d, now, time.Hour)
	if got.Status != StatusExcluded {
		t.Errorf("Status = %s, want %s: a value nobody recognises must still exclude", got.Status, StatusExcluded)
	}
	joined := strings.Join(got.Reasons, "; ")
	for _, want := range []string{"ful", "not a recognised value", libvirtsync.ExcludeFullyIgnored} {
		if !strings.Contains(joined, want) {
			t.Errorf("the reasons do not mention %q: %q", want, joined)
		}
	}
}

// EVIDENCE OF CORRUPTION OUTRANKS ADMINISTRATIVE CALM, which is the rule
// the verify overlay already states for paused, fenced and promoted. An
// excluded replica that is known to differ from its source is still a copy
// known WRONG, and an operator deciding whether it is safe to keep must be
// told -- "I excluded that one" is a reason not to sync it, not a reason
// not to hear about its contents.
func TestVerifyFailureOutranksAnExclusion(t *testing.T) {
	now := time.Unix(1800000000, 0)
	d := Domain{
		Name:               "web01",
		Role:               libvirtsync.RoleTarget,
		LastCheckpoint:     "vmsync-cpt-000042",
		LastSyncUnix:       now.Add(-time.Minute).Unix(),
		Exclude:            libvirtsync.ExcludeReportOnly,
		VerifyState:        libvirtsync.VerifyStateFailed,
		VerifyFailedAtUnix: now.Add(-time.Hour).Unix(),
	}

	got := Assess(d, now, time.Hour)
	if got.Status != StatusCritical {
		t.Errorf("Status = %s, want %s: an exclusion must not bury the finding that this copy is known wrong", got.Status, StatusCritical)
	}
	if len(got.Reasons) == 0 || !strings.Contains(got.Reasons[0], "known WRONG") {
		t.Errorf("the verification failure is not the FIRST reason: %q -- a UI showing one line would show the exclusion instead", got.Reasons)
	}
}

// Ranked above OK so it stays visible, below the states that need a
// decision. Asserted because the order is what Worse() compares, and a
// renumbering that put excluded above warning would hide real faults on
// any host that folds many domains into one verdict.
func TestExcludedRanksWithTheAdministrativeStates(t *testing.T) {
	if !(StatusOK < StatusExcluded) {
		t.Error("excluded must rank above ok, or a domain nothing will ever happen to blends into the healthy list")
	}
	if !(StatusExcluded < StatusWarning) {
		t.Error("excluded must rank below warning: it is a decision, not a degradation")
	}
	if Worse(StatusExcluded, StatusCritical) != StatusCritical {
		t.Error("critical must win over excluded")
	}
	if Worse(StatusExcluded, StatusOK) != StatusExcluded {
		t.Error("excluded must win over ok")
	}
	if got := StatusExcluded.String(); got != "excluded" {
		t.Errorf("StatusExcluded.String() = %q, want \"excluded\" -- it is a metrics label and a console value", got)
	}
}

// A SPLIT BRAIN OUTRANKS THE EXCLUSION, the same way a verification
// failure does. Fenced-and-running means this domain is live beside the
// peer that displaced it and both are taking writes -- the one finding
// where every other gauge reads clear, so an exclusion that buried it
// would leave nothing at all saying so.
//
// This was wrong when first written: the short-circuit sat above the
// detection and reported `excluded`.
func TestFencedAndRunningOutranksAnExclusion(t *testing.T) {
	now := time.Unix(1800000000, 0)
	d := Domain{
		Name: "web01", Active: true,
		Role:          libvirtsync.RoleFenced,
		ReplicaSource: "hyper01p:web01",
		LastSyncUnix:  now.Add(-time.Minute).Unix(),
		Exclude:       libvirtsync.ExcludeReportOnly,
	}

	got := Assess(d, now, time.Hour)
	if got.Status != StatusCritical {
		t.Errorf("Status = %s, want %s: an exclusion must not bury a live split brain", got.Status, StatusCritical)
	}
	joined := strings.Join(got.Reasons, " | ")
	if !strings.Contains(joined, "FENCED BUT STILL RUNNING") {
		t.Errorf("the split-brain finding is missing: %q", joined)
	}
	// ...and the exclusion is still said, or an operator reads the alarm
	// and waits for an automatic fence that is never coming.
	if !strings.Contains(joined, "exclude") {
		t.Errorf("the reasons do not mention the exclusion: %q -- nothing will act on this, and the line must say so", joined)
	}

	// A fenced domain that actually STOPPED is not dangerous, so the
	// exclusion still wins there.
	d.Active = false
	if stopped := Assess(d, now, time.Hour); stopped.Status != StatusExcluded {
		t.Errorf("a stopped fenced domain assessed %s, want %s: only the LIVE case outranks", stopped.Status, StatusExcluded)
	}
}
