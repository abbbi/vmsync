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
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"vmsync/pkg/util"
)

// adoptFixture builds a Scheduler with its lock directory pointed at a temp
// path, so adoption can be exercised without /run/vmsync-locks.
func adoptFixture(t *testing.T, entries ...ScheduleEntry) (*Scheduler, *agentConfig, string) {
	t.Helper()
	dir := t.TempDir()
	m := newAgentMetrics("test", "host01", true)
	cfg := agentConfig{
		VmsyncPath: "/usr/local/bin/vmsync",
		StateDir:   dir,
		metrics:    m,
		runLog:     newRunLog(dir, "session-2", m),
	}
	if err := cfg.runLog.Open(); err != nil {
		t.Fatalf("run log: %v", err)
	}
	t.Cleanup(func() { cfg.runLog.Close() })

	lv := newLive(cfg)
	state := &sharedState{cached: CachedConfig{Config: UIConfig{Schedule: entries}}}
	s := NewScheduler(lv, state)
	s.lockDir = dir
	return s, lv.get(), dir
}

// The accounting an adopted run must produce. This is the half that matters
// and the half that was missing: launchDue's lock probe already stopped a
// duplicate LAUNCH, but nothing made the run visible, so the concurrency slot
// stayed free and the dashboard showed an idle host.
func TestAdoptTakesASlotAndPreservesCadence(t *testing.T) {
	entry := ScheduleEntry{VM: "web01", IntervalSeconds: 900, Enabled: true}
	s, cfg, _ := adoptFixture(t, entry)

	// Cancelled, so the watcher goroutine returns immediately instead of
	// polling for ten seconds and then releasing what this test is asserting.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	startedAt := time.Now().Add(-5 * time.Minute)
	s.adopt(ctx, cfg, entry, util.RunLockIdentity{
		PID: 4242, RunID: "run-abc", TargetRef: "dr01:web01",
		StartedAtUnix: startedAt.Unix(),
	}, "pid 4242 is still running")

	if !s.isRunning("web01") {
		t.Error("an adopted run is not counted as in flight, so the next tick would launch a duplicate")
	}

	s.mu.Lock()
	load := s.hostLoad["dr01"]
	next := s.nextRun["web01"]
	s.mu.Unlock()

	if load != 1 {
		t.Errorf("hostLoad[dr01] = %d, want 1 -- without the slot this host over-admits into a target another host may already be saturating", load)
	}
	// startedAt + interval, NOT now + interval. Restarting the cadence would
	// let an agent restart quietly delay every VM by up to a full interval.
	wantNext := startedAt.Add(900 * time.Second)
	if d := next.Sub(wantNext); d > time.Second || d < -time.Second {
		t.Errorf("nextRun = %v, want %v (cadence preserved from the original start, not restarted)", next, wantNext)
	}

	// The metric must carry the ORIGINAL start, or an adopted VM's
	// last-attempt timestamp jumps forward on every agent restart.
	_, _ = s.metricsSnapshot()
	s.metrics.mu.Lock()
	last := s.metrics.lastAttempt["web01"]
	s.metrics.mu.Unlock()
	if last != startedAt.Unix() {
		t.Errorf("last attempt = %d, want the original start %d", last, startedAt.Unix())
	}
}

// syncs_running must include adopted runs. It read 0 during one before, which
// tells an operator the host is idle while a sync is demonstrably in progress.
func TestAdoptedRunCountsAsRunning(t *testing.T) {
	entry := ScheduleEntry{VM: "web01", IntervalSeconds: 900, Enabled: true}
	s, cfg, _ := adoptFixture(t, entry)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if running, _ := s.metricsSnapshot(); running != 0 {
		t.Fatalf("running = %d before adopting anything", running)
	}
	s.adopt(ctx, cfg, entry, util.RunLockIdentity{PID: 4242, TargetRef: "dr01:web01"}, "")
	if running, _ := s.metricsSnapshot(); running != 1 {
		t.Errorf("running = %d after adopting a run, want 1", running)
	}
}

// The handover is recorded, so the run log shows it rather than an
// unexplained gap between one session's launch and another session's exit.
func TestAdoptWritesAnAdoptRecord(t *testing.T) {
	entry := ScheduleEntry{VM: "web01", IntervalSeconds: 900, Enabled: true}
	s, cfg, _ := adoptFixture(t, entry)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s.adopt(ctx, cfg, entry, util.RunLockIdentity{
		PID: 4242, RunID: "run-abc", TargetRef: "dr01:web01",
		StartedAtUnix: time.Now().Add(-time.Minute).Unix(),
	}, "")

	data, err := os.ReadFile(cfg.runLog.path)
	if err != nil {
		t.Fatal(err)
	}
	if !containsAll(string(data), `"event":"adopt"`, `"run_id":"run-abc"`, `"vm":"web01"`) {
		t.Errorf("no adopt record naming the run:\n%s", data)
	}
}

// An identity with no start time is an older vmsync. It must still adopt --
// refusing would leave the slot free, which is the bug -- and simply lose the
// cadence, which only affects when that VM is next due.
func TestAdoptToleratesAnIdentityWithNoStartTime(t *testing.T) {
	entry := ScheduleEntry{VM: "web01", IntervalSeconds: 900, Enabled: true}
	s, cfg, _ := adoptFixture(t, entry)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s.adopt(ctx, cfg, entry, util.RunLockIdentity{PID: 4242, TargetRef: "dr01:web01"}, "")
	if !s.isRunning("web01") {
		t.Error("a run with no recorded start time was not adopted")
	}
}

// End to end, against a genuinely live process: this one. Needs /proc, so it
// only means anything on Linux.
func TestReconcileAdoptsALiveForeignRun(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs /proc to establish that a pid is alive")
	}
	entry := ScheduleEntry{VM: "web01", IntervalSeconds: 900, Enabled: true}
	s, _, dir := adoptFixture(t, entry)

	// The test process itself stands in for a running vmsync. RunLockHeld
	// reports a binary mismatch as HELD -- something is holding a vmsync lock
	// and launching into that is not an improvement -- so this adopts, which
	// is the behaviour being checked.
	self := os.Getpid()
	ticks, err := util.ProcStartTicks(self)
	if err != nil {
		t.Fatalf("ProcStartTicks(self): %v", err)
	}
	boot, _ := util.CurrentBootID()

	f, err := os.Create(util.RunLockPath(dir, "web01"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := util.WriteRunLockIdentity(f, util.RunLockIdentity{
		PID: self, BootID: boot, StartTicks: ticks,
		Kind: "sync", SourceDomain: "web01", TargetRef: "dr01:web01",
		RunID: "run-live", StartedAtUnix: time.Now().Add(-time.Minute).Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.Reconcile(ctx)

	if !s.isRunning("web01") {
		t.Error("a live foreign run was not adopted; the next tick would launch a duplicate")
	}
}

// A lock left behind by a process that is gone must NOT be adopted, or the VM
// is held out of the schedule forever by a file nobody owns.
func TestReconcileIgnoresAStaleLock(t *testing.T) {
	entry := ScheduleEntry{VM: "web01", IntervalSeconds: 900, Enabled: true}
	s, _, dir := adoptFixture(t, entry)

	f, err := os.Create(util.RunLockPath(dir, "web01"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	// A boot id from a previous boot: settled by one string compare, before
	// /proc is consulted at all.
	if err := util.WriteRunLockIdentity(f, util.RunLockIdentity{
		PID: 4242, BootID: "a-previous-boot", StartTicks: 1,
		TargetRef: "dr01:web01", StartedAtUnix: time.Now().Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.Reconcile(ctx)

	if s.isRunning("web01") {
		t.Error("a lock from a previous boot was adopted; that VM would never sync again")
	}
}

// No lock file at all is the ordinary case for every VM on a freshly started
// agent, and must be silent.
func TestReconcileWithNoLocksDoesNothing(t *testing.T) {
	entry := ScheduleEntry{VM: "web01", IntervalSeconds: 900, Enabled: true}
	s, _, _ := adoptFixture(t, entry)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s.Reconcile(ctx)
	if s.isRunning("web01") {
		t.Error("a VM with no lock file was adopted")
	}
	if running, _ := s.metricsSnapshot(); running != 0 {
		t.Errorf("running = %d, want 0", running)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
