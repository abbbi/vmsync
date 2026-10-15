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
//     parts were written. It is supervised for the liveness line alone.
//   - the write and verify export starts are not safe either, as their
//     teardown stands: qemu-nbd --fork daemonizes before it has finished
//     opening the image and writes its pidfile only after setup, so an
//     abandoned start leaves a process that no pidfile names -- and when the
//     storage recovers, that process takes a pidfile shared with the retry
//     that meanwhile succeeded. Giving them a limit needs the argv-matched
//     reclaim the pre-commit checksum export already has.
//   - creating the overlay is not safe while its cleanup is armed after the
//     create rather than before it.
//
// So the default is no limit, and this is opted into per call site.
const targetCommandMaxRuntime = 30 * time.Second

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
func runSupervisedTargetCommand(ctx context.Context, client *remotessh.Client, helperPath string, maxRuntime time.Duration, command, action string) (string, error) {
	// No mode flag: supervising one command from stdin is what the helper
	// does when nothing else is asked for, and a word every caller passes is
	// a word that says nothing. -max-runtime is what distinguishes these calls
	// from each other.
	helperCmd := util.ShQuote(helperPath) +
		" -max-runtime " + util.ShQuote(maxRuntime.String()) +
		" -kill-grace " + util.ShQuote(targetCommandKillGrace.String()) +
		" -label " + util.ShQuote(action)

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
