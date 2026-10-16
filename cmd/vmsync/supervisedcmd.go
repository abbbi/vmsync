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
	"strings"
	"time"

	"vmsync/pkg/remotessh"
	"vmsync/pkg/trace"
	"vmsync/pkg/util"
)

// targetCommandMaxRuntime is how long a target command that is SAFE TO ABANDON
// may run before the helper signals it.
//
// Thirty seconds because every command given this limit is one that should
// complete in milliseconds: making a directory under the runtime dir,
// probing for a binary, starting a qemu-nbd whose image opens instantly,
// removing an overlay. A command in that set that has taken thirty seconds
// is not slow, it is stuck, and the storage it is stuck on is the thing this
// bound exists to report.
//
// MOST TARGET COMMANDS DO NOT GET ONE, and that is the part worth reading
// before raising or lowering the number. Abandoning a command only helps if
// what it leaves behind can be cleaned up by whoever comes next, and for
// most of these it cannot:
//
//   - qemu-img commit must never be abandoned. The base image is then
//     neither its old self nor its new one, and no later run can tell which
//     parts were written. This is the one entry on this list that is not
//     going to change -- but it is now WATCHED rather than merely
//     supervised: targetCommandStallAfter in warn-only mode measures it
//     exactly like the others and signals nothing, so a commit wedged on
//     the replica's storage reports "NO PROGRESS for 3 beats" instead of
//     repeating "still alive" for an hour. Not being allowed to kill a
//     command was never a reason not to look at it.
//
// The other two have since been made safe, and how says what "safe to
// abandon" actually requires:
//
//   - the write and verify export starts now carry -watch-argv keyed on
//     their pidfile path, so the daemon qemu-nbd --fork setsid()s out of
//     the helper's process group is measured and signalled with the rest,
//     and an argv-matched reclaim before each start kills one that
//     outlived its run anyway.
//   - creating the overlay now arms its cleanup BEFORE the create rather
//     than after, so a create signalled part-way leaves nothing behind.
//
// Both took the stall bound rather than this one. See targetCommandStallAfter.
//
// So the default is no limit, and this is opted into per call site.
//
// One minute rather than the thirty seconds this started at: these
// commands should finish in milliseconds, but a hypervisor under real load
// can take seconds to fork and exec anything at all, and the cost of being
// generous here is one extra minute before a genuinely stuck mkdir is
// reported. The cost of being tight is a run failed on a busy host.
const targetCommandMaxRuntime = time.Minute

// targetCommandStallAfter is how long a target command may make NO progress
// -- no CPU, no read/write syscalls, no block I/O, across its whole process
// group -- before the helper signals it.
//
// THIS IS THE BOUND TO PREFER, and the difference from targetCommandMaxRuntime
// is not the number but the question. A clock asks "how long should this
// take", which cannot be answered for anything whose runtime scales with the
// storage: opening a cold 2 TB qcow2 is slow and healthy on one host and a
// symptom on another. A stall bound asks "is it doing anything", which has
// the same answer everywhere. A command advancing at one byte a minute is
// never signalled by it.
//
// Three minutes, as three beats of trace.ProgressInterval, because that is
// the bound the digest already uses for the same judgement -- and an
// operator who has learned what "three missed beats" means should not have
// to learn a second number for the same idea.
const targetCommandStallAfter = 3 * trace.ProgressInterval

// targetCommandBounds is how one supervised command may be stopped. All
// fields zero means unbounded, which remains the default for everything
// that cannot prove it is safe to abandon.
type targetCommandBounds struct {
	// MaxRuntime is a wall clock. Zero means none.
	MaxRuntime time.Duration
	// StallAfter is a progress bound. Zero means none.
	StallAfter time.Duration
	// StallWarnOnly measures the same way and never signals, for a command
	// that must run to completion whatever the storage does.
	StallWarnOnly bool
	// WatchArgv names a process that leaves the helper's process group --
	// qemu-nbd --fork calls setsid() -- and so must be found again by its
	// command line to be measured and signalled. Empty when none does.
	WatchArgv string
}

// reclaimAbandonedExportCmd builds a shell command that kills any qemu-nbd
// whose command line contains match, and prints what it killed.
//
// WHY THIS IS STILL NEEDED NOW THAT THE HELPER SIGNALS THE DAEMON. The
// helper's -watch-argv finds the daemon that setsid() put outside its
// process group and kills it on the way out, which closes the common case.
// It cannot close two others. A daemon wedged in uninterruptible I/O does
// not die when signalled, and vmsync dying takes the SSH session and the
// helper with it, leaving nobody on the target to signal anything at all.
// In both, a qemu-nbd outlives the run that started it -- and when its
// storage recovers it writes the pidfile that a later run's export is
// using, so the later run's teardown kills the wrong process by number.
// Matching on argv is the only way to find one, because by definition the
// pidfile no longer names it.
//
// IT PRINTS WHAT IT KILLS, like the checksum export's own reclaim: a
// reclaim that cleans up silently hides the leak it compensates for, and
// the next person to look finds a green run and no evidence.
func reclaimAbandonedExportCmd(match string) string {
	return "for d in /proc/[0-9]*; do " +
		"[ -r \"$d/comm\" ] || continue; " +
		"read -r c < \"$d/comm\" || continue; " +
		"[ \"$c\" = qemu-nbd ] || continue; " +
		"tr '\\0' '\\n' < \"$d/cmdline\" 2>/dev/null | grep -qxF " + util.ShQuote(match) + " || continue; " +
		"img=$(tr '\\0' '\\n' < \"$d/cmdline\" 2>/dev/null | tail -1); " +
		"echo \"pid=${d#/proc/} image=$img\"; " +
		"kill -9 \"${d#/proc/}\" 2>/dev/null || true; " +
		"done"
}

// targetCommandKillGrace is how long a signalled target command has to
// honour SIGTERM before SIGKILL, and then how long the helper waits for a
// killed command to be reaped before reporting that it was not.
//
// Five seconds: everything bounded here either exits on SIGTERM at once or
// is wedged in uninterruptible I/O and will never exit at all. Waiting
// longer does not change which of those it was, and only delays the report
// somebody is waiting for.
const targetCommandKillGrace = 5 * time.Second

// runSupervisedTargetCommand runs one command on the target through
// vmsync-bridge-helper instead of through a bare SSH exec.
//
// WHAT THIS BUYS, which is not obvious: only a process on the target can
// signal a command whose storage has wedged. Cancelling a context here stops
// vmsync LISTENING and nothing else -- the command keeps running, unsignalled
// and unsupervised, and if the storage recovers an hour later it finishes,
// binding a port this run has long since released. The helper can signal it,
// can report every minute while it waits, and can say what the command's
// process is actually doing (state D is uninterruptible I/O, which is the
// whole diagnosis).
//
// Returns the command's own combined output, exactly as the bare SSH exec
// did, so the error a caller builds from it says the same things it always
// said.
func runSupervisedTargetCommand(ctx context.Context, client *remotessh.Client, helperPath string, bounds targetCommandBounds, command, action string) (string, error) {
	// No mode flag: supervising one command from stdin is what the helper
	// does when nothing else is asked for, and a word every caller passes is
	// a word that says nothing. The bounds are what distinguish these calls
	// from each other.
	helperCmd := util.ShQuote(helperPath) +
		" -kill-grace " + util.ShQuote(targetCommandKillGrace.String()) +
		" -label " + util.ShQuote(action)
	// Only the bounds this command actually has. A zero passed explicitly
	// means the same thing to the helper as omitting it, but omitting it
	// keeps the command line of an unbounded call honest about being one.
	if bounds.MaxRuntime > 0 {
		helperCmd += " -max-runtime " + util.ShQuote(bounds.MaxRuntime.String())
	}
	if bounds.StallAfter > 0 {
		flag := " -stall-after "
		if bounds.StallWarnOnly {
			flag = " -stall-warn "
		}
		helperCmd += flag + util.ShQuote(bounds.StallAfter.String())
	}
	if bounds.WatchArgv != "" {
		helperCmd += " -watch-argv " + util.ShQuote(bounds.WatchArgv)
	}

	watch := watchHelperSilence(ctx, func(quiet, elapsed time.Duration) {
		trace.Info("target: still waiting on a command", "action", action,
			"elapsed", elapsed.Round(time.Second).String(),
			"silent_for", quiet.Round(time.Second).String())
	})
	defer watch.stop()

	// The helper's beats and its verdict arrive here line by line while the
	// command runs. Logged as they arrive rather than with the result,
	// because a command that never finishes has no result -- which is the
	// case all of this is for.
	onLine := func(line string) {
		watch.heard(line)
		trace.Info("target: "+line, "action", action)
	}

	// The command on stdin, never in argv: it would otherwise be quoted
	// twice over, once for the remote shell and once for the helper's flag,
	// and every one of these commands carries paths and shell operators.
	stdout, stderr, err := client.RunWithInputProgress(watch.ctx, helperCmd, []byte(command), onLine)
	if err == nil {
		return strings.TrimSpace(stdout), nil
	}

	if stalled, lastLine, heardAny, quiet, elapsed := watch.verdict(); stalled {
		trace.Error("target: giving up on a command the helper stopped reporting", "action", action,
			"helper", helperPath,
			"elapsed", elapsed.Round(time.Second).String(),
			"silent_for", quiet.Round(time.Second).String(),
			"bound", targetDigestStallBound.String(),
			"heard_anything", heardAny, "last_line", lastLine)
		return strings.TrimSpace(stdout), fmt.Errorf("the helper on the target stopped reporting: nothing for %s (bound %s, %s elapsed) -- it beats once per %s while a command runs, so silence this long means the helper or its output channel is gone",
			quiet.Round(time.Second), targetDigestStallBound, elapsed.Round(time.Second), trace.ProgressInterval)
	}
	return strings.TrimSpace(stdout), fmt.Errorf("%w%s", err, formatRemoteStderr(stderr))
}
