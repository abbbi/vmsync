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
// options are used. --compress and --mbuffer are independent: either can be
// used alone, or both together.
type Config struct {
	Compress      bool
	CompressLevel int
	MbufferBlock  string // e.g. "64k"; empty means mbuffer is disabled
	MbufferSize   string // e.g. "512M"
}

// MbufferEnabled reports whether --mbuffer was set.
func (c Config) MbufferEnabled() bool {
	return c.MbufferBlock != "" || c.MbufferSize != ""
}

// Enabled reports whether any bridging is requested at all.
func (c Config) Enabled() bool {
	return c.Compress || c.MbufferEnabled()
}

// ValidateCompressLevel checks the --compress-level value is one zstd
// accepts.
func ValidateCompressLevel(level int) error {
	if level < 1 || level > 19 {
		return fmt.Errorf("--compress-level must be between 1 and 19, got %d", level)
	}
	return nil
}

var mbufferSizeRe = regexp.MustCompile(`(?i)^[0-9]+[bkmgt]?$`)

// ParseMbufferSpec parses a --mbuffer value of the form
// "<blocksize>,<buffersize>" (e.g. "64k,512M") into its two mbuffer -s/-m
// arguments. An empty spec is valid and means mbuffer is disabled.
func ParseMbufferSpec(spec string) (block, size string, err error) {
	if spec == "" {
		return "", "", nil
	}
	parts := strings.SplitN(spec, ",", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("--mbuffer must be of the form <blocksize>,<buffersize> (e.g. 64k,512M), got %q", spec)
	}
	if !mbufferSizeRe.MatchString(parts[0]) {
		return "", "", fmt.Errorf("--mbuffer block size %q is invalid (expected a number optionally followed by b/k/m/g/t)", parts[0])
	}
	if !mbufferSizeRe.MatchString(parts[1]) {
		return "", "", fmt.Errorf("--mbuffer buffer size %q is invalid (expected a number optionally followed by b/k/m/g/t)", parts[1])
	}
	return parts[0], parts[1], nil
}
