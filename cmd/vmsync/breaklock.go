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

	"vmsync/pkg/trace"
	"vmsync/pkg/util"
)

// runBreakTargetLock clears a target-side run lock whose holder is gone.
//
// The situation it exists for: a sync driven from another host took this host's
// run lock over SSH and then died without closing the connection -- a power loss
// or a partition. Nothing was delivered here, so the session holding the lock
// survives until sshd's TCP keepalive expires it, roughly two hours, and every
// operation that protects this replica queues behind it. That includes -promote,
// during the disaster the replica exists for.
//
// A leased lock does not need this: vmsync-bridge-helper releases it on its own a
// lease after the driver stops beating. This is the escape for a target that has
// no helper, where the lock has no clock of its own.
//
// It runs ON the host holding the lock, with a local URI, for the same reason
// -promote does: the lock and the process claiming it are both local facts, and a
// decision about them taken from somewhere else would be taken from stale
// information about a host that may be unreachable.
func runBreakTargetLock(ctx context.Context, cfg syncConfig) error {
	if cfg.TargetDomain == "" {
		return fmt.Errorf("-break-target-lock needs -target-domain naming the replica whose lock is stuck")
	}
	if err := requireLocalURI(cfg.TargetURI, "-target-uri"); err != nil {
		return fmt.Errorf("-break-target-lock decides from this host's own /proc, so it has to run on the host that holds the lock: %w", err)
	}

	key := targetLockKey(cfg.TargetDomain)
	d := util.BreakRunLockDecision(runLockDir, key, "")

	if !d.Break {
		// Refused, and the refusal names what to do instead. Breaking a lock
		// whose holder is alive removes the one thing keeping two writers off a
		// single replica, so there is deliberately no flag that overrides this.
		trace.Error("REFUSING to break the target-side run lock", "vm", cfg.TargetDomain, "reason", d.Reason)
		if d.HasHolder {
			return fmt.Errorf("refusing to break the run lock for %s: %s. If that process is genuinely gone, kill it or its ssh session on this host and the lock goes with it: %w",
				cfg.TargetDomain, d.Reason, util.ErrLockHolderAlive)
		}
		return fmt.Errorf("refusing to break the run lock for %s: %s. An unleased lock records nothing, so the only safe ways past it are to wait for this host's sshd to expire the session (see ClientAliveInterval), or to find and end that session yourself -- `ss -tp | grep sshd` and `loginctl`/`ps` name it. Deploying vmsync-bridge-helper on this host makes future locks release themselves: %w",
			cfg.TargetDomain, d.Reason, util.ErrLockUnattributed)
	}

	// Journalled before it acts, like every other destructive verb: the record
	// beside the replica is what says a lock was broken and on whose evidence,
	// which is the first thing anybody investigating a two-writer incident will
	// look for.
	journal := newRecorder(cfg, nil, cfg.TargetDiskPath, cfg.TargetDomain)
	journal.Intent(ctx, journalVerbBreakTargetLock, map[string]string{
		"holder_pid": fmt.Sprintf("%d", d.Holder.PID),
		"holder_run": d.Holder.RunID,
		"evidence":   d.Reason,
	})

	released, err := util.BreakRunLock(runLockDir, key)
	finishAction(ctx, journal, err, map[string]string{"released": fmt.Sprintf("%t", released)})
	if err != nil {
		return fmt.Errorf("break the run lock for %s: %w", cfg.TargetDomain, err)
	}

	// ERROR level on success, matching -release-promotion: this is a line
	// somebody needs to find later, and INFO is where lines go to be unread.
	trace.Error("BROKE the target-side run lock", "vm", cfg.TargetDomain,
		"evidence", d.Reason, "holder_pid", d.Holder.PID, "holder_run", d.Holder.RunID,
		"note", "operations on this replica are possible again. If the host that held this lock comes back and resumes writing, it will find the lock gone -- its own gates before commit and before define refuse on that, which is what stops two writers")
	return nil
}
