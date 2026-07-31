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

// StartRemote launches a backgrounded socat+zstd bridge on client, listening
// on 127.0.0.1:bridgePort and forwarding (decompressed) to the real NBD
// export already listening on 127.0.0.1:realPort on the same host. It waits
// for the bridge process to actually be running before returning, and
// returns a stop command the caller should append to its own teardown list
// -- this function does not manage the bridge's lifecycle itself beyond that.
func StartRemote(ctx context.Context, client *remotessh.Client, bridgePort, realPort int, cfg Config) (stopCmd string, err error) {
	pidFile := path.Join("/tmp", fmt.Sprintf("vmsync-bridge-%d.pid", bridgePort))
	logFile := path.Join("/tmp", fmt.Sprintf("vmsync-bridge-%d.log", bridgePort))

	startCmd := BuildStartCommand(cfg, bridgePort, realPort, pidFile, logFile)
	if out, err := client.Run(ctx, startCmd); err != nil {
		return "", fmt.Errorf("start remote nbd bridge on port %d: %w: %s", bridgePort, err, out)
	}

	if err := waitForRemoteProcess(ctx, client, pidFile, 10*time.Second); err != nil {
		return "", fmt.Errorf("remote nbd bridge on port %d did not start: %w", bridgePort, err)
	}

	return BuildStopCommand(pidFile, logFile), nil
}

// waitForRemoteProcess polls until the process recorded in pidFile is alive,
// using a plain liveness check ("kill -0") rather than a TCP connection.
//
// A real TCP connect-and-close probe was used here previously and caused a
// deadlock: socat's "fork" option treats ANY accepted connection as a real
// one and immediately runs the full decompress/forward/compress pipeline for
// it, including opening a genuine connection to the real NBD export --
// which, by qemu-nbd's default --shared=1, only allows a single simultaneous
// client. A disposable readiness probe occupied that one slot until its side
// of the pipeline fully unwound, racing against (and usually losing to) the
// real data connection that immediately followed it, wedging the whole sync.
func waitForRemoteProcess(ctx context.Context, client *remotessh.Client, pidFile string, timeout time.Duration) error {
	check := "kill -0 $(cat " + util.ShQuote(pidFile) + ") 2>/dev/null"
	deadline := time.Now().Add(timeout)
	for {
		if _, err := client.Run(ctx, check); err == nil {
			// The process exists; give socat a brief moment to reach its
			// bind()/listen() call. Startup has no heavy work before that
			// point, so this is a generous margin, not a race of its own.
			time.Sleep(150 * time.Millisecond)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("process tracked by %s did not start in time", pidFile)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
