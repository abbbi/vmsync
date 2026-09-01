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
	"time"

	"vmsync/pkg/trace"
	"vmsync/pkg/util"
)

// adoptPollInterval is how often an adopted run is checked for having ended.
//
// The same cadence as the scheduler's own tick. There is nothing to gain from
// noticing sooner: the only thing that changes when an adopted run ends is
// that its VM becomes eligible again, which is a decision that tick makes
// anyway.
const adoptPollInterval = tickInterval

// Reconcile takes ownership of syncs a PREVIOUS instance of this agent
// started and which are still running.
//
// Called once, before Run, and synchronously -- otherwise the first
// launchDue can fire a duplicate before this has finished looking.
//
// The problem it closes: inFlight is in-memory, so a new process after a
// restart, a crash or a package upgrade knows nothing about a vmsync the
// previous one launched. launchDue's per-tick lock probe already stops it
// LAUNCHING a duplicate, but it does not make the run visible: the concurrency
// slot is not taken, so the host can over-admit into a target another host is
// already saturating, and vmsync_agent_syncs_running reads 0 while a sync is
// demonstrably in progress. An operator looking at that dashboard sees an idle
// host.
//
// NOT a ledger, and deliberately not durable. This is rebuilt from the run
// locks every time the agent starts, because the locks are the only record
// whose lifetime is tied to the processes they describe -- anything the agent
// wrote about its own children outlives them.
func (s *Scheduler) Reconcile(ctx context.Context) {
	cfg := s.lv.get()
	cached := s.state.get()

	adopted := 0
	for _, entry := range cached.Config.Schedule {
		if entry.VM == "" {
			continue
		}
		id, ok, err := util.ReadRunLockIdentity(util.RunLockDir, entry.VM)
		if err != nil || !ok {
			continue
		}
		held, reason := util.RunLockHeld(id, cfg.VmsyncPath)
		if !held {
			continue
		}
		s.adopt(ctx, cfg, entry, id, reason)
		adopted++
	}
	if adopted > 0 {
		trace.Warning("adopted syncs started by a previous instance of this agent; they are counted as running and their VMs will not be launched again until they finish",
			"count", adopted)
	}
}

// adopt records a foreign run as in-flight and watches for it to end.
func (s *Scheduler) adopt(ctx context.Context, cfg *agentConfig, entry ScheduleEntry, id util.RunLockIdentity, reason string) {
	startedAt := time.Unix(id.StartedAtUnix, 0)
	if id.StartedAtUnix == 0 {
		// An older vmsync that wrote no start time. Now is wrong but bounded,
		// and it only affects when this VM next becomes due.
		startedAt = time.Now()
	}
	targetHost, _ := splitReplicaRef(id.TargetRef)

	s.mu.Lock()
	s.inFlight[entry.VM] = true
	if targetHost != "" {
		// Re-take the per-target slot. Without this a restarted agent
		// over-admits into a target that another host may already be
		// saturating -- the one limit only the control plane can compute,
		// silently over-committed for the length of the adopted run.
		s.hostLoad[targetHost]++
	}
	// startedAt + interval, NOT now + interval: the cadence is preserved
	// rather than restarted, which is the same reasoning markRunning uses.
	// Restarting it would let an agent restart quietly delay every VM.
	s.nextRun[entry.VM] = startedAt.Add(time.Duration(entry.IntervalSeconds) * time.Second)
	s.mu.Unlock()

	// The ORIGINAL start time, so an adopted VM's last-attempt timestamp does
	// not jump forward every time the agent restarts.
	s.metrics.runStarted(entry.VM, startedAt)

	trace.Warning("adopting a sync this agent did not start",
		"vm", entry.VM, "target", targetHost, "pid", id.PID,
		"run_id", id.RunID, "started", startedAt.Format(time.RFC3339), "reason", reason)

	// Recorded so the run log shows the handover rather than an unexplained
	// gap between one session's launch and another session's exit.
	_ = cfg.runLog.Append(runLogRecord{
		Event: runEventAdopt, RunID: id.RunID, Origin: runOriginScheduled,
		VM: entry.VM, TargetHost: targetHost, PID: id.PID,
		StartedAtUnix: id.StartedAtUnix,
	})

	go s.watchAdopted(ctx, cfg, entry.VM, targetHost, id, startedAt)
}

// watchAdopted releases an adopted run once its process is gone.
//
// Polling, not a wait: this process is not the child's parent, so there is
// nothing to Wait on and no exit status to collect. That loss is permanent
// and by construction -- see the outcome recorded below, which says so rather
// than guessing.
func (s *Scheduler) watchAdopted(ctx context.Context, cfg *agentConfig, vm, targetHost string, id util.RunLockIdentity, startedAt time.Time) {
	t := time.NewTicker(adoptPollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// Shutting down. The slot goes with the process.
			return
		case <-t.C:
		}
		if held, _ := util.RunLockHeld(id, cfg.VmsyncPath); held {
			continue
		}

		s.mu.Lock()
		delete(s.inFlight, vm)
		if targetHost != "" && s.hostLoad[targetHost] > 0 {
			s.hostLoad[targetHost]--
			if s.hostLoad[targetHost] == 0 {
				delete(s.hostLoad, targetHost)
			}
		}
		s.mu.Unlock()

		// UNKNOWN, not success. The exit status of a process this agent did
		// not start cannot be read, and reporting 0 would put a green tick on
		// a run nobody observed the end of. The console renders this as its
		// own state for exactly that reason.
		trace.Info("an adopted sync has ended; its outcome is not knowable because this agent did not start it",
			"vm", vm, "run_id", id.RunID, "pid", id.PID)
		_ = cfg.runLog.Append(runLogRecord{
			Event: runEventExit, RunID: id.RunID, VM: vm,
			Outcome: outcomeUnknown, PID: id.PID,
			DurationS: int64(time.Since(startedAt).Seconds()),
		})
		s.record(SyncResult{
			VM:             vm,
			TargetHost:     targetHost,
			StartedAtUnix:  startedAt.Unix(),
			FinishedAtUnix: time.Now().Unix(),
			DurationSecs:   int64(time.Since(startedAt).Seconds()),
			RunID:          id.RunID,
			Outcome:        outcomeUnknown,
			// ExitCode stays nil. That is the whole point of it being a
			// pointer.
		})
		return
	}
}
