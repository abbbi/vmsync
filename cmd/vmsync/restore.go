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
	"io"
	"path"
	"strconv"
	"time"

	"vmsync/pkg/disk"
	"vmsync/pkg/libvirtsync"
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
	// store is this TARGET DOMAIN's restore point store, and point is the one
	// being restored from.
	store restorepoint.Store
	point restorepoint.Point
	// root is the store's path, for messages only.
	root string
	dir  string
	// replicaDir is the directory the replica's disks live in -- taken from
	// -target-disk-path when given, otherwise read off the domain itself. Kept
	// on the plan rather than passed around so every message names the
	// directory actually used, not the flag that may have been empty.
	replicaDir string

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

	// restoredBy is -restored-by, carried onto the plan so the assessment can
	// show what would be recorded; provenance is the whole record.
	restoredBy string
	provenance restorepoint.Provenance

	updates  map[string]string
	removals []string
}

// runRestoreRestorePoint is the -restore-restore-point verb.
//
// The return is named so the action journal's outcome is written on every path
// out, assessment and refusal included: a restore that was assessed and not
// carried out is a decision somebody made, and it belongs in the history
// beside the one that was.
func runRestoreRestorePoint(ctx context.Context, cfg syncConfig, tagName string) (runErr error) {
	tag, err := restorepoint.ParseTag(tagName)
	if err != nil {
		return fmt.Errorf("%w -- run -list-restore-points to see the available tags", err)
	}
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

	plan := restorePlan{tag: tag, restoredBy: cfg.RestoredBy}
	if err := checkRestoreTargetState(tgtMgr, cfg, &plan); err != nil {
		return err
	}

	replicaDir, store, err := restoreRootFor(cfg, plan)
	if err != nil {
		return err
	}
	plan.replicaDir, plan.store = replicaDir, store

	client, closeRunner, err := targetRunnerForRestorePoints(cfg)
	if err != nil {
		return err
	}
	defer closeRunner()

	// The same lock a sync takes, for the same reason and under the same key.
	// Without it a scheduled sync can be mid-copy on the exact files being
	// replaced, with a qemu-nbd holding them open -- and neither side would
	// see the other. Phase 1's read-only verbs take no lock; this one must.
	lock, err := acquireTargetRunLock(ctx, cfg, client)
	if err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	defer lock.Close()

	// Beside the replica's own disks, reached the same way every other command
	// in this verb reaches them -- over SSH when the target is remote, with os
	// when vmsync is running on the target itself.
	//
	// Before the plan is even loaded, so that a restore which then refuses is
	// recorded too. A restore is the most destructive thing vmsync does to a
	// replica on purpose, and "who asked for this, when, against which point,
	// and was it carried out" is exactly what nobody can reconstruct afterwards
	// from the disks.
	journal := newRecorderInDir(cfg, client, plan.replicaDir, cfg.TargetDomain)
	journal.Intent(ctx, journalVerbRestoreRestore, map[string]string{
		"tag":           plan.tag.String(),
		"forced":        strconv.FormatBool(cfg.ForceRestore),
		"was_role":      plan.role,
		"replaced_disk": cfg.ReplacedDiskAction,
	})
	defer func() { finishAction(ctx, journal, runErr, nil) }()

	if err := loadRestorePlan(ctx, client, cfg, &plan); err != nil {
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

// restoreRootFor decides where this restore's files are: the directory holding
// the replica's disks, and the restore point root inside it.
//
// -target-disk-path is optional HERE and required by the read-only verbs, and
// the asymmetry is not an oversight. Those verbs are built to work on a target
// that is gone, half-defined, or was never defined -- which is a state an
// operator reaching for a restore point may well be in -- so they cannot ask
// libvirt anything. A restore is the opposite case by construction: it refuses
// outright without the domain, because rolling the disks back and failing to
// invalidate the domain's replication metadata is the one outcome the next sync
// cannot detect. So by the time this runs, the domain is known to exist and has
// already told us where its disks are.
//
// Deriving is also MORE correct than the flag, not merely more convenient. The
// sync path builds its store with restorepoint.StoreFor(the actual disk path,
// target domain); the read-only verbs build theirs with StoreForDir(
// -target-disk-path, target domain). Those agree only when the flag names the
// directory the disks are really in -- so a sync run WITHOUT -target-disk-path
// (target path defaults to the source's own) takes restore points the verbs
// then cannot find. Reading the path back off the domain uses the same rule the
// sync used, whatever the flags were.
//
// The DOMAIN half of that coordinate is not optional either way: restore points
// are kept per target domain, so a directory alone names no set.
//
// An explicit flag still wins, so an operator can point at a directory
// deliberately; checkRestoreIdentity then verifies it against this same domain
// and refuses if they disagree.
func restoreRootFor(cfg syncConfig, plan restorePlan) (replicaDir string, store restorepoint.Store, err error) {
	if cfg.TargetDiskPath != "" {
		store, err = restorepoint.StoreForDir(cfg.TargetDiskPath, cfg.TargetDomain)
		return cfg.TargetDiskPath, store, err
	}

	disks, err := disk.ParseQcowDisks(plan.domXML)
	if err != nil {
		return "", restorepoint.Store{}, fmt.Errorf("read the disks of target domain %s to locate its restore points: %w -- or name the directory with -target-disk-path", cfg.TargetDomain, err)
	}
	if len(disks) == 0 {
		return "", restorepoint.Store{}, fmt.Errorf("target domain %s lists no qcow2 disks, so there is nowhere for its restore points to be -- name the directory with -target-disk-path if they are somewhere else", cfg.TargetDomain)
	}

	// A restore point is a SET, and a set only exists if the disks share a
	// directory -- which is exactly why -retention refuses to run when they do
	// not. A domain whose disks are scattered therefore has no restore points
	// to find, and guessing one of the directories would look up an empty one
	// and report "no restore point" for the wrong reason.
	replicaDir = path.Dir(disks[0].Source)
	for _, d := range disks[1:] {
		if other := path.Dir(d.Source); other != replicaDir {
			return "", restorepoint.Store{}, fmt.Errorf("target domain %s keeps its disks in more than one directory (%s and %s), so it has no single restore point set -- -retention refuses such a domain for the same reason; name the directory with -target-disk-path if you know where to look",
				cfg.TargetDomain, replicaDir, other)
		}
	}
	trace.Debug("located the restore points from the target domain's own disks", "vm", cfg.TargetDomain, "dir", replicaDir)
	store, err = restorepoint.StoreFor(disks[0].Source, cfg.TargetDomain)
	return replicaDir, store, err
}

// acquireTargetRunLock takes the target-side run lock, whichever side of the
// SSH boundary this is running on.
//
// The two implementations meet on the same file: both flock RunLockPath(dir,
// key), so a local restore and a sync driving that target over SSH contend
// correctly with each other -- which is the entire point, since the thing
// being excluded is a sync writing the very disks about to be replaced.
func acquireTargetRunLock(ctx context.Context, cfg syncConfig, runner remoteRunner) (io.Closer, error) {
	key := targetLockKey(cfg.TargetDomain)
	if holder, ok := runner.(util.CommandHolder); ok {
		return util.AcquireRemoteRunLock(ctx, holder, runLockDir, key, util.RemoteLockOptions{
			HelperPath: cfg.BridgeHelperPath,
			Identity: util.NewRunLockIdentity("restore", cfg.SourceDomain,
				util.ReplicaHost(cfg.TargetURI, cfg.LocalHostName)+":"+cfg.TargetDomain,
				cfg.ActionID, time.Now().Unix()),
		})
	}
	// Local: the same flock the source-side lock in main() uses.
	return util.AcquireRunLock(runLockDir, key)
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
	// The role gate above passes `paused` and `fenced` on purpose -- rolling a
	// paused replica back is the ordinary reason this verb exists. That is
	// exactly what leaves this verb reachable against a copy that was failed
	// over to and then shut down, because shutting a promoted domain down
	// records `paused`. Same read shape as the role, and refused rather than
	// warned past for the same reason: a trace that could not be read is
	// indistinguishable from one that says this copy served production.
	servedLive, err := libvirtsync.ReadPromotionTrace(tgtMgr, cfg.TargetDomain)
	if err != nil {
		return fmt.Errorf("read the target domain's %s: %w -- refusing to restore over a domain whose promotion history could not be established", libvirtsync.MetadataFieldLastPromotedAt, err)
	}
	if err := libvirtsync.ServedLiveAllowsOverwrite(servedLive, "restore a restore point over it"); err != nil {
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
			return fmt.Errorf("target domain %s does not refer to %s, but %s is where this restore would look for its replica (the domain's disks are %v) -- these are not the same machine's files; check that -target-domain and -target-disk-path name the same replica",
				cfg.TargetDomain, p, plan.replicaDir, ownedList)
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
	// NOTE ON SCOPE, because this check is narrower than it reads and was once
	// the only thing standing here: it detects OUTWARD replication only. A domain
	// that served live and replicated nowhere -- a promoted copy with no target of
	// its own, which is the ordinary post-failover shape -- moves no
	// last_replicated_at at all and sails straight through. That gap is closed by
	// last_promoted_at (see ServedLiveAllowsOverwrite, applied before the plan is
	// even loaded), and by TargetRoleAllowsRestore refusing `fenced` for the
	// displaced end. This check remains worth keeping for what it alone catches:
	// a domain that genuinely replicated outward after the point, which neither
	// of those two can see.
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
//
// cfg is read for identity only -- which host to record as having started the
// rollback, and this invocation's action id -- both of which go into the
// replica_incomplete value the plan would write. Nothing here acts on it.
func loadRestorePlan(ctx context.Context, client remoteRunner, cfg syncConfig, plan *restorePlan) error {
	replicaDir := plan.replicaDir
	// Confirm the tag is really there before reading anything out of it, so a
	// mistyped tag fails naming the tag rather than naming a missing file.
	point, err := findRestorePoint(ctx, client, plan.store, plan.tag)
	if err != nil {
		return err
	}
	plan.point, plan.root = point, plan.store.String()
	if plan.dir, err = point.Dir(); err != nil {
		return err
	}

	statusCmd, err := restorepoint.ReadStatusCommand(point)
	if err != nil {
		return err
	}
	out, err := client.Run(ctx, statusCmd)
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
	// The rollback's own instant, not the copy's. Fixed here rather than at
	// the moment of the write so the assessment names exactly what the
	// restore would record.
	at := time.Now().Unix()
	plan.provenance = restorepoint.Provenance{
		Tag:    plan.tag.String(),
		AtUnix: at,
		By:     plan.restoredBy,
	}
	// A restore replaces the replica's contents wholesale, which is the same
	// hazard a full copy carries and it is armed the same way: between the
	// metadata write and the end of the swap the disks are a mixture of two
	// moments, while the metadata that same write just laid down says
	// failure_count=0 and names one coherent checkpoint. Without this,
	// pkg/failover's evidence check finds nothing wrong and a half-swapped
	// machine promotes.
	//
	// stamp is the one suffix every displaced disk of this restore is renamed
	// with, so the refusal can name the exact files to put back -- and it is
	// the same stamp plan.asides were built from a few lines up, not a fresh
	// reading of the clock.
	//
	// Failing here refuses the restore before a single file is staged, which
	// is the right direction: the alternative is swapping disks with nothing
	// recording that the swap began.
	incomplete, err := libvirtsync.ReplicaIncompleteValue(
		libvirtsync.ReplicaIncompleteVerbRestore, at, cfg.ActionID,
		util.ReplicaHost(cfg.TargetURI, cfg.LocalHostName), strconv.FormatInt(stamp, 10))
	if err != nil {
		return fmt.Errorf("restore: %w -- nothing has been staged or changed", err)
	}
	plan.provenance.ReplicaIncomplete = incomplete
	plan.updates, plan.removals = restorepoint.MetadataPlan(status, plan.provenance)
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
	fmt.Printf("  restore points  %s\n", plan.root)
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
	// Listed because the assessment is what an operator reads before saying
	// yes, and this is the one field here that is written NOW and withdrawn
	// later: between the metadata write and the end of the swap it refuses a
	// promotion, and if the restore dies in that window it keeps refusing.
	// Leaving it off the list would make that refusal arrive unannounced,
	// during an incident, on a domain they were told would be promotable.
	restorepoint.FieldReplicaIncomplete,
	restorepoint.FieldRestoredFrom,
	restorepoint.FieldRestoredAt,
	restorepoint.FieldRestoredBy,
}

func annotateRestoreField(field, value string) string {
	switch field {
	case restorepoint.FieldCheckpointAt, restorepoint.FieldLastSync, restorepoint.FieldRestoredAt:
		if n, err := strconv.ParseInt(value, 10, 64); err == nil && n > 0 {
			return fmt.Sprintf("%s  (%s)", value, time.Unix(n, 0).UTC().Format("2006-01-02 15:04:05 UTC"))
		}
	case restorepoint.FieldReplicaIncomplete:
		// The raw value is a machine-readable line; what an operator needs
		// from this row is what it DOES, and that it goes away by itself.
		return value + "  (set while the disks are being swapped, so a restore that dies half-way cannot be promoted; withdrawn once every disk is in place)"
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
func applyRestore(ctx context.Context, client remoteRunner, tgtMgr *libvirtsync.Manager, cfg syncConfig, plan restorePlan) error {
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
	// asided counts how many aside copies exist, so a stand-down can remove
	// exactly those and no more.
	//
	// It is tracked for the same reason `staged` is, and the omission was worse:
	// an aside is a full-size reflink copy of a replica disk, and a run that
	// created some and then stood down left one per disk behind with nothing
	// recording them, nothing reaping them, and a message reading "no disk has
	// been replaced" -- which is true, and which an operator reads as "nothing
	// was left behind". They share extents at first and diverge as the replica
	// is written, so the cost arrives later, on the target's filesystem, with
	// no artefact pointing at the run that caused it.
	//
	// Only safe to remove on the paths where NO disk was ever replaced, which is
	// what makes each aside provably byte-identical to the live file beside it
	// and therefore worth nothing. discardAsides is called from exactly those
	// paths; see undoRestore for the one that keeps them.
	asided := 0
	discardAsides := func() {
		if asided == 0 {
			return
		}
		if _, err := client.Run(ctx, restorepoint.RestoreDiscardCommand(plan.asides[:asided]...)); err != nil {
			trace.Warning("restore: could not remove the aside copies after standing down. No disk was replaced, so each of these is an exact copy of the live disk beside it and holds nothing -- but they are full-size files that will diverge as the replica is written, so remove them by hand",
				"paths", plan.asides[:asided], "error", err)
			return
		}
		trace.Info("restore: removed the aside copies made before standing down", "count", asided)
	}
	for i, name := range plan.status.Disks {
		cmd, err := restorepoint.RestoreStageCommand(plan.point, name, plan.temps[i])
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
			discardAsides()
			return fmt.Errorf("restore: preserve the current contents of %s before replacing it: %w: %s -- no disk has been replaced", d, err, out)
		}
		asided = i + 1
	}
	for i, d := range plan.disks {
		if out, err := client.Run(ctx, restorepoint.RestorePromoteCommand(plan.temps[i], d)); err != nil {
			if i == 0 {
				// Nothing was replaced, so every aside is an exact copy of the
				// file beside it. Removed HERE rather than inside undoRestore
				// because that function's job is putting disks back, and on
				// this one path there are none to put back -- it is the
				// stand-down case wearing the rollback's clothes.
				discardAsides()
			}
			undoRestore(ctx, client, tgtMgr, cfg, plan, i, owners)
			return fmt.Errorf("restore: replace %s with the restored copy: %w: %s", d, err, out)
		}
		trace.Info("restore: replaced a replica disk", "disk", d, "from", path.Join(plan.dir, plan.status.Disks[i]))
	}

	// --- ownership -----------------------------------------------------------
	ownershipOK := true
	for i, d := range plan.disks {
		if err := applyTargetDiskOwner(ctx, client, cfg, d, owners[i]); err != nil {
			ownershipOK = false
			trace.Warning("restore: could not set ownership on the restored disk; if the promoted domain cannot open it, chown it by hand",
				"disk", d, "error", err)
		}
	}

	// --- stand the replica back up -------------------------------------------
	// The counterpart of the replica_incomplete armed in step 2, withdrawn
	// only now: every disk has been swapped and every one of them has an
	// owner qemu can open, so the replica on these files is the restore point
	// entire and a promotion of it is a promotion of a known, coherent copy.
	//
	// A second, narrow write rather than part of any of the above, because
	// there is nothing here to make it atomic with -- the swap is N renames
	// on a remote host, not a libvirt transaction -- and the only honest
	// place to clear a "this is mid-flight" marker is after the flight.
	//
	// WHAT A FAILURE OF THIS WRITE COSTS, plainly: a healthy restored replica
	// that -promote refuses, and refuses with a message about an interrupted
	// copy that in fact completed. It stays force-only until the next
	// successful sync into it clears the field (UpdateSyncMetadata does so
	// unconditionally) -- and a restored replica is paused, so that sync
	// needs -update-role=target first. Force-only is the cost, and it is the
	// right way round: the opposite order would clear the marker before the
	// disks were actually in place.
	//
	// Cleared once the swap loop has succeeded, and NOT made conditional on
	// the ownership pass above.
	//
	// It was, briefly, and that was wrong in the way this codebase keeps
	// warning about: one field would then carry two unrelated findings, and
	// the refusal it produces states something false. This marker says "a
	// full copy was STARTED and never recorded as finished, so these files
	// are a partial image". After the swap loop that is simply not true --
	// the disks are the restore point entire. A disk qemu cannot open is a
	// real problem and a different one; reporting it through this field
	// would send an operator hunting for a half-written copy that does not
	// exist, and would teach them that this refusal sometimes means
	// something else. Ownership failure stays what it is: a loud per-disk
	// warning, and a domain that fails to boot visibly if it was never
	// fixed.
	//
	// WHAT A FAILURE OF THIS WRITE COSTS, plainly: a healthy restored replica
	// that -promote refuses, and refuses with a message about an interrupted
	// copy that in fact completed. It stays force-only until the next
	// successful sync into it clears the field (UpdateSyncMetadata does so
	// unconditionally) -- and a restored replica is paused, so that sync
	// needs -update-role=target first. Force-only is the cost, and it is the
	// right way round: the opposite order would clear the marker before the
	// disks were actually in place.
	if err := libvirtsync.SetDomainMetadataFields(tgtMgr, cfg.TargetDomain, nil,
		libvirtsync.MetadataFieldReplicaIncomplete); err != nil {
		trace.Warning("restore: the disks are fully restored, but the record saying a restore was in flight could not be withdrawn. This replica is healthy and -promote will nonetheless refuse it until a successful sync clears the field; force the promotion if you need it now, or clear it by hand",
			"vm", cfg.TargetDomain, "field", libvirtsync.MetadataFieldReplicaIncomplete, "error", err)
	}
	if !ownershipOK {
		trace.Warning("restore: the disks are restored and the in-flight record has been withdrawn, but at least one disk could not be given an owner qemu can open. A promoted domain may fail to start on it -- chown it by hand before failing over to this replica",
			"vm", cfg.TargetDomain)
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
func undoRestore(ctx context.Context, client remoteRunner, tgtMgr *libvirtsync.Manager, cfg syncConfig, plan restorePlan, upTo int, owners []util.DiskOwner) {
	if upTo == 0 {
		// The asides are removed by the caller on this path, not left "for
		// inspection": nothing was replaced, so each one is a byte-identical
		// copy of the live disk beside it and there is nothing in it to
		// inspect. The staged copies DO stay -- the mv that just failed is the
		// thing somebody will want to look at, and its source is one of them.
		trace.Warning("restore: no disk had been replaced yet, so the replica is exactly as it was. The staged copies are left in place for inspection; the aside copies are being removed, because with nothing replaced each is an exact duplicate of a disk that is still there",
			"staged", plan.temps)
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
	// The asides are KEPT here, unlike the no-disk-replaced path above, and the
	// difference is that these disks were genuinely modified and then written
	// back from these files. The put-back is a cp across a link that has just
	// proved unreliable, so this is the wrong moment to delete the only second
	// copy of the pre-restore contents. They are named so they are not a leak
	// nobody can find: full-size files, one per disk, that share extents now and
	// stop sharing them as the replica is written.
	trace.Warning("restore: a disk could not be replaced, so every disk already replaced was put back. The replica is as it was before this ran, but its replication metadata was already invalidated -- it is paused, its metadata describes the restore point rather than its contents, and the record saying a restore was in flight is deliberately left armed so -promote refuses it. Re-run the restore, or run -update-role=target followed by a -reinit full sync. The pre-restore contents are kept in the aside files listed here; remove them once this replica has been confirmed good",
		"disks", len(plan.disks), "asides", plan.asides)
}
