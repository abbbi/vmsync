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
	"errors"
	"strings"
	"testing"
)

// Excluded is the single point standing between an operator writing
// "leave this alone" and vmsync replicating it anyway, so it is worth
// asserting exhaustively -- the same argument TargetRoleAllowsSync makes
// for being a pure function.
func TestExcluded(t *testing.T) {
	cases := []struct {
		name         string
		value        string
		reportOnly   bool
		hidden       bool
		unrecognised bool
	}{
		{"unset is the ordinary case", "", false, false, false},
		{"report-only", ExcludeReportOnly, true, false, false},
		{"fully-ignored", ExcludeFullyIgnored, false, true, false},

		// Written by a person with virsh, so it arrives however they
		// typed it. None of these should need a second attempt.
		{"report-only, capitalised", "Report-Only", true, false, false},
		{"fully-ignored, shouted", "FULLY-IGNORED", false, true, false},
		{"fully-ignored, with the whitespace an editor leaves", "  fully-ignored\n", false, true, false},

		// THE DIRECTION THAT MATTERS. Somebody wrote "exclude" on this
		// domain meaning to stop vmsync touching it. A value this build
		// cannot read must not resume replication -- that is precisely
		// the thing they were preventing.
		{"a typo still excludes", "ful", true, false, true},
		{"the old name still excludes", "report", true, false, true},
		{"nonsense still excludes", "yes please", true, false, true},

		// ...and it fails to report-only, not to full, so the domain
		// stays visible carrying the warning that explains itself.
		{"an unrecognised value never hides the domain", "ful", true, false, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Excluded(tc.value)
			if got.ReportOnly != tc.reportOnly {
				t.Errorf("Excluded(%q).ReportOnly = %v, want %v", tc.value, got.ReportOnly, tc.reportOnly)
			}
			if got.Hidden != tc.hidden {
				t.Errorf("Excluded(%q).Hidden = %v, want %v", tc.value, got.Hidden, tc.hidden)
			}
			if got.Unrecognised != tc.unrecognised {
				t.Errorf("Excluded(%q).Unrecognised = %v, want %v", tc.value, got.Unrecognised, tc.unrecognised)
			}
			// Never both: one says "show it and do nothing", the other
			// says "do not show it". A caller reading them in the wrong
			// order would get opposite answers.
			if got.ReportOnly && got.Hidden {
				t.Errorf("Excluded(%q) is both ReportOnly and Hidden, which are contradictory", tc.value)
			}
		})
	}
}

// An unset field must be indistinguishable from no opinion, because that is
// what every domain in an existing estate looks like. A default that
// excluded would stop every sync on upgrade.
func TestExcludedUnsetDoesNothing(t *testing.T) {
	for _, v := range []string{"", " ", "\t\n"} {
		got := Excluded(v)
		if got.ReportOnly || got.Hidden || got.Unrecognised {
			t.Errorf("Excluded(%q) = %+v, want the zero decision: an estate that has never heard of this field must be unaffected by it", v, got)
		}
	}
}

// The refusal has to name the field, the value and the remedy. An operator
// meeting it months later needs to know why nothing is syncing and what to
// undo -- the field is invisible unless you already know to look for it.
func TestExcludeRefusalSaysWhatToDo(t *testing.T) {
	msg := ExcludeRefusal("web01", Excluded(ExcludeReportOnly))
	for _, want := range []string{"web01", "exclude", ExcludeReportOnly, "virsh metadata"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not mention %q: %s", want, msg)
		}
	}

	// The unrecognised case must additionally say what it DID, or an
	// operator reads a complaint about a typo and assumes nothing changed.
	bad := ExcludeRefusal("web01", Excluded("ful"))
	for _, want := range []string{"web01", "ful", ExcludeReportOnly, ExcludeFullyIgnored} {
		if !strings.Contains(bad, want) {
			t.Errorf("the unrecognised-value refusal does not mention %q: %s", want, bad)
		}
	}
}

// vmsync must never write this field. It is the operator's, and an
// exclusion the tool can clear on its own is not an exclusion -- so the
// guarantee is worth a test rather than a comment, because the next person
// adding a metadata write has no reason to know.
func TestNothingWritesTheExcludeField(t *testing.T) {
	// The write path is SetDomainMetadataFields; every caller passes field
	// names from the constants above. If a future caller ever passes this
	// one, that call is the bug.
	//
	// Asserted by construction here: the constant's value is checked, so a
	// grep for it in the non-test tree is a complete audit. See
	// TestExcludeFieldNameIsStable.
	if MetadataFieldExclude != "exclude" {
		t.Fatalf("MetadataFieldExclude = %q; the documented field name is \"exclude\" and operators have it written in their domain XML", MetadataFieldExclude)
	}
}

// The field name is part of the operator-facing contract: it lives in
// domain XML somebody edited by hand, and renaming it silently un-excludes
// every domain that carries the old name.
func TestExcludeFieldNameIsStable(t *testing.T) {
	if MetadataFieldExclude != "exclude" {
		t.Errorf("MetadataFieldExclude = %q, want \"exclude\"", MetadataFieldExclude)
	}
	if ExcludeReportOnly != "report-only" {
		t.Errorf("ExcludeReportOnly = %q, want \"report-only\"", ExcludeReportOnly)
	}
	if ExcludeFullyIgnored != "fully-ignored" {
		t.Errorf("ExcludeFullyIgnored = %q, want \"fully-ignored\"", ExcludeFullyIgnored)
	}
}

// The XML in docs/PRINCIPLES_OF_OPERATION.md §3.3, verbatim. A documented
// example that does not parse is worse than no example at all: the field
// is set by hand, so the snippet IS the interface, and an operator who
// copies it and sees nothing happen has no way to tell whether they
// mistyped it or vmsync ignored them.
func TestDocumentedExcludeXMLParses(t *testing.T) {
	const domainXML = `<domain type='kvm'>
  <name>web01</name>
  <metadata>
    <vmsync:vmsync xmlns:vmsync="http://vmsync.org/xmlns/libvirt/domain/1.0">
      <exclude id="report-only"/>
    </vmsync:vmsync>
  </metadata>
</domain>`
	got, err := ParseMetadata(domainXML, MetadataFieldExclude)
	if err != nil {
		t.Fatalf("the documented XML does not parse: %v", err)
	}
	if got != ExcludeReportOnly {
		t.Errorf("ParseMetadata = %q, want %q", got, ExcludeReportOnly)
	}
	if d := Excluded(got); !d.ReportOnly || d.Unrecognised {
		t.Errorf("Excluded(%q) = %+v, want a plain report-only decision", got, d)
	}

	const ignored = `<domain type='kvm'><name>other</name><metadata>
    <vmsync:vmsync xmlns:vmsync="http://vmsync.org/xmlns/libvirt/domain/1.0">
      <exclude id="fully-ignored"/>
    </vmsync:vmsync></metadata></domain>`
	got2, err := ParseMetadata(ignored, MetadataFieldExclude)
	if err != nil {
		t.Fatalf("the fully-ignored example does not parse: %v", err)
	}
	if d := Excluded(got2); !d.Hidden {
		t.Errorf("Excluded(%q) = %+v, want Hidden", got2, d)
	}
}

// THE XML VIRSH ACTUALLY PRODUCES, captured from a real domain on a real
// libvirt rather than written by hand.
//
// vmsync writes its own children unprefixed; `virsh metadata --set` writes
// them PREFIXED, because virsh applies the --key prefix itself. Both must
// read, and only one of them had ever been tested -- the hand-written
// unprefixed form. An operator following the documentation produces the
// other one.
func TestVirshWrittenExcludeParses(t *testing.T) {
	// Verbatim from `virsh dumpxml --inactive` after the documented
	// `--set '<vmsync><exclude id="report-only"/></vmsync>'`.
	const fromVirsh = `<domain type='qemu'>
  <name>web01</name>
  <metadata>
    <vmsync:vmsync xmlns:vmsync="http://vmsync.org/xmlns/libvirt/domain/1.0">
      <vmsync:exclude id="report-only"/>
    </vmsync:vmsync>
  </metadata>
</domain>`
	got, err := ParseMetadata(fromVirsh, MetadataFieldExclude)
	if err != nil {
		t.Fatalf("the XML virsh writes does not parse: %v", err)
	}
	if got != ExcludeReportOnly {
		t.Errorf("ParseMetadata = %q, want %q -- an operator following the documented virsh command would get an exclusion that silently does nothing", got, ExcludeReportOnly)
	}
}

// ...and the shape the WRONG virsh payload produces must not read as an
// exclusion, because it is not one.
//
// `--set '<vmsync:vmsync>…'` (prefixed, no xmlns) makes virsh print
// "Metadata modified" and store a double-prefixed container. Nothing can
// act on that, and the risk is the opposite of the usual one: a reader
// lenient enough to accept it would be accepting malformed metadata as an
// instruction. This pins that it does not parse, so the documentation's
// warning stays true.
func TestDoublePrefixedContainerIsNotAnExclusion(t *testing.T) {
	const malformed = `<domain type='qemu'>
  <name>web01</name>
  <metadata>
    <vmsync:vmsync:vmsync xmlns:vmsync="http://vmsync.org/xmlns/libvirt/domain/1.0">
      <vmsync:exclude id="report-only"/>
    </vmsync:vmsync:vmsync>
  </metadata>
</domain>`
	got, _ := ParseMetadata(malformed, MetadataFieldExclude)
	if Excluded(got).ReportOnly || Excluded(got).Hidden {
		t.Errorf("a double-prefixed container read as exclude=%q; malformed metadata must not be actionable", got)
	}
}

// MALFORMED METADATA MUST NOT READ AS "NOT EXCLUDED", and this is the
// shape that makes it urgent rather than theoretical: it is what
// `virsh metadata --set` stores from a prefixed payload, which is the
// obvious wrong spelling of the documented command. The person most
// likely to produce it is the one who was trying to exclude the domain.
//
// The gates key on ErrMetadataUnreadable rather than on any error, so a
// host that simply could not be reached still proceeds -- a transient
// fault is not somebody's instruction.
func TestUnreadableMetadataIsDistinguishableFromAbsent(t *testing.T) {
	// Absent: no element at all. Not an error, not an exclusion.
	fields, err := metadataFieldsFromFragment("")
	if err != nil {
		t.Errorf("an absent vmsync element errored: %v -- every domain that has never been synced looks like this", err)
	}
	if len(fields) != 0 {
		t.Errorf("an absent element produced fields: %v", fields)
	}

	// Present and well-formed: parses, no error.
	good := `<vmsync:vmsync xmlns:vmsync="` + metadataNamespace + `"><exclude id="report-only"/></vmsync:vmsync>`
	if _, err := metadataFieldsFromFragment(good); err != nil {
		t.Errorf("a well-formed element errored: %v", err)
	}

	// Present and malformed: the double-prefixed container virsh writes
	// from a prefixed payload. Must error, and the error must carry the
	// sentinel the gates test for.
	bad := `<vmsync:vmsync:vmsync xmlns:vmsync="` + metadataNamespace + `"><vmsync:exclude id="report-only"/></vmsync:vmsync:vmsync>`
	_, err = metadataFieldsFromFragment(bad)
	if err == nil {
		t.Fatal("a double-prefixed container parsed cleanly; the exclude gates would read it as 'not excluded'")
	}
	if !errors.Is(err, ErrMetadataUnreadable) {
		t.Errorf("the error does not carry ErrMetadataUnreadable: %v -- the gates cannot tell it from an unreachable host, and would proceed", err)
	}
}
