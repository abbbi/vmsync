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

// StateSuccess and StateFailure are the only two values RunMetric.State
// takes -- there is no partial/degraded "warning" state in vmsync today.
const (
	StateSuccess = 0
	StateFailure = 1
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
	// network for this disk (the sum of whichever bridge legs -- source
	// read, target write -- were active). It equals TransferredBytes
	// whenever neither --compress nor --netbuffer bridged that leg, since
	// then nothing sat between the plain NBD read and write.
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

	fmt.Fprintln(&b, "# HELP vmsync_sync_state Result of the last vmsync run as a whole (0=success, 1=failure).")
	fmt.Fprintln(&b, "# TYPE vmsync_sync_state gauge")
	fmt.Fprintf(&b, "vmsync_sync_state{source_host=%q,target_host=%q,vm=%q} %d\n",
		run.SourceHost, run.TargetHost, run.VM, run.State)

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
