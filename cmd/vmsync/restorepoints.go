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

	"vmsync/pkg/metrics"
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
	// store addresses ONE target domain's restore points. Not a directory:
	// keying this by the directory the disks live in is what let two co-located
	// domains starve, prune and sweep each other's history -- see the note at
	// the top of pkg/restorepoint/store.go.
	store  restorepoint.Store
	point  restorepoint.Point
	policy restorepoint.Policy

	// armed is false when retention is off, or when the interval has not
	// elapsed yet. Every method is a no-op then, so callers need no
	// conditionals of their own.
	armed bool

	// wantDisks is how many disks this run is copying, so commit can refuse to
	// publish a set that is short of one. take() fails the run if any single
	// reflink fails, so a short set can only come from an accounting error in
	// here -- which is worth failing on rather than publishing a restore point
	// that would silently restore a machine missing a disk.
	wantDisks int

	// stats is what this run will publish about the store. Filled at the
	// decision point and updated by commit and prune, so a run that never
	// takes a point still reports what is there and whether it was overdue.
	stats restorePointStats

	mu    sync.Mutex
	disks []string
}

// Outcomes reported by restorePointStats.Outcome, one of which is always true.
//
// Aliases of the metrics package's vocabulary rather than a second copy of it:
// these strings become Prometheus LABEL VALUES, and two spellings of "not_due"
// would file a pair's history under a series nobody alerts on. See
// metrics.RestorePointTaken for why the outcome is a label set and not a 0/1
// gauge.
const (
	rpOutcomeTaken        = metrics.RestorePointTaken
	rpOutcomeNotDue       = metrics.RestorePointNotDue
	rpOutcomeFailed       = metrics.RestorePointFailed
	rpOutcomeUndetermined = metrics.RestorePointUndetermined
)

// restorePointStats is what one run publishes about one domain's store.
//
// Separate from the metrics package's own types so that nothing on the
// retention path has to know how a Prometheus textfile is rendered, and so this
// can be asserted on in a test that needs no libvirt.
type restorePointStats struct {
	// Asked is whether -retention was set at all. Everything else is gated on
	// it: a run with no retention policy must publish no restore point series,
	// because a zero count would assert the store is empty when it was never
	// looked at.
	Asked bool
	// Listed is whether this run actually read the store. False when the run
	// was refused before it got there -- a filesystem without reflink support,
	// a domain with no disks -- and the counts below then mean nothing.
	Listed bool
	// Outcome is one of the rpOutcome* values above.
	Outcome string
	// Count, Over, Staging, Newest and Oldest describe the store AFTER this run
	// -- see restorepoint.Summary for what each one is.
	Count   int
	Over    int
	Staging int
	Newest  int64
	Oldest  int64
	// OverdueSeconds is how long ago another point became due and was not
	// produced, and it is zero on every healthy run: zero when the floor has
	// not elapsed, and zero once a point is published. It is deliberately NOT a
	// staleness threshold derived from the interval, because the interval is a
	// floor and not a cadence -- a healthy pair whose syncs are far apart has
	// points far apart, and any multiple-of-the-interval rule fires on it.
	OverdueSeconds float64
	// PolicyCount and PolicyInterval are what -retention asked for, published
	// beside the counts so that "is this replica as deep as it was meant to be"
	// needs no join against a configuration file on another host.
	PolicyCount    int
	PolicyInterval float64
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
func newRestorePoints(ctx context.Context, policy restorepoint.Policy, runner remoteRunner, targetDiskPaths []string, targetDomain, checkpoint string, at time.Time) (*restorePoints, error) {
	if !policy.Enabled() {
		return &restorePoints{}, nil
	}
	if len(targetDiskPaths) == 0 {
		return nil, fmt.Errorf("-retention is set but this domain has no disks to copy")
	}

	// A restore point is a SET: one disk from this sync beside another from
	// a different one is not a recoverable machine. That only works if they
	// share a directory, so refuse rather than scatter them.
	store, err := restorepoint.StoreFor(targetDiskPaths[0], targetDomain)
	if err != nil {
		return nil, fmt.Errorf("-retention: %w", err)
	}
	for _, p := range targetDiskPaths[1:] {
		other, err := restorepoint.StoreFor(p, targetDomain)
		if err != nil {
			return nil, fmt.Errorf("-retention: %w", err)
		}
		if other.ReplicaDir() != store.ReplicaDir() {
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

	existing, err := listRestorePoints(ctx, runner, store)
	if err != nil {
		return nil, fmt.Errorf("-retention: %w", err)
	}

	// Everything published about the store is decided here, including on the
	// paths that take no point: a run that reports nothing is indistinguishable
	// from a host that is not running, and a domain quietly taking none is the
	// exact failure the per-domain layout was introduced to end.
	stats := restorePointStats{
		Asked:          true,
		Listed:         true,
		Outcome:        rpOutcomeFailed,
		PolicyCount:    policy.Count,
		PolicyInterval: policy.Interval.Seconds(),
	}
	applyRestorePointSummary(&stats, restorepoint.Summarize(existing, policy))
	stats.OverdueSeconds = restorepoint.OverdueBy(restorepoint.Latest(existing.Points), at, policy).Seconds()

	if !restorepoint.Due(restorepoint.Latest(existing.Points), at, policy) {
		trace.Info("restore point not due yet", "kept", len(existing.Points), "interval", policy.Interval.String(), "dir", store.String())
		stats.Outcome = rpOutcomeNotDue
		return &restorePoints{store: store, policy: policy, stats: stats}, nil
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

	point := store.Point(tag)
	stage, err := restorepoint.StageCommand(point)
	if err != nil {
		return nil, fmt.Errorf("-retention: %w", err)
	}
	// mkdir -p, so this is where the domain's store and the shared root above
	// it come into being on a pair's first run with -retention.
	if _, err := runner.Run(ctx, stage); err != nil {
		return nil, fmt.Errorf("-retention: create the staging directory for restore point %s: %w", tag, err)
	}
	trace.Info("taking a restore point", "tag", tag.String(), "keep", policy.Count, "dir", store.String())
	return &restorePoints{
		runner:    runner,
		store:     store,
		point:     point,
		policy:    policy,
		armed:     true,
		wantDisks: len(targetDiskPaths),
		stats:     stats,
	}, nil
}

// applyRestorePointSummary copies a pure Summary onto the stats a run reports.
func applyRestorePointSummary(s *restorePointStats, sum restorepoint.Summary) {
	s.Count, s.Over, s.Staging = sum.Count, sum.Over, sum.Staging
	s.Newest, s.Oldest = 0, 0
	if !sum.Newest.IsZero() {
		s.Newest = sum.Newest.Unix()
	}
	if !sum.Oldest.IsZero() {
		s.Oldest = sum.Oldest.Unix()
	}
}

// snapshot is what the run publishes about restore points. Safe on a nil
// receiver, which is what a run refused before the retention decision holds.
func (r *restorePoints) snapshot() restorePointStats {
	if r == nil {
		return restorePointStats{Outcome: rpOutcomeUndetermined}
	}
	if r.stats.Outcome == "" {
		// Retention off: nothing was asked and nothing was looked at.
		return restorePointStats{Outcome: rpOutcomeUndetermined}
	}
	return r.stats
}

func listRestorePoints(ctx context.Context, runner remoteRunner, store restorepoint.Store) (restorepoint.Listing, error) {
	cmd, err := restorepoint.ListCommand(store)
	if err != nil {
		return restorepoint.Listing{}, err
	}
	out, err := runner.Run(ctx, cmd)
	if err != nil {
		return restorepoint.Listing{}, fmt.Errorf("list existing restore points in %s: %w", store, err)
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
	cmd, err := restorepoint.CopyCommand(r.point, diskPath)
	if err != nil {
		return fmt.Errorf("-retention: %w", err)
	}
	if _, err := r.runner.Run(ctx, cmd); err != nil {
		return fmt.Errorf("-retention: copy %s into restore point %s: %w", diskPath, r.point.Tag(), err)
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

	// A published set that is short of a disk would restore a machine missing
	// one of its volumes, and would look like any other restore point while
	// doing it. take() fails the run on any single reflink failure, so the only
	// way to be here with a short set is an accounting mistake in this file --
	// which is worth failing the run over rather than publishing.
	if len(disks) != r.wantDisks {
		return fmt.Errorf("-retention: refusing to publish restore point %s: it holds %d disk(s) but this sync copied %d -- a set missing a disk would restore an incomplete machine",
			r.point.Tag(), len(disks), r.wantDisks)
	}

	status := restorepoint.Status{
		Checkpoint:   effectiveCheckpoint,
		CheckpointAt: checkpointAt.Unix(),
		TakenAt:      time.Now().Unix(),
		Source:       source,
		Verify:       verifyState,
		Disks:        disks,
	}
	cmd, err := restorepoint.StatusCommand(r.point, status)
	if err != nil {
		return fmt.Errorf("-retention: %w", err)
	}
	if _, err := r.runner.Run(ctx, cmd); err != nil {
		return fmt.Errorf("-retention: write the status sidecar for restore point %s: %w", r.point.Tag(), err)
	}
	commit, err := restorepoint.CommitCommand(r.point)
	if err != nil {
		return fmt.Errorf("-retention: %w", err)
	}
	if _, err := r.runner.Run(ctx, commit); err != nil {
		return fmt.Errorf("-retention: publish restore point %s: %w", r.point.Tag(), err)
	}
	dir, _ := r.point.Dir()
	trace.Info("restore point taken", "tag", r.point.Tag().String(), "disks", len(disks), "verify", verifyState, "path", dir)

	// Published, so nothing is owed any more whatever the floor said on the way
	// in. Recorded before the prune, because a prune failure must not make a
	// run that DID take a point report as overdue.
	r.stats.Outcome = rpOutcomeTaken
	r.stats.OverdueSeconds = 0

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
	listing, err := listRestorePoints(ctx, r.runner, r.store)
	if err != nil {
		trace.Warning("could not list restore points to prune them; they will accumulate until this succeeds", "error", err)
		return
	}
	for _, name := range listing.Unknown {
		trace.Warning("ignoring an unrecognised entry in the restore point directory; vmsync will not delete something it cannot identify", "entry", name, "dir", r.store.String())
	}

	// Abandoned staging directories are junk from an interrupted run, and
	// are swept whatever the retention count says.
	//
	// Scoped to THIS domain's store, one of the several ways a shared store
	// goes wrong: a sweep over the directory every co-located domain stages
	// into removes a concurrently running sibling's in-flight set -- and the
	// rename that sibling then attempts fails, failing a sync whose data has
	// already landed, and counting toward -reinit-after-failures. Two runs of
	// the SAME domain cannot collide here because the target run lock is held
	// for the whole run (see util.AcquireRunLock and the target lock in
	// cmd/vmsync/main.go).
	for _, name := range listing.Staging {
		cmd, err := restorepoint.RemoveStagingCommand(r.store, name)
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

	// Over this domain's own points only. Pruning every point in the shared
	// directory applies a retention count meant for one machine to the sum of
	// several, and one domain's churn silently evicts another's history.
	plan := restorepoint.Prune(listing.Points, r.policy)
	for _, tag := range plan.Remove {
		cmd, err := restorepoint.RemoveCommand(r.store.Point(tag))
		if err != nil {
			trace.Warning("leaving a restore point in place", "tag", tag.String(), "error", err)
			continue
		}
		if _, err := r.runner.Run(ctx, cmd); err != nil {
			trace.Warning("could not remove an expired restore point; restore points will accumulate until this succeeds", "tag", tag.String(), "error", err)
			continue
		}
		trace.Info("removed an expired restore point", "tag", tag.String())
	}
	trace.Info("restore points on target", "kept", len(plan.Keep), "removed", len(plan.Remove), "dir", r.store.String())

	// Re-read rather than computed from the plan, so what is published is what
	// the store actually holds and not what the prune intended. A prune failure
	// is only a warning on the run itself -- Over staying positive is how it
	// becomes visible from outside.
	if after, err := listRestorePoints(ctx, r.runner, r.store); err == nil {
		applyRestorePointSummary(&r.stats, restorepoint.Summarize(after, r.policy))
	}
}

// sweepRestorePointsForReinit decides what a -reinit does to the restore
// points of the replica it is about to discard.
//
// THIS TARGET DOMAIN'S ONLY. Acting on the shared directory would mean that
// reinitialising one replica deletes or orphans the entire restore point
// history of every other domain replicating into the same -target-disk-path --
// in one rm -rf, while logging that it had removed "the restore points
// belonging to the replaced replica". A co-located domain's store is a sibling
// this function cannot name: restorepoint.RemoveStoreCommand takes a Store,
// and a Store cannot be built without a domain.
//
// Anything else in the shared directory is likewise left alone, including any
// restore points sitting flat in it rather than under a domain's store: this
// addresses one store, so there is no expression here that could name them.
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
	store, err := restorepoint.StoreFor(aTargetDiskPath, cfg.TargetDomain)
	if err != nil {
		// Not fatal, for the same reason a failed listing is not: refusing a
		// reinit over restore point bookkeeping would block a recovery.
		trace.Warning("reinit: could not locate this domain's restore points; leaving them in place", "error", err)
		return nil
	}
	root := store.String()

	listing, err := listRestorePoints(ctx, runner, store)
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
		//
		// The cause is named rather than assumed: two different things force
		// an automatic reinit now, and an operator reading this line is being
		// told why their history survived a reinit, so it has to be true.
		// VerifyRepairAttempt and not VerifyFailureReinit, because the latter
		// is set on every run of a self-repairing pair -- including one whose
		// reinit was forced by the failure counter instead.
		why := "this reinit was forced by -reinit-after-failures rather than asked for -- repeated sync failures are exactly when an older copy is worth having"
		if cfg.VerifyRepairAttempt {
			why = "this reinit is -verify-failure-reinit repairing a replica that FAILED verification -- these copies predate the finding, so they are the only candidates for a clean one and discarding them here would destroy the very thing the finding calls for"
		}
		trace.Warning("reinit: keeping the existing restore points, because "+why+". They stay listed and stay under retention, but they no longer share storage with the replica being rebuilt, so they now cost their full size",
			"kept", len(listing.Points), "dir", root)
		return nil
	}

	switch cfg.ReplacedDiskAction {
	case replacedDiskDelete:
		cmd, err := restorepoint.RemoveStoreCommand(store)
		if err != nil {
			return fmt.Errorf("reinit: %w", err)
		}
		if _, err := runner.Run(ctx, cmd); err != nil {
			return fmt.Errorf("reinit: remove restore points in %s: %w", root, err)
		}
		trace.Info("reinit: removed the restore points belonging to the replaced replica. Only this target domain's: any other domain replicating into the same directory keeps its own", "removed", len(listing.Points), "domain", cfg.TargetDomain, "dir", root)
	default:
		cmd, aside, err := restorepoint.RenameStoreCommand(store, time.Now())
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

// restorePointStore derives this target domain's restore point store from
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
// It needs -target-domain as well: restore points are kept per target domain,
// so a directory alone does not name a set. It cannot be defaulted from
// -source-domain the way a sync's can, because these verbs dispatch before
// that defaulting happens -- and defaulting it would be worse than refusing
// anyway, since the domain given here has to be the same string the sync used
// or the history reads as empty.
func restorePointStore(cfg syncConfig) (restorepoint.Store, error) {
	if cfg.TargetDiskPath == "" {
		return restorepoint.Store{}, fmt.Errorf("-target-disk-path is required to locate restore points; they live in %s inside it. (-restore-restore-point reads it off the target domain instead, but these verbs are built to work on a target whose domain is gone, so they cannot)", restorepoint.DirName)
	}
	return restorepoint.StoreForDir(cfg.TargetDiskPath, cfg.TargetDomain)
}

// runListRestorePoints prints what is available to go back to.
func runListRestorePoints(ctx context.Context, cfg syncConfig) error {
	store, err := restorePointStore(cfg)
	if err != nil {
		return err
	}
	client, closeRunner, err := targetRunnerForRestorePoints(cfg)
	if err != nil {
		return err
	}
	defer closeRunner()

	listing, err := listRestorePoints(ctx, client, store)
	if err != nil {
		return err
	}
	if len(listing.Points) == 0 {
		trace.Info("no restore points on the target for this domain", "domain", cfg.TargetDomain, "dir", store.String())
	}

	// Oldest first: read top to bottom, this is the history in the order it
	// happened.
	points := append([]restorepoint.Tag(nil), listing.Points...)
	sort.Slice(points, func(i, j int) bool { return points[i].At.Before(points[j].At) })

	fmt.Printf("%-20s  %-20s  %-10s  %s\n", "TAKEN", "CHECKPOINT", "VERIFY", "TAG")
	for _, tag := range points {
		verify, checkpoint := "unknown", tag.Checkpoint
		if cmd, cerr := restorepoint.ReadStatusCommand(store.Point(tag)); cerr == nil {
			out, rerr := client.Run(ctx, cmd)
			if rerr == nil {
				if s, derr := restorepoint.DecodeStatus([]byte(out)); derr == nil {
					verify = s.Verify
					if s.Checkpoint != "" {
						checkpoint = s.Checkpoint
					}
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

// findRestorePoint resolves a tag to a point in this target domain's store, and
// confirms it is there before anything is written.
//
// Separate from just building the Point so that a mistyped tag fails naming the
// tag, rather than failing later on a missing file -- and so a clone leaves no
// half-written destination behind.
func findRestorePoint(ctx context.Context, client remoteRunner, store restorepoint.Store, tag restorepoint.Tag) (restorepoint.Point, error) {
	listing, err := listRestorePoints(ctx, client, store)
	if err != nil {
		return restorepoint.Point{}, err
	}
	for _, t := range listing.Points {
		if t.String() == tag.String() {
			return store.Point(t), nil
		}
	}
	return restorepoint.Point{}, fmt.Errorf("no restore point %q in %s -- run -list-restore-points to see what is there", tag, store)
}

// runCloneRestorePoint materialises one restore point's disks somewhere the
// operator names, and stops there.
//
// This is the whole point of phase 1. During an incident the question is "is
// this copy clean", not "make the replica be this" -- and answering it by
// booting a scratch domain from a clone reconciles no metadata, changes no
// role, and leaves last_checkpoint valid. Restoring in place is a different
// operation with a different risk, and is deliberately not this one.
// The return is named so the action journal's outcome is written on every path
// out. A clone changes nothing about the replica, but it does put a full copy
// of it somewhere an operator chose, and "a second copy of this machine exists
// at /scratch" is a fact worth being able to find later.
func runCloneRestorePoint(ctx context.Context, cfg syncConfig, tagName, dest string) (runErr error) {
	if dest == "" {
		return fmt.Errorf("-clone-restore-point needs -clone-to DIR, the directory to write the copies into")
	}
	tag, err := restorepoint.ParseTag(tagName)
	if err != nil {
		return fmt.Errorf("%w -- run -list-restore-points to see the available tags", err)
	}
	store, err := restorePointStore(cfg)
	if err != nil {
		return err
	}
	client, closeRunner, err := targetRunnerForRestorePoints(cfg)
	if err != nil {
		return err
	}
	defer closeRunner()

	// Beside the replica's disks rather than beside the clone: the journal
	// belongs with the machine this copy was taken FROM, which is what anybody
	// later asking "where did that second copy come from" is standing in front
	// of. cfg.TargetDiskPath is required by this verb (restorePointStore refuses
	// without it), so the directory is known.
	journal := newRecorderInDir(cfg, client, cfg.TargetDiskPath, journalDomainName(cfg))
	journal.Intent(ctx, journalVerbCloneRestorePoint, map[string]string{
		"tag":  tag.String(),
		"dest": dest,
	})
	defer func() { finishAction(ctx, journal, runErr, nil) }()

	// Confirm it is actually there before writing anything, so a mistyped tag
	// fails clean instead of leaving an empty destination behind.
	point, err := findRestorePoint(ctx, client, store, tag)
	if err != nil {
		return err
	}

	statusCmd, err := restorepoint.ReadStatusCommand(point)
	if err != nil {
		return err
	}
	out, err := client.Run(ctx, statusCmd)
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

	// Same ownership treatment as a sync's target directory: -clone-restore-point
	// produces disks somebody may well go on to boot, and a chain of
	// root-owned directories above them is the same trap. Directories that
	// already exist are left exactly as they are.
	if _, err := client.Run(ctx, util.MkdirOwnedCommand(targetDirOwner(ctx, client, cfg), dest)); err != nil {
		return fmt.Errorf("create %s on the target: %w", dest, err)
	}
	for _, name := range status.Disks {
		// A bare name, checked before it is joined onto either end. These come
		// out of a sidecar on the target -- a file this run did not write and
		// cannot vouch for -- and both ends of the cp are built from them, so a
		// sidecar listing "../../etc/passwd" would read outside the restore
		// point and write outside -clone-to. The restore path applies the same
		// check in RestoreStageCommand; this one had nothing.
		if name == "" || name == "." || name == ".." || name != path.Base(name) {
			return fmt.Errorf("restore point %s lists %q as one of its disks, which is not a bare file name -- refusing to copy it", tag, name)
		}
		to := path.Join(dest, name)
		clone, err := restorepoint.CloneCommand(point, name, to)
		if err != nil {
			return err
		}
		if _, err := client.Run(ctx, clone); err != nil {
			return fmt.Errorf("clone %s from restore point %s: %w", name, tag, err)
		}
		trace.Info("cloned a restore point disk", "disk", name, "to", to)
	}

	// "from" names the store the copy came out of, so an operator comparing a
	// clone against a replica later has a way back to it.

	trace.Info("restore point cloned; the replica and its replication state are untouched", "tag", tag.String(), "from", store.String(), "disks", len(status.Disks), "dir", dest, "verify", status.Verify)
	trace.Info("to inspect it, define a throwaway domain pointing at these files and boot it -- nothing here has changed the replica or its metadata")
	return nil
}
