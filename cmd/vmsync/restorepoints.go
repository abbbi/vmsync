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
	"net/url"
	"os/exec"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"vmsync/pkg/remotessh"
	"vmsync/pkg/restorepoint"
	"vmsync/pkg/trace"
	"vmsync/pkg/util"
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
		// Left exactly where they are, not moved aside: they stay listed by
		// -list-restore-points, stay clonable, and stay under retention, so
		// they age out normally instead of becoming a pile nothing reaps.
		//
		// What does change is their cost. The replica this reinit is about to
		// rebuild shares no extents with them, so from now on they are charged
		// at their full independent size rather than as deltas against the
		// live replica.
		trace.Warning("reinit: keeping the existing restore points, because this reinit was forced by -reinit-after-failures rather than asked for -- repeated sync failures are exactly when an older copy is worth having. They stay listed and stay under retention, but they no longer share storage with the replica being rebuilt, so they now cost their full size",
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

// --- operator verbs ----------------------------------------------------------
//
// Both work off the filesystem alone and never touch libvirt, replication
// state or the replica. That is not a shortcut: the restore point inventory IS
// the directory (see docs/design/restore-points.md on why it cannot live in
// vmsync's metadata), and an operator running these during an incident should
// not be able to change anything by looking.

// localRunner runs a restore point command on THIS host.
//
// Restore points live beside the replica's disks, so every command here acts
// on the host holding them -- and when vmsync is already running on that host,
// reaching it means exec, not ssh. Without this, all three verbs refused a
// local -target-uri, which meant they could not be used on the one machine
// where the files actually are: an operator standing at the DR site, and the
// agent, which has only qemu:///system.
//
// Its Run must behave exactly as remotessh.Client.Run does, because every
// command builder in pkg/restorepoint is written against those semantics:
// stdout and stderr merged, whitespace trimmed, a non-zero exit reported as an
// error with the output still returned. sh -c rather than a parsed argv,
// because the builders emit shell -- pipes, redirections, if/fi.
type localRunner struct{}

func (localRunner) Run(ctx context.Context, command string) (string, error) {
	out, err := exec.CommandContext(ctx, "sh", "-c", command).CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return text, fmt.Errorf("run command %q: %w", command, err)
	}
	return text, nil
}

// uriRunsCommandsLocally reports whether a shell command for this libvirt URI
// would act on the host vmsync is running on.
//
// Deliberately NOT "is it not ssh". qemu+tcp://otherhost/system is neither ssh
// nor local, and treating it as local would run rm and mv against the wrong
// machine's filesystem -- silently, since the paths would very likely exist
// there too. Only a URI naming no host at all, or naming this one, qualifies.
//
// An ssh URI pointing at localhost stays on the ssh path on purpose: the
// operator named a user and a key, and quietly running as whoever invoked
// vmsync instead would change which account owns the files it creates.
func uriRunsCommandsLocally(raw string) bool {
	if util.UriUsesSSH(raw) {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	switch u.Hostname() {
	case "", "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// targetRunnerForRestorePoints returns something that can run commands where
// the restore points are, plus the cleanup for it.
//
// The cleanup is always non-nil, so callers defer it unconditionally.
func targetRunnerForRestorePoints(cfg syncConfig) (remoteRunner, func(), error) {
	if uriRunsCommandsLocally(cfg.TargetURI) {
		trace.Debug("restore points: reaching the target filesystem locally", "uri", cfg.TargetURI)
		return localRunner{}, func() {}, nil
	}
	if !util.UriUsesSSH(cfg.TargetURI) {
		// qemu+tcp:// and friends: a remote host with no way to run a command
		// on it. Named explicitly rather than folded into a generic refusal,
		// because the fix is a different URI scheme and not a missing flag.
		return nil, func() {}, fmt.Errorf("-target-uri %q names a remote host but not a way to run commands on it -- restore points live in the target's filesystem, so this needs either an ssh-based URI (qemu+ssh://) or vmsync running on the target itself with a local URI (qemu:///system)", cfg.TargetURI)
	}
	sshCfg, err := remotessh.ConfigFromLibvirtURI(
		cfg.TargetURI, cfg.SSHUser, cfg.SSHKey, cfg.SSHPassword,
		cfg.KnownHosts, cfg.SSHPort, cfg.SSHInsecure,
		time.Duration(cfg.SSHTimeoutSec)*time.Second,
	)
	if err != nil {
		return nil, func() {}, err
	}
	client, err := remotessh.Dial(sshCfg)
	if err != nil {
		return nil, func() {}, err
	}
	return client, func() { client.Close() }, nil
}

// restorePointRoot derives the restore point directory from -target-disk-path,
// for the two READ-ONLY verbs.
//
// Deliberately not by asking libvirt what disks the target has: these verbs
// must work when the target domain is gone, half-defined, or was never
// defined -- which is exactly the situation somebody reaching for a restore
// point may be in. They also never connect to libvirt at all, so they still
// answer when libvirtd itself is the thing that is broken.
//
// -restore-restore-point does derive it (restoreRootFor), and the asymmetry is
// deliberate: a restore refuses outright without the domain, so by the time it
// looks the domain has already said where its disks are. See that function for
// why deriving is the more correct answer rather than merely the convenient
// one.
func restorePointRoot(cfg syncConfig) (string, error) {
	if cfg.TargetDiskPath == "" {
		return "", fmt.Errorf("-target-disk-path is required to locate restore points; they live in %s inside it. (-restore-restore-point reads it off the target domain instead, but these verbs are built to work on a target whose domain is gone, so they cannot)", restorepoint.DirName)
	}
	return path.Join(cfg.TargetDiskPath, restorepoint.DirName), nil
}

// runListRestorePoints prints what is available to go back to.
func runListRestorePoints(ctx context.Context, cfg syncConfig) error {
	root, err := restorePointRoot(cfg)
	if err != nil {
		return err
	}
	client, closeRunner, err := targetRunnerForRestorePoints(cfg)
	if err != nil {
		return err
	}
	defer closeRunner()

	listing, err := listRestorePoints(ctx, client, root)
	if err != nil {
		return err
	}
	if len(listing.Points) == 0 {
		trace.Info("no restore points on the target", "dir", root)
	}

	// Oldest first: read top to bottom, this is the history in the order it
	// happened.
	points := append([]restorepoint.Tag(nil), listing.Points...)
	sort.Slice(points, func(i, j int) bool { return points[i].At.Before(points[j].At) })

	fmt.Printf("%-20s  %-20s  %-10s  %s\n", "TAKEN", "CHECKPOINT", "VERIFY", "TAG")
	for _, tag := range points {
		verify, checkpoint := "unknown", tag.Checkpoint
		out, err := client.Run(ctx, restorepoint.ReadStatusCommand(root, tag))
		if err == nil {
			if s, derr := restorepoint.DecodeStatus([]byte(out)); derr == nil {
				verify = s.Verify
				if s.Checkpoint != "" {
					checkpoint = s.Checkpoint
				}
			}
		}
		// "unknown" rather than a guess when the sidecar is missing or
		// unreadable: the whole reason it exists is to say how much
		// confidence a restore point has earned, and inventing one would
		// defeat it.
		fmt.Printf("%-20s  %-20s  %-10s  %s\n",
			tag.At.UTC().Format("2006-01-02 15:04:05"), checkpoint, verify, tag.String())
	}

	for _, name := range listing.Staging {
		trace.Warning("an incomplete restore point is present, left by an interrupted run; the next sync with -retention will remove it", "entry", name)
	}
	for _, name := range listing.Unknown {
		trace.Warning("an unrecognised entry is present in the restore point directory; vmsync will not touch it", "entry", name)
	}
	return nil
}

// runCloneRestorePoint materialises one restore point's disks somewhere the
// operator names, and stops there.
//
// This is the whole point of phase 1. During an incident the question is "is
// this copy clean", not "make the replica be this" -- and answering it by
// booting a scratch domain from a clone reconciles no metadata, changes no
// role, and leaves last_checkpoint valid. Restoring in place is a different
// operation with a different risk, and is deliberately not this one.
func runCloneRestorePoint(ctx context.Context, cfg syncConfig, tagName, dest string) error {
	if dest == "" {
		return fmt.Errorf("-clone-restore-point needs -clone-to DIR, the directory to write the copies into")
	}
	tag, err := restorepoint.ParseTag(tagName)
	if err != nil {
		return fmt.Errorf("%w -- run -list-restore-points to see the available tags", err)
	}
	root, err := restorePointRoot(cfg)
	if err != nil {
		return err
	}
	client, closeRunner, err := targetRunnerForRestorePoints(cfg)
	if err != nil {
		return err
	}
	defer closeRunner()

	// Confirm it is actually there before writing anything, so a mistyped tag
	// fails clean instead of leaving an empty destination behind.
	listing, err := listRestorePoints(ctx, client, root)
	if err != nil {
		return err
	}
	found := false
	for _, t := range listing.Points {
		if t.String() == tag.String() {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("no restore point %q in %s -- run -list-restore-points to see what is there", tag, root)
	}

	out, err := client.Run(ctx, restorepoint.ReadStatusCommand(root, tag))
	if err != nil {
		return fmt.Errorf("read the status sidecar of restore point %s: %w", tag, err)
	}
	status, err := restorepoint.DecodeStatus([]byte(out))
	if err != nil {
		return fmt.Errorf("restore point %s: %w", tag, err)
	}
	if len(status.Disks) == 0 {
		return fmt.Errorf("restore point %s lists no disks; it may have been written by a newer vmsync", tag)
	}

	if _, err := client.Run(ctx, "mkdir -p "+util.ShQuote(dest)); err != nil {
		return fmt.Errorf("create %s on the target: %w", dest, err)
	}
	for _, name := range status.Disks {
		to := path.Join(dest, name)
		if _, err := client.Run(ctx, restorepoint.CloneCommand(root, tag, name, to)); err != nil {
			return fmt.Errorf("clone %s from restore point %s: %w", name, tag, err)
		}
		trace.Info("cloned a restore point disk", "disk", name, "to", to)
	}

	trace.Info("restore point cloned; the replica and its replication state are untouched", "tag", tag.String(), "disks", len(status.Disks), "dir", dest, "verify", status.Verify)
	trace.Info("to inspect it, define a throwaway domain pointing at these files and boot it -- nothing here has changed the replica or its metadata")
	return nil
}
