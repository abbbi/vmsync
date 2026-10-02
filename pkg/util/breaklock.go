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
// Pure with respect to everything except /proc and the lock file: it takes no
// lock, kills nothing and deletes nothing, so a caller can print the decision and
// then ask a human. The action itself is the caller's.
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
	return BreakLockDecision{
		Break:  true,
		Reason: fmt.Sprintf("the lock for %q names pid %d, and %s", key, id.PID, why),
		Holder: id, HasHolder: true,
	}
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
func DescribeLockHolder(dir, key string) string {
	id, ok, err := ReadRunLockIdentity(dir, key)
	if err != nil {
		return fmt.Sprintf("its holder could not be identified (%v)", err)
	}
	if !ok {
		return "it records no holder, which is what an older vmsync and the unleased shell lock both leave"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "held by pid %d", id.PID)
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
	if alive, why := RunLockHeld(id, ""); alive {
		fmt.Fprintf(&b, " -- still running (%s)", why)
	} else {
		fmt.Fprintf(&b, " -- NOT running (%s), so this lock is stale and -break-target-lock can clear it", why)
	}
	return b.String()
}

// BreakRunLock removes the lock file, so the next acquirer creates a fresh one
// and locks that instead.
//
// Unlinking does not revoke the dead holder's flock -- nothing can, from outside
// the process that owns the descriptor -- and it does not need to: the lock lives
// on the inode, and after the unlink no acquirer ever sees that inode again. The
// orphan goes when whatever still holds it does.
//
// The race this opens is already handled where it has to be. AcquireRunLock
// re-checks, after locking, that the path still refers to the inode it locked,
// and retries when it does not -- so a run acquiring the lock at the moment it is
// broken either gets the old inode and retries, or gets the new one and wins.
// Both are correct; neither ends up believing it holds a lock it does not.
//
// Reports whether a file was actually removed, so a caller can tell "broken" from
// "there was nothing there", which look identical afterwards.
func BreakRunLock(dir, key string) (bool, error) {
	path := RunLockPath(dir, key)
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("remove the lock file %s: %w", path, err)
	}
	return true, nil
}
