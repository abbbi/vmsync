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
	"strings"

	"vmsync/pkg/remotessh"
	"vmsync/pkg/util"
	"vmsync/pkg/version"
)

// CheckLocal verifies the tools this bridge needs on the machine running
// vmsync are present. Compression and buffering are now done natively in Go
// (pkg/streamrelay), so there is no local external-binary dependency at all --
// this always succeeds. Kept as a function (rather than removed outright) so
// call sites don't need to change if that ever stops being true.
func CheckLocal(cfg Config) error {
	return nil
}

// CheckRemote verifies the remote side of the bridge is usable on host,
// reached through client. It is a no-op when bridging isn't requested.
func CheckRemote(ctx context.Context, client *remotessh.Client, cfg Config, host string) error {
	if !cfg.Enabled() {
		return nil
	}

	if out, err := client.Run(ctx, "test -x "+util.ShQuote(cfg.HelperPath)); err != nil {
		return fmt.Errorf("vmsync-bridge-helper not found (or not executable) at %s on %s: %w: %s\n"+
			"build cmd/vmsync-bridge-helper and deploy it there yourself, or pass -bridge-helper-path "+
			"to point at wherever you've placed it", cfg.HelperPath, host, err, out)
	}

	// vmsync-bridge-helper is deployed to host manually, ahead of time, by
	// the user -- vmsync never uploads it (see its own doc comment) -- so
	// its version can silently drift from this vmsync binary's, with no
	// natural point at which that would otherwise surface: an out-of-date
	// helper still starts, still accepts connections, and any protocol or
	// flag-handling difference between versions would only show up as a
	// confusing failure (or worse, a silent one) deep inside a running
	// sync. Check it up front, before any sync work starts, the same way
	// the executability check above already does for a missing binary.
	helperVersion, err := client.Run(ctx, util.ShQuote(cfg.HelperPath)+" -version")
	if err != nil {
		return fmt.Errorf("unable to determine vmsync-bridge-helper version at %s on %s: %w: %s", cfg.HelperPath, host, err, helperVersion)
	}
	helperVersion = strings.TrimSpace(helperVersion)
	if helperVersion != version.Version {
		return fmt.Errorf("vmsync-bridge-helper at %s on %s is version %q, but this vmsync is version %q -- "+
			"rebuild and redeploy vmsync-bridge-helper so both match before syncing", cfg.HelperPath, host, helperVersion, version.Version)
	}

	// Bridging bypasses SSH tunneling for the bridged connection entirely by
	// default (see Config.UseSSH), so the SSH-port-forwarding requirement
	// below only applies when UseSSH is explicitly set. There's no way to
	// pre-check direct network reachability between the two hosts here (the
	// remote bridge port isn't listening yet at this point), so that failure
	// mode surfaces naturally, with a clear error, the first time the local
	// relay actually tries to dial it.
	if !cfg.UseSSH {
		return nil
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
