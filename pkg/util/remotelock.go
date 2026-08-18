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
	"context"
	"fmt"
	"io"
	"strings"
)

// CommandHolder starts a remote command and keeps it running until the
// returned Closer is closed. Satisfied by *remotessh.Client.
//
// An interface rather than the concrete type so this package does not
// depend on remotessh, matching how RemotePathExists already takes the
// runner it needs.
type CommandHolder interface {
	HoldCommand(ctx context.Context, command, readyLine string) (io.Closer, error)
}

// Markers the remote script prints. Distinct per failure so a caller can
// tell "somebody else holds this" -- an ordinary, expected outcome -- from
// "this host cannot lock at all", which is a misconfiguration.
const (
	remoteLockReady    = "VMSYNC-LOCK-ACQUIRED"
	remoteLockBusy     = "VMSYNC-LOCK-BUSY"
	remoteLockNoFlock  = "VMSYNC-LOCK-NO-FLOCK"
	remoteLockNoDir    = "VMSYNC-LOCK-NO-DIR"
	remoteLockNoCreate = "VMSYNC-LOCK-NO-CREATE"
)

// AcquireRemoteRunLock takes the same advisory lock AcquireRunLock takes,
// but on a remote host over SSH.
//
// The lock is held by a remote `flock` process that then blocks reading its
// stdin, and released when that process exits. That gives the property a
// lock across a network most needs: it is released by the SSH connection
// dropping, by this process being killed, and by the network partitioning,
// because all three deliver EOF to that stdin. There is no lease to renew,
// no timeout to tune, and -- critically -- no stale lock to break by hand
// after a crash, which is the failure mode that makes operators disable
// locking entirely.
//
// Returns an error wrapping ErrLockHeld when another vmsync already holds
// it, so callers can treat contention as a clean skip rather than a sync
// failure, exactly as the local lock is treated.
func AcquireRemoteRunLock(ctx context.Context, h CommandHolder, dir, key string) (io.Closer, error) {
	path := RunLockPath(dir, key)

	// One shell script, one round trip. Each failure prints its own marker
	// before exiting so the error a caller sees names the actual cause
	// instead of a generic non-zero exit.
	//
	// `exec 9>` opens the descriptor in the shell itself rather than in a
	// child, so the lock belongs to the shell and outlives the flock
	// invocation; `cat` then holds that shell alive until stdin closes.
	script := strings.Join([]string{
		`command -v flock >/dev/null 2>&1 || { echo ` + remoteLockNoFlock + `; exit 3; }`,
		`mkdir -p ` + ShQuote(dir) + ` || { echo ` + remoteLockNoDir + `; exit 4; }`,
		`exec 9>` + ShQuote(path) + ` || { echo ` + remoteLockNoCreate + `; exit 5; }`,
		`flock -n 9 || { echo ` + remoteLockBusy + `; exit 6; }`,
		`echo ` + remoteLockReady,
		`cat`,
	}, "\n")

	closer, err := h.HoldCommand(ctx, script, remoteLockReady)
	if err == nil {
		return closer, nil
	}

	msg := err.Error()
	switch {
	case strings.Contains(msg, remoteLockBusy):
		return nil, fmt.Errorf("another vmsync is already working on %q on the target host (lock %s held): %w", key, path, ErrLockHeld)
	case strings.Contains(msg, remoteLockNoFlock):
		return nil, fmt.Errorf("the target host has no flock(1), so concurrent runs against %q cannot be excluded there -- install util-linux", key)
	case strings.Contains(msg, remoteLockNoDir):
		return nil, fmt.Errorf("could not create the lock directory %s on the target host", dir)
	case strings.Contains(msg, remoteLockNoCreate):
		return nil, fmt.Errorf("could not create the lock file %s on the target host", path)
	default:
		return nil, fmt.Errorf("take the target-side run lock for %q: %w", key, err)
	}
}
