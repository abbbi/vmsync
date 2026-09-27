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
	"reflect"
	"testing"

	"vmsync/pkg/inventory"
	"vmsync/pkg/libvirtsync"
)

// fullyRecordedDomain is a domain carrying a value in every single field,
// including the ones almost no real domain has at once.
//
// Deliberately implausible -- a domain is not usually both a promotion and a
// restore and a fence -- because the point is not to model a real host. Every
// field being non-zero is what lets the walk below insist that every field
// arrives on the wire: a field the mapping drops is zero in the result, and
// zero is indistinguishable from "the fixture never set it" unless the
// fixture sets everything.
//
// Every value is also DISTINCT, which is a second requirement and an easy one
// to lose. The three host references and the three actor names would each
// read naturally as one value repeated -- one operator armed the fence and
// performed the promotion, one peer is both the replica source and the
// promotion's origin -- and with them shared, a mapping that crossed two of
// them (PromotedBy taking RestoredBy's value, say) would leave both fields
// non-zero, both equal to what the fixture expected, and the walk perfectly
// happy. Distinct values are what make a cross-wire fail rather than pass.
func fullyRecordedDomain() inventory.Domain {
	return inventory.Domain{
		Name:                 "web01",
		UUID:                 "3f1a0f3e-0000-4000-8000-000000000001",
		Active:               true,
		Persistent:           true,
		Role:                 libvirtsync.RoleTarget,
		LastCheckpoint:       "vmsync-cpt-000042",
		LastSyncUnix:         1799999700,
		FailureCount:         2,
		VerifyState:          libvirtsync.VerifyStateFailed,
		VerifyFailedAtUnix:   1799999100,
		ReplicaIncomplete:    "verb=reinit,at=1758441600,action=9f3c1a2b4d5e6f70,host=hv-a,aside=1758441600",
		ReplicaSource:        "hyper01p:web01",
		ReplicaTargets:       []string{"dr01:web01", "dr02:web01"},
		PromotedFrom:         "hyper02p:web01",
		PromotedAtUnix:       1799999500,
		PromotedBy:           "promoted-by@example.org",
		PromotionMode:        "forced",
		FenceID:              "0123456789abcdef0123456789abcdef",
		FenceSource:          "hyper03p:web01",
		FenceArmedAtUnix:     1799999400,
		FenceArmedBy:         "fence-armed-by@example.org",
		LastReplicatedAtUnix: 1799999300,
		LastReplicatedTo:     "dr01:web01",
		RestoredFrom:         "1756041600-vmsync-cpt-000040",
		RestoredAtUnix:       1799999200,
		RestoredBy:           "restored-by@example.org",
		Disks: []inventory.DiskInfo{{
			Path: "/var/lib/libvirt/images/web01.qcow2", ApparentBytes: 107374182400,
			AllocatedBytes: 42949672960, Missing: true,
		}},
		RestorePoints: []inventory.RestorePointInfo{{
			Tag: "1756041600-vmsync-cpt-000040", TakenAtUnix: 1756041600,
			CheckpointAtUnix: 1756041500, Checkpoint: "vmsync-cpt-000040",
			Source: "hyper01p:web01", Verify: "passed",
			Disks: []string{"web01.qcow2"}, Incomplete: true,
		}},
	}
}

// TestReportDomainFromMapsEveryField is the test the wire mapping did not
// have, and its absence is what let a field be consumed by the console while
// the agent never sent anything under it.
//
// The existing contract test builds a ReportDomain by hand and marshals it,
// so it proves the two programs agree on a json tag -- not that the scan ever
// fills that field in. Those are different claims, and only the second one
// keeps replica_incomplete travelling: a console showing a healthy replica
// whose disks are a half-written rebuild is exactly the failure this field
// exists to prevent, and it looks identical to a replica that simply has no
// marker.
func TestReportDomainFromMapsEveryField(t *testing.T) {
	d := fullyRecordedDomain()
	fenced := &ReportFenced{
		FenceID: "0123456789abcdef0123456789abcdef", State: "done",
		AtUnix: 1799999400, PeerRef: "dr01:web01", ArmedBy: "ops@example.org",
		Error: "domain would not stop within the timeout",
	}
	a := inventory.Assessment{
		Status:     inventory.StatusCritical,
		Reasons:    []string{"verification failed"},
		AgeSeconds: 900,
	}

	got := reportDomainFrom(d, fenced, a)

	// Verbatim, down to the last byte: pkg/failover parses this value to
	// decide whether to refuse a promotion, and the console renders it, so
	// anything this mapping normalises away is a field that reads differently
	// on the two sides of the wire.
	if got.ReplicaIncomplete != d.ReplicaIncomplete {
		t.Errorf("ReplicaIncomplete = %q, want %q -- the marker that says these disks are a half-written rebuild never reaches the console", got.ReplicaIncomplete, d.ReplicaIncomplete)
	}
	if got.VerifyState != d.VerifyState || got.VerifyFailedAtUnix != d.VerifyFailedAtUnix {
		t.Errorf("the verify verdict did not survive the mapping: %q/%d", got.VerifyState, got.VerifyFailedAtUnix)
	}
	// The two values that do not come off the domain at all, checked by
	// value so a walk finding them non-zero cannot be satisfied by the wrong
	// source being copied into them.
	if got.Fenced != fenced {
		t.Errorf("Fenced = %+v, want this host's own ledger entry -- metadata cannot say whether the fence actually worked", got.Fenced)
	}
	if got.Status != "critical" || got.AgeSeconds != a.AgeSeconds || len(got.Reasons) != 1 {
		t.Errorf("the assessment did not survive the mapping: %q/%d/%v", got.Status, got.AgeSeconds, got.Reasons)
	}
	if len(got.Disks) != 1 || got.Disks[0].Path != d.Disks[0].Path {
		t.Errorf("Disks = %+v, want the one disk the fixture recorded", got.Disks)
	}
	if len(got.RestorePoints) != 1 || got.RestorePoints[0].Tag != d.RestorePoints[0].Tag {
		t.Errorf("RestorePoints = %+v, want the one point the fixture recorded", got.RestorePoints)
	}

	// Every field of the result is non-zero above, so this walk can insist on
	// finding them all. A field added to ReportDomain and never mapped here is
	// zero in both a comparison's got and its want, so only an explicit check
	// in this direction catches the omission -- which is the direction this
	// defect came from, twice.
	//
	// Nothing is exempt, and that is deliberate rather than lucky. Three
	// fields genuinely do not come from the domain -- Fenced from this host's
	// fence ledger, Status/Reasons/AgeSeconds from the assessment -- and they
	// are exempt from nothing because they arrive as arguments that the
	// fixture sets. A future field sourced from somewhere else again should be
	// given the same treatment rather than an exemption here: an exempt field
	// is one this test has stopped watching.
	v := reflect.ValueOf(got)
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).IsZero() {
			t.Errorf("ReportDomain.%s came back zero although the fixture recorded it: reportDomainFrom does not map it, so the console is told nothing about it", v.Type().Field(i).Name)
		}
	}
}

// A domain vmsync has never touched must report as nothing recorded, rather
// than as a finding nobody wrote. The inverse of the walk above: a mapping
// that invented a value would be worse than one that dropped it, since the
// console acts on presence.
func TestReportDomainFromInventsNothing(t *testing.T) {
	got := reportDomainFrom(inventory.Domain{}, nil, inventory.Assessment{AgeSeconds: -1})
	want := ReportDomain{Status: inventory.StatusUnreplicated.String(), AgeSeconds: -1}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("reportDomainFrom() = %+v, want %+v for a domain carrying nothing at all", got, want)
	}
}
