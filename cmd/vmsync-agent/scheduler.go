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
	"bytes"
	"context"
	"fmt"
	"hash/fnv"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"vmsync/pkg/inventory"
	"vmsync/pkg/libvirtsync"
	"vmsync/pkg/trace"
	"vmsync/pkg/util"
)

// SyncResult is the outcome of one scheduled run, reported to the UI so an
// operator can see what happened without reading a journal on the host.
type SyncResult struct {
	VM             string `json:"vm"`
	TargetHost     string `json:"target_host,omitempty"`
	StartedAtUnix  int64  `json:"started_at_unix"`
	FinishedAtUnix int64  `json:"finished_at_unix"`
	DurationSecs   int64  `json:"duration_seconds"`
	ExitCode       int    `json:"exit_code"`
	Error          string `json:"error,omitempty"`
	// LogTail is the last few lines of vmsync's own output. Bounded on
	// purpose: enough to see why a run failed, not so much that a chatty
	// failure loop fills the UI's disk.
	LogTail string `json:"log_tail,omitempty"`
}

const (
	// defaultMaxConcurrent is what the agent runs with when nothing has said
	// otherwise. vmsync is heavy -- NBD, compression, disk and network at
	// once -- so this stays well below what a modern hypervisor could
	// manage: a default that saturates the host is one every operator has to
	// notice and turn down, whereas one that is merely conservative costs
	// only a longer cycle on hosts nobody has tuned.
	defaultMaxConcurrent = 4
	// hardMaxConcurrent clamps whatever is asked for, from any source. It is
	// a backstop against a nonsense value -- a mistyped 5000, or a UI of a
	// different vintage -- not a recommendation: a host that genuinely wants
	// this many parallel syncs is unusual, and the number it should actually
	// run is a property of its disks and NICs, set deliberately with
	// -max-concurrent-syncs rather than arrived at by hitting this ceiling.
	hardMaxConcurrent = 128
	// logTailBytes bounds what is kept from a run's output.
	logTailBytes = 4000
	// resultsKept is how many recent outcomes ride along in reports.
	resultsKept = 50
	// tickInterval is how often the scheduler re-examines what is due.
	// Nothing here needs second-level precision; a replication cadence is
	// minutes at best.
	tickInterval = 10 * time.Second
)

// Scheduler runs the schedule the UI distributes.
//
// It keeps running from the cached schedule when the UI is unreachable,
// which is the control plane's central invariant: the UI lives at the DR
// site and a partition is an ordinary event, so replication must not stop
// just because nobody can currently change it.
type Scheduler struct {
	// lv, not a captured agentConfig value. A copy taken at construction can
	// never observe a reload, and the scheduler is the component whose
	// settings an operator is most likely to change while it runs.
	lv    *live
	state *sharedState

	mu       sync.Mutex
	nextRun  map[string]time.Time
	inFlight map[string]bool // by VM
	hostLoad map[string]int  // concurrent syncs INTO each target host
	metrics  *agentMetrics   // nil is safe: every call is nil-guarded
	results  []SyncResult
}

func NewScheduler(lv *live, state *sharedState) *Scheduler {
	return &Scheduler{
		lv:       lv,
		state:    state,
		nextRun:  map[string]time.Time{},
		inFlight: map[string]bool{},
		hostLoad: map[string]int{},
		metrics:  lv.get().metrics,
	}
}

// Results returns the recent outcomes for inclusion in a report.
func (s *Scheduler) Results() []SyncResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SyncResult, len(s.results))
	copy(out, s.results)
	return out
}

func (s *Scheduler) record(r SyncResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.results = append(s.results, r)
	if len(s.results) > resultsKept {
		s.results = s.results[len(s.results)-resultsKept:]
	}
}

// Run is the scheduler loop. It returns when ctx is cancelled, after
// waiting for any sync still in flight to finish unwinding.
func (s *Scheduler) Run(ctx context.Context) {
	var wg sync.WaitGroup
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	for {
		s.launchDue(ctx, &wg)
		select {
		case <-ctx.Done():
			// vmsync's own SIGTERM handling unwinds a run cleanly -- it
			// stops the backup job, deletes the verify-window checkpoint,
			// tears down exports and resumes a suspended source. systemd
			// will have signalled the whole control group, so the children
			// are already unwinding; this just waits for them.
			wg.Wait()
			return
		case <-ticker.C:
		}
	}
}

// launchDue starts every entry that is due and admissible right now.
func (s *Scheduler) launchDue(ctx context.Context, wg *sync.WaitGroup) {
	// ONE configuration snapshot per tick, threaded through everything this
	// tick does and captured into each launch. A second get() further down
	// would give generation N's argv and generation N+1's binary -- and a
	// sync that started under one ssh_key must finish under it.
	cfg := s.lv.get()
	cached := s.state.get()
	now := time.Now()

	for _, entry := range cached.Config.Schedule {
		if !entry.Enabled || entry.IntervalSeconds <= 0 || entry.VM == "" {
			continue
		}
		// Dueness first, then busy-ness. The order matters for the metric:
		// asking "is it running" every tick would count one long sync as
		// hundreds of missed schedules, when what is worth counting is a
		// slot its interval said it should have had.
		if !s.due(entry, now) {
			continue
		}
		if s.isRunning(entry.VM) {
			// Its previous run has not finished. Counted separately from a
			// concurrency refusal: this one means the interval is shorter
			// than the sync takes, which no amount of extra capacity fixes.
			s.metrics.skip(skipAlreadyRunning)
			continue
		}
		// ...and the same question asked of a run THIS agent did not start.
		//
		// inFlight is memory-only, so a new process after a restart, a crash
		// or a package upgrade knows nothing about a vmsync the previous one
		// launched and which is still going. Without this it launches a second
		// one every interval for as long as the first runs. That is now
		// harmless rather than catastrophic -- the engine stands down with
		// ExitBusy instead of exiting 0 and being recorded as a success -- but
		// harmless is not free: it spawns a process, takes a concurrency slot
		// and writes a run-log pair, every interval, for hours.
		//
		// Consulted, never obeyed: a foreign run that cannot be confirmed
		// reads as "not held", the launch goes ahead, and the engine's own
		// lock makes the real decision. See util.RunLockHeld on why this fails
		// open.
		if held, reason := s.foreignRunHolds(cfg, entry.VM); held {
			trace.Info("skipping a scheduled sync: a vmsync started outside this agent is still working on this domain",
				"vm", entry.VM, "reason", reason)
			s.metrics.skip(skipForeignRun)
			continue
		}
		if err := entry.Profile.Validate(); err != nil {
			// Loud and skipped, not fatal: one bad entry must not stop the
			// rest of the schedule, and the message names the field so it
			// can be fixed in the UI.
			trace.Error("refusing to run a scheduled sync with an invalid profile", "vm", entry.VM, "error", err)
			s.metrics.skip(skipInvalidProfile)
			s.deferEntry(entry, now)
			continue
		}

		req, err := s.buildRequest(cfg, entry)
		if err != nil {
			trace.Error("could not prepare a scheduled sync", "vm", entry.VM, "error", err)
			s.metrics.skip(skipNoTarget)
			s.deferEntry(entry, now)
			continue
		}
		if !s.admit(cfg, entry.VM, req.targetHost, cached.Config) {
			// Not deferred: leaving nextRun in the past means this entry is
			// retried on the next tick, as soon as a slot frees up.
			continue
		}

		s.markRunning(entry, now)
		wg.Add(1)
		go func(entry ScheduleEntry, req syncPlan) {
			defer wg.Done()
			defer s.release(entry.VM, req.targetHost)
			s.runOne(ctx, cfg, entry, req)
		}(entry, req)
	}
}

// due reports whether an entry should run now.
//
// A first-ever entry is scheduled at now plus a per-VM offset rather than
// immediately: on agent start every entry would otherwise be due at once,
// and while the concurrency limit stops that from overwhelming the host, it
// would still bunch every VM's sync into the same minute forever after. The
// offset is derived from the VM name, so it is stable across restarts and a
// given VM keeps its slot.
// Deliberately says nothing about whether the VM is currently syncing:
// launchDue checks that separately, immediately after, so a busy VM can be
// counted as a missed slot rather than silently conflated with one that is
// simply not due yet.
func (s *Scheduler) due(entry ScheduleEntry, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	next, known := s.nextRun[entry.VM]
	if !known {
		interval := time.Duration(entry.IntervalSeconds) * time.Second
		s.nextRun[entry.VM] = now.Add(stagger(entry.VM, interval))
		return false
	}
	return !now.Before(next)
}

// foreignRunHolds reports whether a vmsync THIS agent did not start is still
// syncing vm.
//
// Reads the identity the engine stamps into its own run lock. It never opens
// that file for writing and never flocks it, because acquiring the lock is the
// only way to test a flock and acquiring it would produce exactly the
// contention it is looking for.
//
// Every uncertain answer is false. A missing lock file, an empty one (which is
// what every vmsync before this feature left behind), an unparseable one, an
// unreadable /proc: all mean "launch, and let the engine decide". The reason
// string is for the log and is never a reason to refuse.
func (s *Scheduler) foreignRunHolds(cfg *agentConfig, vm string) (bool, string) {
	id, ok, err := util.ReadRunLockIdentity(util.RunLockDir, vm)
	if err != nil {
		// Worth saying out loud -- somebody has put something else in this
		// path -- but not worth deferring a sync over.
		trace.Debug("could not read the run lock identity; proceeding as if nothing holds it", "vm", vm, "error", err)
		return false, ""
	}
	if !ok {
		return false, ""
	}
	return util.RunLockHeld(id, cfg.VmsyncPath)
}

// isRunning reports whether this VM's previous run is still going.
func (s *Scheduler) isRunning(vm string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inFlight[vm]
}

// metricsSnapshot returns what the metrics writer needs, taken under one
// lock so the running count and the due times cannot disagree.
func (s *Scheduler) metricsSnapshot() (running int, nextRun map[string]int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	nextRun = make(map[string]int64, len(s.nextRun))
	for vm, t := range s.nextRun {
		nextRun[vm] = t.Unix()
	}
	return len(s.inFlight), nextRun
}

// stagger spreads first runs across the interval, deterministically per VM.
//
// A 64-bit hash, not 32-bit, and that is the whole point rather than a detail.
// interval is a time.Duration -- NANOSECONDS -- so an hour is 3.6e12 and a day
// is 8.64e13, while a 32-bit hash cannot exceed 4294967295, which is 4.29
// SECONDS. Taking that modulo any interval longer than 4.29s returns the hash
// unchanged, so this function used to return the same handful of sub-5-second
// offsets whether the cadence was 30 seconds or 24 hours: every entry became
// due within seconds of agent start, together, which is exactly what the doc
// comment on due() says this exists to prevent.
//
// fnv.New64a spans 1.8e19, comfortably past any plausible interval, so the
// modulo distributes across the whole cadence and the bias is one part in
// millions. Still deterministic per VM, so a given VM keeps its slot across
// restarts -- though not the SAME slot it had before this fix, since the hash
// function changed. That is a one-off reshuffle, not a behaviour change.
func stagger(vm string, interval time.Duration) time.Duration {
	h := fnv.New64a()
	_, _ = h.Write([]byte(vm))
	if interval <= 0 {
		return 0
	}
	return time.Duration(h.Sum64() % uint64(interval))
}

// markRunning claims an entry and sets its next slot.
//
// The next slot is now+interval, NOT previous+interval. That is what
// implements "skip missed slots, at most one catch-up": an agent that was
// down for three hours runs each entry once when it comes back, then
// resumes its normal cadence, instead of firing every interval it missed in
// a burst against a target host that serves many pairs.
func (s *Scheduler) markRunning(entry ScheduleEntry, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inFlight[entry.VM] = true
	s.nextRun[entry.VM] = now.Add(time.Duration(entry.IntervalSeconds) * time.Second)
}

// deferEntry pushes a broken entry to its next slot without running it, so
// a misconfiguration produces one log line per interval rather than one per
// tick.
func (s *Scheduler) deferEntry(entry ScheduleEntry, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextRun[entry.VM] = now.Add(time.Duration(entry.IntervalSeconds) * time.Second)
}

// admit applies both concurrency limits, taking a slot if it succeeds.
// effectiveMaxConcurrent resolves how many syncs may run at once here.
//
// Three inputs, and the order between them is the point:
//
//   - fromConfig is what the schedule asks for (the UI's estate-wide
//     setting, or max_concurrent_syncs in a standalone file). Zero means it
//     expressed no opinion.
//   - hostLimit is -max-concurrent-syncs on this agent. It is a CEILING,
//     never a floor: it can only lower the number, never raise it. How much
//     concurrent I/O a hypervisor can absorb is a property of that machine
//     -- its disks, its NICs, what else it is running -- and the host is the
//     only party that knows it. A UI must be able to ask for less than the
//     host allows, and must never be able to ask for more.
//   - hardMaxConcurrent clamps everything, because the UI is a separately
//     versioned program whose answers are input to validate rather than
//     trusted state.
func effectiveMaxConcurrent(fromConfig, hostLimit int) int {
	max := fromConfig
	if max <= 0 {
		max = defaultMaxConcurrent
	}
	if max > hardMaxConcurrent {
		max = hardMaxConcurrent
	}
	if hostLimit > 0 && max > hostLimit {
		max = hostLimit
	}
	return max
}

func (s *Scheduler) admit(host *agentConfig, vm, targetHost string, cfg UIConfig) bool {
	max := effectiveMaxConcurrent(cfg.MaxConcurrentSyncs, host.MaxConcurrentSyncs)

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.inFlight) >= max {
		s.metrics.skip(skipHostConcurrency)
		return false
	}
	// Replication slots are the limit only the UI can compute: this agent
	// cannot see that other hosts are also writing to targetHost. Counted
	// here per agent, so the UI's number is what EACH agent may use, not an
	// estate-wide total.
	if slots, ok := cfg.TargetReplicationSlots[targetHost]; ok && slots > 0 {
		if s.hostLoad[targetHost] >= slots {
			s.metrics.skip(skipTargetSlots)
			return false
		}
	}
	s.hostLoad[targetHost]++
	return true
}

func (s *Scheduler) release(vm, targetHost string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inFlight, vm)
	if s.hostLoad[targetHost] > 0 {
		s.hostLoad[targetHost]--
	}
	if s.hostLoad[targetHost] == 0 {
		delete(s.hostLoad, targetHost)
	}
}

// syncPlan is a resolved entry: the request plus the target host it was
// resolved to, which the concurrency accounting needs separately.
type syncPlan struct {
	SyncRequest
	targetHost string
}

// buildRequest resolves an entry into a runnable request.
//
// The target comes from the VM's own replica_targets metadata, read from
// local libvirt -- not from the UI. A domain name supplied over the network
// would be a parameter needing validation; one read from local state is not
// attacker-controlled at all.
func (s *Scheduler) buildRequest(cfg *agentConfig, entry ScheduleEntry) (syncPlan, error) {
	return buildSyncRequest(*cfg, entry)
}

// buildSyncRequest is buildRequest's body, as a free function.
//
// Split out because the operations loop needs it too: a reinit operation is a
// full sync, so it needs this pair's entire transport configuration, and
// building that a second way would let a reinit drift from the scheduled sync
// it is supposed to be a one-off variant of. Nothing here ever needed the
// Scheduler -- only its agentConfig.
func buildSyncRequest(cfg agentConfig, entry ScheduleEntry) (syncPlan, error) {
	mgr, err := libvirtsync.Connect(cfg.LibvirtURI)
	if err != nil {
		return syncPlan{}, fmt.Errorf("connect to %s: %w", cfg.LibvirtURI, err)
	}
	defer mgr.Close()

	domains, err := inventory.Scan(mgr)
	if err != nil {
		return syncPlan{}, err
	}
	var dom inventory.Domain
	found := false
	for _, d := range domains {
		if d.Name == entry.VM {
			dom, found = d, true
			break
		}
	}
	if !found {
		return syncPlan{}, fmt.Errorf("no domain named %q on this host", entry.VM)
	}
	if len(dom.ReplicaTargets) == 0 {
		return syncPlan{}, fmt.Errorf("domain %q records no replica_targets, so there is nothing to sync it to -- run one sync by hand first, or check that this host is really its source", entry.VM)
	}

	ref, err := pickTarget(dom.ReplicaTargets, entry.TargetHost)
	if err != nil {
		return syncPlan{}, fmt.Errorf("domain %q: %w", entry.VM, err)
	}
	host, targetDomain := splitReplicaRef(ref)
	if host == "" || targetDomain == "" {
		return syncPlan{}, fmt.Errorf("domain %q: replica target %q is not in host:domain form", entry.VM, ref)
	}

	plan := syncPlan{targetHost: host}
	plan.SourceURI = cfg.LibvirtURI
	plan.LocalHostName = cfg.Hostname
	plan.SourceDomain = entry.VM
	plan.TargetURI = fmt.Sprintf(cfg.TargetURIPattern, host)
	plan.TargetDomain = targetDomain
	plan.Profile = entry.Profile
	plan.SSHUser = cfg.SSHUser
	plan.SSHKey = cfg.SSHKey
	plan.SSHPort = cfg.SSHPort
	plan.SSHKnownHosts = cfg.SSHKnownHosts
	plan.BridgeHelperPath = cfg.BridgeHelperPath
	if cfg.PrometheusDir != "" {
		plan.PrometheusTextfile = filepath.Join(cfg.PrometheusDir, "vmsync_"+entry.VM+".prom")
	}
	return plan, nil
}

// pickTarget chooses among a source's replica targets.
//
// Refuses to guess when a source fans out to several targets and the entry
// does not say which: silently picking one would mean an operator's
// schedule quietly syncs a different pair than they think.
func pickTarget(targets []string, want string) (string, error) {
	if want == "" {
		if len(targets) == 1 {
			return targets[0], nil
		}
		sorted := append([]string(nil), targets...)
		sort.Strings(sorted)
		return "", fmt.Errorf("replicates to %d targets (%s) but the schedule entry does not say which -- set a target host on it", len(targets), strings.Join(sorted, ", "))
	}
	for _, t := range targets {
		if h, _ := splitReplicaRef(t); strings.EqualFold(h, want) {
			return t, nil
		}
	}
	return "", fmt.Errorf("the schedule entry names target host %q, which is not among this domain's replica_targets", want)
}

func splitReplicaRef(ref string) (host, domain string) {
	i := strings.LastIndex(ref, ":")
	if i < 0 {
		return "", ""
	}
	return ref[:i], ref[i+1:]
}

// runOne executes a single sync and records its outcome.
func (s *Scheduler) runOne(ctx context.Context, cfg *agentConfig, entry ScheduleEntry, plan syncPlan) {
	args := plan.CommandArgs()
	started := time.Now()

	trace.Info("starting scheduled sync", "vm", entry.VM, "target", plan.targetHost, "interval_s", entry.IntervalSeconds)
	trace.Debug("sync command", "vm", entry.VM, "binary", cfg.VmsyncPath, "args", strings.Join(args, " "))

	// exec.CommandContext, not a shell: args is an argv, so nothing here is
	// ever parsed for metacharacters. Cancelling ctx sends SIGKILL by
	// default, which would rob vmsync of its cleanup -- so Cancel is
	// overridden to send SIGTERM and WaitDelay gives it time to unwind
	// before the kill lands.
	cmd := exec.CommandContext(ctx, cfg.VmsyncPath, args...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 60 * time.Second

	// Recorded BEFORE the process exists, and the launch does not happen if it
	// cannot be. Everything except the pid is already known, and the pid
	// belongs on the exit record.
	//
	// Appending afterwards would be worse than useless: it would put a
	// durable write with an fsync between fork and Wait, and if that fsync
	// blocked (NFS, a wedged device) cmd.Wait() would never be reached --
	// cmd.Cancel and WaitDelay are enforced INSIDE Wait, so neither would
	// engage, the deferred release would never run, and the agent would wedge
	// with an orphaned child.
	runID := newRunID()
	if err := cfg.runLog.Append(runLogRecord{
		Event: runEventLaunch, RunID: runID, Origin: runOriginScheduled,
		VM: entry.VM, TargetHost: plan.targetHost,
		Binary: cfg.VmsyncPath, Args: redactArgs(args),
	}); err != nil {
		// Not deferred: nextRun is left in the past so this retries on the
		// next tick, the moment the disk frees. Deferring would add an
		// interval of outage to a condition that may clear in seconds.
		trace.Error("not starting a scheduled sync: its launch could not be recorded, and an unrecorded vmsync is a process nothing can account for",
			"vm", entry.VM, "target", plan.targetHost, "error", err)
		s.metrics.skip(skipRunLogUnwritable)
		return
	}

	s.metrics.runStarted(entry.VM, started)

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err := cmd.Run()
	finished := time.Now()

	res := SyncResult{
		VM:             entry.VM,
		TargetHost:     plan.targetHost,
		StartedAtUnix:  started.Unix(),
		FinishedAtUnix: finished.Unix(),
		DurationSecs:   int64(finished.Sub(started).Seconds()),
		ExitCode:       cmd.ProcessState.ExitCode(),
		LogTail:        tail(out.String(), logTailBytes),
	}

	// Exit 75 is vmsync saying it stood down without touching anything,
	// because another vmsync already holds this domain's lock (util.ExitBusy,
	// and see its comment for why a sync reports it at all).
	//
	// Neither a success nor a failure, and recording it as either is wrong in
	// a way that matters. As a success -- which it was, while the engine
	// exited 0 -- an agent restarted during a long sync reported a phantom
	// healthy run every interval for as long as the real one lasted. As a
	// failure it would drive -reinit-after-failures toward discarding a
	// replica because its own previous run was still going.
	//
	// It is also not a result worth shipping: it says nothing about the pair,
	// and 50 of them would push real outcomes off the report's ring
	// (resultsKept). Counted and journalled, not recorded.
	// The exit record closes the pair opened above. Best-effort, unlike the
	// launch: the process has already run, so refusing anything now would
	// change nothing that has not already happened, and losing this record
	// leaves an open run the log itself reports rather than a silent gap.
	code := res.ExitCode
	outcome := "success"
	switch {
	case res.ExitCode == util.ExitBusy:
		outcome = "busy"
	case err != nil:
		outcome = "failure"
	}
	exitRec := runLogRecord{
		Event: runEventExit, RunID: runID, VM: entry.VM,
		ExitCode: &code, DurationS: res.DurationSecs, Outcome: outcome,
	}
	if cmd.Process != nil {
		exitRec.PID = cmd.Process.Pid
	}
	if outcome != "success" {
		exitRec.LogTail = res.LogTail
	}
	if lerr := cfg.runLog.Append(exitRec); lerr != nil {
		trace.Error("could not record how a sync ended; it will show as an open run until this is cleaned up",
			"vm", entry.VM, "run_id", runID, "error", lerr)
	}

	if res.ExitCode == util.ExitBusy {
		trace.Info("a scheduled sync stood down: another vmsync is already working on this domain, and nothing was changed",
			"vm", entry.VM, "target", plan.targetHost)
		s.metrics.runBusy()
		return
	}

	if err != nil {
		res.Error = err.Error()
		trace.Error("scheduled sync failed", "vm", entry.VM, "target", plan.targetHost,
			"exit_code", res.ExitCode, "duration_s", res.DurationSecs, "error", err)
		// vmsync's own output goes to the host's log too, not only into the
		// report. "exit status 1" on its own says nothing about what went
		// wrong, and the journal on this host is where anyone looks first --
		// before the UI, and in standalone mode instead of it, since there
		// is no report for the tail to travel in and it would otherwise be
		// captured into a buffer and dropped on the floor.
		if out := strings.TrimSpace(res.LogTail); out != "" {
			trace.Error("scheduled sync output", "vm", entry.VM, "output", out)
		}
	} else {
		trace.Info("scheduled sync finished", "vm", entry.VM, "target", plan.targetHost, "duration_s", res.DurationSecs)
	}
	s.metrics.runFinished(err == nil)
	s.record(res)
}

// tail returns the last n bytes, cut at a line boundary so the result is
// readable rather than starting mid-word.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	if i := strings.IndexByte(s, '\n'); i >= 0 && i+1 < len(s) {
		s = s[i+1:]
	}
	return s
}
