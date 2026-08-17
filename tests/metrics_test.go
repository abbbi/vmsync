/*
	Copyright (C) 2026  Michael Ablassmeier <abi@grinser.de>

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

package tests

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vmsync/pkg/metrics"
)

// readMetricsFile reads path and fails the test immediately if that's not
// possible, so every test below can just call this and move on to
// assertions against the content.
func readMetricsFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

// TestWriteTextfileNoVerification is the most important test in this file:
// it pins down that a run with VerificationRan == false -- the plain
// sync/-reinit case -- never emits vmsync_verification_state or
// vmsync_verification_timestamp_seconds, in any run outcome (success,
// failure, or the degraded freeze-failed state), while the always-on
// run-level metrics are still present and correct.
func TestWriteTextfileNoVerification(t *testing.T) {
	states := []int{metrics.StateSuccess, metrics.StateFailure, metrics.StateFSFreezeFailed}

	for _, state := range states {
		state := state
		t.Run(fmt.Sprintf("state=%d", state), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "metrics.prom")
			run := metrics.RunMetric{
				SourceHost:            "src.example.com",
				TargetHost:            "tgt.example.com",
				VM:                    "vm1",
				State:                 state,
				Timestamp:             1700000000,
				ExternalSnapshotCount: 2,
				VerificationRan:       false,
				VerificationState:     99,  // must not leak into output at all
				VerificationTimestamp: 123, // must not leak into output at all
			}

			if err := metrics.WriteTextfile(path, nil, run); err != nil {
				t.Fatalf("WriteTextfile returned unexpected error: %v", err)
			}

			content := readMetricsFile(t, path)

			if strings.Contains(content, "vmsync_verification_state") {
				t.Error("content contains \"vmsync_verification_state\" though VerificationRan is false")
			}
			if strings.Contains(content, "vmsync_verification_timestamp_seconds") {
				t.Error("content contains \"vmsync_verification_timestamp_seconds\" though VerificationRan is false")
			}

			wantSyncState := fmt.Sprintf(`vmsync_sync_state{source_host="src.example.com",target_host="tgt.example.com",vm="vm1"} %d`, state)
			if !strings.Contains(content, wantSyncState) {
				t.Errorf("content missing %q\ngot:\n%s", wantSyncState, content)
			}

			wantTimestamp := `vmsync_last_run_timestamp_seconds{source_host="src.example.com",target_host="tgt.example.com",vm="vm1"} 1700000000`
			if !strings.Contains(content, wantTimestamp) {
				t.Errorf("content missing %q\ngot:\n%s", wantTimestamp, content)
			}

			wantSnapshotCount := `vmsync_external_snapshot_count{source_host="src.example.com",target_host="tgt.example.com",vm="vm1"} 2`
			if !strings.Contains(content, wantSnapshotCount) {
				t.Errorf("content missing %q\ngot:\n%s", wantSnapshotCount, content)
			}

			wantWarningCount := `vmsync_warning_count{source_host="src.example.com",target_host="tgt.example.com",vm="vm1"} 0`
			if !strings.Contains(content, wantWarningCount) {
				t.Errorf("content missing %q\ngot:\n%s", wantWarningCount, content)
			}

			wantErrorCount := `vmsync_error_count{source_host="src.example.com",target_host="tgt.example.com",vm="vm1"} 0`
			if !strings.Contains(content, wantErrorCount) {
				t.Errorf("content missing %q\ngot:\n%s", wantErrorCount, content)
			}
		})
	}
}

// TestWriteTextfileWarningErrorCounts confirms WarningCount/ErrorCount are
// rendered with their exact values, independent of VerificationRan/State --
// these mirror ExternalSnapshotCount as always-on run-level metrics (see
// RunMetric.WarningCount's own comment for why they're tracked separately
// from State).
func TestWriteTextfileWarningErrorCounts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.prom")
	run := metrics.RunMetric{
		SourceHost:   "src.example.com",
		TargetHost:   "tgt.example.com",
		VM:           "vm1",
		State:        metrics.StateSuccess,
		Timestamp:    1700000000,
		WarningCount: 3,
		ErrorCount:   5,
	}

	if err := metrics.WriteTextfile(path, nil, run); err != nil {
		t.Fatalf("WriteTextfile returned unexpected error: %v", err)
	}

	content := readMetricsFile(t, path)

	wantWarning := `vmsync_warning_count{source_host="src.example.com",target_host="tgt.example.com",vm="vm1"} 3`
	if !strings.Contains(content, wantWarning) {
		t.Errorf("content missing %q\ngot:\n%s", wantWarning, content)
	}

	wantError := `vmsync_error_count{source_host="src.example.com",target_host="tgt.example.com",vm="vm1"} 5`
	if !strings.Contains(content, wantError) {
		t.Errorf("content missing %q\ngot:\n%s", wantError, content)
	}
}

// TestWriteTextfileWithVerification confirms that when VerificationRan is
// true, both verification series are emitted with the exact
// VerificationState/VerificationTimestamp values passed in.
func TestWriteTextfileWithVerification(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.prom")
	run := metrics.RunMetric{
		SourceHost:            "src.example.com",
		TargetHost:            "tgt.example.com",
		VM:                    "vm1",
		State:                 metrics.StateSuccess,
		Timestamp:             1700000000,
		ExternalSnapshotCount: 0,
		VerificationRan:       true,
		VerificationState:     metrics.StateFailure,
		VerificationTimestamp: 1700000555,
	}

	if err := metrics.WriteTextfile(path, nil, run); err != nil {
		t.Fatalf("WriteTextfile returned unexpected error: %v", err)
	}

	content := readMetricsFile(t, path)

	wantState := `vmsync_verification_state{source_host="src.example.com",target_host="tgt.example.com",vm="vm1"} 1`
	if !strings.Contains(content, wantState) {
		t.Errorf("content missing %q\ngot:\n%s", wantState, content)
	}

	wantTimestamp := `vmsync_verification_timestamp_seconds{source_host="src.example.com",target_host="tgt.example.com",vm="vm1"} 1700000555`
	if !strings.Contains(content, wantTimestamp) {
		t.Errorf("content missing %q\ngot:\n%s", wantTimestamp, content)
	}
}

// TestWriteTextfileEmptyDisks confirms that an empty (nil) disks slice still
// produces the four per-disk HELP/TYPE header pairs -- so a Prometheus
// scraper always sees well-formed metric declarations -- but zero actual
// series lines, since there is nothing to report.
func TestWriteTextfileEmptyDisks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.prom")
	run := metrics.RunMetric{
		SourceHost: "src.example.com",
		TargetHost: "tgt.example.com",
		VM:         "vm1",
		State:      metrics.StateSuccess,
		Timestamp:  1700000000,
	}

	if err := metrics.WriteTextfile(path, nil, run); err != nil {
		t.Fatalf("WriteTextfile returned unexpected error: %v", err)
	}

	content := readMetricsFile(t, path)

	perDiskMetrics := []string{
		"vmsync_disk_size_bytes",
		"vmsync_transferred_bytes",
		"vmsync_compressed_transferred_bytes",
		"vmsync_sync_duration_seconds",
	}
	for _, m := range perDiskMetrics {
		if !strings.Contains(content, "# HELP "+m+" ") {
			t.Errorf("content missing HELP line for %s\ngot:\n%s", m, content)
		}
		if !strings.Contains(content, "# TYPE "+m+" gauge") {
			t.Errorf("content missing TYPE line for %s\ngot:\n%s", m, content)
		}
		// A series line starts with "<metric>{" -- distinct from the HELP/TYPE
		// comment lines, which never put "{" directly after the metric name.
		if strings.Contains(content, m+"{") {
			t.Errorf("content has a series line for %s despite an empty disks slice\ngot:\n%s", m, content)
		}
	}
}

// TestWriteTextfileMultipleDisks confirms one correctly labeled series line
// per disk per per-disk metric is emitted, with the right values, in the
// same order as the input slice.
func TestWriteTextfileMultipleDisks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.prom")
	disks := []metrics.DiskMetric{
		{
			SourceHost:                 "src.example.com",
			TargetHost:                 "tgt.example.com",
			VM:                         "vmA",
			Disk:                       "vda",
			DiskSizeBytes:              1000,
			TransferredBytes:           500,
			CompressedTransferredBytes: 400,
			DurationSeconds:            12.5,
		},
		{
			SourceHost:                 "src.example.com",
			TargetHost:                 "tgt.example.com",
			VM:                         "vmA",
			Disk:                       "vdb",
			DiskSizeBytes:              2000,
			TransferredBytes:           800,
			CompressedTransferredBytes: 750,
			DurationSeconds:            20.25,
		},
		{
			SourceHost:                 "src.example.com",
			TargetHost:                 "tgt.example.com",
			VM:                         "vmA",
			Disk:                       "vdc",
			DiskSizeBytes:              3000,
			TransferredBytes:           1200,
			CompressedTransferredBytes: 1100,
			DurationSeconds:            5,
		},
	}
	run := metrics.RunMetric{
		SourceHost: "src.example.com",
		TargetHost: "tgt.example.com",
		VM:         "vmA",
		State:      metrics.StateSuccess,
		Timestamp:  1700000000,
	}

	if err := metrics.WriteTextfile(path, disks, run); err != nil {
		t.Fatalf("WriteTextfile returned unexpected error: %v", err)
	}

	content := readMetricsFile(t, path)

	type wantLine struct {
		metric string
		disk   string
		value  string
	}
	// Expected series lines, per metric, in disk-slice order.
	wants := []wantLine{
		{"vmsync_disk_size_bytes", "vda", "1000"},
		{"vmsync_disk_size_bytes", "vdb", "2000"},
		{"vmsync_disk_size_bytes", "vdc", "3000"},
		{"vmsync_transferred_bytes", "vda", "500"},
		{"vmsync_transferred_bytes", "vdb", "800"},
		{"vmsync_transferred_bytes", "vdc", "1200"},
		{"vmsync_compressed_transferred_bytes", "vda", "400"},
		{"vmsync_compressed_transferred_bytes", "vdb", "750"},
		{"vmsync_compressed_transferred_bytes", "vdc", "1100"},
		{"vmsync_sync_duration_seconds", "vda", "12.500"},
		{"vmsync_sync_duration_seconds", "vdb", "20.250"},
		{"vmsync_sync_duration_seconds", "vdc", "5.000"},
	}

	lastIdxByMetric := map[string]int{}
	for _, w := range wants {
		line := fmt.Sprintf(`%s{source_host="src.example.com",target_host="tgt.example.com",vm="vmA",disk="%s"} %s`,
			w.metric, w.disk, w.value)
		idx := strings.Index(content, line)
		if idx == -1 {
			t.Errorf("content missing series line %q\ngot:\n%s", line, content)
			continue
		}
		if prev, ok := lastIdxByMetric[w.metric]; ok && idx <= prev {
			t.Errorf("series line %q appeared out of input order (index %d <= previous %d)", line, idx, prev)
		}
		lastIdxByMetric[w.metric] = idx
	}
}

// TestWriteTextfileOverwritesNotMerges confirms each call to WriteTextfile
// fully replaces the previous file content rather than merging with it --
// documented behavior that matters when multiple domains might share a
// misconfigured textfile path.
func TestWriteTextfileOverwritesNotMerges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.prom")
	run := metrics.RunMetric{
		SourceHost: "src.example.com",
		TargetHost: "tgt.example.com",
		VM:         "vm1",
		State:      metrics.StateSuccess,
		Timestamp:  1700000000,
	}

	diskA := metrics.DiskMetric{
		SourceHost:    "src.example.com",
		TargetHost:    "tgt.example.com",
		VM:            "vm1",
		Disk:          "diskA",
		DiskSizeBytes: 111,
	}
	if err := metrics.WriteTextfile(path, []metrics.DiskMetric{diskA}, run); err != nil {
		t.Fatalf("first WriteTextfile returned unexpected error: %v", err)
	}
	first := readMetricsFile(t, path)
	if !strings.Contains(first, `disk="diskA"`) {
		t.Fatalf("first write missing diskA\ngot:\n%s", first)
	}

	diskB := metrics.DiskMetric{
		SourceHost:    "src.example.com",
		TargetHost:    "tgt.example.com",
		VM:            "vm1",
		Disk:          "diskB",
		DiskSizeBytes: 222,
	}
	if err := metrics.WriteTextfile(path, []metrics.DiskMetric{diskB}, run); err != nil {
		t.Fatalf("second WriteTextfile returned unexpected error: %v", err)
	}
	second := readMetricsFile(t, path)

	if strings.Contains(second, "diskA") {
		t.Errorf("second write still contains diskA -- WriteTextfile merged instead of overwriting\ngot:\n%s", second)
	}
	if !strings.Contains(second, `disk="diskB"`) {
		t.Errorf("second write missing diskB\ngot:\n%s", second)
	}
}

// TestWriteTextfileErrorPath confirms WriteTextfile surfaces an error
// (rather than e.g. silently creating directories) when the target path's
// parent directory doesn't exist.
func TestWriteTextfileErrorPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nonexistent-subdir", "metrics.prom")
	run := metrics.RunMetric{
		SourceHost: "src.example.com",
		TargetHost: "tgt.example.com",
		VM:         "vm1",
		State:      metrics.StateSuccess,
		Timestamp:  1700000000,
	}

	err := metrics.WriteTextfile(path, nil, run)
	if err == nil {
		t.Fatal("WriteTextfile returned nil error for a path in a nonexistent directory, want an error")
	}
}

// TestWriteTextfileLabelValueSafety is a light robustness check: label
// values containing a literal double quote and a backslash must not break
// WriteTextfile or produce an unreadable/empty file. This does not validate
// full Prometheus text-exposition escaping rules, just that vmsync doesn't
// choke on awkward guest/host/VM names.
func TestWriteTextfileLabelValueSafety(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.prom")
	disks := []metrics.DiskMetric{
		{
			SourceHost:    `vm-with-"quote`,
			TargetHost:    `back\slash`,
			VM:            `both-"-and-\`,
			Disk:          "vda",
			DiskSizeBytes: 1,
		},
	}
	run := metrics.RunMetric{
		SourceHost: `vm-with-"quote`,
		TargetHost: `back\slash`,
		VM:         `both-"-and-\`,
		State:      metrics.StateSuccess,
		Timestamp:  1700000000,
	}

	if err := metrics.WriteTextfile(path, disks, run); err != nil {
		t.Fatalf("WriteTextfile returned unexpected error for quote/backslash label values: %v", err)
	}

	content := readMetricsFile(t, path)
	if len(content) == 0 {
		t.Fatal("WriteTextfile produced an empty file for quote/backslash label values")
	}
}
