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
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The backstop vmsync's own break message used to claim, made real.
//
// cmd/vmsync/breaklock.go told an operator that a dispossessed holder "will
// find the lock gone -- its own gates before commit and before define refuse on
// that". Those gates are targetLock.Lost(), which is set by a failed heartbeat
// or by the holder exiting, and unlinking the lock file does neither: the
// heartbeat is a newline down the SSH channel, which knows nothing about the
// file. So the holder kept its flock on an orphaned inode and went on to commit
// and redefine.
//
// This is the link that makes the claim true for a leased hold. Nothing in
// vmsync unlinks a held lock any more, so reaching it means something outside
// did -- a tmpfiles.d rule clearing /run, an operator with rm. The helper
// notices on the next beat and exits, the driver sees the channel close, and
// Lost() makes both gates refuse.
func TestTheHelperLetsGoWhenItsLockFileIsTakenAway(t *testing.T) {
	path := lockPath(t)
	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close() })

	// Long enough that an expiry cannot be mistaken for the answer: if this
	// test passes in a second, it passed because of the check and not the
	// clock.
	const lease = 30 * time.Second
	done := make(chan error, 1)
	go func() {
		var out bytes.Buffer
		done <- holdRunLock(runLockConfig{Path: path, Lease: lease}, pr, &out)
	}()

	// Let it take the lock, then confirm it really has before removing
	// anything -- otherwise this could be racing the open and testing nothing.
	waitFor(t, func() bool {
		_, err := os.Stat(path)
		return err == nil
	}, "the helper never created its lock file")

	if _, err := pw.Write([]byte("\n")); err != nil {
		t.Fatalf("first heartbeat: %v", err)
	}
	select {
	case err := <-done:
		t.Fatalf("the helper let go of an untouched lock after one beat: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	// Something outside vmsync removes it.
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove the lock file: %v", err)
	}

	// Keep beating. A driver that is alive and well is exactly the case the
	// old code could not detect.
	go func() {
		for {
			if _, err := pw.Write([]byte("\n")); err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	select {
	case err := <-done:
		if !errors.Is(err, errLockDispossessed) {
			t.Fatalf("holdRunLock() = %v, want errLockDispossessed -- the holder has to say WHY it let go, because the driver turns a silent exit and this one into different log lines", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the helper kept holding a lock whose file had been removed, while its driver beat on -- so a second writer could hold the replacement and neither would know")
	}
}

// The other half, and the one that costs a sync if it is wrong: a holder must
// not let go because a stat blipped. TestHeartbeatsKeepTheLock covers the
// untouched case over five leases; this covers the case where the lock
// directory itself becomes unreadable, which is the shape of a transient error.
func TestTheHelperKeepsTheLockWhenThePathMerelyCannotBeRead(t *testing.T) {
	path := lockPath(t)
	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close() })

	const lease = 30 * time.Second
	done := make(chan error, 1)
	go func() {
		var out bytes.Buffer
		done <- holdRunLock(runLockConfig{Path: path, Lease: lease}, pr, &out)
	}()
	waitFor(t, func() bool {
		_, err := os.Stat(path)
		return err == nil
	}, "the helper never created its lock file")

	// Make the path un-statable with something that is not "it is gone", by
	// replacing its parent directory with a regular file. The helper's own
	// descriptor stays valid throughout.
	dir := filepath.Dir(path)
	if err := os.Rename(dir, dir+".moved"); err != nil {
		t.Fatalf("move the lock dir: %v", err)
	}
	if err := os.WriteFile(dir, nil, 0o644); err != nil {
		t.Fatalf("put a file where the lock dir was: %v", err)
	}
	if _, err := os.Stat(path); err == nil || os.IsNotExist(err) {
		t.Skipf("this host does not produce a non-ENOENT stat error for that path (%v)", err)
	}

	for i := 0; i < 5; i++ {
		if _, err := pw.Write([]byte("\n")); err != nil {
			t.Fatalf("heartbeat %d: %v", i, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case err := <-done:
		t.Fatalf("the helper abandoned a lock it still held because the path could not be read: %v -- a blip on the lock directory must not cost a sync", err)
	default:
	}

	// A bare "it did not happen" proves nothing on its own: a helper whose
	// loop had died, or which never reached the check, would pass the same
	// assertion. So put the directory back and then remove the lock file for
	// real. Letting go now is what shows the check was live and silent above
	// rather than absent.
	if err := os.Remove(dir); err != nil {
		t.Fatalf("remove the placeholder file: %v", err)
	}
	if err := os.Rename(dir+".moved", dir); err != nil {
		t.Fatalf("restore the lock dir: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove the lock file: %v", err)
	}
	go func() {
		for {
			if _, err := pw.Write([]byte("\n")); err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	select {
	case err := <-done:
		if !errors.Is(err, errLockDispossessed) {
			t.Fatalf("holdRunLock() = %v, want errLockDispossessed once the file was genuinely gone", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the helper never reacted to its lock file being removed, so the silence above was an absent check rather than a quiet one")
	}
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}
