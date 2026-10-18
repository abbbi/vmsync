package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// READY is a promise, and this is what keeps it honest.
//
// holdRunLock prints VMSYNC-LOCK-ACQUIRED and a driver that reads it starts
// writing a replica. If the lock file were unlinked and recreated between this
// process's OpenFile and its Flock, the lock would sit on an orphaned inode
// that excludes nobody and READY would be a lie for the length of the sync.
// Checking on the heartbeat is too late: the heartbeat begins after READY.
//
// The window is two adjacent statements, so the comparison is forced through
// the lockIsAtPath seam rather than raced. The flock is the real one.
func TestTheHelperWillNotAnnounceReadyAboutAnOrphan(t *testing.T) {
	path := lockPath(t)
	// lockTheRunLockFile does not create the directory -- holdRunLock does that
	// before calling it. Without this the open fails with ENOENT and BOTH
	// subtests below pass for that reason instead of the one they name, which
	// is how the first draft of this test passed while proving nothing.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	original := lockIsAtPath
	t.Cleanup(func() { lockIsAtPath = original })

	t.Run("never the right file: give up rather than hold an orphan", func(t *testing.T) {
		lockIsAtPath = func(f *os.File, p string) bool { return false }
		f, err := lockTheRunLockFile(path)
		if err == nil {
			f.Close()
			t.Fatal("the helper took a lock on a file that is not the one at the path, and would have announced READY about it")
		}
		if !errors.Is(err, errLockNoCreate) {
			t.Errorf("err = %v, want one wrapping errLockNoCreate so runLockExit maps it to the marker vmsync already understands", err)
		}
		// The message, not just the sentinel: errLockNoCreate is also what a
		// missing directory produces, and a test that cannot tell those apart
		// is satisfied by the open never succeeding at all.
		if !strings.Contains(err.Error(), "kept being replaced") {
			t.Errorf("err = %v, which does not say the lock file kept being replaced -- so this may be failing at the open rather than at the check under test", err)
		}
	})

	t.Run("replaced once, then stable: the retry is what makes that survivable", func(t *testing.T) {
		calls := 0
		lockIsAtPath = func(f *os.File, p string) bool {
			calls++
			return calls > 1
		}
		f, err := lockTheRunLockFile(path)
		if err != nil {
			t.Fatalf("a lock file replaced once was not retried: %v -- a single tmpfiles.d sweep would then fail the sync", err)
		}
		defer f.Close()
		if calls != 2 {
			t.Errorf("the check ran %d times, want 2: once for the replaced file and once for the one that stuck", calls)
		}
	})
}
