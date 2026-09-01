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

// Package metrics renders per-disk sync results and the overall run result
// as a Prometheus textfile collector file (see -prometheus-textfile in
// cmd/vmsync).
package metrics

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// StateSuccess, StateFailure, and StateFSFreezeFailed are the values
// RunMetric.State takes. StateFSFreezeFailed is a degraded-but-not-failed
// outcome: the sync itself completed, but the guest filesystem couldn't be
// quiesced first (no/unresponsive guest agent, guest doesn't support it,
// ...), so the resulting checkpoint is only crash-consistent, not
// application-consistent -- worth alerting on separately from a clean
// success, but not worth failing the whole run over, since vmsync already
// tolerates a failed freeze and proceeds (see cmd/vmsync's own handling).
const (
	StateSuccess        = 0
	StateFailure        = 1
	StateFSFreezeFailed = 2
)

// DiskMetric holds one disk's sync result, ready to be rendered into the
// Prometheus text exposition format for a node_exporter textfile collector.
type DiskMetric struct {
	SourceHost       string
	TargetHost       string
	VM               string
	Disk             string
	DiskSizeBytes    uint64
	TransferredBytes uint64
	// CompressedTransferredBytes is the actual bytes that crossed the
	// network writing this disk to the target. It equals TransferredBytes
	// whenever neither --compress nor --netbuffer bridged that leg, since
	// then nothing sat between the plain NBD read and write.
	//
	// The TARGET leg only. The source-side bridge is shared by every disk in
	// a run, so it cannot be attributed to one and is reported on RunMetric
	// instead -- see SourceBridgeReceivedBytes. This series is therefore
	// safe to sum across disks, which is the whole reason for the split.
	CompressedTransferredBytes uint64
	DurationSeconds            float64
}

// RunMetric holds the overall result of one vmsync invocation -- unlike
// DiskMetric, this is not per-disk: a single sync either succeeded or
// failed as a whole, regardless of which (if any) individual disk caused a
// failure.
type RunMetric struct {
	SourceHost string
	TargetHost string
	VM         string
	State      int
	// Timestamp is the Unix time (seconds) this run finished, for staleness
	// detection (e.g. "time() - vmsync_last_run_timestamp_seconds > 86400"
	// in an alert rule or dashboard panel) -- the textfile itself doesn't
	// carry a reliable "last written" signal node_exporter exposes on its
	// own, so vmsync has to report it itself.
	Timestamp int64
	// SourceBridgeReceivedBytes and SourceBridgeSentBytes are the wire bytes
	// on the SOURCE-side compression bridge, which exists only when the
	// source is reached over qemu+ssh.
	//
	// Run-level rather than per-disk because the bridge is: one shared
	// libvirt backup export, one listener, one counter for every disk's
	// connection. Folding it into DiskMetric -- which is what this replaced
	// -- made summing the per-disk series count it once per disk, and no
	// per-disk delta can fix that while the disks sync concurrently through
	// it.
	//
	// Received is the payload direction here, and that is not a detail. The
	// bridge's Sent counts the outbound leg, which on the source side is the
	// local NBD client's REQUESTS; the disk data comes back inbound. Reading
	// Sent -- which this used to do -- measured the command stream and
	// reported it as transferred data.
	SourceBridgeReceivedBytes uint64
	SourceBridgeSentBytes     uint64
	// FSFreezeFailed is true when the guest filesystems could not be
	// quiesced, so this copy is only crash-consistent.
	//
	// This duplicates StateFSFreezeFailed on purpose, and is the signal to
	// alert on. State is an enum, so its values are mutually exclusive and
	// StateFailure wins: a run that could not freeze AND then failed reports
	// only the failure, and the crash-consistency of what it did copy is
	// lost. That is precisely the run where it matters most.
	FSFreezeFailed bool
	// FSThawFailed is true when the source guest was left with its
	// filesystems FROZEN because the thaw did not take.
	//
	// Its own signal rather than a State value, because it is orthogonal to
	// whether the sync worked: the copy can be perfect and the source still
	// be hung. Folding it into State would force a choice between reporting
	// the sync's outcome and reporting the guest's, and the guest matters
	// more.
	FSThawFailed bool
	// ExternalSnapshotCount is how many external disk snapshots were present
	// on the source domain during this run (see
	// libvirtsync.ExternalSnapshotCount). Libvirt refuses to create a new
	// checkpoint while any exist, so a run can be syncing correctly (see
	// libvirtsync.IsCheckpointBlockedBySnapshot's fallback) while its
	// checkpoint chain sits stalled -- this metric is what explains that
	// from the outside, purely diagnostic.
	ExternalSnapshotCount int
	// WarningCount/ErrorCount are how many trace.Warning/trace.Error calls
	// this run made (from trace.WarningCount()/trace.ErrorCount()) -- a
	// coarse, always-available "did anything degrade along the way" signal
	// to alert on, independent of and finer-grained than State: a run can
	// finish as StateSuccess while still having logged, then transparently
	// recovered from, one or more warnings (a reconnect fallback kicking
	// in, a self-heal cleaning up leftover state from a prior crash, ...)
	// that State alone would never surface.
	WarningCount uint64
	ErrorCount   uint64
	// VerificationRan is true when this run had -verify set. It gates
	// whether VerificationState/VerificationTimestamp are rendered at all --
	// a run that never verified anything must not emit a bare
	// vmsync_verification_state 0, which would be indistinguishable from
	// "verified and passed."
	VerificationRan bool
	// VerificationState mirrors State: a -verify run fails as a whole (see
	// cmd/vmsync's per-disk compare) on any mismatch, so this needs no
	// tracking of its own beyond the overall run outcome.
	VerificationState int
	// VerificationTimestamp is the Unix time (seconds) this run's
	// verification finished, success or failure -- same staleness-detection
	// purpose as Timestamp, but specific to when a disk was last actually
	// byte-compared against its source, not just synced.
	VerificationTimestamp int64
}

// WriteTextfile renders disks and run in the Prometheus text exposition
// format and writes them to path, atomically (write to a temp file in the
// same directory, then rename) so a node_exporter textfile collector
// scanning that directory concurrently never observes a partially written
// file.
//
// Each call overwrites path with exactly the metrics passed in -- there is
// no merge with whatever the file already contained. If multiple domains
// share the same textfile path across separate vmsync invocations, only the
// most recent invocation's disks will be represented; point -prometheus-textfile
// at a distinct path per domain (e.g. one per cron job/timer) to avoid that.
func WriteTextfile(path string, disks []DiskMetric, run RunMetric) error {
	var b strings.Builder

	fmt.Fprintln(&b, "# HELP vmsync_disk_size_bytes Virtual size of the disk being synced, in bytes.")
	fmt.Fprintln(&b, "# TYPE vmsync_disk_size_bytes gauge")
	for _, m := range disks {
		fmt.Fprintf(&b, "vmsync_disk_size_bytes{source_host=%q,target_host=%q,vm=%q,disk=%q} %d\n",
			m.SourceHost, m.TargetHost, m.VM, m.Disk, m.DiskSizeBytes)
	}

	fmt.Fprintln(&b, "# HELP vmsync_transferred_bytes Logical bytes actually copied from source to target in this sync.")
	fmt.Fprintln(&b, "# TYPE vmsync_transferred_bytes gauge")
	for _, m := range disks {
		fmt.Fprintf(&b, "vmsync_transferred_bytes{source_host=%q,target_host=%q,vm=%q,disk=%q} %d\n",
			m.SourceHost, m.TargetHost, m.VM, m.Disk, m.TransferredBytes)
	}

	fmt.Fprintln(&b, "# HELP vmsync_compressed_transferred_bytes Bytes actually sent over the network for this sync. Equal to vmsync_transferred_bytes when neither --compress nor --netbuffer bridged that leg.")
	fmt.Fprintln(&b, "# TYPE vmsync_compressed_transferred_bytes gauge")
	for _, m := range disks {
		fmt.Fprintf(&b, "vmsync_compressed_transferred_bytes{source_host=%q,target_host=%q,vm=%q,disk=%q} %d\n",
			m.SourceHost, m.TargetHost, m.VM, m.Disk, m.CompressedTransferredBytes)
	}

	fmt.Fprintln(&b, "# HELP vmsync_sync_duration_seconds Duration of the sync for this disk, in seconds.")
	fmt.Fprintln(&b, "# TYPE vmsync_sync_duration_seconds gauge")
	for _, m := range disks {
		fmt.Fprintf(&b, "vmsync_sync_duration_seconds{source_host=%q,target_host=%q,vm=%q,disk=%q} %.3f\n",
			m.SourceHost, m.TargetHost, m.VM, m.Disk, m.DurationSeconds)
	}

	fmt.Fprintln(&b, "# HELP vmsync_sync_state Result of the last vmsync run as a whole (0=success, 1=failure, 2=succeeded but guest filesystem freeze failed -- checkpoint is only crash-consistent, not application-consistent).")
	fmt.Fprintln(&b, "# TYPE vmsync_sync_state gauge")
	fmt.Fprintf(&b, "vmsync_sync_state{source_host=%q,target_host=%q,vm=%q} %d\n",
		run.SourceHost, run.TargetHost, run.VM, run.State)

	fmt.Fprintln(&b, "# HELP vmsync_last_run_timestamp_seconds Unix time (seconds) this vmsync run finished, success or failure.")
	fmt.Fprintln(&b, "# TYPE vmsync_last_run_timestamp_seconds gauge")
	fmt.Fprintf(&b, "vmsync_last_run_timestamp_seconds{source_host=%q,target_host=%q,vm=%q} %d\n",
		run.SourceHost, run.TargetHost, run.VM, run.Timestamp)

	fmt.Fprintln(&b, "# HELP vmsync_external_snapshot_count Number of external disk snapshots existing on the source domain. Libvirt blocks new checkpoint creation while any exist; incremental syncs still succeed against the existing checkpoint, but the checkpoint chain won't advance until it's zero again.")
	fmt.Fprintln(&b, "# TYPE vmsync_external_snapshot_count gauge")
	fmt.Fprintf(&b, "vmsync_external_snapshot_count{source_host=%q,target_host=%q,vm=%q} %d\n",
		run.SourceHost, run.TargetHost, run.VM, run.ExternalSnapshotCount)

	// Only when a source bridge was actually in play. Unlike the quiescing
	// gauges below, a zero here would be a lie rather than a useful
	// baseline: no bridge means no wire on that side at all, which is a
	// different thing from a bridge that carried nothing.
	if run.SourceBridgeReceivedBytes > 0 || run.SourceBridgeSentBytes > 0 {
		fmt.Fprintln(&b, "# HELP vmsync_source_bridge_wire_bytes Bytes over the source-side compression bridge, present only when the source is reached over qemu+ssh. Run-level, not per-disk: one bridge serves every disk in the run, so this must NOT be added to vmsync_compressed_transferred_bytes. direction=\"received\" is the disk payload being read from the source; direction=\"sent\" is the NBD request stream going the other way.")
		fmt.Fprintln(&b, "# TYPE vmsync_source_bridge_wire_bytes gauge")
		fmt.Fprintf(&b, "vmsync_source_bridge_wire_bytes{source_host=%q,target_host=%q,vm=%q,direction=\"received\"} %d\n",
			run.SourceHost, run.TargetHost, run.VM, run.SourceBridgeReceivedBytes)
		fmt.Fprintf(&b, "vmsync_source_bridge_wire_bytes{source_host=%q,target_host=%q,vm=%q,direction=\"sent\"} %d\n",
			run.SourceHost, run.TargetHost, run.VM, run.SourceBridgeSentBytes)
	}

	// Both freeze and thaw are emitted unconditionally, including as 0, so
	// the series exist from the first run and an alert can use them before
	// anything has ever gone wrong. A metric that only appears once the bad
	// thing happens cannot be alerted on over a window that starts before it.
	fmt.Fprintln(&b, "# HELP vmsync_fsfreeze_failed 1 when the guest filesystems could not be quiesced, so this copy is only crash-consistent rather than application-consistent. Alert on this rather than on vmsync_sync_state=2: state is an enum and a failure outranks it, so a run that could not freeze and then failed reports only the failure.")
	fmt.Fprintln(&b, "# TYPE vmsync_fsfreeze_failed gauge")
	fmt.Fprintf(&b, "vmsync_fsfreeze_failed{source_host=%q,target_host=%q,vm=%q} %d\n",
		run.SourceHost, run.TargetHost, run.VM, boolMetric(run.FSFreezeFailed))

	fmt.Fprintln(&b, "# HELP vmsync_fsthaw_failed 1 when the source guest was left with its filesystems FROZEN because the thaw did not take. The guest blocks on every write until somebody runs virsh domfsthaw against it. Independent of vmsync_sync_state: the copy can be perfect and the source still be hung.")
	fmt.Fprintln(&b, "# TYPE vmsync_fsthaw_failed gauge")
	fmt.Fprintf(&b, "vmsync_fsthaw_failed{source_host=%q,target_host=%q,vm=%q} %d\n",
		run.SourceHost, run.TargetHost, run.VM, boolMetric(run.FSThawFailed))

	fmt.Fprintln(&b, "# HELP vmsync_warning_count Number of WARNING-level log lines emitted during this run.")
	fmt.Fprintln(&b, "# TYPE vmsync_warning_count gauge")
	fmt.Fprintf(&b, "vmsync_warning_count{source_host=%q,target_host=%q,vm=%q} %d\n",
		run.SourceHost, run.TargetHost, run.VM, run.WarningCount)

	fmt.Fprintln(&b, "# HELP vmsync_error_count Number of ERROR-level log lines emitted during this run.")
	fmt.Fprintln(&b, "# TYPE vmsync_error_count gauge")
	fmt.Fprintf(&b, "vmsync_error_count{source_host=%q,target_host=%q,vm=%q} %d\n",
		run.SourceHost, run.TargetHost, run.VM, run.ErrorCount)

	// Only emitted for a run that actually had -verify set -- see
	// RunMetric.VerificationRan's own comment for why a run that never
	// verified anything must not emit these at all, rather than a
	// misleadingly-successful-looking 0.
	if run.VerificationRan {
		fmt.Fprintln(&b, "# HELP vmsync_verification_state Mirrors vmsync_sync_state for the same run (0=success, 1=failure, 2=succeeded but guest filesystem freeze failed). Only present for runs that had -verify set.")
		fmt.Fprintln(&b, "# TYPE vmsync_verification_state gauge")
		fmt.Fprintf(&b, "vmsync_verification_state{source_host=%q,target_host=%q,vm=%q} %d\n",
			run.SourceHost, run.TargetHost, run.VM, run.VerificationState)

		fmt.Fprintln(&b, "# HELP vmsync_verification_timestamp_seconds Unix time (seconds) this vmsync run last performed -verify, success or failure. Only present for runs that had -verify set.")
		fmt.Fprintln(&b, "# TYPE vmsync_verification_timestamp_seconds gauge")
		fmt.Fprintf(&b, "vmsync_verification_timestamp_seconds{source_host=%q,target_host=%q,vm=%q} %d\n",
			run.SourceHost, run.TargetHost, run.VM, run.VerificationTimestamp)
	}

	return writeAtomic(path, b.String())
}

// writeAtomic writes content to a temp file next to path and renames it into
// place, so a reader (node_exporter's textfile collector) never sees a
// partially written file. The temp file is made world-readable (0644)
// since it is typically read by a different user/process than the one
// running vmsync.
func writeAtomic(path, content string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".vmsync-metrics-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp metrics file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the rename below succeeds

	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp metrics file: %w", err)
	}
	if err := tmp.Chmod(0644); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp metrics file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp metrics file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename temp metrics file to %s: %w", path, err)
	}
	return nil
}

// boolMetric renders a boolean as prometheus wants it.
func boolMetric(b bool) int {
	if b {
		return 1
	}
	return 0
}
