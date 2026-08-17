/*
	Copyright (C) 2026  Michael Ablassmeier <abi@grinser.de>

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

package nbdbridge

import (
	"context"
	"fmt"
	"path"
	"time"

	"vmsync/pkg/remotessh"
	"vmsync/pkg/trace"
	"vmsync/pkg/util"
)

// bridgeStateDir holds bridge PID/log files. Deliberately NOT under /tmp:
// hardened hosts commonly run pam_namespace to give each login session its
// own private, bind-mounted /tmp, so a file written by the SSH session that
// starts the bridge can be genuinely invisible to a different SSH session
// (or an interactive shell) checking it afterward, even though it exists.
// /run is session-independent tmpfs, the standard location for this kind of
// daemon state, and isn't subject to that per-session isolation.
const bridgeStateDir = "/run/vmsync-bridge"

// StartRemote launches a backgrounded vmsync-bridge-helper (cfg.HelperPath)
// on client, listening on bridgePort (all interfaces by default, loopback-only
// when cfg.UseSSH is set -- see BuildStartCommand) and, per accepted
// connection, relaying to the real NBD export already listening on
// 127.0.0.1:realPort on the same host. It waits for the bridge process to
// actually be running before returning, and returns a stop command the
// caller should append to its own teardown list -- this function does not
// manage the bridge's lifecycle itself beyond that.
//
// A caller only ever gets one of two outcomes: a confirmed-running bridge
// plus the stop command to register, or a non-nil error with nothing left
// running on client. Once the start command has been issued, any failure
// path (the SSH call itself failing, or the process never reaching
// listening state) kills and cleans up whatever was just started before
// returning -- see killOrphanedRemoteBridge. Without that, a slow or wedged
// helper failing its readiness check would leave the actual backgrounded
// process running on the remote host indefinitely: the empty stopCmd this
// returns alongside an error was never something any caller could act on,
// since the whole point of a stop command is to name the pidFile/logFile a
// caller doesn't otherwise know.
func StartRemote(ctx context.Context, client *remotessh.Client, bridgePort, realPort int, cfg Config) (stopCmd string, err error) {
	if out, err := client.Run(ctx, "mkdir -p "+util.ShQuote(bridgeStateDir)); err != nil {
		return "", fmt.Errorf("create bridge state dir %s: %w: %s", bridgeStateDir, err, out)
	}

	pidFile := path.Join(bridgeStateDir, fmt.Sprintf("vmsync-bridge-%d.pid", bridgePort))
	logFile := path.Join(bridgeStateDir, fmt.Sprintf("vmsync-bridge-%d.log", bridgePort))

	startCmd := BuildStartCommand(cfg, bridgePort, realPort, pidFile, logFile)
	if out, err := client.Run(ctx, startCmd); err != nil {
		// The backgrounded helper may or may not have actually forked before
		// the SSH call itself failed -- ambiguous, but killOrphanedRemoteBridge
		// is safe to run either way (see its own doc comment).
		killOrphanedRemoteBridge(ctx, client, pidFile, logFile, bridgePort)
		return "", fmt.Errorf("start remote nbd bridge on port %d: %w: %s", bridgePort, err, out)
	}

	if err := waitForRemoteListening(ctx, client, bridgePort, pidFile, 10*time.Second); err != nil {
		killOrphanedRemoteBridge(ctx, client, pidFile, logFile, bridgePort)
		return "", fmt.Errorf("remote nbd bridge on port %d did not start: %w", bridgePort, err)
	}

	return BuildStopCommand(pidFile, logFile), nil
}

// killOrphanedRemoteBridge best-effort kills and removes the bookkeeping
// files for the helper process StartRemote just tried to start, for any of
// its failure paths after the start command was issued. Reuses
// BuildStopCommand verbatim -- the same command a successful start would
// have handed back to the caller to run later -- rather than a separate,
// parallel cleanup implementation: its "kill ... || true" and "rm -f" are
// already tolerant of pidFile never having been written at all (e.g. the
// start command's SSH channel failed before the backgrounded process could
// even fork), so it's safe to run unconditionally on any of these paths.
// Only logs its own failure rather than returning it: the error that
// triggered this cleanup is what the caller needs to see, and a failed
// best-effort cleanup attempt must not shadow it.
func killOrphanedRemoteBridge(ctx context.Context, client *remotessh.Client, pidFile, logFile string, bridgePort int) {
	if out, err := client.Run(ctx, BuildStopCommand(pidFile, logFile)); err != nil {
		trace.Warning("failed to clean up remote nbd bridge helper after a failed start -- it may still be running on the remote host", "port", bridgePort, "error", err, "output", out)
	}
}

// waitForRemoteListening polls the remote host's own socket table (via "ss")
// until bridgePort is actually in LISTEN state under pidFile's own PID,
// rather than merely checking that the process exists ("kill -0"),
// attempting a real TCP connection, or that *anything* is listening on the
// port at all.
//
// A real TCP connect-and-close probe was used here originally and caused a
// deadlock: vmsync-bridge-helper's accept loop treats ANY accepted
// connection as a real one and immediately dials the real NBD export for
// it -- which, by qemu-nbd's default --shared=1, only allows a single
// simultaneous client. A disposable readiness probe occupied that one slot
// until its side of the relay fully unwound, racing against (and usually
// losing to) the real data connection that immediately followed it, wedging
// the whole sync.
//
// A "kill -0" liveness check was used after that, but only proves the
// process exists, not that it has reached bind()/listen() yet -- under
// enough load/latency the local relay could dial in before the remote
// listener was actually up, getting connection-refused and tearing down the
// client connection it was serving. Reading the socket table directly avoids
// both problems: it's a passive read of kernel state, so it can never
// trigger the helper's accept-and-dial machinery, and it only succeeds once
// the socket is genuinely listening.
//
// Checking the port alone (without -p/pid) isn't enough either: an
// uncleanly-killed prior run (OOM, host reboot, kill -9) can leave a stale
// helper still listening on this same deterministically-computed port, or a
// second, concurrent invocation for a different source domain can land on
// it too (the run-lock is keyed only by source domain, not by port). If the
// helper just started fails to bind because of that and exits immediately,
// checking for "anything" listening would still see the stale/foreign
// process and report success -- silently relaying this sync's traffic
// through a helper wired up for an entirely different job's -connect
// target. Matching pidFile's own PID against the socket's owning process
// (ss -p) confirms it's genuinely this run's helper that's listening.
func waitForRemoteListening(ctx context.Context, client *remotessh.Client, bridgePort int, pidFile string, timeout time.Duration) error {
	check := BuildReadinessCheckCommand(bridgePort, pidFile)
	deadline := time.Now().Add(timeout)
	for {
		if _, err := client.Run(ctx, check); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("port %d is not listening under the helper process just started (pid file %s) after %s -- a different, stale process may already be holding this port", bridgePort, pidFile, timeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
