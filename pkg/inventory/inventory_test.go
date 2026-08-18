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
	"encoding/json"
	"strings"
	"testing"
	"time"

	"vmsync/pkg/libvirtsync"
)

// now is a fixed reference point; every case below states its own age
// relative to it rather than depending on the wall clock.
var now = time.Unix(1_800_000_000, 0)

func target(ageSeconds int64) Domain {
	return Domain{
		Name:           "web01",
		ReplicaSource:  "hyper01p:web01",
		LastCheckpoint: "vmsync-cpt-000042",
		LastSyncUnix:   now.Unix() - ageSeconds,
	}
}

func TestAssessHealthyTarget(t *testing.T) {
	got := Assess(target(60), now, 15*time.Minute)
	if got.Status != StatusOK {
		t.Errorf("Assess(fresh target) = %v, want ok -- reasons: %v", got.Status, got.Reasons)
	}
	if got.AgeSeconds != 60 {
		t.Errorf("AgeSeconds = %d, want 60", got.AgeSeconds)
	}
	// Even a healthy verdict carries a reason, so a UI never has to render
	// an empty explanation cell.
	if len(got.Reasons) == 0 {
		t.Error("a healthy target produced no reason at all")
	}
}

func TestAssessStaleness(t *testing.T) {
	const cadence = 15 * time.Minute
	cases := []struct {
		name string
		age  time.Duration
		want Status
	}{
		{"well inside the cadence", 1 * time.Minute, StatusOK},
		{"exactly at the cadence is not yet late", cadence, StatusOK},
		{"just past the cadence", cadence + time.Second, StatusWarning},
		{"twice the cadence", 2 * cadence, StatusWarning},
		{"exactly 3x is still only a warning", 3 * cadence, StatusWarning},
		{"beyond 3x the cadence", 3*cadence + time.Second, StatusCritical},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Assess(target(int64(tc.age.Seconds())), now, cadence)
			if got.Status != tc.want {
				t.Errorf("Assess(age=%s, cadence=%s) = %v, want %v -- reasons: %v",
					tc.age, cadence, got.Status, tc.want, got.Reasons)
			}
		})
	}
}

func TestAssessWithoutCadenceMakesNoFreshnessClaim(t *testing.T) {
	// A cadence of 0 means "unknown", which must disable the staleness
	// checks rather than fall back to a guessed threshold. A pair that
	// legitimately syncs weekly would otherwise be reported critical
	// forever by an agent that simply has not been told its schedule.
	got := Assess(target(30*86400), now, 0)
	if got.Status != StatusOK {
		t.Errorf("Assess(30 days old, unknown cadence) = %v, want ok -- reasons: %v", got.Status, got.Reasons)
	}
	if got.AgeSeconds != 30*86400 {
		t.Errorf("AgeSeconds = %d, want the age still reported even when it is not judged", got.AgeSeconds)
	}
}

func TestAssessNeverSynced(t *testing.T) {
	d := target(0)
	d.LastSyncUnix = 0
	d.LastCheckpoint = ""
	got := Assess(d, now, 15*time.Minute)
	if got.Status != StatusCritical {
		t.Errorf("Assess(never synced) = %v, want critical", got.Status)
	}
	if got.AgeSeconds != -1 {
		t.Errorf("AgeSeconds = %d, want -1 -- there is no age to report, and 0 would read as \"just synced\"", got.AgeSeconds)
	}
}

func TestAssessFailureCount(t *testing.T) {
	d := target(60)
	d.FailureCount = 3
	got := Assess(d, now, 15*time.Minute)
	if got.Status != StatusWarning {
		t.Errorf("Assess(3 failures, otherwise fresh) = %v, want warning", got.Status)
	}
	if !strings.Contains(strings.Join(got.Reasons, " "), "3 consecutive") {
		t.Errorf("reasons %v do not report the failure count", got.Reasons)
	}
}

func TestAssessMissingCheckpointBlocksIncrementals(t *testing.T) {
	// A target with a sync timestamp but no checkpoint is the state
	// vmsync's own unverifiableCheckpointMetadataError refuses to trust:
	// every future run falls back to a full copy. Worth a warning even
	// though the last sync itself succeeded.
	d := target(60)
	d.LastCheckpoint = ""
	got := Assess(d, now, 15*time.Minute)
	if got.Status != StatusWarning {
		t.Errorf("Assess(no checkpoint) = %v, want warning", got.Status)
	}
	if !strings.Contains(strings.Join(got.Reasons, " "), "incrementally") {
		t.Errorf("reasons %v do not explain the consequence", got.Reasons)
	}
}

// TestAssessAdministrativeStatesSuppressStaleness pins the behaviour that
// keeps a failover from drowning an operator in noise: a promoted or paused
// domain is SUPPOSED to stop receiving syncs, so its growing last_sync age
// is expected, not a fault.
func TestAssessAdministrativeStatesSuppressStaleness(t *testing.T) {
	for _, tc := range []struct {
		role string
		want Status
	}{
		{libvirtsync.RolePromoted, StatusPromoted},
		{libvirtsync.RolePaused, StatusPaused},
	} {
		t.Run(tc.role, func(t *testing.T) {
			d := target(90 * 86400) // wildly stale
			d.Role = tc.role
			d.FailureCount = 7
			got := Assess(d, now, 15*time.Minute)
			if got.Status != tc.want {
				t.Errorf("Assess(role=%s, very stale) = %v, want %v", tc.role, got.Status, tc.want)
			}
			if len(got.Reasons) != 1 {
				t.Errorf("reasons = %v, want exactly the administrative explanation and nothing about staleness", got.Reasons)
			}
		})
	}
}

func TestAssessClockSkew(t *testing.T) {
	d := target(-3600) // last sync an hour in the future
	got := Assess(d, now, 15*time.Minute)
	if got.Status != StatusWarning {
		t.Errorf("Assess(future timestamp) = %v, want warning", got.Status)
	}
	if !strings.Contains(strings.Join(got.Reasons, " "), "clock skew") {
		t.Errorf("reasons %v do not name clock skew -- every freshness number is untrustworthy until it is fixed", got.Reasons)
	}
	if got.AgeSeconds < 0 {
		t.Errorf("AgeSeconds = %d, want a non-negative value rather than a nonsense negative age", got.AgeSeconds)
	}
}

func TestAssessUnreplicatedIsNotHealthy(t *testing.T) {
	// The distinction that matters most in an availability view: a vm
	// nobody configured replication for must not sit in a green list
	// looking protected.
	got := Assess(Domain{Name: "scratch01", Active: true}, now, 15*time.Minute)
	if got.Status != StatusUnreplicated {
		t.Errorf("Assess(no vmsync metadata at all) = %v, want unreplicated", got.Status)
	}
	if got.Status == StatusOK {
		t.Error("an unreplicated vm was reported as OK")
	}
}

func TestAssessSourceIsNotJudgedOnItsOwnTimestamp(t *testing.T) {
	// A source's own last_sync is not the pair's freshness -- that lives on
	// the target, written by the run that updated it. Judging a source on
	// its own would report every source in the estate as permanently stale.
	d := Domain{
		Name:           "web01",
		ReplicaTargets: []string{"hyper02p:web01"},
		LastSyncUnix:   0,
	}
	got := Assess(d, now, 15*time.Minute)
	if got.Status != StatusOK {
		t.Errorf("Assess(source with no timestamp of its own) = %v, want ok -- reasons: %v", got.Status, got.Reasons)
	}
	if !strings.Contains(strings.Join(got.Reasons, " "), "hyper02p:web01") {
		t.Errorf("reasons %v do not name where this source replicates to", got.Reasons)
	}
}

func TestDomainRoleHelpers(t *testing.T) {
	src := Domain{ReplicaTargets: []string{"h2:vm"}}
	tgt := Domain{ReplicaSource: "h1:vm"}
	none := Domain{Name: "scratch"}

	if !src.IsSource() || src.IsTarget() {
		t.Error("a domain with replica_targets should be a source and not a target")
	}
	if !tgt.IsTarget() || tgt.IsSource() {
		t.Error("a domain with replica_source should be a target and not a source")
	}
	if none.Participates() {
		t.Error("a domain with no vmsync metadata should not count as participating")
	}
	// A role alone is enough to participate: a promoted domain has had its
	// replica_source left behind, but a paused one set before any sync may
	// have nothing else.
	if !(Domain{Role: libvirtsync.RolePaused}).Participates() {
		t.Error("a domain carrying only a replication_role should still count as participating")
	}
}

func TestStatusSerializesAsAName(t *testing.T) {
	// The UI and the agent exchange these as JSON; an integer would make
	// the wire format depend on the iota ordering, so reordering the
	// constants would silently change every stored record's meaning.
	b, err := json.Marshal(struct {
		S Status `json:"s"`
	}{StatusCritical})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if string(b) != `{"s":"critical"}` {
		t.Errorf("marshalled as %s, want {\"s\":\"critical\"}", b)
	}
}

func TestWorseKeepsTheHigherPriority(t *testing.T) {
	if Worse(StatusOK, StatusCritical) != StatusCritical {
		t.Error("Worse(ok, critical) should be critical")
	}
	if Worse(StatusCritical, StatusWarning) != StatusCritical {
		t.Error("Worse(critical, warning) should be critical")
	}
	if Worse(StatusOK, StatusOK) != StatusOK {
		t.Error("Worse(ok, ok) should be ok")
	}
}
