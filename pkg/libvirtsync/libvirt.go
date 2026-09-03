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
	"regexp"
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
)

// TestFaults is every accepted -test value, for validation and for the flag's
// own help text.
var TestFaults = []string{TestFaultFailureDefine, TestFaultCorruptBeforeChecksum, TestFaultCorruptAfterCommit}

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
)

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
func UpdateSyncMetadata(domainXML, checkpoint, sourceHost, sourceDomain, targetRole string, checkpointAtUnix int64, sourceStopped bool, replicaWrittenAt string) (string, error) {
	updates := map[string]string{
		MetadataFieldLastCheckpoint: checkpoint,
		MetadataFieldLastSync:       strconv.FormatInt(time.Now().Unix(), 10),
		MetadataFieldFailureCount:   "0",
		MetadataFieldReplicaSource:  ReplicaEntry(sourceHost, sourceDomain),
		MetadataFieldCheckpointAt:   strconv.FormatInt(checkpointAtUnix, 10),
	}
	remove := []string{
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
		MetadataFieldReplicaTargets,
		MetadataFieldPromotedAt,
		MetadataFieldPromotedBy,
		MetadataFieldPromotedFrom,
		MetadataFieldPromotionMode,
		MetadataFieldFenceID,
		MetadataFieldFenceSource,
		MetadataFieldFenceArmedAt,
		MetadataFieldFenceArmedBy,
	}
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
	}, MetadataFieldLastCheckpoint, MetadataFieldLastSync, MetadataFieldFailureCount,
		MetadataFieldReplicaWrittenAt, MetadataFieldPendingCheckpoint,
		MetadataFieldVerifyState, MetadataFieldVerifyFailedAt)
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
	case "", RoleTarget, RolePaused, RoleFenced:
		return nil
	case RoleSource:
		return fmt.Errorf("target domain is marked replication_role=%q, meaning it is the SOURCE of a replication pair -- restoring a restore point over it would overwrite the original with an old copy of its own replica; check that -target-uri/-target-domain name the replica and not the source", RoleSource)
	case RolePromoted:
		return fmt.Errorf("target domain is marked replication_role=%q, meaning it was failed over to and is now serving live -- refusing to overwrite live data with a restore point, whether or not it happens to be running at this moment; run -update-role=%s first if this domain is genuinely a replica again (its current disk contents will then be discarded)", RolePromoted, RoleTarget)
	default:
		return fmt.Errorf("target domain has an unrecognized replication_role=%q -- refusing to restore over a domain whose role this vmsync build does not understand (it was most likely written by a newer version; upgrade rather than overriding)", role)
	}
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
func SetReplicationRole(mgr *Manager, domainName, role string) (previous string, err error) {
	if err := ValidateRole(role); err != nil {
		return "", err
	}

	previous, err = ReadDomainMetadataField(mgr, domainName, MetadataFieldReplicationRole)
	if err != nil {
		return "", err
	}

	// Moving away from `promoted` takes the promotion record with it.
	//
	// Those fields describe a failover that is, by this very call, no longer
	// in force. Leaving them behind lets a domain carry
	// replication_role=target alongside a promoted_at and a promoted_from --
	// a combination no promotion ever wrote, which anything reasoning about
	// "was this displaced, and by whom" would read as fact. The one
	// documented remedy for an unwanted promotion is -update-role=target
	// (see TargetRoleAllowsSync's own message), so this is the common path,
	// not an edge case.
	//
	// Only UpdateSyncMetadata and an inversion stripped them before, and the
	// first of those runs only after a SUCCESSFUL sync -- which a domain
	// stuck mid-recovery is precisely not getting.
	promotionFields := []string{
		MetadataFieldPromotedAt,
		MetadataFieldPromotedBy,
		MetadataFieldPromotedFrom,
		MetadataFieldPromotionMode,
		// The fence that promotion armed dies with it: a domain that is
		// no longer promoted is not displacing anybody.
		MetadataFieldFenceID,
		MetadataFieldFenceSource,
		MetadataFieldFenceArmedAt,
		MetadataFieldFenceArmedBy,
	}
	switch {
	case role == RoleNone:
		err = SetDomainMetadataFields(mgr, domainName, nil,
			append([]string{MetadataFieldReplicationRole}, promotionFields...)...)
	case role == RolePromoted:
		// Promotion itself is written by -promote, which records the whole
		// record atomically. Setting the role to promoted by hand must not
		// invent one, but must not destroy an existing one either.
		err = SetDomainMetadataFields(mgr, domainName, map[string]string{
			MetadataFieldReplicationRole: role,
		})
	default:
		err = SetDomainMetadataFields(mgr, domainName, map[string]string{
			MetadataFieldReplicationRole: role,
		}, promotionFields...)
	}
	if err != nil {
		return "", err
	}
	return previous, nil
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
