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

package util

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// liveHolder takes the lock the way a holder does and keeps it for the test.
func liveHolder(t *testing.T, dir, key string) *os.File {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f, err := os.OpenFile(RunLockPath(dir, key), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open the lock file: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	if err := FlockExclusiveNB(f); err != nil {
		t.Fatalf("take the flock: %v", err)
	}
	return f
}

// THE TWO-WRITER DEFECT, as an assertion.
//
// The decision to break probes the flock and then lets it go; the caller
// connects to libvirt, reads a domain and writes a journal record before the
// unlink happens. A run that acquires the lock inside that window used to have
// its lock file removed from under it -- measured, with both inodes -- after
// which a second acquirer created a fresh file and won it. Neither could tell.
//
// The property is not "BreakRunLock is careful". It is that there is never a
// moment when two acquirers both hold what they believe is this lock.
func TestBreakRunLockWillNotUnlinkALockSomebodyHolds(t *testing.T) {
	dir, key := t.TempDir(), "target-web01"
	holder := liveHolder(t, dir, key)

	removed, err := BreakRunLock(dir, key)
	if removed {
		t.Error("BreakRunLock removed a lock file while another process held the lock on it")
	}
	if !errors.Is(err, ErrLockHolderAlive) {
		t.Errorf("BreakRunLock err = %v, want one wrapping ErrLockHolderAlive so the caller can tell a refusal from a failure", err)
	}

	// The holder is untouched: same path, same inode, still its lock.
	if _, err := os.Stat(RunLockPath(dir, key)); err != nil {
		t.Fatalf("the lock file is gone after a refused break: %v", err)
	}
	if !RunLockIsAtPath(holder, RunLockPath(dir, key)) {
		t.Error("the holder's lock no longer refers to the lock path, so it was dispossessed by a break that reported it had refused")
	}

	// And the property that matters: nobody else can hold this lock.
	second, err := AcquireRunLock(dir, key)
	if err == nil {
		second.Close()
		t.Fatal("a second acquirer took the lock after the break -- two writers on one replica, which is the whole defect")
	}
	if !errors.Is(err, ErrLockHeld) {
		t.Errorf("the second acquirer was refused with %v, want ErrLockHeld", err)
	}
}

// The refusal must not be universal, or the function is merely broken the other
// way and every test above it passes.
func TestBreakRunLockStillBreaksALockNobodyHolds(t *testing.T) {
	dir, key := t.TempDir(), "target-web01"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(RunLockPath(dir, key), []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write the lock file: %v", err)
	}

	removed, err := BreakRunLock(dir, key)
	if err != nil {
		t.Fatalf("BreakRunLock on a free lock: %v", err)
	}
	if !removed {
		t.Error("BreakRunLock reported it removed nothing, but there was a free lock file to remove")
	}
	if _, err := os.Stat(RunLockPath(dir, key)); !os.IsNotExist(err) {
		t.Errorf("the lock file survived a successful break: %v", err)
	}
}

// THE INODE GATE, behaviourally.
//
// BreakRunLock's second refusal covers the file at path being swapped between
// its own OpenFile and its own Flock -- two adjacent statements, with no way
// for a test to be scheduled in between. A structural assertion that the check
// is present turned out to be satisfied by syntax: measured on 2026-10-19, a
// bare call, a call behind an always-false condition and a call moved into a
// defer all left the whole suite green while the unlink went ahead regardless.
//
// So the stat is indirected through lockStillAtPath and forced here. The seam
// is the comparison only -- the flock and the unlink are the real ones -- so
// what this proves is exactly what matters: given a negative answer,
// BreakRunLock refuses, says why, and leaves the file alone.
func TestBreakRunLockRefusesWhenWhatItLockedIsNotWhatIsAtThePath(t *testing.T) {
	dir, key := t.TempDir(), "target-web01"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Free and unheld, so the break would otherwise succeed. Any failure below
	// is the gate and nothing else.
	if err := os.WriteFile(RunLockPath(dir, key), []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write the lock file: %v", err)
	}

	original := lockStillAtPath
	t.Cleanup(func() { lockStillAtPath = original })
	asked := 0
	lockStillAtPath = func(f *os.File, path string) bool {
		asked++
		return false
	}

	removed, err := BreakRunLock(dir, key)

	if asked == 0 {
		t.Error("BreakRunLock never asked whether the file it locked is still the file at the path, so a lock file swapped during its own open-to-flock window is unlinked while the lock it holds is on the orphan")
	}
	if removed {
		t.Error("BreakRunLock removed the file after being told it is not the one it locked")
	}
	if !errors.Is(err, ErrLockReplaced) {
		t.Errorf("BreakRunLock err = %v, want one wrapping ErrLockReplaced -- and NOT ErrLockHolderAlive, which claims something about a holder this branch never looked at", err)
	}
	if errors.Is(err, ErrLockHolderAlive) {
		t.Error("the refusal claims the holder is still running, which this branch did not check")
	}
	if _, err := os.Stat(RunLockPath(dir, key)); err != nil {
		t.Errorf("the lock file was removed despite the refusal: %v", err)
	}
}

// RunLockWasReplaced decides whether a holder abandons a lock mid-sync, so it
// answers only when it is SURE. Its sibling RunLockIsAtPath answers the
// same question with the opposite policy, which is correct there and would be a
// sync-killer here.
func TestRunLockWasReplacedSpeaksOnlyOnAPositiveAnswer(t *testing.T) {
	t.Run("the lock is still the lock", func(t *testing.T) {
		dir, key := t.TempDir(), "target-web01"
		f := liveHolder(t, dir, key)
		if why := RunLockWasReplaced(f, RunLockPath(dir, key)); why != "" {
			t.Errorf("a holder of an untouched lock was told it had lost it: %s", why)
		}
	})

	t.Run("the lock file was removed", func(t *testing.T) {
		dir, key := t.TempDir(), "target-web01"
		f := liveHolder(t, dir, key)
		if err := os.Remove(RunLockPath(dir, key)); err != nil {
			t.Fatalf("remove: %v", err)
		}
		why := RunLockWasReplaced(f, RunLockPath(dir, key))
		if why == "" {
			t.Fatal("a holder whose lock file was removed was told nothing had changed")
		}
		if !strings.Contains(why, "removed") {
			t.Errorf("the reason does not say the file was removed: %q", why)
		}
	})

	t.Run("the lock file was replaced by a different one", func(t *testing.T) {
		dir, key := t.TempDir(), "target-web01"
		f := liveHolder(t, dir, key)
		other := filepath.Join(dir, "fresh")
		if err := os.WriteFile(other, nil, 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := os.Rename(other, RunLockPath(dir, key)); err != nil {
			t.Fatalf("rename over the lock path: %v", err)
		}
		why := RunLockWasReplaced(f, RunLockPath(dir, key))
		if why == "" {
			t.Fatal("a holder whose lock file was replaced was told nothing had changed")
		}
		if !strings.Contains(why, "different file") {
			t.Errorf("the reason does not say the file is a different one: %q", why)
		}
	})

	t.Run("the path cannot be read at all", func(t *testing.T) {
		// A stat failure that is NOT "it is gone". Replacing the parent
		// directory with a regular file makes the path un-statable with
		// ENOTDIR while the holder's own descriptor stays perfectly valid.
		// This must report nothing: a transient error reading the lock
		// directory is not evidence that somebody else holds the lock, and
		// treating it as such would abandon a sync over a blip.
		root := t.TempDir()
		dir := filepath.Join(root, "locks")
		key := "target-web01"
		f := liveHolder(t, dir, key)

		if err := os.Rename(dir, filepath.Join(root, "locks.moved")); err != nil {
			t.Fatalf("move the lock dir: %v", err)
		}
		if err := os.WriteFile(dir, nil, 0o644); err != nil {
			t.Fatalf("put a file where the lock dir was: %v", err)
		}
		if _, err := os.Stat(RunLockPath(dir, key)); err == nil || os.IsNotExist(err) {
			t.Skipf("this host does not produce a non-ENOENT stat error for that path (%v), so the fail-quiet branch cannot be reached here", err)
		}

		if why := RunLockWasReplaced(f, RunLockPath(dir, key)); why != "" {
			t.Errorf("an unreadable lock path was reported as dispossession: %s -- a blip reading the lock directory would abandon a sync", why)
		}
	})
}
