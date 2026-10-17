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
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// everyMetadataField is every field vmsync can write into domain metadata,
// read out of the constant declarations themselves.
//
// PARSED, NOT LISTED, and not derived from metadataFieldOrder either. That
// list looked like the right source and is not: its own doc says it exists
// "purely for stable/readable XML output", and buildMetadataEntry explicitly
// handles "any OTHER field present" that it does not name -- so a field can
// be written without ever joining it, and would then have slipped past the
// test below without even moving its count.
//
// The constants are the one place a new field cannot avoid. A field added
// there enters this test on its own and has to be classified, which is the
// whole purpose: syncRedefineStrips' own comment calls "forgot one of the
// strip lists" this codebase's documented failure mode, and says a list
// nothing can enumerate is a list nothing can check.
func everyMetadataField(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "libvirt.go", nil, 0)
	if err != nil {
		t.Fatalf("parse libvirt.go: %v", err)
	}
	var out []string
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "MetadataField") || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				val, uerr := strconv.Unquote(lit.Value)
				if uerr != nil {
					t.Fatalf("unquote %s = %s: %v", name.Name, lit.Value, uerr)
				}
				out = append(out, val)
			}
		}
	}
	sort.Strings(out)
	return out
}

// THE WHOLE STRIP LIST, CHECKED AT ONCE.
//
// UpdateSyncMetadata merges into the SOURCE's XML and defines the TARGET, so
// every field not named in syncRedefineStrips is INHERITED. syncRedefineStrips'
// own comment calls "forgot one of the strip lists" this codebase's documented
// failure mode for target-only fields, and says "a list nothing can enumerate
// is a list nothing can check". This is that check.
//
// It works by giving a source EVERY field vmsync knows how to write, running
// a sync redefine, and asserting exactly which ones come out the other side.
// A field added later and not considered fails here, in both directions:
// inherited when it should not be, or missing when the target needs it.
//
// Five fields were being inherited when this was written. Three of them --
// restored_from/at/by -- were already INTENDED to be stripped; their doc
// comment said so and relied on a mechanism that does not exist.
func TestOnlyTheseMetadataFieldsSurviveASyncRedefine(t *testing.T) {
	// What a target legitimately carries after a successful sync, and why.
	// Everything absent from this map must be stripped.
	// VALUE, not merely presence. Asserting "something is here" lets the
	// SEEDED value through, which is inheritance wearing the name of a
	// legitimate write -- the precise failure this whole test is about. Only
	// exclude may legitimately come back seeded, because that one really is
	// inherited.
	wantSurvives := map[string]struct{ value, why string }{
		MetadataFieldLastCheckpoint:  {"vmsync-cpt-000009", "the checkpoint this replica is at: the whole point of the write"},
		MetadataFieldFailureCount:    {"0", "reset by a successful sync"},
		MetadataFieldReplicaSource:   {"src-host:testvm", "who this is a replica OF, written fresh from the source's identity"},
		MetadataFieldCheckpointAt:    {"1700000000", "the checkpoint's own timestamp"},
		MetadataFieldAutostartIntent: {AutostartIntentUnknown, "the source's autostart intent, carried so a promotion can honour it"},
		MetadataFieldReplicationRole: {RoleTarget, "written explicitly, so a deliberate -update-role=target survives a sync"},
		// THE ONE DELIBERATE INHERITANCE. An operator who excludes a source
		// means its replicas too, and a replica that had to be excluded
		// separately would be one somebody forgot. So here, and only here,
		// the seeded value IS the expected one.
		MetadataFieldExclude: {"seeded-" + MetadataFieldExclude, "inherited on purpose: excluding a source excludes its replicas"},
	}

	base := minimalDomainXML("testvm", "12345678-1234-1234-1234-123456789abc", "/var/lib/libvirt/images/x.qcow2")

	// A source carrying every field there is. Several of these combinations
	// could not occur together on one real domain; that is deliberate -- the
	// question is which fields CAN cross, not which commonly do.
	fields := everyMetadataField(t)
	// A floor, not an exact count: the list is parsed, so it grows by itself
	// and a new field gets classified by the leak check below rather than by
	// a number somebody has to remember to bump. This catches only the parse
	// silently finding nothing, which would make every assertion below pass
	// for the wrong reason.
	if len(fields) < 25 {
		t.Fatalf("only %d MetadataField constants were parsed out of libvirt.go; this audit is no longer exhaustive", len(fields))
	}
	seed := map[string]string{}
	for _, f := range fields {
		seed[f] = "seeded-" + f
	}
	srcXML, err := SetMetadataFields(base, seed)
	if err != nil {
		t.Fatalf("building the source xml: %v", err)
	}
	// Guard against the seeding silently doing nothing, which would make
	// every "stripped" assertion below pass for the wrong reason.
	for _, f := range fields {
		if got, _ := ParseMetadataField(srcXML, f); got != "seeded-"+f {
			t.Fatalf("seeding %s did not take (got %q); this test would otherwise pass without proving anything", f, got)
		}
	}

	out, err := UpdateSyncMetadata(srcXML, "vmsync-cpt-000009", "src-host", "testvm", RoleTarget, 1700000000, false, "", AutostartIntentUnknown)
	if err != nil {
		t.Fatalf("UpdateSyncMetadata() error = %v", err)
	}

	var leaked []string
	for _, f := range fields {
		// last_sync is this run's clock, so it has no fixed value; asserted
		// on its own below.
		if f == MetadataFieldLastSync {
			continue
		}
		got, _ := ParseMetadataField(out, f)
		want, allowed := wantSurvives[f]
		switch {
		case allowed && got == "":
			t.Errorf("%s is absent from the target, but it should survive: %s", f, want.why)
		case allowed && got != want.value:
			t.Errorf("%s = %q on the target, want %q (%s).\n"+
				"  A seeded value means the field was INHERITED from the source rather than written for this replica -- inheritance wearing the name of a legitimate write, which is the defect this test exists to catch.",
				f, got, want.value, want.why)
		case !allowed && got != "":
			leaked = append(leaked, f+"="+got)
		}
	}
	// last_sync must be this run's own clock, which is to say NOT the
	// source's inherited value.
	if got, _ := ParseMetadataField(out, MetadataFieldLastSync); got == "seeded-"+MetadataFieldLastSync || got == "" {
		t.Errorf("last_sync_timestamp = %q, want this run's own clock rather than the source's inherited value", got)
	}
	if len(leaked) > 0 {
		sort.Strings(leaked)
		t.Errorf("these fields were INHERITED from the source onto the replica: %s\n"+
			"  Every field not in syncRedefineStrips crosses from source to target, because UpdateSyncMetadata merges into the source's XML.\n"+
			"  If the field describes the SOURCE (a life as a source, a promotion, a fence) or describes the TARGET's own past, it belongs in syncRedefineStrips.\n"+
			"  If it genuinely belongs on the replica, add it to wantSurvives here with the reason.",
			strings.Join(leaked, ", "))
	}

	// The seeded value must never be what survives for a field the sync
	// writes itself -- that would mean inheritance wearing the right name.
	if got, _ := ParseMetadataField(out, MetadataFieldReplicaSource); got != "src-host:testvm" {
		t.Errorf("replica_source = %q, want it written from the source's identity rather than inherited", got)
	}
	if got, _ := ParseMetadataField(out, MetadataFieldLastCheckpoint); got != "vmsync-cpt-000009" {
		t.Errorf("last_checkpoint = %q, want the checkpoint just taken", got)
	}
}

// The specific consequence that made the five inherited fields worth
// finding, rather than a tidiness complaint.
//
// checkRestoreIdentity reads last_replicated_at off the TARGET to ask "has
// this domain served as a source since the point was taken". The inherited
// value tracks the SOURCE's most recent outward sync, so it is always about
// one sync old and advances on every run -- making every restore point older
// than the previous sync permanently unrestorable, on a domain that never
// replicated outward at all.
func TestAReplicaCarriesNoOutwardReplicationHistory(t *testing.T) {
	base := minimalDomainXML("testvm", "12345678-1234-1234-1234-123456789abc", "/var/lib/libvirt/images/x.qcow2")
	srcXML, err := SetMetadataFields(base, map[string]string{
		MetadataFieldReplicationRole: RoleSource,
		// The source replicated outward five minutes ago, as a healthy
		// source does on every run.
		MetadataFieldLastReplicatedAt: "1792000000",
		MetadataFieldLastReplicatedTo: "dr01:testvm",
	})
	if err != nil {
		t.Fatalf("building the source xml: %v", err)
	}

	out, err := UpdateSyncMetadata(srcXML, "vmsync-cpt-000009", "src-host", "testvm", RoleTarget, 1700000000, false, "", AutostartIntentUnknown)
	if err != nil {
		t.Fatalf("UpdateSyncMetadata() error = %v", err)
	}

	if got, _ := ParseMetadataField(out, MetadataFieldLastReplicatedAt); got != "" {
		t.Errorf("the replica carries last_replicated_at=%q inherited from its source. A restore of any point older than that stamp is refused as though this copy had served live, and the stamp advances on every sync", got)
	}
	if got, _ := ParseMetadataField(out, MetadataFieldLastReplicatedTo); got != "" {
		t.Errorf("the replica carries last_replicated_to=%q, so the console reports it as having replicated to itself", got)
	}
}

// ...and the restore record, whose doc comment already said it should be
// cleared by the next sync. A source that was itself once a restored replica
// is the ordinary shape of an inverted pair, and it was stamping its own
// rollback onto every replica of it -- where -promote then warned about a
// data loss that had happened to a different domain.
func TestAReplicaDoesNotInheritItsSourcesRestoreRecord(t *testing.T) {
	base := minimalDomainXML("testvm", "12345678-1234-1234-1234-123456789abc", "/var/lib/libvirt/images/x.qcow2")
	srcXML, err := SetMetadataFields(base, map[string]string{
		MetadataFieldReplicationRole: RoleSource,
		MetadataFieldRestoredFrom:    "1756041600-vmsync-cpt-000042",
		MetadataFieldRestoredAt:      "1756050000",
		MetadataFieldRestoredBy:      "alice",
	})
	if err != nil {
		t.Fatalf("building the source xml: %v", err)
	}

	out, err := UpdateSyncMetadata(srcXML, "vmsync-cpt-000009", "src-host", "testvm", RoleTarget, 1700000000, false, "", AutostartIntentUnknown)
	if err != nil {
		t.Fatalf("UpdateSyncMetadata() error = %v", err)
	}

	for _, f := range []string{MetadataFieldRestoredFrom, MetadataFieldRestoredAt, MetadataFieldRestoredBy} {
		if got, _ := ParseMetadataField(out, f); got != "" {
			t.Errorf("%s = %q on a replica that was never restored; it describes an event in its SOURCE's history", f, got)
		}
	}
}

// THE OTHER DIRECTION, asserted on the lists themselves.
//
// These fields are source-only, so the symmetry is tempting and wrong:
// having just seen them added to syncRedefineStrips, the obvious next tidy
// is to add them to recordReplicaTargetStrips as well -- the list a domain
// being established as a SOURCE is written with. That would erase the
// source's own outward history on every sync, which is the exact record the
// fields exist for: "standing at one host with the other unreachable, when
// did this VM last replicate, and where to" (see their doc comment).
//
// mergeMetadataFields applies removals AFTER updates, so such a strip would
// beat RecordReplicaTarget's own write of the value and the write would
// appear to succeed while storing nothing.
//
// Asserted against the named lists rather than through RecordReplicaTarget,
// which needs a live Manager -- the same reason, and the same shape, as
// TestPromotionFieldsDoNotStripTheTrace.
func TestTheOutwardHistoryIsStrippedFromReplicasOnlyNotFromSources(t *testing.T) {
	outward := []string{MetadataFieldLastReplicatedAt, MetadataFieldLastReplicatedTo}

	// On the REPLICA side they must be stripped: that is this change.
	for _, f := range outward {
		if !slices.Contains(syncRedefineStrips, f) {
			t.Errorf("%s is not in syncRedefineStrips, so every sync stamps the source's value onto the replica. checkRestoreIdentity reads it off the TARGET, so the forged stamp refuses restores of every point older than the previous sync", f)
		}
	}
	// On the SOURCE side they must NOT be: stripping them there deletes the
	// thing they exist to record.
	for _, f := range outward {
		if slices.Contains(recordReplicaTargetStrips, f) {
			t.Errorf("%s is in recordReplicaTargetStrips. A domain being established as a source would have its own outward-replication history erased on every sync -- and silently, because removals are applied after updates, so the write that records it would appear to succeed", f)
		}
	}
	// Neither list may be empty or renamed out from under this test, which
	// is how an assertion like the one above quietly stops meaning anything.
	if len(syncRedefineStrips) < 10 || len(recordReplicaTargetStrips) < 5 {
		t.Fatalf("strip lists look wrong (sync=%d, replicaTarget=%d); these assertions would pass against an empty list",
			len(syncRedefineStrips), len(recordReplicaTargetStrips))
	}
	// ...and the lists really are the ones in use: a field known to belong
	// to each, so a rename cannot leave both assertions vacuously true.
	if !slices.Contains(syncRedefineStrips, MetadataFieldVerifyState) {
		t.Error("syncRedefineStrips no longer contains verify_state; this is not the list it was")
	}
	if !slices.Contains(recordReplicaTargetStrips, MetadataFieldReplicaIncomplete) {
		t.Error("recordReplicaTargetStrips no longer contains replica_incomplete; this is not the list it was")
	}
}

// The wire names of the two source-only fields.
//
// pkg/failover asserts that an inversion does NOT strip them from the
// demoted end (TestInvertKeepsTheDemotedEndsOutwardReplicationHistory), and
// has to spell them as string literals because it cannot import this
// package. A rename here would turn that assertion into one about a field
// that no longer exists -- absence being trivially true -- and the decision
// it records would stop being enforced without anything failing.
func TestTheOutwardHistoryFieldNamesAreStable(t *testing.T) {
	if MetadataFieldLastReplicatedAt != "last_replicated_at" {
		t.Errorf("MetadataFieldLastReplicatedAt = %q, want \"last_replicated_at\"; pkg/failover's invert test spells this literally", MetadataFieldLastReplicatedAt)
	}
	if MetadataFieldLastReplicatedTo != "last_replicated_to" {
		t.Errorf("MetadataFieldLastReplicatedTo = %q, want \"last_replicated_to\"; pkg/failover's invert test spells this literally", MetadataFieldLastReplicatedTo)
	}
}
