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
	"os"
	"path/filepath"
	"strconv"
	"time"

	"vmsync/pkg/disk"
	"vmsync/pkg/failover"
	"vmsync/pkg/libvirtsync"
	"vmsync/pkg/trace"
	"vmsync/pkg/util"
)

// The failover modes run where the domain they act on lives, and refuse a
// remote URI.
//
// That is a deliberate restriction, not a missing feature. Promotion has to
// work when the primary site is unreachable, so it cannot depend on
// reaching anything; running it on the target host means it needs no
// network at all beyond the local libvirt socket. It also keeps the
// credential graph as it is: every SSH path this system provisions runs
// source->target, and a DR host holding credentials that can shut down
// production VMs is a much worse thing to own than a small restriction on
// where a command may be typed.
//
// A planned failover is therefore two local operations rather than one
// remote one: -shutdown-domain on the source's own host, then -promote on
// the target's. The control plane sequences them; each runs where it needs
// no credentials it does not already have.
func requireLocalURI(uri, flagName string) error {
	if util.UriUsesSSH(uri) {
		return fmt.Errorf("%s must name a LOCAL libvirt URI (for example qemu:///system): this command acts on the host it runs on, so that it works when the other site is unreachable and needs no credentials to reach it", flagName)
	}
	return nil
}

// runPromote makes a replica authoritative.
func runPromote(ctx context.Context, cfg syncConfig) error {
	if cfg.TargetURI == "" || cfg.TargetDomain == "" {
		return fmt.Errorf("-promote needs -target-uri and -target-domain naming the replica to promote")
	}
	if err := requireLocalURI(cfg.TargetURI, "-target-uri"); err != nil {
		return err
	}
	mode := failover.Mode(cfg.PromoteMode)
	switch mode {
	case failover.ModePlanned, failover.ModeForced:
	default:
		return fmt.Errorf("-promote-mode must be %q or %q, not %q", failover.ModePlanned, failover.ModeForced, cfg.PromoteMode)
	}

	// The same lock a sync takes on this target, taken locally because this
	// IS the target host. This is the positive interlock against promoting
	// a domain something is currently writing -- far better than inferring
	// it from leftover pid files, and it is why the sync path and this one
	// deliberately compute their lock path through the same helper.
	lock, err := util.AcquireRunLock(runLockDir, targetLockKey(cfg.TargetDomain))
	if err != nil {
		return fmt.Errorf("promote %s: %w", cfg.TargetDomain, err)
	}
	defer lock.Close()

	mgr, err := libvirtsync.Connect(cfg.TargetURI)
	if err != nil {
		return fmt.Errorf("connect to libvirt at %s: %w", cfg.TargetURI, err)
	}
	defer mgr.Close()

	st, err := libvirtsync.ReadFailoverState(mgr, cfg.TargetDomain)
	if err != nil {
		return err
	}
	if !st.Exists {
		return fmt.Errorf("no domain named %s on this host -- there is nothing here to promote", cfg.TargetDomain)
	}

	disksPresent, overlayPresent, err := inspectReplicaDisks(mgr, cfg.TargetDomain)
	if err != nil {
		// Refusing rather than assuming the worst or the best: this feeds a
		// safety check, and a check fed by a guess is not a check.
		return fmt.Errorf("could not inspect %s's disk files, so its replica cannot be corroborated: %w", cfg.TargetDomain, err)
	}

	plan, err := failover.AssessPromote(failover.TargetState{
		Role:             st.Role,
		LastCheckpoint:   st.LastCheckpoint,
		LastSyncUnix:     st.LastSyncUnix,
		CheckpointAtUnix: st.CheckpointAtUnix,
		ReplicaSource:    st.ReplicaSource,
		FailureCount:     st.FailureCount,
		DisksPresent:     disksPresent,
		OverlayPresent:   overlayPresent,
		// The lock above already proved no sync is writing this domain: it
		// could not have been acquired otherwise.
		SyncInFlight: false,
		Active:       st.Active,
	}, failover.PromoteOptions{
		Mode:    mode,
		Start:   cfg.Start,
		Force:   cfg.ForcePromote,
		NowUnix: time.Now().Unix(),
	})
	if err != nil {
		return fmt.Errorf("refusing to promote %s: %w", cfg.TargetDomain, err)
	}

	for _, n := range plan.Notes {
		trace.Warning("promote", "vm", cfg.TargetDomain, "note", n)
	}

	if plan.AlreadyPromoted {
		trace.Info("domain is already promoted; leaving its promotion record untouched", "vm", cfg.TargetDomain)
	} else if plan.WriteMetadata {
		// Metadata BEFORE the domain is started, always. If this process
		// dies between the two the domain is marked promoted but not
		// running -- visible, safe, and already protected by
		// TargetRoleAllowsSync. The other order would leave a RUNNING
		// domain still marked as an ordinary replica, which the next
		// scheduled sync would overwrite underneath a live workload.
		updates := map[string]string{
			libvirtsync.MetadataFieldReplicationRole: libvirtsync.RolePromoted,
			libvirtsync.MetadataFieldPromotedAt:      strconv.FormatInt(time.Now().Unix(), 10),
			libvirtsync.MetadataFieldPromotionMode:   string(mode),
		}
		if plan.PromotedFrom != "" {
			updates[libvirtsync.MetadataFieldPromotedFrom] = plan.PromotedFrom
		}
		if cfg.PromotedBy != "" {
			updates[libvirtsync.MetadataFieldPromotedBy] = cfg.PromotedBy
		}
		if err := libvirtsync.ApplyMetadata(mgr, cfg.TargetDomain, updates); err != nil {
			return fmt.Errorf("record the promotion on %s: %w", cfg.TargetDomain, err)
		}
		trace.Info("promoted", "vm", cfg.TargetDomain, "mode", mode,
			"from", plan.PromotedFrom, "by", cfg.PromotedBy, "data_loss", plan.DataLoss.String())
	}

	if plan.StartDomain {
		if err := libvirtsync.StartDomain(mgr, cfg.TargetDomain); err != nil {
			return fmt.Errorf("promotion of %s was recorded but the domain did not start: %w", cfg.TargetDomain, err)
		}
	} else if cfg.Start {
		trace.Info("domain is already running", "vm", cfg.TargetDomain)
	} else {
		trace.Warning("the promotion is recorded but the domain was NOT started; pass -start, or start it yourself", "vm", cfg.TargetDomain)
	}
	return nil
}

// inspectReplicaDisks reports whether every disk file the domain refers to
// exists, and whether an uncommitted incremental overlay was left behind.
//
// The overlay check is a glob for the "<disk>_<bitmap>" files the sync path
// writes and then commits: their presence means a copy was interrupted
// between writing and committing, so the disk beside them is mid-update.
func inspectReplicaDisks(mgr *libvirtsync.Manager, domainName string) (present bool, overlay bool, err error) {
	dom, err := mgr.LookupDomain(domainName)
	if err != nil {
		return false, false, err
	}
	defer dom.Free()

	domXML, err := dom.GetXMLDesc(0)
	if err != nil {
		return false, false, fmt.Errorf("read domain xml: %w", err)
	}
	disks, err := disk.ParseQcowDisks(domXML)
	if err != nil {
		return false, false, fmt.Errorf("parse disks: %w", err)
	}
	if len(disks) == 0 {
		return false, false, fmt.Errorf("the domain definition lists no qcow2 disks")
	}

	present = true
	for _, d := range disks {
		if _, statErr := os.Stat(d.Source); statErr != nil {
			trace.Warning("replica disk is missing", "vm", domainName, "path", d.Source, "error", statErr)
			present = false
			continue
		}
		matches, globErr := filepath.Glob(d.Source + "_*")
		if globErr == nil && len(matches) > 0 {
			trace.Warning("an uncommitted incremental overlay is present", "vm", domainName, "path", matches[0])
			overlay = true
		}
	}
	return present, overlay, nil
}

// runShutdownDomain stops a domain cleanly and marks its replication
// paused, on the host it runs on.
//
// The source half of a planned failover. Separate from -promote so each
// half runs where its credentials already reach; see requireLocalURI.
func runShutdownDomain(ctx context.Context, cfg syncConfig) error {
	if cfg.TargetURI == "" || cfg.TargetDomain == "" {
		return fmt.Errorf("-shutdown-domain needs -target-uri and -target-domain naming the domain to stop")
	}
	if err := requireLocalURI(cfg.TargetURI, "-target-uri"); err != nil {
		return err
	}

	mgr, err := libvirtsync.Connect(cfg.TargetURI)
	if err != nil {
		return fmt.Errorf("connect to libvirt at %s: %w", cfg.TargetURI, err)
	}
	defer mgr.Close()

	if err := libvirtsync.ShutdownDomain(ctx, mgr, cfg.TargetDomain, time.Duration(cfg.ShutdownTimeoutSec)*time.Second); err != nil {
		return err
	}

	// paused, not target: this domain has just stopped serving, but nothing
	// has yet decided it is expendable. Marking it a target here would
	// invite a sync to overwrite it before anyone made that call, and the
	// data it holds is the entire reason a planned failover is preferred
	// over a forced one. Inversion is where it becomes a target, deliberately.
	previous, err := libvirtsync.SetReplicationRole(mgr, cfg.TargetDomain, libvirtsync.RolePaused)
	if err != nil {
		return fmt.Errorf("domain %s was shut down but its replication role could not be set to %s: %w", cfg.TargetDomain, libvirtsync.RolePaused, err)
	}
	trace.Info("domain shut down and replication paused", "vm", cfg.TargetDomain, "previous_role", previous)
	return nil
}

// runInvert reverses a pair's direction after a failover.
//
// Runs on the OLD SOURCE's host: that is the end which already has an SSH
// path to the other one, because that is the direction syncs run.
func runInvert(ctx context.Context, cfg syncConfig) error {
	if cfg.SourceURI == "" || cfg.SourceDomain == "" || cfg.TargetURI == "" {
		return fmt.Errorf("-invert needs -source-uri/-source-domain naming the OLD source and -target-uri/-target-domain naming the promoted replica")
	}
	if cfg.TargetDomain == "" {
		cfg.TargetDomain = cfg.SourceDomain
	}

	srcMgr, err := libvirtsync.Connect(cfg.SourceURI)
	if err != nil {
		return fmt.Errorf("connect to the old source at %s: %w", cfg.SourceURI, err)
	}
	defer srcMgr.Close()
	tgtMgr, err := libvirtsync.Connect(cfg.TargetURI)
	if err != nil {
		return fmt.Errorf("connect to the promoted replica at %s: %w", cfg.TargetURI, err)
	}
	defer tgtMgr.Close()

	oldSrc, err := libvirtsync.ReadFailoverState(srcMgr, cfg.SourceDomain)
	if err != nil {
		return err
	}
	if !oldSrc.Exists {
		return fmt.Errorf("no domain named %s at %s", cfg.SourceDomain, cfg.SourceURI)
	}
	promoted, err := libvirtsync.ReadFailoverState(tgtMgr, cfg.TargetDomain)
	if err != nil {
		return err
	}
	if !promoted.Exists {
		return fmt.Errorf("no domain named %s at %s", cfg.TargetDomain, cfg.TargetURI)
	}

	srcHost := util.HostFromURIOrLocal(cfg.SourceURI)
	tgtHost := util.HostFromURIOrLocal(cfg.TargetURI)

	plan, err := failover.AssessInvert(failover.PairState{
		OldSource: failover.DomainEnd{
			Host: srcHost, Domain: cfg.SourceDomain, Role: oldSrc.Role, Active: oldSrc.Active,
			ReplicaSource: oldSrc.ReplicaSource, ReplicaTargets: oldSrc.ReplicaTargets,
			HasCheckpoints: oldSrc.HasCheckpoints,
		},
		Promoted: failover.DomainEnd{
			Host: tgtHost, Domain: cfg.TargetDomain, Role: promoted.Role, Active: promoted.Active,
			ReplicaSource: promoted.ReplicaSource, ReplicaTargets: promoted.ReplicaTargets,
			HasCheckpoints: promoted.HasCheckpoints,
		},
	})
	if err != nil {
		return fmt.Errorf("refusing to invert %s:%s <-> %s:%s: %w", srcHost, cfg.SourceDomain, tgtHost, cfg.TargetDomain, err)
	}
	if plan.AlreadyInverted {
		trace.Info("this pair is already inverted; nothing to do",
			"new_source", tgtHost+":"+cfg.TargetDomain, "new_target", srcHost+":"+cfg.SourceDomain)
		return nil
	}

	// The old source's real checkpoint objects go first. They describe a
	// chain running the other way, they would be chained onto by a later
	// fail-back, and they block the undefine that every sync into this
	// domain now ends with. Fatal if it fails: proceeding would leave a
	// pair that cannot complete a single sync.
	if plan.DropCheckpointsOnOldSource {
		dom, lookupErr := srcMgr.LookupDomain(cfg.SourceDomain)
		if lookupErr != nil {
			return lookupErr
		}
		err := libvirtsync.DeleteAllManagedCheckpoints(dom)
		dom.Free()
		if err != nil {
			return fmt.Errorf("discard %s's stale checkpoint chain before inverting: %w", cfg.SourceDomain, err)
		}
		trace.Info("discarded the old source's checkpoint chain", "vm", cfg.SourceDomain)
	}

	// New target first, then new source. Both orderings are safe from a
	// stray sync -- TargetRoleAllowsSync refuses `promoted` and `source`
	// alike -- so the ordering is chosen for recoverability instead: this
	// end is the LOCAL one, so the write that is most likely to fail (the
	// remote one) happens second, leaving the promoted domain still reading
	// `promoted`, which is exactly the precondition a retry needs.
	if err := libvirtsync.ApplyMetadata(srcMgr, cfg.SourceDomain, plan.NewTargetUpdates, plan.NewTargetRemovals...); err != nil {
		return fmt.Errorf("make %s a replication target: %w", cfg.SourceDomain, err)
	}
	if err := libvirtsync.ApplyMetadata(tgtMgr, cfg.TargetDomain, plan.NewSourceUpdates, plan.NewSourceRemovals...); err != nil {
		return fmt.Errorf("%s is now a replication target, but %s could not be made the new source -- re-run this to finish: %w",
			cfg.SourceDomain, cfg.TargetDomain, err)
	}

	trace.Info("replication direction inverted",
		"new_source", tgtHost+":"+cfg.TargetDomain, "new_target", srcHost+":"+cfg.SourceDomain)
	trace.Warning("the first sync in the new direction must be a full one: there is no checkpoint chain this way round, and the new target's disks diverged at the failover")
	return nil
}
