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

package libvirtsync

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"vmsync/pkg/disk"
	"vmsync/pkg/trace"

	"libvirt.org/go/libvirt"
	"libvirt.org/go/libvirtxml"
)

// TestFault names a failure vmsync should inject into itself, or "" for the
// only value any real run ever has. Set once from -test before any sync
// begins; never written again.
//
// This exists because some error paths cannot be reached from outside the
// process. DefineDomain's rollback is the case that forced it: the window
// between undefining the target and redefining it contains no I/O at all --
// it is a few milliseconds of in-memory XML editing -- so there is nothing an
// external harness can interrupt. Worse, the rollback restores over the same
// libvirt connection, so cutting that connection to force the failure also
// destroys the recovery being tested. The path was untestable, and an
// untested rollback is one that restores a domain wrongly on the day it
// finally runs.
//
// A flag rather than an environment variable, deliberately. An env var is
// inherited by every child process by default, so one set in a systemd unit,
// a cron environment or a container image would silently arm fault injection
// in every vmsync the agent ever spawns. A flag has to be typed on a command
// line -- and vmsync-agent builds its argv from a fixed allowlist of flags
// (cmd/vmsync-agent/profile.go, opexec.go), so it cannot pass this one
// through even if a control-plane payload asked it to.
//
// Unknown values are rejected at startup rather than ignored, so -test=typo
// fails loudly instead of running a normal sync the operator believes is
// testing something.
var TestFault string

// Injectable faults. Add the constant, add it to TestFaults, and put the
// check where the failure belongs.
const (
	// TestFaultFailureDefine makes the target's redefine fail, exercising
	// DefineDomain's rollback to the previous definition.
	TestFaultFailureDefine = "failure-define"
	// The two corruption faults, named for WHEN they fire, because that is the
	// only thing separating them and each one defeats a different check.
	// Neither can be staged from outside the process.

	// TestFaultCorruptBeforeChecksum writes garbage into the image the copy
	// just wrote -- the fleecing overlay on an incremental, the base on a full
	// sync -- in the window after the write export is stopped and before the
	// pre-commit digest check reads it back. The digest check is expected to
	// CATCH it: the run fails, the overlay is discarded, and the replica's base
	// is left untouched.
	//
	// This is the only way to test that check against genuinely corrupted
	// bytes. contrib/bench/bench.sh's stage 13b gets there with a shim that
	// falsifies the helper's REPLY, which proves vmsync reacts to a
	// mismatching answer -- not that the check detects wrong bytes. A vmsync
	// hashing the wrong ranges, or a helper hashing the overlay's backing file
	// instead of the overlay, would pass that shim test and fail this one.
	//
	// The corruption is placed inside a range the copy actually WROTE, not at
	// a fixed offset. The check only hashes what was written, so a fixed
	// offset would fall outside the plan on any small incremental, sail
	// through, and commit -- a silent pass for a check that never looked.
	//
	// Requires the check to be enabled and running (see checksumEnabled in
	// run()): with it off there is nothing to catch the corruption, so the
	// fault would just commit damage.
	TestFaultCorruptBeforeChecksum = "corrupt-before-checksum"

	// TestFaultCorruptAfterCommit writes garbage over the replica AFTER the
	// copy has been committed and the digest check has passed, and before
	// -verify reads it back -- so every -verify run under this fault reports a
	// genuine mismatch.
	//
	// The only way to reach the branch where a verification failure is
	// answered by a full recopy that then fails verification AGAIN, which is
	// what makes vmsync stop and mark the replica faulty. Corruption staged
	// beforehand cannot get there: the recopy overwrites it, which is
	// precisely the recopy's job.
	//
	// It also models the one corruption class nothing upstream can see. The
	// digest check proves the bytes arrived; the mtime guard catches a write
	// through the filesystem. Neither can see storage that went bad AFTER a
	// write was confirmed, which is the whole reason -verify re-reads with
	// --cache=none.
	//
	// Refused without -verify (see the flag's own validation): without a
	// comparison to fail, this is not a test, it is just damage.
	TestFaultCorruptAfterCommit = "corrupt-after-commit"

	// TestFaultFailLastDisk fails exactly ONE disk of a multi-disk domain,
	// after its copy and its digest check have both passed and immediately
	// before it would be staged for commit.
	//
	// It exists to test the commit barrier, and nothing else can. Every other
	// fault here fires inside the per-disk worker, so on a multi-disk domain
	// it fires on EVERY disk -- which proves only that a run where everything
	// fails commits nothing. The case worth proving is the asymmetric one: one
	// disk perfect, one disk failed, and the target left wholly at its
	// previous checkpoint rather than half at this one.
	//
	// The LAST disk rather than the first, and after the digest check rather
	// than before it, so the failing disk is the one that got furthest: it
	// copied every byte correctly and was refused at the final step. A fault
	// that fired earlier would let the barrier pass for the wrong reason, by
	// failing before its siblings had anything staged to discard.
	//
	// "Last" means the last disk to BECOME READY, not the last entry in the
	// domain's disk list. The two are rarely the same disk: the disks copy
	// concurrently, so the one that finishes last is whichever has the largest
	// delta this run, and picking by list position meant a domain whose
	// last-listed disk had the smallest delta refused first -- before any
	// sibling had staged anything, which is exactly the vacuous pass the
	// paragraph above warns about. See lastToStage in cmd/vmsync/main.go.
	TestFaultFailLastDisk = "fail-last-disk"

	// TestFaultDieWritingBase kills the process outright -- os.Exit(137), no
	// unwinding, no deferred cleanup, no signal handler -- once the first
	// base image of a FULL copy has been created and has had real extents
	// copied into it, and before anything has checked, committed or recorded
	// that the copy happened.
	//
	// It is the only fault that reproduces the geometry replica_incomplete
	// exists for, and the geometry IS the point. A full copy -- a -reinit, a
	// -force-clean, or any sync that writes bases directly -- renames the
	// good replica disks aside and writes new ones with no overlay, while the
	// target domain keeps its OLD metadata. A run killed in that window
	// leaves a half-written image on disk underneath a last_checkpoint,
	// last_sync_timestamp, replica_source and failure_count that all still
	// describe the replica it replaced. Every evidence check -promote makes
	// therefore reads healthy, and without the field it accepts the
	// half-written image and reports an ordinary data-loss window, while the
	// complete copy sits unused in the .vmsync-replaced-<unix> files.
	//
	// A fault that exited right after the rename would prove NOTHING. It
	// leaves the target with no disk files at all, which -promote already
	// refuses on ("one or more disk files are missing from the target host"),
	// so the refusal would fire identically against a build that never had
	// the field and the test would be green precisely when the feature was
	// absent. The state worth reaching is the one where every other check
	// passes.
	//
	// os.Exit rather than a returned error, and 137 rather than 1, because an
	// orderly failure is not what this models: an orderly failure unwinds,
	// stops the exports and writes an outcome, none of which the real cause
	// does. vmsync's own SIGTERM handler calls os.Exit too, so an agent
	// restart runs no defers either; a power cut and an OOM kill run fewer
	// still. 137 is the shell's encoding of SIGKILL, so a harness reading the
	// status sees a killed process rather than a failed sync.
	//
	// Refused on an incremental run (see run()): with a parent checkpoint the
	// copy lands in an overlay that is removed on every path out that is not
	// a commit, so dying there leaves the replica's base untouched, healthy
	// and correctly described by its own metadata -- nothing for the refusal
	// to catch, and a stage asserting that refusal would fail for a reason
	// that says nothing about vmsync.
	//
	// WHAT IT LEAVES ON THE TARGET, because nothing unwinds: the write export
	// is still running there, a qemu-nbd holding the half-written base open,
	// with its pidfile in -target-runtime-dir. Nothing reaps it -- the
	// deferred cleanup and the signal handler's replay of the registered stop
	// commands are both precisely what this fault skips. It blocks nothing
	// (-promote never opens the disks, and a later run creates a new file
	// rather than reopening this one), but it does hold that file's blocks
	// after any later rm, so a harness running this repeatedly against one
	// target must kill it. contrib/bench/bench.sh does, immediately after the
	// run and again from its cleanup trap.
	TestFaultDieWritingBase = "die-writing-base"
)

// TestFaults is every accepted -test value, for validation and for the flag's
// own help text.
var TestFaults = []string{TestFaultFailureDefine, TestFaultCorruptBeforeChecksum, TestFaultCorruptAfterCommit, TestFaultFailLastDisk, TestFaultDieWritingBase}

// ValidateTestFault reports whether name is an injectable fault. "" is valid
// and means no injection.
func ValidateTestFault(name string) error {
	if name == "" {
		return nil
	}
	for _, f := range TestFaults {
		if name == f {
			return nil
		}
	}
	return fmt.Errorf("unknown -test fault %q: must be one of %s", name, strings.Join(TestFaults, ", "))
}

const CheckpointPrefix = "vmsync-cpt"

// IsManagedCheckpointName reports whether a name belongs to vmsync's own
// checkpoint chain.
//
// The single definition of "ours", because two things now ask it and they must
// agree: ListManagedCheckpoints, deciding which checkpoints to report, and the
// inversion's offline cleanup, deciding which dirty BITMAPS it may delete off
// a disk image. A disagreement there means either leaving a bitmap whose
// checkpoint has gone -- which blocks every later sync -- or removing one that
// belongs to something else entirely.
//
// Note what this excludes on purpose: VerifyWindowCheckpointName, which is
// deliberately not prefixed so it stays invisible to the chain logic. Both
// callers ignore it identically, which is the intended outcome.
func IsManagedCheckpointName(name string) bool {
	return strings.HasPrefix(name, CheckpointPrefix+"-")
}

// VerifyWindowCheckpointName names a checkpoint NOTHING CREATES ANY MORE.
// The former -verify=online (now -verify=full) used to make one per run and
// that was the near-100%-false-positive bug (see the note where
// CreateVerifyWindowCheckpoint used to be). The name survives only so
// DeleteVerifyWindowCheckpoint can clear leftovers from older builds.
// Deliberately NOT prefixed with
// CheckpointPrefix+"-": ListManagedCheckpoints (and therefore
// NextCheckpointName, and -reinit's DeleteAllManagedCheckpoints) only ever
// look at names starting with "vmsync-cpt-", so this name is permanently
// invisible to all of them, unconditionally -- regardless of whether
// cleanup ever actually runs. That matters because NextCheckpointName hard
// -fails if the checkpoint it thinks is "latest" doesn't end in a numeric
// suffix; a leaked verify-window checkpoint must never be able to reach
// that code path at all, not just usually get cleaned up in time.
const VerifyWindowCheckpointName = "vmsync-verify-window"

const (
	metadataNamespace = `http://vmsync.org/xmlns/libvirt/domain/1.0`
	// vmsync's metadata element is written in TWO different spellings,
	// because it is written through two APIs that own the namespace
	// differently. Which one is used is not a style choice; each is the only
	// one its API accepts without destroying something.
	//
	// virDomainSetMetadata (metadata.go) takes the fragment plus a prefix and
	// a uri and does the binding itself. libvirt's own source, in
	// virXMLInjectNamespace:
	//
	//	if (!(ns = xmlNewNs(node, uri, key)))                 -> hard error
	//	virXMLForeachNode(node, virXMLAddElementNamespace, ns);
	//
	// where virXMLAddElementNamespace is `if (!node->ns) xmlSetNs(node, ns)`.
	// So a fragment that binds nothing gets every one of its elements bound
	// by libvirt, and a fragment that binds its own elements leaves libvirt's
	// declaration attached to nothing. And xmlNewNs returns NULL when that
	// PREFIX is already declared on the node -- which is why the original
	// `<vmsync:vmsync xmlns:vmsync="...">` failed every write through this API
	// with "internal error: failed to create a new XML namespace": promotion,
	// role changes, failure counting, replica_targets on the source.
	//
	// The reason the fragment must be NAKED, rather than merely avoiding the
	// `vmsync` prefix, is the read side. virXMLExtractNamespaceXML unbinds
	// every element in the uri and then removes ONE declaration of it:
	//
	//	virXMLForeachNode(nodeCopy, virXMLRemoveElementNamespace, uri);
	//	for (actualNs = nodeCopy->nsDef; actualNs; actualNs = actualNs->next) {
	//	    if (STREQ_NULLABLE(actualNs->href, uri)) { ...unlink...; break; }
	//
	// A fragment that declares the uri itself therefore leaves TWO
	// declarations on the stored element -- its own and libvirt's -- of which
	// the extractor deletes exactly one. What comes back is
	//
	//	<vmsync xmlns:vmsync="http://vmsync.org/xmlns/libvirt/domain/1.0">
	//	  <failure_count id="1"/>
	//	</vmsync>
	//
	// with every element unbound and a declaration nothing uses: read on its
	// own, a domain with no metadata. That is a real fragment off a real
	// domain, and it is what a default declaration produced. A different
	// prefix produces it too -- the extractor deletes whichever declaration
	// comes first and keeps libvirt's. Declaring nothing leaves exactly one
	// declaration to delete, and the fragment returns byte-identical to the
	// one that was sent.
	metadataFragmentStart = `<vmsync>`
	metadataFragmentEnd   = `</vmsync>`

	// The other writer grafts the element straight into a domain document and
	// redefines it (domxml.go), where nothing injects anything -- so here the
	// element must bind itself. It must also not be naked: every define runs
	// virDomainDefPostParseCommon, which calls
	// virXMLNodeSanitizeNamespaces(def->metadata), and that deletes any child
	// of <metadata> with no namespace outright.
	//
	// The two spellings converge on ONE on-disk form, because libvirt binds
	// the naked fragment to exactly this prefix: `<vmsync:vmsync
	// xmlns:vmsync="...">` with prefixed children. That is also the spelling
	// every vmsync version ever shipped recognises, which matters more than it
	// looks -- virXMLNodeSanitizeNamespaces resolves two children sharing a
	// namespace by deleting the LATER one, so if an older build ever failed to
	// recognise this element and appended a second beside it, libvirt would
	// keep the stale one and silently discard the new write.
	metadataElementStart = `<` + metadataPrefix + `:vmsync xmlns:` + metadataPrefix + `="` + metadataNamespace + `">`
	metadataElementEnd   = `</` + metadataPrefix + `:vmsync>`

	MetadataFieldLastCheckpoint = "last_checkpoint"
	MetadataFieldLastSync       = "last_sync_timestamp"
	MetadataFieldFailureCount   = "failure_count"
	// MetadataFieldReplicaSource is written on the TARGET: "<host>:<domain>"
	// of the source it's currently being replicated from.
	MetadataFieldReplicaSource = "replica_source"
	// MetadataFieldReplicaTargets is written on the SOURCE: a comma-
	// separated, deduplicated, ever-growing list of "<host>:<domain>"
	// entries for every distinct target this source has ever been
	// replicated to (a source can fan out to more than one target, unlike
	// a target, which only ever has the one source it was last defined
	// from).
	MetadataFieldReplicaTargets = "replica_targets"
	// MetadataFieldReplicationRole records what a domain currently IS in a
	// replication pair, persistently, independent of whether it happens to
	// be running at this instant. It exists to close a split-brain window
	// that runtime-state checks alone cannot: vmsync already refuses to
	// overwrite a target that is currently running (see
	// refuseReinitIfTargetRunning and DefineDomain's own active re-check),
	// but a domain that was failed over to, and then shut down for ten
	// minutes of maintenance, passes every one of those guards. The next
	// scheduled sync from the old source would then overwrite live data
	// with a stale replica -- and if -reinit-after-failures had been
	// climbing during the failover, what fires is not an incremental sync
	// but a full reinit, which removes the target's disks first.
	//
	// Enforced by vmsync itself rather than by whatever schedules it: cron
	// jobs, an operator running the binary by hand, and any future UI all
	// go through the same check, so the interlock cannot be bypassed by
	// simply not using the thing that knows about it.
	MetadataFieldReplicationRole = "replication_role"

	// The promotion record. Written together when a domain is promoted to
	// serve live, and stripped together whenever it stops being promoted
	// (an inversion, or -update-role away from promoted). They are an audit
	// trail rather than an interlock -- replication_role is what actually
	// refuses a sync -- but they are what tells an operator, days later,
	// which failover this was and how much data it accepted losing.
	MetadataFieldPromotedAt   = "promoted_at"
	MetadataFieldPromotedBy   = "promoted_by"
	MetadataFieldPromotedFrom = "promoted_from"
	// MetadataFieldPromotionMode records "planned" or "forced": whether the
	// source was cleanly shut down first, or whether the promotion went
	// ahead without reaching it at all. The difference is the difference
	// between a failover with no data loss and one with an unbounded
	// window, so it is recorded rather than inferred.
	MetadataFieldPromotionMode = "promotion_mode"

	// MetadataFieldLastPromotedAt records that this domain HAS SERVED LIVE at
	// least once, and when the last such promotion was. It is the only field in
	// this block that deliberately outlives the state it describes.
	//
	// It exists because every other trace of a failover is stripped the moment
	// the domain stops being promoted (see SetReplicationRole), and the
	// console's own advice routes through that strip: to force-clean a replica
	// you must shut it down first, and shutting down a promoted copy records
	// `paused` and takes the promotion record with it. A copy that served
	// production for a week therefore became, in two operations with nothing
	// refusing either, an ordinary paused replica that -restore-restore-point
	// and -force-clean will both overwrite from the source it displaced.
	//
	// Its own field rather than simply not stripping promoted_at, because those
	// four are an audit trail written in the PRESENT tense: a domain carrying
	// replication_role=target beside a promoted_at describes a failover that is
	// still in force, which no promotion ever wrote and which anything
	// reasoning about "was this displaced, and by whom" would read as fact.
	// UpdateSyncMetadata says the same in its own words. This one is past tense
	// by name, so role=target beside it is not a contradiction -- it is the true
	// and useful statement that this replica once held live data.
	//
	// WHAT IT IS NOT: proof the domain ever booted. It is written when a domain
	// BECOMES promoted, atomically with the promotion record, because a second
	// write after the guest started would leave a window in which a serving
	// domain carries no trace at all -- and this field only earns its place by
	// failing closed. So it over-records a promotion nobody ever started, such
	// as a drill, and the price of that is one deliberate -update-role rather
	// than a silently destroyed replica.
	MetadataFieldLastPromotedAt = "last_promoted_at"

	// The restore record: this domain's disks were rolled back to one of its
	// restore points, rather than being what the last sync copied.
	//
	// Written together by a restore and never inferred, because every
	// indirect signal for it is ambiguous. An old checkpoint_at looks exactly
	// like a lagging replica; failure_count=0 is what a clean sync writes;
	// and replication_role=paused is overwritten by the very promotion that
	// most needs this recorded. Without these fields an operator looking at a
	// promoted domain six months later cannot learn that its contents were
	// deliberately rolled back before it was promoted -- only that its
	// data-loss window was unusually wide, which is a symptom and not an
	// explanation.
	//
	// They are the counterpart of the promotion record, and they are here for
	// the same reason it is: in domain metadata rather than only in a control
	// plane's audit log, so the answer survives losing the control plane.
	//
	// Cleared by the next successful sync, without being named in
	// UpdateSyncMetadata's removal list: that function rebuilds the target's
	// metadata from the SOURCE's XML, so a target-only field it does not set
	// simply does not survive -- and a replica that has just been fully
	// recopied is, correctly, no longer a restored one.
	MetadataFieldRestoredFrom = "restored_from"
	MetadataFieldRestoredAt   = "restored_at"
	MetadataFieldRestoredBy   = "restored_by"

	// MetadataFieldReplicaWrittenAt records when vmsync itself last wrote
	// each replica disk: "vda=<unix>,vdb=<unix>", per disk, keyed by target
	// dev. Written on the TARGET.
	//
	// It exists because last_sync_timestamp is only written when a whole run
	// SUCCEEDS. A run that copied the disks and then failed -- a failed
	// -verify being much the commonest case -- left every replica disk with
	// a fresh mtime and the recorded timestamp untouched, so the next run's
	// out-of-band-modification check saw a disk newer than the last sync and
	// refused. Forever, since each refusal happens before the copy that
	// would have fixed it. One failed verify wedged the pair.
	//
	// The values come from `stat` on the TARGET host, which is the same
	// clock the mtime they are compared against comes from. Where a stamp
	// exists the comparison is therefore exact rather than cross-clock,
	// which is what -timestamp-tolerance-sec exists to paper over.
	//
	// WHAT IT DOES NOT COVER: a run KILLED mid-flight. The signal handler
	// stamps what it can, but it deliberately does not wait for the disk
	// goroutines, so a disk still inside `qemu-img commit` when the signal
	// lands is not recorded. That under-records, never over-records -- the
	// next run may still refuse, exactly as it does today, and never
	// wrongly accepts.
	//
	// A promoted domain keeps its last stamp deliberately: the role gate
	// refuses a sync into it long before the timestamp check runs, so
	// clearing it would buy nothing and lose the record.
	//
	// Being listed in metadataFieldOrder is for stable XML ordering ONLY --
	// buildMetadataEntry emits unknown fields too. What actually keeps this
	// field correct across a domain's life is the set-or-remove in
	// UpdateSyncMetadata plus the strip lists in RecordReplicaTarget,
	// pkg/failover's invert removals and pkg/restorepoint's MetadataPlan.
	MetadataFieldReplicaWrittenAt = "replica_written_at"

	// MetadataFieldPendingCheckpoint is the checkpoint the SOURCE is about
	// to advance to, recorded on the target BEFORE the source's chain
	// actually advances, and cleared once the target accepts it.
	//
	// It exists because the source's chain and the target's record of that
	// chain are advanced by two different writes, at two different times, by
	// two different libvirt calls -- and the second one can fail on its own.
	// CreateCheckpoint adds the checkpoint to the source; only
	// UpdateSyncMetadata -> DefineDomain (one DomainDefineXML at the very
	// end of the run) sets MetadataFieldLastCheckpoint on the target. A run
	// that copied successfully and then failed that define left the source at
	// cpt-000002 and the target still saying cpt-000001, and every later
	// incremental refused with "checkpoint inconsistency detected" until
	// somebody ran -reinit. That is a wedge, and it is a different one from
	// the mtime wedge MetadataFieldReplicaWrittenAt fixed.
	//
	// Write-ahead is what closes it, and the ORDER is the whole mechanism:
	//
	//   - written first, so if this write fails the run aborts before the
	//     source's chain has moved and there is nothing to reconcile;
	//   - written with SetDomainMetadataFields, a narrow namespaced
	//     SetMetadata that is a DIFFERENT libvirt call from the full
	//     redefine that fails -- demonstrably so, since replica_written_at
	//     lands on exactly the runs whose DefineDomain does not;
	//   - cleared by UpdateSyncMetadata, so acceptance and clearing are one
	//     atomic define: never both set, never neither.
	//
	// The next run then reads a RECORD rather than inferring from divergence.
	// If this names a checkpoint the source has and the target never
	// accepted, that checkpoint is the chain's tip and is deleted (bitmap
	// and all, see DeleteCheckpointIfExists) and the sync recopies from the
	// last accepted one. Recopying from the older baseline is a superset of
	// whatever the failed run managed to write, which matters because the
	// copy is per-disk and concurrent: a run can commit vda's overlay and
	// die before vdb's, so ADOPTING the newer checkpoint would declare a
	// baseline the target only partly holds.
	//
	// Same care as MetadataFieldReplicaWrittenAt about where it must be
	// stripped: UpdateSyncMetadata merges into the SOURCE's XML, so a source
	// that was once somebody's replica must not carry a stale value onto a
	// target. See the strip lists named in that field's comment.
	MetadataFieldPendingCheckpoint = "pending_checkpoint"

	// MetadataFieldReplicaIncomplete is written on the TARGET before a full
	// copy starts destroying the replica that is there, and cleared by the
	// write that records the copy as finished.
	//
	// It exists because a full copy -- -reinit, -force-clean, or any sync
	// whose computed parent is empty -- renames the good replica disks aside
	// and writes NEW base images directly, with no overlay, while the target
	// domain keeps its OLD metadata. A run that dies in that window (a
	// network drop, an agent restart whose signal handler calls os.Exit so
	// main() never runs, power loss) leaves the target saying
	// last_checkpoint=<old>, last_sync_timestamp=<old>, replica_source=<set>
	// and failure_count=0. Every one of those is true about the replica the
	// copy REPLACED and false about the half-written image now on the disks,
	// so pkg/failover's evidenceProblems finds nothing wrong and -promote
	// accepts it, reporting a normal data-loss window. The complete copy sits
	// unread in the .vmsync-replaced-<unix> files beside it.
	//
	// ON THE ACTED-ON DOMAIN'S OWN METADATA, deliberately, and that is the
	// whole reason it is a metadata field rather than a file or a control
	// plane record: -promote and -restore run LOCALLY on the target host, and
	// during the disaster they exist for THE SOURCE HOST IS GONE. Anything a
	// refusal depends on has to be readable on the replica's own host with
	// the other site unreachable.
	//
	// SINGLE-VALUED, never appended to, and cleared atomically by the same
	// DomainDefineXML that records success -- exactly the pattern
	// MetadataFieldPendingCheckpoint uses, and for the same reason: never
	// both set and accepted, never neither.
	//
	// The value is a single line of comma-separated k=v pairs built by
	// ReplicaIncompleteValue and read by failover.ParseReplicaIncomplete. A
	// value that cannot be read still refuses, because the presence of the
	// field is the finding and the parse only decides the wording.
	//
	// WHAT IT DELIBERATELY DOES NOT DO: it does not carry a failure_count.
	// Arming could have written failure_count=1, which every existing engine
	// already refuses a promotion on, and that was rejected -- a counter that
	// means "the last attempt failed" would then also mean "a copy is in
	// flight", and the two want different cures. The honest cost of that
	// choice: a PRE-UPGRADE engine doing a -promote on the DR host does not
	// know this field and would still accept a half-written replica, so every
	// host that drives syncs must be upgraded before the refusal can be
	// relied on.
	//
	// Same care as MetadataFieldReplicaWrittenAt about where it must be
	// stripped: UpdateSyncMetadata merges into the SOURCE's XML, so a source
	// that was once somebody's replica must not carry a stale value onto a
	// target. See the strip lists named in that field's comment, and the test
	// that pins all of them at once.
	MetadataFieldReplicaIncomplete = "replica_incomplete"

	// MetadataFieldVerifyState and MetadataFieldVerifyFailedAt record that
	// -verify found this replica's contents differing from its source, so
	// that the finding outlives the run that made it.
	//
	// Presence IS the state: absent means no recorded failure, and the only
	// value written is VerifyStateFailed. A "passed" value would be worse
	// than nothing -- it would go stale the moment the replica changed and
	// invite reading it as fresh assurance.
	//
	// Deliberately only the VERDICT and the date. The diagnosis -- how many
	// blocks, which offsets, scattered or contiguous -- goes to the log,
	// where an operator investigating actually looks. Putting a block count
	// here would invite policy being written against it ("only pause if
	// more than N"), and domain XML is the wrong place for a decision that
	// wants a human.
	//
	// Written by SetDomainMetadataFields (the narrow merge), because a
	// mismatch fails the run and the full-redefine path never executes.
	// Cleared UNCONDITIONALLY by UpdateSyncMetadata -- which needs no
	// parameter, and that is a consequence of how the finding is kept alive:
	// a domain carrying this refuses to sync at all
	// (TargetVerifyStateAllowsSync), so no successful sync can occur while
	// it is set. Any run that reaches UpdateSyncMetadata therefore either
	// verified and passed, or came through the recovery paths that are meant
	// to clear it. Removing rather than omitting also stops a source that
	// was once somebody's replica stamping a stale failure onto a healthy
	// target -- the same hazard replica_written_at documents.
	//
	// The durability of the record depends on that refusal being complete.
	// That is the accepted cost of this shape: get the refusal wrong, or add
	// a path around it later, and findings are lost silently instead of
	// loudly. The metrics and the log keep the history regardless; what is
	// lost is only the enforcement.
	MetadataFieldVerifyState    = "verify_state"
	MetadataFieldVerifyFailedAt = "verify_failed_at"

	// MetadataFieldCheckpointAt is when the checkpoint the replica's
	// contents correspond to was created -- the START of the copy that
	// produced them, not its end.
	//
	// last_sync_timestamp records when the copy FINISHED, which is the wrong
	// instant to measure a failover's data loss from: everything the guest
	// wrote from the checkpoint onward belongs to the NEXT checkpoint, so
	// the replica is frozen at that earlier moment. Measuring from the end
	// understates the loss by the whole copy duration -- minutes for a small
	// delta, hours for a full sync over a WAN, and wrong in the unsafe
	// direction exactly when the gap is widest. Absent on a target last
	// written by an older vmsync, which pkg/failover then reports as a lower
	// bound rather than as a precise figure.
	MetadataFieldCheckpointAt = "checkpoint_at"

	// MetadataFieldSourceStoppedAtSync is written on the TARGET: the source
	// domain was already shut off when the checkpoint behind this replica
	// was taken.
	//
	// It is the only honest basis for saying a failover loses nothing. A
	// stopped source cannot write, so the replica is complete as of that
	// instant -- as opposed to "-promote-mode=planned was passed", which is
	// a word the caller chose and evidence of nothing. Absent whenever the
	// source was running, so a later incremental from a running source
	// clears a stale one.
	MetadataFieldSourceStoppedAtSync = "source_stopped_at_sync"

	// Written on the SOURCE: when it last replicated successfully, and to
	// where.
	//
	// Distinct from last_sync_timestamp, which lives on a TARGET and means
	// "when was I last written". Same fact from two sides, and giving them
	// one name would make a domain that is both a source and a target
	// ambiguous about which it was reporting.
	//
	// These exist for the disaster case: standing at one host with the other
	// unreachable, the question is "when did this VM last replicate, and
	// where to" -- and before this the answer lived exclusively on the
	// machine you cannot reach. With a source fanning out to several
	// targets, it records the most recent one.
	MetadataFieldLastReplicatedAt = "last_replicated_at"
	MetadataFieldLastReplicatedTo = "last_replicated_to"

	// The fence a promotion armed, written on the PROMOTED domain: "this
	// failover displaces <host>:<domain>, and that source must not run".
	//
	// Written only when the promotion was explicitly asked to arm one, so a
	// DR drill promotes without authorising anything to be shut down. The
	// displaced source reads these from the promoted domain's own libvirt
	// and acts on them; it never infers a fence from role=promoted alone,
	// because a drill and a real failover leave identical records and only
	// one of them may stop a production VM. See pkg/failover/fence.go.
	//
	// fence_id makes the decision single-use: an agent records it in its
	// ledger before acting, so one token can never fire twice -- which is
	// what stops a token left behind by a January failover from shutting
	// something down in August.
	MetadataFieldFenceID      = "fence_id"
	MetadataFieldFenceSource  = "fence_source"
	MetadataFieldFenceArmedAt = "fence_armed_at"
	MetadataFieldFenceArmedBy = "fence_armed_by"

	// MetadataFieldAutostartIntent records whether the SOURCE of this replica
	// is marked to autostart, so that a promotion can restore the operator's
	// actual intention rather than guess at one.
	//
	// libvirt keeps autostart as a symlink beside the domain definition, not
	// as part of the XML, so it does not travel with a replica the way the
	// rest of a domain's shape does -- and DomainDefineXML does not touch it.
	// That asymmetry cuts both ways, and both ways are wrong:
	//
	//   - A domain that already autostarts and then BECOMES a replica keeps
	//     autostarting. An inversion does exactly that to a production source.
	//     The host reboots, libvirt starts the replica, and now a second copy
	//     of the VM is live with the source's MAC and identity. vmsync itself
	//     refuses to write into it (a running target is refused before any
	//     copy), so the replica's disks survive -- but the booted guest can
	//     corrupt everything OUTSIDE them: an AD computer account, an NFS
	//     mount, a clustered database.
	//   - A replica that is PROMOTED does not start autostarting, because
	//     nothing ever set it. Fail over, reboot the DR host, and production
	//     does not come back -- silently, in the window where the estate is
	//     already degraded.
	//
	// Recording the source's setting on the target closes both, and it has to
	// be recorded rather than read live for the case that matters: at
	// promotion time the source is usually GONE. This field is the only
	// surviving evidence of what the operator wanted.
	//
	// Three values, never absent: AutostartIntentYes, AutostartIntentNo and
	// AutostartIntentUnknown. "Unknown" is written deliberately when the
	// source's flag could not be read, and is distinct from the field being
	// missing -- one says "asked and could not tell", the other says "nothing
	// ever asked". Both refuse to autostart a promotion, but only one of them
	// is a record.
	//
	// NOT stripped on promotion, unlike the promotion record itself. A
	// promoted domain keeps it because -update-role=target is the documented
	// remedy for an unwanted promotion and turns autostart back off; keeping
	// the record means a later re-promotion still knows what the source
	// wanted, rather than falling back to "unknown" and refusing to start
	// production a second time.
	//
	// It cannot go stale in the way MetadataFieldReplicaWrittenAt warns about,
	// and by construction rather than by care: UpdateSyncMetadata merges into
	// the SOURCE's XML, so a source that was once a replica carries its own
	// old value -- but this field is always SET from a live read of the
	// source's real flag on every sync, so the merged-through value is
	// overwritten before it can ever be written to a target.
	MetadataFieldAutostartIntent = "autostart_intent"
)

// The values MetadataFieldAutostartIntent can hold. Strings rather than a
// bool-plus-presence, so "we asked and could not tell" is a thing the record
// can say -- see the field's own comment.
const (
	AutostartIntentYes     = "yes"
	AutostartIntentNo      = "no"
	AutostartIntentUnknown = "unknown"
)

// AutostartIntentFor turns a live reading of a domain's autostart flag into
// the value to record. ok is whether the flag could be read at all.
func AutostartIntentFor(autostart, ok bool) string {
	switch {
	case !ok:
		return AutostartIntentUnknown
	case autostart:
		return AutostartIntentYes
	default:
		return AutostartIntentNo
	}
}

// autostartForRole is the whole policy, kept pure so it can be tested without
// a hypervisor: given a domain's replication role and its recorded intent, say
// what its real autostart flag should be, and whether vmsync owns that
// decision at all.
//
// managed=false means "not vmsync's business, leave the flag exactly as it
// is". That is the right answer for a SOURCE: its autostart belongs to the
// operator who runs it, vmsync only ever READS it to record the intent. Taking
// ownership of a production domain's boot behaviour because it happens to be
// replicating would be a much worse surprise than the bug this fixes.
//
// The roles that DO get managed are the ones where vmsync's opinion is the
// safety property:
//
//   - target, paused: a replica must never boot. Unconditional, regardless of
//     intent -- the intent describes the source, and this domain is not it.
//   - fenced: unconditional and the sharpest of the three. A fence exists to
//     stop a displaced source running beside the copy that replaced it, and a
//     fence a power cycle undoes is not a fence. The host that was just
//     fenced is also the host most likely to be rebooted next.
//   - promoted: this domain IS production now, so it gets what the source had.
//     Only an explicit "yes" turns it on; both "no" and "unknown" leave it off,
//     which is the conservative direction and the one an operator can fix in
//     one command if it was wrong.
func autostartForRole(role, intent string) (want, managed bool) {
	switch role {
	case RoleTarget, RolePaused, RoleFenced:
		return false, true
	case RolePromoted:
		return intent == AutostartIntentYes, true
	default:
		// RoleSource, RoleNone, and anything unrecognised. Deliberately
		// permissive: an unknown role is not a licence to start changing a
		// domain's boot behaviour.
		return false, false
	}
}

// Replication roles, as stored in MetadataFieldReplicationRole. An empty or
// absent value is deliberately NOT one of these: it means "no role
// recorded", which every pre-existing deployment has, and which
// TargetRoleAllowsSync treats as permission to proceed. Roles are opt-in;
// vmsync never assigns one on its own (see TargetRoleAllowsSync's own doc
// comment for why).
const (
	// RoleSource marks a domain as the SOURCE of a replication pair.
	// Syncing INTO it is refused: that means something has the direction
	// backwards, which would overwrite the live original with its replica.
	RoleSource = "source"
	// RoleTarget marks a domain as a replication TARGET -- the normal,
	// permitted state for the receiving side of a sync.
	RoleTarget = "target"
	// RolePromoted marks a domain that WAS a target and has since been
	// promoted to serve live (a failover happened). Syncing into it is
	// refused regardless of whether it is running right now: that is the
	// whole point, since a promoted domain shut down for maintenance is
	// precisely the case runtime checks miss.
	RolePromoted = "promoted"
	// RolePaused marks a domain whose replication is administratively
	// suspended -- for maintenance, an investigation, or a migration in
	// progress. Refused like the others, but says "deliberately stopped"
	// rather than "direction is wrong" or "this is live now".
	RolePaused = "paused"
	// RoleFenced marks a domain an automatic FENCE stopped, because its peer
	// was promoted and this copy had been displaced.
	//
	// Distinct from RolePaused, which it used to share, and the difference is
	// what an operator does next. `paused` means a person suspended
	// replication and will resume it when they are ready. `fenced` means
	// nobody chose this: a peer took over, and the pair's direction has
	// probably reversed -- so the usual next step is -invert, not "resume".
	// Collapsing the two lost that, and lost it exactly where it was most
	// wanted; vmsync_ui carried a WasFenced heuristic that existed solely to
	// guess which of the two had happened, because "both end up paused with
	// nothing in libvirt telling them apart".
	//
	// It can also be recorded while the domain is STILL RUNNING, which
	// `paused` never legitimately is. A fence that could not stop its guest
	// (ACPI ignored, and vmsync never escalates to destroying a domain) writes
	// this anyway -- at that moment it is the only thing refusing a sync into
	// a live split brain, so it is needed most in the case where the shutdown
	// failed. See runFenceDomain.
	//
	// A fenced domain is still PROMOTABLE, deliberately: a fence acts on the
	// evidence of a peer's promotion, and that evidence can be wrong -- a
	// mistaken failover, a drill, a partition that healed. Refusing to promote
	// it would make a wrong fence unrecoverable.
	RoleFenced = "fenced"
	// RoleNone is not a stored value: it is the argument that CLEARS the
	// field, returning a domain to the no-role-recorded state.
	RoleNone = "none"
)

// ValidRoles lists the values SetReplicationRole accepts, in the order a
// CLI help message should present them.
//
// RoleFenced is settable by hand as well as written by the fence, so an
// operator can record one they carried out themselves -- and, more usefully,
// so `-update-role=fenced` exists as the honest way to say what happened
// rather than reaching for `paused` because it was the only word available.
var ValidRoles = []string{RoleSource, RoleTarget, RolePromoted, RolePaused, RoleFenced, RoleNone}

// metadataFieldOrder fixes the field order vmsync writes its own metadata
// entries in, purely for stable/readable XML output.
var metadataFieldOrder = []string{
	MetadataFieldReplicationRole,
	MetadataFieldLastCheckpoint,
	MetadataFieldPendingCheckpoint,
	MetadataFieldReplicaIncomplete,
	MetadataFieldVerifyState,
	MetadataFieldVerifyFailedAt,
	MetadataFieldLastSync,
	MetadataFieldReplicaWrittenAt,
	MetadataFieldFailureCount,
	MetadataFieldReplicaSource,
	MetadataFieldReplicaTargets,
	MetadataFieldPromotedAt,
	MetadataFieldPromotedBy,
	MetadataFieldPromotedFrom,
	MetadataFieldPromotionMode,
	MetadataFieldLastPromotedAt,
	MetadataFieldCheckpointAt,
	MetadataFieldSourceStoppedAtSync,
	MetadataFieldRestoredFrom,
	MetadataFieldRestoredAt,
	MetadataFieldRestoredBy,
	MetadataFieldLastReplicatedAt,
	MetadataFieldLastReplicatedTo,
	MetadataFieldFenceID,
	MetadataFieldFenceSource,
	MetadataFieldFenceArmedAt,
	MetadataFieldFenceArmedBy,
	MetadataFieldAutostartIntent,
}

// vmsyncBlockRe is gone: finding and replacing the metadata element by
// regex was only ever needed because libvirtxml models <metadata> as an
// opaque string. The element is now located and replaced in the parsed
// tree, which cannot be fooled by an attribute containing ">" or by the
// element appearing inside a comment.

type Manager struct {
	Conn *libvirt.Connect
	URI  string
}

type Checkpoint struct {
	Name   string
	Parent string
	Time   time.Time
}

func Connect(uri string) (*Manager, error) {
	conn, err := libvirt.NewConnect(uri)
	if err != nil {
		return nil, fmt.Errorf("connect libvirt %s: %w", uri, err)
	}
	return &Manager{Conn: conn, URI: uri}, nil
}

// ExternalSnapshotCountViaReconnect is ExternalSnapshotCount for a caller
// that has no open connection to sourceURI yet -- used for the
// -ignore-external-snapshot preflight check in cmd/vmsync, which runs before
// run() ever opens its own source connection.
func ExternalSnapshotCountViaReconnect(sourceURI, domainName string) (int, error) {
	mgr, err := Connect(sourceURI)
	if err != nil {
		return 0, fmt.Errorf("reconnect source libvirt: %w", err)
	}
	defer mgr.Close()
	dom, err := mgr.LookupDomain(domainName)
	if err != nil {
		return 0, fmt.Errorf("lookup domain %s on reconnect: %w", domainName, err)
	}
	defer dom.Free()
	return ExternalSnapshotCount(dom)
}

func StopBackupViaReconnect(sourceURI, domainName string) error {
	mgr, err := Connect(sourceURI)
	if err != nil {
		return fmt.Errorf("reconnect source libvirt: %w", err)
	}
	defer mgr.Close()
	dom, err := mgr.LookupDomain(domainName)
	if err != nil {
		return fmt.Errorf("lookup domain %s on reconnect: %w", domainName, err)
	}
	defer dom.Free()
	return StopBackup(dom)
}

func DeleteCheckpointViaReconnect(sourceURI, domainName, checkpointName string) error {
	mgr, err := Connect(sourceURI)
	if err != nil {
		return fmt.Errorf("reconnect source libvirt: %w", err)
	}
	defer mgr.Close()
	dom, err := mgr.LookupDomain(domainName)
	if err != nil {
		return fmt.Errorf("lookup domain %s on reconnect: %w", domainName, err)
	}
	defer dom.Free()
	return DeleteCheckpointIfExists(dom, checkpointName)
}

// ResumeDomainViaReconnect is the resume-source-VM counterpart to
// StopBackupViaReconnect/DeleteCheckpointViaReconnect above, for exactly the
// same reason: a primary-connection failure (a wedged/stale connection
// after a long-running sync, a transient network blip) must not be the
// difference between a suspended-for-verify production source resuming or
// staying paused indefinitely -- of everything the interrupt-cleanup path
// touches, a paused production source is the single most availability-
// critical thing left unresumed, more so than a leftover backup job or
// checkpoint.
func ResumeDomainViaReconnect(sourceURI, domainName string) error {
	mgr, err := Connect(sourceURI)
	if err != nil {
		return fmt.Errorf("reconnect source libvirt: %w", err)
	}
	defer mgr.Close()
	dom, err := mgr.LookupDomain(domainName)
	if err != nil {
		return fmt.Errorf("lookup domain %s on reconnect: %w", domainName, err)
	}
	defer dom.Free()
	return dom.Resume()
}

func (m *Manager) Close() error {
	if m == nil || m.Conn == nil {
		return nil
	}
	_, err := m.Conn.Close()
	return err
}

// Reconnect replaces this Manager's connection with a fresh one to the same
// URI, so every later call through it uses the new connection.
//
// For the case a long sync creates and nothing else recovers from: a verify
// of a large disk can run for an hour, and a qemu+ssh connection idle that
// long gets closed underneath us. Everything the run does after that fails
// with "client socket is closed" -- including, on a run where every disk
// copied, committed and verified correctly, the final role read and redefine
// that record the work as having happened.
//
// Healing the Manager rather than offering a one-shot ViaReconnect helper per
// call, which is how the SOURCE side does it: those helpers open a connection,
// do one thing and close it, so the NEXT call is still broken. Here the
// failing calls come in a sequence that has to complete as a unit -- read the
// role, then redefine the domain with it -- and reconnecting once has to fix
// all of them.
//
// Safe only where nothing else is using the old connection concurrently. Every
// caller today is past the disk workers' barrier, on run()'s own goroutine.
//
// The old connection is closed best-effort: it is already broken by
// assumption, and failing to close a broken connection is not a reason to
// refuse a working replacement.
func (m *Manager) Reconnect() error {
	if m == nil {
		return fmt.Errorf("reconnect: nil manager")
	}
	if m.Conn != nil {
		if _, err := m.Conn.Close(); err != nil {
			trace.Debug("closing the old libvirt connection before reconnecting failed; it was already broken", "uri", m.URI, "error", err)
		}
		m.Conn = nil
	}
	conn, err := libvirt.NewConnect(m.URI)
	if err != nil {
		return fmt.Errorf("reconnect libvirt %s: %w", m.URI, err)
	}
	m.Conn = conn
	return nil
}

func (m *Manager) LookupDomain(name string) (*libvirt.Domain, error) {
	dom, err := m.Conn.LookupDomainByName(name)
	if err != nil {
		return nil, fmt.Errorf("lookup domain %s: %w", name, err)
	}
	return dom, nil
}

func DomainExists(conn *libvirt.Connect, name string) (bool, error) {
	d, err := conn.LookupDomainByName(name)
	if err != nil {
		if lvErr, ok := err.(libvirt.Error); ok && lvErr.Code == libvirt.ERR_NO_DOMAIN {
			return false, nil
		}
		return false, err
	}
	_ = d.Free()
	return true, nil
}

// isUUIDCollisionError reports whether err is libvirt's specific rejection
// of a DomainDefineXML call because another domain already registered on
// the same host uses domainXML's own UUID: virDomainObjListAddLocked (see
// libvirt's own src/conf/virdomainobjlist.c) reports this as
// VIR_ERR_OPERATION_FAILED with the message "domain '%s' is already defined
// with uuid %s" -- confirmed directly against libvirt's current source
// rather than guessed.
//
// Checked via the error's structured Code plus domainXML's own UUID
// appearing in the message, rather than matching the English phrase
// "already defined with uuid": libvirt's error messages are translated via
// gettext based on the process's own locale (LC_ALL/LANG/LANGUAGE), so a
// plain English substring match silently never fires on a non-English
// system -- observed directly against a French-locale libvirtd, where this
// exact error read "... est déjà défini avec l'uuid ...". Code alone isn't
// specific enough on its own (VIR_ERR_OPERATION_FAILED is used broadly
// across libvirt for unrelated failures too), but combined with the UUID
// itself -- never translated, since it's substituted data, not prose --
// genuinely pins this down to the same specific condition regardless of
// locale.
func isUUIDCollisionError(err error, domainXML string) bool {
	lvErr, ok := err.(libvirt.Error)
	if !ok || lvErr.Code != libvirt.ERR_OPERATION_FAILED {
		return false
	}
	domcfg := &libvirtxml.Domain{}
	if unmarshalErr := domcfg.Unmarshal(domainXML); unmarshalErr != nil || domcfg.UUID == "" {
		return false
	}
	return strings.Contains(strings.ToLower(lvErr.Message), strings.ToLower(domcfg.UUID))
}

// DefineDomain (re)defines targetDomainName on target from sourceDomainXML.
// rootSourceByLiveSource maps each disk's live source path to its resolved
// backing-chain root file (see disk.QcowDisk.RootSource) -- passed straight
// through to replaceDomainDiskPath so the domain definition names disks the
// same way the actual data copy does. This runs independently of
// targetDiskPath (which only controls relocation to a different directory):
// pass nil/empty only when every disk's live source is already the correct
// target-side name (no external snapshot/linked clone in play for any
// disk) -- targetDiskPath being empty is not by itself a reason to pass an
// empty map too.
//
// If targetDomainName already exists, it is undefined first (persistent
// definitions can't be replaced in place) and its prior XML is kept in
// memory so a subsequent failure to define the replacement -- a transient
// libvirtd error, or the rewritten XML itself being rejected -- can restore
// it instead of leaving the target permanently undefined.
func DefineDomain(target *Manager, targetDomainName string, sourceDomainXML string, targetDiskPath string, rootSourceByLiveSource map[string]string) error {
	exists, err := DomainExists(target.Conn, targetDomainName)
	if err != nil {
		return fmt.Errorf("check target domain existence: %w", err)
	}

	var originalXML string
	if exists {
		trace.Info("Undefining domain on target system", "vm", targetDomainName)
		d, err := target.Conn.LookupDomainByName(targetDomainName)
		if err != nil {
			return fmt.Errorf("look up existing target domain %s for undefine: %w", targetDomainName, err)
		}
		defer d.Free()
		// Captured before undefining -- see rollback below. Failing to even
		// read it is treated as fatal rather than undefining a domain with
		// no way back to its current definition. DOMAIN_XML_INACTIVE
		// explicitly requests the persistent, offline definition rather
		// than whatever the live domain would report if it happened to be
		// running at this exact moment (see the state re-check just
		// below) -- flags=0 returns the LIVE definition in that case,
		// which can include ephemeral runtime-only elements (actual PCI
		// addresses assigned at boot, live CPU/NUMA pinning) that don't
		// belong in, and may not even be valid as, the persistent
		// definition rollback would later try to restore via
		// DomainDefineXML.
		originalXML, err = d.GetXMLDesc(libvirt.DOMAIN_XML_INACTIVE)
		if err != nil {
			return fmt.Errorf("read existing target domain %s xml before undefine: %w", targetDomainName, err)
		}
		// Re-checked here, immediately before undefining, rather than
		// trusting an EARLIER check made by DefineDomain's own caller (in
		// cmd/vmsync/main.go, run() checks the target is shut off once,
		// near the very start): DefineDomain runs at the very end of a
		// sync, potentially hours later for a large disk, and nothing
		// stops an operator or another tool from starting the target in
		// the meantime. Undefining -- then this function's own redefine
		// further down replacing -- a domain's persistent definition while
		// qemu still has it running is exactly the same hazard
		// refuseReinitIfTargetRunning (cmd/vmsync/main.go) exists to
		// prevent for -reinit's own disk-file removal. Checked after
		// capturing originalXML above (that capture is valid regardless of
		// the domain's run state) and as close as possible to the actual
		// UndefineFlags call below, to keep the unavoidable TOCTOU window
		// between this check and that call as small as it can be. Nothing
		// has been undefined yet at this point, so a plain error return
		// here needs no rollback -- the domain's existing definition is
		// still completely untouched.
		active, err := DomainActive(d)
		if err != nil {
			return fmt.Errorf("check target domain %s state before undefine: %w", targetDomainName, err)
		}
		if active {
			return fmt.Errorf("target domain %s is running, refusing to undefine/redefine its persistent definition while active -- shut it down before syncing", targetDomainName)
		}
		// KEEP_NVRAM: vmsync never copies or manages a domain's NVRAM/
		// varstore file itself (see DetectNvram -- it only checks the file
		// already exists on the target and warns if not), so undefining
		// here must not delete it out from under whatever provisioned it.
		// Undefine() (no flags) unconditionally refuses to undefine any
		// domain that has an NVRAM file present at all, which is exactly
		// why this previously failed -- silently, since the error was
		// swallowed -- for every UEFI/OVMF target domain.
		// CHECKPOINTS_METADATA is required, not optional: libvirt refuses to
		// undefine an inactive domain that carries checkpoint metadata
		// unless it is passed. A target acquires checkpoints whenever it has
		// previously been a SOURCE -- which is exactly what the far end of
		// an inverted pair is -- so without this the first sync in the new
		// direction fails at its very last step, after having already copied
		// the entire VM. Dropping the checkpoints is correct here anyway:
		// this domain is being replaced wholesale by the source's
		// definition, and a chain describing the disks it used to have is
		// meaningless against the disks it is about to be given.
		if err := d.UndefineFlags(libvirt.DOMAIN_UNDEFINE_KEEP_NVRAM | libvirt.DOMAIN_UNDEFINE_CHECKPOINTS_METADATA); err != nil {
			return fmt.Errorf("undefine existing target domain %s: %w", targetDomainName, err)
		}
	}

	// rollback restores the target domain to its pre-undefine definition
	// (best effort) whenever cause is about to make this function return
	// without having left a valid replacement defined -- a no-op if the
	// domain didn't already exist, since there's nothing to roll back to.
	rollback := func(cause error) error {
		if !exists {
			return cause
		}
		restored, rbErr := target.Conn.DomainDefineXML(originalXML)
		if rbErr != nil {
			return fmt.Errorf("%w (also failed to restore target domain's previous definition: %v)", cause, rbErr)
		}
		restored.Free()
		trace.Warning("restored target domain to its previous definition after redefine failure", "vm", targetDomainName, "cause", cause)
		return cause
	}

	// Keep source XML intact (including UUID) unless libvirt rejects duplicate UUID.
	updatedXML, err := replaceDomainName(sourceDomainXML, targetDomainName)
	if err != nil {
		return rollback(fmt.Errorf("rewrite target domain xml: %w", err))
	}

	if shouldRewriteDiskPaths(targetDiskPath, rootSourceByLiveSource) {
		updatedXML, err = replaceDomainDiskPath(updatedXML, targetDiskPath, rootSourceByLiveSource)
		if err != nil {
			return rollback(fmt.Errorf("rewrite target domain xml: %w", err))
		}
	}

	// The UEFI varstore, repathed for the same reason and in the same breath as
	// the disks. libvirt names it after the DOMAIN, so a replica inheriting the
	// source's path verbatim carries a file named for a different domain --
	// harmless while the two names match, and a shared-varstore collision the
	// moment the target host also runs a domain genuinely called that. See
	// TargetNvramPath for exactly which paths are rewritten and which are left
	// to the operator.
	//
	// Reads the SOURCE's name from the source XML rather than being handed it:
	// replaceDomainName above has already renamed the document, so asking
	// updatedXML would answer with the target's name and rewrite nothing.
	if srcName, nameErr := domainNameFromXML(sourceDomainXML); nameErr == nil {
		updatedXML, err = replaceDomainNvramPath(updatedXML, srcName, targetDomainName)
		if err != nil {
			return rollback(fmt.Errorf("rewrite target domain xml: %w", err))
		}
	}

	warnIfXMLElementsDropped("DefineDomain", sourceDomainXML, updatedXML)

	// -test=failure-define corrupts the document rather than returning a
	// synthetic error, so that libvirt is genuinely called, genuinely refuses,
	// and the rollback below runs against whatever state a real rejection
	// leaves behind. Short-circuiting the call would instead test a path that
	// only exists under the test flag: it would prove the rollback closure
	// compiles, not that it recovers a domain libvirtd has just declined to
	// redefine. See TestFault.
	if TestFault == TestFaultFailureDefine {
		trace.Warning("-test=" + TestFaultFailureDefine + ": corrupting the target domain XML so libvirt rejects this redefine")
		updatedXML = "<vmsync-test-injected-failure>" + updatedXML
	}

	dom, err := target.Conn.DomainDefineXML(updatedXML)
	if err != nil {
		// Fallback for cloning into same target where another domain already uses the UUID.
		if isUUIDCollisionError(err, updatedXML) {
			// Logged as a Warning, not Info: this isn't a routine step --
			// it means something ELSE on the target host currently claims
			// the source's own UUID, and the consequence is significant
			// and easy to miss otherwise: the target domain gets a brand
			// new, randomly-assigned UUID (stripDomainUUID leaves none for
			// libvirt to preserve), silently, on every single run this
			// keeps happening. Anything tracking the target by UUID (an
			// inventory system, another tool, an operator's own notes)
			// would see it change out from under them with nothing in the
			// logs to explain why, until now.
			trace.Warning("target domain redefine hit a UUID collision with another domain on the target host; stripping the UUID and letting libvirt assign a new random one for this domain instead -- if this keeps happening on every run, something else on the target (a stray clone, a leftover throwaway domain) is claiming the source's UUID and should be investigated", "vm", targetDomainName, "error", err)
			withoutUUID, stripErr := stripDomainUUID(updatedXML)
			if stripErr != nil {
				return rollback(fmt.Errorf("strip uuid from target domain xml for uuid-collision fallback: %w", stripErr))
			}
			dom, retryErr := target.Conn.DomainDefineXML(withoutUUID)
			if retryErr != nil {
				return rollback(fmt.Errorf("define target domain after uuid fallback: %w", retryErr))
			}
			trace.Info("Redefined target vm with new configuration (uuid-collision fallback: new random uuid assigned)", "vm", targetDomainName)
			return dom.Free()
		}
		return rollback(fmt.Errorf("define target domain: %w", err))
	}
	trace.Info("Redefined target vm with new configuration", "vm", targetDomainName)
	return dom.Free()
}

// thawAttempts and thawRetryDelay bound the retry. Short and few: the caller
// is often a signal handler on its way out, and a guest agent that has not
// answered twice a second apart is not going to answer on the third try
// either -- at which point saying so loudly beats blocking the unwind.
const (
	thawAttempts   = 3
	thawRetryDelay = time.Second
)

// ThawFs releases a filesystem freeze, and reports whether it failed.
//
// Retried, and loud, because the two halves of a freeze are not symmetric. A
// freeze that fails costs consistency on a copy; a THAW that fails leaves the
// guest's filesystems frozen, and every write in that guest blocks until
// somebody thaws it by hand. That is not a degraded backup, it is a hung
// production VM -- caused by a run that otherwise reports success.
//
// The realistic failure is a guest agent that is momentarily busy or was
// restarted mid-run, which a second attempt clears. Retrying an unfreeze is
// safe in a way retrying most things is not: FSThaw is idempotent, and
// thawing something already thawed is a no-op rather than an escalation.
//
// Returns true when the filesystems are still frozen after every attempt.
func ThawFs(srcDom *libvirt.Domain, freezed bool) (failed bool) {
	if !freezed {
		return false
	}
	var lastErr error
	for attempt := 1; attempt <= thawAttempts; attempt++ {
		if err := srcDom.FSThaw(nil, 0); err == nil {
			if attempt > 1 {
				trace.Warning("thawed the source filesystems, but only after a retry -- the guest agent did not answer the first attempt",
					"attempts", attempt)
			} else {
				trace.Info("Successfully thawed file systems using guest agent")
			}
			return false
		} else {
			lastErr = err
			trace.Warning("filesystem thaw attempt failed", "attempt", attempt, "of", thawAttempts, "error", err)
		}
		if attempt < thawAttempts {
			time.Sleep(thawRetryDelay)
		}
	}
	// Error, not Warning. The guest is left with its filesystems frozen: it
	// will accept no writes until a person runs `virsh domfsthaw` against it.
	trace.Error("FILESYSTEM THAW FAILED: this guest's filesystems are still FROZEN and it will block on every write until somebody thaws it by hand (virsh domfsthaw). This is not a problem with the copy -- it is a problem with the source VM",
		"attempts", thawAttempts, "error", lastErr)
	return true
}

// shouldRewriteDiskPaths decides whether DefineDomain needs to run
// replaceDomainDiskPath at all: either there's a relocation to apply
// (targetDiskPath set) or a live-source-to-root-source substitution to
// apply (rootSourceByLiveSource non-empty). Gating this on targetDiskPath
// alone -- the previous behavior -- skipped the whole rewrite, root-source
// substitution included, for the common case of no -target-disk-path. That
// was exactly wrong for an external-snapshot/linked-clone source: its live
// Source (an overlay vmsync's data copy never writes to under that name)
// would then survive unchanged into the target's own definition, pointing
// at a file that doesn't exist on the target at all. SetTargetPath's own
// empty-targetDiskPath branch already returns rootSource verbatim, so
// running this with targetDiskPath == "" is exactly the "keep the same
// path, just rename to root" case, not a no-op to be skipped.
func shouldRewriteDiskPaths(targetDiskPath string, rootSourceByLiveSource map[string]string) bool {
	return targetDiskPath != "" || len(rootSourceByLiveSource) > 0
}

// replaceDomainDiskPath rewrites each non-ignored disk's <source file> to its
// target-side path, and clears any <backingStore> the live domain XML
// carried for it. rootSourceByLiveSource maps a disk's live Source path (as
// currently written in domainXML) to its resolved backing-chain root file --
// see disk.QcowDisk.RootSource's own doc comment for why this distinction
// matters: the live Source can point at an external-snapshot overlay that
// was never actually copied to the target under that name, while RootSource
// is the stable base filename the sync's own data-copy path always uses.
// Without this, the domain definition and the actual replicated file could
// silently disagree on the disk's name the moment an external snapshot
// exists. A disk missing from the map is a hard error, not a fallback to its
// own live Source: shouldn't happen for anything ParseQcowDisks would also
// have picked up, since both apply the same IgnoreDevice filter, but if it
// ever does, silently writing the live Source into the target's persistent
// definition would be exactly the bug this function exists to prevent --
// the live Source can be an external-snapshot overlay never copied to the
// target under that name, so returning an error here beats corrupting the
// target definition without a trace of why.
//
// Clearing BackingStore is required for the same reason, not an unrelated
// cleanup: whatever the live domain XML's backing chain says (an external
// snapshot's parent, or a permanent linked clone's shared base image) names
// a file on the *source* host, which vmsync never copies over -- only the
// resolved root file itself is copied, and always as a complete, standalone
// image: cmd/vmsync's main.go creates the target file flat for a full sync,
// and for an incremental sync commits the temporary delta overlay straight
// into that same root file with `qemu-img commit` before deleting the
// overlay, so by the time this runs the target's on-disk file never depends
// on any backing file at all. Left as copied verbatim from the source XML,
// a stale <backingStore> would describe a chain that either doesn't exist
// on the target host or doesn't match its actual (backing-file-free) disk
// -- something libvirt/qemu can refuse to start the domain over, or worse,
// misinterpret.
// xmlElementCounts returns, for each distinct element tag name (local name
// only -- "hostdev", "commandline", etc. -- namespace prefixes aren't
// distinguished, since the same element can legitimately round-trip through
// a different prefix with no actual loss) appearing anywhere in domainXML,
// how many times it occurs. Counting rather than just recording presence is
// what lets missingXMLElements notice a repeated element (multiple <disk>,
// <interface>, <hostdev>, ...) losing one or more instances even when at
// least one same-named sibling survives elsewhere in the document. Returns
// nil if domainXML doesn't even parse as XML, rather than produce a false
// "elements are missing" signal for something that was never valid to
// begin with.
//
// A <backingStore> element is counted itself but its CONTENTS are skipped
// entirely, on both sides of missingXMLElements' comparison. This is the
// content-level counterpart to intentionallyDroppedXMLElements suppressing
// the "backingStore" name itself, and is needed for the same reason: a
// backing chain's <backingStore> nests its own <format> and <source>
// (libvirt renders it as <backingStore><format/><source/><backingStore/>
// </backingStore>), so replaceDomainDiskPath clearing the whole subtree on
// purpose takes those children with it. Counting them would report "source"
// as dropped on every sync of any domain with an external snapshot or a
// permanent linked clone -- the disk's OWN <source> survives, rewritten, so
// only the count falls, not the name -- and "format" likewise, which
// appears nowhere else in a typical domain. Both are exactly the
// "guaranteed, permanent false positive on every such run" that
// intentionallyDroppedXMLElements exists to prevent, just one level down.
// Skipping the subtree rather than adding "source"/"format" to that
// suppression list keeps a genuinely dropped disk <source> -- the real loss
// this check is here to catch -- still fully visible.
func xmlElementCounts(domainXML string) map[string]int {
	counts := map[string]int{}
	dec := xml.NewDecoder(strings.NewReader(domainXML))
	for {
		tok, err := dec.Token()
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		counts[start.Name.Local]++
		if start.Name.Local == "backingStore" {
			// Consumes through this element's matching end tag, so nothing
			// nested inside it is ever counted. Self-closing <backingStore/>
			// works the same way: the decoder synthesizes both tokens, so
			// Skip just consumes the end tag immediately.
			if err := dec.Skip(); err != nil {
				return nil
			}
		}
	}
	return counts
}

// intentionallyDroppedXMLElements lists element names missingXMLElements
// must never report, because this package removes them itself, on purpose,
// every time they're present -- not an accidental casualty of the
// unmarshal/marshal round-trip warnIfXMLElementsDropped exists to catch.
// Currently just backingStore: replaceDomainDiskPath clears it on every
// disk it touches (see that function's own doc comment for why), so it
// would otherwise be reported as "dropped" on literally every sync of a
// domain with an external snapshot or a permanent linked clone -- a
// guaranteed, permanent false positive on every such run, unlike the rare,
// genuinely-worth-a-look struct-modeling gaps this warning is meant to
// surface.
//
// This covers the element NAME only. Suppressing what a cleared
// <backingStore> takes down with it -- the <format> and <source> a real
// backing chain nests inside it -- is xmlElementCounts' job (see its own doc
// comment): it skips the whole subtree instead, so those names stay fully
// reportable everywhere else in the document. Adding them here instead would
// be the easy fix and the wrong one, since a disk genuinely losing its own
// <source> is precisely the loss this check exists to catch.
var intentionallyDroppedXMLElements = map[string]bool{
	"backingStore": true,
}

// missingXMLElements returns the sorted list of distinct element names
// whose occurrence count in rewritten is lower than in original -- catching
// both a name disappearing entirely (count drops to 0) and a repeated
// element (multiple <disk>, <interface>, <hostdev>, ...) losing one or more
// instances while same-named siblings survive elsewhere in the document.
// Empty (nil) when original doesn't parse, rewritten doesn't parse, or
// nothing is missing. This still can't see attribute-level loss within an
// instance that survives (a <disk> keeping its tag but losing an attribute,
// say) -- only that an instance of a given tag name went away. Split out
// from warnIfXMLElementsDropped below purely so this actual comparison
// logic is directly testable without needing to capture log output.
func missingXMLElements(original, rewritten string) []string {
	before := xmlElementCounts(original)
	if before == nil {
		return nil
	}
	after := xmlElementCounts(rewritten)
	if after == nil {
		return nil
	}
	var missing []string
	for name, beforeCount := range before {
		if intentionallyDroppedXMLElements[name] {
			continue
		}
		if after[name] < beforeCount {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}

// warnIfXMLElementsDropped logs a warning (tagged with context, e.g. a
// function/call-site name) listing any element names missingXMLElements
// finds went missing between original and rewritten.
//
// It exists because replaceDomainName, replaceDomainDiskPath and
// SetMetadataFields all used to go through a full libvirtxml.Domain
// unmarshal-then-marshal round-trip, which silently dropped any element
// that struct did not model -- hostdev passthrough, TPM/launchSecurity,
// <qemu:commandline> and similar less-common features -- with nothing to
// indicate anything had gone wrong until whatever that configuration was
// for turned out to be missing on a failed-over target.
//
// That round-trip is gone: those functions now patch a parsed tree (see
// domxml.go), so unmodelled content survives by construction rather than by
// the struct happening to model it. This check is kept as a tripwire. It
// should never fire again, and if it does, the patching path is losing
// something and that is worth hearing about at once.
//
// This check can't tell a genuine loss apart from a legitimate omission
// (an empty or default-valued element the struct correctly normalizes
// away on marshal), so it only warns rather than failing the sync outright
// -- but it turns an otherwise completely invisible risk into something an
// operator can actually notice and investigate the first time it would
// matter for a real domain, instead of silent, permanent, possibly
// never-discovered configuration loss.
//
// missingXMLElements compares element occurrence counts, not full element
// content, so it also can't see a surviving instance quietly losing an
// attribute (a <disk> keeping its tag but dropping a driver/cache setting,
// say) -- only that an instance of some element name went missing entirely.
// That narrower class of loss is real but is much harder to check for
// without false-positiving on libvirt's own legitimate attribute
// normalization on marshal (auto-assigned addresses, inserted defaults, and
// the like), so it's a known, accepted gap rather than something this
// function attempts.
// expected names elements this particular call asked to have removed. They
// are filtered out before warning, because an element the caller deleted BY
// NAME is not evidence that the patching path lost anything -- it is the
// patching path doing exactly what it was told.
//
// Without this, UpdateSyncMetadata warns on every successful sync of a domain
// that has ever been a replication source: it removes replica_targets (and the
// promotion record) from the metadata it derives for the TARGET, because those
// fields describe a domain acting as a source and are meaningless, and
// actively misleading, on a replica. That is the same "guaranteed, permanent
// false positive on every such run" that intentionallyDroppedXMLElements
// exists to prevent, differing only in that the set varies per call and so
// cannot be a package-level list.
//
// It stayed hidden until the metadata writer was fixed. RecordReplicaTarget
// could not write replica_targets at all while virDomainSetMetadata was
// failing, so the field was never there to be removed and the tripwire never
// fired.
func warnIfXMLElementsDropped(context, original, rewritten string, expected ...string) {
	missing := missingXMLElements(original, rewritten)
	if len(expected) > 0 {
		skip := make(map[string]bool, len(expected))
		for _, name := range expected {
			skip[name] = true
		}
		kept := missing[:0]
		for _, name := range missing {
			if !skip[name] {
				kept = append(kept, name)
			}
		}
		missing = kept
	}
	if len(missing) == 0 {
		return
	}
	trace.Warning("domain xml elements present before this rewrite are missing afterward -- this rewrite preserves everything it is not explicitly changing, so something in the patching path has dropped configuration; verify the affected domain's definition still has everything you expect", "context", context, "missing_elements", strings.Join(missing, ", "))
}

// metadataFieldNameRe is the set of field names SetMetadataFields accepts
// from a caller. Deliberately narrower than what XML itself permits in an
// element name: every field vmsync writes is lowercase ASCII with
// underscores (see metadataFieldOrder), so there is nothing to gain from
// accepting the full Unicode NCName grammar, and a conservative pattern is
// far easier to be confident is safe.
var metadataFieldNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$`)

// SetMetadataFields operates on a whole domain document and is now used by
// exactly one caller: UpdateSyncMetadata, whose output feeds DefineDomain.
//
// Everything that MUTATES metadata on an existing domain goes through
// SetDomainMetadataFields instead, which uses libvirt's own metadata API and
// never reconstructs the domain document. This one survives because the
// target's definition is genuinely rebuilt from the source's XML on every
// sync -- there the round-trip is the operation, not a side effect of
// recording a field. See metadata.go.
//
// SetMetadataFields merges the given vmsync:field->value pairs into
// domainXML's <metadata> block, preserving any existing vmsync fields not
// mentioned in updates or removeFields (and any unrelated, non-vmsync
// metadata some other tool may have added) untouched. Fields named in
// removeFields are dropped entirely -- winning over updates if a field
// somehow appears in both -- used to strip metadata that's become
// semantically meaningless for a domain's current role (e.g. a target's
// last_checkpoint/failure_count once that domain becomes a replication
// SOURCE instead, which has no checkpoint chain of its own to report).
//
// A field name in updates that isn't a safe XML element name is rejected
// here rather than written out. buildMetadataEntry interpolates field names
// straight into the tag it emits ("<vmsync:" + field), and unlike the
// values -- which go through xml.EscapeText -- an element NAME has no
// escaping available: a name containing a space, '>', '/' or a leading
// digit simply cannot be expressed, and would produce malformed XML that
// the Marshal below (or DomainDefineXML further downstream) rejects with a
// confusing parse error pointing at the whole domain document rather than
// at the offending key. This used to be structurally impossible, because
// buildMetadataEntry only ever emitted names from the fixed
// metadataFieldOrder list and silently dropped anything else; that changed
// when it started emitting unrecognized fields too, so that
// SetMetadataFields could keep its promise to preserve fields it doesn't
// know about. This check is what the fixed list used to provide for free.
//
// Only caller-supplied names are checked. Names recovered from the domain's
// existing metadata by allMetadataFields are already valid XML element
// names by construction -- they came from a parsed document, and
// xml.StartElement.Name.Local can't even carry the namespace colon --
// so validating those too would risk rejecting, and thereby destroying,
// metadata written by a newer vmsync that this build is supposed to
// preserve untouched.
func SetMetadataFields(domainXML string, updates map[string]string, removeFields ...string) (string, error) {
	for field := range updates {
		if !metadataFieldNameRe.MatchString(field) {
			return "", fmt.Errorf("invalid vmsync metadata field name %q: must start with a letter or underscore and contain only letters, digits, '_', '.' or '-'", field)
		}
	}

	changed, err := setMetadataFieldsInDoc(domainXML, updates, removeFields...)
	if err != nil {
		return "", err
	}

	// Kept as a tripwire rather than removed. It should now never fire --
	// nothing here reconstructs the document any more -- so if it ever does,
	// something in the patching path is dropping content and that is worth
	// hearing about immediately.
	//
	// removeFields is handed over so the fields this call deliberately
	// deleted are not reported as losses; see warnIfXMLElementsDropped.
	warnIfXMLElementsDropped("SetMetadataFields", domainXML, changed, removeFields...)
	return changed, nil
}

// UpdateSyncMetadata records a fresh checkpoint/timestamp, resets
// failure_count to 0, and records sourceHost:sourceDomain as this
// (target) domain's current replica_source -- called on the TARGET's new
// definition once a sync completes successfully.
//
// Note what domainXML actually is at the only call site: the SOURCE's XML.
// DefineDomain replaces the target's persistent definition with one derived
// from it, so whatever metadata this function leaves in place becomes the
// target's metadata, and anything the target used to carry is gone. That
// makes the removeFields list below load-bearing rather than tidy-up:
//
//   - replica_targets and the promotion fields describe the SOURCE. Letting
//     them ride along stamps them onto the replica, so a target would claim
//     to replicate to the source's own targets, and a target of a domain
//     that was once promoted would claim to have been promoted itself.
//
//   - replication_role is the worst of them. It is the interlock
//     TargetRoleAllowsSync enforces, and carrying the source's value across
//     means that after a direction inversion -- where the new source
//     legitimately carries role=source -- the first sync stamps `source`
//     onto the new target, and every subsequent sync is refused with an
//     error telling the operator to check whether the URIs are reversed:
//     advice that is exactly backwards for someone who has just
//     deliberately reversed them.
//
// targetRole is the role the TARGET itself carries, read by the caller
// immediately before this and re-checked against TargetRoleAllowsSync. It
// is written back explicitly so a deliberate -update-role=target survives a
// sync; empty means the target had no role, and the field is then removed
// rather than inherited, preserving the property that vmsync never assigns
// a role on its own.
// replicaWrittenAt is this run's per-disk write record (see
// MetadataFieldReplicaWrittenAt), or "" when there is none.
//
// SET-OR-REMOVE, never merely omitted, and that is not symmetry for its own
// sake. This function transforms the SOURCE's XML into what the target will
// be defined as, so a field it does not mention is whatever the SOURCE
// happened to carry -- and a source can legitimately still carry a stale
// replica_written_at from an earlier life as somebody's replica. Omitting
// the key would stamp that onto this target, so the next run would compare
// this host's disks against mtimes taken on a different host at a different
// time.
// syncRedefineStrips is what a successful sync's full redefine removes from the
// definition it builds.
//
// UpdateSyncMetadata merges the SOURCE's XML and defines the TARGET, so this is
// not tidiness: any field NOT named here is INHERITED by the target from the
// source. That is the direction that matters, and it is the opposite of
// promotionFields' -- there the danger is stripping too much, here it is
// stripping too little.
//
// A package-level list for the same reason promotionFields is one: these lists
// ARE the mechanism, "forgot one of the strip lists" is this codebase's
// documented failure mode for target-only fields (see
// MetadataFieldReplicaWrittenAt, which names every place), and a list nothing
// can enumerate is a list nothing can check. Its caller clones it, because two
// entries are added per run depending on what that run found.
var syncRedefineStrips = []string{
	// A successful define means the target has accepted last_checkpoint,
	// so nothing is pending. Cleared HERE rather than in a separate
	// write so that accepting and clearing are one atomic
	// DomainDefineXML: never both set, never neither.
	// Cleared on every successful sync. Safe without a parameter because
	// a domain carrying a verify failure refuses to sync at all, so any
	// run reaching here either verified and passed or came through a
	// recovery path meant to clear it. See MetadataFieldVerifyState.
	MetadataFieldVerifyState,
	MetadataFieldVerifyFailedAt,
	MetadataFieldPendingCheckpoint,
	// Cleared HERE and nowhere else on the success path, for exactly the
	// reason pending_checkpoint is: reaching this function means the copy
	// finished and is about to be recorded, so accepting the new replica
	// and withdrawing the warning about the old one are ONE atomic
	// DomainDefineXML. A separate write afterwards could leave a healthy
	// replica permanently refusing promotion, and a separate write before
	// could leave a half-written one accepted.
	//
	// Unconditional, like verify_state: this function merges into the
	// SOURCE's XML, so a source that was once somebody's replica can
	// carry a stale value of its own, and omitting the key would stamp
	// that onto a target the copy has just proved complete.
	MetadataFieldReplicaIncomplete,
	MetadataFieldReplicaTargets,
	MetadataFieldPromotedAt,
	MetadataFieldPromotedBy,
	MetadataFieldPromotedFrom,
	MetadataFieldPromotionMode,
	// Stripped here even though it is the one promotion field meant to
	// OUTLIVE the promotion, and for the same reason the four above are:
	// this function merges the SOURCE's XML and defines the TARGET, so
	// leaving the key out would copy the source's history onto a replica
	// that has none -- a copy that never served live would start
	// refusing restores because the domain it was copied FROM once did.
	//
	// Losing the target's own trace here is not a hole, because a domain
	// still carrying it cannot reach this function: the trace only exists
	// alongside paused/fenced/promoted, and TargetRoleAllowsSync refuses
	// all three. The only way to a syncable role is a deliberate
	// -update-role target, which clears the trace itself.
	MetadataFieldLastPromotedAt,
	MetadataFieldFenceID,
	MetadataFieldFenceSource,
	MetadataFieldFenceArmedAt,
	MetadataFieldFenceArmedBy,
}

func UpdateSyncMetadata(domainXML, checkpoint, sourceHost, sourceDomain, targetRole string, checkpointAtUnix int64, sourceStopped bool, replicaWrittenAt, autostartIntent string) (string, error) {
	updates := map[string]string{
		MetadataFieldLastCheckpoint: checkpoint,
		MetadataFieldLastSync:       strconv.FormatInt(time.Now().Unix(), 10),
		MetadataFieldFailureCount:   "0",
		MetadataFieldReplicaSource:  ReplicaEntry(sourceHost, sourceDomain),
		MetadataFieldCheckpointAt:   strconv.FormatInt(checkpointAtUnix, 10),
		// Always SET, never merged through, and refreshed on every single
		// sync. Two reasons it cannot be allowed to arrive by merge: this
		// function merges into the SOURCE's XML, so a source that was once
		// somebody's replica still carries its own old intent -- which
		// describes a different domain entirely -- and an operator who
		// changes the source's autostart expects the record to follow within
		// one sync rather than at the next full copy.
		MetadataFieldAutostartIntent: autostartIntent,
	}
	// A clone, because the conditional appends below must not grow the shared
	// list: every sync would otherwise inherit the last one's decisions about
	// source_stopped_at_sync and replica_written_at.
	remove := append([]string(nil), syncRedefineStrips...)
	// Recorded only when true, and actively removed otherwise: a stale "the
	// source was stopped" from an earlier sync would make a later promotion
	// claim a verified zero it has no right to.
	if sourceStopped {
		updates[MetadataFieldSourceStoppedAtSync] = "1"
	} else {
		remove = append(remove, MetadataFieldSourceStoppedAtSync)
	}
	// Same shape, and see the parameter's own note above for why the
	// removal branch is load-bearing rather than tidiness.
	if replicaWrittenAt != "" {
		updates[MetadataFieldReplicaWrittenAt] = replicaWrittenAt
	} else {
		remove = append(remove, MetadataFieldReplicaWrittenAt)
	}
	if targetRole == "" {
		remove = append(remove, MetadataFieldReplicationRole)
	} else {
		updates[MetadataFieldReplicationRole] = targetRole
	}
	return SetMetadataFields(domainXML, updates, remove...)
}

// ReplicaEntry formats a host+domain pair the same way on both sides of a
// replica_source/replica_targets metadata field, so the two are directly
// comparable (e.g. by replicaListContains) and consistently readable by a
// human or external tooling inspecting either domain's XML by hand.
func ReplicaEntry(host, domain string) string {
	return host + ":" + domain
}

// The verbs MetadataFieldReplicaIncomplete records, and the only ones
// ReplicaIncompleteValue will build a value for.
//
// A closed set rather than a free string, because the verb is what the
// refusal names back to an operator standing in front of a broken replica:
// "a reinit was started" tells them where to look, and an unrecognised word
// invented at a call site tells them nothing. Refusing an unknown one also
// means a typo at a call site fails the ARMING write, which returns before
// anything is destroyed, rather than writing a value the refusal cannot
// explain.
const (
	// ReplicaIncompleteVerbReinit is a plain -reinit: the replica's disks are
	// renamed aside and rebuilt from scratch.
	ReplicaIncompleteVerbReinit = "reinit"
	// ReplicaIncompleteVerbForceClean is -reinit -force-clean, which
	// additionally removes the target DOMAIN. Distinct because the two leave
	// different wreckage behind: after a force-clean the domain carrying this
	// value may itself be gone.
	ReplicaIncompleteVerbForceClean = "force-clean"
	// ReplicaIncompleteVerbFullSync is an ordinary sync that computed no
	// parent checkpoint, so it writes base images directly with no overlay.
	// No disks are renamed aside on this path, so it records no aside stamp.
	ReplicaIncompleteVerbFullSync = "full-sync"
	// ReplicaIncompleteVerbRestore is a restore point being put back over the
	// replica in place.
	ReplicaIncompleteVerbRestore = "restore"
)

// replicaIncompleteMaxBytes caps the whole value. Metadata is spliced into a
// production domain's persistent definition, and an unbounded field there is
// an unbounded write on every arming; 512 bytes is far more than the grammar
// can legitimately need and small enough that no domain document notices it.
const replicaIncompleteMaxBytes = 512

// replicaIncompleteHostUnsafe matches every character a hostname cannot
// legitimately contain, which is exactly the set that would corrupt the
// grammar or the XML around it.
//
// Stripped rather than refused. The host is the least load-bearing part of
// the value -- the refusal fires on the field's PRESENCE, and the verb, the
// time and the aside stamp are what make it actionable -- so a strange
// hostname must never be the reason a replica goes unarmed and a half-written
// promotion is accepted.
var replicaIncompleteHostUnsafe = regexp.MustCompile(`[^A-Za-z0-9._:-]`)

// replicaIncompleteActionIDRe matches an action id this value will carry.
//
// WIDER THAN HEX ON PURPOSE, although the engine's own minted ids are hex.
// The ids that actually arrive here come from vmsync-agent, which stamps an
// operation's id from the control plane and deliberately permits hyphens,
// dots and underscores -- its own degraded run-id form uses them. Refusing
// those would refuse the ARMING WRITE, which fails the whole reinit before it
// starts: a correlation id must never be the reason a replica goes
// unprotected, so this only has to be wide enough to admit every id the
// estate really produces and narrow enough that none of them can break the
// grammar or the XML.
var replicaIncompleteActionIDRe = regexp.MustCompile(`^[0-9A-Za-z._-]+$`)

// replicaIncompleteActionIDMax bounds an id before it is carried, matching
// vmsync-agent's own bound on what it will pass. An id near it is already
// wrong, and a longer one would eat into the host name, which is the part a
// person reads during a disaster.
const replicaIncompleteActionIDMax = 64

// replicaIncompleteDigits matches an aside stamp: a unix second, or nothing.
var replicaIncompleteDigits = regexp.MustCompile(`^[0-9]*$`)

// ReplicaIncompleteValue builds the single-line value
// MetadataFieldReplicaIncomplete carries:
//
//	verb=<reinit|force-clean|full-sync|restore>,at=<unix>,action=<hex>,host=<replica-host>,aside=<unix>
//
// for example
//
//	verb=reinit,at=1758441600,action=9f3c1a2b4d5e6f70,host=hv-a,aside=1758441600
//
// Pure, so the one string standing between an interrupted copy and a
// promotion that accepts it can be tested exhaustively without libvirt.
//
// atUnix is when the copy STARTED, which is what the refusal quotes back.
// actionID correlates this value with the run's journal and its log, and is
// omitted entirely when the caller has none OR when the id could not be
// carried safely -- an unarmed field would be far worse than an unattributed
// one, so no correlation id ever stops the write. asideStamp is the suffix
// the displaced disks were renamed with
// (".vmsync-replaced-<stamp>"), omitted on the paths that rename nothing:
// naming a suffix that does not exist would send an operator looking for
// files that were deleted.
//
// host is the machine that STARTED the copy and was expected to finish it --
// the one to go and look at when nothing did. On a restore, which runs
// locally on the replica's own host, that is the replica host itself.
//
// REFUSES an unknown verb, a non-positive time and a non-numeric aside
// stamp. Those three are call-site errors rather than anything an operator or
// a control plane supplies, so refusing costs nothing real -- and it is safe
// here precisely because the value is written BEFORE anything is destroyed,
// so the caller returns having touched nothing. An unusable ACTION ID is the
// deliberate exception: it arrives over the network, so it is dropped rather
// than made into a reason to leave a replica unprotected.
//
// The grammar deliberately contains no XML-special character, so the value
// needs no escaping to survive the metadata element it is spliced into.
func ReplicaIncompleteValue(verb string, atUnix int64, actionID, host string, asideStamp string) (string, error) {
	switch verb {
	case ReplicaIncompleteVerbReinit, ReplicaIncompleteVerbForceClean,
		ReplicaIncompleteVerbFullSync, ReplicaIncompleteVerbRestore:
	default:
		return "", fmt.Errorf("replica_incomplete: unknown verb %q -- it must be one of %s, %s, %s or %s, because the verb is what the promotion refusal names back to whoever has to recover from the interrupted copy",
			verb, ReplicaIncompleteVerbReinit, ReplicaIncompleteVerbForceClean,
			ReplicaIncompleteVerbFullSync, ReplicaIncompleteVerbRestore)
	}
	if atUnix <= 0 {
		return "", fmt.Errorf("replica_incomplete: refusing to record a copy as starting at %d -- the refusal quotes this time back to an operator, and a zero would tell them the copy started in 1970", atUnix)
	}
	// DROPPED, never refused, and never escaped. An id that cannot survive
	// the grammar -- a comma, an equals sign, a newline, or one long enough
	// to crowd out the host -- would make the whole value unreadable, and an
	// unreadable value refuses with no verb, no host and no aside stamp. But
	// refusing to BUILD the value would be worse still: this is written
	// before anything is destroyed, so a bad id would abort the reinit
	// outright. A marker with no correlation id is a smaller loss than no
	// marker, and a far smaller one than a marker nobody can read. Escaping
	// was rejected because it puts the burden on every reader of the value,
	// for ever. vmsync-agent applies the same rule on its side and for the
	// same reason.
	// The drop is visible without being logged here, which keeps this
	// function pure: the caller logs the whole value it wrote, so an id that
	// did not survive is missing from a line that shows everything else.
	if actionID != "" && (len(actionID) > replicaIncompleteActionIDMax || !replicaIncompleteActionIDRe.MatchString(actionID)) {
		actionID = ""
	}
	if !replicaIncompleteDigits.MatchString(asideStamp) {
		return "", fmt.Errorf("replica_incomplete: aside stamp %q is not a unix second -- it names the .vmsync-replaced-<stamp> files an operator is told to recover from, so a wrong one sends them looking for files that do not exist", asideStamp)
	}

	host = replicaIncompleteHostUnsafe.ReplaceAllString(host, "")

	build := func(h string) string {
		v := "verb=" + verb + ",at=" + strconv.FormatInt(atUnix, 10)
		if actionID != "" {
			v += ",action=" + actionID
		}
		if h != "" {
			v += ",host=" + h
		}
		if asideStamp != "" {
			v += ",aside=" + asideStamp
		}
		return v
	}

	value := build(host)
	if len(value) <= replicaIncompleteMaxBytes {
		return value, nil
	}
	// Host first, because it is the only part an operator can recover
	// without: the verb, the time and the aside suffix are what turn this
	// refusal into a one-line recovery, and truncating any of them would cost
	// more than a shortened hostname does. Cut to fit rather than dropped
	// outright, so a long name still identifies its host by its prefix.
	over := len(value) - replicaIncompleteMaxBytes
	if over < len(host) {
		return build(host[:len(host)-over]), nil
	}
	if value = build(""); len(value) <= replicaIncompleteMaxBytes {
		return value, nil
	}
	// A tripwire rather than a reachable path: with the verb from a closed
	// set, the time and the aside stamp bounded by their own formats and the
	// action id bounded above, nothing left can make the value this long.
	// Kept because if it ever does fire, something has widened one of those
	// bounds without widening the cap, and that is worth hearing about at the
	// arming write -- which destroys nothing -- rather than at the promotion.
	return "", fmt.Errorf("replica_incomplete: the value is %d bytes with no host at all, over the %d-byte cap -- the action id (%d bytes) is the only thing left that can be that long, so it is wrong",
		len(value), replicaIncompleteMaxBytes, len(actionID))
}

// replicaListContains reports whether entry is already present in a
// comma-separated replica_targets-style list. Pure and side-effect-free
// so the exact dedup logic RecordReplicaTarget depends on is directly
// testable without a live domain.
func replicaListContains(list, entry string) bool {
	if list == "" {
		return false
	}
	for _, e := range strings.Split(list, ",") {
		if e == entry {
			return true
		}
	}
	return false
}

// appendReplicaTarget adds entry to a comma-separated replica_targets-style
// list, returning list unchanged if entry is already present -- so
// repeated syncs to the same target never grow the list, only a genuinely
// new distinct target does.
func appendReplicaTarget(list, entry string) string {
	if list == "" {
		return entry
	}
	if replicaListContains(list, entry) {
		return list
	}
	return list + "," + entry
}

// RecordReplicaTarget updates the SOURCE domain's own persistent
// definition (sourceDomainName, looked up on mgr) to add targetHost:
// targetDomain to its replica_targets metadata list (deduplicated -- a
// repeat sync to the same target is a no-op), and strips
// last_checkpoint/last_sync_timestamp/failure_count/replica_written_at from
// it if present: those fields describe a domain's state as a replication TARGET,
// and are meaningless -- and actively misleading to a human or external
// tool reading this domain's XML -- once it's acting as a SOURCE instead,
// which this call establishes it as. This is the one place vmsync ever
// writes to the source's own definition; unlike DefineDomain, it never
// undefines anything first, since it's the exact same domain (same name,
// same UUID) being patched in place -- DomainDefineXML updates a
// persistent definition that already matches by UUID directly, the same
// safe, already-established pattern RecordTargetSyncFailure uses to patch
// a live target's failure_count.
func RecordReplicaTarget(mgr *Manager, sourceDomainName, targetHost, targetDomain string, at time.Time) error {
	existing, err := ReadDomainMetadataField(mgr, sourceDomainName, MetadataFieldReplicaTargets)
	if err != nil {
		return err
	}
	entry := ReplicaEntry(targetHost, targetDomain)
	updatedList := appendReplicaTarget(existing, entry)

	// This used to return early once the target was already recorded and no
	// stale target-role fields remained, to skip an XML round-trip and a
	// domain redefine per sync. That shortcut is gone, deliberately: the
	// whole point of last_replicated_at is that it moves on EVERY successful
	// sync, so there is no steady state left to skip.
	//
	// What made dropping it affordable is that this is no longer a redefine
	// at all. The write goes through libvirt's own metadata API and touches
	// only vmsync's namespaced element, so a per-sync write to a PRODUCTION
	// domain costs a small metadata splice rather than a full-document
	// round-trip that could drop configuration this tool does not model.
	return SetDomainMetadataFields(mgr, sourceDomainName, map[string]string{
		MetadataFieldReplicaTargets:   updatedList,
		MetadataFieldLastReplicatedAt: strconv.FormatInt(at.Unix(), 10),
		MetadataFieldLastReplicatedTo: entry,
		// replica_written_at joins the strip list for exactly the reason the
		// other three are on it: it describes a domain's life as somebody's
		// TARGET, and this domain is acting as a SOURCE. Left behind it is
		// meaningless to anyone reading the XML, and worse, it is what
		// UpdateSyncMetadata would inherit onto a real replica.
	}, recordReplicaTargetStrips...)
}

// recordReplicaTargetStrips is the set of TARGET-only fields
// RecordReplicaTarget removes from a domain it is establishing as a SOURCE.
//
// Named rather than written inline at the call site so that one test can read
// it. "Forgot one of the strip lists" is this codebase's documented failure
// mode for target-only fields -- see MetadataFieldReplicaWrittenAt, which
// names all four places -- and a list nothing can enumerate is a list nothing
// can check.
var recordReplicaTargetStrips = []string{
	MetadataFieldLastCheckpoint,
	MetadataFieldLastSync,
	MetadataFieldFailureCount,
	MetadataFieldReplicaWrittenAt,
	MetadataFieldPendingCheckpoint,
	MetadataFieldVerifyState,
	MetadataFieldVerifyFailedAt,
	// A copy interrupted while this domain was somebody's replica says
	// nothing about it as a source, and left behind it is what
	// UpdateSyncMetadata would inherit onto a real replica -- which would
	// refuse a promotion of a replica nothing was ever copying.
	MetadataFieldReplicaIncomplete,
	// NOT MetadataFieldLastPromotedAt, which is not a target-only field: a
	// source that reached that role by being promoted and then inverted has
	// genuinely served live, and the record of it belongs to the domain for
	// the rest of its life. See the field's own comment.
}

// ReadTargetFailureCount reconnects to the target and returns the
// failure_count currently recorded in its domain metadata. Returns 0 (no
// error) if the target domain genuinely doesn't exist yet (ERR_NO_DOMAIN)
// or exists but has no such field -- any other lookup error (a transient
// connection blip, a permissions problem, libvirtd being temporarily
// unreachable) is a real failure and must not be conflated with "doesn't
// exist," or -reinit-after-failures's threshold comparison silently sees 0
// instead of the real count on every call that happens to race a transient
// error.
func ReadTargetFailureCount(targetURI, targetDomain string) (int, error) {
	mgr, err := Connect(targetURI)
	if err != nil {
		return 0, fmt.Errorf("reconnect target libvirt: %w", err)
	}
	defer mgr.Close()

	dom, err := mgr.Conn.LookupDomainByName(targetDomain)
	if err != nil {
		if lvErr, ok := err.(libvirt.Error); ok && lvErr.Code == libvirt.ERR_NO_DOMAIN {
			return 0, nil
		}
		return 0, fmt.Errorf("look up target domain %s: %w", targetDomain, err)
	}
	defer dom.Free()

	// DOMAIN_XML_INACTIVE, matching ReadReplicationRole and every write:
	// RecordTargetSyncFailure stores this counter with AFFECT_CONFIG, so it
	// only ever appears in the persistent definition. Flags 0 hands back the
	// LIVE definition of a running domain instead, which no config write
	// reaches -- and the case that most needs this counter read correctly is
	// exactly a target that was promoted and is now running.
	domXML, err := dom.GetXMLDesc(libvirt.DOMAIN_XML_INACTIVE)
	if err != nil {
		return 0, fmt.Errorf("read target domain xml: %w", err)
	}
	value, err := ParseMetadata(domXML, MetadataFieldFailureCount)
	if err != nil || value == "" {
		return 0, nil
	}
	count, err := strconv.Atoi(value)
	if err != nil {
		return 0, nil
	}
	return count, nil
}

// TargetRoleAllowsSync reports whether a domain carrying the given
// replication_role may be written to as a sync target, returning a nil
// error when it may and an explanatory one when it may not.
//
// An empty role means no role has ever been recorded and is ALLOWED. This
// is deliberate and load-bearing: every domain in every deployment that
// predates this field has no role, and failing closed on them would break
// replication everywhere the moment this version is installed. Roles are
// opt-in, and vmsync never assigns one on its own -- an automatic "stamp
// target on whatever I just synced into" would be convenient, but it would
// also mean a single misdirected invocation permanently marks the wrong
// domain, and it would fight an operator who deliberately set something
// else. Setting a role is an explicit administrative act (-update-role).
//
// An UNRECOGNIZED role is refused rather than ignored. A role this build
// doesn't know is most likely one a newer vmsync wrote, and treating it as
// "no opinion" would silently discard exactly the protection it was set to
// provide. Failing closed on an unknown value is the safe direction: it
// costs a clear error and a version upgrade, where the alternative costs
// data.
//
// Kept as a standalone, pure function so this decision is directly
// testable without a live libvirt connection -- it is the single point
// standing between a scheduled sync and overwriting a promoted, live
// domain, so it is worth being able to assert exhaustively.
func TargetRoleAllowsSync(role string) error {
	switch role {
	case "", RoleTarget:
		return nil
	case RoleSource:
		return fmt.Errorf("%w: target domain is marked replication_role=%q, meaning it is the SOURCE of a replication pair -- syncing into it would overwrite the original with its own replica; check that -source-uri/-target-uri are not reversed, or run -update-role=%s if the direction has genuinely changed", ErrRoleRefusesSync, RoleSource, RoleTarget)
	case RolePromoted:
		return fmt.Errorf("%w: target domain is marked replication_role=%q, meaning it was failed over to and is now serving live -- refusing to overwrite it with a replica from the old source, whether or not it happens to be running at this moment; run -update-role=%s to deliberately turn it back into a replication target (its current disk contents will be discarded)", ErrRoleRefusesSync, RolePromoted, RoleTarget)
	case RolePaused:
		return fmt.Errorf("%w: target domain is marked replication_role=%q, so replication into it is administratively suspended -- run -update-role=%s to resume", ErrRoleRefusesSync, RolePaused, RoleTarget)
	case RoleFenced:
		// Its own message, not RolePaused's. Both refuse, but they call for
		// opposite things: a paused replica is waiting for its operator to
		// resume it, while a fenced one was displaced by a peer that is now
		// serving -- so resuming the sync in the SAME direction is very likely
		// the wrong repair, and would overwrite the copy that took over.
		return fmt.Errorf("%w: target domain is marked replication_role=%q, meaning a fence stopped it because a peer was promoted over it -- syncing into it now would resume replication in a direction that may already have reversed. If the failover stands, run -invert to reverse the pair; if the fence was wrong, run -update-role=%s to make this a replication target again", ErrRoleRefusesSync, RoleFenced, RoleTarget)
	default:
		return fmt.Errorf("%w: target domain has an unrecognized replication_role=%q -- refusing to sync into a domain whose role this vmsync build does not understand (it was most likely written by a newer version; upgrade, or run -update-role=%s to override)", ErrRoleRefusesSync, role, RoleTarget)
	}
}

// ErrRoleRefusesSync marks every refusal TargetRoleAllowsSync produces, so a
// caller can tell "this domain's role forbids replicating into it" from "the
// sync tried and broke".
//
// It exists for the failure counter. -reinit-after-failures counts consecutive
// sync failures and, at its threshold, forces a full resync to auto-heal a
// broken incremental chain -- and a role refusal is not that. It is a
// deliberate administrative state, it says nothing about whether the
// incremental mechanism works, and a reinit cannot heal it because this same
// gate refuses the reinit too. Counting it does active harm in two directions:
// the counter climbs forever against a domain nobody is trying to sync, and a
// non-zero failure_count is itself one of the things that blocks a promotion
// (pkg/failover's evidence check) -- so a paused replica an operator restored
// in order to promote would become unpromotable purely because the scheduler
// kept being told no.
//
// main() already exempts run-lock contention from the same counter on the same
// reasoning: another vmsync holding the lock is not a broken sync either.
var ErrRoleRefusesSync = errors.New("replication role does not permit syncing into this domain")

// VerifyStateFailed is the only value MetadataFieldVerifyState ever takes.
// See that field for why presence is the state.
const VerifyStateFailed = "failed"

// ErrVerifyStateRefusesSync reports that this domain carries a recorded
// verification failure, so vmsync will not sync into it.
//
// Its own sentinel rather than reusing ErrRoleRefusesSync, because callers
// treat them alike in one respect and must not in another: both are
// administrative refusals exempt from failure_count, but only this one is
// cleared by proving the replica again.
var ErrVerifyStateRefusesSync = errors.New("a recorded verification failure does not permit syncing into this domain")

// TargetVerifyStateAllowsSync refuses a sync into a domain whose last
// verification found its contents differing from the source.
//
// Refusing is what keeps the finding alive. A successful sync rebuilds the
// target's definition from the SOURCE's XML (see UpdateSyncMetadata), so any
// target-only field it does not explicitly write is lost -- which would have
// meant tonight's ordinary incremental quietly erasing a verify failure
// recorded this morning, and the promotion gate then seeing a clean replica.
//
// It also refuses a plain -reinit, which is the less obvious half. A reinit
// does recopy everything, so it plausibly REPAIRS the replica -- but it does
// not verify it, so allowing it would move the domain from "known bad" to
// "assumed good, unverified" while clearing the record that said otherwise.
// That is precisely the state evidenceProblems exists to distrust.
//
// Two deliberate acts get past it: -verify-failure-reinit, which recopies
// and then proves the result before clearing anything, and -force-clean,
// which is already the documented override for a wedged target and logs that
// it discarded a finding.
func TargetVerifyStateAllowsSync(verifyState, failedAt string) error {
	if verifyState == "" {
		return nil
	}
	when := "at an unrecorded time"
	if failedAt != "" {
		if unix, err := strconv.ParseInt(failedAt, 10, 64); err == nil {
			when = "on " + time.Unix(unix, 0).UTC().Format(time.RFC3339)
		}
	}
	return fmt.Errorf("%w: -verify found this replica's contents differing from its source %s, and the finding has not been cleared -- the replica is NOT trustworthy for a promotion. See that run's log for which blocks differed (this metadata deliberately records only the verdict). Recover with -verify-failure-reinit, which recopies and then re-verifies before clearing this, or override with -force-clean if you have decided the replica is disposable",
		ErrVerifyStateRefusesSync, when)
}

// THERE IS DELIBERATELY NO TargetReplicaIncompleteAllowsSync, and this is
// where a reader will come looking for one.
//
// MetadataFieldReplicaIncomplete refuses a PROMOTION, never a sync. The two
// gates above exist because syncing into those domains would destroy
// something (a paused operator's work, a finding about the replica's
// contents); this field means the opposite -- a full copy was started and
// never finished, so the replica on those disks is half-written and a sync is
// the only thing that repairs it. Gating a sync on it would wedge the one
// path that heals the replica, and wedge it precisely on the runs that most
// need to be re-run: the interrupted ones. The next successful sync clears
// the field as part of recording its own success (see UpdateSyncMetadata), so
// the refusal lifts itself without anybody having to override anything.

// TargetRoleAllowsRestore is TargetRoleAllowsSync's counterpart for putting a
// restore point back over a replica in place.
//
// It differs on exactly one value, and the difference is the point. A sync into
// a PAUSED domain is refused because pausing means "stop replicating into this"
// -- but restoring is not replicating, and an operator who paused replication
// to work out what went wrong is precisely the operator who then wants to roll
// the replica back. Making them run -update-role=target first would re-arm
// every scheduled sync against a replica they are mid-way through repairing,
// which is the opposite of what pausing was for. A restore leaves the domain
// paused afterwards (see restorepoint.MetadataPlan), so allowing it here does
// not weaken the interlock; it keeps a paused domain paused.
//
// source and promoted are refused for the same reasons as a sync, with the same
// cure. promoted is the one that matters: a domain failed over to and then shut
// down for maintenance passes every runtime check there is, and its disks are
// live data that a restore would overwrite with an old replica's.
//
// Pure and standalone, mirroring TargetRoleAllowsSync, so the one decision
// standing between an operator's typo and a live domain's disks is directly
// testable without libvirt.
func TargetRoleAllowsRestore(role string) error {
	switch role {
	case "", RoleTarget, RolePaused:
		return nil
	case RoleFenced:
		// Refused, although it looks like `paused` above: neither is replicating
		// and both are stopped.
		//
		// `paused` is accepted for a good reason -- an operator who paused
		// replication to work out what went wrong is exactly the one who then
		// wants to roll the replica back. But a `fenced` domain is not a replica
		// somebody paused. It is the copy a peer was promoted OVER, which means
		// its disks hold whatever it was serving at the instant of the failover --
		// data no restore point of its own contains, because it was the source
		// when that point was taken. Rolling it back discards that, and unlike a
		// promoted copy it carries no last_promoted_at to refuse on, because it
		// was never promoted: it was displaced.
		//
		// Nor is a restore any documented cure for this state. A fence means the
		// pair's direction has probably reversed, and the two answers are about
		// direction, not about contents.
		return fmt.Errorf("target domain is marked replication_role=%q, meaning a peer was promoted over it and a fence stopped it -- its disks hold what it was serving when that happened, which no restore point of its own contains, so rolling it back would discard exactly the data somebody may still need. If the failover stands, run -invert to reverse the pair; if the fence was wrong, run -update-role=%s first and then restore deliberately",
			RoleFenced, RoleTarget)
	case RoleSource:
		return fmt.Errorf("target domain is marked replication_role=%q, meaning it is the SOURCE of a replication pair -- restoring a restore point over it would overwrite the original with an old copy of its own replica; check that -target-uri/-target-domain name the replica and not the source", RoleSource)
	case RolePromoted:
		return fmt.Errorf("target domain is marked replication_role=%q, meaning it was failed over to and is now serving live -- refusing to overwrite live data with a restore point, whether or not it happens to be running at this moment; run -update-role=%s first if this domain is genuinely a replica again (its current disk contents will then be discarded)", RolePromoted, RoleTarget)
	default:
		return fmt.Errorf("target domain has an unrecognized replication_role=%q -- refusing to restore over a domain whose role this vmsync build does not understand (it was most likely written by a newer version; upgrade rather than overriding)", role)
	}
}

// ErrServedLive marks every refusal ServedLiveAllowsOverwrite produces, so a
// caller can tell "this copy has served live and nobody has released it" from
// a role refusal or a genuine failure.
//
// Its own sentinel rather than ErrRoleRefusesSync for the reason that one is
// not ErrVerifyStateRefusesSync: the three are cleared by different things,
// and only this one needs a human to say out loud that the data is disposable.
// It shares the exemption from failure_count for the same reason as the
// others -- a refusal is not a broken sync, and a reinit cannot heal it.
var ErrServedLive = errors.New("this domain has served live and has not been released")

// ServedLiveAllowsOverwrite refuses an operation that would destroy the disk
// contents of a domain that has been promoted at some point and never released.
//
// This is the check the role gates cannot make. A role says what a domain is
// FOR right now, and every route out of `promoted` rewrites it: a clean
// shutdown records paused, a fence records fenced, -update-role records
// whatever was asked. So by the time an operator reaches for a restore or a
// force-clean, the copy that spent a week serving production looks exactly
// like a replica that has never done anything -- and TargetRoleAllowsRestore
// deliberately permits paused and fenced, because rolling back a paused
// replica is normally the right thing to do. The distinction those gates are
// missing is not the role. It is whether these particular disks were ever
// live, and that is what the trace records.
//
// what names the operation in the message ("restore a restore point over it",
// "force-clean it"), because the correct next step is the same for all of them
// and the thing an operator needs to read back is which of their commands was
// stopped.
//
// Pure and standalone, like TargetRoleAllowsSync and TargetRoleAllowsRestore,
// so the decision standing between one wrong command and a week of production
// data is testable without libvirt.
func ServedLiveAllowsOverwrite(lastPromotedAt, what string) error {
	if lastPromotedAt == "" {
		return nil
	}
	when := "at an unrecorded time"
	if unix, err := strconv.ParseInt(lastPromotedAt, 10, 64); err == nil {
		when = "on " + time.Unix(unix, 0).UTC().Format(time.RFC3339)
	}
	return fmt.Errorf("%w: this domain was promoted %s, so its disks hold data that served live and may be the only copy of it -- refusing to %s. If that data is genuinely disposable, say so with -%s (which refuses while the domain is running) and then re-run this command; if it is not, -invert reverses the pair and keeps it",
		ErrServedLive, when, what, FlagReleasePromotion)
}

// FlagReleasePromotion is the name of the one flag that clears the durable
// promotion trace, held here rather than in main so every refusal message can
// name it without the engine and the CLI drifting apart.
//
// One flag, and deliberately not a field on anything the control plane can
// send. The console can compute that a release is needed and print this
// command; it cannot issue it. That is the whole design: every other guard in
// vmsync can be satisfied by a button, and this is the one that requires
// somebody to have typed the words.
const FlagReleasePromotion = "release-promotion"

// ReleasePromotionTrace clears a domain's durable promotion trace, which is
// the operator's statement that the data this copy served is disposable.
//
// Refuses while the domain is RUNNING. A release is only ever wanted in order
// to overwrite the disks afterwards, and a running domain is the one case
// where that is certainly wrong -- and where it is also most tempting, since a
// copy still serving traffic is exactly the state an operator mistakes for
// "the failover I meant to undo".
//
// Refuses while the role is still `promoted`, for a narrower reason: the
// demotion carries the trace forward (see SetReplicationRole), so releasing
// first and demoting second would simply write it again. Making the order
// explicit avoids an operator concluding the flag does not work.
//
// Returns the value that was cleared, so the caller can log and journal what
// was given up rather than just that something was.
func ReleasePromotionTrace(mgr *Manager, domainName string) (released string, err error) {
	dom, err := mgr.Conn.LookupDomainByName(domainName)
	if err != nil {
		return "", fmt.Errorf("look up domain %s: %w", domainName, err)
	}
	active, activeErr := dom.IsActive()
	dom.Free()
	if activeErr != nil {
		return "", fmt.Errorf("domain %s: could not determine whether it is running, and releasing a promotion record on a running domain is refused: %w", domainName, activeErr)
	}
	if active {
		return "", fmt.Errorf("domain %s is RUNNING -- refusing to release its promotion record while it is serving; shut it down first (-shutdown-domain), confirm it is the copy you mean to discard, and re-run", domainName)
	}

	role, err := ReadReplicationRole(mgr, domainName)
	if err != nil {
		return "", err
	}
	if role == RolePromoted {
		return "", fmt.Errorf("domain %s is still marked replication_role=%s -- demote it first (-update-role=%s or -update-role=%s); a demotion re-records the promotion, so releasing before demoting would have no effect", domainName, RolePromoted, RolePaused, RoleFenced)
	}

	released, err = ReadDomainMetadataField(mgr, domainName, MetadataFieldLastPromotedAt)
	if err != nil {
		return "", err
	}
	if released == "" {
		return "", nil // nothing recorded; a no-op, not an error
	}
	if err := SetDomainMetadataFields(mgr, domainName, nil, MetadataFieldLastPromotedAt); err != nil {
		return "", err
	}
	return released, nil
}

// ValidateRole reports whether role is one a caller may ask
// SetReplicationRole to store. Shared by the -update-role flag's own
// pre-flight check and by SetReplicationRole itself, so the CLI can reject
// a typo without a libvirt round trip while the accepted set stays defined
// in exactly one place. Note that "" is NOT accepted here: clearing the
// field is spelled RoleNone, so that an empty flag value (i.e. -update-role
// never passed at all) can never be mistaken for a request to clear.
func ValidateRole(role string) error {
	switch role {
	case RoleSource, RoleTarget, RolePromoted, RolePaused, RoleFenced, RoleNone:
		return nil
	default:
		return fmt.Errorf("invalid replication role %q: must be one of %s", role, strings.Join(ValidRoles, ", "))
	}
}

// ReadReplicationRole returns the replication_role recorded on a domain, or
// "" when the domain has no role recorded. A domain that does not exist at
// all is likewise reported as "" with a nil error: a target that hasn't
// been created yet cannot have been promoted, so it has nothing to protect
// and the first full sync must be free to create it.
func ReadReplicationRole(mgr *Manager, domainName string) (string, error) {
	dom, err := mgr.Conn.LookupDomainByName(domainName)
	if err != nil {
		if lvErr, ok := err.(libvirt.Error); ok && lvErr.Code == libvirt.ERR_NO_DOMAIN {
			return "", nil
		}
		return "", fmt.Errorf("look up domain %s to read its replication role: %w", domainName, err)
	}
	defer dom.Free()

	// DOMAIN_XML_INACTIVE for the same reason DefineDomain uses it: the
	// role lives in the persistent definition, and reading the live one
	// would mix in runtime-only elements irrelevant here.
	domXML, err := dom.GetXMLDesc(libvirt.DOMAIN_XML_INACTIVE)
	if err != nil {
		return "", fmt.Errorf("read domain %s xml to get its replication role: %w", domainName, err)
	}
	role, err := ParseMetadata(domXML, MetadataFieldReplicationRole)
	if err != nil {
		return "", nil
	}
	return role, nil
}

// ReadPromotionTrace returns the durable record that a domain has served live
// (MetadataFieldLastPromotedAt), or "" when it never has.
//
// A domain that does not exist at all is reported as "" with a nil error, for
// the same reason ReadReplicationRole does it: a target that has not been
// created yet cannot have been promoted, so it has nothing to protect and the
// first full sync must be free to create it. Its own function rather than
// ReadDomainMetadataField at each call site precisely because of that
// tolerance -- the generic reader treats a missing domain as a lookup failure,
// which on this path would refuse every first sync of every new pair.
func ReadPromotionTrace(mgr *Manager, domainName string) (string, error) {
	dom, err := mgr.Conn.LookupDomainByName(domainName)
	if err != nil {
		if lvErr, ok := err.(libvirt.Error); ok && lvErr.Code == libvirt.ERR_NO_DOMAIN {
			return "", nil
		}
		return "", fmt.Errorf("look up domain %s to read its promotion history: %w", domainName, err)
	}
	defer dom.Free()

	// DOMAIN_XML_INACTIVE, matching every other metadata read: the field is
	// written with AFFECT_CONFIG, so a running domain's live document -- which
	// is exactly the document a promoted copy has -- would not carry it.
	domXML, err := dom.GetXMLDesc(libvirt.DOMAIN_XML_INACTIVE)
	if err != nil {
		return "", fmt.Errorf("read domain %s xml to get its promotion history: %w", domainName, err)
	}
	// Unlike ReadReplicationRole, an unparsable domain document is RETURNED as
	// an error rather than flattened to "": it is indistinguishable from a
	// document carrying a trace that says this copy served production, and the
	// caller's job on this path is to refuse rather than to proceed on a
	// blank. An ABSENT field is not that case -- ParseMetadata reports it as
	// ("", nil), which is the ordinary answer for nearly every domain.
	served, err := ParseMetadata(domXML, MetadataFieldLastPromotedAt)
	if err != nil {
		return "", fmt.Errorf("read domain %s %s: %w", domainName, MetadataFieldLastPromotedAt, err)
	}
	return served, nil
}

// ReadDomainAutostart reports a domain's real autostart flag, and whether it
// could be read at all.
//
// ok=false with a nil error is the "asked and could not tell" case that
// AutostartIntentUnknown exists to record: a domain that does not exist yet
// (the first sync of a new pair reads the SOURCE, so this is rare, but a
// racing undefine is not impossible), or a libvirt that answered with an
// error. Neither is worth failing a sync over -- the replica is still correct,
// only the intent is unknown -- so the caller records "unknown" and carries on
// rather than aborting a good copy over a boot flag.
func ReadDomainAutostart(mgr *Manager, domainName string) (autostart, ok bool) {
	dom, err := mgr.Conn.LookupDomainByName(domainName)
	if err != nil {
		return false, false
	}
	defer dom.Free()

	on, err := dom.GetAutostart()
	if err != nil {
		return false, false
	}
	return on, true
}

// setDomainAutostart writes a domain's real autostart flag, and is a no-op
// when it already holds the wanted value.
//
// The read-before-write is not an optimisation. libvirt implements autostart
// as a symlink under /etc/libvirt/qemu/autostart/, so setting it is a
// filesystem operation that can fail on a read-only or full /etc while the
// domain itself is perfectly healthy -- and on the overwhelmingly common path
// (every sync, re-asserting "off" on a replica that is already off) there is
// nothing to write at all. Skipping the write when it would change nothing
// means that failure mode cannot be reached by a run that had no work to do.
func setDomainAutostart(mgr *Manager, domainName string, want bool) (changed bool, err error) {
	dom, err := mgr.Conn.LookupDomainByName(domainName)
	if err != nil {
		return false, fmt.Errorf("look up domain %s to set its autostart flag: %w", domainName, err)
	}
	defer dom.Free()

	current, err := dom.GetAutostart()
	if err != nil {
		return false, fmt.Errorf("read domain %s autostart flag: %w", domainName, err)
	}
	if current == want {
		return false, nil
	}
	if err := dom.SetAutostart(want); err != nil {
		return false, fmt.Errorf("set domain %s autostart to %v: %w", domainName, want, err)
	}
	return true, nil
}

// ApplyAutostartForRole makes a domain's real autostart flag agree with what
// its replication role says it should be, using the recorded intent for the
// one role that restores it.
//
// Returns changed=false both when the flag already agreed and when the role is
// one vmsync does not manage -- see autostartForRole for which, and why a
// source's boot behaviour is left to its operator.
func ApplyAutostartForRole(mgr *Manager, domainName, role, intent string) (changed bool, err error) {
	want, managed := autostartForRole(role, intent)
	if !managed {
		return false, nil
	}
	return setDomainAutostart(mgr, domainName, want)
}

// ReadVerifyState returns the verify_state and verify_failed_at recorded on a
// domain, both "" when there is no recorded verification failure -- which is
// the case for every healthy replica. A domain that does not exist is likewise
// reported as empty with a nil error, for the same reason
// ReadReplicationRole does it: nothing has been verified into it yet.
//
// Both fields come out of ONE XML read, not two. Read separately they could
// straddle a concurrent write and report a state with no timestamp, or a
// timestamp for a finding that had just been cleared -- and this pair is read
// specifically to decide whether to refuse a sync, so a torn read of it is a
// wrong answer to the only question being asked.
func ReadVerifyState(mgr *Manager, domainName string) (verifyState, failedAt string, err error) {
	dom, err := mgr.Conn.LookupDomainByName(domainName)
	if err != nil {
		if lvErr, ok := err.(libvirt.Error); ok && lvErr.Code == libvirt.ERR_NO_DOMAIN {
			return "", "", nil
		}
		return "", "", fmt.Errorf("look up domain %s to read its verification state: %w", domainName, err)
	}
	defer dom.Free()

	// DOMAIN_XML_INACTIVE, as everywhere else that reads these fields: they
	// live in the persistent definition.
	domXML, err := dom.GetXMLDesc(libvirt.DOMAIN_XML_INACTIVE)
	if err != nil {
		return "", "", fmt.Errorf("read domain %s xml to get its verification state: %w", domainName, err)
	}
	// A parse error means the field is absent, matching ReadReplicationRole.
	// Absent is the overwhelmingly common case here -- it is what every
	// replica that has never failed a verify looks like -- so it must not be
	// an error condition.
	verifyState, _ = ParseMetadata(domXML, MetadataFieldVerifyState)
	failedAt, _ = ParseMetadata(domXML, MetadataFieldVerifyFailedAt)
	return verifyState, failedAt, nil
}

// SetReplicationRole records role as domainName's replication_role,
// leaving the rest of its definition untouched. role must be one of
// ValidRoles; RoleNone removes the field entirely rather than storing the
// literal string "none", returning the domain to the no-role-recorded
// state TargetRoleAllowsSync treats as permission to proceed.
//
// Returns the role that was previously recorded ("" if none), so a caller
// can report the transition rather than just the destination.
//
// Uses the same read-modify-re-read-write shape as RecordTargetSyncFailure,
// and for the same reason: libvirt's domain-definition API has no atomic
// compare-and-swap, so this re-reads the domain's XML immediately before
// writing and refuses if it changed underneath -- narrowing the window in
// which a concurrent `virsh define`, another orchestration layer, or a
// second vmsync could have its write silently discarded. That matters more
// here than for a failure counter: losing a promoted marker is exactly the
// failure this whole field exists to prevent.
// promotionFields is what moving away from `promoted` takes with it.
//
// Those fields describe a failover that is, by that very call, no longer in
// force. Leaving them behind lets a domain carry replication_role=target
// alongside a promoted_at and a promoted_from -- a combination no promotion
// ever wrote, which anything reasoning about "was this displaced, and by whom"
// would read as fact. The one documented remedy for an unwanted promotion is
// -update-role=target (see TargetRoleAllowsSync's own message), so this is the
// common path, not an edge case.
//
// Only UpdateSyncMetadata and an inversion stripped them before, and the first
// of those runs only after a SUCCESSFUL sync -- which a domain stuck
// mid-recovery is precisely not getting.
//
// A package-level list rather than a literal inside SetReplicationRole, for the
// reason recordReplicaTargetStrips gives: "forgot one of the strip lists" is
// this codebase's documented failure mode for these fields, and a list nothing
// can enumerate is a list nothing can check. What must be checked here is an
// ABSENCE, which is the one thing a live-domain test cannot demonstrate -- see
// TestPromotionFieldsDoNotStripTheTrace.
var promotionRecordFields = []string{
	MetadataFieldPromotedAt,
	MetadataFieldPromotedBy,
	MetadataFieldPromotedFrom,
	MetadataFieldPromotionMode,
	// MetadataFieldLastPromotedAt is DELIBERATELY ABSENT from this list, and
	// the absence is the entire point of the field.
	//
	// Everything above describes a failover in force and goes when the failover
	// does. The trace describes one that HAPPENED, and outliving the demotion is
	// its only job: without it, SetReplicationRole is itself the hole -- the
	// console's own advice is to shut a promoted copy down and then clean it,
	// and the shutdown records `paused` through this very list, taking every
	// trace of the promotion with it. Two operations later the copy that served
	// production for a week is an ordinary paused replica that
	// -restore-restore-point and -force-clean will both overwrite.
	//
	// It is cleared by exactly one thing, ReleasePromotionTrace, reached by
	// exactly one flag. Do not add it here, and do not add it to a strip list
	// for tidiness: a strip on any path an operator can reach by accident puts
	// the hole back.
}

// fenceTokenFields is the credential a promotion armed against the source it
// displaced: the thing that authorises the agent on THAT host to stop THAT
// domain, once, when it comes back.
//
// Its own list, separate from the promotion record, because the two do not
// always die together and treating them as one was a real hole. A promotion
// record describes a failover that is over the moment the role changes. The
// token describes a RELATIONSHIP -- "this copy is displacing that one" -- and
// whether that relationship is over depends on which role the domain moves to:
//
//   - to paused, fenced, target or none: over. This copy is not the live one any
//     more, so it is not displacing anybody, and a token left behind is a
//     credential authorising a shutdown nothing justifies.
//   - to SOURCE: NOT over. `source` says this copy is the authoritative one --
//     the same claim `promoted` made -- and `-update-role source` rewrites only
//     this end, so the domain it displaced is still out there still calling
//     itself a source. Stripping the token there destroyed the only record of an
//     arrangement that was still in force, on the one transition the console
//     offers for a live promoted copy.
//
// Note what keeping it does NOT do: the agent's sweep requires role=promoted to
// act (AssessFence), so a token on a `source` is inert until an -invert or a
// re-promotion resolves the pair. What it preserves is the RECORD -- readable by
// -read-fence and -explain-domain -- rather than the action.
var fenceTokenFields = []string{
	MetadataFieldFenceID,
	MetadataFieldFenceSource,
	MetadataFieldFenceArmedAt,
	MetadataFieldFenceArmedBy,
}

// promotionFields is what a transition out of `promoted` strips in the ordinary
// case: the record and the token together. Kept as one name because that is what
// every arm but promoted->source applies, and because the pairing is the common
// case rather than the exception.
var promotionFields = append(append([]string{}, promotionRecordFields...), fenceTokenFields...)

// strippedPromotionFields is what a transition FROM previous TO role removes of
// the promotion record and the fence token.
//
// Pure, and its own function, because it is the difference between two lists
// that look interchangeable and are not: the record is always dropped, the token
// only when the relationship it describes has ended. Inline in the write switch
// this was a single `promotionFields...` that quietly destroyed a live
// credential on the one transition the console offers a serving copy.
func strippedPromotionFields(previous, role string) []string {
	if previous == RolePromoted && role == RoleSource {
		return promotionRecordFields
	}
	return promotionFields
}

// droppedFenceToReport is the fence id a role change discarded, or "" when it
// discarded none: what SetReplicationRole returns and logs.
//
// Pure so the reporting rule has a test of its own. It was inline, which made
// the one decision in this file that clause 2 of the audit is about the one
// decision nothing could check.
//
// TrimSpace because a whitespace-only value is not a token, and reporting one as
// a discarded credential would send an operator hunting for a fence that never
// existed.
func droppedFenceToReport(previous, role, fenceID string) string {
	id := strings.TrimSpace(fenceID)
	if id == "" {
		return ""
	}
	// Nothing was stripped, so nothing was dropped: the promoted arm applies no
	// strip list at all, and promoted->source deliberately keeps the token.
	if role == RolePromoted {
		return ""
	}
	if !slices.Contains(strippedPromotionFields(previous, role), MetadataFieldFenceID) {
		return ""
	}
	return id
}

// ErrDemotesLiveCopy marks the refusal demotionRefusedWhileActive produces, so a
// caller can tell "this domain is serving and you are recording that it is not"
// from a metadata write that broke.
var ErrDemotesLiveCopy = errors.New("refusing to record a RUNNING promoted domain as anything but the live copy")

// domainIsActive answers "is this domain running" by name, for the callers that
// hold only a Manager. A domain that does not exist is reported as not running
// rather than as an error, matching ReadReplicationRole: there is no guest to
// protect.
func domainIsActive(mgr *Manager, domainName string) (bool, error) {
	dom, err := mgr.Conn.LookupDomainByName(domainName)
	if err != nil {
		if lvErr, ok := err.(libvirt.Error); ok && lvErr.Code == libvirt.ERR_NO_DOMAIN {
			return false, nil
		}
		return false, fmt.Errorf("look up domain %s: %w", domainName, err)
	}
	defer dom.Free()
	return DomainActive(dom)
}

// demotionRefusedWhileActive reports whether a role transition must be refused
// because it would record a domain that is SERVING RIGHT NOW as something other
// than the copy that serves.
//
// SetReplicationRole read no runtime state at all before this. It is the single
// place every role transition passes through, so it was also the single place a
// live promoted copy could be relabelled by one command -- and the relabel takes
// the fence token with it (see promotionFields), which is the credential that
// authorises stopping the source when it comes back. One `-update-role paused`,
// or one click of the console's "Shut down cleanly" against a guest that ignores
// ACPI, and a domain still taking writes was recorded as an administratively
// paused replica with no fence and nothing watching it.
//
// TWO destinations are still allowed, for opposite reasons, and neither is a
// loophole:
//
//   - `source` is the only value that remains TRUE of a running copy. It says
//     "this is the authoritative copy of its pair", which is what a promoted
//     domain is -- the label changes, the claim does not. It is also the
//     documented way to keep a copy that took over (the console's own advice,
//     and what an operator reaches for short of a full -invert), so refusing it
//     would close the one route that loses no data. The fence token survives
//     this transition precisely because the claim does; see fenceTokenFields.
//   - `fenced` is allowed because it is the honest record of a fence that FAILED
//     to stop the guest, and CI-07 exists because that record is the only thing
//     that keeps replication out of the resulting split brain and the only thing
//     the fenced-and-running alarms can key on. Refusing it would reintroduce
//     exactly the silence roleToRecord was written to prevent, from the other
//     side: a hand-run -fence-domain against a running promoted copy would leave
//     it marked `promoted` and running, which is the NORMAL healthy
//     post-failover state and alarms on nothing.
//
// Every other value says "something else is the live copy", which is false while
// this one is serving.
//
// -invert is unaffected because it does not come through here: it writes both
// ends with ApplyMetadata, having made its own checks about both. That is
// deliberate -- reversing a pair is exactly the case where a running promoted
// domain legitimately stops being promoted.
//
// Pure, like roleToRecord and fenceFailedOpen, because it is a small rule about
// a live production domain and no test could reach it through a real libvirt.
func demotionRefusedWhileActive(previous, role string, active bool) bool {
	if !active || previous != RolePromoted || role == RolePromoted {
		return false
	}
	return role != RoleSource && role != RoleFenced
}

// promotionTraceUpdate decides what a role transition must write to keep the
// durable promotion trace correct. It returns the value to store in
// MetadataFieldLastPromotedAt, or "" for "write nothing".
//
// Pure, and its own function, because it is the decision that keeps a copy which
// served production from being overwritten, it has five cases, and not one of
// them is reachable by a test without a live libvirt holding a real promoted
// domain. Same reasoning as roleToRecord and fenceFailedOpen.
//
// The cases, and why each is what it is:
//
//   - BECOMING promoted with no trace recorded -> now. `-update-role promoted`
//     is a deliberate statement that this copy is the one serving, and a copy
//     brought up that way is no less dangerous to overwrite than one -promote
//     brought up.
//
//   - BECOMING promoted with a trace already there -> nothing. An existing
//     record wins, and re-running the command must not push the time forward.
//
//   - LEAVING promoted with a trace already there -> nothing. The strip about to
//     happen does not touch it (see promotionFields), so there is nothing to do.
//
//   - LEAVING promoted with NO trace -> promoted_at, else now. This is the
//     migration, and it is the whole reason this case exists: a domain promoted
//     by a binary that predated the field carries promoted_at and no trace, and
//     the demotion is the LAST moment anything knows it was promoted. Those
//     domains are precisely the population the mechanism exists to protect, so
//     the value is carried across rather than assumed to be there. A promotion
//     whose time was never recorded at all -- an old `-update-role promoted` by
//     hand -- is stamped with the demotion's own time: the field's job is to
//     answer "did this serve live", and answering that an hour late beats not
//     answering it.
//
//   - anything else -> nothing. In particular, pausing or fencing an ORDINARY
//     replica must not invent a promotion history for it, which would refuse
//     every restore and force-clean against a domain that never served anything.
func promotionTraceUpdate(previous, role, existingTrace, promotedAt string, nowUnix int64) string {
	if existingTrace != "" {
		return ""
	}
	switch {
	case role == RolePromoted && previous != RolePromoted:
		return strconv.FormatInt(nowUnix, 10)
	case previous == RolePromoted && role != RolePromoted:
		if promotedAt != "" {
			return promotedAt
		}
		return strconv.FormatInt(nowUnix, 10)
	}
	return ""
}

func SetReplicationRole(mgr *Manager, domainName, role string) (previous, droppedFence string, err error) {
	if err := ValidateRole(role); err != nil {
		return "", "", err
	}

	// Everything this function decides from, in ONE read: the current role, the
	// two fields the promotion trace's lifecycle needs, and the fence token it
	// may be about to strip. Five separate reads were five LookupDomain plus
	// GetXMLDesc pairs against a host that may be mid-incident.
	before, err := ReadDomainMetadataFields(mgr, domainName,
		MetadataFieldReplicationRole, MetadataFieldLastPromotedAt, MetadataFieldPromotedAt,
		MetadataFieldFenceID, MetadataFieldFenceSource)
	if err != nil {
		return "", "", err
	}
	previous = before[MetadataFieldReplicationRole]

	// Is this domain SERVING right now? Asked only for a transition out of
	// `promoted`, which is the only one where the answer changes anything, so an
	// ordinary role change still costs no runtime query.
	//
	// Fails CLOSED, unlike the role read in shutdownAndMark, and the difference
	// is which direction the write protects in. There, the write ADDS a refusal
	// and not being able to read the old role is no reason to withhold it. Here
	// the domain is already `promoted`, which refuses everything there is to
	// refuse -- so declining to change it on an unreadable state leaves the
	// strongest possible interlock in place, and proceeding would remove it
	// without knowing whether a guest is live behind it.
	if previous == RolePromoted && role != RolePromoted {
		active, activeErr := domainIsActive(mgr, domainName)
		if activeErr != nil {
			return "", "", fmt.Errorf("domain %s: could not determine whether it is still running before recording it as %s: %w -- leaving it marked %s, which refuses every sync, restore and clean over it", domainName, role, activeErr, RolePromoted)
		}
		if demotionRefusedWhileActive(previous, role, active) {
			return "", "", fmt.Errorf("%w: domain %s is marked %s and is RUNNING, so recording it as %q would say something else is the live copy while this one is taking writes -- and it would take the fence against the displaced source with it. Shut it down first (-shutdown-domain) and then record %q, or -update-role=%s if this copy IS the primary now, or -invert to reverse the pair",
				ErrDemotesLiveCopy, domainName, RolePromoted, role, role, RoleSource)
		}
	}

	// The durable promotion trace, decided by promotionTraceUpdate rather than
	// here for the reason roleToRecord and fenceFailedOpen exist: it is a small
	// rule with several cases, all of them about whether a copy's disks stay
	// protected, and none of them reachable by a test that needs a live libvirt
	// holding a real promoted domain. Inline it would be a decision table
	// nothing could check.
	updates := map[string]string{}
	if v := promotionTraceUpdate(previous, role, before[MetadataFieldLastPromotedAt], before[MetadataFieldPromotedAt], time.Now().Unix()); v != "" {
		updates[MetadataFieldLastPromotedAt] = v
	}
	// Whatever it decided goes in the SAME SetDomainMetadataFields call as the
	// role write and the strip, so there is never a window in which the
	// promotion record is gone and the trace is not yet there.
	switch {
	case role == RoleNone:
		err = SetDomainMetadataFields(mgr, domainName, updates,
			append([]string{MetadataFieldReplicationRole}, strippedPromotionFields(previous, role)...)...)
	case role == RolePromoted:
		// Promotion itself is written by -promote, which records the whole
		// record atomically. Setting the role to promoted by hand must not
		// invent one, but must not destroy an existing one either -- so
		// promotionFields is deliberately not applied here.
		updates[MetadataFieldReplicationRole] = role
		err = SetDomainMetadataFields(mgr, domainName, updates)
	default:
		// The promotion record always goes. The fence token goes with it EXCEPT
		// on promoted->source, where the arrangement it describes is still in
		// force: `source` makes the same claim `promoted` did, and
		// `-update-role source` rewrites only this end, so the domain this copy
		// displaced is still out there calling itself a source. See
		// fenceTokenFields.
		updates[MetadataFieldReplicationRole] = role
		err = SetDomainMetadataFields(mgr, domainName, updates, strippedPromotionFields(previous, role)...)
	}
	if err != nil {
		return "", "", err
	}

	// An armed fence token must never go SILENTLY, and until now it did.
	//
	// Where it dies, dying is correct: a domain that is no longer the live copy
	// is not displacing anybody, and a token left on one is a credential
	// authorising a shutdown nothing justifies. What was wrong is that nothing
	// said so. The token is the only thing that authorises the agent on the
	// displaced source's host to stop that source when it comes back, its loss is
	// invisible in every other field, and NO role change restores it --
	// re-promoting by hand writes no promotion record (see the RolePromoted arm)
	// and `-promote -fence-source` is refused on a domain already marked
	// `source`, so the way back is `-update-role promoted` and then
	// `-promote -fence-source`. An operator not told has no way to know there was
	// anything to re-arm.
	//
	// Reported after the write rather than before, and as an ERROR on a command
	// that succeeded, for the reason -release-promotion logs the way it does:
	// this line is the last trace of the token, so it has to be the one an
	// operator finds. Returned as well, so the callers can journal it -- a
	// terminal is not a record.
	if droppedFence = droppedFenceToReport(previous, role, before[MetadataFieldFenceID]); droppedFence != "" {
		trace.Error("this role change DISCARDED an armed fence token: nothing now authorises stopping the displaced source if it comes back, and no role change restores it. If the failover still stands, re-arm with -update-role promoted followed by -promote -fence-source against this domain",
			"vm", domainName, "from_role", previous, "to_role", role,
			"fence_id", droppedFence, "fence_named_source", before[MetadataFieldFenceSource])
	}

	// Autostart follows the role, and this is the single place every role
	// transition passes through -- an inversion, a fence, -update-role, a
	// promotion by hand. Enforcing it here rather than at each of those call
	// sites is what makes the invariant hold for a caller nobody has written
	// yet.
	//
	// ORDER: the metadata is written first and the flag second, which is the
	// safe way round for the direction that matters. Every role this manages
	// except `promoted` turns autostart OFF, and by the time we get here the
	// domain is already recorded as a replica -- so a failure below leaves a
	// domain marked target/paused/fenced that might still autostart, which is
	// visible in its own metadata and fixable with one command. The reverse
	// order would risk the opposite on promotion: a domain that autostarts
	// with nothing recording that it was promoted.
	//
	// The error is returned, but `previous` is returned WITH it: the role
	// change did happen, and a caller that reported only the failure would
	// have an operator believe the transition did not occur.
	intent, intentErr := ReadDomainMetadataField(mgr, domainName, MetadataFieldAutostartIntent)
	if intentErr != nil {
		// Not fatal on its own: an unreadable intent is exactly the
		// AutostartIntentUnknown case, and every role except `promoted`
		// ignores the intent entirely.
		intent = AutostartIntentUnknown
	}
	if _, err := ApplyAutostartForRole(mgr, domainName, role, intent); err != nil {
		return previous, droppedFence, fmt.Errorf("domain %s was recorded as %s but its autostart flag could not be set to match: %w", domainName, role, err)
	}
	return previous, droppedFence, nil
}

// RecordTargetSyncFailure reconnects to the target, increments
// failure_count in its domain metadata (leaving the rest of its definition
// untouched) and returns the new count. A target domain that genuinely
// doesn't exist yet (ERR_NO_DOMAIN) has nothing to record against and is
// treated as a no-op -- but any other lookup error is a real failure and
// must be propagated, not swallowed the same way: silently no-op'ing this
// increment on a transient connection blip means a real, consecutive sync
// failure never gets counted, which can keep -reinit-after-failures from
// ever tripping its threshold at all.
//
// The run-lock (pkg/util/lock.go) is keyed only by source domain, so it
// gives this function no protection at all against a concurrent writer of
// the *target* domain -- another vmsync invocation misconfigured to point
// at the same target, or any external tool (virsh define, another
// orchestration layer) redefining it. libvirt's domain-definition API has
// no atomic compare-and-swap primitive to close that race outright, so
// instead this re-reads the domain's XML immediately before writing and
// refuses to proceed if it no longer matches what the increment above was
// actually computed from -- narrowing the window from this whole
// function's read-then-write span down to a single extra round-trip, and
// turning what would otherwise be a silent, last-write-wins clobber into a
// loud, diagnosable error.
func RecordTargetSyncFailure(targetURI, targetDomain string) (int, error) {
	mgr, err := Connect(targetURI)
	if err != nil {
		return 0, fmt.Errorf("reconnect target libvirt: %w", err)
	}
	defer mgr.Close()

	dom, err := mgr.Conn.LookupDomainByName(targetDomain)
	if err != nil {
		if lvErr, ok := err.(libvirt.Error); ok && lvErr.Code == libvirt.ERR_NO_DOMAIN {
			return 0, nil
		}
		return 0, fmt.Errorf("look up target domain %s: %w", targetDomain, err)
	}
	defer dom.Free()

	// The whole read-modify-write is now confined to vmsync's own metadata
	// element. It used to read the entire domain definition and define the
	// result back, which mattered here more than anywhere: the case that
	// most needs a failure recorded is a target that has been promoted and
	// is RUNNING, and rewriting a live domain's persistent definition from
	// a typed round-trip is how configuration goes missing.
	// readDomainMetadataFields, not allMetadataFields: what comes back here is
	// the single element virDomainGetMetadata returned, not a <metadata> body,
	// and the two are read by deliberately different rules. Reading a fragment
	// with the document reader is how this counter got stuck at 1 -- every
	// increment read the stored value as absent and re-recorded 1, so
	// -reinit-after-failures never reached its threshold.
	fields, err := readDomainMetadataFields(dom)
	if err != nil {
		return 0, fmt.Errorf("target domain %s: %w", targetDomain, err)
	}

	current := 0
	if value := fields[MetadataFieldFailureCount]; value != "" {
		if n, convErr := strconv.Atoi(value); convErr == nil {
			current = n
		}
	}
	next := current + 1

	if err := SetDomainMetadataFields(mgr, targetDomain, map[string]string{
		MetadataFieldFailureCount: strconv.Itoa(next),
	}); err != nil {
		return 0, err
	}
	return next, nil
}

func DetectNvram(domainXML string) (string, error) {
	domcfg := &libvirtxml.Domain{}
	err := domcfg.Unmarshal(domainXML)
	if err != nil {
		return "", err
	}
	if domcfg.OS != nil && domcfg.OS.NVRam != nil {

		nvram := domcfg.OS.NVRam
		return nvram.NVRam, nil

	}
	return "", nil
}

// TargetNvramPath is where a replica's UEFI varstore belongs, given the
// source's.
//
// libvirt derives this path from the domain NAME --
// /var/lib/libvirt/qemu/nvram/<name>_VARS.fd -- so a replica that inherits the
// source's path verbatim ends up with a varstore named after a different
// domain. When the two names match that is invisible and correct, because the
// inherited path is exactly what libvirt would have chosen anyway. When they
// differ it is a collision waiting to happen: if the target host ever also runs
// a domain genuinely called that, the two share one file and each boot
// overwrites the other's boot entries and Secure Boot keys.
//
// So the name is rewritten, for the same reason and in the same place as the
// disk paths. DefineDomain already repaths every disk for the target; leaving
// the varstore pointing at the source's filename was the inconsistency, not the
// rewrite.
//
// The BASENAME only, and only when it actually contains the source's name:
//
//   - /var/lib/libvirt/qemu/nvram/web01_VARS.fd -> .../web01-dr_VARS.fd.
//     libvirt's own layout, rewritten to what libvirt would have picked.
//   - /srv/uefi/web01.fd -> /srv/uefi/web01-dr.fd. A custom DIRECTORY is the
//     operator's choice and is preserved; the filename is still domain-derived
//     and still collides, so it is still rewritten.
//   - /srv/uefi/shared-vars.fd -> unchanged. Nothing here is domain-derived,
//     so there is no name to correct and guessing would override a deliberate
//     choice.
//
// Returns sourceNvram unchanged whenever there is nothing to do, so callers can
// use it unconditionally.
func TargetNvramPath(sourceNvram, sourceDomain, targetDomain string) string {
	if sourceNvram == "" || sourceDomain == "" || targetDomain == "" || sourceDomain == targetDomain {
		return sourceNvram
	}
	dir, base := path.Split(sourceNvram)
	if !strings.Contains(base, sourceDomain) {
		return sourceNvram
	}
	// Once, not every occurrence: a name appearing twice in one filename is
	// not a thing to be clever about, and replacing only the first keeps the
	// result predictable.
	return dir + strings.Replace(base, sourceDomain, targetDomain, 1)
}

func DetectLoader(domainXML string) (string, error) {
	domcfg := &libvirtxml.Domain{}
	err := domcfg.Unmarshal(domainXML)
	if err != nil {
		return "", err
	}
	if domcfg.OS != nil && domcfg.OS.Loader != nil {

		loader := domcfg.OS.Loader
		return loader.Path, nil

	}
	return "", nil
}

func ParseMetadata(domainXML string, metadataField string) (string, error) {
	domcfg := &libvirtxml.Domain{}
	err := domcfg.Unmarshal(domainXML)
	if err != nil {
		return "", err
	}
	if domcfg.Metadata == nil {
		return "", nil
	}

	return parseMetadataValue(domcfg.Metadata.XML, metadataField), nil
}

func ParseMetadataField(domainXML string, field string) (string, error) {
	domcfg := &libvirtxml.Domain{}
	err := domcfg.Unmarshal(domainXML)
	if err != nil {
		return "", err
	}
	if domcfg.Metadata == nil {
		return "", nil
	}

	return parseMetadataValue(domcfg.Metadata.XML, field), nil
}

// buildMetadataEntry renders a full <vmsync:vmsync> block from the given
// field values: known fields (metadataFieldOrder) first, in that fixed
// order, for the stable/readable output every domain vmsync itself writes
// gets -- then any OTHER field present in fields that metadataFieldOrder
// doesn't know about (see allMetadataFields' own doc comment for how such
// a field would get in here at all), sorted alphabetically so that
// unrecognized-field ordering is still deterministic across runs rather
// than depending on Go's randomized map iteration. Fields absent from the
// map are simply omitted.
// buildMetadataFragment renders the naked form, for virDomainSetMetadata to
// bind itself. buildMetadataElement renders the self-binding form, for
// grafting into a domain document. See metadataFragmentStart for why these
// cannot be the same string.
func buildMetadataFragment(fields map[string]string) string {
	return buildMetadataEntry(metadataFragmentStart, metadataFragmentEnd, "", fields)
}

func buildMetadataElement(fields map[string]string) string {
	return buildMetadataEntry(metadataElementStart, metadataElementEnd, metadataPrefix+":", fields)
}

func buildMetadataEntry(open, close, fieldPrefix string, fields map[string]string) string {
	var b strings.Builder
	b.WriteString(open)
	written := make(map[string]bool, len(fields))
	writeField := func(field, value string) {
		b.WriteString("\n  <")
		b.WriteString(fieldPrefix)
		b.WriteString(field)
		b.WriteString(" id=\"")
		_ = xml.EscapeText(&b, []byte(value))
		b.WriteString("\"/>")
		written[field] = true
	}
	for _, field := range metadataFieldOrder {
		if value, ok := fields[field]; ok {
			writeField(field, value)
		}
	}
	extra := make([]string, 0, len(fields)-len(written))
	for field := range fields {
		if !written[field] {
			extra = append(extra, field)
		}
	}
	sort.Strings(extra)
	for _, field := range extra {
		writeField(field, fields[field])
	}
	b.WriteString("\n")
	b.WriteString(close)
	return b.String()
}

// parseMetadataValue returns the id attribute of one vmsync metadata field,
// "" when that field is absent.
//
// Delegates to allMetadataFields rather than walking the document itself.
// The two used to carry their own copies of the same matching rules, and a
// pair like that only has to drift once to leave half of vmsync able to read
// a domain the other half reads as empty.
func parseMetadataValue(metadataXML string, field string) string {
	return allMetadataFields(metadataXML)[field]
}

// allMetadataFields returns every vmsync:field->id-attribute-value pair
// actually present in metadataXML, not just the ones metadataFieldOrder
// happens to enumerate. SetMetadataFields uses this (rather than looking
// up each known field individually, as it used to) specifically so a field
// outside that list -- written by a newer or older vmsync version sharing
// the same target, say, or simply added to metadataFieldOrder after this
// build was compiled -- survives a metadata update instead of silently
// disappearing the moment anything else touches this domain's metadata:
// SetMetadataFields's own doc comment already promises "preserving any
// existing vmsync fields not mentioned in updates or removeFields...
// untouched", a guarantee the old known-fields-only read broke for
// anything not on that list. The wrapping <vmsync:vmsync> element itself
// is excluded -- it's the container, not a field.
func allMetadataFields(metadataXML string) map[string]string {
	fields, _, _ := metadataFields(metadataXML)
	return fields
}

// Two readers, and the difference between them is deliberate.
//
// metadataFields is handed a whole <metadata> BODY, which on any ordinary
// host holds other tools' blocks too -- libosinfo's is usually the first
// child. It must therefore identify vmsync's element and decline everything
// else, because a field harvested from a neighbour's block does not merely
// read wrong: the next merge writes it into vmsync's own element, on the
// source, and every replica made from it afterwards.
//
// metadataFragmentFields is handed the single element virDomainGetMetadata
// returned, which libvirt located BY vmsync's uri. It is ours by
// construction, and it has to be taken on that basis, because the extractor
// deliberately strips the evidence: virXMLExtractNamespaceXML unbinds every
// element in the uri and deletes a declaration of it before serialising, so
// absence of a namespace on the way out says nothing at all about what is
// stored. See metadataFragmentStart.
//
// Both share the field rule, and that rule matches on CONTAINMENT rather than
// on each field's own namespace. It has to: under the spelling libvirt hands
// back, the fields have no namespace. Matching per-field -- what this did --
// read a fully populated domain as empty, and since every writer here is a
// read-modify-write, a field that reads as absent is not preserved but
// dropped from the next write. One unrecognised spelling therefore erases
// replication_role, last_checkpoint and the whole promotion record the next
// time anything touches this domain's metadata for any reason.
//
// Both also report `malformed`, for a shape that parses but cannot be
// trusted -- a nested container, which no version has ever written and which
// would make the fields under it read as an empty-but-valid block. That is
// the one case a merge must refuse rather than treat as "nothing was there".
func metadataFields(metadataXML string) (fields map[string]string, sawContainer, malformed bool) {
	return walkMetadata(metadataXML, func(_ int, el xml.StartElement) bool {
		return isMetadataContainer(el)
	})
}

func metadataFragmentFields(fragment string) (fields map[string]string, sawContainer, malformed bool) {
	return walkMetadata(fragment, func(depth int, el xml.StartElement) bool {
		// depth 2 is the fragment's own root -- 1 is the <metadata> wrapper
		// walkMetadata puts around it. The name is still checked, so a
		// wildly unexpected return is refused rather than mined for fields.
		return (depth == 2 && el.Name.Local == metadataPrefix) || isMetadataContainer(el)
	})
}

func walkMetadata(metadataXML string, isContainer func(depth int, el xml.StartElement) bool) (map[string]string, bool, bool) {
	fields := map[string]string{}
	sawContainer, malformed := false, false
	decoder := xml.NewDecoder(strings.NewReader("<metadata>" + metadataXML + "</metadata>"))
	depth, containerDepth := 0, -1
	for {
		token, err := decoder.Token()
		if err != nil {
			return fields, sawContainer, malformed
		}
		switch el := token.(type) {
		case xml.StartElement:
			depth++
			if isContainer(depth, el) {
				sawContainer = true
				if containerDepth > 0 {
					malformed = true
				} else {
					containerDepth = depth
				}
				continue
			}
			// Direct children only. vmsync's fields are a flat list, so
			// anything deeper belongs to a structure this version does not
			// write and must not be flattened into a field beside them.
			inContainer := containerDepth > 0 && depth == containerDepth+1
			if !inContainer && el.Name.Space != metadataNamespace {
				continue
			}
			// Never a field called `vmsync`. Writing one back would nest a
			// second container inside the first, which every later read
			// reports as malformed -- a trap this would otherwise set for
			// itself.
			if el.Name.Local == metadataPrefix {
				continue
			}
			for _, attr := range el.Attr {
				// An unprefixed id, specifically: a `t:id` belongs to
				// whatever declared `t`, and taking it would make the value
				// depend on attribute order.
				if attr.Name.Local == "id" && attr.Name.Space == "" {
					fields[el.Name.Local] = attr.Value
					break
				}
			}
		case xml.EndElement:
			if depth == containerDepth {
				containerDepth = -1
			}
			depth--
		}
	}
}

// isMetadataContainer reports whether el is vmsync's own <vmsync> element, in
// any spelling it can arrive in.
//
// The rule is: an element named `vmsync` that either RESOLVES to vmsync's
// namespace or DECLARES it. Resolving alone is not enough, because libvirt
// hands the element back in a state where it resolves to nothing:
//
//	<vmsync xmlns:vmsync="http://vmsync.org/xmlns/libvirt/domain/1.0">
//	  <failure_count id="1"/>
//	</vmsync>
//
// That is a real fragment read off a real domain. The default declaration
// vmsync wrote has become a PREFIXED declaration that nothing in the fragment
// uses, while the tag stayed unprefixed -- so parsed on its own, the element
// and every field in it are in no namespace at all. libvirt's own in-memory
// tree still has the element bound to the uri (virDomainGetMetadata found it
// by uri, and `virsh metadata --uri ... --remove` removes it), so the binding
// is real; it is the serialisation that arrives incomplete.
//
// An unresolved `vmsync:` prefix is accepted for the same family of reasons:
// ParseMetadata is handed the raw inner XML of <metadata>, torn out of the
// domain document by encoding/xml's ,innerxml, so a declaration sitting on
// <domain> is simply not in the text being parsed. An unresolved prefix is
// also the only way Space can be the literal string "vmsync" -- a prefix
// bound to somebody else's namespace resolves to that namespace, not to
// itself -- so accepting it cannot capture another tool's element.
//
// What this still refuses is a bare <vmsync> that neither resolves to nor
// declares the uri anywhere on itself. That element belongs to somebody else
// until it proves otherwise, and mergeMetadataFields turns the refusal into a
// loud error rather than a silent overwrite.
func isMetadataContainer(el xml.StartElement) bool {
	if el.Name.Local != metadataPrefix {
		return false
	}
	if el.Name.Space == metadataNamespace || el.Name.Space == metadataPrefix {
		return true
	}
	for _, attr := range el.Attr {
		if isMetadataNamespaceDeclaration(attr.Name.Space, attr.Name.Local, attr.Value) {
			return true
		}
	}
	return false
}

// isMetadataNamespaceDeclaration reports whether one attribute declares
// vmsync's namespace, as the default (xmlns="...") or under any prefix
// (xmlns:anything="...").
//
// Shared in spirit with domxml.go's etree-side check, and deliberately
// indifferent to WHICH prefix carries it: what matters is that the element
// names vmsync's uri, not how it spells the binding. Both parsers report an
// `xmlns:foo` attribute with the space "xmlns" and the local name "foo", and
// a bare `xmlns` with no space at all.
func isMetadataNamespaceDeclaration(space, local, value string) bool {
	if value != metadataNamespace {
		return false
	}
	return space == "xmlns" || (space == "" && local == "xmlns")
}

// stripDomainUUID returns domainXML with its <uuid> element removed, so a
// subsequent DomainDefineXML lets libvirt assign a fresh, random one instead
// of colliding with the one already in use elsewhere on the target (see
// this function's only call site, DefineDomain's UUID-collision fallback).
// Returns the real Unmarshal/Marshal error on failure rather than silently
// returning an empty string: a caller that fed that empty string straight
// into DomainDefineXML would see a generic, misleading "empty/malformed
// XML" failure from libvirt with no indication the actual problem was here,
// not there.
// ListManagedCheckpoints lists dom's vmsync-managed checkpoints. Callers
// (NextCheckpointName's chain-parent selection, DeleteAllManagedCheckpoints'
// -reinit cleanup) act on the assumption that this list is complete -- a
// silently incomplete one is worse than an error: NextCheckpointName could
// pick a stale parent or a name that collides with a checkpoint it never
// saw, and -reinit could leave a real vmsync checkpoint behind while
// believing it wiped the chain clean. So any lookup failure on an entry
// this can't positively rule out as one of vmsync's own aborts the whole
// listing instead of just skipping that entry -- a checkpoint whose name
// can't even be read might still be one of ours, and one whose name
// matches the vmsync prefix but whose XML can't be read definitely is.
// Only a successfully-read name that doesn't match the prefix is a
// legitimate, silent skip (some other tool's checkpoint on the same
// domain).
func ListManagedCheckpoints(dom *libvirt.Domain) ([]Checkpoint, error) {
	cpts, err := dom.ListAllCheckpoints(0)
	if err != nil {
		return nil, fmt.Errorf("list checkpoints: %w", err)
	}

	var out []Checkpoint
	var lookupErr error
	for _, c := range cpts {
		// ListAllCheckpoints returns every checkpoint on the domain, not
		// just vmsync's own -- the prefix check below is what filters those
		// out. Each entry's handle must be freed regardless of which path
		// is taken, so this runs the per-checkpoint logic in its own
		// closure with a single defer covering all of them, rather than
		// needing a Free() call before every continue. The range loop
		// itself always runs to completion, even once a failure has been
		// recorded, so every remaining handle still gets freed -- only the
		// final return value is affected.
		cp, ok, err := func() (Checkpoint, bool, error) {
			defer c.Free()
			name, err := c.GetName()
			if err != nil {
				return Checkpoint{}, false, fmt.Errorf("get checkpoint name: %w", err)
			}
			if !IsManagedCheckpointName(name) {
				return Checkpoint{}, false, nil
			}

			xmlDesc, err := c.GetXMLDesc(0)
			if err != nil {
				return Checkpoint{}, false, fmt.Errorf("get xml for checkpoint %s: %w", name, err)
			}

			return parseCheckpointXML(name, xmlDesc), true, nil
		}()
		if err != nil && lookupErr == nil {
			lookupErr = err
		}
		if ok {
			out = append(out, cp)
		}
	}
	if lookupErr != nil {
		return nil, fmt.Errorf("list checkpoints: incomplete listing, at least one entry could not be read: %w", lookupErr)
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].Time.Before(out[j].Time)
	})
	return out, nil
}

func parseCheckpointXML(name, desc string) Checkpoint {
	type cpXML struct {
		Creation string `xml:"creationTime"`
		Parent   struct {
			Name string `xml:"name"`
		} `xml:"parent"`
	}
	var cp cpXML
	_ = xml.Unmarshal([]byte(desc), &cp)

	var t time.Time
	if cp.Creation != "" {
		if sec, err := time.ParseDuration(cp.Creation + "s"); err == nil {
			t = time.Unix(int64(sec.Seconds()), 0)
		}
	}

	return Checkpoint{Name: name, Parent: cp.Parent.Name, Time: t}
}

func CreateCheckpoint(dom *libvirt.Domain, checkpointName, parent string, diskTargets []disk.QcowDisk) error {
	xmlBody := buildCheckpointXML(checkpointName, parent, diskTargets)
	cp, err := dom.CreateCheckpointXML(xmlBody, 0)
	if err != nil {
		return fmt.Errorf("create checkpoint %s: %w", checkpointName, err)
	}
	return cp.Free()
}

// CreateVerifyWindowCheckpoint is GONE, deliberately, and this note stands
// in its place so it does not come back.
//
// It created a fresh, parentless (therefore empty) checkpoint at the moment
// the former -verify=online's compare window opened, and the compare then tried to
// excuse mismatches using that checkpoint's bitmap. The bitmap described
// only the instant between its own creation and BackupBegin, while every
// mismatch the compare actually saw came from guest writes during the COPY,
// minutes or hours earlier. So it exonerated nothing and healthy replicas
// were reported corrupt -- observed in production as "mismatches=260,
// selected=0, 260 real".
//
// -verify now compares against the primary backup export the copy read from,
// whose bitmap covers exactly the interval that produces the differences.
// Nothing creates VerifyWindowCheckpointName any more; only the deletion
// below survives, to clean up after older builds.

// DeleteVerifyWindowCheckpoint removes the ephemeral verify-window
// checkpoint if it exists, tolerating the case where it doesn't -- which is
// now the normal case: nothing creates it. Retained purely to self-heal a
// leftover from a build that did, either a crashed run of one or the first
// run after an upgrade. Called defensively regardless of whether -verify is
// requested this run.
func DeleteVerifyWindowCheckpoint(dom *libvirt.Domain) error {
	return DeleteCheckpointIfExists(dom, VerifyWindowCheckpointName)
}

func buildCheckpointXML(name, parent string, diskTargets []disk.QcowDisk) string {
	var b strings.Builder
	b.WriteString("<domaincheckpoint>\n")
	b.WriteString("  <name>" + name + "</name>\n")
	if parent != "" {
		b.WriteString("  <parent><name>" + parent + "</name></parent>\n")
	}
	b.WriteString("  <description>vmsync checkpoint</description>\n")
	b.WriteString("  <disks>\n")
	for _, dev := range diskTargets {
		b.WriteString("    <disk name=\"" + dev.TargetDev + "\" checkpoint=\"bitmap\" bitmap=\"" + name + "\"/>\n")
	}
	b.WriteString("  </disks>\n")
	b.WriteString("</domaincheckpoint>")
	return b.String()
}

func NextCheckpointName(existing []Checkpoint) (name string, parent string, err error) {
	if len(existing) == 0 {
		return fmt.Sprintf("%s-%06d", CheckpointPrefix, 1), "", nil
	}
	latest := existing[len(existing)-1]

	re := regexp.MustCompile(`^(.*-)(\d+)$`)
	m := re.FindStringSubmatch(latest.Name)
	if m == nil {
		return "", "", fmt.Errorf("checkpoint name %q does not end in a numeric suffix, cannot determine next checkpoint name", latest.Name)
	}
	numStr := m[2]
	n, _ := strconv.Atoi(numStr)
	n = n + 1
	return fmt.Sprintf("%s-%0*d", CheckpointPrefix, len(numStr), n), latest.Name, nil
}

func FailIfBlockJobActive(dom *libvirt.Domain, qcowDisks []disk.QcowDisk) error {
	for _, disk := range qcowDisks {
		info, err := dom.GetBlockJobInfo(disk.TargetDev, 0)
		if err != nil {
			return fmt.Errorf("check block job on disk %s: %w", disk.TargetDev, err)
		}

		// With no active job, libvirt typically returns unknown type and zero progress.
		if info.Type != libvirt.DOMAIN_BLOCK_JOB_TYPE_UNKNOWN || info.Cur > 0 || info.End > 0 {
			return fmt.Errorf("active block job detected on disk %s (type=%d cur=%d end=%d)", disk.TargetDev, info.Type, info.Cur, info.End)
		}
	}
	return nil
}

// AbortActiveBlockJobs cancels any running block job (typically a backup job
// left over from a previous, interrupted sync) on each of qcowDisks. Used by
// -reinit to clear the state that would otherwise make FailIfBlockJobActive
// permanently refuse to proceed -- a stuck job is exactly the kind of broken
// state -reinit is meant to recover from (see the "Bitmap already exists"
// failure in https://github.com/abbbi/vmsync/issues/9).
func AbortActiveBlockJobs(dom *libvirt.Domain, qcowDisks []disk.QcowDisk) error {
	for _, d := range qcowDisks {
		info, err := dom.GetBlockJobInfo(d.TargetDev, 0)
		if err != nil {
			return fmt.Errorf("check block job on disk %s: %w", d.TargetDev, err)
		}
		// Same "no active job" signature FailIfBlockJobActive checks for.
		if info.Type == libvirt.DOMAIN_BLOCK_JOB_TYPE_UNKNOWN && info.Cur == 0 && info.End == 0 {
			continue
		}
		trace.Warning("reinit: aborting active block job", "disk", d.TargetDev, "type", info.Type)
		if err := dom.BlockJobAbort(d.TargetDev, 0); err != nil {
			return fmt.Errorf("abort block job on disk %s: %w", d.TargetDev, err)
		}
	}
	return nil
}

func DeleteCheckpointIfExists(dom *libvirt.Domain, checkpointName string) error {
	cp, err := dom.CheckpointLookupByName(checkpointName, 0)
	if err != nil {
		if lvErr, ok := err.(libvirt.Error); ok && lvErr.Code == libvirt.ERR_NO_DOMAIN_CHECKPOINT {
			return nil
		}
		return fmt.Errorf("lookup checkpoint %s for deletion: %w", checkpointName, err)
	}
	defer cp.Free()

	// NEVER metadata-only. It was tried here and it corrupts the pair.
	//
	// Deleting a checkpoint means merging its dirty bitmap into the next one,
	// which qemu only does live -- so on an inactive domain libvirt refuses
	// with VIR_ERR_OPERATION_UNSUPPORTED, "cannot delete checkpoint for
	// inactive domain". VIR_DOMAIN_CHECKPOINT_DELETE_METADATA_ONLY gets past
	// that refusal by dropping libvirt's RECORD of the checkpoint and leaving
	// the bitmap in the qcow2. The domain then has no checkpoints as far as
	// libvirt is concerned, `virsh checkpoint-list` shows none, and the next
	// sync starts its chain again at vmsync-cpt-000001 -- at which point qemu
	// refuses: "Bitmap already exists: vmsync-cpt-000001". Every subsequent
	// sync for that pair fails the same way, and nothing in libvirt's view
	// explains why. Recovery is qemu-img bitmap --remove per disk, by hand.
	//
	// The rule is: checkpoint metadata may only be dropped without its bitmap
	// when the IMAGE ITSELF is about to be deleted or replaced, which is what
	// makes DefineDomain's DOMAIN_UNDEFINE_CHECKPOINTS_METADATA safe --
	// nothing there survives to carry the orphan. This function has seven
	// callers, most of them ordinary sync paths against images that stay, so
	// it cannot make that assumption for any of them.
	//
	// The refusal on an inactive domain is therefore left to propagate. The
	// one caller that legitimately needs to get past it is -invert, and it
	// uses DeleteAllManagedCheckpointsMetadataOnly below -- which is a
	// different function precisely so nobody reaches this behaviour without
	// having read what it costs.
	if err := cp.Delete(0); err != nil {
		return fmt.Errorf("delete checkpoint %s: %w", checkpointName, err)
	}
	return nil
}

// DeleteAllManagedCheckpointsMetadataOnly drops libvirt's record of every
// vmsync checkpoint WITHOUT touching the bitmaps in the images.
//
// On its own this corrupts the pair. It leaves each disk carrying a persistent
// bitmap named after a checkpoint libvirt no longer knows about, so the next
// sync starts its chain again at vmsync-cpt-000001 and qemu refuses -- "Bitmap
// already exists" -- for that pair, permanently, with `virsh checkpoint-list`
// showing nothing that would explain it.
//
// It is exported only because an offline domain gives no alternative: deleting
// a checkpoint properly merges its bitmap into the next one, and only a running
// qemu can do that. The offline equivalent is this plus disk.RemoveBitmap for
// every bitmap on every disk, and the CALLER MUST DO BOTH.
//
// Bitmaps first, then this. That order matters: bitmaps gone with metadata
// still present is a visible, recoverable state -- checkpoint-list shows the
// checkpoints and a later delete cleans up -- whereas metadata gone with
// bitmaps present is the invisible one that has to be found with qemu-img and
// unpicked by hand.
func DeleteAllManagedCheckpointsMetadataOnly(dom *libvirt.Domain) error {
	existing, err := ListManagedCheckpoints(dom)
	if err != nil {
		return err
	}
	for _, name := range checkpointDeletionOrder(existing) {
		cp, err := dom.CheckpointLookupByName(name, 0)
		if err != nil {
			if lvErr, ok := err.(libvirt.Error); ok && lvErr.Code == libvirt.ERR_NO_DOMAIN_CHECKPOINT {
				continue
			}
			return fmt.Errorf("lookup checkpoint %s for deletion: %w", name, err)
		}
		err = cp.Delete(libvirt.DOMAIN_CHECKPOINT_DELETE_METADATA_ONLY)
		cp.Free()
		if err != nil {
			return fmt.Errorf("delete checkpoint metadata %s: %w", name, err)
		}
	}
	return nil
}

// DeleteAllManagedCheckpoints removes every vmsync-managed checkpoint on dom,
// used by -reinit to recover from a broken checkpoint chain (e.g. the
// "Bitmap already exists" failure in
// https://github.com/abbbi/vmsync/issues/9) by discarding it entirely and
// letting the next sync start over as a fresh full sync.
// PruneCheckpointsOlderThan deletes every vmsync-managed checkpoint created
// before baseline, and returns the names it removed.
//
// vmsync keeps exactly one checkpoint on a source: the baseline the next
// incremental will diff against. Anything older is a leftover from a run that
// created its successor and then failed before tidying up -- which is now the
// ordinary failure shape, because the parent is deleted only AFTER the target
// has accepted the new checkpoint. Trading "lose the baseline" for "leak a
// checkpoint" is the right way round, but only if something eventually
// collects the leaks. This is that something.
//
// Deletes oldest-first, which is the only safe direction. libvirt merges a
// deleted checkpoint's bitmap into its child, so removing the bottom of the
// chain is exactly the per-run parent cleanup vmsync has always done, just
// repeated. Removing from the middle is a different operation that later
// checkpoints depend on, and this never does it.
//
// Refuses to delete anything when baseline is not among the domain's own
// checkpoints. That means the caller's idea of the chain and the source's
// disagree, and the one thing worse than a leaked checkpoint is deleting the
// baseline a replica is diffing against.
// checkpointsOlderThan picks the entries preceding baseline in an
// oldest-first list.
//
// Separate from the deleting, and pure, because this is the decision that
// chooses which checkpoints get destroyed -- it should be readable and
// exhaustively testable without a hypervisor. Every guard here is the
// difference between collecting a leak and deleting the baseline a replica is
// diffing against.
func checkpointsOlderThan(existing []Checkpoint, baseline string) ([]Checkpoint, error) {
	idx := -1
	for i, c := range existing {
		if c.Name == baseline {
			idx = i
			break
		}
	}
	if idx < 0 {
		// The caller's idea of the chain and the source's disagree. Deleting
		// on that basis is how a baseline gets destroyed, so nothing is.
		return nil, fmt.Errorf("prune checkpoints: the baseline %s is not among this domain's %d vmsync checkpoints, so the chain is not what this run believes it is -- refusing to delete anything",
			baseline, len(existing))
	}
	return existing[:idx], nil
}

func PruneCheckpointsOlderThan(dom *libvirt.Domain, baseline string) ([]string, error) {
	if baseline == "" {
		return nil, fmt.Errorf("prune checkpoints: no baseline checkpoint given")
	}
	existing, err := ListManagedCheckpoints(dom)
	if err != nil {
		return nil, fmt.Errorf("prune checkpoints: %w", err)
	}
	stale, err := checkpointsOlderThan(existing, baseline)
	if err != nil {
		return nil, err
	}

	var deleted []string
	for _, c := range stale {
		if err := DeleteCheckpointIfExists(dom, c.Name); err != nil {
			// Stop at the first failure rather than pressing on: the ones
			// after it are its children, and deleting a child while its
			// parent survives is the mid-chain removal this avoids.
			return deleted, fmt.Errorf("prune checkpoints: deleting %s: %w", c.Name, err)
		}
		deleted = append(deleted, c.Name)
	}
	return deleted, nil
}

func DeleteAllManagedCheckpoints(dom *libvirt.Domain) error {
	existing, err := ListManagedCheckpoints(dom)
	if err != nil {
		return err
	}
	for _, name := range checkpointDeletionOrder(existing) {
		// Returned unwrapped: DeleteCheckpointIfExists already names the
		// checkpoint, and wrapping again produced "delete checkpoint X:
		// delete checkpoint X: <cause>" in the log -- twice the length for
		// none of the information.
		if err := DeleteCheckpointIfExists(dom, name); err != nil {
			return err
		}
	}
	return nil
}

// checkpointDeletionOrder returns existing's checkpoint names in the order
// DeleteAllManagedCheckpoints deletes them: newest-first (the reverse of
// ListManagedCheckpoints' own oldest-first result).
//
// This is NOT because a checkpoint with children refuses to delete --
// unlike a disk *snapshot* (where a child depends on its parent as a
// backing file, so the parent can't go away until that data is committed
// forward into the child), virDomainCheckpointDelete's own documented
// behavior is the opposite direction and has no such restriction at all:
// deleting a checkpoint merges the dirty-tracking region it owns into its
// OWN PARENT (not a child), and succeeds unconditionally regardless of
// whether the checkpoint being deleted has children -- an earlier version
// of this comment had the mechanism and the restriction both backwards,
// describing snapshot semantics instead of checkpoint semantics.
//
// Newest-first is kept anyway, not because it's required by the documented
// behavior above, but because it costs nothing here: DeleteAllManagedCheckpoints
// discards the entire chain regardless of order (this is -reinit's full
// recovery path, not selective pruning), so there's no reason to depend on
// every targeted libvirt/QEMU version continuing to allow arbitrary-order
// bulk deletion identically, when deleting leaf-to-root is unconditionally
// safe on every version instead. Kept as a standalone, pure function
// (taking and returning plain data, not a live domain handle) so this
// ordering choice stays directly testable regardless of why it's made.
func checkpointDeletionOrder(existing []Checkpoint) []string {
	order := make([]string, len(existing))
	for i, c := range existing {
		order[len(existing)-1-i] = c.Name
	}
	return order
}

func StartPullBackupTCP(
	dom *libvirt.Domain,
	incrementalCheckpoint,
	exportBitmap,
	bindAddr string,
	port int,
	diskTargets []disk.QcowDisk,
) error {
	backupXML := buildPullBackupXML(incrementalCheckpoint, exportBitmap, bindAddr, port, diskTargets)
	if err := dom.BackupBegin(backupXML, "", 0); err != nil {
		return fmt.Errorf("start pull backup (tcp %s:%d): %w (xml=%s)", bindAddr, port, err, backupXML)
	}
	return nil
}

// ExternalSnapshotCount returns how many external snapshots (as opposed to
// internal, in-file ones) currently exist on dom -- the condition
// IsCheckpointBlockedBySnapshot exists to react to after the fact. This is a
// direct query (VIR_DOMAIN_SNAPSHOT_LIST_EXTERNAL), not an inference from a
// failed call, so it's accurate on its own even on runs that never attempt
// CreateCheckpoint at all -- used for the vmsync_external_snapshot_count
// metric.
func ExternalSnapshotCount(dom *libvirt.Domain) (int, error) {
	n, err := dom.SnapshotNum(libvirt.DOMAIN_SNAPSHOT_LIST_EXTERNAL)
	if err != nil {
		return 0, fmt.Errorf("count external snapshots: %w", err)
	}
	return n, nil
}

// IsCheckpointBlockedBySnapshot reports whether err is libvirt's specific,
// documented rejection of checkpoint creation while an external snapshot
// exists on the domain: "the creation of checkpoints when external
// snapshots exist is currently forbidden" (see
// https://libvirt.org/formatcheckpoint.html). Matched on the error text
// rather than a dedicated error code, since libvirt reports this as a
// generic invalid-operation error with no code of its own specific to this
// case -- unlike StopBackup just below, which has a structured alternative
// available and uses that instead.
//
// Unwraps err fully before checking it: err is CreateCheckpoint's own
// returned error, which always wraps the real libvirt failure behind a
// "create checkpoint %s: %w" prefix (see CreateCheckpoint) -- so err.Error()
// itself always contains "checkpoint" regardless of what actually failed,
// which would silently defeat the whole point of requiring both terms
// below. Checking the innermost, fully-unwrapped error instead means
// "checkpoint" only matches when libvirt's own message is genuinely about
// checkpoints, restoring the real disambiguation this function is supposed
// to provide at its only real call site.
//
// Requires both "checkpoint" and "external snapshot" to appear, rather than
// just the single generic word "snapshot" (this function's own previous
// implementation) -- libvirt/qemu use that word pervasively for entirely
// unrelated conditions, any of which would otherwise get misclassified as
// this specific, tolerated case. The caller only tolerates a true result by
// proceeding with an incremental sync against a checkpoint whose validity
// was never actually re-established this run (see its own comment) -- a
// false positive here is the dangerous direction, so requiring both terms
// together, closely matching libvirt's actual documented wording, is worth
// the (much smaller) risk of a false negative on some future, differently
// worded version of this same message; that failure mode is safe by
// comparison; it just falls back to failing the run outright, same as any
// other unrecognized CreateCheckpoint error.
func IsCheckpointBlockedBySnapshot(err error) bool {
	if err == nil {
		return false
	}
	for {
		unwrapped := errors.Unwrap(err)
		if unwrapped == nil {
			break
		}
		err = unwrapped
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "checkpoint") && strings.Contains(msg, "external snapshot")
}

// domainJobOperationNames maps the subset of libvirt's job-operation
// constants StopBackup cares about to short, human-readable names for its
// refusal error/log -- libvirt-go's DomainJobOperationType has no String()
// of its own.
var domainJobOperationNames = map[libvirt.DomainJobOperationType]string{
	libvirt.DOMAIN_JOB_OPERATION_UNKNOWN:         "unknown",
	libvirt.DOMAIN_JOB_OPERATION_START:           "start",
	libvirt.DOMAIN_JOB_OPERATION_SAVE:            "save",
	libvirt.DOMAIN_JOB_OPERATION_RESTORE:         "restore",
	libvirt.DOMAIN_JOB_OPERATION_MIGRATION_IN:    "migration (incoming)",
	libvirt.DOMAIN_JOB_OPERATION_MIGRATION_OUT:   "migration (outgoing)",
	libvirt.DOMAIN_JOB_OPERATION_SNAPSHOT:        "snapshot",
	libvirt.DOMAIN_JOB_OPERATION_SNAPSHOT_REVERT: "snapshot revert",
	libvirt.DOMAIN_JOB_OPERATION_DUMP:            "dump",
	libvirt.DOMAIN_JOB_OPERATION_BACKUP:          "backup",
	libvirt.DOMAIN_JOB_OPERATION_SNAPSHOT_DELETE: "snapshot delete",
}

// domainJobOperationName returns op's human-readable name, or a numeric
// fallback for anything not in domainJobOperationNames -- forward-compatible
// with a libvirt-go release that adds new operation constants this list
// hasn't been updated for yet, rather than panicking or printing nothing.
func domainJobOperationName(op libvirt.DomainJobOperationType) string {
	if name, ok := domainJobOperationNames[op]; ok {
		return name
	}
	return fmt.Sprintf("operation type %d", int(op))
}

// StopBackup aborts any pull-backup job in progress on dom, tolerating the
// case where none is running (already stopped, or never started -- e.g. the
// retry-via-reconnect path after a primary stop that actually succeeded
// server-side but timed out client-side). Checks GetJobStats first rather
// than relying solely on pattern-matching AbortJob's own error text: whether
// there's currently no job at all is exposed as a stable, structured enum
// (DomainJobType, verified directly against libvirt-go's own source), not a
// message string whose exact wording can vary across libvirt versions --
// which is exactly why the previous version of this function's text match
// ("no current job") never actually matched libvirt's real message and this
// short-circuit could never fire.
//
// libvirt allows only one asynchronous job (migration, save, dump, backup,
// ...) on a domain at a time, and AbortJob aborts whatever that current job
// happens to be -- it has no notion of "vmsync's own" job. Calling it
// without checking which job is actually running would silently abort
// someone else's operation the moment one is active on the same domain when
// this runs: an operator-initiated live migration, save, or dump started
// after vmsync's own backup job already finished (or on a reconnect retry
// after the primary connection went stale) would just stop, with AbortJob
// itself reporting success and nothing here noticing anything went wrong.
// GetJobStats reports which operation the current job actually is
// (OperationSet/Operation, populated from libvirt's VIR_DOMAIN_JOB_OPERATION
// typed parameter on a new enough libvirt/QEMU driver pair) -- when that's
// known and isn't DOMAIN_JOB_OPERATION_BACKUP, this refuses to touch it at
// all rather than risk aborting it. An older libvirt that doesn't report
// VIR_DOMAIN_JOB_OPERATION leaves OperationSet false, in which case this
// falls back to the previous, coarser behavior (abort whatever's running) --
// having no operation information at all is not itself evidence that
// vmsync doesn't own the job, just that this libvirt can't say either way.
func StopBackup(dom *libvirt.Domain) error {
	info, err := dom.GetJobStats(0)
	if err != nil {
		// GetJobStats can fail against a driver/connection that doesn't
		// support it; fall back to the plain job-existence check rather
		// than treating a stats-query failure as license to abort blindly.
		info, err = dom.GetJobInfo()
	}
	if err == nil {
		if info.Type == libvirt.DOMAIN_JOB_NONE {
			return nil
		}
		if info.OperationSet && info.Operation != libvirt.DOMAIN_JOB_OPERATION_BACKUP {
			opName := domainJobOperationName(info.Operation)
			trace.Warning("refusing to abort the domain's current job -- it is not vmsync's own backup job", "operation", opName)
			return fmt.Errorf("refusing to abort domain job: current job is %s, not the backup job vmsync started -- aborting it would interrupt an unrelated operation instead", opName)
		}
	}
	if err := dom.AbortJob(); err != nil {
		// Fallback for the rare case GetJobStats/GetJobInfo above didn't
		// catch it (e.g. it errored itself) -- kept, but not relied upon. If
		// this still doesn't match in practice, the Debug log captures the
		// real text so it can be fixed with actual data instead of another
		// guess.
		if strings.Contains(strings.ToLower(err.Error()), "no current job") {
			return nil
		}
		trace.Debug("abort backup job failed", "error", err)
		return fmt.Errorf("abort backup job: %w", err)
	}
	return nil
}

func buildPullBackupXML(
	incrementalCheckpoint,
	exportBitmap,
	bindAddr string,
	port int,
	diskTargets []disk.QcowDisk,
) string {
	if bindAddr == "" {
		bindAddr = "0.0.0.0"
	}
	if port <= 0 {
		port = 10809
	}

	var b strings.Builder
	b.WriteString("<domainbackup mode=\"pull\">\n")
	if incrementalCheckpoint != "" {
		b.WriteString("  <incremental>" + incrementalCheckpoint + "</incremental>\n")
	}
	b.WriteString(fmt.Sprintf("  <server transport=\"tcp\" name=\"%s\" port=\"%d\"/>\n", bindAddr, port))
	b.WriteString("  <disks>\n")
	for _, dev := range diskTargets {
		b.WriteString("    <disk name=\"" + dev.TargetDev + "\" exportname=\"" + dev.TargetDev + "\"")
		if exportBitmap != "" {
			b.WriteString(" exportbitmap=\"" + exportBitmap + "\"")
		}
		b.WriteString("/>\n")
	}
	b.WriteString("  </disks>\n")
	b.WriteString("</domainbackup>")
	return b.String()
}

// DomainActive reports whether dom is active in libvirt's own sense --
// anything other than shut off, which includes paused/suspended domains,
// not just ones actively executing. This is the right check both for
// deciding whether a domain needs starting (Create()/CreateWithFlags() only
// work on a shut-off domain -- calling them on an already-paused one fails
// with "domain is already running", exactly the class of error this
// replaces a check that used to miss) and for the safety checks that refuse
// to touch a domain's disk files while it's active: a paused domain still
// holds those files open exactly like a running one does, so treating it as
// safe to delete/overwrite under -- as a naive "state == DOMAIN_RUNNING"
// check would -- is a real risk, not just an inconvenience.
func DomainActive(dom *libvirt.Domain) (bool, error) {
	state, _, err := dom.GetState()
	if err != nil {
		return false, fmt.Errorf("unable to get domain state: %w", err)
	}
	return state != libvirt.DOMAIN_SHUTOFF, nil
}
