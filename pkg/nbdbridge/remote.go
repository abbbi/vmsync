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
)

// StartRemote launches a backgrounded socat+zstd bridge on client, listening
// on 127.0.0.1:bridgePort and forwarding (decompressed) to the real NBD
// export already listening on 127.0.0.1:realPort on the same host. It
// returns a stop command the caller should append to its own teardown list
// -- this function does not manage the bridge's lifecycle itself.
func StartRemote(ctx context.Context, client *remotessh.Client, bridgePort, realPort int, cfg Config) (stopCmd string, err error) {
	pidFile := path.Join("/tmp", fmt.Sprintf("vmsync-bridge-%d.pid", bridgePort))
	logFile := path.Join("/tmp", fmt.Sprintf("vmsync-bridge-%d.log", bridgePort))

	startCmd := BuildStartCommand(cfg, bridgePort, realPort, pidFile, logFile)
	if out, err := client.Run(ctx, startCmd); err != nil {
		return "", fmt.Errorf("start remote nbd bridge on port %d: %w: %s", bridgePort, err, out)
	}

	return BuildStopCommand(pidFile, logFile), nil
}

// WaitForRemoteReady polls until the bridge's listener on 127.0.0.1:bridgePort
// accepts a connection, dialed through client's SSH tunnel (the bridge port
// is not expected to be reachable any other way).
func WaitForRemoteReady(client *remotessh.Client, bridgePort int, timeout time.Duration) error {
	addr := fmt.Sprintf("127.0.0.1:%d", bridgePort)
	deadline := time.Now().Add(timeout)
	for {
		conn, err := client.DialTCP(addr)
		if err == nil {
			conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("nbd bridge not ready on %s: %w", addr, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
