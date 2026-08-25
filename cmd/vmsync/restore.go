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
	"context"
	"fmt"
	"path"
	"strconv"
	"time"

	"vmsync/pkg/disk"
	"vmsync/pkg/libvirtsync"
	"vmsync/pkg/remotessh"
	"vmsync/pkg/restorepoint"
	"vmsync/pkg/trace"
	"vmsync/pkg/util"

	"libvirt.org/go/libvirt"
)

// Phase 2 of restore points: putting one back over the replica in place.
//
// Phase 1's verbs (-list-restore-points, -clone-restore-point) deliberately
// touch nothing -- they answer "is Tuesday's copy clean?" without changing
// replication state, which is what an operator actually needs during an
// incident. This one changes everything about the replica, so it is a separate
// file, needs libvirt where those need none, and refuses to act on its own.
//
// WHAT A RESTORE IS FOR is worth stating plainly, because it determines the
// design: a restore is done in order to PROMOTE. If the goal were to resume
// replicating, the restore would be pointless -- the next sync from the same
// source overwrites the restored data with exactly what the operator rolled
// away from. So a restore ends with replication PAUSED (see
// restorepoint.MetadataPlan) and a replica that promotes cleanly and reports an
// honest data-loss window.
//
// See docs/design/restore-points.md.

// restorePlan is what the assessment prints and what the restore then does.
//
// Built in full before anything is written, so -restore-restore-point without
// -force-restore is a complete, side-effect-free answer to "what would this
// do", rather than a partial one that stops at the first thing it checks.
type restorePlan struct {
	tag    restorepoint.Tag
	status restorepoint.Status
	root   string
	dir    string

	// role is the target's replication_role as found, before the restore
	// pauses it.
	role string
	// What the domain says about itself, read once from its INACTIVE
	// definition and used by the identity checks. domXML is the disk
	// topology; the other two are what corroborate that this restore point
	// belongs to this domain.
	domXML           string
	replicaSource    string
	lastReplicatedAt string
	// disks, in sidecar order, as absolute replica paths.
	disks []string
	// asides parallels disks: where each displaced replica goes.
	asides []string
	// temps parallels disks: where each staged copy lands before the swap.
	temps []string

	updates  map[string]string
	removals []string
}

// runRestoreRestorePoint is the -restore-restore-point verb.
func runRestoreRestorePoint(ctx context.Context, cfg syncConfig, tagName string) error {
	tag, err := restorepoint.ParseTag(tagName)
	if err != nil {
		return fmt.Errorf("%w -- run -list-restore-points to see the available tags", err)
	}
	root, err := restorePointRoot(cfg)
	if err != nil {
		return err
	}
	replicaDir := cfg.TargetDiskPath

	// libvirt first, and before the SSH connection: the two questions that can
	// refuse this outright -- what role the domain has, and whether it is
	// running -- are both answered there, and asking them first means a
	// misdirected restore costs one libvirt round trip rather than a staged
	// copy of every disk.
	tgtMgr, err := libvirtsync.Connect(cfg.TargetURI)
	if err != nil {
		return fmt.Errorf("connect to the target hypervisor: %w", err)
	}
	defer tgtMgr.Close()

	plan := restorePlan{tag: tag, root: root, dir: restorepoint.Dir(root, tag)}
	if err := checkRestoreTargetState(tgtMgr, cfg, &plan); err != nil {
		return err
	}

	client, err := dialTargetForRestorePoints(cfg)
	if err != nil {
		return err
	}
	defer client.Close()

	// The same lock a sync takes, for the same reason and under the same key.
	// Without it a scheduled sync can be mid-copy on the exact files being
	// replaced, with a qemu-nbd holding them open -- and neither side would
	// see the other. Phase 1's read-only verbs take no lock; this one must.
	lock, err := util.AcquireRemoteRunLock(ctx, client, runLockDir, targetLockKey(cfg.TargetDomain))
	if err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	defer lock.Close()

	if err := loadRestorePlan(ctx, client, replicaDir, &plan); err != nil {
		return err
	}
	// Before the assessment, not after: the assessment describes a restore in
	// the present tense, so it must only ever describe one that would be
	// allowed to happen.
	if err := checkRestoreIdentity(cfg, plan); err != nil {
		return err
	}
	printRestoreAssessment(cfg, plan)

	if !cfg.ForceRestore {
		trace.Info("nothing was changed. This is the assessment only -- add -force-restore to carry it out")
		return nil
	}
	return applyRestore(ctx, client, tgtMgr, cfg, plan)
}

// checkRestoreTargetState answers the two questions that refuse a restore
// outright, and records the role for the assessment.
func checkRestoreTargetState(tgtMgr *libvirtsync.Manager, cfg syncConfig, plan *restorePlan) error {
	dom, err := tgtMgr.LookupDomain(cfg.TargetDomain)
	if err != nil {
		// Unlike the read-only verbs, which work on a target that is gone
		// half-defined or never existed, a restore needs the domain: the
		// metadata invalidation is the half of the operation that keeps the
		// rolled-back disks from being silently synced over, and there is
		// nowhere to write it. Restoring the files alone would leave them with
		// no interlock at all.
		return fmt.Errorf("target domain %s not found on %s: %w -- a restore needs the domain, because rolling the disks back without invalidating its replication metadata is what makes the next sync corrupt them silently. Use -clone-restore-point to materialise the copy somewhere else instead", cfg.TargetDomain, cfg.TargetURI, err)
	}
	defer dom.Free()

	active, err := libvirtsync.DomainActive(dom)
	if err != nil {
		return fmt.Errorf("determine whether target domain %s is running: %w", cfg.TargetDomain, err)
	}
	if active {
		return fmt.Errorf("target domain %s is running -- shut it down before restoring, or its disks will be replaced underneath a live guest", cfg.TargetDomain)
	}

	role, err := libvirtsync.ReadReplicationRole(tgtMgr, cfg.TargetDomain)
	if err != nil {
		// Refused, not warned past. An unreadable role is indistinguishable
		// from a role that would have said "promoted", and that is the one
		// this check exists to catch.
		return fmt.Errorf("read the target domain's replication_role: %w -- refusing to restore over a domain whose role could not be established", err)
	}
	if err := libvirtsync.TargetRoleAllowsRestore(role); err != nil {
		return err
	}
	plan.role = role

	// DOMAIN_XML_INACTIVE, matching every other metadata read in vmsync: the
	// metadata is written to the persistent definition, so a running domain's
	// live document would not carry it. The disk topology is read from the
	// same document deliberately -- what a restore must not do is replace
	// files the PERSISTENT definition does not reference.
	xml, err := dom.GetXMLDesc(libvirt.DOMAIN_XML_INACTIVE)
	if err != nil {
		return fmt.Errorf("read the target domain's definition: %w", err)
	}
	plan.domXML = xml
	plan.replicaSource, _ = libvirtsync.ParseMetadataField(xml, libvirtsync.MetadataFieldReplicaSource)
	plan.lastReplicatedAt, _ = libvirtsync.ParseMetadataField(xml, libvirtsync.MetadataFieldLastReplicatedAt)
	return nil
}

// checkRestoreIdentity refuses a restore that has not been shown to belong to
// the domain it names.
//
// Three separate ways of getting the wrong machine, and none of the checks
// above catches any of them. The role gate asks what the domain IS, not
// whether this restore point is ITS history; and the disk-presence check in
// loadRestorePlan compares the restore point against the directory it was
// taken from, so it can never disagree.
//
// This is the shape -promote already uses: it will not promote a domain with
// no replica_source, on the reasoning that a check fed by a guess is not a
// check. A restore writes more than a promotion does and had none.
func checkRestoreIdentity(cfg syncConfig, plan restorePlan) error {
	// 1. Does this domain actually own these files?
	//
	// -target-domain and -target-disk-path are independent flags and nothing
	// binds them. Crossed between two replicas, the disks of one are rolled
	// back while the metadata of the OTHER is rewritten and paused -- which
	// leaves the first with contents older than its metadata claims, the one
	// state the next incremental sync cannot detect. A path comparison, so it
	// needs no SSH and works against a remote target URI.
	disks, err := disk.ParseQcowDisks(plan.domXML)
	if err != nil {
		return fmt.Errorf("read the disks of target domain %s: %w -- refusing to replace files without confirming the domain refers to them", cfg.TargetDomain, err)
	}
	owned := make(map[string]bool, len(disks))
	var ownedList []string
	for _, d := range disks {
		owned[d.Source] = true
		ownedList = append(ownedList, d.Source)
	}
	for _, p := range plan.disks {
		if !owned[p] {
			return fmt.Errorf("target domain %s does not refer to %s, but -target-disk-path %s says that is where its replica lives (the domain's disks are %v) -- these are not the same machine's files; check that -target-domain and -target-disk-path name the same replica",
				cfg.TargetDomain, p, cfg.TargetDiskPath, ownedList)
		}
	}

	// 2. Was this restore point taken from the same source this domain is a
	// replica of? Both strings are built by the same expression on both
	// sides, so a genuine pair matches byte for byte.
	if plan.status.Source != "" && plan.replicaSource != "" && plan.status.Source != plan.replicaSource {
		return fmt.Errorf("restore point %s was taken while replicating from %q, but %s records replica_source=%q -- this restore point is another pair's history",
			plan.tag, plan.status.Source, cfg.TargetDomain, plan.replicaSource)
	}

	// 3. Has this domain served as a SOURCE since the point was taken?
	//
	// This is what tells the two meanings of replication_role=paused apart.
	// TargetRoleAllowsRestore allows paused, deliberately -- an operator who
	// paused replication to investigate is exactly the one who then wants to
	// roll the replica back. But -shutdown-domain also writes paused, on a
	// domain that was serving live and has just been stopped by a planned
	// failover or a fence, and its disks then hold everything written since
	// the last sync in the other direction. last_replicated_at moves on every
	// successful sync a domain performs AS a source, so a value newer than
	// this point means the domain replicated outward after the point was
	// captured -- and the point therefore cannot contain what its disks hold.
	if plan.lastReplicatedAt != "" && plan.status.TakenAt > 0 {
		if at, perr := strconv.ParseInt(plan.lastReplicatedAt, 10, 64); perr == nil && at > plan.status.TakenAt {
			return fmt.Errorf("%s last replicated OUT to another host at %s, which is after restore point %s was taken (%s) -- this domain has served as a source since, so its disks hold writes no replica of it contains and this restore point would discard them. If it is genuinely a replica again, run -update-role=%s first",
				cfg.TargetDomain,
				time.Unix(at, 0).UTC().Format("2006-01-02 15:04:05 UTC"),
				plan.tag,
				time.Unix(plan.status.TakenAt, 0).UTC().Format("2006-01-02 15:04:05 UTC"),
				libvirtsync.RoleTarget)
		}
	}
	return nil
}

// loadRestorePlan fills in everything the restore would touch, writing nothing.
func loadRestorePlan(ctx context.Context, client remoteRunner, replicaDir string, plan *restorePlan) error {
	// Confirm the tag is really there before reading anything out of it, so a
	// mistyped tag fails naming the tag rather than naming a missing file.
	listing, err := listRestorePoints(ctx, client, plan.root)
	if err != nil {
		return err
	}
	found := false
	for _, t := range listing.Points {
		if t.String() == plan.tag.String() {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("no restore point %q in %s -- run -list-restore-points to see what is there", plan.tag, plan.root)
	}

	out, err := client.Run(ctx, restorepoint.ReadStatusCommand(plan.root, plan.tag))
	if err != nil {
		return fmt.Errorf("read the status sidecar of restore point %s: %w", plan.tag, err)
	}
	status, err := restorepoint.DecodeStatus([]byte(out))
	if err != nil {
		return fmt.Errorf("restore point %s: %w", plan.tag, err)
	}
	if len(status.Disks) == 0 {
		return fmt.Errorf("restore point %s lists no disks; it may have been written by a newer vmsync", plan.tag)
	}
	plan.status = status

	// Every disk the restore point holds must still be a file at the replica's
	// path. A name that is missing means the replica's shape changed since --
	// a disk was removed on the source, or the replica was rebuilt somewhere
	// else -- and restoring the intersection would produce a machine that is
	// half one point in history and half another. Refuse and say which.
	presence, err := client.Run(ctx, restorepoint.ReplicaPresentCommand(replicaDir, status.Disks))
	if err != nil {
		return fmt.Errorf("check which of restore point %s's disks the replica still has: %w", plan.tag, err)
	}
	missing, err := restorepoint.ParseReplicaPresent(presence, status.Disks)
	if err != nil {
		return fmt.Errorf("restore point %s: %w", plan.tag, err)
	}
	if len(missing) > 0 {
		return fmt.Errorf("restore point %s holds %d disk(s) the replica no longer has at %s: %v -- the replica's disk set has changed since this point was taken, and restoring only the ones that match would leave a machine assembled from two different moments. Use -clone-restore-point to materialise it somewhere else and inspect it",
			plan.tag, len(missing), replicaDir, missing)
	}

	stamp := time.Now().Unix()
	for _, name := range status.Disks {
		replica := path.Join(replicaDir, name)
		plan.disks = append(plan.disks, replica)
		plan.asides = append(plan.asides, replica+replacedDiskSuffix+strconv.FormatInt(stamp, 10))
		plan.temps = append(plan.temps, restorepoint.RestoreTempPath(replica, stamp))
	}
	plan.updates, plan.removals = restorepoint.MetadataPlan(status)
	return nil
}

// printRestoreAssessment says what is about to happen, in the terms an operator
// standing in front of a broken VM actually needs.
//
// Printed on every run, including the one that goes ahead: an operator who
// passed -force-restore because they had already read this still wants the
// record of what it did in the same log as the doing.
func printRestoreAssessment(cfg syncConfig, plan restorePlan) {
	age := time.Since(time.Unix(plan.status.TakenAt, 0)).Round(time.Minute)

	fmt.Printf("\nRestore assessment for %s on %s\n", cfg.TargetDomain, cfg.TargetURI)
	fmt.Printf("  restore point   %s\n", plan.tag)
	fmt.Printf("  taken           %s (%s ago)\n",
		time.Unix(plan.status.TakenAt, 0).UTC().Format("2006-01-02 15:04:05 UTC"), age)
	fmt.Printf("  checkpoint      %s\n", orNone(plan.status.Checkpoint))
	// Both, side by side. These are the two values the operator is being
	// asked to trust are the same pair, and checkRestoreIdentity has just
	// refused if they disagree -- showing only one of them would hide half of
	// what that check was looking at.
	fmt.Printf("  taken from      %s\n", orNone(plan.status.Source))
	fmt.Printf("  replica of      %s\n", orNone(plan.replicaSource))
	fmt.Printf("  verify          %s%s\n", orNone(plan.status.Verify), verifyCaveat(plan.status.Verify))
	fmt.Printf("  target role     %s\n", orNone(plan.role))
	fmt.Printf("\n  disks to replace (%d):\n", len(plan.disks))
	for i, d := range plan.disks {
		fmt.Printf("    %s\n", d)
		if cfg.ReplacedDiskAction == replacedDiskDelete {
			fmt.Printf("      current contents: DELETED after the swap (-replaced-disk-action=delete)\n")
		} else {
			fmt.Printf("      current contents: kept at %s\n", plan.asides[i])
		}
	}
	fmt.Printf("\n  replication metadata afterwards:\n")
	for _, f := range restoreFieldOrder {
		if v, ok := plan.updates[f]; ok {
			fmt.Printf("    %-24s %s\n", f, annotateRestoreField(f, v))
			continue
		}
		for _, r := range plan.removals {
			if r == f {
				fmt.Printf("    %-24s (removed)\n", f)
				break
			}
		}
	}
	fmt.Printf("\n  after this, replication into %s is PAUSED and the next sync will refuse.\n", cfg.TargetDomain)
	fmt.Printf("  to promote this restored replica:  vmsync -promote -target-uri %s -target-domain %s\n", cfg.TargetURI, cfg.TargetDomain)
	fmt.Printf("  to go back to replicating instead: vmsync -update-role=target ... then a -reinit full sync,\n")
	fmt.Printf("                                     which rebuilds from the source and discards what was just restored.\n\n")
}

// restoreFieldOrder fixes the order the assessment lists metadata in, so two
// runs are diffable and so the field that matters most is read first.
var restoreFieldOrder = []string{
	restorepoint.FieldLastCheckpoint,
	restorepoint.FieldCheckpointAt,
	restorepoint.FieldLastSync,
	restorepoint.FieldSourceStoppedAtSync,
	restorepoint.FieldFailureCount,
	restorepoint.FieldReplicationRole,
}

func annotateRestoreField(field, value string) string {
	switch field {
	case restorepoint.FieldCheckpointAt, restorepoint.FieldLastSync:
		if n, err := strconv.ParseInt(value, 10, 64); err == nil && n > 0 {
			return fmt.Sprintf("%s  (%s)", value, time.Unix(n, 0).UTC().Format("2006-01-02 15:04:05 UTC"))
		}
	}
	return value
}

func orNone(s string) string {
	if s == "" {
		return "(none recorded)"
	}
	return s
}

// verifyCaveat spells out what a verify state does and does not promise.
//
// "not-run" is the ordinary case rather than a warning sign -- -verify is
// expensive and runs on its own cadence -- but an operator about to discard a
// replica deserves to be told which of the two they are looking at instead of
// having to know that a restore point is taken before verify ever runs.
func verifyCaveat(v string) string {
	switch v {
	case restorepoint.VerifyPassed:
		return "  (a compare against the source ran and matched at the time)"
	case restorepoint.VerifyFailed:
		return "  (a compare against the source ran and MISMATCHED -- this copy is known bad)"
	default:
		return "  (never compared against the source; restore points are taken before -verify runs, so this is the usual state, not a fault)"
	}
}

// applyRestore carries the plan out.
//
// Order is the safety argument, and it is not the obvious one:
//
//  1. stage every disk, committing nothing
//  2. write the metadata
//  3. swap the disks in
//
// Metadata BEFORE the disks, because of how each half fails. Metadata written
// and disks not swapped leaves a replica whose contents are NEWER than its
// metadata claims: replication is paused, a promotion understates what it has,
// and nothing is silently wrong. Disks swapped and metadata not written leaves
// a replica whose contents are OLDER than its metadata claims -- which is
// precisely the state the next incremental sync cannot detect and will corrupt.
// One of those two failure modes is recoverable and one is not.
func applyRestore(ctx context.Context, client *remotessh.Client, tgtMgr *libvirtsync.Manager, cfg syncConfig, plan restorePlan) error {
	// Remembered before anything displaces the files, exactly as -reinit does:
	// the copies are created by the SSH user (often root), and a replica qemu
	// cannot open is a restore that produced an unbootable VM.
	owners := make([]util.DiskOwner, len(plan.disks))
	for i, d := range plan.disks {
		if out, err := client.Run(ctx, util.StatOwnerCommand(d)); err == nil {
			owners[i] = util.ParseStatOwner(out)
		}
	}

	// --- 1. stage ------------------------------------------------------------
	staged := 0
	discardStaged := func() {
		if staged == 0 {
			return
		}
		if _, err := client.Run(ctx, restorepoint.RestoreDiscardCommand(plan.temps[:staged]...)); err != nil {
			trace.Warning("restore: could not remove the staged copies after standing down; they cost nothing but should be removed by hand",
				"paths", plan.temps[:staged], "error", err)
		}
	}
	for i, name := range plan.status.Disks {
		cmd, err := restorepoint.RestoreStageCommand(plan.root, plan.tag, name, plan.temps[i])
		if err != nil {
			discardStaged()
			return fmt.Errorf("restore: %w", err)
		}
		if out, err := client.Run(ctx, cmd); err != nil {
			discardStaged()
			return fmt.Errorf("restore: stage %s from restore point %s: %w: %s", name, plan.tag, err, out)
		}
		staged++
		trace.Info("restore: staged a disk beside the replica", "disk", name, "at", plan.temps[i])
	}

	// --- 2. metadata ---------------------------------------------------------
	// One call for every field. SetDomainMetadataFields refuses rather than
	// retries if the metadata changed between its read and its write, so
	// splitting this up would give a concurrent writer several chances to make
	// half of it land.
	if err := libvirtsync.SetDomainMetadataFields(tgtMgr, cfg.TargetDomain, plan.updates, plan.removals...); err != nil {
		discardStaged()
		return fmt.Errorf("restore: invalidate the replication metadata on %s: %w -- nothing was changed on disk, so the replica is exactly as it was", cfg.TargetDomain, err)
	}
	trace.Info("restore: replication metadata now describes the restored point, and replication is paused",
		"tag", plan.tag.String(), "was_role", orNone(plan.role))

	// --- 3. swap -------------------------------------------------------------
	// Aside first for ALL disks, then promote for all: after this loop every
	// displaced replica exists under a second name, so a promote that fails
	// part-way can be undone. Both halves are per-disk atomic renames or
	// reflinks within one directory, so a disk is never half-swapped.
	for i, d := range plan.disks {
		if out, err := client.Run(ctx, restorepoint.RestoreAsideCommand(d, plan.asides[i])); err != nil {
			discardStaged()
			return fmt.Errorf("restore: preserve the current contents of %s before replacing it: %w: %s -- no disk has been replaced", d, err, out)
		}
	}
	for i, d := range plan.disks {
		if out, err := client.Run(ctx, restorepoint.RestorePromoteCommand(plan.temps[i], d)); err != nil {
			undoRestore(ctx, client, tgtMgr, cfg, plan, i, owners)
			return fmt.Errorf("restore: replace %s with the restored copy: %w: %s", d, err, out)
		}
		trace.Info("restore: replaced a replica disk", "disk", d, "from", path.Join(plan.dir, plan.status.Disks[i]))
	}

	// --- ownership -----------------------------------------------------------
	for i, d := range plan.disks {
		if err := applyTargetDiskOwner(ctx, client, cfg, d, owners[i]); err != nil {
			trace.Warning("restore: could not set ownership on the restored disk; if the promoted domain cannot open it, chown it by hand",
				"disk", d, "error", err)
		}
	}

	// --- the displaced contents ---------------------------------------------
	if cfg.ReplacedDiskAction == replacedDiskDelete {
		if out, err := client.Run(ctx, restorepoint.RestoreDiscardCommand(plan.asides...)); err != nil {
			trace.Warning("restore: could not remove the displaced replica contents (-replaced-disk-action=delete); remove them by hand",
				"paths", plan.asides, "error", err, "output", out)
		} else {
			trace.Info("restore: removed the displaced replica contents", "count", len(plan.asides))
		}
	} else {
		trace.Warning("restore: the replica's previous contents were kept (-replaced-disk-action=rename). Nothing reaps these -- they share extents with the restore points for now, but they are what the target pays for the rollback being undoable. Remove them once the restore is confirmed good",
			"paths", plan.asides)
	}

	trace.Info("restore complete", "tag", plan.tag.String(), "disks", len(plan.disks), "domain", cfg.TargetDomain)
	trace.Warning("replication into this domain is now PAUSED and the next sync will refuse. Promote it, or run -update-role=target followed by a -reinit full sync to go back to replicating (which rebuilds from the source and discards what was just restored)")
	return nil
}

// undoRestore puts back the disks a failed multi-disk swap already replaced.
//
// Best effort by necessity -- if the target host is refusing renames there is
// no reason to think it will accept these either -- so every failure is
// reported individually and loudly. The end state is named explicitly either
// way, because "some disks are from Tuesday and some are from today" is a state
// nobody should have to work out from a stack trace.
func undoRestore(ctx context.Context, client *remotessh.Client, tgtMgr *libvirtsync.Manager, cfg syncConfig, plan restorePlan, upTo int, owners []util.DiskOwner) {
	if upTo == 0 {
		trace.Warning("restore: no disk had been replaced yet, so the replica is exactly as it was. The staged copies and the aside copies are left in place for inspection",
			"staged", plan.temps, "asides", plan.asides)
		return
	}
	failed := 0
	for i := 0; i < upTo; i++ {
		if out, err := client.Run(ctx, restorepoint.RestoreUndoCommand(plan.asides[i], plan.disks[i])); err != nil {
			failed++
			trace.Warning("restore: could not put back a disk that had already been replaced -- this disk is from the restore point while others are not",
				"disk", plan.disks[i], "aside", plan.asides[i], "error", err, "output", out)
			continue
		}
		// The promote replaced this path's inode with the staged copy's,
		// which cp created as the SSH user -- usually root. Putting the
		// CONTENTS back does not put the ownership back, and a replica qemu
		// cannot open is not a replica. The ordinary success path does this
		// too; skipping it here would mean a failed restore left the domain
		// unbootable when it had been bootable before.
		if err := applyTargetDiskOwner(ctx, client, cfg, plan.disks[i], owners[i]); err != nil {
			trace.Warning("restore: put a disk's contents back but could not restore its ownership; chown it by hand before starting the domain",
				"disk", plan.disks[i], "error", err)
		}
	}
	if failed > 0 {
		// A log line is not an interlock, and this is the one state in the
		// whole feature that must not be promoted: the disks are a mixture of
		// two moments, while the metadata written moments ago says
		// failure_count=0 and names a single coherent checkpoint -- so
		// pkg/failover's evidence check finds nothing wrong and -promote
		// accepts it without a word. Writing a non-zero failure_count is what
		// that check already refuses on, so it makes the refusal survive the
		// terminal this ran in.
		if err := libvirtsync.SetDomainMetadataFields(tgtMgr, cfg.TargetDomain,
			map[string]string{libvirtsync.MetadataFieldFailureCount: strconv.Itoa(failed)}); err != nil {
			trace.Warning("restore: could not mark the domain as inconsistent in its metadata, so nothing will stop a promotion of it. Do not promote or sync this domain until its disks are sorted out by hand",
				"vm", cfg.TargetDomain, "error", err)
		}
		trace.Warning("restore: the rollback of a partial restore did not fully succeed. The replica is now assembled from two different moments and MUST NOT be promoted or synced until it is sorted out by hand; each disk's pre-restore contents are in its aside file. failure_count has been set so that -promote refuses it",
			"disks", len(plan.disks), "not_put_back", failed, "asides", plan.asides)
		return
	}
	trace.Warning("restore: a disk could not be replaced, so every disk already replaced was put back. The replica is as it was before this ran, but its replication metadata was already invalidated -- it is paused and its metadata describes the restore point rather than its contents. Re-run the restore, or run -update-role=target followed by a -reinit full sync",
		"disks", len(plan.disks))
}
