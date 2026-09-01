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

package runresult

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "run.json")
	want := Result{VM: "db01", RunID: "abc123", FSThawFailed: true}
	if err := Write(p, want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := Read(p)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.VM != want.VM || got.RunID != want.RunID {
		t.Errorf("identity lost: got %+v, want %+v", got, want)
	}
	if !got.FSThawFailed || got.FSFreezeFailed {
		t.Errorf("flags not round-tripped: %+v", got)
	}
	if got.Version != Version {
		t.Errorf("version = %d, want %d", got.Version, Version)
	}
}

// TestMissingFileIsNotAnError is the fail-open half of the contract. A run
// that died before writing one, or an older vmsync that writes none at all,
// must not turn into a reported degradation -- the agent would then alarm on
// its own upgrade window.
func TestMissingFileIsNotAnError(t *testing.T) {
	got, err := Read(filepath.Join(t.TempDir(), "never-written.json"))
	if err != nil {
		t.Fatalf("a missing result must read as no degradation, got error: %v", err)
	}
	if got.Degraded() {
		t.Errorf("the zero Result reports a degradation: %+v", got)
	}
}

// TestCorruptFileIsAnError is the other half: nothing writes half a file by
// accident, so one that will not parse means something is wrong and the
// agent must say so rather than treat it as a clean run.
func TestCorruptFileIsAnError(t *testing.T) {
	p := filepath.Join(t.TempDir(), "run.json")
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(p); err == nil {
		t.Error("a corrupt result file read as a clean run")
	}
}

// TestUnknownFieldsAreIgnored covers the rolling upgrade: a newer vmsync
// writing a field this agent has never heard of must not cost it the fields
// it does understand.
func TestUnknownFieldsAreIgnored(t *testing.T) {
	p := filepath.Join(t.TempDir(), "run.json")
	if err := os.WriteFile(p, []byte(
		`{"version":99,"vm":"db01","fsthaw_failed":true,"invented_later":{"x":1}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Read(p)
	if err != nil {
		t.Fatalf("a newer schema must not be refused outright: %v", err)
	}
	if !got.FSThawFailed {
		t.Error("a thaw failure was lost to an unknown sibling field, which is the worst possible trade")
	}
}

// TestReasonNamesTheGuestAndTheFix is what an operator actually reads. A
// reason that says "thaw failed" and stops has moved the problem from the
// journal to the dashboard without making it any more actionable.
func TestReasonNamesTheGuestAndTheFix(t *testing.T) {
	for _, tc := range []struct {
		name     string
		r        Result
		degraded bool
		want     []string
	}{
		{"clean", Result{VM: "db01"}, false, nil},
		{"thaw", Result{VM: "db01", FSThawFailed: true}, true,
			[]string{"FROZEN", "virsh domfsthaw db01"}},
		{"freeze", Result{VM: "db01", FSFreezeFailed: true}, true,
			[]string{"crash-consistent"}},
		{"both", Result{VM: "db01", FSFreezeFailed: true, FSThawFailed: true}, true,
			[]string{"FROZEN", "crash-consistent", "virsh domfsthaw db01"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.r.Degraded(); got != tc.degraded {
				t.Fatalf("Degraded() = %v, want %v", got, tc.degraded)
			}
			reason := tc.r.Reason()
			if !tc.degraded {
				if reason != "" {
					t.Errorf("a clean run has a reason: %q", reason)
				}
				return
			}
			for _, want := range tc.want {
				if !strings.Contains(reason, want) {
					t.Errorf("reason %q does not mention %q", reason, want)
				}
			}
		})
	}
}

// TestBothFailuresLeadWithTheFrozenGuest fixes the priority between the two.
// The copy is finished and cannot get worse; the guest is blocked right now.
func TestBothFailuresLeadWithTheFrozenGuest(t *testing.T) {
	reason := Result{VM: "db01", FSFreezeFailed: true, FSThawFailed: true}.Reason()
	frozen := strings.Index(reason, "FROZEN")
	crash := strings.Index(reason, "crash-consistent")
	if frozen < 0 || crash < 0 {
		t.Fatalf("reason mentions only one of the two: %q", reason)
	}
	if frozen > crash {
		t.Errorf("reason leads with the copy rather than the blocked guest: %q", reason)
	}
}
