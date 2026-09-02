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

// CommandRunner is the single method ProbeHelper needs, so it can be given
// a *remotessh.Client in production and a stub in a test. Narrow on purpose:
// CheckRemote below needs more of the client than this (DialTCP,
// LoopbackSelfAddress) and so still takes the concrete type.
type CommandRunner interface {
	Run(ctx context.Context, command string) (string, error)
}

// HelperStatus is what ProbeHelper found out about the target's
// vmsync-bridge-helper.
type HelperStatus struct {
	// Present means the binary exists at cfg.HelperPath and is executable.
	Present bool
	// Version is what the helper reported; empty when it could not be asked.
	Version string
	// Usable means Present AND the reported version matches this vmsync's.
	// The only field a caller deciding whether to use the helper should
	// consult.
	Usable bool
	// Reason explains !Usable in a form ready to drop into either a log line
	// or an error message. Empty when Usable.
	Reason string
}

// ProbeHelper reports whether the target's vmsync-bridge-helper is present,
// executable and version-matched, WITHOUT failing.
//
// Split out of CheckRemote because two callers need the same two questions
// answered and want opposite things done about the answer. CheckRemote is a
// precondition for bridging: no helper means the run cannot do what was
// asked, so it errors. The pre-commit integrity check is the other way
// round: it is on by default, so a missing helper must not break a sync
// nobody asked to bridge -- it just means no check, and a log line saying so.
//
// The version equality is not fussiness. The helper is deployed by hand and
// vmsync never uploads it, so its version drifts silently; and since the
// digest exchange is a wire format between the two binaries (see
// pkg/blockdigest), a mismatched pair is exactly the case whose digests would
// disagree everywhere and read as a corrupt replica. Refusing to use a
// mismatched helper at all is a stronger guard than detecting it afterwards.
func ProbeHelper(ctx context.Context, client CommandRunner, cfg Config, host string) HelperStatus {
	if out, err := client.Run(ctx, "test -x "+util.ShQuote(cfg.HelperPath)); err != nil {
		return HelperStatus{Reason: fmt.Sprintf("not found (or not executable) at %s on %s: %v: %s", cfg.HelperPath, host, err, out)}
	}
	helperVersion, err := client.Run(ctx, util.ShQuote(cfg.HelperPath)+" -version")
	if err != nil {
		return HelperStatus{
			Present: true,
			Reason:  fmt.Sprintf("version could not be determined at %s on %s: %v: %s", cfg.HelperPath, host, err, helperVersion),
		}
	}
	helperVersion = strings.TrimSpace(helperVersion)
	if helperVersion != version.Version {
		return HelperStatus{
			Present: true,
			Version: helperVersion,
			Reason:  fmt.Sprintf("at %s on %s is version %q, but this vmsync is version %q", cfg.HelperPath, host, helperVersion, version.Version),
		}
	}
	return HelperStatus{Present: true, Version: helperVersion, Usable: true}
}

// CheckRemote verifies the remote side of the bridge is usable on host,
// reached through client. It is a no-op when bridging isn't requested.
func CheckRemote(ctx context.Context, client *remotessh.Client, cfg Config, host string) error {
	if !cfg.Enabled() {
		return nil
	}

	// Both failures below were once checked inline here; they now come from
	// ProbeHelper so that the integrity check's own, non-fatal use of the
	// same two questions cannot drift from this one.
	st := ProbeHelper(ctx, client, cfg, host)
	if !st.Present {
		return fmt.Errorf("vmsync-bridge-helper %s\n"+
			"build cmd/vmsync-bridge-helper and deploy it there yourself, or pass -bridge-helper-path "+
			"to point at wherever you've placed it", st.Reason)
	}
	if !st.Usable {
		// An out-of-date helper still starts and still accepts connections,
		// so any protocol or flag-handling difference between versions would
		// otherwise only show up as a confusing failure (or worse, a silent
		// one) deep inside a running sync. Fail up front instead.
		return fmt.Errorf("vmsync-bridge-helper %s -- "+
			"rebuild and redeploy vmsync-bridge-helper so both match before syncing", st.Reason)
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
