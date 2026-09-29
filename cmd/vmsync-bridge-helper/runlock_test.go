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
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vmsync/pkg/util"
)

// lockPath is a lock file inside a temp directory that does not exist yet, so
// the directory creation is exercised too.
func lockPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "locks", "target-web01.lock")
}

// THE test for this mode: silence releases the lock.
//
// Everything else here is a guard around this one behaviour. A driver that loses
// power sends nothing and closes nothing, so the only thing that can free the
// lock is a clock on this side; without it the lock stands until TCP keepalive
// gives up about two hours later, and -promote on this host exits 75 for that
// whole window -- during the disaster the replica exists for.
func TestSilenceReleasesTheLock(t *testing.T) {
	path := lockPath(t)
	var out bytes.Buffer

	// A reader that never produces a line and never closes: a partitioned
	// driver, which is exactly the case a `cat` cannot detect.
	silent, _ := io.Pipe()
	defer silent.Close()

	start := time.Now()
	err := holdRunLock(runLockConfig{Path: path, Lease: 150 * time.Millisecond}, silent, &out)
	elapsed := time.Since(start)

	if !errors.Is(err, errLeaseExpired) {
		t.Fatalf("holdRunLock() = %v, want errLeaseExpired -- a driver that went silent must lose the lock", err)
	}
	if elapsed < 150*time.Millisecond {
		t.Errorf("released after %s, before the lease was up; a lock that goes early can be taken while its driver is still writing", elapsed)
	}
	if elapsed > 3*time.Second {
		t.Errorf("released after %s, far past the lease", elapsed)
	}
	if got := strings.TrimSpace(out.String()); got != util.RemoteLockReady {
		t.Errorf("stdout = %q, want exactly %q: the caller matches this line to know the lock is held", got, util.RemoteLockReady)
	}
}

// Heartbeats hold the lock open indefinitely, which is the other half of the
// same contract: a sync legitimately holds this lock for hours copying a large
// disk, so the lease must never expire under a driver that is still there.
func TestHeartbeatsKeepTheLock(t *testing.T) {
	path := lockPath(t)
	pr, pw := io.Pipe()

	const lease = 120 * time.Millisecond
	done := make(chan error, 1)
	go func() {
		var out bytes.Buffer
		done <- holdRunLock(runLockConfig{Path: path, Lease: lease}, pr, &out)
	}()

	// Beat at a third of the lease, the way vmsync does, for several leases'
	// worth of time. If the lease were not being renewed this would have
	// expired four times over.
	stop := time.After(5 * lease)
	beating := true
	for beating {
		select {
		case <-stop:
			beating = false
		case <-time.After(lease / 3):
			if _, err := pw.Write([]byte("\n")); err != nil {
				t.Fatalf("heartbeat write: %v", err)
			}
		}
	}
	select {
	case err := <-done:
		t.Fatalf("the lock was released after %v while its driver was still beating: %v -- a sync copying a 2 TB disk would lose its lock mid-run", 5*lease, err)
	default:
	}

	// And stopping releases it: closing stdin is the clean path, distinguished
	// from the lease expiring because the two mean different things in a log.
	pw.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("holdRunLock() = %v after stdin closed, want nil -- the driver let go cleanly", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the lock was not released after stdin closed")
	}
}

// A second holder must be refused with the marker the caller keys on, and must
// not disturb the first one's provenance on the way out.
func TestASecondHolderIsRefusedAndTouchesNothing(t *testing.T) {
	path := lockPath(t)
	pr, pw := io.Pipe()
	defer pw.Close()

	held := make(chan error, 1)
	go func() {
		var out bytes.Buffer
		held <- holdRunLock(runLockConfig{Path: path, Lease: 10 * time.Second}, pr, &out)
	}()

	// Wait for the first holder to have written its stamp.
	var first []byte
	for i := 0; i < 200; i++ {
		time.Sleep(10 * time.Millisecond)
		if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) > 0 {
			first = b
			break
		}
	}
	if len(first) == 0 {
		t.Fatal("the first holder never recorded its provenance")
	}

	var out bytes.Buffer
	err := holdRunLock(runLockConfig{Path: path, Lease: time.Second, Stamp: `{"kind":"restore"}`}, strings.NewReader(""), &out)
	if !errors.Is(err, errLockBusy) {
		t.Fatalf("a second holder got %v, want errLockBusy", err)
	}
	if out.Len() != 0 {
		t.Errorf("a refused holder wrote %q to stdout; the caller reads the first line as the verdict and must not see a ready marker", out.String())
	}

	// The loser must not have blanked or rewritten the winner's record. An
	// operator deciding whether to break this lock reads exactly this file, and
	// the answer has to describe whoever actually holds it.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read lock file: %v", err)
	}
	if !bytes.Equal(first, after) {
		t.Errorf("the lock file changed when a second holder was refused:\n before %s\n after  %s", first, after)
	}
}

// The provenance recorded is THIS process, whatever the caller claimed, because
// it is the thing whose liveness answers "is this lock still real".
func TestTheStampDescribesTheHoldingProcess(t *testing.T) {
	path := lockPath(t)
	pr, pw := io.Pipe()
	defer pw.Close()

	go func() {
		var out bytes.Buffer
		// A caller claiming somebody else's pid, which must not be believed.
		_ = holdRunLock(runLockConfig{
			Path:  path,
			Lease: 10 * time.Second,
			Stamp: `{"pid":999999,"kind":"sync","source_domain":"web01","target_ref":"dr01:web01","run_id":"run-abc","start_ticks":12345}`,
		}, pr, &out)
	}()

	var id util.RunLockIdentity
	for i := 0; i < 200; i++ {
		time.Sleep(10 * time.Millisecond)
		b, err := os.ReadFile(path)
		if err != nil || len(strings.TrimSpace(string(b))) == 0 {
			continue
		}
		if json.Unmarshal(b, &id) == nil && id.PID != 0 {
			break
		}
	}
	if id.PID != os.Getpid() {
		t.Errorf("stamp records pid %d, want this process (%d): the pid is what a reader checks for liveness, so a claimed one would make a dead lock look held and a live one look free", id.PID, os.Getpid())
	}
	// The descriptive fields ARE the caller's to state -- only the identity
	// fields are taken from here.
	if id.Kind != "sync" || id.SourceDomain != "web01" || id.TargetRef != "dr01:web01" || id.RunID != "run-abc" {
		t.Errorf("the caller's description did not survive: %+v", id)
	}
	if id.StartTicks == 12345 {
		t.Error("the claimed start_ticks was kept; with a claimed value the pid-reuse guard compares against a number this process never had")
	}
}

// Refusals a caller has to be able to tell apart, and the one that is not a
// refusal at all.
func TestRunLockExitMapsEveryOutcome(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		wantMarker string
		wantCode   int
	}{
		{"let go cleanly", nil, "", 0},
		// Not a failure: the lease expiring is the outcome this mode exists to
		// produce, and a non-zero exit would make every successful release look
		// like a broken helper in a log.
		{"lease expired", errLeaseExpired, "", 0},
		{"somebody else holds it", errLockBusy, util.RemoteLockBusy, util.RemoteLockExitBusy},
		{"no lock directory", errLockNoDir, util.RemoteLockNoDir, util.RemoteLockExitNoDir},
		{"cannot create the file", errLockNoCreate, util.RemoteLockNoCreate, util.RemoteLockExitNoCreate},
		{"anything else", errors.New("disk on fire"), "", 1},
	} {
		marker, code := runLockExit(tc.err)
		if marker != tc.wantMarker || code != tc.wantCode {
			t.Errorf("%s: runLockExit = %q/%d, want %q/%d", tc.name, marker, code, tc.wantMarker, tc.wantCode)
		}
	}
}

func TestHoldRunLockRefusesWhatItCannotLock(t *testing.T) {
	var out bytes.Buffer
	if err := holdRunLock(runLockConfig{Path: "locks/relative.lock", Lease: time.Second}, strings.NewReader(""), &out); !errors.Is(err, errLockPathRelative) {
		t.Errorf("a relative lock path gave %v; it names a different file depending on where the login lands, and two drivers could each hold their own", err)
	}
	if err := holdRunLock(runLockConfig{Path: lockPath(t), Lease: 0}, strings.NewReader(""), &out); err == nil {
		t.Error("a zero lease was accepted; it would release the lock immediately and let a second run start alongside the first")
	}
}
