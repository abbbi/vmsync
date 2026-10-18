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

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"vmsync/pkg/util"
)

// The target-side run lock, held here under a lease.
//
// The lock has to be released when the driver holding it dies, and the driver's
// death is invisible to this host: a power loss or a partition sends no FIN,
// sshd's ClientAliveInterval is off by default, and a remote shell parked on
// `cat` never writes, so nothing notices until TCP keepalive gives up roughly
// two hours later. Until then `-promote` and `-restore` on this host exit 75 --
// during the disaster the replica exists for.
//
// A lease answers that with a clock on THIS side of the link: the driver has to
// keep saying it is alive, and the lock is released the moment it stops. The
// same effect is reachable with `read -t` in the remote shell, but that is a
// bash builtin dash does not have, and whether a hypervisor can release a stale
// lock must not depend on which /bin/sh it ships.
//
// This helper is optional, so the lock cannot depend on it: vmsync falls back to
// a POSIX `flock` + `cat` shell when it is absent or too old, which still
// excludes concurrent runs but cannot release the lock on its own. See
// util.AcquireRemoteRunLock, which chooses, and util.BreakRemoteRunLock, which
// is the escape on a host that fell back.
type runLockConfig struct {
	// Path is the lock file, absolute. vmsync derives it with util.RunLockPath
	// so both ends name the same file by the same rule.
	Path string
	// Lease is how long the lock survives silence. The driver beats at a third
	// of it, so two beats can be lost to a stalled host or a slow link before
	// the lock goes.
	Lease time.Duration
	// Stamp is the provenance vmsync wants recorded, as a marshalled
	// util.RunLockIdentity. The pid, boot id and start ticks in it describe THIS
	// process and are filled in here, because only this process knows them.
	Stamp string
}

// errLeaseExpired reports that no heartbeat arrived within the lease, which is
// the whole reason this mode exists: the driver is gone and the lock is now
// released for whoever needs it next.
var errLeaseExpired = errors.New("run lock lease expired")

// errLockDispossessed is the lock file going away under a holder that is still
// alive and still beating.
//
// Nothing in vmsync does this any more -- BreakRunLock takes the lock before it
// unlinks it -- so reaching this means something outside did: a tmpfiles.d rule
// clearing /run, or an operator with rm. Exiting is the only correct response,
// because the flock this process holds is now on an orphaned inode and excludes
// nobody, while a fresh file at the same path may already have a second writer
// on it.
//
// It deliberately gets no marker of its own and falls to runLockExit's default
// of exit 1. The shell implementation cannot produce this outcome, and the two
// are required to be indistinguishable to a caller; the driver learns of it the
// way it learns of any helper exit, through the closed channel, and turns that
// into the refusal at its own commit and redefine gates.
var errLockDispossessed = errors.New("the run lock was removed from under this holder")

// The ways the lock cannot be taken, each mapping to one marker and one exit
// code that vmsync's shell path also uses, so the two implementations are
// interchangeable from the caller's side.
var (
	errLockBusy         = errors.New(util.RemoteLockBusy)
	errLockNoDir        = errors.New(util.RemoteLockNoDir)
	errLockNoCreate     = errors.New(util.RemoteLockNoCreate)
	errLockPathRelative = errors.New("a run lock path must be absolute")
)

// runLockExit maps a holdRunLock result onto the marker to print and the status
// to exit with.
//
// Pure, so the mapping every caller depends on is testable without a lock, a
// filesystem or an SSH session.
func runLockExit(err error) (marker string, code int) {
	switch {
	case err == nil, errors.Is(err, errLeaseExpired):
		// Both are the lock doing its job. The lease expiring is not a failure
		// of this process; it is the outcome it exists to produce.
		return "", 0
	case errors.Is(err, errLockBusy):
		return util.RemoteLockBusy, util.RemoteLockExitBusy
	case errors.Is(err, errLockNoDir):
		return util.RemoteLockNoDir, util.RemoteLockExitNoDir
	case errors.Is(err, errLockNoCreate):
		return util.RemoteLockNoCreate, util.RemoteLockExitNoCreate
	default:
		return "", 1
	}
}

// holdRunLock takes the lock, records who holds it, and holds it until the
// driver stops beating.
//
// Returns nil when the driver let go cleanly (stdin closed), errLeaseExpired
// when it went silent, and any other error when the lock could not be taken at
// all. The caller maps those to the exit codes and markers vmsync's shell path
// already uses, so a caller cannot tell the two implementations apart.
func holdRunLock(cfg runLockConfig, stdin io.Reader, stdout io.Writer) error {
	if !filepath.IsAbs(cfg.Path) {
		return fmt.Errorf("%w: %q", errLockPathRelative, cfg.Path)
	}
	if cfg.Lease <= 0 {
		return fmt.Errorf("a run lock lease must be positive, not %s", cfg.Lease)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Path), 0o755); err != nil {
		return fmt.Errorf("%w: %v", errLockNoDir, err)
	}

	f, err := lockTheRunLockFile(cfg.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	// The flock lives on the open file description, so it is released by this
	// process exiting for ANY reason -- a clean return, a panic, a SIGKILL, the
	// host rebooting. That is what makes the lease safe to be the only timer:
	// nothing here has to run for the lock to go away.

	writeLockStamp(f, cfg.Stamp)

	if _, err := fmt.Fprintln(stdout, util.RemoteLockReady); err != nil {
		return fmt.Errorf("announce the run lock: %w", err)
	}

	return awaitHeartbeats(stdin, cfg.Lease, f, cfg.Path)
}

// lockTheRunLockFile takes the lock and returns it only once it is the lock
// that path actually names.
//
// The open-then-flock window is the one util.AcquireRunLock has always guarded,
// and this holder needs it for a sharper reason: the next thing it does is
// print VMSYNC-LOCK-ACQUIRED, and a driver that reads that line starts writing
// a replica. If path were unlinked and recreated in between -- a tmpfiles.d
// rule clearing /run, or anything else that mistakes a live lock for litter --
// the flock would land on an orphaned inode that excludes nobody, and READY
// would be a lie for the whole length of the sync. Checking only on the
// heartbeat is too late, because the heartbeat starts after READY.
//
// Fails CLOSED, via RunLockIsAtPath: at acquisition a "cannot tell" costs one
// retry, and after ten of them a refusal the driver reports, which is a far
// better outcome than announcing a lock this cannot vouch for.
//
// No O_TRUNC on the open. The file is truncated only after the lock is held,
// because truncating first would blank the CURRENT holder's provenance on the
// way to discovering that it is the current holder.
// lockIsAtPath is util.RunLockIsAtPath, indirected for the same reason
// pkg/util does it for BreakRunLock: the window it guards is between this
// function's own OpenFile and its own Flock, and nothing outside can be
// scheduled there. Forcing the answer is the only way to prove the retry
// happens and that READY is withheld when it cannot be earned.
var lockIsAtPath = util.RunLockIsAtPath

func lockTheRunLockFile(path string) (*os.File, error) {
	const maxAttempts = 10
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errLockNoCreate, err)
		}
		if err := util.FlockExclusiveNB(f); err != nil {
			f.Close()
			return nil, fmt.Errorf("%w: %v", errLockBusy, err)
		}
		if lockIsAtPath(f, path) {
			return f, nil
		}
		// Locked an inode that is no longer at path. Let it go -- which also
		// releases the useless lock -- and take whatever is there now.
		f.Close()
	}
	return nil, fmt.Errorf("%w: %s kept being replaced out from under repeated attempts to lock it (%d tries)", errLockNoCreate, path, maxAttempts)
}

// writeLockStamp records who holds the lock, for an operator who has to decide
// whether to break it.
//
// Best-effort by util.WriteRunLockIdentity's contract, and it has to stay that
// way: losing provenance must never cost a sync, and every reader of it falls
// back to treating the lock as held by somebody unidentified.
func writeLockStamp(f *os.File, stamp string) {
	var id util.RunLockIdentity
	if stamp != "" {
		if err := json.Unmarshal([]byte(stamp), &id); err != nil {
			fmt.Fprintf(os.Stderr, "vmsync-bridge-helper: run lock stamp could not be read (%v); holding the lock with no provenance\n", err)
		}
	}
	// This process is the holder, whatever the caller said, so the three fields
	// that decide liveness are taken from here rather than trusted.
	self := util.NewRunLockIdentity(id.Kind, id.SourceDomain, id.TargetRef, id.RunID, id.StartedAtUnix)
	if err := util.WriteRunLockIdentity(f, self); err != nil {
		fmt.Fprintf(os.Stderr, "vmsync-bridge-helper: could not record who holds the run lock (%v); the lock is held, but an operator deciding whether to break it will find no provenance\n", err)
	}
}

// awaitHeartbeats returns when the driver stops talking, by either route, or
// when this process stops being the holder of the lock it took.
//
// Any line is a heartbeat: the content carries nothing, because the only fact
// being transmitted is that the driver is still there to send it. EOF is the
// clean path -- the driver closed stdin, or the SSH channel went with it -- and
// it is distinguished from the lease expiring because the two mean different
// things to whoever reads the log afterwards.
//
// The dispossession check rides on the beat rather than on a timer of its own.
// A beat is already the moment this process has something to do, it arrives at
// a third of the lease so the check is as prompt as anything here needs to be,
// and a silent driver loses the lock to the lease regardless -- so a second
// ticker would only add a way for the two to disagree.
func awaitHeartbeats(stdin io.Reader, lease time.Duration, held *os.File, path string) error {
	beats := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(stdin)
		for sc.Scan() {
			// Non-blocking: a beat already pending says the same thing this one
			// would, so the reader never stalls behind the lease loop.
			select {
			case beats <- struct{}{}:
			default:
			}
		}
	}()

	for {
		// A fresh timer per wait rather than one reset in place. Reset needs the
		// previous timer drained to be correct, and getting that wrong is a
		// lock that either never expires or expires while its driver is healthy.
		select {
		case <-beats:
			if why := util.RunLockWasReplaced(held, path); why != "" {
				return fmt.Errorf("%w: %s", errLockDispossessed, why)
			}
		case <-done:
			return nil
		case <-time.After(lease):
			return errLeaseExpired
		}
	}
}
