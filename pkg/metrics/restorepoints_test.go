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

package metrics

import (
	"fmt"
	"strings"
	"testing"
)

// restorePoint*Families is every series writeRestorePoints can emit, split by
// which gate it sits behind. Listed by hand so that adding a series without
// deciding its gate fails a test rather than shipping: which gate a series is
// under IS its meaning, because this file persists between runs -- one written
// every run is scrapeable for the whole inter-run gap and can carry any `for:`
// clause, while one omitted on some runs flickers and can carry none.
var (
	restorePointAlwaysFamilies = []string{
		"vmsync_restore_point_outcome",
		"vmsync_restore_points_policy_count",
		"vmsync_restore_points_policy_interval_seconds",
		"vmsync_restore_points_store_readable",
		"vmsync_restore_point_overdue_seconds",
	}
	restorePointListedFamilies = []string{
		"vmsync_restore_points",
		"vmsync_restore_points_over_count",
		"vmsync_restore_points_staging",
		"vmsync_restore_point_last_taken_timestamp_seconds",
		"vmsync_restore_point_oldest_timestamp_seconds",
	}
)

// hasFamily reports whether a family is declared in the rendered file.
//
// By its HELP line and not by a substring search, because the names nest:
// "vmsync_restore_points" is a prefix of "vmsync_restore_points_policy_count",
// so a Contains check for the first is satisfied by the second and the gating
// tests below would pass whatever the gates did.
func hasFamily(out, name string) bool {
	return strings.Contains(out, "# HELP "+name+" ")
}

// A run that never asked for retention must publish NOTHING about restore
// points.
//
// The same rule VerificationRan enforces, and for the same reason: a
// vmsync_restore_points 0 from such a run does not mean "the store is empty", it
// means nobody looked -- and an alert on a replica having no restore points
// would then fire on every host that does not use the feature.
func TestNoRetentionEmitsNoRestorePointSeries(t *testing.T) {
	out := writeAndRead(t, RunMetric{VM: "web01", TargetVM: "web01", RetentionAsked: false})
	for _, name := range append(append([]string{}, restorePointAlwaysFamilies...), restorePointListedFamilies...) {
		if hasFamily(out, name) {
			t.Errorf("%s was emitted for a run with no -retention; a zero there reads as an empty store rather than as a question nobody asked", name)
		}
	}
}

// A run refused BEFORE it could read its store -- a filesystem without reflink
// support, a domain with no disks -- still says what was asked for and that it
// could not look. What it must not do is publish a count.
//
// last_taken=0 is the damaging one: read as a timestamp it means 1970, so
// `time() - last_taken` turns a store nobody opened into the most starved
// replica in the estate.
func TestRetentionAskedButStoreUnreadEmitsTheOutcomeWithoutTheCounts(t *testing.T) {
	out := writeAndRead(t, RunMetric{
		VM: "web01", TargetVM: "web01",
		RetentionAsked:                     true,
		RestorePointsListed:                false,
		RestorePointOutcome:                RestorePointUndetermined,
		RestorePointsPolicyCount:           24,
		RestorePointsPolicyIntervalSeconds: 10800,
	})
	for _, name := range restorePointAlwaysFamilies {
		if !hasFamily(out, name) {
			t.Errorf("%s is missing; it is known from the flag alone and has to exist on every run, or no `for:` clause can read it", name)
		}
	}
	for _, name := range restorePointListedFamilies {
		if hasFamily(out, name) {
			t.Errorf("%s was emitted for a run that never read its store, so its value describes nothing", name)
		}
	}
	if !strings.Contains(out, `vmsync_restore_points_store_readable{source_host="",target_host="",vm="web01",target_vm="web01"} 0`) {
		t.Errorf("store_readable is not 0, so nothing says the counts are absent because nobody could look:\n%s", out)
	}
}

// Every listed series appears once the store was read, so a gate cannot silently
// swallow one.
func TestAReadStoreEmitsEveryRestorePointSeries(t *testing.T) {
	out := writeAndRead(t, RunMetric{
		VM: "web01", TargetVM: "web01",
		RetentionAsked: true, RestorePointsListed: true,
		RestorePointOutcome: RestorePointTaken,
		RestorePoints:       24,
	})
	for _, name := range append(append([]string{}, restorePointAlwaysFamilies...), restorePointListedFamilies...) {
		if !hasFamily(out, name) {
			t.Errorf("%s is missing from a run that read its store", name)
		}
	}
}

// Exactly one outcome series is 1, and all four exist, on every run.
//
// Both halves matter. A missing series cannot be alerted on over a window that
// began before it appeared; two at 1 would make "taken" and "failed" true at
// once. The label set is what buys the separation a 0/1 gauge cannot: not_due is
// the ordinary state of a healthy pair, failed means the recovery history has
// stopped growing, and an alert must fire on the second and never on the first.
func TestExactlyOneRestorePointOutcomeIsSet(t *testing.T) {
	all := []string{RestorePointTaken, RestorePointNotDue, RestorePointFailed, RestorePointUndetermined}
	for _, want := range all {
		t.Run(want, func(t *testing.T) {
			out := writeAndRead(t, RunMetric{
				VM: "web01", TargetVM: "web01",
				RetentionAsked:      true,
				RestorePointOutcome: want,
			})
			ones, zeros := 0, 0
			for _, o := range all {
				line := fmt.Sprintf(`vmsync_restore_point_outcome{source_host="",target_host="",vm="web01",target_vm="web01",outcome=%q} `, o)
				switch {
				case strings.Contains(out, line+"1"):
					ones++
					if o != want {
						t.Errorf("outcome %q is 1, want %q", o, want)
					}
				case strings.Contains(out, line+"0"):
					zeros++
				default:
					t.Errorf("outcome %q has no series at all, so an alert on it can never fire", o)
				}
			}
			if ones != 1 || zeros != 3 {
				t.Errorf("%d series at 1 and %d at 0, want exactly one and three", ones, zeros)
			}
		})
	}

	// An empty outcome renders as undetermined rather than as a fifth label
	// value nothing alerts on.
	out := writeAndRead(t, RunMetric{VM: "web01", RetentionAsked: true})
	if !strings.Contains(out, `outcome="undetermined"} 1`) {
		t.Errorf("a run with no outcome recorded did not fall back to undetermined:\n%s", out)
	}
}

// The restore-point families carry target_vm, and the pre-existing families do
// not gain it.
//
// The run-level series are labelled by vm, which is the SOURCE domain. A restore
// point store is addressed by the TARGET domain, so labelling the two alike would
// make a series claim to describe a directory it does not name -- and after an
// inversion the two differ. Adding the label to the OLD families would instead
// break every dashboard and alert already written against them.
func TestRestorePointSeriesAreLabelledByTargetDomain(t *testing.T) {
	out := writeAndRead(t, RunMetric{
		VM: "web01", TargetVM: "web01-dr",
		RetentionAsked: true, RestorePointsListed: true,
		RestorePoints: 7,
	})
	if !strings.Contains(out, `vmsync_restore_points{source_host="",target_host="",vm="web01",target_vm="web01-dr"} 7`) {
		t.Errorf("the count is not labelled by the target domain that names its store:\n%s", out)
	}
	if !strings.Contains(out, `vmsync_sync_state{source_host="",target_host="",vm="web01"} 0`) {
		t.Errorf("vmsync_sync_state gained or lost a label:\n%s", out)
	}
}

// One HELP and one TYPE per family, for the whole file.
//
// node_exporter's textfile collector rejects the ENTIRE file when a family
// declares either twice -- so a second HELP line takes down every metric vmsync
// publishes, including the one being alerted on, while the file still looks fine
// to anyone reading it by eye. The agent's own test has checked this for its file
// since it was written; this is the engine's, now that the engine emits a family
// with more than one sample line.
func TestEngineTextfileDeclaresEachFamilyOnce(t *testing.T) {
	out := writeAndRead(t, RunMetric{
		VM: "web01", TargetVM: "web01",
		RetentionAsked: true, RestorePointsListed: true,
		VerificationRan: true, ChecksumRan: true,
		SourceBridgeReceivedBytes: 1, SourceBridgeSentBytes: 2,
		RestorePoints: 3,
	})
	helps, types := map[string]int{}, map[string]int{}
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "# HELP "):
			helps[strings.Fields(line)[2]]++
		case strings.HasPrefix(line, "# TYPE "):
			types[strings.Fields(line)[2]]++
		}
	}
	if len(helps) == 0 {
		t.Fatal("no HELP lines at all")
	}
	for name, n := range helps {
		if n != 1 {
			t.Errorf("%s declares HELP %d times; node_exporter would reject the whole file and every series in it would vanish", name, n)
		}
	}
	for name, n := range types {
		if n != 1 {
			t.Errorf("%s declares TYPE %d times", name, n)
		}
	}
}

// Overdue is published even when it is zero, which is the normal case.
//
// It is the series the starvation alert fires on directly, so it must exist on
// every run -- including the great majority that take no restore point because
// the interval floor has not elapsed. Emitting it only when positive would mean
// the alert could not use a `for:` clause at all.
func TestOverdueIsPublishedAtZero(t *testing.T) {
	out := writeAndRead(t, RunMetric{
		VM: "web01", TargetVM: "web01",
		RetentionAsked: true, RestorePointsListed: true,
		RestorePointOutcome:        RestorePointNotDue,
		RestorePointOverdueSeconds: 0,
	})
	if !strings.Contains(out, `vmsync_restore_point_overdue_seconds{source_host="",target_host="",vm="web01",target_vm="web01"} 0.000`) {
		t.Errorf("overdue is not published at zero, so the starvation alert has no series to sit on between failures:\n%s", out)
	}

	out = writeAndRead(t, RunMetric{
		VM: "web01", TargetVM: "web01",
		RetentionAsked: true, RestorePointsListed: true,
		RestorePointOutcome:        RestorePointFailed,
		RestorePointOverdueSeconds: 3600,
	})
	if !strings.Contains(out, `vmsync_restore_point_overdue_seconds{source_host="",target_host="",vm="web01",target_vm="web01"} 3600.000`) {
		t.Errorf("overdue did not render the seconds it was given:\n%s", out)
	}
}
