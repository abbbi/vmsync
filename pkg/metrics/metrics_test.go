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
// Including the SOURCE bridge in CompressedTransferredBytes adds a run-wide
// total -- one shared listener for the whole run -- to every disk, so the
// obvious query, sum by (vm), counts it once per disk and per-disk
// compression ratios are nonsense. This asserts the per-disk series carries
// only what is genuinely per-disk.
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

// The distinction the CheckState* values exist for: "the replica differs"
// and "the check could not run" must be different numbers.
//
// One number for both -- VerificationState mirroring State, so any failing
// verify reports 1 -- is what lets a -verify=qemu-img which cannot even open
// its export (asking for an unnamed export against a named one, and exiting
// before comparing a byte) report a mismatch on every run, and pass a bench
// stage whose whole job is detecting a tampered replica.
func TestVerificationStateSeparatesMismatchFromCouldNotRun(t *testing.T) {
	mismatch := writeAndRead(t, RunMetric{
		VM: "web01", State: StateFailure,
		VerificationRan: true, VerificationState: CheckStateMismatch,
	})
	if v, ok := sampleFor(mismatch, "vmsync_verification_state"); !ok || v != "1" {
		t.Errorf("a found difference did not render as 1:\n%s", mismatch)
	}

	couldNotRun := writeAndRead(t, RunMetric{
		VM: "web01", State: StateFailure,
		VerificationRan: true, VerificationState: CheckStateNotPerformed,
	})
	if v, ok := sampleFor(couldNotRun, "vmsync_verification_state"); !ok || v != "2" {
		t.Errorf("an unperformable check did not render as 2:\n%s", couldNotRun)
	}

	// Both runs FAILED overall, so if this series still mirrored State the
	// two would be indistinguishable. That they differ is the whole point.
	if mismatch == couldNotRun {
		t.Error("a data mismatch and a check that could not run rendered identically")
	}
}

// A passing verify on a run that later fails for some unrelated reason must
// still report that verification passed: the series says what was learned
// about the replica, not whether the process exited zero.
func TestVerificationStateIsIndependentOfRunState(t *testing.T) {
	text := writeAndRead(t, RunMetric{
		VM: "web01", State: StateFailure,
		VerificationRan: true, VerificationState: CheckStatePassed,
	})
	if v, ok := sampleFor(text, "vmsync_verification_state"); !ok || v != "0" {
		t.Errorf("a passing verify on a failed run did not render as 0:\n%s", text)
	}
	if v, _ := sampleFor(text, "vmsync_sync_state"); v != "1" {
		t.Errorf("the run state should still be 1:\n%s", text)
	}
}

// The checksum series must be emitted for a SKIPPED check.
//
// This is the state the metric exists for and the easiest one to lose: a
// helper that is missing or version-skewed turns a default-on integrity
// check off for every run on that host, while each sync still reports
// success. If "skipped" were the case that omitted the series, the only
// signal would be a log line nobody scrapes.
func TestChecksumSkippedStillEmitsTheSeries(t *testing.T) {
	text := writeAndRead(t, RunMetric{
		VM: "web01", State: StateSuccess,
		ChecksumRan: true, ChecksumState: CheckStateNotPerformed, ChecksumBytes: 0,
	})
	if v, ok := sampleFor(text, "vmsync_checksum_state"); !ok || v != "2" {
		t.Errorf("a skipped checksum did not render as 2:\n%s", text)
	}
	if v, ok := sampleFor(text, "vmsync_checksum_bytes"); !ok || v != "0" {
		t.Errorf("a skipped checksum should still report zero bytes checked:\n%s", text)
	}
}

func TestChecksumStatesRender(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state int
		bytes uint64
		want  string
	}{
		{"passed", CheckStatePassed, 131072, "0"},
		{"mismatch", CheckStateMismatch, 131072, "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := writeAndRead(t, RunMetric{
				VM: "web01", ChecksumRan: true, ChecksumState: tc.state, ChecksumBytes: tc.bytes,
			})
			if v, ok := sampleFor(text, "vmsync_checksum_state"); !ok || v != tc.want {
				t.Errorf("want %q in:\n%s", tc.want, text)
			}
			if v, _ := sampleFor(text, "vmsync_checksum_bytes"); v != "131072" {
				t.Errorf("checked bytes not reported:\n%s", text)
			}
		})
	}
}

// A run that never got as far as deciding about the check emits nothing,
// rather than a misleading zero that would read as "checked and clean".
func TestChecksumNotReachedEmitsNoSeries(t *testing.T) {
	text := writeAndRead(t, RunMetric{VM: "web01", State: StateFailure})
	for _, unwanted := range []string{"vmsync_checksum_state", "vmsync_checksum_bytes"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("%s was emitted for a run that never reached the checksum decision:\n%s", unwanted, text)
		}
	}
}

// writeAndReadDisks is writeAndRead for the per-disk series.
func writeAndReadDisks(t *testing.T, disks []DiskMetric, run RunMetric) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "vmsync.prom")
	if err := WriteTextfile(p, disks, run); err != nil {
		t.Fatalf("WriteTextfile: %v", err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	return string(b)
}

// TestVerifyPlanBytesRenderPerDisk pins the pair that makes a verify's cost
// readable after the fact: how much of each disk could not differ (and so was
// not hashed), and how much was.
//
// Raw bytes rather than a ratio, because that is the only form that
// re-aggregates: sum them across a VM's disks or across an estate and the
// division still means something, which a pre-divided percentage would not.
func TestVerifyPlanBytesRenderPerDisk(t *testing.T) {
	disks := []DiskMetric{
		{VM: "mail01", Disk: "vda", VerifySkippedBytes: 1835008, VerifyHashedBytes: 53685256192},
		{VM: "mail01", Disk: "vdb", VerifySkippedBytes: 101261639680, VerifyHashedBytes: 972480184320},
	}
	text := writeAndReadDisks(t, disks, RunMetric{
		VM: "mail01", State: StateSuccess,
		VerificationRan: true, VerificationState: CheckStatePassed,
	})

	for _, want := range []string{
		`vmsync_verify_skipped_bytes{source_host="",target_host="",vm="mail01",disk="vda"} 1835008`,
		`vmsync_verify_hashed_bytes{source_host="",target_host="",vm="mail01",disk="vda"} 53685256192`,
		`vmsync_verify_skipped_bytes{source_host="",target_host="",vm="mail01",disk="vdb"} 101261639680`,
		`vmsync_verify_hashed_bytes{source_host="",target_host="",vm="mail01",disk="vdb"} 972480184320`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing series %q in:\n%s", want, text)
		}
	}
}

// TestVerifyPlanBytesAreAbsentWithoutAVerify is the one that matters, and it
// is the same trap vmsync_verification_state exists to avoid: a run that never
// verified would otherwise emit skipped=0 hashed=0, which is indistinguishable
// from a verify that found nothing skippable. An alert on "skipped is falling
// toward zero" -- the signal that a guest has stopped trimming -- would then
// fire on every ordinary sync.
func TestVerifyPlanBytesAreAbsentWithoutAVerify(t *testing.T) {
	disks := []DiskMetric{{VM: "web01", Disk: "vda", TransferredBytes: 4096}}
	text := writeAndReadDisks(t, disks, RunMetric{VM: "web01", State: StateSuccess})

	if _, ok := sampleFor(text, "vmsync_verify_skipped_bytes"); ok {
		t.Errorf("a run that did not verify emitted vmsync_verify_skipped_bytes:\n%s", text)
	}
	if _, ok := sampleFor(text, "vmsync_verify_hashed_bytes"); ok {
		t.Errorf("a run that did not verify emitted vmsync_verify_hashed_bytes:\n%s", text)
	}
	// And the ordinary per-disk series must still be there, so this is a
	// gate on two series rather than on the whole disk block.
	if _, ok := sampleFor(text, "vmsync_transferred_bytes"); !ok {
		t.Errorf("gating the verify series suppressed the ordinary per-disk ones:\n%s", text)
	}
}

// TestVerifyPlanBytesRenderZeroWhenNothingWasSkipped separates the two
// readings of a zero: a verify that ran and found nothing skippable is a real
// answer and must be reported, because its absence is what would hide a
// fully-allocated replica whose verify costs the whole disk every time.
func TestVerifyPlanBytesRenderZeroWhenNothingWasSkipped(t *testing.T) {
	disks := []DiskMetric{{VM: "db01", Disk: "vda", VerifySkippedBytes: 0, VerifyHashedBytes: 53687091200}}
	text := writeAndReadDisks(t, disks, RunMetric{
		VM: "db01", State: StateSuccess,
		VerificationRan: true, VerificationState: CheckStatePassed,
	})
	if v, ok := sampleFor(text, "vmsync_verify_skipped_bytes"); !ok || v != "0" {
		t.Errorf("a verify that skipped nothing must still say so, got %q ok=%v:\n%s", v, ok, text)
	}
	if v, ok := sampleFor(text, "vmsync_verify_hashed_bytes"); !ok || v != "53687091200" {
		t.Errorf("hashed bytes = %q ok=%v, want the whole disk:\n%s", v, ok, text)
	}
}
