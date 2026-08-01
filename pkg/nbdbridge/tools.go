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
	"os/exec"

	"vmsync/pkg/remotessh"
)

// CheckLocal verifies the tools this bridge needs on the machine running
// vmsync are present. It is a no-op when bridging isn't requested, so the
// core sync path never depends on zstd being installed.
func CheckLocal(cfg Config) error {
	if cfg.Compress {
		if _, err := exec.LookPath("zstd"); err != nil {
			return fmt.Errorf("--compress requires the 'zstd' binary to be installed locally: %w", err)
		}
	}
	if cfg.MbufferEnabled() {
		if _, err := exec.LookPath("mbuffer"); err != nil {
			return fmt.Errorf("--mbuffer requires the 'mbuffer' binary to be installed locally: %w", err)
		}
	}
	return nil
}

// CheckRemote verifies the tools this bridge needs are present on host,
// reached through client. It is a no-op when bridging isn't requested.
func CheckRemote(ctx context.Context, client *remotessh.Client, cfg Config, host string) error {
	if !cfg.Enabled() {
		return nil
	}
	tools := []string{"socat"}
	if cfg.Compress {
		tools = append(tools, "zstd")
	}
	if cfg.MbufferEnabled() {
		tools = append(tools, "mbuffer")
	}
	for _, tool := range tools {
		if out, err := client.Run(ctx, "command -v "+tool); err != nil {
			return fmt.Errorf("nbd bridge used for compression / buffering requires the %q binary to be installed on %s: %w: %s", tool, host, err, out)
		}
	}

	// The bridge relies entirely on SSH direct-tcpip channels (DialTCP) to
	// reach the remote listener -- separate from, and independent of, the
	// session channels client.Run just used above. A server can (and
	// commonly does, as a hardening measure) allow command execution while
	// rejecting port forwarding outright via "AllowTcpForwarding no", or
	// restrict it per-key via "no-port-forwarding" in authorized_keys.
	// Either shows up as "administratively prohibited (open failed)" only
	// once we actually try to open one -- check for it now, before any sync
	// work starts, rather than deep inside a running sync.
	conn, err := client.DialTCP(client.LoopbackSelfAddress())
	if err != nil {
		return fmt.Errorf("nbd bridge requires SSH port forwarding (direct-tcpip channels) to be permitted on %s, but a test connection failed: %w -- check 'AllowTcpForwarding' in sshd_config and for a 'no-port-forwarding' restriction on this key in authorized_keys", host, err)
	}
	conn.Close()

	return nil
}
