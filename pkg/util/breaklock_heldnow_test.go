/*
	Copyright (C) 2026  Orsiris de Jong <ozy@netpower.fr>
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
	"os"
	"path/filepath"
	"testing"
)

// deadIdentity is a record whose holder is provably gone: the boot id does not
// match this host's, which proveHolderGone accepts as proof on its own and
// which needs no /proc entry. That is deliberately the SAME evidence the real
// failure had -- a stamp left by a process that has since exited.
func deadIdentity() RunLockIdentity {
	id := NewRunLockIdentity("sync", "src", "tgt:dst", "action-id", 1)
	id.BootID = "not-this-hosts-boot-id"
	id.PID = 424242
	return id
}

// requireProvableHolderDeath skips when this host cannot prove a holder is
// gone, which is what every decision-level test below depends on.
//
// Keyed on the mechanism rather than on runtime.GOOS: proveHolderGone's three
// proofs all need Linux host facts (/proc/sys/kernel/random/boot_id, and a
// /proc entry for the pid), so on a development machine without them it
// returns "cannot be told" and BreakRunLockDecision refuses before it ever
// reaches the lock probe. A test left running there would pass while asserting
// nothing -- which is exactly how the first version of this file reported
// green on Windows for a check that never executed.
func requireProvableHolderDeath(t *testing.T) {
	t.Helper()
	if gone, _ := holderProvablyGone(deadIdentity()); !gone {
		t.Skip("this host cannot prove a lock holder is gone (no /proc, no boot id), so the decision refuses before the lock is ever probed")
	}
}

// stampedLock writes a lock file carrying id, and returns dir and key.
func stampedLock(t *testing.T, id RunLockIdentity) (string, string) {
	t.Helper()
	dir := t.TempDir()
	key := "target-vm"
	f, err := os.OpenFile(RunLockPath(dir, key), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("create the lock file: %v", err)
	}
	defer f.Close()
	if err := WriteRunLockIdentity(f, id); err != nil {
		t.Fatalf("write the lock identity: %v", err)
	}
	return dir, key
}

// TestBreakRunLockDecisionRefusesWhileTheLockIsStillHeld is the regression
// guard for the defect this check exists to close.
//
// The record says its holder is gone AND that is true -- but the record
// describes whoever last WROTE the file, and the lock belongs to whoever holds
// the flock. Nothing clears the stamp on any release path, and three acquirers
// of the target lock write none of their own, so a live holder routinely
// inherits a dead predecessor's record. Deciding from the record alone unlinked
// the lock from under a running restore.
func TestBreakRunLockDecisionRefusesWhileTheLockIsStillHeld(t *testing.T) {
	requireProvableHolderDeath(t)
	dir, key := stampedLock(t, deadIdentity())

	// A live holder that wrote nothing, exactly like the unleased shell hold,
	// -promote and a local -restore.
	holder, err := os.OpenFile(RunLockPath(dir, key), os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open the lock file as a holder: %v", err)
	}
	defer holder.Close()
	if err := FlockExclusiveNB(holder); err != nil {
		t.Fatalf("take the lock as the live holder: %v", err)
	}

	d := BreakRunLockDecision(dir, key, "")
	if d.Break {
		t.Errorf("decided to BREAK a lock that is still held; reason was %q", d.Reason)
	}
	if !d.HasHolder {
		t.Error("the stale record should still be reported as a holder, so the refusal can name it")
	}
}

// TestBreakRunLockDecisionBreaksWhenNothingHoldsTheLock keeps the check from
// becoming a refusal of everything. A genuinely abandoned lock -- a stamp whose
// holder is gone and no process holding the flock -- is the case the verb
// exists for, and it must still go through.
func TestBreakRunLockDecisionBreaksWhenNothingHoldsTheLock(t *testing.T) {
	requireProvableHolderDeath(t)
	dir, key := stampedLock(t, deadIdentity())

	d := BreakRunLockDecision(dir, key, "")
	if !d.Break {
		t.Errorf("refused to break an abandoned lock, so -break-target-lock can never help anyone; reason was %q", d.Reason)
	}
}

// TestBreakRunLockDecisionStillRefusesAnUnattributedLock pins the behaviour
// that was already correct, so the new check cannot be mistaken for the whole
// guard: a lock recording nothing is refused before liveness is ever consulted.
func TestBreakRunLockDecisionStillRefusesAnUnattributedLock(t *testing.T) {
	dir := t.TempDir()
	key := "target-vm"
	if err := os.WriteFile(RunLockPath(dir, key), nil, 0o644); err != nil {
		t.Fatalf("create an empty lock file: %v", err)
	}

	d := BreakRunLockDecision(dir, key, "")
	if d.Break {
		t.Errorf("decided to break a lock that records no holder; reason was %q", d.Reason)
	}
	if d.HasHolder {
		t.Error("an empty lock file records no holder")
	}
}

// TestRunLockFreeDistinguishesHeldFromFree exercises the probe on its own,
// including the missing-file case: nothing can hold a lock that does not exist,
// and reporting that as held would make a lock nobody created unbreakable.
func TestRunLockFreeDistinguishesHeldFromFree(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "probe.lock")

	if free, why := runLockFree(path); !free {
		t.Errorf("a missing lock file read as not free: %s", why)
	}

	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("create the lock file: %v", err)
	}
	if free, why := runLockFree(path); !free {
		t.Errorf("an unheld lock file read as not free: %s", why)
	}

	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open the lock file: %v", err)
	}
	defer f.Close()
	if err := FlockExclusiveNB(f); err != nil {
		t.Fatalf("take the lock: %v", err)
	}
	if free, why := runLockFree(path); free {
		t.Errorf("a HELD lock file read as free (why=%q) -- the break decision would then remove a live holder's lock", why)
	}
}

// TestRunLockFreeReleasesWhatItProbes is the subtle one. The probe TAKES the
// lock to find out whether it was free, so a probe that forgot to release would
// leave the lock held by the process that was only asking -- and the very next
// acquirer, including the sync that runs after a successful break, would be
// refused for ever.
func TestRunLockFreeReleasesWhatItProbes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "probe.lock")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("create the lock file: %v", err)
	}

	for i := 0; i < 3; i++ {
		if free, why := runLockFree(path); !free {
			t.Fatalf("probe %d read the lock as not free (%s) -- an earlier probe did not let go", i, why)
		}
	}

	// And a real acquirer can still take it afterwards.
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open the lock file: %v", err)
	}
	defer f.Close()
	if err := FlockExclusiveNB(f); err != nil {
		t.Errorf("could not take the lock after probing it: %v", err)
	}
}
