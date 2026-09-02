/*
	Copyright (C) 2026  Orsiris de Jong <ozy@netpower.fr>

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

// Package version holds the single version string shared by cmd/vmsync and
// cmd/vmsync-bridge-helper. It exists as its own package specifically so
// both binaries are built from the exact same constant -- vmsync-bridge-helper
// is deployed to remote hosts manually, ahead of time, by the user (vmsync
// never uploads it), so its version can silently drift from the vmsync binary
// actually driving it. pkg/nbdbridge's CheckRemote compares the two before a
// sync starts, and having them both read from here is what makes that
// comparison meaningful -- if the two binaries kept separate version
// constants, forgetting to update one on a release would defeat the whole
// point of checking.
package version

const Version = "0.50-2026090201-beta"
