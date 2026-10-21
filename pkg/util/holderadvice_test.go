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
	"fmt"
	"os"
	"strings"
	"testing"
)

// deadNamedHolder is deadIdentity carrying the fields an operator reads, so the
// descriptions asserted below are the ones a promote refusal actually prints.
//
// Dead by the reboot proof -- deadIdentity carries a boot id that is not this
// host's -- so nothing here depends on a particular pid being absent from
// /proc.
func deadNamedHolder() RunLockIdentity {
	id := deadIdentity()
	id.Kind = "sync"
	id.SourceDomain = "web01"
	id.TargetRef = "dr01:web01"
	id.RunID = "run-dead"
	id.StartedAtUnix = 1800000000
	return id
}

// holdTheLock takes the real flock on a lock file and keeps it for the test.
//
// This is the state that tells a stale record from a live holder, and the only
// way to produce it: flock exposes no way to say "pretend this is held".
func holdTheLock(t *testing.T, dir, key string) {
	t.Helper()
	f, err := os.OpenFile(RunLockPath(dir, key), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatalf("open the lock file: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	if err := FlockExclusiveNB(f); err != nil {
		t.Fatalf("take the flock: %v", err)
	}
	// Checked, not assumed. A flock belongs to the open file description, so
	// this process's own second descriptor contends with it exactly as another
	// process's would -- but if that ever stopped holding, every "lock is live"
	// case below would quietly become a "lock is free" case and keep passing
	// against the behaviour it exists to forbid.
	if free, _ := runLockFree(RunLockPath(dir, key)); free {
		t.Fatal("a lock this test holds reads as free, so the live-lock cases prove nothing")
	}
}

func writeRaw(t *testing.T, dir, key, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(RunLockPath(dir, key), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// The advice and the verb must never disagree, and each outcome has to say
// something.
//
// DescribeLockHolder is printed by a refusal and names -break-target-lock as the
// way past it. BreakRunLockDecision is what that verb then consults, and it has
// no override. So a description that says the lock can be cleared while the
// decision refuses sends an operator down a dead end mid-disaster -- and the
// refusal waiting there says to end the holder "if that process is genuinely
// gone", which the description has just told them it is.
//
// Two separate properties, because the first alone is satisfied by a function
// that says nothing at all:
//   - the clearance claim agrees with BreakRunLockDecision, both ways;
//   - each outcome still states its verdict, pinned by wantSays.
func TestHolderAdviceAgreesWithTheBreakDecision(t *testing.T) {
	cases := []struct {
		name string
		// needsProof marks the cases that depend on this host being able to
		// prove a holder dead. The others are decided before any proof is
		// needed, so they must not be skipped with them.
		needsProof bool
		setUp      func(t *testing.T, dir, key string)
		// wantSays is a substring of the verdict this state must produce, so
		// an outcome cannot regress into silence while the agreement below
		// still holds.
		wantSays string
		// wantClear is what BOTH halves must say about breaking the lock.
		wantClear bool
	}{
		{
			name:     "no lock file at all",
			setUp:    func(t *testing.T, dir, key string) {},
			wantSays: "records no holder",
		},
		{
			name:     "an empty file, which is what the unleased shell hold creates",
			setUp:    func(t *testing.T, dir, key string) { writeRaw(t, dir, key, "") },
			wantSays: "records no holder",
		},
		{
			name:     "a record that cannot be parsed",
			setUp:    func(t *testing.T, dir, key string) { writeRaw(t, dir, key, "{not json") },
			wantSays: "could not be identified",
		},
		{
			// ReadRunLockIdentity accepts any parseable object, so a record
			// without a pid field reaches the describer as pid 0. "held by pid
			// 0" named a process that cannot exist and read as a fact about
			// one.
			name:     "a record that parses but names no pid",
			setUp:    func(t *testing.T, dir, key string) { writeRaw(t, dir, key, `{"kind":"sync"}`) },
			wantSays: "carries no pid",
		},
		{
			// The genuinely stale lock the verb exists for: the driver died and
			// took its ssh session with it.
			name:       "a dead holder and nothing holding the lock",
			needsProof: true,
			setUp:      func(t *testing.T, dir, key string) { writeStamp(t, dir, key, deadNamedHolder()) },
			wantSays:   "-break-target-lock can clear it",
			wantClear:  true,
		},
		{
			// THE REGRESSION. An unstamped acquirer -- the unleased shell hold,
			// or any acquirer whose best-effort stamp failed -- inherits a dead
			// predecessor's record. The record is dead, the lock is live, and
			// the old description called that stale and safe to break.
			name:       "a dead record under a live lock",
			needsProof: true,
			setUp: func(t *testing.T, dir, key string) {
				writeStamp(t, dir, key, deadNamedHolder())
				holdTheLock(t, dir, key)
			},
			wantSays: "previous holder",
		},
		{
			// Fail-open against fail-closed, with no lock held, so this
			// isolates the predicate from the kernel probe. RunLockHeld answers
			// "not held" here without needing a boot id; holderProvablyGone
			// refuses, because a pid recycled since the last reboot reads
			// identically.
			name: "a dead pid whose record carries no boot id",
			setUp: func(t *testing.T, dir, key string) {
				noBoot := deadNamedHolder()
				noBoot.BootID = ""
				writeStamp(t, dir, key, noBoot)
			},
			wantSays: "the lock stands",
		},
		{
			name: "this very process, which is certainly alive",
			setUp: func(t *testing.T, dir, key string) {
				writeStamp(t, dir, key, NewRunLockIdentity("restore", "web01", "dr01:web01", "run-live", 1800000000))
			},
			wantSays: "the lock stands",
		},
	}

	// Computed from the table rather than from the run, so it cannot report a
	// false failure when a single subtest is selected with -run.
	clearing := 0
	for _, tc := range cases {
		if tc.wantClear {
			clearing++
		}
	}
	if clearing == 0 {
		t.Fatal("no case in this table expects clearance advice, so the agreement it proves is the trivial one: two functions that always refuse")
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.needsProof {
				requireProvableHolderDeath(t)
			}
			dir, key := t.TempDir(), "target-web01"
			tc.setUp(t, dir, key)

			desc := DescribeLockHolder(dir, key)
			saysClear := strings.Contains(desc, "-break-target-lock can clear it")
			decision := BreakRunLockDecision(dir, key, "")

			if saysClear != decision.Break {
				t.Errorf("the description and the verb disagree: description says clearable=%t, BreakRunLockDecision says Break=%t\n  description: %s\n  decision:    %s",
					saysClear, decision.Break, desc, decision.Reason)
			}
			if saysClear != tc.wantClear {
				t.Errorf("description says clearable=%t, want %t: %s", saysClear, tc.wantClear, desc)
			}
			if decision.Break != tc.wantClear {
				t.Errorf("BreakRunLockDecision says Break=%t, want %t: %s", decision.Break, tc.wantClear, decision.Reason)
			}
			if !strings.Contains(desc, tc.wantSays) {
				t.Errorf("description does not state its verdict: want it to contain %q, got %s", tc.wantSays, desc)
			}
		})
	}
}

// What the operator is told when the record is dead and the lock is not.
//
// The agreement test proves the description stops PROMISING the clearance. It
// cannot prove the description is any use, and the wrong advice here is worse
// than none: this is printed by a promote refused during a disaster, and the
// live holder is UNIDENTIFIED. In the reachable states it is a sync or a restore
// writing this replica, so an unqualified "end whatever holds the lock" is the
// two-writer incident the lock exists to prevent -- the same composition the
// describer's own doc comment set out to remove.
func TestALiveLockWithADeadRecordTellsTheOperatorWhatToDo(t *testing.T) {
	requireProvableHolderDeath(t)
	id := deadNamedHolder()
	dir, key := t.TempDir(), "target-web01"
	writeStamp(t, dir, key, id)
	holdTheLock(t, dir, key)

	got := DescribeLockHolder(dir, key)
	for _, want := range []string{
		// Who the record names, so the operator can tell it from the holder.
		fmt.Sprintf("pid %d", id.PID),
		id.RunID,
		// That the record is not the holder, which is the whole finding.
		"previous holder",
		// That the live holder is not known, so it cannot be ended blind.
		"unidentified",
		// The cost of getting it wrong, which BreakRunLockDecision states for
		// this same state and this used to drop.
		"two writers",
		// The option that is always safe. Its position relative to the
		// unsafe one is asserted below rather than left to the order of this
		// list, which proves nothing about the text.
		"ait for",
		// That the obvious next command will refuse, before they try it.
		"refuses",
		// And how to identify an orphan, if that is what it turns out to be.
		"sshd",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("description is missing %q, so an operator reading it mid-disaster is not told enough:\n%s", want, got)
		}
	}
	if strings.Contains(got, "this lock is stale") {
		t.Errorf("a live lock is described as stale: %s", got)
	}

	// THE ORDER, which is the point and not the politeness. Ending the holder
	// is the faster path and the dangerous one, and in this state the live
	// holder is explicitly unidentified -- so the description must offer
	// waiting before it mentions ending anything.
	lower := strings.ToLower(got)
	wait, end := strings.Index(lower, "wait for"), strings.Index(lower, "end the process")
	switch {
	case wait < 0:
		t.Errorf("the description never offers waiting:\n%s", got)
	case end < 0:
		t.Errorf("the description never names the lever for when waiting will not do:\n%s", got)
	case end < wait:
		t.Errorf("the description tells an operator to end the holder before it tells them they can wait, in the one state where the holder is unidentified:\n%s", got)
	}
	// The imperative must be conditional. "find and end whatever holds the
	// lock" as an instruction is how a live restore gets killed.
	if strings.Contains(got, "find and end whatever holds the lock") {
		t.Errorf("the description instructs the operator to end an unidentified holder: %s", got)
	}
}

// The probe's third exit, which nothing covered.
//
// runLockFree has three: missing file (free), contended flock (held), and open()
// failed for any other reason (held). The first two are covered by
// TestRunLockFreeDistinguishesHeldFromFree. The third is the one a non-root
// operator reaches on a real target, where the lock file belongs to whatever ran
// the sync -- and it is the whole fail-closed contract, so flipping it to
// fail-open must not pass.
//
// A directory at the lock path produces it on any host and needs no second uid:
// open(O_RDWR) on a directory is EISDIR, which is not os.IsNotExist.
func TestRunLockFreeRefusesWhatItCannotOpen(t *testing.T) {
	path := RunLockPath(t.TempDir(), "target-web01")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("put a directory at the lock path: %v", err)
	}

	free, why := runLockFree(path)
	if free {
		t.Error("a lock file that could not be opened read as FREE, so a lock that cannot be tested would be broken -- the one thing this probe must never do")
	}
	if !strings.Contains(why, "could not be opened") {
		t.Errorf("the refusal does not say the lock could not be tested, so the operator cannot tell it from a live holder: %q", why)
	}
}
