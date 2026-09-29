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
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// writeStamp puts a provenance record in a lock file the way a holder would.
func writeStamp(t *testing.T, dir, key string, id RunLockIdentity) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	b, err := json.Marshal(id)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(RunLockPath(dir, key), append(b, '\n'), 0o644); err != nil {
		t.Fatalf("write stamp: %v", err)
	}
}

// Breaking a lock removes the one thing keeping two writers off a single
// replica, so every uncertainty has to refuse. This is the direction that
// matters: RunLockHeld fails OPEN because guessing wrong there costs a wasted
// process, while guessing wrong here costs a corrupted replica.
func TestBreakRefusesEveryUncertainty(t *testing.T) {
	dir := t.TempDir()

	// No lock file at all.
	if d := BreakRunLockDecision(dir, "target-web01", ""); d.Break {
		t.Error("a missing lock file was treated as breakable; there is nothing to break and nothing was proven")
	}

	// A file with no record: what the unleased shell lock leaves, and what every
	// vmsync before provenance left.
	if err := os.WriteFile(RunLockPath(dir, "target-nostamp"), nil, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	d := BreakRunLockDecision(dir, "target-nostamp", "")
	if d.Break {
		t.Error("a lock recording no holder was broken; nothing about it can be proven either way")
	}
	if d.HasHolder {
		t.Error("HasHolder is true for a lock with no record")
	}
	if !strings.Contains(d.Reason, "no holder") {
		t.Errorf("reason %q does not say the record is missing", d.Reason)
	}

	// Unparseable.
	if err := os.WriteFile(RunLockPath(dir, "target-torn"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if d := BreakRunLockDecision(dir, "target-torn", ""); d.Break {
		t.Error("a lock whose record could not be read was broken")
	}

	// A record naming no pid.
	writeStamp(t, dir, "target-nopid", RunLockIdentity{Kind: "sync"})
	if d := BreakRunLockDecision(dir, "target-nopid", ""); d.Break {
		t.Error("a record naming no pid was broken; there is no process to prove gone")
	}
}

// The live holder is the case that must never be broken: this process is running
// and holds the lock, so breaking it would let a second writer in.
func TestBreakRefusesALiveHolder(t *testing.T) {
	dir := t.TempDir()
	writeStamp(t, dir, "target-web01", NewRunLockIdentity("sync", "web01", "dr01:web01", "run-abc", 1800000000))

	d := BreakRunLockDecision(dir, "target-web01", "")
	if d.Break {
		t.Fatalf("the lock held by this very process was declared breakable: %s", d.Reason)
	}
	if !d.HasHolder || d.Holder.PID != os.Getpid() {
		t.Errorf("holder = %+v, want this process", d.Holder)
	}
	if d.Reason == "" {
		t.Error("the refusal gives no reason, so an operator cannot tell a live holder from an unreadable one")
	}
	// The exact evidence depends on what this host exposes: where /proc is
	// available the holder is found running, and where it is not the decision
	// refuses for want of proof. Both are correct and both refuse; asserting the
	// first wording only would make this test pass for the wrong reason
	// somewhere it happens to hold.
	if _, err := CurrentBootID(); err == nil {
		if !strings.Contains(d.Reason, "still running") {
			t.Errorf("reason %q does not say the holder is alive, although this host can see /proc", d.Reason)
		}
	}
}

// And a holder that is genuinely gone IS breakable, or the escape does not
// exist and the replica stays hostage for the full sshd keepalive window.
func TestBreakAllowsAHolderThatIsGone(t *testing.T) {
	dir := t.TempDir()
	// A pid that cannot be running: the kernel's maximum is far below this.
	bootID, bootErr := CurrentBootID()
	if bootErr != nil {
		t.Skip("this host has no /proc boot id, so \"provably gone\" cannot be established here at all")
	}
	writeStamp(t, dir, "target-web01", RunLockIdentity{PID: 1 << 30, BootID: bootID, Kind: "sync", SourceDomain: "web01", StartTicks: 42})

	d := BreakRunLockDecision(dir, "target-web01", "")
	if !d.Break {
		t.Fatalf("a lock naming a pid that cannot exist was not breakable: %s -- during a DR event this is the whole escape", d.Reason)
	}
	if !strings.Contains(d.Reason, "gone") {
		t.Errorf("reason %q does not say the holder is gone", d.Reason)
	}
}

// Unlinking is what frees the path for the next acquirer; the dead holder's
// flock stays on an inode nobody will ever open again.
func TestBreakRunLockRemovesTheFileAndSaysWhether(t *testing.T) {
	dir := t.TempDir()
	writeStamp(t, dir, "target-web01", RunLockIdentity{PID: 1 << 30})
	_ = os.Getpid()

	broke, err := BreakRunLock(dir, "target-web01")
	if err != nil || !broke {
		t.Fatalf("BreakRunLock = %v, %v; want true, nil", broke, err)
	}
	if _, err := os.Stat(RunLockPath(dir, "target-web01")); !os.IsNotExist(err) {
		t.Errorf("the lock file is still there: %v", err)
	}
	// Idempotent, and it says so: "broken" and "there was nothing there" look
	// identical afterwards, and a caller reporting the wrong one misleads.
	broke, err = BreakRunLock(dir, "target-web01")
	if err != nil {
		t.Fatalf("second BreakRunLock: %v", err)
	}
	if broke {
		t.Error("breaking an absent lock reported that it removed something")
	}
}

// The holder description replaces a bare exit 75, so it has to carry what an
// operator needs and has to say plainly when it knows nothing.
func TestDescribeLockHolderIsActionable(t *testing.T) {
	dir := t.TempDir()

	if got := DescribeLockHolder(dir, "target-absent"); !strings.Contains(got, "records no holder") {
		t.Errorf("describing an unattributed lock gave %q", got)
	}

	writeStamp(t, dir, "target-web01", RunLockIdentity{
		PID: 1 << 30, Kind: "sync", SourceDomain: "web01",
		TargetRef: "dr01:web01", RunID: "run-abc", StartedAtUnix: 1800000000,
	})
	got := DescribeLockHolder(dir, "target-web01")
	for _, want := range []string{"sync", "web01", "dr01:web01", "run-abc", "2027-01-15"} {
		if !strings.Contains(got, want) {
			t.Errorf("description %q is missing %q", got, want)
		}
	}
	// And it tells the operator the lock is stale, which is the actionable half.
	if !strings.Contains(got, "-break-target-lock") {
		t.Errorf("description %q does not name the way past a stale lock", got)
	}
}
