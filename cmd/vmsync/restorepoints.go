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
	"sync"
	"time"

	"vmsync/pkg/restorepoint"
	"vmsync/pkg/trace"
)

// The -retention side of a sync: after each replica disk is copied, take a
// reflink copy of it, and once the whole set is there, publish it and prune
// what is now beyond the retention count.
//
// The decisions and every command live in pkg/restorepoint, which has no
// libvirt dependency and is exhaustively tested. What is here is only the
// wiring: when to ask, and what to do with the answer.
//
// See docs/design/restore-points.md.

// remoteRunner is the one-method seam util.RemotePathExists already uses, so
// this needs no concrete SSH type.
type remoteRunner interface {
	Run(ctx context.Context, cmd string) (string, error)
}

type restorePoints struct {
	runner remoteRunner
	root   string
	tag    restorepoint.Tag
	policy restorepoint.Policy

	// armed is false when retention is off, or when the interval has not
	// elapsed yet. Every method is a no-op then, so callers need no
	// conditionals of their own.
	armed bool

	mu    sync.Mutex
	disks []string
}

// newRestorePoints decides whether this run takes a restore point, and refuses
// the run outright if retention was asked for and cannot be delivered.
//
// Refusing rather than warning is deliberate: -retention is a promise about
// what will exist tomorrow, and an operator who set it should learn at startup
// that they are not getting it, not discover months later that a filesystem
// without reflink support meant no restore point was ever taken.
//
// A nil return with no error means "not this run" -- retention is off, or the
// interval has not elapsed -- and every method below then does nothing.
func newRestorePoints(ctx context.Context, policy restorepoint.Policy, runner remoteRunner, targetDiskPaths []string, checkpoint string, at time.Time) (*restorePoints, error) {
	if !policy.Enabled() {
		return &restorePoints{}, nil
	}
	if len(targetDiskPaths) == 0 {
		return nil, fmt.Errorf("-retention is set but this domain has no disks to copy")
	}

	// A restore point is a SET: one disk from this sync beside another from
	// a different one is not a recoverable machine. That only works if they
	// share a directory, so refuse rather than scatter them.
	root := restorepoint.Root(targetDiskPaths[0])
	for _, p := range targetDiskPaths[1:] {
		if other := restorepoint.Root(p); other != root {
			return nil, fmt.Errorf("-retention needs every target disk in one directory so a restore point is a single consistent set, but %s and %s are in different ones; set -target-disk-path",
				targetDiskPaths[0], p)
		}
	}

	// Probe the directory the disks already live in, not the restore point
	// directory: asking a question should not create anything, and the two
	// are the same filesystem, which is all that is being measured.
	out, err := runner.Run(ctx, restorepoint.ProbeCommand(path.Dir(targetDiskPaths[0])))
	if err != nil {
		return nil, fmt.Errorf("-retention: could not test whether the target filesystem supports reflink copies: %w", err)
	}
	ok, err := restorepoint.ParseProbe(out)
	if err != nil {
		return nil, fmt.Errorf("-retention: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("-retention=%s was requested, but %s on the target does not support reflink copies -- restore points would each be a full copy of the replica instead of sharing its storage. Use XFS with reflink=1 (the default on RHEL 8+) or btrfs, or remove -retention",
			policy.String(), path.Dir(targetDiskPaths[0]))
	}

	existing, err := listRestorePoints(ctx, runner, root)
	if err != nil {
		return nil, fmt.Errorf("-retention: %w", err)
	}
	if !restorepoint.Due(restorepoint.Latest(existing.Points), at, policy) {
		trace.Info("restore point not due yet", "kept", len(existing.Points), "interval", policy.Interval.String(), "dir", root)
		return &restorePoints{}, nil
	}

	// The tag names the checkpoint this run is creating. In the rare case
	// where checkpoint creation was blocked by an external snapshot on the
	// source and the chain does not advance, the sidecar records the
	// checkpoint actually in force -- the directory name is the instant,
	// which is what identifies a restore point either way.
	tag, err := restorepoint.NewTag(at, checkpoint)
	if err != nil {
		return nil, fmt.Errorf("-retention: %w", err)
	}

	if _, err := runner.Run(ctx, restorepoint.StageCommand(root, tag)); err != nil {
		return nil, fmt.Errorf("-retention: create the staging directory for restore point %s: %w", tag, err)
	}
	trace.Info("taking a restore point", "tag", tag.String(), "keep", policy.Count, "dir", root)
	return &restorePoints{runner: runner, root: root, tag: tag, policy: policy, armed: true}, nil
}

func listRestorePoints(ctx context.Context, runner remoteRunner, root string) (restorepoint.Listing, error) {
	out, err := runner.Run(ctx, restorepoint.ListCommand(root))
	if err != nil {
		return restorepoint.Listing{}, fmt.Errorf("list existing restore points in %s: %w", root, err)
	}
	return restorepoint.ParseListing(out)
}

// take copies one replica disk into the staging restore point.
//
// Called right after that disk's own copy and commit, and deliberately before
// -verify: the reflink costs milliseconds whatever the image size, and making
// it wait for a compare that can take many minutes would mean a crash in
// between loses the restore point for no benefit. What verify found is
// recorded on the sidecar instead.
func (r *restorePoints) take(ctx context.Context, diskPath string) error {
	if r == nil || !r.armed {
		return nil
	}
	if _, err := r.runner.Run(ctx, restorepoint.CopyCommand(r.root, r.tag, diskPath)); err != nil {
		return fmt.Errorf("-retention: copy %s into restore point %s: %w", diskPath, r.tag, err)
	}
	r.mu.Lock()
	r.disks = append(r.disks, path.Base(diskPath))
	r.mu.Unlock()
	return nil
}

// commit publishes the staged restore point and prunes what is now surplus.
//
// The publish is a rename, which is atomic within a filesystem: until it runs,
// the set is under a name starting with ".incomplete-" and is self-evidently
// junk. Pruning happens after, never before, so a run interrupted here leaves
// one restore point too many rather than one too few.
func (r *restorePoints) commit(ctx context.Context, verifyState, source string, checkpointAt time.Time, effectiveCheckpoint string) error {
	if r == nil || !r.armed {
		return nil
	}

	r.mu.Lock()
	disks := append([]string(nil), r.disks...)
	r.mu.Unlock()

	status := restorepoint.Status{
		Checkpoint:   effectiveCheckpoint,
		CheckpointAt: checkpointAt.Unix(),
		TakenAt:      time.Now().Unix(),
		Source:       source,
		Verify:       verifyState,
		Disks:        disks,
	}
	cmd, err := restorepoint.StatusCommand(r.root, r.tag, status)
	if err != nil {
		return fmt.Errorf("-retention: %w", err)
	}
	if _, err := r.runner.Run(ctx, cmd); err != nil {
		return fmt.Errorf("-retention: write the status sidecar for restore point %s: %w", r.tag, err)
	}
	if _, err := r.runner.Run(ctx, restorepoint.CommitCommand(r.root, r.tag)); err != nil {
		return fmt.Errorf("-retention: publish restore point %s: %w", r.tag, err)
	}
	trace.Info("restore point taken", "tag", r.tag.String(), "disks", len(disks), "verify", verifyState, "path", restorepoint.Dir(r.root, r.tag))

	r.prune(ctx)
	return nil
}

// prune deletes restore points beyond the retention count, oldest first.
//
// Failures here are warnings, not errors. The replica is synced and the new
// restore point is published by the time this runs; too many restore points
// costs disk, while failing the run over it would throw away a sync that
// succeeded. What it must not do is stay silent -- a prune that keeps failing
// is how a target fills up.
func (r *restorePoints) prune(ctx context.Context) {
	listing, err := listRestorePoints(ctx, r.runner, r.root)
	if err != nil {
		trace.Warning("could not list restore points to prune them; they will accumulate until this succeeds", "error", err)
		return
	}
	for _, name := range listing.Unknown {
		trace.Warning("ignoring an unrecognised entry in the restore point directory; vmsync will not delete something it cannot identify", "entry", name, "dir", r.root)
	}

	// Abandoned staging directories are junk from an interrupted run, and
	// are swept whatever the retention count says.
	for _, name := range listing.Staging {
		cmd, err := restorepoint.RemoveStagingCommand(r.root, name)
		if err != nil {
			trace.Warning("leaving an unrecognised staging directory in place", "entry", name, "error", err)
			continue
		}
		if _, err := r.runner.Run(ctx, cmd); err != nil {
			trace.Warning("could not remove an abandoned staging directory", "entry", name, "error", err)
			continue
		}
		trace.Info("removed an abandoned restore point staging directory left by an interrupted run", "entry", name)
	}

	plan := restorepoint.Prune(listing.Points, r.policy)
	for _, tag := range plan.Remove {
		if _, err := r.runner.Run(ctx, restorepoint.RemoveCommand(r.root, tag)); err != nil {
			trace.Warning("could not remove an expired restore point; restore points will accumulate until this succeeds", "tag", tag.String(), "error", err)
			continue
		}
		trace.Info("removed an expired restore point", "tag", tag.String())
	}
	trace.Info("restore points on target", "kept", len(plan.Keep), "removed", len(plan.Remove), "dir", r.root)
}

// sweepRestorePointsForReinit decides what a -reinit does to the restore
// points of the replica it is about to discard.
//
// An operator-initiated reinit takes them with it, following
// -replaced-disk-action exactly as the replica disks do: one knob, and the
// same answer for the replica and for its history, because they describe the
// same lineage and the reinit is discarding it deliberately.
//
// An AUTOMATIC reinit does not. -reinit-after-failures fires on a failure
// count, and "syncs have been failing repeatedly" is uncomfortably close to
// "something is wrong with the source" -- which is the exact scenario restore
// points exist for. Silently discarding them at that moment is the one
// behaviour that would make this feature worse than not having it. So they are
// kept, loudly: refusing the reinit instead would turn an auto-heal into stuck
// replication, which is worse again.
func sweepRestorePointsForReinit(ctx context.Context, cfg syncConfig, runner remoteRunner, aTargetDiskPath string) error {
	root := restorepoint.Root(aTargetDiskPath)

	listing, err := listRestorePoints(ctx, runner, root)
	if err != nil {
		// Not fatal: failing a reinit because the restore point directory
		// could not be listed would block recovery over bookkeeping.
		trace.Warning("reinit: could not list restore points; leaving them in place", "dir", root, "error", err)
		return nil
	}
	if len(listing.Points) == 0 && len(listing.Staging) == 0 {
		return nil
	}

	if cfg.ReinitAutomatic {
		trace.Warning("reinit: keeping the existing restore points, because this reinit was forced by -reinit-after-failures rather than asked for -- repeated sync failures are exactly when an older copy is worth having. They are no longer pruned by retention, since the replica they belong to is being rebuilt; remove them by hand once you are satisfied the new replica is good",
			"kept", len(listing.Points), "dir", root)
		return nil
	}

	switch cfg.ReplacedDiskAction {
	case replacedDiskDelete:
		cmd, err := restorepoint.RemoveRootCommand(root)
		if err != nil {
			return fmt.Errorf("reinit: %w", err)
		}
		if _, err := runner.Run(ctx, cmd); err != nil {
			return fmt.Errorf("reinit: remove restore points in %s: %w", root, err)
		}
		trace.Info("reinit: removed the restore points belonging to the replaced replica", "removed", len(listing.Points), "dir", root)
	default:
		cmd, aside, err := restorepoint.RenameRootCommand(root, time.Now())
		if err != nil {
			return fmt.Errorf("reinit: %w", err)
		}
		if _, err := runner.Run(ctx, cmd); err != nil {
			return fmt.Errorf("reinit: move restore points aside from %s: %w", root, err)
		}
		// Louder than the equivalent line for a single replaced disk, because
		// the cost is different in kind. These copies share extents among
		// themselves, so the set is about one base image plus its deltas --
		// but the replica this reinit is about to build shares nothing with
		// them, so the target now holds a second full base image until
		// somebody removes it.
		trace.Warning("reinit: moved the existing restore points aside rather than deleting them (-replaced-disk-action=rename). Nothing reaps these: the set costs roughly a second full copy of the replica for as long as it stays. Remove it by hand, or use -replaced-disk-action=delete",
			"moved", len(listing.Points), "from", root, "to", aside)
	}
	return nil
}
