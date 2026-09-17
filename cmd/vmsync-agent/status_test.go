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
	"fmt"
	"strings"
	"testing"
)

// Absent values must read as absent, not as blanks that invite guessing:
// this is what a fresh or half-configured host actually looks like.
func TestFormatDomainSummaryReadsHonestly(t *testing.T) {
	d := ReportDomain{
		Name:           "web01",
		Active:         true,
		Role:           "source",
		Status:         "ok",
		Reasons:        []string{"replicating from hyper01p"},
		Disks:          []ReportDisk{{Path: "/data/web01.qcow2"}},
		ReplicaSource:  "",
		ReplicaTargets: []string{"dr01"},
	}
	got := fmt.Sprint(formatDomainSummary(d, 900))
	for _, want := range []string{
		"web01", "source", "ok", "900s",
		"/data/web01.qcow2", "dr01", "replicating from hyper01p",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary does not mention %q, got:\n%s", want, got)
		}
	}
}

func TestFormatDomainSummaryMarksWhatIsMissing(t *testing.T) {
	// No role, no cadence, no replica metadata, no disks: enrolled but the
	// console holds nothing for it yet. None of that may print as a value
	// that looks configured.
	d := ReportDomain{Name: "newvm", Status: "unknown"}
	got := fmt.Sprint(formatDomainSummary(d, 0))
	// Pairs render space-separated ("cadence none"), so assert that shape,
	// not a colon form the log never emits.
	for _, want := range []string{"newvm", "cadence none", "disks []", "targets []"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary does not show %q for the missing value, got:\n%s", want, got)
		}
	}
}
