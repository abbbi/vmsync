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

// CheckState* are the values VerificationState and ChecksumState take.
//
// They answer "what may an operator conclude about this replica", not "what
// went wrong" -- and that distinction is the whole point. The two outcomes
// below demand opposite responses: a difference found means act on the
// replica, a check that could not run means the replica's state is simply
// unknown and the tooling needs fixing.
//
// VerificationState used to mirror RunMetric.State, so both collapsed onto
// 1. That is not a theoretical loss. A -verify=qemu-img that could not open
// its export at all -- it was asking for an unnamed export against a named
// one, and exited before comparing a byte -- reported 1 on every run, and
// the bench stage that tampers a replica and expects a detection scored
// three consecutive PASSes on a comparator that had never compared
// anything. Only the sub-test expecting a CLEAN result could tell.
//
// Deliberately NOT split by cause. A failed SSH, a missing helper, a version
// skew, an export that would not start and a source read error are one
// conclusion, and enumerating them would mean classifying every error site
// while changing nothing an operator does. Transience is a duration, not a
// state: express it with Prometheus's own `for:` clause, which fires only
// while the condition holds continuously --
//
//	expr: vmsync_verification_state == 2
//	for:  24h
//
// -- so a blip resets on the next successful run and a real outage always
// fires, with no extra series and nothing to carry between runs.
const (
	// CheckStatePassed: the check ran and the two sides agreed.
	CheckStatePassed = 0
	// CheckStateMismatch: the check ran and found a difference. The only
	// value that says anything about the DATA.
	CheckStateMismatch = 1
	// CheckStateNotPerformed: the check could not be carried out, so this
	// replica's state is unknown. For the checksum this also covers being
	// skipped -- which is the dangerous one, because it is otherwise
	// completely silent: a helper that is missing or version-skewed turns a
	// default-on integrity check off for every run on that host while each
	// sync still reports success.
	CheckStateNotPerformed = 2
)

// RestorePointOutcome* are the values RunMetric.RestorePointOutcome takes, and
// they are published as a LABEL SET rather than as an integer enum: one series
// per outcome, exactly one of them 1, all of them present from the first run.
//
// The same shape vmsync_agent_mode uses, and for the same reason given there --
// the question has more than two answers. A bare
// vmsync_restore_point_taken 0/1 cannot separate "the interval floor had not
// elapsed" from "one was due and could not be produced", and those two need
// opposite responses: the first is the ordinary state of a healthy pair, the
// second means the recovery history has stopped growing. Collapsed onto one
// value, the alert for the second fires continuously on every pair whose syncs
// are further apart than its floor.
const (
	// RestorePointTaken: one was published by this run.
	RestorePointTaken = "taken"
	// RestorePointNotDue: the floor had not elapsed, so none was attempted.
	// The ordinary outcome, and not a degraded one.
	RestorePointNotDue = "not_due"
	// RestorePointFailed: it was due and this run did not publish one.
	RestorePointFailed = "failed"
	// RestorePointUndetermined: the run never reached the decision -- it was
	// refused earlier, or retention is off -- so nothing here says anything
	// about what the store holds.
	RestorePointUndetermined = "undetermined"
)

// restorePointOutcomes is the complete label set, written out so every series
// exists from the first run. An alert cannot use a `for:` clause over a window
// that begins before the series it reads came into existence.
var restorePointOutcomes = []string{
	RestorePointTaken, RestorePointNotDue, RestorePointFailed, RestorePointUndetermined,
}

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
	// FSThawTimedOut is true when the thaw call did not return before vmsync
	// gave up waiting, and had not reported back by the time this was
	// written.
	//
	// Its own series rather than a second reason to raise vmsync_fsthaw_failed,
	// because the two deserve different alert routing: _failed is a certainty
	// and should page, this is an unknown and should open a ticket. Merging
	// them means either paging on a guest that was merely busy, or waiting
	// out a ticket queue on one that is genuinely hung.
	FSThawTimedOut bool
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
	// VerificationState is one of the CheckState* values: did -verify find
	// a difference, or could it not run at all?
	//
	// It used to mirror State, which made it a duplicate of vmsync_sync_state
	// carrying no information of its own -- and, worse, made "the replica
	// differs from its source" indistinguishable from "the comparator could
	// not connect". See the CheckState* comment for what that cost.
	VerificationState int
	// VerificationTimestamp is the Unix time (seconds) this run's
	// verification finished, success or failure -- same staleness-detection
	// purpose as Timestamp, but specific to when a disk was last actually
	// byte-compared against its source, not just synced.
	VerificationTimestamp int64

	// ChecksumRan gates whether the checksum series are rendered at all,
	// the same way VerificationRan does for verification. Always true when
	// the pre-commit integrity check was reached, INCLUDING when it was
	// skipped -- being skipped is the state most worth exporting, so it
	// must not be the state that omits the metric.
	ChecksumRan bool
	// ChecksumState is one of the CheckState* values for the pre-commit
	// integrity check.
	ChecksumState int
	// ChecksumBytes is how many bytes this run had digested and compared.
	// Zero on a run that wrote nothing, which is ordinary for an
	// incremental against an idle source.
	ChecksumBytes uint64

	// --- restore points -------------------------------------------------
	//
	// Until these existed there was no restore-point metric at all, which is
	// how a replica came to take none for seventy-two hours with nothing to
	// alert on: the sync itself succeeded every time, so vmsync_sync_state,
	// the verification series and the agent's domain counts were all healthy
	// and none of them described the history that had stopped growing.
	//
	// TargetVM labels these families and nothing else. The existing run-level
	// series are labelled by `vm`, which is the SOURCE domain; a restore point
	// store is addressed by the TARGET domain, and labelling the two the same
	// would make a series claim to describe a directory it does not name. One
	// bounded value per run, so no cardinality growth.
	TargetVM string
	// RetentionAsked is whether -retention was set. It gates every series
	// below, because a run with no policy has looked at no store -- and a
	// vmsync_restore_points 0 from such a run is not "the store is empty", it
	// is "nobody asked". Taken from configuration before any I/O, so it is
	// known even on a run refused at the reflink probe.
	RetentionAsked bool
	// RestorePointsListed is whether this run actually read the store. False
	// when it was refused before getting there -- a filesystem without reflink
	// support, a domain with no disks -- and the counts below then mean
	// nothing and are not rendered.
	RestorePointsListed bool
	// RestorePointOutcome is one of the RestorePoint* values above.
	RestorePointOutcome string
	// RestorePointsPolicyCount and RestorePointsPolicyIntervalSeconds are what
	// -retention asked for, published beside the counts so that "is this
	// replica as deep as it was meant to be" is answerable without joining
	// against a configuration file that lives on another host.
	RestorePointsPolicyCount           int
	RestorePointsPolicyIntervalSeconds float64
	// RestorePointsStoreReadable is whether the store could be read at all.
	// Its own series rather than an outcome value, because it is orthogonal:
	// a run can fail to read the store and still be perfectly healthy in every
	// other respect, and this is the one that says "this number is missing
	// because nobody could look", which no count can express.
	RestorePointsStoreReadable bool
	// RestorePointOverdueSeconds is how long ago another restore point became
	// due and was not produced. Zero on every healthy run.
	//
	// The engine computes it rather than leaving it to PromQL, and that is the
	// point of it. The retention interval is a FLOOR and not a cadence, so a
	// threshold derived from it -- "stale if older than 3x the interval" --
	// fires on every healthy pair whose syncs are further apart than that,
	// which is the normal configuration for a long history. This answers the
	// only question with one answer: had the floor passed, and did a point
	// appear. Alert on `> 0` with no arithmetic and no clock comparison.
	RestorePointOverdueSeconds float64
	// RestorePoints is how many published restore points this target domain's
	// store holds after this run. Directories, including any whose sidecar
	// could not be read: what the store holds and what is provably restorable
	// are different questions, and RestorePointsStaging plus the agent's
	// unreadable count carry the difference. Points still flat in the shared
	// directory are outside every store, so nothing here can see them.
	RestorePoints int
	// RestorePointsOverCount is how many are beyond what the policy keeps.
	// Zero after any successful prune, so a number that stays positive means
	// pruning is failing -- which is only a warning on the run itself, and is
	// otherwise how a target silently fills up.
	//
	// Computed in-process rather than left to a `kept > policy_count`
	// expression, which would straddle two scrapes and can read as true while
	// a prune is in progress.
	RestorePointsOverCount int
	// RestorePointsStaging is how many abandoned staging directories are in
	// this domain's store. Non-zero means runs are dying between the first
	// reflink and the rename that publishes the set.
	RestorePointsStaging int
	// RestorePointNewestUnix and RestorePointOldestUnix are the instants the
	// newest and oldest points correspond to.
	//
	// The TAG instant, which is the checkpoint time the contents belong to and
	// the same value the agent publishes, so a dashboard and an alert cannot
	// disagree about the age of one copy. `time() - oldest` is the only
	// restore-point number an operator uses during an incident: how far back
	// this replica can actually be taken.
	RestorePointNewestUnix int64
	RestorePointOldestUnix int64
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

	fmt.Fprintln(&b, "# HELP vmsync_fsthaw_timed_out 1 when the filesystem thaw did not answer before vmsync stopped waiting, and had not reported back when this run ended. UNKNOWN rather than failed: the guest may be frozen, or its agent may simply have been busy. Distinct from vmsync_fsthaw_failed, which is a certainty -- alert on this one more gently.")
	fmt.Fprintln(&b, "# TYPE vmsync_fsthaw_timed_out gauge")
	fmt.Fprintf(&b, "vmsync_fsthaw_timed_out{source_host=%q,target_host=%q,vm=%q} %d\n",
		run.SourceHost, run.TargetHost, run.VM, boolMetric(run.FSThawTimedOut))

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
		fmt.Fprintln(&b, "# HELP vmsync_verification_state What -verify concluded about this replica: 0=ran and matched, 1=ran and found a difference (the replica is wrong), 2=could not be performed (the replica's state is unknown; fix the tooling). Only present for runs that had -verify set. Alert on 2 with a `for:` clause to ignore transient failures.")
		fmt.Fprintln(&b, "# TYPE vmsync_verification_state gauge")
		fmt.Fprintf(&b, "vmsync_verification_state{source_host=%q,target_host=%q,vm=%q} %d\n",
			run.SourceHost, run.TargetHost, run.VM, run.VerificationState)

		fmt.Fprintln(&b, "# HELP vmsync_verification_timestamp_seconds Unix time (seconds) this vmsync run last performed -verify, success or failure. Only present for runs that had -verify set.")
		fmt.Fprintln(&b, "# TYPE vmsync_verification_timestamp_seconds gauge")
		fmt.Fprintf(&b, "vmsync_verification_timestamp_seconds{source_host=%q,target_host=%q,vm=%q} %d\n",
			run.SourceHost, run.TargetHost, run.VM, run.VerificationTimestamp)
	}

	// The pre-commit integrity check. Rendered whenever the run reached the
	// point of deciding about it -- INCLUDING when it was skipped, which is
	// the state most worth exporting and the one nothing else surfaces: a
	// helper that is missing or version-skewed disables a default-on check
	// for every run on that host while each sync still reports success.
	if run.ChecksumRan {
		fmt.Fprintln(&b, "# HELP vmsync_checksum_state What the pre-commit integrity check concluded: 0=ran and matched, 1=ran and the target's bytes differ from what was sent, 2=could not be performed or was skipped (no matching vmsync-bridge-helper, or -no-checksum). Alert on 2 with a `for:` clause to ignore transient failures.")
		fmt.Fprintln(&b, "# TYPE vmsync_checksum_state gauge")
		fmt.Fprintf(&b, "vmsync_checksum_state{source_host=%q,target_host=%q,vm=%q} %d\n",
			run.SourceHost, run.TargetHost, run.VM, run.ChecksumState)

		fmt.Fprintln(&b, "# HELP vmsync_checksum_bytes Bytes this run digested and compared against the target. Zero when the run wrote nothing, which is ordinary for an incremental against an idle source.")
		fmt.Fprintln(&b, "# TYPE vmsync_checksum_bytes gauge")
		fmt.Fprintf(&b, "vmsync_checksum_bytes{source_host=%q,target_host=%q,vm=%q} %d\n",
			run.SourceHost, run.TargetHost, run.VM, run.ChecksumBytes)
	}

	writeRestorePoints(&b, run)

	return writeAtomic(path, b.String())
}

// writeRestorePoints renders the restore-point families.
//
// Gated in two stages, and which stage a series is under is the whole of its
// meaning:
//
//   - RetentionAsked: nothing at all for a run with no -retention. A zero count
//     there would assert an empty store to anybody reading it, when in fact
//     nothing looked.
//   - RestorePointsListed: the counts, on top of the above. A run refused at
//     the reflink probe still publishes its outcome, its policy and
//     store_readable=0 -- but not a last_taken of 0, which would say "this
//     store holds nothing" about a directory it never opened.
//
// The distinction matters because this file PERSISTS between runs: a series
// written on every run is scrapeable for the whole inter-run gap and can carry
// any `for:` clause, while one omitted on some runs flickers and can carry none.
func writeRestorePoints(b *strings.Builder, run RunMetric) {
	if !run.RetentionAsked {
		return
	}
	lbl := fmt.Sprintf("source_host=%q,target_host=%q,vm=%q,target_vm=%q",
		run.SourceHost, run.TargetHost, run.VM, run.TargetVM)

	// HELP and TYPE once for the family, then one sample line per outcome. A
	// per-sample HELP would make node_exporter reject the entire file, taking
	// down every metric in it including the ones being alerted on.
	outcome := run.RestorePointOutcome
	if outcome == "" {
		outcome = RestorePointUndetermined
	}
	fmt.Fprintln(b, "# HELP vmsync_restore_point_outcome What this run did about a restore point; exactly one of these series is 1. taken=one was published. not_due=the retention interval floor had not elapsed, which is the ordinary state of a healthy pair and not a fault. failed=one was due and none appeared, so the recovery history has stopped growing. undetermined=the run never reached the decision. Alert on failed, never on not_due.")
	fmt.Fprintln(b, "# TYPE vmsync_restore_point_outcome gauge")
	for _, o := range restorePointOutcomes {
		fmt.Fprintf(b, "vmsync_restore_point_outcome{%s,outcome=%q} %d\n", lbl, o, boolMetric(o == outcome))
	}

	fmt.Fprintln(b, "# HELP vmsync_restore_points_policy_count How many restore points -retention asks to keep for this pair. Published beside the count so \"is this replica as deep as it was meant to be\" needs no join against a configuration file on another host.")
	fmt.Fprintln(b, "# TYPE vmsync_restore_points_policy_count gauge")
	fmt.Fprintf(b, "vmsync_restore_points_policy_count{%s} %d\n", lbl, run.RestorePointsPolicyCount)

	fmt.Fprintln(b, "# HELP vmsync_restore_points_policy_interval_seconds The minimum spacing -retention asks for. A FLOOR, not a cadence: vmsync does not decide when it runs, so restore points end up as far apart as the syncs are. Do not build a staleness threshold from this -- use vmsync_restore_point_overdue_seconds, which the engine computes.")
	fmt.Fprintln(b, "# TYPE vmsync_restore_points_policy_interval_seconds gauge")
	fmt.Fprintf(b, "vmsync_restore_points_policy_interval_seconds{%s} %.3f\n", lbl, run.RestorePointsPolicyIntervalSeconds)

	fmt.Fprintln(b, "# HELP vmsync_restore_points_store_readable 1 when this run read its own restore point directory. At 0 every count below is ABSENT rather than zero, because this run learned nothing about the store -- either it could not be listed, or the run was refused before it got that far (a target filesystem with no reflink support, a domain with no disks). Either way the depth is unknown, not zero. Alert on 0 with a `for:` clause.")
	fmt.Fprintln(b, "# TYPE vmsync_restore_points_store_readable gauge")
	fmt.Fprintf(b, "vmsync_restore_points_store_readable{%s} %d\n", lbl, boolMetric(run.RestorePointsStoreReadable))

	fmt.Fprintln(b, "# HELP vmsync_restore_point_overdue_seconds How long ago another restore point became due and was not produced. Zero on every healthy run: zero while the interval floor has not elapsed, and zero once one is published. Positive ONLY when the floor had passed and no restore point appeared. Alert on > 0 directly -- no threshold arithmetic, and in particular do not derive one from the policy interval, which is a floor and not a cadence.")
	fmt.Fprintln(b, "# TYPE vmsync_restore_point_overdue_seconds gauge")
	fmt.Fprintf(b, "vmsync_restore_point_overdue_seconds{%s} %.3f\n", lbl, run.RestorePointOverdueSeconds)

	if !run.RestorePointsListed {
		return
	}

	fmt.Fprintln(b, "# HELP vmsync_restore_points How many published restore points this TARGET DOMAIN's store holds. Directories, including any whose sidecar could not be read.")
	fmt.Fprintln(b, "# TYPE vmsync_restore_points gauge")
	fmt.Fprintf(b, "vmsync_restore_points{%s} %d\n", lbl, run.RestorePoints)

	fmt.Fprintln(b, "# HELP vmsync_restore_points_over_count How many restore points are beyond what -retention keeps. Zero after any successful prune, so a value that stays positive means pruning keeps failing -- which is only a warning on the run itself, and is otherwise how a target quietly fills up. Computed in-process rather than as kept > policy_count, which would straddle two scrapes.")
	fmt.Fprintln(b, "# TYPE vmsync_restore_points_over_count gauge")
	fmt.Fprintf(b, "vmsync_restore_points_over_count{%s} %d\n", lbl, run.RestorePointsOverCount)

	fmt.Fprintln(b, "# HELP vmsync_restore_points_staging Abandoned staging directories in this domain's store. Non-zero means runs are dying between the first reflink and the rename that publishes the set. Named _staging and not _incomplete: vmsync_agent_replica_incomplete already means a half-written REPLICA, which is a different fault.")
	fmt.Fprintln(b, "# TYPE vmsync_restore_points_staging gauge")
	fmt.Fprintf(b, "vmsync_restore_points_staging{%s} %d\n", lbl, run.RestorePointsStaging)

	fmt.Fprintln(b, "# HELP vmsync_restore_point_last_taken_timestamp_seconds The instant the NEWEST restore point's contents correspond to. The tag instant, which is what the agent publishes too, so a dashboard and an alert cannot disagree about the age of one copy.")
	fmt.Fprintln(b, "# TYPE vmsync_restore_point_last_taken_timestamp_seconds gauge")
	fmt.Fprintf(b, "vmsync_restore_point_last_taken_timestamp_seconds{%s} %d\n", lbl, run.RestorePointNewestUnix)

	fmt.Fprintln(b, "# HELP vmsync_restore_point_oldest_timestamp_seconds The instant the OLDEST restore point's contents correspond to. time() minus this is how far back this replica can actually be taken, which is the one restore-point number an operator reaches for during an incident.")
	fmt.Fprintln(b, "# TYPE vmsync_restore_point_oldest_timestamp_seconds gauge")
	fmt.Fprintf(b, "vmsync_restore_point_oldest_timestamp_seconds{%s} %d\n", lbl, run.RestorePointOldestUnix)
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
