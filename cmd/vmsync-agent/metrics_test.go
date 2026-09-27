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
	"strconv"
	"strings"
	"testing"
	"time"
)

// checkPrometheusText is a strict reader of the text exposition format,
// written here because the one property that matters is not testable by
// looking for a substring: a metric family may declare HELP and TYPE only
// ONCE per file, and node_exporter's textfile collector rejects the WHOLE
// FILE when it does not -- every series this agent publishes disappears
// together, including the alert that was supposed to fire.
//
// That is not a hypothetical shape of bug. The agent's g() helper writes its
// own HELP and TYPE on every call, so any per-VM loop that calls g() breaks
// the file as soon as a host has two of whatever it is reporting -- which is
// to say, exactly when the condition being reported gets worse.
//
// It enforces, per rendered file:
//   - exactly one HELP line and one TYPE line per metric family;
//   - a TYPE that precedes every sample of its family (the expfmt parser
//     rejects "TYPE reported after samples");
//   - every sample belonging to a family that declared both;
//   - a parseable numeric value on every sample line.
func checkPrometheusText(t *testing.T, body string) {
	t.Helper()

	help := map[string]int{}
	typ := map[string]int{}
	firstSample := map[string]int{}
	typeLine := map[string]int{}

	for i, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		switch {
		case line == "":
			t.Errorf("line %d is blank; a stray newline in the middle of a family is how a file becomes unparseable", i+1)
		case strings.HasPrefix(line, "# HELP "):
			f := strings.Fields(line)
			if len(f) < 3 {
				t.Errorf("malformed HELP line: %q", line)
				continue
			}
			help[f[2]]++
		case strings.HasPrefix(line, "# TYPE "):
			f := strings.Fields(line)
			if len(f) != 4 {
				t.Errorf("malformed TYPE line: %q", line)
				continue
			}
			switch f[3] {
			case "gauge", "counter", "histogram", "summary", "untyped":
			default:
				t.Errorf("TYPE line %q names %q, which is not a metric type", line, f[3])
			}
			typ[f[2]]++
			typeLine[f[2]] = i
		case strings.HasPrefix(line, "#"):
			// A comment. Nothing else in this file emits one, but the
			// format allows it and rejecting it here would be this test
			// inventing a rule.
		default:
			name, rest, ok := strings.Cut(line, "{")
			if ok {
				_, rest, ok = strings.Cut(rest, "} ")
				if !ok {
					t.Errorf("sample line %q does not close its label set", line)
					continue
				}
			} else {
				name, rest, ok = strings.Cut(line, " ")
				if !ok {
					t.Errorf("sample line %q carries no value", line)
					continue
				}
			}
			if _, err := strconv.ParseFloat(strings.TrimSpace(rest), 64); err != nil {
				t.Errorf("sample line %q does not end in a number: %v", line, err)
			}
			if _, seen := firstSample[name]; !seen {
				firstSample[name] = i
			}
		}
	}

	for name, n := range help {
		if n != 1 {
			t.Errorf("%s declares HELP %d times; node_exporter rejects the whole file, so every other metric here disappears with it", name, n)
		}
	}
	for name, n := range typ {
		if n != 1 {
			t.Errorf("%s declares TYPE %d times; node_exporter rejects the whole file, so every other metric here disappears with it", name, n)
		}
	}
	for name, at := range firstSample {
		if typ[name] == 0 {
			t.Errorf("%s has samples but no TYPE line", name)
			continue
		}
		if help[name] == 0 {
			t.Errorf("%s has samples but no HELP line", name)
		}
		if typeLine[name] > at {
			t.Errorf("%s declares its TYPE after its first sample, which the exposition parser rejects", name)
		}
	}
}

// renderWithIncomplete renders one metrics file for a host with the given
// replicas carrying an interrupted rebuild.
func renderWithIncomplete(vms ...string) string {
	m := newAgentMetrics("test", "hyper02p", modeStandalone)
	set := map[string]bool{}
	for _, vm := range vms {
		set[vm] = true
	}
	m.setReplicaIncomplete(set)
	return m.render(CachedConfig{}, nil, 0, time.Unix(1_800_000_000, 0))
}

// TestRenderReplicaIncompleteGauge is the whole point of requirement 4: the
// gauge must be there, it must name the VM, and rendering it for several VMs
// at once must not break the file.
//
// Three is the case that matters. One affected VM hides the defect entirely,
// because a single g() call emits a single HELP; the file only becomes
// invalid on the second one, so a host with one broken replica publishes
// fine and a host with two publishes nothing at all.
func TestRenderReplicaIncompleteGauge(t *testing.T) {
	for _, tc := range []struct {
		name string
		vms  []string
	}{
		{"no affected vm", nil},
		{"one affected vm", []string{"web01"}},
		{"three affected vms", []string{"web01", "db01", "mail01"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := renderWithIncomplete(tc.vms...)
			checkPrometheusText(t, body)

			// The count is emitted even at zero: an alert cannot fire on the
			// absence of a series that exists only while things are broken.
			wantTotal := fmt.Sprintf("vmsync_agent_replica_incomplete_vms{host=\"hyper02p\"} %d\n", len(tc.vms))
			if !strings.Contains(body, wantTotal) {
				t.Errorf("rendered file does not contain %q", strings.TrimSuffix(wantTotal, "\n"))
			}

			for _, vm := range tc.vms {
				want := fmt.Sprintf("vmsync_agent_replica_incomplete{host=\"hyper02p\",vm=%q} 1\n", vm)
				if !strings.Contains(body, want) {
					t.Errorf("rendered file does not contain %q", strings.TrimSuffix(want, "\n"))
				}
			}
			if len(tc.vms) == 0 && strings.Contains(body, "vmsync_agent_replica_incomplete{") {
				t.Error("a host with no interrupted rebuild published a per-VM series anyway")
			}

			// One HELP and one TYPE for this family however many VMs there
			// are. checkPrometheusText already enforces it file-wide; said
			// again here so a failure names the family that caused it.
			if n := strings.Count(body, "# HELP vmsync_agent_replica_incomplete "); n > 1 {
				t.Errorf("vmsync_agent_replica_incomplete declares HELP %d times for %d VMs", n, len(tc.vms))
			}
			if n := strings.Count(body, "# TYPE vmsync_agent_replica_incomplete "); n > 1 {
				t.Errorf("vmsync_agent_replica_incomplete declares TYPE %d times for %d VMs", n, len(tc.vms))
			}
		})
	}
}

// The split-brain gauge is the metric this defect was first found in, and it
// is rendered by the same file, so it gets the same proof. Two split-brain
// VMs used to emit two HELP lines for one family and take the whole textfile
// down with them -- on a host that is, by definition, in the worst state
// this agent can report.
func TestRenderSplitBrainGaugeStaysParseableWithSeveralVMs(t *testing.T) {
	m := newAgentMetrics("test", "hyper02p", modeStandalone)
	m.setSplitBrain(map[string]bool{"web01": true, "db01": true, "mail01": true})
	body := m.render(CachedConfig{}, nil, 0, time.Unix(1_800_000_000, 0))

	checkPrometheusText(t, body)
	for _, vm := range []string{"web01", "db01", "mail01"} {
		want := fmt.Sprintf("vmsync_agent_split_brain{host=\"hyper02p\",vm=%q} 1\n", vm)
		if !strings.Contains(body, want) {
			t.Errorf("rendered file does not contain %q", strings.TrimSuffix(want, "\n"))
		}
	}
}

// Both conditions at once, which is not exotic: a rebuild interrupted by the
// same outage that caused the failover is exactly how a host ends up here.
func TestRenderStaysParseableWithEveryPerVMGaugeAtOnce(t *testing.T) {
	m := newAgentMetrics("test", "hyper02p", modeStandalone)
	m.setSplitBrain(map[string]bool{"web01": true, "db01": true})
	m.setReplicaIncomplete(map[string]bool{"web01": true, "db01": true, "mail01": true})
	m.runStarted("web01", time.Unix(1_799_999_000, 0))
	m.runStarted("db01", time.Unix(1_799_999_100, 0))

	checkPrometheusText(t, m.render(CachedConfig{}, nil, 0, time.Unix(1_800_000_000, 0)))
}

// setReplicaIncomplete replaces rather than merges, which is what lets the
// condition clear on its own once a sync completes and the define that
// records it removes the marker. A merge would latch the alert forever after
// one bad run, and the operator who fixed it would have no way to tell.
func TestSetReplicaIncompleteClearsWhenTheConditionDoes(t *testing.T) {
	m := newAgentMetrics("test", "hyper02p", modeStandalone)
	m.setReplicaIncomplete(map[string]bool{"web01": true, "db01": true})
	m.setReplicaIncomplete(map[string]bool{"db01": true})

	body := m.render(CachedConfig{}, nil, 0, time.Unix(1_800_000_000, 0))
	if strings.Contains(body, `vmsync_agent_replica_incomplete{host="hyper02p",vm="web01"}`) {
		t.Error("a replica whose rebuild has since completed is still reported as incomplete")
	}
	if !strings.Contains(body, `vmsync_agent_replica_incomplete{host="hyper02p",vm="db01"}`) {
		t.Error("the replica still carrying the marker stopped being reported")
	}
	if !strings.Contains(body, `vmsync_agent_replica_incomplete_vms{host="hyper02p"} 1`) {
		t.Error("the count did not follow the set")
	}

	m.setReplicaIncomplete(nil)
	body = m.render(CachedConfig{}, nil, 0, time.Unix(1_800_000_000, 0))
	if !strings.Contains(body, `vmsync_agent_replica_incomplete_vms{host="hyper02p"} 0`) {
		t.Error("an estate with nothing wrong must still publish the count, at zero, or nothing can alert on it")
	}
	checkPrometheusText(t, body)
}

// incompleteReplicaVMs reads the report the agent just sent, so the gauge
// and the console cannot describe different sweeps.
func TestIncompleteReplicaVMs(t *testing.T) {
	got := incompleteReplicaVMs([]ReportDomain{
		{Name: "web01", ReplicaIncomplete: "verb=reinit,at=1758441600,action=9f3c1a2b4d5e6f70,host=hv-a,aside=1758441600"},
		{Name: "db01"},
		// Presence is the state: a value from a newer engine that this build
		// cannot parse still means a rebuild never finished, and reading it
		// as "nothing wrong" is the failure the field exists to close.
		{Name: "mail01", ReplicaIncomplete: "something a later version writes"},
	})
	if len(got) != 2 || !got["web01"] || !got["mail01"] {
		t.Errorf("incompleteReplicaVMs = %v, want exactly web01 and mail01", got)
	}
	if got["db01"] {
		t.Error("a replica with no marker was reported as carrying an interrupted rebuild")
	}
	if n := len(incompleteReplicaVMs(nil)); n != 0 {
		t.Errorf("incompleteReplicaVMs(nil) returned %d entries", n)
	}
}

// A replica with no restore points must still produce a gauge, because the
// series has to exist before the alert window that reads it does. A series that
// appears only once a replica HAS restore points can never fire "this replica
// has none".
func TestRestorePointGaugesArePublishedAtZeroForAReplicaWithNone(t *testing.T) {
	g := restorePointGaugeFor(nil)
	if g.Count != 0 || g.NewestUnix != 0 || g.OldestUnix != 0 {
		t.Errorf("an empty store gave %+v, want zeroes", g)
	}

	// And they are published for every TARGET, including that one -- while a
	// source is left out entirely, since it has no store and a zero for one
	// would read as a starved replica on every hypervisor running production
	// VMs.
	got := restorePointGauges([]ReportDomain{
		{Name: "web01", ReplicaSource: "prod01:web01"},
		{Name: "db01", ReplicaSource: "prod01:db01", RestorePoints: []ReportRestorePoint{
			{Tag: "1756041600-vmsync-cpt-000042", TakenAtUnix: 1756041600},
		}},
		{Name: "app01"}, // a source: no replica_source
	})
	if _, ok := got["web01"]; !ok {
		t.Error("a target with no restore points was left out, so no alert could ever fire on its depth being zero")
	}
	if got["db01"].Count != 1 {
		t.Errorf("db01 Count = %d, want 1", got["db01"].Count)
	}
	if _, ok := got["app01"]; ok {
		t.Error("a source was given a restore point gauge; it has no store, and a zero for one reads as a fault")
	}
}

// renderWithRestorePoints renders one metrics file for a host holding the given
// replicas' restore point stores.
func renderWithRestorePoints(vms map[string]restorePointGauge) string {
	m := newAgentMetrics("test", "hyper02p", modeStandalone)
	m.setRestorePoints(vms)
	return m.render(CachedConfig{}, nil, 0, time.Unix(1_800_000_000, 0))
}

// Every restore-point family must declare HELP and TYPE exactly once while
// carrying one sample line per replica, and every replica must appear even at
// zero.
//
// Two separate failures are being kept out. A second HELP line for one family
// makes node_exporter reject the ENTIRE textfile -- so two replicas on one host
// would have taken down every metric this agent publishes, including the ones
// being alerted on, while the file still looked fine read by eye. And a series
// that only appears once a replica HAS restore points cannot carry a `for:`
// clause over a window beginning before it did, so the alert for "this replica
// has stopped taking them" could never fire.
func TestRenderRestorePointGauges(t *testing.T) {
	body := renderWithRestorePoints(map[string]restorePointGauge{
		"web01": {Count: 24, Unreadable: 1, VerifyFailed: 1, NewestUnix: 1756052400, OldestUnix: 1756041600},
		"db01":  {},
	})
	checkPrometheusText(t, body)

	families := []string{
		"vmsync_agent_restore_points",
		"vmsync_agent_restore_points_unreadable",
		"vmsync_agent_restore_points_verify_failed",
		"vmsync_agent_restore_point_newest_timestamp_seconds",
		"vmsync_agent_restore_point_oldest_timestamp_seconds",
	}
	for _, name := range families {
		if n := strings.Count(body, "# HELP "+name+" "); n != 1 {
			t.Errorf("%s declares HELP %d times, want exactly 1: node_exporter rejects the whole file otherwise and every series in it vanishes", name, n)
		}
		for _, vm := range []string{"web01", "db01"} {
			want := fmt.Sprintf("%s{host=%q,vm=%q} ", name, "hyper02p", vm)
			if !strings.Contains(body, want) {
				t.Errorf("%s has no sample line for %s", name, vm)
			}
		}
	}

	// The replica with nothing is published as zero, not omitted.
	if !strings.Contains(body, `vmsync_agent_restore_points{host="hyper02p",vm="db01"} 0`) {
		t.Error("a replica with no restore points was not published at zero, so no alert could ever fire on its depth")
	}
}

// A host with no replicas at all emits no restore-point families -- and the file
// stays valid, which is the thing that would break if the block were written
// with the per-metric helper instead of one HELP followed by its samples.
func TestRenderWithNoRestorePointsIsStillValid(t *testing.T) {
	body := renderWithRestorePoints(nil)
	checkPrometheusText(t, body)
	if strings.Contains(body, "vmsync_agent_restore_points") {
		t.Error("a host with no replicas published restore point series")
	}
}
