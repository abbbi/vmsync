/*
	Copyright (C) 2026  Orsiris de Jong <ozy@netpower.fr>

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

You should have received a copy of the GNU General Public License
along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

package main

import (
	"errors"
	"strings"
	"testing"

	"vmsync/pkg/libvirtsync"
)

// EVERY MODE IS EITHER GATED OR DELIBERATELY EXEMPT, and this is what makes
// that true rather than hoped for. cliModes is the complete list of verbs,
// so walking it is a complete audit -- a mode added later is refused on an
// excluded domain by default (the exemptions are an allowlist) and shows up
// here the moment somebody adds it without deciding.
func TestEveryModeIsGatedOrDeliberatelyExempt(t *testing.T) {
	// Read-only verbs, exempt on purpose. Anything else must be gated.
	// Listed again here rather than read from excludeReadOnlyModes, so
	// adding a name to that map does not silently satisfy this test too.
	wantExempt := map[string]bool{
		"-read-fence":          true,
		"-list-restore-points": true,
		"-explain-domain":      true,
	}

	// Every mode flag, turned on one at a time is not possible from here
	// (they are struct fields), so walk the table's names instead.
	var names []string
	for _, m := range cliModes(syncConfig{}) {
		names = append(names, m.name)
	}
	names = append(names, modesHandledInline...)
	if len(names) < 10 {
		t.Fatalf("only found %d modes (%v) -- cliModes changed shape and this audit is no longer complete", len(names), names)
	}

	for _, name := range names {
		exempt := excludeReadOnlyModes[name]
		if exempt != wantExempt[name] {
			if exempt {
				t.Errorf("mode %s is exempt from the exclusion gate but is not one of the read-only verbs. An exclusion means vmsync does not act on the domain; if this verb only reads, add it to the list in this test with a note saying why.", name)
			} else {
				t.Errorf("mode %s is gated but this test expects it exempt -- update one of them", name)
			}
		}
	}
}

// The read-only verbs must stay runnable, and -explain-domain most of all:
// it is how an operator finds out WHY nothing is happening to a VM, so
// refusing it on an excluded domain would hide the exclusion behind itself.
func TestReadOnlyModesAreNeverRefused(t *testing.T) {
	// No URIs configured, so the gate cannot reach libvirt at all -- which
	// also proves the exemption is decided before any connection is tried.
	cfg := syncConfig{TargetDomain: "web01", SourceDomain: "web01"}
	for mode := range excludeReadOnlyModes {
		if err := refuseExcludedMode(cfg, mode); err != nil {
			t.Errorf("refuseExcludedMode(%s) = %v, want nil", mode, err)
		}
	}
}

// With nothing to connect to, a gated mode must FAIL OPEN rather than
// refuse: an unreachable libvirt is not evidence of an exclusion, and the
// verb itself is about to fail with a better message.
func TestGatedModeFailsOpenWhenLibvirtIsUnreachable(t *testing.T) {
	cfg := syncConfig{
		TargetURI:    "qemu+ssh://nonexistent.invalid/system",
		TargetDomain: "web01",
	}
	if err := refuseExcludedMode(cfg, "-promote"); err != nil {
		t.Errorf("refuseExcludedMode = %v, want nil: a host that cannot be asked must not read as an exclusion", err)
	}
}

// THE GATE MUST RESOLVE THE DOMAIN THE VERB WILL ACT ON, not the field it
// happens to be typed into. -update-role and -release-promotion address
// "-target-domain, or -source-domain if that is empty", and -invert fills
// the same default in inside its own handler, after the gate has run.
//
// Reading cfg.TargetDomain alone made all three gates no-ops for the
// documented `-target-uri X -source-domain web01` spelling: the target
// side was skipped for having no domain, the source side for having no
// URI, and the gate returned nil having read nothing. On -update-role
// that is the write which makes an excluded domain syncable again.
func TestTheGateResolvesTheDomainTheVerbActsOn(t *testing.T) {
	// Only -source-domain given, with a target URI: the shape that slipped
	// through. It must reach a libvirt lookup rather than returning early,
	// which an unreachable host proves -- a gate that skipped both sides
	// would return instantly and never try to connect.
	cfg := syncConfig{
		TargetURI:    "qemu+ssh://nonexistent.invalid/system",
		SourceDomain: "web01",
	}
	// Fails open because the host is unreachable, so nil is correct here.
	// What is asserted is that it TRIED: with the old resolution the
	// target side was skipped outright.
	if err := refuseExcludedMode(cfg, "-update-role"); err != nil {
		t.Errorf("refuseExcludedMode = %v, want nil (fail-open on an unreachable host)", err)
	}
	// The resolution itself, asserted directly rather than inferred.
	if got := resolvedTargetDomain(cfg); got != "web01" {
		t.Errorf("the gate resolves the target domain as %q; the verbs resolve it as %q, and a gate that disagrees with its verb checks the wrong domain or none",
			got, "web01")
	}
}

// The two sentinels say different things because they describe different
// refusals, and a -promote reported as "does not permit syncing" sends an
// operator looking in the wrong place.
func TestTheTwoExcludeSentinelsReadCorrectly(t *testing.T) {
	if !strings.Contains(libvirtsync.ErrExcludeRefusesSync.Error(), "syncing") {
		t.Errorf("the sync sentinel does not mention syncing: %v", libvirtsync.ErrExcludeRefusesSync)
	}
	if strings.Contains(libvirtsync.ErrExcludeRefusesAction.Error(), "syncing") {
		t.Errorf("the action sentinel claims a sync was refused: %v -- it covers promote, invert, restore and the rest", libvirtsync.ErrExcludeRefusesAction)
	}
	if errors.Is(libvirtsync.ErrExcludeRefusesAction, libvirtsync.ErrExcludeRefusesSync) {
		t.Error("the two sentinels must stay distinguishable")
	}
	// Neither may read as a role refusal: that was the original defect.
	for _, e := range []error{libvirtsync.ErrExcludeRefusesSync, libvirtsync.ErrExcludeRefusesAction} {
		if strings.Contains(e.Error(), "replication role") {
			t.Errorf("%v reads as a role refusal, which is what the operator-facing message got wrong the first time", e)
		}
	}
}
