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

package libvirtsync

import (
	"reflect"
	"testing"

	"vmsync/pkg/failover"
)

// failoverStateDomXML builds a domain carrying exactly the vmsync metadata
// given, using the real writer rather than a hand-typed block -- the same
// approach TestSetMetadataFieldsAndParseMetadata takes, and for the same
// reason: a fixture typed by hand can drift from the element shape vmsync
// actually writes, and a reader tested only against the hand-typed spelling
// would then pass while failing on every real domain.
//
// No fields at all produces the domain untouched, which is what a replica
// that vmsync has never written to looks like.
func failoverStateDomXML(t *testing.T, fields map[string]string) string {
	t.Helper()
	base := minimalDomainXML("web01", "12345678-1234-1234-1234-123456789abc", "/var/lib/libvirt/images/web01.qcow2")
	if len(fields) == 0 {
		return base
	}
	out, err := SetMetadataFields(base, fields)
	if err != nil {
		t.Fatalf("SetMetadataFields() error = %v", err)
	}
	return out
}

// The whole mapping from a domain's metadata to the state a failover decides
// from, asserted field by field.
//
// This test is the one that was missing. The verify verdict was consumed by
// pkg/failover's promotion gate and never read out of the XML here, and
// nothing failed: the consumer's own tests set the field by hand, and this
// side had no test at all because the mapping could only be reached through
// a live libvirt connection. So the whole struct is compared here, not just
// the fields of the day -- a field that stops being mapped has to break
// something, whichever field it is.
func TestFailoverStateFromXML(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields map[string]string
		want   FailoverState
		// everyFieldMapped marks the one case whose fixture sets every
		// metadata-derived field to a non-zero value, so the check below can
		// insist on finding them all. See where it is set for why a
		// whole-struct comparison alone does not cover this.
		everyFieldMapped bool
	}{
		{
			// A domain vmsync has never touched. Every field zero, and in
			// particular no verify verdict invented for a domain that has
			// never been verified.
			name:   "no vmsync metadata at all",
			fields: nil,
			want:   FailoverState{},
		},
		{
			// Everything a target can carry, at once. The point is coverage
			// of the mapping rather than of any one field: this is the case
			// that fails when a field is dropped from the reader.
			name: "a full metadata element",
			fields: map[string]string{
				MetadataFieldReplicationRole:     RoleTarget,
				MetadataFieldLastCheckpoint:      "vmsync-cpt-000012",
				MetadataFieldLastSync:            "1700000300",
				MetadataFieldCheckpointAt:        "1700000240",
				MetadataFieldReplicaSource:       "prod01:web01",
				MetadataFieldReplicaTargets:      "dr01:web01, dr02:web01",
				MetadataFieldFailureCount:        "3",
				MetadataFieldSourceStoppedAtSync: "1",
				MetadataFieldRestoredFrom:        "1756041600-vmsync-cpt-000042",
				MetadataFieldVerifyState:         VerifyStateFailed,
				MetadataFieldVerifyFailedAt:      "1700000500",
				MetadataFieldFenceID:             "0123456789abcdef0123456789abcdef",
				MetadataFieldFenceSource:         "prod01:web01",
				MetadataFieldFenceArmedBy:        "ops@example.org",
				MetadataFieldFenceArmedAt:        "1700000400",
			},
			want: FailoverState{
				Role:                RoleTarget,
				LastCheckpoint:      "vmsync-cpt-000012",
				LastSyncUnix:        1700000300,
				CheckpointAtUnix:    1700000240,
				ReplicaSource:       "prod01:web01",
				ReplicaTargets:      []string{"dr01:web01", "dr02:web01"},
				FailureCount:        3,
				SourceStoppedAtSync: true,
				RestoredFrom:        "1756041600-vmsync-cpt-000042",
				VerifyState:         VerifyStateFailed,
				VerifyFailedAt:      1700000500,
				Fence: failover.FenceToken{
					ID:      "0123456789abcdef0123456789abcdef",
					Source:  "prod01:web01",
					ArmedBy: "ops@example.org",
					ArmedAt: 1700000400,
				},
			},
			// Every metadata-derived field above is deliberately non-zero, so
			// the walk in the loop body can require each of them to arrive.
			// The whole-struct comparison catches a field that STOPS being
			// mapped; it cannot catch one that is added to FailoverState and
			// never mapped here, since that field is zero in both got and
			// want and compares equal. That is the direction this defect came
			// from, so it is the direction worth a test of its own.
			everyFieldMapped: true,
		},
		{
			// The defect this whole change exists for, in its ordinary
			// shape: an otherwise healthy-looking replica that last night's
			// -verify found differing from its source. Both halves of the
			// record have to arrive, because the verdict decides the refusal
			// and the date is what tells an operator whether they are
			// looking at this morning or at March.
			name: "verify_state failed with a recorded timestamp",
			fields: map[string]string{
				MetadataFieldReplicationRole: RoleTarget,
				MetadataFieldLastCheckpoint:  "vmsync-cpt-000012",
				MetadataFieldLastSync:        "1700000300",
				MetadataFieldReplicaSource:   "prod01:web01",
				MetadataFieldVerifyState:     VerifyStateFailed,
				MetadataFieldVerifyFailedAt:  "1700000500",
			},
			want: FailoverState{
				Role:           RoleTarget,
				LastCheckpoint: "vmsync-cpt-000012",
				LastSyncUnix:   1700000300,
				ReplicaSource:  "prod01:web01",
				VerifyState:    VerifyStateFailed,
				VerifyFailedAt: 1700000500,
			},
		},
		{
			// A verdict recorded by something that wrote no date -- an older
			// vmsync, or a write interrupted between the two fields. The
			// verdict must survive on its own: a replica known not to match
			// its source does not become promotable because nobody wrote
			// down when it was found out.
			name: "verify_state failed with no timestamp at all",
			fields: map[string]string{
				MetadataFieldReplicationRole: RoleTarget,
				MetadataFieldVerifyState:     VerifyStateFailed,
			},
			want: FailoverState{
				Role:        RoleTarget,
				VerifyState: VerifyStateFailed,
			},
		},
		{
			// The same, with a date that cannot be read. Fail closed on the
			// VALUE, not on the whole read: the timestamp goes to zero and
			// everything else -- the verdict above all -- still arrives.
			// Rejecting the read outright would turn a garbled character in
			// one metadata field into a promotion that never gets assessed,
			// which is the unsafe direction.
			name: "verify_state failed with a garbage timestamp",
			fields: map[string]string{
				MetadataFieldReplicationRole: RoleTarget,
				MetadataFieldReplicaSource:   "prod01:web01",
				MetadataFieldVerifyState:     VerifyStateFailed,
				MetadataFieldVerifyFailedAt:  "not-a-unix-timestamp",
			},
			want: FailoverState{
				Role:          RoleTarget,
				ReplicaSource: "prod01:web01",
				VerifyState:   VerifyStateFailed,
			},
		},
		{
			// The other half of "presence is the state": a stale timestamp
			// with no verdict beside it is not a finding. Reading one as a
			// failure would make a replica unpromotable on the strength of a
			// field nothing currently asserts, and the override for that
			// would be the same flag used to promote a genuinely bad
			// replica -- which teaches operators to reach for it.
			name: "a timestamp with no verdict is not a finding",
			fields: map[string]string{
				MetadataFieldReplicationRole: RoleTarget,
				MetadataFieldVerifyFailedAt:  "1700000500",
			},
			want: FailoverState{
				Role:           RoleTarget,
				VerifyFailedAt: 1700000500,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := failoverStateFromXML(failoverStateDomXML(t, tc.fields))
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("failoverStateFromXML() =\n%+v\nwant\n%+v", got, tc.want)
			}
			// Said separately because it is a different claim from the
			// comparison above: these three are observations about the
			// domain, not about its metadata, and this function must never
			// answer them. ReadFailoverState fills them in from the libvirt
			// handle it holds, and a false here that came from this function
			// rather than from that read would be a guess wearing the
			// clothes of a measurement.
			if got.Exists || got.Active || got.HasCheckpoints {
				t.Errorf("the metadata reader set a libvirt-only observation: exists=%v active=%v has_checkpoints=%v",
					got.Exists, got.Active, got.HasCheckpoints)
			}
			if !tc.everyFieldMapped {
				return
			}
			// Exists, Active and HasCheckpoints are the three this function
			// must leave alone -- asserted above -- so they are the three
			// exempted here.
			v := reflect.ValueOf(got)
			for i := 0; i < v.NumField(); i++ {
				switch name := v.Type().Field(i).Name; name {
				case "Exists", "Active", "HasCheckpoints":
				default:
					if v.Field(i).IsZero() {
						t.Errorf("FailoverState.%s came back zero although the fixture recorded it: failoverStateFromXML does not map it, so nothing downstream can ever see it", name)
					}
				}
			}
		})
	}
}

// A document that is not a domain at all must read as "nothing recorded"
// rather than panicking or inventing values.
//
// ParseMetadata returns an error for it, and every call in the reader
// deliberately swallows that into the zero value. That is right here: the
// caller is holding XML libvirt itself just produced, so the realistic cause
// of a parse failure is a shape this build does not understand, and the safe
// reading of an unknown shape is "no evidence", which every gate in
// pkg/failover treats as a reason to refuse rather than to proceed.
func TestFailoverStateFromXMLOnUnreadableXML(t *testing.T) {
	if got := failoverStateFromXML("this is not a domain definition"); !reflect.DeepEqual(got, FailoverState{}) {
		t.Errorf("failoverStateFromXML(garbage) = %+v, want a zero state", got)
	}
}
