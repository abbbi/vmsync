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

package util

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// AcquireRunLock takes an exclusive, non-blocking advisory lock (flock)
// scoped to key (e.g. the source domain name), so two vmsync invocations can
// never run concurrently for the same key -- regardless of what launched
// them (a wrapper script, cron, a manual test run) -- avoiding races like two
// processes both creating the same checkpoint name at once, or both writing
// the target domain's vmsync metadata at the same time.
//
// The returned file must be kept open (and closed, typically via defer) for
// as long as the lock should be held. Unlike a pidfile/mkdir-based lock, no
// explicit release/cleanup logic is needed even on a forced shutdown: the
// kernel releases a flock automatically when the holding process's file
// descriptor closes, for any reason -- normal exit, panic, or SIGKILL.
func AcquireRunLock(dir, key string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create lock dir %s: %w", dir, err)
	}
	safeKey := strings.NewReplacer("/", "_", " ", "_").Replace(key)
	path := filepath.Join(dir, safeKey+".lock")

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another vmsync is already running for %q (lock %s held): %w", key, path, err)
	}
	return f, nil
}
