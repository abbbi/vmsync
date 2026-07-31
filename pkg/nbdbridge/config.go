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

import "fmt"

// Config describes how NBD traffic should be bridged/compressed between
// hosts. The zero value disables bridging entirely, which is the required
// default: vmsync's core sync path must work unchanged when none of these
// options are used.
type Config struct {
	Compress      bool
	CompressLevel int
}

// Enabled reports whether any bridging is requested at all.
func (c Config) Enabled() bool {
	return c.Compress
}

// ValidateCompressLevel checks the --compress-level value is one zstd
// accepts.
func ValidateCompressLevel(level int) error {
	if level < 1 || level > 19 {
		return fmt.Errorf("--compress-level must be between 1 and 19, got %d", level)
	}
	return nil
}
