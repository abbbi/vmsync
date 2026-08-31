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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// ErrLockHeld indicates AcquireRunLock failed specifically because another
// process already holds the lock -- the one, genuinely expected outcome
// when two invocations for the same key legitimately overlap. Callers
// must distinguish this (via errors.Is) from every other failure this
// function can return -- can't create the lock directory, can't open the
// lock file, or the lock file kept being replaced out from under repeated
// attempts -- all of which mean something is actually broken (permissions,
// a read-only filesystem, a tmpfiles.d rule fighting the lock directory)
// and must not be silently treated the same as "another instance is
// running, nothing to do here".
var ErrLockHeld = errors.New("lock already held by another process")

// ExitBusy is the process exit status meaning "stood down without doing
// anything, because another vmsync holds the lock for this domain".
//
// 75 is sysexits.h's EX_TEMPFAIL: a temporary failure, retry later. It lives
// here rather than in either command because it is a contract BETWEEN two
// binaries -- vmsync exits with it, and vmsync-agent reads it off a finished
// process and decides not to record a terminal result -- and a constant two
// programs must agree on is exactly what does not belong in one of them.
//
// It exists because the two ordinary codes both lie for an operator verb. 0
// would report a restore that never happened as done; 1 would report one that
// never started as failed, which the agent records terminally and can then
// never retry with the same operation id -- while the reason it could not run
// clears up on its own seconds later.
//
// A SYNC uses it too, and used not to. The original reasoning -- contention is
// an ordinary skip, a scheduler running every few minutes simply tries again,
// nothing needs to tell the cases apart -- was sound while the scheduler's
// memory of what it launched was assumed intact. It is not intact across a
// restart: vmsync-agent holds its in-flight set in memory only, so a new
// instance knows nothing about a child the previous one started, launches a
// second vmsync for that domain, and reads the exit code to decide what
// happened. At 0 it recorded a SUCCESS -- every interval, for as long as the
// real sync ran -- so the agent's metrics, the console and the journal all
// reported healthy replication while nothing was being copied.
//
// Telling the cases apart is therefore no longer optional, and this is the
// only signal that can: it comes from the process that actually holds the
// lock, so unlike anything the agent can infer from /proc or its own files, it
// cannot be stale or wrong.
//
// Consequence for anything else that runs vmsync: a scheduled overlap that was
// previously indistinguishable from success now exits 75. Callers that treat
// any non-zero code as an error need to accept it -- see
// contrib/runner/vmsync-parallel.sh, which passes "0;75" for exactly this.
const ExitBusy = 75

// safeKeyReplacer makes a lock key filesystem-safe reversibly (percent-
// encoding style), rather than via lossy character substitution -- mapping
// both "/" and " " to the same "_" would make two different keys (e.g.
// "web server" and "web_server") sanitize to the identical lock filename,
// causing spurious lock contention between two completely unrelated
// domains. "%" is escaped first (to itself, doubled) specifically so a key
// that already happens to contain a literal "%" can never collide with one
// of these escape sequences appearing "naturally" in another key.
var safeKeyReplacer = strings.NewReplacer("%", "%%", "/", "%2f", " ", "%20")

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
//
// flock() locks the open file description (in turn tied to the inode it was
// opened against) rather than the path itself -- if path is deleted and
// recreated as a fresh inode (e.g. a tmpfiles.d rule clearing /run, or
// anything else cleaning up what it mistakes for a stale lock file) between
// this function's own OpenFile and Flock calls, the lock ends up held on an
// orphaned, unlinked inode that nothing else will ever open, providing no
// actual mutual exclusion against whatever's now using the new file at
// path. This is detected (by comparing the locked fd's inode against a
// fresh stat of path once locked) and retried against whatever's actually
// at path now, up to a bounded number of attempts. This only covers the
// window up to acquisition, though -- it does not detect (nor could it,
// without a background goroutine polling for the rest of this lock's
// entire held lifetime) the same kind of replacement happening to path
// *after* a successful return from this function, while the lock is still
// being held for an in-progress sync.
// RunLockPath is where the lock for a given key lives.
//
// Exported and shared so that a lock taken locally and the same lock taken
// over SSH cannot disagree about the path. They must name the identical
// file: vmsync runs on the source host for a sync and on the target host
// for a promotion, so the two paths meeting is the only thing that makes
// those mutually exclusive.
func RunLockPath(dir, key string) string {
	return filepath.Join(dir, safeKeyReplacer.Replace(key)+".lock")
}

func AcquireRunLock(dir, key string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create lock dir %s: %w", dir, err)
	}
	path := RunLockPath(dir, key)

	const maxAttempts = 10
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0644)
		if err != nil {
			return nil, fmt.Errorf("open lock file %s: %w", path, err)
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			f.Close()
			return nil, fmt.Errorf("another vmsync is already running for %q (lock %s held): %w (%v)", key, path, ErrLockHeld, err)
		}

		if lockRefersToCurrentPath(f, path) {
			return f, nil
		}
		// path no longer refers to the inode we just locked -- something
		// replaced it between our OpenFile above and here. Release this
		// now-worthless lock and retry against whatever's there now.
		f.Close()
	}
	return nil, fmt.Errorf("lock file %s kept being replaced out from under repeated acquisition attempts (%d tries) -- check whether something (e.g. a tmpfiles.d rule) is deleting it", path, maxAttempts)
}

// lockRefersToCurrentPath reports whether f (an already-open, already-
// locked file) is still the same file path currently refers to -- false if
// path was deleted, replaced by a different file, or can no longer be
// stat'd at all. Split out from AcquireRunLock purely so this comparison is
// directly, deterministically testable without needing to win a real
// open-then-flock timing race.
func lockRefersToCurrentPath(f *os.File, path string) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	pathInfo, err := os.Stat(path)
	if err != nil {
		return false
	}
	return os.SameFile(fi, pathInfo)
}
