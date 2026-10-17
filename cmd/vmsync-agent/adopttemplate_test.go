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
	"testing"
	"time"

	"vmsync/pkg/util"
)

// templateAdoptFixture is adoptFixture with templates, and with the
// syncable-VM cache pre-seeded.
//
// Seeding that cache is what keeps this test off libvirt: syncableVMs reads
// it when it is fresh and only scans when it is not, so filling it here
// supplies the one input ResolveSchedule needs that would otherwise require
// a hypervisor.
func templateAdoptFixture(t *testing.T, tpl ScheduleTemplate, syncable []string, entries ...ScheduleEntry) (*Scheduler, *agentConfig, string) {
	t.Helper()
	s, cfg, dir := adoptFixture(t, entries...)
	s.state.set(CachedConfig{Config: UIConfig{
		Schedule:  entries,
		Templates: map[string]ScheduleTemplate{DefaultTemplateName: tpl},
	}})
	s.mu.Lock()
	s.syncable, s.syncableAt = syncable, time.Now()
	s.mu.Unlock()
	return s, cfg, dir
}

// writeLiveLock makes this test process stand in for a running vmsync, the
// same way TestReconcileAdoptsALiveForeignRun does.
func writeLiveLock(t *testing.T, dir, vm string, startedAt time.Time) {
	t.Helper()
	self := os.Getpid()
	ticks, err := util.ProcStartTicks(self)
	if err != nil {
		t.Fatalf("ProcStartTicks(self): %v", err)
	}
	boot, _ := util.CurrentBootID()
	f, err := os.Create(util.RunLockPath(dir, vm))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	if err := util.WriteRunLockIdentity(f, util.RunLockIdentity{
		PID: self, BootID: boot, StartTicks: ticks,
		Kind: "sync", SourceDomain: vm, TargetRef: "dr01:" + vm,
		RunID: "run-live", StartedAtUnix: startedAt.Unix(),
	}); err != nil {
		t.Fatal(err)
	}
}

// A VM COVERED ONLY BY A DEFAULT TEMPLATE HAS NO ENTRY TO ITERATE.
//
// Reconcile walked the published schedule raw, so such a VM was never
// adopted -- which is exactly the state this function exists to repair.
// Its concurrency slot stayed free, so the host over-admits into a target
// another host may already be saturating, and vmsync_agent_syncs_running
// read 0 while a sync was demonstrably in progress.
func TestReconcileAdoptsAVMCoveredOnlyByATemplate(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs /proc to establish that a pid is alive")
	}
	tpl := ScheduleTemplate{Name: DefaultTemplateName, Enabled: true, IntervalSeconds: 900}
	// No explicit entries at all: web01 exists only because the host
	// reports it as syncable and a default template covers it.
	s, _, dir := templateAdoptFixture(t, tpl, []string{"web01"})
	writeLiveLock(t, dir, "web01", time.Now().Add(-time.Minute))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.Reconcile(ctx)

	if !s.isRunning("web01") {
		t.Error("a live run of a template-covered VM was not adopted. Its slot stays free, so this host over-admits into the target, and the running-syncs gauge reads 0 while the sync is still going")
	}
}

// THE SECOND HALF, which is not about WHICH VMs are adopted but with what
// cadence. An entry that names only its VM inherits interval_seconds from
// the template; unresolved it arrives as 0, and adopt then sets nextRun to
// the moment the run STARTED -- so the VM falls due the instant the adopted
// run ends, instead of one interval later.
func TestAnAdoptedRunKeepsTheCadenceItInherited(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs /proc to establish that a pid is alive")
	}
	tpl := ScheduleTemplate{Name: DefaultTemplateName, Enabled: true, IntervalSeconds: 900}
	// An explicit entry that inherits its cadence: no interval_seconds.
	entry := ScheduleEntry{VM: "web01", Enabled: true}
	s, _, dir := templateAdoptFixture(t, tpl, []string{"web01"}, entry)

	startedAt := time.Now().Add(-time.Minute)
	writeLiveLock(t, dir, "web01", startedAt)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.Reconcile(ctx)

	s.mu.Lock()
	next := s.nextRun["web01"]
	s.mu.Unlock()

	if next.IsZero() {
		t.Fatal("the adopted run set no next-run time at all")
	}
	wantAfter := startedAt.Add(800 * time.Second)
	if !next.After(wantAfter) {
		t.Errorf("next run is %v, which is barely after the run started (%v). The inherited 900s cadence was lost, so this VM becomes due the moment the adopted run ends and syncs back-to-back",
			next, startedAt)
	}
}
