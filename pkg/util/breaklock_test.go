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
	// The reason has to name the evidence, because it is what an operator reads
	// before removing an interlock, and it is journalled as the justification.
	// Matched against the three proofs holderProvablyGone accepts rather than
	// against one wording: which one applies depends on the host, and pinning a
	// phrase would fail on a host that answered correctly by a different route.
	proofs := []string{"no such process", "before the last reboot", "started at a different time"}
	named := false
	for _, p := range proofs {
		if strings.Contains(d.Reason, p) {
			named = true
			break
		}
	}
	if !named {
		t.Errorf("reason %q names none of the proofs that justify breaking a lock (%v), so the journalled justification would not say why it was safe", d.Reason, proofs)
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

// Every branch of the break decision, on any host.
//
// This is the function that removes the interlock between two writers on one
// replica, and until the host's answers were made an argument it could only be
// exercised where /proc happened to be in the right state -- which is how an
// assertion in this file shipped having never run. Each case below is a state a
// real host reaches; none of them needs that host.
func TestProveHolderGoneCoversEveryBranch(t *testing.T) {
	const boot = "11111111-2222-3333-4444-555555555555"
	live := RunLockIdentity{PID: 4242, BootID: boot, StartTicks: 900}

	for _, tc := range []struct {
		name      string
		id        RunLockIdentity
		facts     hostFacts
		wantBreak bool
		wantSays  string
	}{
		{
			// The proof that needs no /proc entry at all, and the only one that
			// works for a pid the kernel has since handed to something else.
			name: "the host rebooted since the lock was taken",
			id:   live, facts: hostFacts{bootID: "99999999-0000-0000-0000-000000000000", startTick: 900},
			wantBreak: true, wantSays: "before the last reboot",
		},
		{
			name: "the process is simply gone",
			id:   live, facts: hostFacts{bootID: boot, tickErr: os.ErrNotExist},
			wantBreak: true, wantSays: "no such process",
		},
		{
			// Something answers to the pid, but it started at a different time,
			// so it is not the holder this lock describes.
			name: "the pid was reused by something else",
			id:   live, facts: hostFacts{bootID: boot, startTick: 51234},
			wantBreak: true, wantSays: "started at a different time",
		},
		{
			name: "the holder is still running",
			id:   live, facts: hostFacts{bootID: boot, startTick: 900},
			wantBreak: false, wantSays: "still running",
		},
		// Every refusal below is a "cannot tell". They must all refuse: this is
		// the direction RunLockHeld gets wrong for this caller, where guessing
		// costs a replica rather than a wasted process.
		{
			name: "the boot id cannot be read",
			id:   live, facts: hostFacts{bootErr: os.ErrPermission, startTick: 900},
			wantBreak: false, wantSays: "boot id could not be read",
		},
		{
			name: "the record carries no boot id",
			id:   RunLockIdentity{PID: 4242, StartTicks: 900}, facts: hostFacts{bootID: boot, startTick: 900},
			wantBreak: false, wantSays: "no boot id",
		},
		{
			name: "proc could not be consulted for any other reason",
			id:   live, facts: hostFacts{bootID: boot, tickErr: os.ErrPermission},
			wantBreak: false, wantSays: "could not be established",
		},
		{
			// A partial record: something is running under that pid and there is
			// no start time to rule out reuse, so reuse cannot be ruled out.
			name: "the record carries no start time",
			id:   RunLockIdentity{PID: 4242, BootID: boot}, facts: hostFacts{bootID: boot, startTick: 900},
			wantBreak: false, wantSays: "no start time",
		},
		{
			name: "the record names no pid",
			id:   RunLockIdentity{BootID: boot}, facts: hostFacts{bootID: boot},
			wantBreak: false, wantSays: "not a pid",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gone, why := proveHolderGone(tc.id, tc.facts)
			if gone != tc.wantBreak {
				t.Errorf("proveHolderGone = %v, want %v (%s)", gone, tc.wantBreak, why)
			}
			if !strings.Contains(why, tc.wantSays) {
				t.Errorf("reason %q does not mention %q, and the reason is what an operator reads before removing an interlock", why, tc.wantSays)
			}
		})
	}
}
