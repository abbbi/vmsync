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
	"fmt"
	"regexp"
	"strings"
)

// Config describes how NBD traffic should be bridged/compressed between
// hosts. The zero value disables bridging entirely, which is the required
// default: vmsync's core sync path must work unchanged when none of these
// options are used. --compress and --netbuffer are independent: either can be
// used alone, or both together.
type Config struct {
	Compress       bool
	CompressLevel  int
	NetBufferBlock string // e.g. "64k"; empty means netbuffer is disabled
	NetBufferSize  string // e.g. "512M"
	HelperPath     string // remote path to the vmsync-bridge-helper binary
	// UseSSH, when false (the default), bridges directly: vmsync-bridge-helper
	// binds its listener to all interfaces instead of loopback, and the local
	// relay dials the remote host's real, routable address over plain TCP,
	// bypassing SSH entirely for the bridged connection. This has no
	// encryption or authentication of its own -- only appropriate when the
	// network path between the two hosts is already secured some other way
	// (e.g. a VPN/WireGuard tunnel) -- and requires the bridge port range to
	// actually be reachable between the two hosts (firewall/routing), which
	// vmsync does not itself verify. Set UseSSH to true to instead route the
	// bridged connection through the existing SSH connection as an encrypted
	// tunnel (an SSH direct-tcpip channel), at the cost of SSH's own
	// channel-level flow-control overhead.
	UseSSH bool
}

// NetBufferEnabled reports whether --netbuffer was set.
func (c Config) NetBufferEnabled() bool {
	return c.NetBufferBlock != "" || c.NetBufferSize != ""
}

// Enabled reports whether any bridging is requested at all.
func (c Config) Enabled() bool {
	return c.Compress || c.NetBufferEnabled()
}

// ValidateCompressLevel checks the --compress-level value is one zstd
// accepts.
func ValidateCompressLevel(level int) error {
	if level < 1 || level > 19 {
		return fmt.Errorf("--compress-level must be between 1 and 19, got %d", level)
	}
	return nil
}

var netbufferSizeRe = regexp.MustCompile(`(?i)^[0-9]+[bkmgt]?$`)

// ParseNetBufferSpec parses a --netbuffer value of the form
// "<blocksize>,<buffersize>" into its two block-size/limit-size arguments.
// An empty spec is valid and means netbuffer is disabled.
func ParseNetBufferSpec(spec string) (block, size string, err error) {
	if spec == "" {
		return "", "", nil
	}
	parts := strings.SplitN(spec, ",", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("--netbuffer must be of the form <blocksize>,<buffersize> (e.g. 64k,512M), got %q", spec)
	}
	if !netbufferSizeRe.MatchString(parts[0]) {
		return "", "", fmt.Errorf("--netbuffer block size %q is invalid (expected a number optionally followed by b/k/m/g/t)", parts[0])
	}
	if !netbufferSizeRe.MatchString(parts[1]) {
		return "", "", fmt.Errorf("--netbuffer buffer size %q is invalid (expected a number optionally followed by b/k/m/g/t)", parts[1])
	}
	return parts[0], parts[1], nil
}
