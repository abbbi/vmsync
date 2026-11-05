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
// Split out of CheckRemote because SEVERAL callers need the same two
// questions answered and want different things done about the answer:
//
//   - CheckRemote refuses. The relay IS this binary, so no flag stands in for
//     it and there is no way past the refusal except not bridging.
//   - run()'s checksum gate refuses too, but it HAS a way past it
//     (-no-checksum), so it is worded separately -- see helperMissingRefusal
//     in cmd/vmsync.
//   - target-command supervision and the leased target run lock read the same
//     answer and merely degrade, loudly, with a working substitute each.
//
// So this function reports and never fails, and every caller decides. What it
// must not do is assume which of those three is asking.
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

// requestedBy names the flag that made the relay a requirement, so a refusal
// can say which of the operator's own flags needs the helper rather than
// leaving them to work it out from a list of features.
//
// Enabled() is exactly these two, so there is no third case and no empty one:
// CheckRemote has already returned nil when neither is set, and this is
// unreachable from anywhere else.
func requestedBy(c Config) string {
	switch {
	case c.Compress && c.NetBufferEnabled():
		return "-compress and -netbuffer"
	case c.Compress:
		return "-compress"
	default:
		return "-netbuffer"
	}
}

// relayHelperVerdict turns a probe result into CheckRemote's answer: nil for a
// helper a bridged run can use, and otherwise what to say about it.
//
// The GATE is here rather than at the call site so that "a helper present but
// unusable is still refused" is a thing a test can state. Usable, not Present,
// is the condition: a helper one build old starts, listens and relays, and
// relays wrongly, which is the case this whole probe exists for. Gating on
// Present instead would compile, pass a test that only ever injects an absent
// helper, and accept exactly the pairing ProbeHelper was written to reject.
//
// Two refusals, one shape, because the operator's next move differs by one
// word -- deploy, or redeploy -- and everything else about them is identical,
// including the two things both wordings used to leave out: WHICH of the
// operator's own flags needs the helper, and that not passing that flag is
// the other way on. Saying only "deploy this binary" is what made the
// parallel refusal in cmd/vmsync/helperrefusal.go read as "vmsync cannot be
// used without the helper".
//
// -no-checksum is named and ruled out on purpose. It is the first thing an
// operator reaches for after meeting that other refusal, it is evaluated
// BEFORE bridging in run()'s switch, and it does nothing for this one.
func relayHelperVerdict(st HelperStatus, want, host string) error {
	if st.Usable {
		return nil
	}
	if !st.Present {
		// ONE LINE, as every other refusal in the tree is. This carried an
		// embedded newline until 2026-11-05, which put the half naming the
		// remedy on a line of its own with no timestamp and no level -- so
		// trace's key=value framing broke across it, anything shipping the
		// log either dropped that half or filed it as a separate event, and a
		// grep for the error vmsync died on found only the half that says
		// what is missing and not the half that says what to do.
		return fmt.Errorf("vmsync-bridge-helper %s -- "+
			"%s needs it on %s: the NBD relay that compresses and buffers the wire IS that binary, and nothing here stands in for it -- -no-checksum does not get past this one. "+
			"Build cmd/vmsync-bridge-helper and deploy it there yourself, or pass -bridge-helper-path to point at wherever you've placed it; or drop %s to sync without the relay",
			st.Reason, want, host, want)
	}
	// An out-of-date helper still starts and still accepts connections, so any
	// protocol or flag-handling difference between versions would otherwise
	// only show up as a confusing failure (or worse, a silent one) deep inside
	// a running sync. Fail up front instead.
	return fmt.Errorf("vmsync-bridge-helper %s -- %s needs it on %s, and a version that does not match is refused exactly like one that is absent. "+
		"Rebuild and redeploy vmsync-bridge-helper there so both match before syncing; or drop %s to sync without the relay",
		st.Reason, want, host, want)
}

// CheckRemote verifies the remote side of the bridge is usable on host,
// reached through client. It is a no-op when bridging isn't requested.
func CheckRemote(ctx context.Context, client *remotessh.Client, cfg Config, host string) error {
	if !cfg.Enabled() {
		return nil
	}

	// Both failures were once checked inline here; they now come from
	// ProbeHelper so that the other two readers of the same two questions
	// cannot drift from this one, and both the gate and the wording live in
	// relayHelperVerdict so a test can hold them.
	if err := relayHelperVerdict(ProbeHelper(ctx, client, cfg, host), requestedBy(cfg), host); err != nil {
		return err
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
