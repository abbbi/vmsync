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

// StartRemote launches a backgrounded socat+zstd bridge on client, listening
// on 127.0.0.1:bridgePort and forwarding (decompressed) to the real NBD
// export already listening on 127.0.0.1:realPort on the same host. It waits
// for the bridge process to actually be running before returning, and
// returns a stop command the caller should append to its own teardown list
// -- this function does not manage the bridge's lifecycle itself beyond that.
func StartRemote(ctx context.Context, client *remotessh.Client, bridgePort, realPort int, cfg Config) (stopCmd string, err error) {
	if out, err := client.Run(ctx, "mkdir -p "+util.ShQuote(bridgeStateDir)); err != nil {
		return "", fmt.Errorf("create bridge state dir %s: %w: %s", bridgeStateDir, err, out)
	}

	pidFile := path.Join(bridgeStateDir, fmt.Sprintf("vmsync-bridge-%d.pid", bridgePort))
	logFile := path.Join(bridgeStateDir, fmt.Sprintf("vmsync-bridge-%d.log", bridgePort))

	startCmd := BuildStartCommand(cfg, bridgePort, realPort, pidFile, logFile)
	if out, err := client.Run(ctx, startCmd); err != nil {
		return "", fmt.Errorf("start remote nbd bridge on port %d: %w: %s", bridgePort, err, out)
	}

	if err := waitForRemoteListening(ctx, client, bridgePort, 10*time.Second); err != nil {
		return "", fmt.Errorf("remote nbd bridge on port %d did not start: %w", bridgePort, err)
	}

	return BuildStopCommand(pidFile, logFile), nil
}

// waitForRemoteListening polls the remote host's own socket table (via "ss")
// until bridgePort is actually in LISTEN state, rather than merely checking
// that the process exists ("kill -0") or attempting a real TCP connection.
//
// A real TCP connect-and-close probe was used here originally and caused a
// deadlock: socat's "fork" option treats ANY accepted connection as a real
// one and immediately runs the full decompress/forward/compress pipeline for
// it, including opening a genuine connection to the real NBD export --
// which, by qemu-nbd's default --shared=1, only allows a single simultaneous
// client. A disposable readiness probe occupied that one slot until its side
// of the pipeline fully unwound, racing against (and usually losing to) the
// real data connection that immediately followed it, wedging the whole sync.
//
// A "kill -0" liveness check was used after that, but only proves the
// process exists, not that it has reached bind()/listen() yet -- under
// enough load/latency the local relay could dial in before the remote
// listener was actually up, getting connection-refused and tearing down the
// client connection it was serving. Reading the socket table directly avoids
// both problems: it's a passive read of kernel state, so it can never
// trigger socat's fork machinery, and it only succeeds once the socket is
// genuinely listening.
func waitForRemoteListening(ctx context.Context, client *remotessh.Client, bridgePort int, timeout time.Duration) error {
	filter := fmt.Sprintf("( sport = :%d )", bridgePort)
	check := "ss -Htln " + util.ShQuote(filter) + " | grep -q ."
	deadline := time.Now().Add(timeout)
	for {
		if _, err := client.Run(ctx, check); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("port %d is not listening after %s", bridgePort, timeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
