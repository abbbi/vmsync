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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// CommandHolder starts a remote command and keeps it running until the
// returned handle is closed. Satisfied by *remotessh.Client.
//
// An interface rather than the concrete type so this package does not
// depend on remotessh, matching how RemotePathExists already takes the
// runner it needs.
//
// The handle is a WriteCloser rather than a Closer because the lock's liveness
// travels over it: writes are the heartbeat that keeps a leased lock alive, and
// a failing write is how this side learns the lock is gone.
type CommandHolder interface {
	HoldCommand(ctx context.Context, command, readyLine string) (io.WriteCloser, error)
}

// Markers the remote side prints. Distinct per failure so a caller can tell
// "somebody else holds this" -- an ordinary, expected outcome -- from "this host
// cannot lock at all", which is a misconfiguration.
//
// Exported because the lock has two implementations that must be
// indistinguishable to the caller: the POSIX shell below, and
// vmsync-bridge-helper's leased mode. One set of strings is what lets
// AcquireRemoteRunLock choose between them without its error handling caring
// which one answered.
const (
	RemoteLockReady    = "VMSYNC-LOCK-ACQUIRED"
	RemoteLockBusy     = "VMSYNC-LOCK-BUSY"
	RemoteLockNoFlock  = "VMSYNC-LOCK-NO-FLOCK"
	RemoteLockNoDir    = "VMSYNC-LOCK-NO-DIR"
	RemoteLockNoCreate = "VMSYNC-LOCK-NO-CREATE"
)

// Exit codes the remote side uses per failure, shared for the same reason the
// markers are: the shell script and the helper have to be interchangeable.
const (
	RemoteLockExitNoFlock  = 3
	RemoteLockExitNoDir    = 4
	RemoteLockExitNoCreate = 5
	RemoteLockExitBusy     = 6
)

// AcquireRemoteRunLock takes the same advisory lock AcquireRunLock takes, but on
// a remote host over SSH.
//
// Two holders, tried in that order, and the choice is not cosmetic -- they differ
// in the one case that matters most:
//
//   - vmsync-bridge-helper, with -hold-run-lock, holds it under a LEASE. The
//     target releases it when this process stops beating, so a driver that loses
//     power or is cut off costs one lease before the replica can be promoted.
//   - a POSIX shell parked on `flock` + a reader holds it otherwise, on a target
//     with no usable helper. It is released when the SSH channel closes, which
//     covers a clean exit and a kill -- but NOT a partition or a power loss.
//     Those deliver nothing to the far end: the reader blocks on a stdin that
//     never closes, and sshd notices only when TCP keepalive gives up, roughly
//     two hours later. Everything protecting that replica queues behind the lock
//     for that whole window, including the promotion the replica exists for.
//
// The returned lock says which it got, so a caller can warn when it is the second
// and point at BreakRemoteRunLock. It also reports, through Lost, when this
// process can no longer prove it holds the lock at all -- which the gates before
// the commit and before the define refuse on, because a leased lock the target has
// released may already belong to a promotion there.
//
// Returns an error wrapping ErrLockHeld when another vmsync already holds it, so
// callers can treat contention as a clean skip rather than a sync failure, exactly
// as the local lock is treated.
func AcquireRemoteRunLock(ctx context.Context, h CommandHolder, dir, key string, opts RemoteLockOptions) (*RemoteRunLock, error) {
	path := RunLockPath(dir, key)
	lease := opts.Lease
	if lease <= 0 {
		lease = DefaultRemoteLockLease
	}

	// The leased holder first, because it is the only one that releases the lock
	// when this process dies without closing anything.
	if opts.HelperPath != "" {
		lock, err := holdViaHelper(ctx, h, path, key, lease, opts)
		if err == nil {
			return lock, nil
		}
		if errors.Is(err, ErrLockHeld) {
			// Somebody else holds it. The shell would say the same thing one
			// round trip later, and saying it twice would only delay the answer.
			return nil, err
		}
		// Anything else means this host cannot hold the lock this way -- no
		// helper, one too old to know the flag, a path that is not executable.
		// The shell below still excludes concurrent runs, which is the property
		// a sync cannot proceed without.
		opts.fellBack = err
	}

	lock, err := holdViaShell(ctx, h, path, dir, key, lease)
	if err != nil {
		return nil, err
	}
	lock.fellBackBecause = opts.fellBack
	return lock, nil
}

// holdViaHelper takes the lock as a lease held by vmsync-bridge-helper, which
// releases it on silence rather than on EOF.
func holdViaHelper(ctx context.Context, h CommandHolder, path, key string, lease time.Duration, opts RemoteLockOptions) (*RemoteRunLock, error) {
	stamp, err := json.Marshal(opts.Identity)
	if err != nil {
		return nil, fmt.Errorf("encode the run lock provenance: %w", err)
	}
	cmd := strings.Join([]string{
		ShQuote(opts.HelperPath),
		"-hold-run-lock", ShQuote(path),
		"-lock-lease", ShQuote(lease.String()),
		"-lock-stamp", ShQuote(string(stamp)),
	}, " ")

	handle, err := h.HoldCommand(ctx, cmd, RemoteLockReady)
	if err != nil {
		if strings.Contains(err.Error(), RemoteLockBusy) {
			return nil, fmt.Errorf("another vmsync is already working on %q on the target host (lock %s held): %w", key, path, ErrLockHeld)
		}
		return nil, fmt.Errorf("hold the target-side run lock with %s: %w", opts.HelperPath, err)
	}
	return startHeartbeat(handle, path, lease, true), nil
}

// holdViaShell takes the lock with POSIX flock in the login shell, for a target
// with no usable helper.
//
// One shell script, one round trip. Each failure prints its own marker before
// exiting so the error a caller sees names the actual cause instead of a generic
// non-zero exit.
func holdViaShell(ctx context.Context, h CommandHolder, path, dir, key string, lease time.Duration) (*RemoteRunLock, error) {
	q := ShQuote(path)
	script := strings.Join([]string{
		`command -v flock >/dev/null 2>&1 || { echo ` + RemoteLockNoFlock + `; exit ` + itoa(RemoteLockExitNoFlock) + `; }`,
		`mkdir -p ` + ShQuote(dir) + ` || { echo ` + RemoteLockNoDir + `; exit ` + itoa(RemoteLockExitNoDir) + `; }`,
		// Creatability is probed in a subshell before the redirection that
		// depends on it. POSIX lets a redirection failure on `exec` with no
		// command terminate a non-interactive shell, which dash does: the `||`
		// branch never runs there, so the marker below is unreachable and the
		// caller is left guessing at a bare non-zero exit.
		`( : >>` + q + ` ) 2>/dev/null || { echo ` + RemoteLockNoCreate + `; exit ` + itoa(RemoteLockExitNoCreate) + `; }`,
		// `exec 9>>` in the shell itself, so the lock belongs to the shell and
		// outlives the flock invocation. Append, never truncate: `>` truncates
		// BEFORE flock decides who wins, so a contender that then loses would
		// have blanked the provenance of the holder it lost to.
		`exec 9>>` + q,
		`flock -n 9 || { echo ` + RemoteLockBusy + `; exit ` + itoa(RemoteLockExitBusy) + `; }`,
		`echo ` + RemoteLockReady,
		// exec, so exactly one process holds fd 9 and the lock cannot outlive it
		// in a forked child. >/dev/null, because nothing drains this command's
		// stdout: a reader that echoed the heartbeat would fill the channel
		// window and wedge both ends.
		`exec cat >/dev/null`,
	}, "\n")

	handle, err := h.HoldCommand(ctx, script, RemoteLockReady)
	if err == nil {
		return startHeartbeat(handle, path, lease, false), nil
	}

	msg := err.Error()
	switch {
	case strings.Contains(msg, RemoteLockBusy):
		return nil, fmt.Errorf("another vmsync is already working on %q on the target host (lock %s held): %w", key, path, ErrLockHeld)
	case strings.Contains(msg, RemoteLockNoFlock):
		return nil, fmt.Errorf("the target host has no flock(1), so concurrent runs against %q cannot be excluded there -- install util-linux", key)
	case strings.Contains(msg, RemoteLockNoDir):
		return nil, fmt.Errorf("could not create the lock directory %s on the target host", dir)
	case strings.Contains(msg, RemoteLockNoCreate):
		return nil, fmt.Errorf("could not create the lock file %s on the target host", path)
	default:
		return nil, fmt.Errorf("take the target-side run lock for %q: %w", key, err)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// RemoteLockOptions selects how the target-side lock is held.
type RemoteLockOptions struct {
	// HelperPath is vmsync-bridge-helper on the target. Empty, or a helper too
	// old to know -hold-run-lock, means the POSIX shell fallback -- which
	// excludes concurrent runs but cannot release the lock when this process
	// dies without closing the connection.
	HelperPath string
	// Lease is how long the far end waits before releasing a leased lock.
	// Zero means DefaultRemoteLockLease.
	Lease time.Duration
	// Identity is the provenance recorded in the lock file, for whoever has to
	// decide whether breaking the lock is safe. Only a leased lock records it:
	// the shell fallback cannot describe its own process portably, and a
	// partial record is worse than none, because BreakRemoteRunLock would then
	// be deciding from fields it cannot trust.
	Identity RunLockIdentity

	// fellBack carries why the leased holder was unusable, from the attempt to
	// the returned lock.
	fellBack error
}
