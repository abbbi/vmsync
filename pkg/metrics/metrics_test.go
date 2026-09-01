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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeAndRead(t *testing.T, run RunMetric) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "vmsync.prom")
	if err := WriteTextfile(p, nil, run); err != nil {
		t.Fatalf("WriteTextfile: %v", err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	return string(b)
}

// sampleFor returns the value of the named metric, and whether the series
// was emitted at all. Those are different answers: a series that is absent
// cannot be alerted on over a window that starts before the incident, which
// is the whole reason the quiescing gauges are unconditional.
func sampleFor(text, name string) (string, bool) {
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, name+"{") {
			continue
		}
		if i := strings.LastIndexByte(line, ' '); i >= 0 {
			return line[i+1:], true
		}
	}
	return "", false
}

// TestQuiescingGaugesAreAlwaysEmitted covers the reason these exist as
// gauges rather than as State values.
func TestQuiescingGaugesAreAlwaysEmitted(t *testing.T) {
	for _, name := range []string{"vmsync_fsfreeze_failed", "vmsync_fsthaw_failed"} {
		t.Run(name, func(t *testing.T) {
			clean := writeAndRead(t, RunMetric{VM: "web01", State: StateSuccess})
			got, ok := sampleFor(clean, name)
			if !ok {
				t.Fatalf("%s is missing from a clean run; an alert cannot use a series "+
					"that only appears once the bad thing has already happened", name)
			}
			if got != "0" {
				t.Errorf("%s on a clean run = %s, want 0", name, got)
			}
		})
	}
}

// TestFreezeFailureSurvivesAFailedRun is the bug that made the separate
// gauge necessary.
//
// State is an enum, so StateFailure and StateFSFreezeFailed are mutually
// exclusive and the failure wins. Before vmsync_fsfreeze_failed existed, a
// run that could not quiesce the guest AND then failed reported state=1 and
// nothing at all said the partial copy it left behind was crash-consistent
// -- exactly the run where that matters most.
func TestFreezeFailureSurvivesAFailedRun(t *testing.T) {
	text := writeAndRead(t, RunMetric{
		VM:             "db01",
		State:          StateFailure,
		FSFreezeFailed: true,
	})

	if got, _ := sampleFor(text, "vmsync_sync_state"); got != "1" {
		t.Errorf("vmsync_sync_state = %s, want 1 (the failure still outranks the degradation)", got)
	}
	if got, ok := sampleFor(text, "vmsync_fsfreeze_failed"); !ok || got != "1" {
		t.Errorf("vmsync_fsfreeze_failed = %q (present=%v), want 1: the freeze failure "+
			"must not be shadowed by the run's own failure", got, ok)
	}
}

// TestThawFailureIsIndependentOfSuccess is the mirror image, and the worse
// half: a perfect copy whose source guest is still frozen and blocking on
// every write.
func TestThawFailureIsIndependentOfSuccess(t *testing.T) {
	text := writeAndRead(t, RunMetric{
		VM:           "db01",
		State:        StateSuccess,
		FSThawFailed: true,
	})

	if got, _ := sampleFor(text, "vmsync_sync_state"); got != "0" {
		t.Errorf("vmsync_sync_state = %s, want 0: the sync itself did succeed", got)
	}
	if got, ok := sampleFor(text, "vmsync_fsthaw_failed"); !ok || got != "1" {
		t.Errorf("vmsync_fsthaw_failed = %q (present=%v), want 1", got, ok)
	}
}

// TestFreezeAndThawAreSeparateSignals guards the two from ever being folded
// back into one "quiescing went wrong" flag. They call for different
// actions: a failed freeze is a copy to distrust, a failed thaw is a
// production guest to go and unblock right now.
func TestFreezeAndThawAreSeparateSignals(t *testing.T) {
	text := writeAndRead(t, RunMetric{
		VM:             "db01",
		State:          StateFSFreezeFailed,
		FSFreezeFailed: true,
	})
	if got, _ := sampleFor(text, "vmsync_fsfreeze_failed"); got != "1" {
		t.Errorf("vmsync_fsfreeze_failed = %s, want 1", got)
	}
	if got, _ := sampleFor(text, "vmsync_fsthaw_failed"); got != "0" {
		t.Errorf("vmsync_fsthaw_failed = %s, want 0: a guest that was never frozen "+
			"cannot have been left frozen", got)
	}
}

// TestPerDiskCompressedBytesAreSummable is the contract the source/target
// split exists to create.
//
// CompressedTransferredBytes used to include the SOURCE bridge, which is one
// shared listener for the whole run. Adding a run-wide total to every disk
// meant the obvious query -- sum by (vm) -- counted it once per disk, and
// per-disk compression ratios were nonsense. This asserts the per-disk
// series now carries only what is genuinely per-disk.
func TestPerDiskCompressedBytesAreSummable(t *testing.T) {
	disks := []DiskMetric{
		{VM: "db01", Disk: "vda", TransferredBytes: 1000, CompressedTransferredBytes: 300},
		{VM: "db01", Disk: "vdb", TransferredBytes: 2000, CompressedTransferredBytes: 600},
	}
	run := RunMetric{
		VM: "db01", State: StateSuccess,
		SourceBridgeReceivedBytes: 5000,
		SourceBridgeSentBytes:     40,
	}

	p := filepath.Join(t.TempDir(), "vmsync.prom")
	if err := WriteTextfile(p, disks, run); err != nil {
		t.Fatalf("WriteTextfile: %v", err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)

	// The per-disk series must total exactly the two disks' own values. If
	// the source bridge leaked back in, this is 300+5000 and 600+5000.
	var total uint64
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "vmsync_compressed_transferred_bytes{") {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		if i < 0 {
			t.Fatalf("unparsable sample: %q", line)
		}
		var v uint64
		if _, err := fmt.Sscanf(line[i+1:], "%d", &v); err != nil {
			t.Fatalf("unparsable value in %q: %v", line, err)
		}
		total += v
	}
	if total != 900 {
		t.Errorf("sum of per-disk compressed bytes = %d, want 900 -- a run-wide "+
			"total is being added to every disk again", total)
	}
}

// TestSourceBridgeIsReportedPerRunAndPerDirection pins the other half: the
// shared leg is still reported, just not per disk, and the payload direction
// is labelled so nobody has to guess which of the two is the disk data.
func TestSourceBridgeIsReportedPerRunAndPerDirection(t *testing.T) {
	text := writeAndRead(t, RunMetric{
		VM: "db01", State: StateSuccess,
		SourceBridgeReceivedBytes: 5000,
		SourceBridgeSentBytes:     40,
	})

	for _, want := range []string{
		`direction="received"} 5000`,
		`direction="sent"} 40`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %s from:\n%s", want, text)
		}
	}

	// The two directions must stay DISTINGUISHABLE, which is the whole point
	// of the label: on the source side the payload arrives inbound while
	// Sent is only the NBD request stream, and reading Sent as the payload
	// was the original bug. A single unlabelled series would let that
	// mistake back in silently.
	if strings.Count(text, "vmsync_source_bridge_wire_bytes{") != 2 {
		t.Errorf("want exactly two labelled samples, got:\n%s", text)
	}
}

// A run with no source bridge must emit no series at all, rather than zeros.
// A zero would read as "the bridge carried nothing", which is a different
// claim from "there was no bridge on that side".
func TestNoSourceBridgeEmitsNoSeries(t *testing.T) {
	text := writeAndRead(t, RunMetric{VM: "db01", State: StateSuccess})
	if strings.Contains(text, "vmsync_source_bridge_wire_bytes") {
		t.Errorf("a run with no source bridge still emitted the series:\n%s", text)
	}
}
