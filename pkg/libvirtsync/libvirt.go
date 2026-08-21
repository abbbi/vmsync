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
	"vmsync/pkg/util"

	"libvirt.org/go/libvirt"
	"libvirt.org/go/libvirtxml"
)

const CheckpointPrefix = "vmsync-cpt"

// VerifyWindowCheckpointName names the ephemeral, throwaway checkpoint
// -verify=online creates right when its compare window opens, to find out
// afterward which regions the guest wrote to during the compare (see
// CreateVerifyWindowCheckpoint). Deliberately NOT prefixed with
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
	metadataStart     = `<vmsync:vmsync xmlns:vmsync="` + metadataNamespace + `">`
	metadataEnd       = `</vmsync:vmsync>`

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
	// RoleNone is not a stored value: it is the argument that CLEARS the
	// field, returning a domain to the no-role-recorded state.
	RoleNone = "none"
)

// ValidRoles lists the values SetReplicationRole accepts, in the order a
// CLI help message should present them.
var ValidRoles = []string{RoleSource, RoleTarget, RolePromoted, RolePaused, RoleNone}

// metadataFieldOrder fixes the field order vmsync writes its own metadata
// entries in, purely for stable/readable XML output.
var metadataFieldOrder = []string{
	MetadataFieldReplicationRole,
	MetadataFieldLastCheckpoint,
	MetadataFieldLastSync,
	MetadataFieldFailureCount,
	MetadataFieldReplicaSource,
	MetadataFieldReplicaTargets,
	MetadataFieldPromotedAt,
	MetadataFieldPromotedBy,
	MetadataFieldPromotedFrom,
	MetadataFieldPromotionMode,
	MetadataFieldCheckpointAt,
	MetadataFieldSourceStoppedAtSync,
	MetadataFieldLastReplicatedAt,
	MetadataFieldLastReplicatedTo,
}

var vmsyncBlockRe = regexp.MustCompile(`(?s)<vmsync:vmsync[^>]*>.*?</vmsync:vmsync>`)

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

func ThawFs(srcDom *libvirt.Domain, freezed bool) {
	if !freezed {
		return
	}
	if err := srcDom.FSThaw(nil, 0); err != nil {
		trace.Warning("Filesystem thaw failed", "error", err)
	} else {
		trace.Info("Successfully thawed file systems using guest agent")
	}
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
func replaceDomainDiskPath(domainXML, targetDiskPath string, rootSourceByLiveSource map[string]string) (string, error) {
	domcfg := &libvirtxml.Domain{}
	err := domcfg.Unmarshal(domainXML)
	if err != nil {
		return "", err
	}

	for i, d := range domcfg.Devices.Disks {
		if util.IgnoreDevice(d) == true {
			continue
		}
		// IgnoreDevice only guarantees a non-nil Driver; Source/Source.File
		// are separate pointers and could still be nil for a malformed disk.
		if d.Source == nil || d.Source.File == nil {
			continue
		}

		liveSource := d.Source.File.File
		rootSource, ok := rootSourceByLiveSource[liveSource]
		if !ok {
			// ParseQcowDisks and this domain's own live XML disagree on which
			// disks exist -- exactly the "shouldn't happen" case above, but
			// having actually happened. liveSource may be an external-snapshot
			// overlay that was never copied to the target under that name (see
			// this function's own doc comment), so writing it into the
			// target's persistent definition here would silently point the
			// domain at a nonexistent or wrong disk file. Fail loud instead.
			return "", fmt.Errorf("disk %s: no resolved root source available, refusing to write its live path into the target's persistent definition", liveSource)
		}
		domcfg.Devices.Disks[i].Source.File.File = util.SetTargetPath(targetDiskPath, rootSource)
		domcfg.Devices.Disks[i].BackingStore = nil
	}

	changed, err := domcfg.Marshal()
	if err != nil {
		return "", err
	}

	return changed, nil
}

func replace(domainXML, name string) (string, error) {
	domcfg := &libvirtxml.Domain{}
	err := domcfg.Unmarshal(domainXML)
	if err != nil {
		return "", err
	}

	domcfg.Name = name

	changed, err := domcfg.Marshal()
	if err != nil {
		return "", err
	}

	return changed, nil
}

func replaceDomainName(domainXML, name string) (string, error) {
	domcfg := &libvirtxml.Domain{}
	err := domcfg.Unmarshal(domainXML)
	if err != nil {
		return "", err
	}

	domcfg.Name = name

	changed, err := domcfg.Marshal()
	if err != nil {
		return "", err
	}

	return changed, nil
}

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
// This exists because replaceDomainName, replaceDomainDiskPath, and
// SetMetadataFields all go through a full libvirtxml.Domain
// unmarshal-then-marshal round-trip rather than editing the XML text
// surgically, despite their own doc comments' promise to "keep source XML
// intact" -- any element that struct doesn't model (hostdev passthrough,
// TPM/launchSecurity, <qemu:commandline>, and similar less-common domain
// features) would otherwise be silently dropped on marshal with nothing to
// indicate anything went wrong, possibly not discovered until whatever
// that configuration was for turns out to be missing on a failed-over
// target VM. A real, exhaustive fix would mean editing the XML text
// surgically instead of round-tripping it through a typed struct at all --
// a much larger, higher-risk change to the single most-used code path in
// this tool (every disk's target definition, every sync) that isn't
// something to take on without the ability to test it against a wide
// range of real, complex domain definitions first.
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
func warnIfXMLElementsDropped(context, original, rewritten string) {
	missing := missingXMLElements(original, rewritten)
	if len(missing) == 0 {
		return
	}
	trace.Warning("domain xml elements present before this rewrite are missing afterward -- the domain-xml round-trip (unmarshal into a typed struct, then re-marshal) may have silently dropped configuration this tool doesn't model; verify the affected domain's definition still has everything you expect", "context", context, "missing_elements", strings.Join(missing, ", "))
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

	domcfg := &libvirtxml.Domain{}
	err := domcfg.Unmarshal(domainXML)
	if err != nil {
		return "", err
	}

	current := map[string]string{}
	if domcfg.Metadata != nil {
		current = allMetadataFields(domcfg.Metadata.XML)
	}
	for field, value := range updates {
		current[field] = value
	}
	for _, field := range removeFields {
		delete(current, field)
	}
	entry := buildMetadataEntry(current)

	if domcfg.Metadata == nil {
		domcfg.Metadata = &libvirtxml.DomainMetadata{XML: entry}
	} else if vmsyncBlockRe.MatchString(domcfg.Metadata.XML) {
		domcfg.Metadata.XML = vmsyncBlockRe.ReplaceAllLiteralString(domcfg.Metadata.XML, entry)
	} else {
		domcfg.Metadata.XML += entry
	}

	changed, err := domcfg.Marshal()
	if err != nil {
		return "", err
	}

	warnIfXMLElementsDropped("SetMetadataFields", domainXML, changed)
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
func UpdateSyncMetadata(domainXML, checkpoint, sourceHost, sourceDomain, targetRole string, checkpointAtUnix int64, sourceStopped bool) (string, error) {
	updates := map[string]string{
		MetadataFieldLastCheckpoint: checkpoint,
		MetadataFieldLastSync:       strconv.FormatInt(time.Now().Unix(), 10),
		MetadataFieldFailureCount:   "0",
		MetadataFieldReplicaSource:  ReplicaEntry(sourceHost, sourceDomain),
		MetadataFieldCheckpointAt:   strconv.FormatInt(checkpointAtUnix, 10),
	}
	remove := []string{
		MetadataFieldReplicaTargets,
		MetadataFieldPromotedAt,
		MetadataFieldPromotedBy,
		MetadataFieldPromotedFrom,
		MetadataFieldPromotionMode,
	}
	// Recorded only when true, and actively removed otherwise: a stale "the
	// source was stopped" from an earlier sync would make a later promotion
	// claim a verified zero it has no right to.
	if sourceStopped {
		updates[MetadataFieldSourceStoppedAtSync] = "1"
	} else {
		remove = append(remove, MetadataFieldSourceStoppedAtSync)
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
// last_checkpoint/last_sync_timestamp/failure_count from it if present:
// those three fields describe a domain's state as a replication TARGET,
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
	}, MetadataFieldLastCheckpoint, MetadataFieldLastSync, MetadataFieldFailureCount)
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

	domXML, err := dom.GetXMLDesc(0)
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
		return fmt.Errorf("target domain is marked replication_role=%q, meaning it is the SOURCE of a replication pair -- syncing into it would overwrite the original with its own replica; check that -source-uri/-target-uri are not reversed, or run -update-role=%s if the direction has genuinely changed", RoleSource, RoleTarget)
	case RolePromoted:
		return fmt.Errorf("target domain is marked replication_role=%q, meaning it was failed over to and is now serving live -- refusing to overwrite it with a replica from the old source, whether or not it happens to be running at this moment; run -update-role=%s to deliberately turn it back into a replication target (its current disk contents will be discarded)", RolePromoted, RoleTarget)
	case RolePaused:
		return fmt.Errorf("target domain is marked replication_role=%q, so replication into it is administratively suspended -- run -update-role=%s to resume", RolePaused, RoleTarget)
	default:
		return fmt.Errorf("target domain has an unrecognized replication_role=%q -- refusing to sync into a domain whose role this vmsync build does not understand (it was most likely written by a newer version; upgrade, or run -update-role=%s to override)", role, RoleTarget)
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
	case RoleSource, RoleTarget, RolePromoted, RolePaused, RoleNone:
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
	frag, err := ReadDomainMetadata(dom)
	if err != nil {
		return 0, fmt.Errorf("target domain %s: %w", targetDomain, err)
	}

	current := 0
	if value := allMetadataFields(frag)[MetadataFieldFailureCount]; value != "" {
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
func buildMetadataEntry(fields map[string]string) string {
	var b strings.Builder
	b.WriteString(metadataStart)
	written := make(map[string]bool, len(fields))
	writeField := func(field, value string) {
		b.WriteString("\n  <vmsync:")
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
	b.WriteString(metadataEnd)
	return b.String()
}

func parseMetadataValue(metadataXML string, field string) string {
	decoder := xml.NewDecoder(strings.NewReader("<metadata>" + metadataXML + "</metadata>"))
	for {
		token, err := decoder.Token()
		if err != nil {
			return ""
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Space != metadataNamespace || start.Name.Local != field {
			continue
		}

		for _, attr := range start.Attr {
			if attr.Name.Local == "id" {
				return attr.Value
			}
		}

		return ""
	}
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
	fields := map[string]string{}
	decoder := xml.NewDecoder(strings.NewReader("<metadata>" + metadataXML + "</metadata>"))
	for {
		token, err := decoder.Token()
		if err != nil {
			return fields
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Space != metadataNamespace || start.Name.Local == "vmsync" {
			continue
		}
		for _, attr := range start.Attr {
			if attr.Name.Local == "id" {
				fields[start.Name.Local] = attr.Value
				break
			}
		}
	}
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
func stripDomainUUID(domainXML string) (string, error) {
	domcfg := &libvirtxml.Domain{}
	if err := domcfg.Unmarshal(domainXML); err != nil {
		return "", fmt.Errorf("parse domain xml to strip uuid: %w", err)
	}

	domcfg.UUID = ""
	changed, err := domcfg.Marshal()
	if err != nil {
		return "", fmt.Errorf("re-marshal domain xml after stripping uuid: %w", err)
	}

	return changed, nil
}

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
			if !strings.HasPrefix(name, CheckpointPrefix+"-") {
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

// CreateVerifyWindowCheckpoint creates the ephemeral, domain-wide checkpoint
// -verify=online uses to detect (after the fact) which regions the guest
// wrote to during its compare window -- see VerifyWindowCheckpointName.
// Standalone (parent=""): it has no lineage relationship to the regular
// vmsync-cpt-NNNNNN chain, and since nothing ever nominates it as another
// checkpoint's parent, it can never have children either -- not that this
// matters for deletion order anyway (see checkpointDeletionOrder's own doc
// comment): it's always safe to delete on its own regardless.
func CreateVerifyWindowCheckpoint(dom *libvirt.Domain, diskTargets []disk.QcowDisk) error {
	return CreateCheckpoint(dom, VerifyWindowCheckpointName, "", diskTargets)
}

// DeleteVerifyWindowCheckpoint removes the ephemeral verify-window
// checkpoint if it exists, tolerating the case where it doesn't (already
// cleaned up, or never created this run). Called both defensively (self-
// healing a prior crashed -verify=online run, unconditionally, regardless
// of whether -verify=online is requested this run) and as real cleanup once
// a -verify=online run's compare window closes.
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
	if err := cp.Delete(0); err != nil {
		return fmt.Errorf("delete checkpoint %s: %w", checkpointName, err)
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
		if err := DeleteCheckpointIfExists(dom, name); err != nil {
			return fmt.Errorf("delete checkpoint %s: %w", name, err)
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
