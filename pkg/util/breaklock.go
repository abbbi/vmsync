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
	"fmt"
	"os"
	"strings"
	"time"
)

// Breaking a run lock held on THIS host by a process that is no longer there.
//
// This exists for one situation: a driver on another host took the lock over SSH
// and then died without closing the connection, so the session holding it -- and
// the lock -- survive until sshd's TCP keepalive expires them, roughly two hours.
// Every operation that protects the replica waits behind that lock, which
// includes the promotion the replica exists for. A leased lock does not need
// this; an unleased one, on a target with no vmsync-bridge-helper, has nothing
// else.
//
// The refusals are the substance. Breaking a lock whose holder is alive removes
// the only thing stopping two writers from sharing one replica, so this proves
// the holder is gone rather than assuming it from a stuck-looking session.

// ErrLockHolderAlive reports that the lock's recorded holder is still running, so
// the lock is doing its job and must not be broken.
var ErrLockHolderAlive = errors.New("the run lock's holder is still running")

// ErrLockReplaced is the lock file at a path having been swapped for another
// one. Distinct from ErrLockHolderAlive because the branch that reports it has
// proved nothing about any holder: it knows only that the file it locked is no
// longer the file at that path, which is a different fact and a different
// remedy.
var ErrLockReplaced = errors.New("the run lock file was replaced while it was being locked")

// ErrLockUnattributed reports that the lock file names no holder, so nothing can
// be proven about it either way.
var ErrLockUnattributed = errors.New("the run lock records no holder")

// BreakLockDecision is what BreakRunLockDecision concluded, for a caller that has
// to explain itself before acting.
type BreakLockDecision struct {
	// Break is true only when the holder is provably gone.
	Break bool
	// Reason is one sentence naming the evidence, for the log and for the
	// operator reading a refusal.
	Reason string
	// Holder is what the lock file said, when it said anything.
	Holder RunLockIdentity
	// HasHolder distinguishes "no record" from "a record describing pid 0".
	HasHolder bool
}

// BreakRunLockDecision decides whether the lock for key may be broken, from the
// provenance its holder recorded and the state of this host.
//
// Pure with respect to everything except /proc and the lock file: it kills
// nothing and deletes nothing, so a caller can print the decision and then ask a
// human. The action itself is the caller's. It does take the flock, but only to
// ask whether it could -- see runLockFree, which releases it in the same breath
// and leaves nothing behind.
//
// FAILS CLOSED, the opposite of RunLockHeld, and for the opposite reason. There,
// "cannot tell" means launch and let the real lock decide, which costs a wasted
// process. Here, "cannot tell" would mean removing the interlock between two
// writers on one replica, so every uncertainty refuses: an unreadable record, a
// record naming no pid, a holder whose liveness cannot be established.
func BreakRunLockDecision(dir, key, expectBinary string) BreakLockDecision {
	id, ok, err := ReadRunLockIdentity(dir, key)
	switch {
	case err != nil:
		return BreakLockDecision{Reason: fmt.Sprintf("the lock file for %q could not be read (%v), so its holder cannot be identified", key, err)}
	case !ok:
		// An unleased shell lock records nothing, and so does every lock taken
		// by a vmsync that predates provenance. Refused rather than guessed at.
		return BreakLockDecision{Reason: fmt.Sprintf("the lock for %q records no holder, so there is no evidence that whoever holds it has stopped", key)}
	}

	gone, why := holderProvablyGone(id)
	if !gone {
		return BreakLockDecision{
			Reason: fmt.Sprintf("the lock for %q names pid %d, and %s", key, id.PID, why),
			Holder: id, HasHolder: true,
		}
	}

	// The record says its holder is gone. Now ask the LOCK, because the record
	// describes whoever last WROTE this file and the lock belongs to whoever
	// currently holds the flock, and those are not the same process.
	//
	// Nothing clears the stamp: the file is never unlinked on any release path,
	// and the unleased shell hold (remotelock.go's `exec 9>>`, append-only by
	// design) writes no stamp at all. Every other acquirer -- a sync, -promote,
	// a local -restore, the helper's leased hold -- does stamp, but
	// WriteRunLockIdentity is best-effort by contract, so one failed stamp puts
	// a LIVE holder under a dead predecessor's record again. That is why the
	// stamping cannot be the guarantee and this check is. Every link of it
	// confirmed by test on 2026-10-06. Without this check the decision
	// read "its holder is gone" and unlinked the lock from under a running
	// restore, after which the next sync created a fresh file, won it, and
	// wrote the same qcow2 images the restore was rolling back.
	//
	// Stamping every acquirer would make the record truthful, and should still
	// happen, but it cannot be the guarantee: WriteRunLockIdentity is
	// best-effort by contract, so one failed stamp would bring the hazard
	// straight back. This asks the kernel instead, which no acquirer can forget
	// to do.
	//
	// FAILS CLOSED like the rest of this function: a lock that cannot be tested
	// is a lock that must not be broken.
	if free, whyNot := runLockFree(RunLockPath(dir, key)); !free {
		return BreakLockDecision{
			Reason: fmt.Sprintf("the lock for %q names pid %d and %s -- but %s, so that record describes a previous holder rather than the live one. Breaking it would leave two writers on this replica", key, id.PID, why, whyNot),
			Holder: id, HasHolder: true,
		}
	}
	return BreakLockDecision{
		Break:  true,
		Reason: fmt.Sprintf("the lock for %q names pid %d, %s, and nothing is holding the lock", key, id.PID, why),
		Holder: id, HasHolder: true,
	}
}

// runLockFree reports whether the advisory lock on path could be taken, and
// when it could not, one clause saying why for the refusal to quote.
//
// Taking the lock and letting it go again is the only way to ask: flock exposes
// no "who holds this" query. The probe is non-blocking and the descriptor is
// closed immediately, and because the lock lives on the open file description,
// closing is what releases it -- so a probe leaves nothing behind.
//
// "Held" and "could not be tested" are deliberately NOT told apart. Both have
// to refuse, so distinguishing them would add an errno dependency that cannot
// change the decision -- and EWOULDBLOCK is not even the error every platform
// reports for a contended non-blocking flock.
//
// A missing file is free: there is nothing to hold, and treating that as held
// would make a lock nobody has created unbreakable.
func runLockFree(path string) (bool, string) {
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		if os.IsNotExist(err) {
			return true, ""
		}
		return false, fmt.Sprintf("the lock file could not be opened to test whether anything holds it (%v)", err)
	}
	defer f.Close()

	if err := FlockExclusiveNB(f); err != nil {
		return false, fmt.Sprintf("something still holds the lock, or it could not be tested (%v)", err)
	}
	return true, ""
}

// holderProvablyGone reports whether id's process is CERTAINLY not running, and
// says on what evidence.
//
// Deliberately not RunLockHeld, although the question looks identical. That one
// fails OPEN -- every "cannot tell" answers "not held", so its caller launches and
// lets the real lock decide, costing one wasted process. Reusing it here would
// turn the same uncertainty into "safe to break", so an unreadable /proc on one
// host would remove the interlock between two writers on every replica it holds.
// The two callers need opposite defaults, so they need separate functions.
//
// Three kinds of proof, in the order that settles the most cases first:
//   - a different boot id: the host rebooted, so nothing from before it survives,
//     and this is the one proof that needs no /proc entry at all;
//   - no /proc entry for the pid: the process is gone. Only os.IsNotExist counts;
//     any other error means /proc could not be consulted, which proves nothing;
//   - a /proc entry whose start time differs: the pid was reused, so the holder
//     this lock describes is gone even though something answers to its number.
func holderProvablyGone(id RunLockIdentity) (bool, string) {
	return proveHolderGone(id, gatherHostFacts(id.PID))
}

// hostFacts is everything the host was able to say about a pid, including the
// failures to say it.
//
// It exists so proveHolderGone can be a pure function. The decision it makes is
// the one that removes the interlock between two writers on a replica, and every
// branch of it -- including "there is no such process", "the pid was reused" and
// each way of failing to find out -- has to be provable by a test on any machine,
// not only on one that can be made to have the right /proc state.
type hostFacts struct {
	bootID    string
	bootErr   error
	startTick uint64
	tickErr   error
}

func gatherHostFacts(pid int) hostFacts {
	var f hostFacts
	f.bootID, f.bootErr = CurrentBootID()
	if pid > 0 {
		f.startTick, f.tickErr = ProcStartTicks(pid)
	}
	return f
}

// proveHolderGone is the decision, with the host's answers already gathered.
func proveHolderGone(id RunLockIdentity, f hostFacts) (bool, string) {
	if id.PID <= 0 {
		return false, "that is not a pid, so nothing can be proven about it"
	}
	if id.BootID == "" {
		// Without a boot id a reboot and a crash are indistinguishable, and a pid
		// recycled since the reboot would look like the original holder.
		return false, "its record carries no boot id, so a pid reused since the last reboot could not be told from the original holder"
	}
	switch {
	case f.bootErr != nil:
		return false, fmt.Sprintf("this host's boot id could not be read (%v), so a lock from a previous boot cannot be told from a live one", f.bootErr)
	case f.bootID != id.BootID:
		return true, "it was taken before the last reboot, so that process cannot exist any more"
	}

	switch {
	case os.IsNotExist(f.tickErr):
		return true, "there is no such process on this host"
	case f.tickErr != nil:
		return false, fmt.Sprintf("its liveness could not be established (%v)", f.tickErr)
	case id.StartTicks == 0:
		// Nothing to compare against, so a reused pid is indistinguishable from
		// the original holder -- and something IS running under that number.
		return false, "something is running under that pid and its record carries no start time to rule out pid reuse"
	case f.startTick != id.StartTicks:
		return true, fmt.Sprintf("the process under that pid started at a different time (%d, not %d), so the holder was replaced by an unrelated one", f.startTick, id.StartTicks)
	default:
		return false, "it is still running"
	}
}

// DescribeLockHolder renders a holder for an operator, or says plainly that
// nothing is recorded.
//
// Used by the refusals that must not be a bare exit 75. "Another vmsync is
// working on this" is not actionable during a disaster; "the sync from hv-a that
// started at 02:14 holds it, run id 0b9f5c2e" is.
//
// THREE OUTCOMES, NOT TWO, and the third is the one this had wrong. The record
// describes whoever last WROTE the file; the lock belongs to whoever holds the
// flock, and those are not the same process. Nothing unlinks the file on any
// release path, the unleased shell hold stamps nothing at all, and every other
// stamp is best-effort by contract -- so a live holder routinely inherits a
// dead predecessor's record, and a description that reads only the record
// calls a live lock stale.
//
// Not a corner case here: the caller reaches this ONLY after AcquireRunLock
// returned ErrLockHeld, so the kernel has just proven something holds the
// lock. "Stale, safe to break" was wrong by construction every time the record
// looked dead. And it composed badly -- -break-target-lock then refuses and
// tells the operator to end the holder "if that process is genuinely gone",
// which vmsync has just said it is. During a promote, in the disaster the
// replica exists for, that reads as an instruction to kill whatever holds the
// lock. Sometimes that is a live restore rolling the disks back.
//
// So it asks the same two questions BreakRunLockDecision asks, in the same
// order, and has to keep agreeing with it: a description that promises what
// the verb refuses is worse than the bare exit 75 it replaced, which at least
// made the operator look. Deliberately holderProvablyGone and not RunLockHeld
// -- that one FAILS OPEN by design, every "cannot tell" answering "not held",
// which is the right direction for deciding whether to launch a process and
// exactly the wrong one for telling an operator a lock is safe to break. On a
// host where /proc cannot be read for another user's pid it called every live
// holder stale.
//
// THE THIRD OUTCOME MUST NOT TELL THE OPERATOR TO END THE HOLDER. It has proven
// the RECORDED holder gone, so whatever holds the flock is somebody else -- and
// nothing here can say who. In the states that reach it that somebody is a live
// sync or a live restore at least as often as it is an abandoned session, so a
// bare "end whatever holds the lock" is the two-writer incident with an extra
// step removed: the operator does not even have to run the verb to get there.
// It offers the wait first, carries the same hazard clause
// BreakRunLockDecision states for this state, and only then says how to
// identify an orphan.
//
// A caller that holds this lock itself would see its own hold as "something
// still holds the lock": flock lives on the open file description, so the
// probe's fresh descriptor contends with it. Both this and
// BreakRunLockDecision describe locks their caller failed to take.
func DescribeLockHolder(dir, key string) string {
	id, ok, err := ReadRunLockIdentity(dir, key)
	if err != nil {
		return fmt.Sprintf("its holder could not be identified (%v)", err)
	}
	if !ok {
		return "it records no holder, which is what an older vmsync and the unleased shell lock both leave"
	}
	var b strings.Builder
	if id.PID > 0 {
		fmt.Fprintf(&b, "held by pid %d", id.PID)
	} else {
		// ReadRunLockIdentity accepts any parseable object, so a record with
		// no pid field arrives here as pid 0. "held by pid 0" stated a fact
		// about a process that cannot exist.
		b.WriteString("held by something the record does not name (it carries no pid)")
	}
	if id.Kind != "" {
		fmt.Fprintf(&b, " running a %s", id.Kind)
	}
	if id.SourceDomain != "" {
		fmt.Fprintf(&b, " of %s", id.SourceDomain)
	}
	if id.TargetRef != "" {
		fmt.Fprintf(&b, " into %s", id.TargetRef)
	}
	if id.StartedAtUnix > 0 {
		fmt.Fprintf(&b, ", started %s", time.Unix(id.StartedAtUnix, 0).UTC().Format(time.RFC3339))
	}
	if id.RunID != "" {
		fmt.Fprintf(&b, ", run id %s", id.RunID)
	}
	gone, why := holderProvablyGone(id)
	if !gone {
		// One sentence for all six of proveHolderGone's refusals, from "it is
		// still running" to the four that cannot tell. It must not lead with a
		// hedge, because one of them is a certainty, and it must not point at
		// the holder: in the cannot-tell states the recorded pid may BE the
		// live holder, so the advice that is safe in the branch below would be
		// an instruction to kill a running sync here.
		fmt.Fprintf(&b, " -- the lock stands (%s). Waiting is the safe option here: in this state the record may be describing the live holder, so identify it before ending anything", why)
		return b.String()
	}
	if free, whyNot := runLockFree(RunLockPath(dir, key)); !free {
		fmt.Fprintf(&b, " -- that process is gone (%s), but %s, so this record describes a previous holder rather than the live one, and the live one is unidentified. It may be a sync or a restore writing this replica right now, and ending it would leave two writers on this replica -- the thing this lock exists to prevent. So waiting is the first option: a leased hold releases itself one lease after its driver stops beating, and an unleased one goes when this host's sshd expires the session (see ClientAliveInterval). -break-target-lock refuses this state and has no override; if the holder does turn out to be an abandoned session, `ss -tp | grep sshd` and `loginctl`/`ps` name it", why, whyNot)
		return b.String()
	}
	fmt.Fprintf(&b, " -- NOT running (%s), and nothing is holding the lock, so it is stale and -break-target-lock can clear it", why)
	return b.String()
}

// BreakRunLock removes the lock file, so the next acquirer creates a fresh one
// and locks that instead.
//
// IT TAKES THE LOCK BEFORE UNLINKING IT, and refuses if it cannot. The decision
// is made by BreakRunLockDecision, which does probe the flock -- but that probe
// releases in the same breath, and the caller then connects to libvirt, reads a
// domain definition and appends a journal record before arriving here. A run
// that acquires the lock inside that window used to have its lock file unlinked
// from under it, and nothing told it: its flock survives on the orphaned inode,
// the next acquirer creates a fresh file and wins that, and two writers share
// one replica. Measured on 2026-10-18, both inodes named in the output.
//
// Holding the lock closes the window for every acquirer that checks, after
// locking, that it locked the file the path still names -- AcquireRunLock does,
// and so does vmsync-bridge-helper's leased hold. Such a run either already
// holds the lock, in which case the non-blocking flock below fails and nothing
// is removed; or its own acquisition is refused while this holds the lock; or
// it locked the old inode and its own re-check sends it round again. Every one
// of those is a refusal or a retry.
//
// The unleased shell hold is the exception, and it is narrowed rather than
// closed: remotelock.go parks `exec 9>>` and `flock -n 9` in a POSIX shell that
// compares no inodes, so a shell holding the file open before this takes the
// lock can still flock the orphan afterwards and believe it holds the run lock.
// Nothing on this side can fix that -- the shell would have to compare inodes
// itself -- and the window is now only between its own open and its own flock.
//
// Unlinking still revokes nobody's flock -- nothing can, from outside the
// process owning the descriptor -- and it does not need to: the lock lives on
// the inode, and after the unlink no acquirer ever sees that inode again. The
// orphan here goes when this function returns.
//
// Reports whether a file was actually removed, so a caller can tell "broken" from
// "there was nothing there", which look identical afterwards.
// lockStillAtPath is RunLockIsAtPath, indirected for one reason: the window it
// guards is between BreakRunLock's own OpenFile and its own Flock, two adjacent
// statements, and nothing outside the function can be scheduled into it. Every
// structural way of asserting the check is present is satisfied by syntax --
// measured on 2026-10-19, where a bare call, a call behind an always-false
// condition and a call moved into a defer all left the suite green. A seam on
// the STAT lets a test force the answer and watch what BreakRunLock does with
// it, which is the behaviour that matters; it fakes neither the flock nor the
// unlink, so nothing about the interlock itself becomes test-controlled.
var lockStillAtPath = RunLockIsAtPath

func BreakRunLock(dir, key string) (bool, error) {
	path := RunLockPath(dir, key)

	// No O_CREATE: a lock file that is already gone is nothing to break, and
	// creating one here only to remove it again would turn "there was nothing
	// there" into "broken".
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("open the lock file %s in order to break it: %w", path, err)
	}
	defer f.Close()

	if err := FlockExclusiveNB(f); err != nil {
		return false, fmt.Errorf("refusing to remove the lock file %s: something has taken the lock since the decision to break it was made (%v) -- removing it now would leave two writers on this replica: %w", path, err, ErrLockHolderAlive)
	}
	// Locked, but possibly not the lock that is at path any more: the file
	// could have been replaced between the open above and the flock just
	// taken. Removing it then would unlink somebody else's live lock while
	// holding a worthless one, which is precisely the thing this function
	// exists to stop doing.
	if !lockStillAtPath(f, path) {
		return false, fmt.Errorf("refusing to remove the lock file %s: it was replaced between being opened and being locked, so the file there now is not the one this locked: %w", path, ErrLockReplaced)
	}

	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("remove the lock file %s: %w", path, err)
	}
	return true, nil
}
