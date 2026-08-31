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
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"vmsync/pkg/disk"
	"vmsync/pkg/failover"
	"vmsync/pkg/libvirtsync"
	"vmsync/pkg/trace"
	"vmsync/pkg/util"

	"libvirt.org/go/libvirt"
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
		// Written by the sync that produced this replica: the source was
		// already stopped when its checkpoint was taken, so nothing was
		// written afterwards. This is what turns "planned failover" from a
		// claim into a measurement.
		SourceStoppedAtSync: st.SourceStoppedAtSync,
		// Changes nothing about whether this promotion is allowed; it changes
		// what the plan SAYS about the window, which on a rolled-back replica
		// is the age of a copy somebody chose rather than replication lag.
		RestoredFrom: st.RestoredFrom,
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

	// Resolved before either branch, because arming is useful in both. The
	// recovery case is real and not rare: promote, then notice the source is
	// still serving, then re-run with -fence-source. That domain is already
	// promoted, so nothing below would write -- and the operator would be
	// left with no way to arm a fence short of demoting and promoting again.
	fenced, err := resolveFenceSource(cfg.FenceSource, plan.PromotedFrom, st.ReplicaSource)
	if err != nil {
		return fmt.Errorf("refusing to promote %s: %w", cfg.TargetDomain, err)
	}

	if plan.AlreadyPromoted {
		trace.Info("domain is already promoted; leaving its promotion record untouched", "vm", cfg.TargetDomain)
		if fenced != "" {
			// Only the fence fields. The promotion's own timestamp and
			// actor stay exactly as the original promotion left them --
			// that record describes when the failover happened, and a
			// later fence does not change that.
			updates, err := fenceUpdates(fenced, cfg.PromotedBy)
			if err != nil {
				return err
			}
			if err := libvirtsync.ApplyMetadata(mgr, cfg.TargetDomain, updates); err != nil {
				return fmt.Errorf("arm a fence on the already-promoted %s: %w", cfg.TargetDomain, err)
			}
			trace.Info("armed a fence against the displaced source on an already-promoted domain",
				"vm", cfg.TargetDomain, "fence_source", fenced)
		}
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

		// Arming in the SAME write as the promotion record is the point.
		// Two writes could leave a domain promoted with no fence (the split
		// brain the operator asked to prevent) or -- worse -- a fence with
		// no promotion, which is a token authorising a shutdown that
		// nothing justifies. One metadata call makes both true or neither.
		if fenced != "" {
			armed, err := fenceUpdates(fenced, cfg.PromotedBy)
			if err != nil {
				return err
			}
			for k, v := range armed {
				updates[k] = v
			}
		}

		if err := libvirtsync.ApplyMetadata(mgr, cfg.TargetDomain, updates); err != nil {
			return fmt.Errorf("record the promotion on %s: %w", cfg.TargetDomain, err)
		}
		trace.Info("promoted", "vm", cfg.TargetDomain, "mode", mode,
			"from", plan.PromotedFrom, "by", cfg.PromotedBy, "data_loss", plan.DataLoss.String())
		if fenced != "" {
			trace.Info("armed a fence against the displaced source; it will shut itself down when its agent next checks, once and only once",
				"vm", cfg.TargetDomain, "fence_source", fenced)
		} else {
			trace.Info("no fence was armed, so the source is free to keep running; pass -fence-source to stop it",
				"vm", cfg.TargetDomain)
		}
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

// fenceReport is what -read-fence prints: everything a displaced source
// needs to decide whether to stop itself, and nothing else.
//
// Deliberately the raw observation rather than a verdict. The decision needs
// one input this command cannot see -- whether this fence was acted on
// before, which lives in the agent's durable ledger -- so computing a verdict
// here would produce an authoritative-looking answer that is missing the
// condition preventing a token from firing twice.
type fenceReport struct {
	// Reachable distinguishes "the peer says there is no fence" from "the
	// peer could not be asked". The difference is the whole point: a
	// partition is EXACTLY when a promotion is most likely to have happened
	// and least likely to be visible, and treating silence as "no fence"
	// keeps this domain running, which is the safe direction. Treating it as
	// a fence would shut down a healthy primary every time a link flapped.
	Reachable bool   `json:"reachable"`
	Error     string `json:"error,omitempty"`

	TargetRef    string              `json:"target_ref"`
	TargetRole   string              `json:"target_role,omitempty"`
	TargetActive bool                `json:"target_active"`
	Fence        failover.FenceToken `json:"fence"`
}

// runReadFence reports the fence a peer's promotion armed, if any.
//
// The one failover mode that takes a REMOTE uri, and the only one that
// needs to: it asks the other site a question rather than acting on this
// one. It reads and prints; it changes nothing anywhere. The shutdown that
// may follow is a separate, deliberate step by the caller -- which is what
// lets the agent record its intent in the ledger between the two.
func runReadFence(cfg syncConfig) error {
	if cfg.TargetURI == "" || cfg.TargetDomain == "" {
		return fmt.Errorf("-read-fence needs -target-uri and -target-domain naming the PROMOTED peer to ask")
	}

	rep := fenceReport{
		// ReplicaHost, not HostFromURIOrLocal: this reference is an identity
		// written for another machine to read and compare, not an address to
		// dial. The two differ exactly where it matters -- a local uri
		// resolves to a loopback literal, which names every host and so
		// identifies none.
		TargetRef: libvirtsync.ReplicaEntry(util.ReplicaHost(cfg.TargetURI, cfg.LocalHostName), cfg.TargetDomain),
	}

	// An unreachable peer is a normal answer here, not an error: it is
	// reported as unreachable on stdout and the command still succeeds, so
	// the caller can tell the two apart without parsing an error string or
	// mapping an exit code. Exiting non-zero would make every network blip
	// look like a broken invocation.
	mgr, err := libvirtsync.Connect(cfg.TargetURI)
	if err != nil {
		rep.Error = err.Error()
		return printFenceReport(rep)
	}
	defer mgr.Close()

	st, err := libvirtsync.ReadFailoverState(mgr, cfg.TargetDomain)
	if err != nil {
		rep.Error = err.Error()
		return printFenceReport(rep)
	}
	rep.Reachable = true
	if !st.Exists {
		// Reached the host, and the domain is not there. Not an error and
		// emphatically not a fence: a peer with no such domain has promoted
		// nothing.
		return printFenceReport(rep)
	}
	rep.TargetRole = st.Role
	rep.TargetActive = st.Active
	rep.Fence = st.Fence
	return printFenceReport(rep)
}

func printFenceReport(rep fenceReport) error {
	out, err := json.Marshal(rep)
	if err != nil {
		return fmt.Errorf("encode the fence report: %w", err)
	}
	fmt.Println(string(out))
	return nil
}

// resolveFenceSource turns the -fence-source flag into the reference to
// write, or "" for no fence at all.
//
// candidates are the references worth using for bare -fence-source, best
// first: the promotion's own corroborated promoted_from, then the target's
// raw replica_source. Both come from the target's metadata; neither is
// invented here, because a fence naming the wrong host would shut down an
// uninvolved production VM.
func resolveFenceSource(flagValue string, candidates ...string) (string, error) {
	flagValue = strings.TrimSpace(flagValue)
	if flagValue == "" {
		return "", nil
	}
	if flagValue != fenceSourceAuto {
		// An explicit reference. Validated for shape only: whether the host
		// exists is not knowable from here, and the fence is addressed
		// anyway -- a source only ever acts on a token naming itself, so a
		// typo produces a fence nobody honours rather than a wrong shutdown.
		if _, _, ok := splitReplicaRef(flagValue); !ok {
			return "", fmt.Errorf("-fence-source %q is not a host:domain reference", flagValue)
		}
		return flagValue, nil
	}
	for _, c := range candidates {
		if c = strings.TrimSpace(c); c != "" {
			if _, _, ok := splitReplicaRef(c); ok {
				return c, nil
			}
		}
	}
	// Refusing rather than quietly promoting without a fence. The operator
	// asked for one; silently not arming it would leave them believing the
	// source had been dealt with when nothing will ever stop it.
	return "", fmt.Errorf(
		"-fence-source was asked to work out the source by itself, but this domain records no usable replica_source; name it explicitly, as -fence-source=host:domain")
}

// fenceUpdates builds the metadata a fence is made of.
func fenceUpdates(source, by string) (map[string]string, error) {
	id, err := failover.NewFenceID()
	if err != nil {
		return nil, fmt.Errorf("arm a fence: %w", err)
	}
	updates := map[string]string{
		libvirtsync.MetadataFieldFenceID:      id,
		libvirtsync.MetadataFieldFenceSource:  source,
		libvirtsync.MetadataFieldFenceArmedAt: strconv.FormatInt(time.Now().Unix(), 10),
	}
	if by != "" {
		updates[libvirtsync.MetadataFieldFenceArmedBy] = by
	}
	return updates, nil
}

// splitReplicaRef checks a "host:domain" reference has both halves.
func splitReplicaRef(ref string) (host, domain string, ok bool) {
	i := strings.LastIndex(ref, ":")
	if i <= 0 || i == len(ref)-1 {
		return "", "", false
	}
	return ref[:i], ref[i+1:], true
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

	srcHost := util.ReplicaHost(cfg.SourceURI, cfg.LocalHostName)
	tgtHost := util.ReplicaHost(cfg.TargetURI, cfg.LocalHostName)

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
		err := dropCheckpointChain(dom, cfg.SourceDomain, cfg.SourceURI, "-invert")
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
	warnAboutReversedDiskPaths(srcMgr, tgtMgr, cfg)
	return nil
}

// warnAboutReversedDiskPaths tells the operator where the reversed sync has
// to put its disks, when that is not where the old direction put them.
//
// -target-disk-path describes where THIS pair's replicas went, so after an
// inversion it names the new SOURCE's own disks. Reused unchanged on the
// reversed sync it aims at the wrong directory on the wrong host: either
// failing because it does not exist there, or -- where it does -- writing
// the replica there and redefining the domain to match, silently orphaning
// the original disk.
//
// Only a warning, and it has to be. This command does not run the reversed
// sync and has no schedule to correct; the next invocation's flags are the
// operator's to type. (The control plane, which does own the schedule,
// re-aims it itself -- see moveScheduleEntryLocked.)
//
// Best-effort throughout: this runs after the inversion has already been
// applied, so nothing here may turn a completed inversion into a failure.
func warnAboutReversedDiskPaths(srcMgr, tgtMgr *libvirtsync.Manager, cfg syncConfig) {
	newTargetDir, ok := singleDiskDir(srcMgr, cfg.SourceDomain)
	if !ok {
		return
	}
	newSourceDir, ok := singleDiskDir(tgtMgr, cfg.TargetDomain)
	if !ok {
		return
	}
	if newTargetDir == newSourceDir {
		// Symmetric layout: leaving -target-disk-path unset already puts the
		// copy at the source's own path, which is the right place.
		return
	}
	trace.Warning("the two ends keep their disks in different directories, so the reversed sync needs a different -target-disk-path than the old direction used -- without it the copy lands somewhere the new target does not keep its disks, and redefining the domain to match would orphan the originals",
		"new_source_disks", newSourceDir, "new_target_disks", newTargetDir,
		"use", "-target-disk-path "+newTargetDir)
}

// singleDiskDir reports the one directory holding every qcow2 disk of a
// domain. False when there are none, or when they span several -- which
// -target-disk-path cannot express in either direction.
// cleanVerb names whichever flag the operator actually typed, for messages
// that tell them what to re-run rather than what vmsync calls it internally.
func cleanVerb(cfg syncConfig) string {
	if cfg.ForceClean {
		return "-force-clean"
	}
	return "-reinit"
}

// forceCleanTargetDomain removes the target domain definition, whatever state
// it is in, so a wedged one cannot block the sync that is about to replace it.
//
// Tolerant of almost everything by design -- a domain that is already gone, or
// was never defined, is a success here, because the postcondition this
// promises is "no target domain", not "a target domain was removed".
//
// The one thing it will NOT do is touch a RUNNING domain. -force-clean
// overrides the replication-role interlock because a promoted replica is still
// only data; it does not override this one, because undefining a domain and
// deleting the disks underneath a live guest corrupts what that guest is
// actively writing and cannot be undone by re-running anything.
func forceCleanTargetDomain(tgtMgr *libvirtsync.Manager, cfg syncConfig) error {
	dom, err := tgtMgr.LookupDomain(cfg.TargetDomain)
	if err != nil {
		// Almost certainly "no such domain", which is the desired end state.
		trace.Info("-force-clean: no target domain to remove", "vm", cfg.TargetDomain)
		return nil
	}
	defer dom.Free()

	active, err := libvirtsync.DomainActive(dom)
	if err != nil {
		return fmt.Errorf("determine whether %s is running: %w", cfg.TargetDomain, err)
	}
	if active {
		return fmt.Errorf("%s is RUNNING. -force-clean removes a target domain and replaces its disks, which underneath a live guest corrupts whatever it is writing -- shut it down first. This is the one refusal -force-clean does not override", cfg.TargetDomain)
	}

	// KEEP_NVRAM because the varstore belongs to the machine rather than to
	// this replica of it, and CHECKPOINTS_METADATA because libvirt refuses to
	// undefine an inactive domain carrying checkpoints without it -- which a
	// target acquires whenever it has previously been a SOURCE, i.e. exactly
	// the far end of an inverted pair. Same pair of flags, same reasons, as
	// DefineDomain's own undefine.
	if err := dom.UndefineFlags(libvirt.DOMAIN_UNDEFINE_KEEP_NVRAM | libvirt.DOMAIN_UNDEFINE_CHECKPOINTS_METADATA); err != nil {
		return fmt.Errorf("undefine target domain %s: %w", cfg.TargetDomain, err)
	}
	trace.Warning("-force-clean: removed the target domain definition; the sync below will define it again from the source",
		"vm", cfg.TargetDomain)
	return nil
}

// dropCheckpointChain removes every vmsync checkpoint from a domain, by
// whichever of the two mechanisms that domain's state actually permits.
//
// The distinction is not cosmetic. A checkpoint IS a persistent bitmap in the
// qcow2, and deleting one properly means merging its bitmap into the next --
// which only a live qemu can do. Against a shut-down domain libvirt simply
// refuses: "cannot delete checkpoint for inactive domain".
//
// Callers are -invert (the old source, always shut down) and -reinit /
// -force-clean (the source, usually running but not necessarily), which is
// why the choice is made here from the domain's state rather than assumed by
// each caller.
func dropCheckpointChain(dom *libvirt.Domain, domainName, uri, verb string) error {
	active, err := libvirtsync.DomainActive(dom)
	if err != nil {
		return fmt.Errorf("determine whether %s is running: %w", domainName, err)
	}
	if active {
		// Running: libvirt merges each bitmap into the next as it deletes,
		// which is the proper job and needs nothing from us.
		return libvirtsync.DeleteAllManagedCheckpoints(dom)
	}
	return dropCheckpointsOffline(dom, domainName, uri, verb)
}

// dropCheckpointsOffline is dropCheckpointChain's shut-down half: it removes
// the domain's vmsync checkpoints -- both libvirt's record of them AND the
// bitmaps they are made of.
//
// It is two halves that MUST both happen. Doing only the metadata half is what
// broke every sync for a pair once already: the images keep bitmaps named after
// checkpoints libvirt no longer knows about, the next sync restarts its chain
// at vmsync-cpt-000001, and qemu refuses with "Bitmap already exists" while
// checkpoint-list shows nothing. That is the invisible failure this function
// exists to prevent, so the two halves stay welded together here rather than
// being offered separately to callers.
//
// Bitmaps first, metadata second, deliberately. Interrupted after the bitmaps,
// libvirt still lists the checkpoints and a later delete tidies up -- visible
// and recoverable. Interrupted the other way round leaves the invisible
// breakage that has to be found with qemu-img.
func dropCheckpointsOffline(dom *libvirt.Domain, domainName, uri, verb string) error {
	// qemu-img runs HERE, on the files as this process sees them. That is
	// only true when the domain is on this host -- vmsync's SSH path goes to
	// the target, not the source, so there is no way to reach a remote
	// source's images. Refusing beats the alternative of dropping the
	// metadata and leaving the bitmaps, which is exactly the corruption this
	// function exists to avoid.
	if err := requireLocalURI(uri, "-source-uri"); err != nil {
		return fmt.Errorf("%s is shut down and has a checkpoint chain that must be discarded, and discarding it on a stopped domain means editing its disk images, which only works where they are: run %s on %s's own host with a local -source-uri (this is where -promote and -shutdown-domain already have to run). %w",
			domainName, verb, domainName, err)
	}

	// INACTIVE, matching every other metadata read: the domain is shut down
	// by definition here, so this is its persistent definition either way.
	domXML, err := dom.GetXMLDesc(libvirt.DOMAIN_XML_INACTIVE)
	if err != nil {
		return fmt.Errorf("read %s's definition: %w", domainName, err)
	}
	disks, err := disk.ParseQcowDisks(domXML)
	if err != nil {
		return fmt.Errorf("read %s's disks: %w", domainName, err)
	}

	// --- half one: the bitmaps ----------------------------------------------
	for _, d := range disks {
		path := d.Path()
		if path == "" {
			continue
		}
		chain, err := disk.QemuImgInfoChainJSON(path)
		if err != nil {
			return fmt.Errorf("inspect %s for checkpoint bitmaps: %w", path, err)
		}
		// chain[0] is the image itself; a backing file's own bitmaps are not
		// this domain's checkpoints and are not ours to remove.
		for _, name := range disk.BitmapNames(chain[0]) {
			if !libvirtsync.IsManagedCheckpointName(name) {
				// Somebody else's bitmap on the same image. Left alone: this
				// function knows what vmsync's checkpoints are named and has
				// no business guessing about anything else.
				trace.Warning("leaving a dirty bitmap that vmsync did not create", "disk", path, "bitmap", name)
				continue
			}
			if err := disk.RemoveBitmap(path, name); err != nil {
				return err
			}
			trace.Info("removed a checkpoint bitmap from an offline image", "disk", path, "bitmap", name)
		}
	}

	// --- half two: libvirt's record ------------------------------------------
	return libvirtsync.DeleteAllManagedCheckpointsMetadataOnly(dom)
}

func singleDiskDir(mgr *libvirtsync.Manager, domain string) (string, bool) {
	dom, err := mgr.LookupDomain(domain)
	if err != nil {
		return "", false
	}
	defer dom.Free()
	domXML, err := dom.GetXMLDesc(0)
	if err != nil {
		return "", false
	}
	disks, err := disk.ParseQcowDisks(domXML)
	if err != nil || len(disks) == 0 {
		return "", false
	}
	dir := ""
	for _, d := range disks {
		// Path(), not RootSource: ParseQcowDisks leaves RootSource empty --
		// only the sync path fills it in, after resolving each chain with
		// qemu-img. Reading it here gave path.Dir("") = "." for every disk on
		// both ends, so the two always compared equal and this function's
		// caller silently decided there was nothing to warn about. The
		// warning had never fired for anyone.
		//
		// path, not filepath: these are paths on a libvirt host.
		this := path.Dir(d.Path())
		if dir == "" {
			dir = this
			continue
		}
		if dir != this {
			return "", false
		}
	}
	if dir == "" {
		return "", false
	}
	return dir, true
}
