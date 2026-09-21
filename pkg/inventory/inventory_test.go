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
	"reflect"
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

// TestAssessVerificationFailureIsCritical covers the finding that says the
// replica is WRONG rather than merely old. A fresh, healthy-looking target
// that failed its last verification must not sit in a green list: the whole
// reason the verdict is persisted on the domain is so it outlives the run
// that made it and is still visible on the day somebody promotes.
func TestAssessVerificationFailureIsCritical(t *testing.T) {
	d := target(60) // otherwise perfectly healthy: fresh, checkpointed, no failures
	d.VerifyState = libvirtsync.VerifyStateFailed
	d.VerifyFailedAtUnix = now.Unix() - 3600

	got := Assess(d, now, 15*time.Minute)
	if got.Status != StatusCritical {
		t.Errorf("Assess(fresh target carrying a verify failure) = %v, want critical -- reasons: %v", got.Status, got.Reasons)
	}
	joined := strings.Join(got.Reasons, " ")
	if !strings.Contains(joined, "differ from its source") {
		t.Errorf("reasons %v do not say the contents were found to differ from the source", got.Reasons)
	}
	// The date is what separates "failed an hour ago" from "failed last
	// quarter and nobody noticed", so it has to survive into the reason.
	if !strings.Contains(joined, time.Unix(d.VerifyFailedAtUnix, 0).UTC().Format(time.RFC3339)) {
		t.Errorf("reasons %v do not say when the verification failed", got.Reasons)
	}
	// Leading, because a UI with room for one line must show this one.
	if !strings.Contains(got.Reasons[0], "differ from its source") {
		t.Errorf("the verification finding is not the first reason: %v", got.Reasons)
	}
}

// TestAssessVerificationFailureSurvivesAdministrativeRoles is the assertion
// that made this worth restructuring Assess for. Every administrative role
// returns early from the replication verdict, so a finding checked inside
// that chain would be invisible on exactly the domains where it is most
// dangerous -- a promoted one is a live service running on data known not to
// match what it replaced, and "promoted" alone reads as nothing to do.
func TestAssessVerificationFailureSurvivesAdministrativeRoles(t *testing.T) {
	for _, role := range []string{
		libvirtsync.RolePaused,
		libvirtsync.RoleFenced,
		libvirtsync.RolePromoted,
	} {
		t.Run(role, func(t *testing.T) {
			d := target(90 * 86400)
			d.Role = role
			d.VerifyState = libvirtsync.VerifyStateFailed
			d.VerifyFailedAtUnix = now.Unix() - 86400

			got := Assess(d, now, 15*time.Minute)
			if got.Status != StatusCritical {
				t.Errorf("Assess(role=%s with a verify failure) = %v, want critical -- known wrong outranks the administrative states", role, got.Status)
			}
			if !strings.Contains(strings.Join(got.Reasons, " "), "differ from its source") {
				t.Errorf("role=%s swallowed the verification finding: %v", role, got.Reasons)
			}
			// The administrative explanation is still there: the operator
			// needs both facts, not one replaced by the other.
			if len(got.Reasons) < 2 {
				t.Errorf("role=%s reasons = %v, want the verification finding AND the administrative explanation", role, got.Reasons)
			}
		})
	}
}

// TestAssessVerificationFailureOutranksUnreplicated covers the fail-closed
// edge: a domain whose only vmsync metadata left is a verification failure.
// Reporting it as "unreplicated" would file a known-bad copy under "nobody
// configured this", which is the one bucket an operator never looks in.
func TestAssessVerificationFailureOutranksUnreplicated(t *testing.T) {
	got := Assess(Domain{Name: "web01", VerifyState: libvirtsync.VerifyStateFailed}, now, 15*time.Minute)
	if got.Status != StatusCritical {
		t.Errorf("Assess(verify failure and nothing else) = %v, want critical", got.Status)
	}
}

func TestAssessVerificationStateIsPresenceNotValue(t *testing.T) {
	// Presence is the state (libvirtsync.MetadataFieldVerifyState), so a
	// value this build does not recognise must still be bad news. The
	// alternative -- matching only "failed" -- means a future value reads as
	// healthy on an older agent, which is silent data loss dressed as green.
	d := target(60)
	d.VerifyState = "mismatch-on-vdb"
	if got := Assess(d, now, 15*time.Minute); got.Status != StatusCritical {
		t.Errorf("Assess(unrecognised verify_state) = %v, want critical -- any value is a recorded failure", got.Status)
	}
}

func TestAssessWithoutVerificationFailureIsUnchanged(t *testing.T) {
	// The no-false-positive half: the overwhelmingly common case is a
	// replica that has never failed a verification, and it must be assessed
	// exactly as it was before this check existed. A verdict that cried wolf
	// on every healthy target would be switched off within a week.
	got := Assess(target(60), now, 15*time.Minute)
	if got.Status != StatusOK {
		t.Errorf("Assess(healthy target, no verify record) = %v, want ok -- reasons: %v", got.Status, got.Reasons)
	}
	for _, r := range got.Reasons {
		if strings.Contains(r, "differ from its source") {
			t.Errorf("a target with no recorded verification failure was told it had one: %v", got.Reasons)
		}
	}
	// And an empty verify_failed_at on its own is not a finding either:
	// without a state there is nothing being recorded.
	d := target(60)
	d.VerifyFailedAtUnix = now.Unix() - 60
	if got := Assess(d, now, 15*time.Minute); got.Status != StatusOK {
		t.Errorf("Assess(timestamp but no verify_state) = %v, want ok -- presence of the STATE is the finding", got.Status)
	}
}

func TestAssessVerificationFailureWithNoUsableTimestamp(t *testing.T) {
	// A verify_failed_at that describe() could not parse -- or that was
	// never written -- leaves the timestamp at 0. The finding still has to
	// be reported: an undated verification failure is still a verification
	// failure, and dropping it because its date is unreadable would discard
	// the part that matters. Nor may it be rendered as the epoch, which
	// would read as a real date in 1970.
	d := target(60)
	d.VerifyState = libvirtsync.VerifyStateFailed
	d.VerifyFailedAtUnix = 0

	got := Assess(d, now, 15*time.Minute)
	if got.Status != StatusCritical {
		t.Errorf("Assess(verify failure with no timestamp) = %v, want critical", got.Status)
	}
	joined := strings.Join(got.Reasons, " ")
	if !strings.Contains(joined, "unrecorded time") {
		t.Errorf("reasons %v do not say the time is unrecorded", got.Reasons)
	}
	if strings.Contains(joined, "1970") {
		t.Errorf("a missing timestamp was rendered as the epoch: %v", got.Reasons)
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

// domainXMLWithMetadata builds a domain document carrying exactly the vmsync
// metadata given, through the real writer rather than a hand-typed element.
//
// Hand-typed fixtures drift from the shape vmsync actually writes, and a
// reader test standing on one proves only that it agrees with the fixture.
func domainXMLWithMetadata(t *testing.T, fields map[string]string) string {
	t.Helper()
	const bare = `<domain type="kvm"><name>web01</name><uuid>3f1a0f3e-0000-4000-8000-000000000001</uuid></domain>`
	if len(fields) == 0 {
		return bare
	}
	xml, err := libvirtsync.SetMetadataFields(bare, fields)
	if err != nil {
		t.Fatalf("SetMetadataFields: %v", err)
	}
	return xml
}

// TestApplyDomainMetadataMapsEveryField is the test the scan side did not
// have, and its absence is what let a field be consumed downstream while
// nothing ever filled it in.
//
// The verify verdict is the case in point: the promotion gate and the
// console both act on it, and until this change the scan simply did not read
// it, which is indistinguishable -- from every test that builds a Domain by
// hand -- from no domain ever carrying one.
func TestApplyDomainMetadataMapsEveryField(t *testing.T) {
	fields := map[string]string{
		libvirtsync.MetadataFieldReplicationRole:  libvirtsync.RoleTarget,
		libvirtsync.MetadataFieldLastCheckpoint:   "vmsync-cpt-000042",
		libvirtsync.MetadataFieldReplicaSource:    "hyper01p:web01",
		libvirtsync.MetadataFieldReplicaTargets:   "dr01:web01, dr02:web01",
		libvirtsync.MetadataFieldLastSync:         "1799999700",
		libvirtsync.MetadataFieldFailureCount:     "2",
		libvirtsync.MetadataFieldPromotedFrom:     "hyper01p:web01",
		libvirtsync.MetadataFieldPromotedBy:       "ops@example.org",
		libvirtsync.MetadataFieldPromotedAt:       "1799999500",
		libvirtsync.MetadataFieldPromotionMode:    "forced",
		libvirtsync.MetadataFieldFenceID:          "0123456789abcdef0123456789abcdef",
		libvirtsync.MetadataFieldFenceSource:      "hyper01p:web01",
		libvirtsync.MetadataFieldFenceArmedBy:     "ops@example.org",
		libvirtsync.MetadataFieldFenceArmedAt:     "1799999400",
		libvirtsync.MetadataFieldLastReplicatedAt: "1799999300",
		libvirtsync.MetadataFieldLastReplicatedTo: "dr01:web01",
		libvirtsync.MetadataFieldRestoredFrom:     "1756041600-vmsync-cpt-000040",
		libvirtsync.MetadataFieldRestoredAt:       "1799999200",
		libvirtsync.MetadataFieldRestoredBy:       "ops@example.org",
		libvirtsync.MetadataFieldVerifyState:      libvirtsync.VerifyStateFailed,
		libvirtsync.MetadataFieldVerifyFailedAt:   "1799999100",
	}

	var d Domain
	applyDomainMetadata(domainXMLWithMetadata(t, fields), &d)

	if d.VerifyState != libvirtsync.VerifyStateFailed {
		t.Errorf("VerifyState = %q, want %q -- the finding that says this replica is WRONG never leaves the hypervisor", d.VerifyState, libvirtsync.VerifyStateFailed)
	}
	if d.VerifyFailedAtUnix != 1799999100 {
		t.Errorf("VerifyFailedAtUnix = %d, want 1799999100", d.VerifyFailedAtUnix)
	}
	if d.Role != libvirtsync.RoleTarget || d.LastCheckpoint != "vmsync-cpt-000042" || d.ReplicaSource != "hyper01p:web01" {
		t.Errorf("the ordinary fields did not survive the extraction: %+v", d)
	}
	if len(d.ReplicaTargets) != 2 || d.ReplicaTargets[0] != "dr01:web01" || d.ReplicaTargets[1] != "dr02:web01" {
		t.Errorf("ReplicaTargets = %v, want the two entries split and trimmed", d.ReplicaTargets)
	}

	// Every field above is non-zero on purpose, so this walk can insist on
	// finding them all. A field added to Domain and never mapped here is zero
	// in both a comparison's got and its want, so only an explicit check in
	// this direction catches the omission -- which is the direction this
	// defect came from.
	//
	// The exemptions are the fields that do not come from metadata at all:
	// libvirt answers Name, UUID, Active and Persistent, and the disks and
	// restore points are read off the filesystem by describe.
	v := reflect.ValueOf(d)
	for i := 0; i < v.NumField(); i++ {
		switch name := v.Type().Field(i).Name; name {
		case "Name", "UUID", "Active", "Persistent", "Disks", "RestorePoints":
		default:
			if v.Field(i).IsZero() {
				t.Errorf("Domain.%s came back zero although the fixture recorded it: applyDomainMetadata does not map it, so the agent reports nothing for it", name)
			}
		}
	}
}

// A domain vmsync has never touched must read as nothing recorded, rather
// than as a finding nobody wrote.
func TestApplyDomainMetadataOnADomainWithNoMetadata(t *testing.T) {
	var d Domain
	applyDomainMetadata(domainXMLWithMetadata(t, nil), &d)
	if !reflect.DeepEqual(d, Domain{}) {
		t.Errorf("applyDomainMetadata() invented %+v for a domain carrying no vmsync metadata", d)
	}
}

// A finding whose date will not parse must keep the verdict and lose only
// the date. Dropping the verdict because its timestamp is garbled would let
// one unreadable character promote a replica known not to match its source.
func TestApplyDomainMetadataKeepsAVerdictWithAnUnreadableDate(t *testing.T) {
	var d Domain
	applyDomainMetadata(domainXMLWithMetadata(t, map[string]string{
		libvirtsync.MetadataFieldReplicationRole: libvirtsync.RoleTarget,
		libvirtsync.MetadataFieldVerifyState:     libvirtsync.VerifyStateFailed,
		libvirtsync.MetadataFieldVerifyFailedAt:  "not-a-unix-timestamp",
	}), &d)

	if d.VerifyState != libvirtsync.VerifyStateFailed {
		t.Errorf("VerifyState = %q, want the verdict to survive an unreadable date", d.VerifyState)
	}
	if d.VerifyFailedAtUnix != 0 {
		t.Errorf("VerifyFailedAtUnix = %d, want 0 for a date that cannot be read", d.VerifyFailedAtUnix)
	}
	if a := Assess(d, now, time.Hour); a.Status != StatusCritical {
		t.Errorf("Assess() status = %v, want critical: a finding with no usable date is still a finding", a.Status)
	}
}
