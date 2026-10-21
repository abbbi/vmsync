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

// unleasedLockWarning is the warning every unleased run prints, and
// unattributedLockRefusal is what -break-target-lock answers when it is asked
// to break the lock that warning is about. They live next to each other because
// they disagreed, and because the disagreement was only visible to somebody who
// read both.
//
// WHAT THE WARNING USED TO SAY: "until then vmsync -break-target-lock is the
// way past a stale lock". That verb refuses every unleased lock -- the hold
// records no holder, so there is no evidence its holder stopped -- and it has
// to run ON the target, not on the host printing the warning.
// docs/RUNBOOK.md calls this warning "the inventory of hosts where a disaster
// costs two hours", so it is read exactly when there is no time left to
// discover that the remedy named in it cannot work. The project's own bench
// asserts the refusal while treating this line as trustworthy.
//
// Both now take their ways-past from util.UnleasedLockEscape, so the warning
// cannot promise something the refusal contradicts.
func unleasedLockWarning(targetDomain, host, helperPath, reason string) (string, []any) {
	return "the target-side run lock is NOT leased on this target, so a power loss or a partition here will block -promote and -restore on the target for as long as its sshd takes to notice (about two hours by default)",
		[]any{
			"vm", targetDomain, "host", host,
			"reason", reason,
			"remedy", "deploy a matching vmsync-bridge-helper at " + helperPath + " on the target, which makes the lock release itself a lease after this host stops beating",
			// The verb is named only to say that it will refuse. Saying so
			// here costs one line; finding it out costs part of a disaster.
			"if-this-host-dies", "-break-target-lock will REFUSE this lock, and has to run ON the target rather than here in any case: an unleased hold records no holder, so there is no evidence that whoever holds it has stopped. " + util.UnleasedLockEscape("the target", util.RunLockPath(runLockDir, targetLockKey(targetDomain)), ""),
		}
}

// unattributedLockRefusal is the refusal for a lock that records no holder,
// which is every lock the unleased shell hold takes.
func unattributedLockRefusal(targetDomain, reason, helperPath string) error {
	return fmt.Errorf("refusing to break the run lock for %s: %s. An unleased lock records nothing, so there is no evidence to break it on. %s: %w",
		targetDomain, reason, util.UnleasedLockEscape("this host", util.RunLockPath(runLockDir, targetLockKey(targetDomain)), helperPath), util.ErrLockUnattributed)
}

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
		return unattributedLockRefusal(cfg.TargetDomain, d.Reason, cfg.BridgeHelperPath)
	}

	// Journalled before it acts, like every other destructive verb: the record
	// beside the replica is what says a lock was broken and on whose evidence,
	// which is the first thing anybody investigating a two-writer incident will
	// look for.
	//
	// Beside the replica, which is the part this got wrong twice: see
	// newBreakLockRecorder. The directory comes from the domain's own
	// definition rather than from -target-disk-path, which the documented
	// invocation of this verb does not pass.
	journal := newBreakLockRecorder(cfg)
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
		"note", "operations on this replica are possible again. Nothing was dispossessed by this: the lock was taken before the file was removed, so there was no holder to dispossess. What protects the replica if the old driver comes back is its own heartbeat -- a run whose session died learns it on the next beat and refuses both to commit and to redefine. It does NOT learn it from the lock file being gone; that was claimed here and was never true")
	return nil
}
