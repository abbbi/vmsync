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
//
// Shared vocabulary, not identical output: two of these describe things only a
// shell can fail at, and the leased holder never prints them. What has to hold
// is that a marker means the same thing whoever printed it, and that the caller
// has somewhere to put every one of them.
const (
	RemoteLockReady    = "VMSYNC-LOCK-ACQUIRED"
	RemoteLockBusy     = "VMSYNC-LOCK-BUSY"
	RemoteLockNoFlock  = "VMSYNC-LOCK-NO-FLOCK"
	RemoteLockNoDir    = "VMSYNC-LOCK-NO-DIR"
	RemoteLockNoCreate = "VMSYNC-LOCK-NO-CREATE"

	// Shell-only, like RemoteLockNoFlock: the leased holder is a Go binary and
	// compares inodes with os.SameFile, so it has nothing to be unable to do.
	RemoteLockNoSameFile = "VMSYNC-LOCK-NO-SAMEFILE"
)

// Each marker above pairs with an exit status, and those live with every
// other status these binaries use, in exitcodes.go:
// RemoteLockExitNoFlock, RemoteLockExitNoDir, RemoteLockExitNoCreate,
// RemoteLockExitBusy, RemoteLockExitNoSameFile.

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

// shellHoldAttempts bounds the open-flock-compare loop, and is deliberately the
// same 10 as util.AcquireRunLock and the helper's lockTheRunLockFile: all three
// are the same loop against the same hazard, and a reader comparing them should
// not have to wonder whether a different number means something.
const shellHoldAttempts = 10

// shellHoldScript is the script holdViaShell parks on.
//
// Built by its own function so a test can RUN it rather than only read it.
// Everything that matters here is a property of what the shell does -- whether
// READY is withheld, whether the loop goes round, which marker comes out -- and
// a test that greps the text can only assert that the words are present. That
// is the same reason util.RunLockIsAtPath exists separately from
// AcquireRunLock.
//
// WHAT THE LOOP IS FOR. `exec 9>>path` then `flock -n 9` has a window between
// the open and the lock, and a second one between the lock and READY. If path
// is unlinked and recreated in either -- a tmpfiles.d rule clearing /run, an
// operator with rm, BreakRemoteRunLock -- then fd 9 holds an ORPHANED inode.
// flock on it succeeds and excludes nobody, because the file another vmsync
// will open at that path is a different inode entirely. READY would then be a
// lie for the whole length of the sync, and two runs would write one replica.
//
// So the lock is only announced once the file on fd 9 is the file path names,
// and otherwise the loop drops it and takes whatever is there now. Fails
// CLOSED: a comparison that cannot be made reads as "not the same file", which
// costs one iteration at acquisition and, after ten, a refusal the caller
// reports. That is the right policy here for the reason RunLockIsAtPath gives
// for itself -- at acquisition a wrong "yes" is unrecoverable and a wrong "no"
// costs a retry.
//
// `[ a -ef b ]` rather than `stat -c %i`, and this was measured rather than
// assumed. `-ef` compares device and inode, which is os.SameFile, and it is a
// shell builtin -- present in dash, bash, busybox ash and ksh, so it adds no
// dependency to a fallback path that exists precisely because the target has
// no helper binary. `stat -c %i /proc/$$/fd/9` looks like the obvious
// alternative and is WRONG: GNU stat does not dereference by default, so it
// reports the inode of the procfs SYMLINK, which never equals the inode of the
// file at path. A hold built on it would fail its own check on every attempt
// and never acquire at all.
func shellHoldScript(path, dir string) string {
	q := ShQuote(path)

	// The one expression that decides whether the lock on a descriptor is the
	// lock path names. A function of the descriptor rather than a literal
	// because the probe above checks its two ingredients separately, and a
	// reader comparing them should be able to see that this is assembled from
	// exactly those: `-ef`, and a /proc descriptor path.
	sameFile := func(fd string) string {
		return "[ " + q + " -ef /proc/$$/fd/" + fd + " ]"
	}

	return strings.Join([]string{
		`command -v flock >/dev/null 2>&1 || { echo ` + RemoteLockNoFlock + `; exit ` + itoa(RemoteLockExitNoFlock) + `; }`,
		`mkdir -p ` + ShQuote(dir) + ` || { echo ` + RemoteLockNoDir + `; exit ` + itoa(RemoteLockExitNoDir) + `; }`,
		// Creatability is probed in a subshell before the redirection that
		// depends on it. POSIX lets a redirection failure on `exec` with no
		// command terminate a non-interactive shell, which dash does: the `||`
		// branch never runs there, so the marker below is unreachable and the
		// caller is left guessing at a bare non-zero exit.
		`( : >>` + q + ` ) 2>/dev/null || { echo ` + RemoteLockNoCreate + `; exit ` + itoa(RemoteLockExitNoCreate) + `; }`,
		// THE INSTRUMENT IS PROVED BEFORE IT IS TRUSTED, and in two parts
		// NEITHER OF WHICH TOUCHES THE LOCK FILE'S IDENTITY. That restriction
		// is the whole design of this probe, and the first version got it
		// wrong: it asked sameFile() about the lock file itself, so the one
		// observation it could make -- "this descriptor is not that path" --
		// had two causes, a host that cannot compare files and a lock file
		// that had just been replaced. It attributed every one of them to the
		// first. A concurrent `-break-target-lock` unlinks that file, and
		// BreakRunLock takes the flock before it unlinks while this probe
		// holds no flock at all, so it is not excluded here: the result was a
		// retryable race reported as a permanent misconfiguration, telling an
		// operator to go and check /proc on a host where /proc is fine, and
		// failing the sync instead of spending one of the ten retries the loop
		// below exists to spend. The identical event one statement later is
		// absorbed silently.
		//
		// So the two capabilities the loop depends on are checked separately,
		// each on something that cannot be unlinked underneath it:
		//
		//   - `[ / -ef / ]` asks only whether the operator exists. The root
		//     directory cannot be replaced, so this can fail for exactly one
		//     reason. stderr is deliberately NOT silenced: a shell without
		//     `-ef` says so there ("unexpected operator" in dash) and exits 2
		//     rather than 1, and HoldCommand reads stderr on its pre-READY
		//     refusal branches, so that sentence reaches the operator.
		//   - `[ -e /proc/$$/fd/8 ]` asks only whether a descriptor path
		//     resolves. Measured: it stays true after the file is unlinked,
		//     because procfs resolves the descriptor to the inode and not to
		//     the name -- which is exactly what makes it immune to the race
		//     the first version tripped over.
		//
		// What is left for the loop is an answer that can only mean the file
		// moved, which is the one meaning every other acquirer in this tree
		// gives it.
		`[ / -ef / ] || { echo ` + RemoteLockNoSameFile + `; exit ` + itoa(RemoteLockExitNoSameFile) + `; }`,
		`exec 8>>` + q,
		`[ -e /proc/$$/fd/8 ] || { echo ` + RemoteLockNoSameFile + `; exit ` + itoa(RemoteLockExitNoSameFile) + `; }`,
		`exec 8>&-`,
		`n=0`,
		`while :; do`,
		`n=$((n+1))`,
		`( : >>` + q + ` ) 2>/dev/null || { echo ` + RemoteLockNoCreate + `; exit ` + itoa(RemoteLockExitNoCreate) + `; }`,
		// `exec 9>>` in the shell itself, so the lock belongs to the shell and
		// outlives the flock invocation. Append, never truncate: `>` truncates
		// BEFORE flock decides who wins, so a contender that then loses would
		// have blanked the provenance of the holder it lost to.
		`exec 9>>` + q,
		`flock -n 9 || { echo ` + RemoteLockBusy + `; exit ` + itoa(RemoteLockExitBusy) + `; }`,
		sameFile("9") + ` && break`,
		// Locked an inode that is no longer at path. Closing fd 9 releases
		// that useless lock; the next turn takes whatever is there now.
		//
		// Deleting this line is an equivalent mutation, and no test can
		// catch it: the next turn's `exec 9>>` reopens fd 9, which closes
		// the previous open file description, and a flock goes when the
		// last descriptor referring to it does. Measured, with a control
		// showing the lock IS still held while fd 9 refers to it. It stays
		// because otherwise the release is a consequence of where the
		// reopen happens rather than something this loop does, and an edit
		// moving the open would take the release with it in silence.
		`exec 9>&-`,
		`[ "$n" -lt ` + itoa(shellHoldAttempts) + ` ] || { echo ` + RemoteLockNoCreate + `; exit ` + itoa(RemoteLockExitNoCreate) + `; }`,
		`done`,
		`echo ` + RemoteLockReady,
		// exec, so exactly one process holds fd 9 and the lock cannot outlive it
		// in a forked child. >/dev/null, because nothing drains this command's
		// stdout: a reader that echoed the heartbeat would fill the channel
		// window and wedge both ends.
		//
		// AND IT DISCARDS THE BEATS DELIBERATELY. The heartbeat arrives here as
		// one newline per beat, so a `while read` loop could repeat the
		// comparison above for the lock's whole held life, which is exactly
		// what the leased holder does -- RunLockWasReplaced on every beat, then
		// exit dispossessed. Doing it here would be wrong, because the error
		// policy inverts once the lock is held: RunLockWasReplaced counts only
		// a POSITIVE answer, since "cannot tell" at that point "would mean a
		// holder abandoning a lock in the middle of a sync", and every other
		// stat failure reports nothing. Shell `test` cannot tell those apart --
		// `[ -e path ]` is false both for a path that is gone and for one that
		// could not be read, with no errno to consult -- so a shell version
		// would carry the fail-CLOSED policy into the one place this package
		// says it must not go, and a remount or a momentarily unreadable lock
		// directory would drop a good lock and fail the sync behind it. The
		// held-life check needs errno, so it belongs to the holder that has it.
		`exec cat >/dev/null`,
	}, "\n")
}

// holdViaShell takes the lock with POSIX flock in the login shell, for a target
// with no usable helper.
//
// One shell script, one round trip. Each failure prints its own marker before
// exiting so the error a caller sees names the actual cause instead of a generic
// non-zero exit.
func holdViaShell(ctx context.Context, h CommandHolder, path, dir, key string, lease time.Duration) (*RemoteRunLock, error) {
	script := shellHoldScript(path, dir)

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
	case strings.Contains(msg, RemoteLockNoSameFile):
		// Two capabilities, each probed on something that cannot be unlinked,
		// so this says what is missing rather than guessing: either the login
		// shell has no `test -ef` (it will have said so on stderr, which is
		// included in the error below it), or /proc does not resolve
		// descriptor paths there. A replaced lock file cannot reach this
		// message -- that is the retry loop's business.
		return nil, fmt.Errorf("the target host cannot support the shell run lock on %s: either its login shell has no `test -ef` or /proc/<pid>/fd does not resolve there, and without both a lock taken there cannot be shown to be the lock that path names -- which would let a run hold an unlinked file that excludes nobody. Install vmsync-bridge-helper on the target to use the leased hold instead, which needs neither", path)
	case strings.Contains(msg, RemoteLockNoCreate):
		// Two causes, one marker, deliberately: the leased holder reports the
		// same errLockNoCreate when the file keeps being replaced out from
		// under it, and the two implementations are meant to be
		// indistinguishable to a caller.
		return nil, fmt.Errorf("could not take the lock file %s on the target host -- either it cannot be created there, or it kept being replaced out from under repeated attempts to lock it (%d tries), which is what something clearing that directory looks like", path, shellHoldAttempts)
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
