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
	"strconv"

	"vmsync/pkg/netbuffer"
	"vmsync/pkg/streamrelay"
)

// Config describes how NBD traffic should be bridged/compressed between
// hosts. The zero value disables bridging entirely, which is the required
// default: vmsync's core sync path must work unchanged when none of these
// options are used. --compress and --netbuffer are independent: either can be
// used alone, or both together.
type Config struct {
	Compress bool
	// CompressLevel's accepted values depend on CompressAlgo: a traditional
	// numeric zstd level ("1"-"19") for "zstd", or one of "default"/
	// "better"/"best" for "s2" -- see streamrelay.NewEncoder.
	CompressLevel  string
	CompressAlgo   string // "zstd" (default) or "s2" -- see streamrelay.Algo
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

// ValidateCompressLevel checks the --compress-level value is valid for the
// given compression algorithm (vmsync's -compress=zstd|s2 flag value): zstd
// takes a traditional numeric level ("1"-"19"), while S2 has no numeric
// levels at all -- only "default" (fastest, S2's own default), "better", or
// "best" (see streamrelay.NewEncoder).
func ValidateCompressLevel(algo, level string) error {
	a, err := streamrelay.ParseAlgo(algo)
	if err != nil {
		return err
	}
	if a == streamrelay.AlgoS2 {
		switch level {
		case "default", "better", "best":
			return nil
		default:
			return fmt.Errorf("--compress-level must be \"default\", \"better\", or \"best\" when -compress=s2, got %q", level)
		}
	}
	n, err := strconv.Atoi(level)
	if err != nil {
		return fmt.Errorf("--compress-level must be a number between 1 and 19 for -compress=zstd, got %q", level)
	}
	if n < 1 || n > 19 {
		return fmt.Errorf("--compress-level must be between 1 and 19, got %d", n)
	}
	return nil
}

// ValidateCompressAlgo checks vmsync's -compress=<value> is one
// pkg/streamrelay recognizes.
func ValidateCompressAlgo(algo string) error {
	_, err := streamrelay.ParseAlgo(algo)
	return err
}

// ParseNetBufferSpec parses a --netbuffer value of the form
// "<blocksize>,<buffersize>" into its two block-size/limit-size arguments.
// An empty spec is valid and means netbuffer is disabled. Thin wrapper
// around pkg/netbuffer.ParseSpec -- the actual logic lives there so
// cmd/vmsync-bridge-helper can share it directly, without importing this
// package (and therefore pkg/remotessh, its SSH client dependency, which
// that otherwise minimal, standalone binary avoids on purpose).
func ParseNetBufferSpec(spec string) (block, size string, err error) {
	return netbuffer.ParseSpec(spec)
}
