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

// Package atomicjson writes JSON files that a concurrent reader, or a power
// loss, can never catch half-written.
package atomicjson

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Write writes to a temporary file in the same directory and
// renames it into place.
//
// The rename is the point: the config cache is what the agent falls back on
// when the UI is unreachable, so a crash or a full disk partway through a
// plain write would replace a working fallback with a truncated file at
// exactly the moment it is needed. rename(2) within a directory is atomic,
// so a reader sees either the old contents or the new ones.
//
// Its own package rather than a function in vmsync-agent, where it was
// written, because vmsync writes a run-result file the agent reads and both
// ends need the same guarantee -- two copies of a durable-write idiom is one
// copy that quietly loses a step.
//
// And its own package rather than pkg/util, which is Linux-only (flock), so
// that a helper depending on nothing but os and encoding/json stays testable
// wherever it is edited.
func Write(path string, value any, perm os.FileMode) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state dir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("create temp file for %s: %w", path, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename below succeeds

	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp file for %s: %w", path, err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	// Flush to disk before the rename. Without this the rename can be
	// durable while the contents are not, leaving a valid-looking but empty
	// file after a power loss -- the same failure the atomic write is here
	// to prevent.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file for %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("install %s: %w", path, err)
	}
	// And flush the DIRECTORY, so the rename itself is durable.
	//
	// Syncing the temp file above makes its CONTENTS survive a power loss; it
	// says nothing about the directory entry that points at them. Without this
	// the rename can be lost while the data is intact, and the previous file
	// reappears -- which for operations.json means an operation that already
	// RAN comes back marked as never seen, and Seen() lets it execute a second
	// time. For a promote or a restore, twice is not a retry.
	//
	// This is the half of the durable-rename idiom that is easy to leave out
	// because everything works until the machine loses power at the wrong
	// moment, and then works again afterwards.
	//
	// Its result is deliberately IGNORED, and that is not laziness. By this
	// point the data is written, flushed and renamed -- the write has
	// succeeded. A failure here means only that the directory entry's
	// durability could not be confirmed, which leaves us exactly where this
	// function stood before the fsync was added. Returning an error instead
	// would tell the caller the write did not happen, and callers act on that:
	// operationLedger.Begin refuses to execute, and fenceLedger's caller
	// proceeds unrecorded. Trading an availability outage for an unobtainable
	// durability guarantee is the wrong way round.
	//
	// Not hypothetical: POSIX permits fsync on a directory descriptor to
	// refuse, and platforms differ on WHICH error they give for it -- Windows
	// returns access-denied rather than EINVAL, so an errno allowlist here
	// silently becomes an allowlist of platforms.
	//
	// Called through syncDir rather than SyncDir directly so a test can stage
	// the refusal. On Linux -- the deployment target -- fsync on a directory
	// succeeds, so a test that merely calls Write proves nothing about this
	// branch: without the seam, deleting the "_ =" and returning the error
	// keeps the whole suite green while turning a durability non-guarantee
	// into an availability outage. That edit is exactly what a linter
	// complaining about an unchecked error would propose.
	_ = syncDir(dir)
	return nil
}

// syncDir is SyncDir, indirected only so a test can make it fail. Never
// reassigned outside a test.
var syncDir = SyncDir

// SyncDir fsyncs a directory so a rename into it is durable.
//
// Returns its error for testability and for any future caller that can
// genuinely act on one; Write deliberately cannot -- see above.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open state dir %s to flush it: %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("flush state dir %s: %w", dir, err)
	}
	return nil
}
