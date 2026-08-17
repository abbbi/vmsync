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

// Package netbuffer parses the --netbuffer/-netbuffer CLI flag value shared
// by vmsync (pkg/nbdbridge) and cmd/vmsync-bridge-helper. It exists as its
// own small package, depending on nothing beyond pkg/zstdrelay and the
// standard library, specifically so cmd/vmsync-bridge-helper -- a minimal,
// standalone binary deployed to arbitrary remote hosts -- can share this
// exact, tested parsing logic without also importing pkg/nbdbridge's own
// pkg/remotessh (SSH client) dependency, which that binary deliberately
// avoids. Before this package existed, the helper carried its own
// hand-duplicated copy of this logic, which had already drifted: it was
// missing the zero-buffer-size rejection below, so a "-netbuffer=X,0" would
// silently deadlock zstdrelay.BoundedBuffer.Write forever on the first
// relayed byte instead of failing at startup like it does through
// pkg/nbdbridge's own path.
package netbuffer

import (
	"fmt"
	"regexp"
	"strings"

	"vmsync/pkg/zstdrelay"
)

var sizeRe = regexp.MustCompile(`(?i)^[0-9]+[bkmgt]?$`)

// ParseSpec parses a --netbuffer/-netbuffer value of the form
// "<blocksize>,<buffersize>" into its two block-size/limit-size arguments.
// An empty spec is valid and means netbuffer is disabled.
func ParseSpec(spec string) (block, size string, err error) {
	if spec == "" {
		return "", "", nil
	}
	parts := strings.SplitN(spec, ",", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("--netbuffer must be of the form <blocksize>,<buffersize> (e.g. 64k,512M), got %q", spec)
	}
	if !sizeRe.MatchString(parts[0]) {
		return "", "", fmt.Errorf("--netbuffer block size %q is invalid (expected a number optionally followed by b/k/m/g/t)", parts[0])
	}
	if !sizeRe.MatchString(parts[1]) {
		return "", "", fmt.Errorf("--netbuffer buffer size %q is invalid (expected a number optionally followed by b/k/m/g/t)", parts[1])
	}
	// A zero-byte buffer deadlocks BoundedBuffer.Write forever: it blocks
	// while curBytes >= maxBytes, which is trivially true from the first
	// byte when maxBytes is 0, and nothing can ever be dequeued to clear
	// it. Reject it here so this is a normal startup error instead of a
	// silent, permanent hang.
	if bufBytes, err := zstdrelay.ParseByteSize(parts[1]); err == nil && bufBytes <= 0 {
		return "", "", fmt.Errorf("wrong buffer size")
	}
	return parts[0], parts[1], nil
}
