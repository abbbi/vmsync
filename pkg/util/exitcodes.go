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

// Every exit status these binaries can leave behind, in one place.
//
// They live in pkg/util because each one is a contract BETWEEN programs --
// one of ours exits with it, something else reads it off a finished process
// and decides what happened -- and a number two programs must agree on does
// not belong inside either of them.
//
// THE WHOLE SPACE, because the reason to collect them is to stop the next
// one colliding with an existing meaning. vmsync-bridge-helper is the
// binary to watch: it can exit with the run-lock codes in one mode and the
// supervision codes in another, so its statuses share a single space:
//
//	  0  success. Also "skipped" where that is not a failure: an
//	     -ignore-external-snapshot run, or a helper that printed -version.
//	  1  the run failed. The ordinary failure, used as a bare literal.
//	  2  bad flags or a bad invocation; nothing was attempted. Bare literal,
//	     and the only status produced before anything can have happened.
//	  3  RemoteLockExitNoFlock  -- no flock(1) on the target
//	  4  RemoteLockExitNoDir    -- the lock directory could not be made
//	  5  RemoteLockExitNoCreate -- the lock file could not be created
//	  6  RemoteLockExitBusy     -- the target-side lock is held
//	  7  RemoteLockExitNoSameFile -- the target's shell cannot compare two
//	     files for identity, so a lock taken there cannot be shown to be the
//	     lock the path names
//	 75  ExitBusy               -- stood down, nothing touched
//	124  ExitSupervisedTimeout  -- a supervised command outlived -max-runtime
//	125  ExitSuperviseFailed    -- the supervision itself failed
//	137  TEST ONLY: -test=die-writing-base kills itself mid-copy. 128+9 is a
//	     shell's encoding of SIGKILL, chosen so a harness sees a killed
//	     process rather than a failed sync. Never a real run.
//
// 0, 1 and 2 are deliberately left as bare literals at their ~70 call
// sites. Naming them would touch every one of those to say what the number
// already says; what was missing was the map, not the names.
const (
	// The statuses the remote side of the target run lock exits with, one
	// per failure. Each pairs with a VMSYNC-LOCK-* marker it prints on
	// stdout (see remotelock.go), and both halves are exported for the same
	// reason: the lock has two implementations that must be
	// indistinguishable to the caller -- a POSIX shell snippet and
	// vmsync-bridge-helper's leased mode -- so AcquireRemoteRunLock can
	// choose between them without its error handling caring which answered.
	//
	// Low numbers because they are the exit status of a shell `exit N` in
	// that snippet, where the usual conventions leave 3 upwards free.
	RemoteLockExitNoFlock  = 3
	RemoteLockExitNoDir    = 4
	RemoteLockExitNoCreate = 5
	RemoteLockExitBusy     = 6

	// RemoteLockExitNoSameFile means the shell hold could not prove that the
	// file it locked is the file the lock path names, because the test it
	// needs does not work on that host: no `test -ef`, or no /proc.
	//
	// Refusing is the point. Without that test the hold can flock a file that
	// has since been unlinked and replaced, which excludes nobody, and then
	// announce VMSYNC-LOCK-ACQUIRED about it -- so a second vmsync takes the
	// file that replaced it and both run against the same replica.
	RemoteLockExitNoSameFile = 7

	// ExitBusy means "stood down without doing anything, because another
	// vmsync holds the lock for this domain".
	//
	// 75 is sysexits.h's EX_TEMPFAIL: a temporary failure, retry later.
	//
	// It exists because the two ordinary codes both lie for an operator
	// verb. 0 would report a restore that never happened as done; 1 would
	// report one that never started as failed, which the agent records
	// terminally and can then never retry with the same operation id --
	// while the reason it could not run clears up on its own seconds later.
	//
	// A SYNC uses it too, and the reason is the agent's memory rather than
	// the operator's: vmsync-agent holds its in-flight set in memory only,
	// so a new instance knows nothing about a child the previous one
	// started, launches a second vmsync for that domain, and reads the exit
	// code to decide what happened. At 0 it recorded a SUCCESS -- every
	// interval, for as long as the real sync ran -- so the metrics, the
	// console and the journal all reported healthy replication while
	// nothing was being copied. This status is the only signal that can
	// tell those apart, because it comes from the process that actually
	// holds the lock: unlike anything the agent can infer from /proc or its
	// own files, it cannot be stale or wrong.
	//
	// Consequence for anything that runs vmsync: a scheduled overlap is no
	// longer indistinguishable from success, so a caller treating any
	// non-zero code as an error has to accept this one -- see
	// contrib/runner/vmsync-parallel.sh, which passes "0;75" for exactly
	// that.
	ExitBusy = 75

	// ExitSupervisedTimeout: a supervised command outlived its
	// -max-runtime and was signalled. Whether it actually died is in the
	// helper's verdict line, not here.
	ExitSupervisedTimeout = 124

	// ExitSuperviseFailed: the supervision itself failed -- the command
	// could not be started, or its fate could not be determined. Never the
	// command's own status.
	ExitSuperviseFailed = 125
)

// 124 and 125 are GNU timeout(1)'s, deliberately. There is no sysexits.h
// code for "the command I was supervising ran out of time", but there is a
// convention every operator on a Linux host already knows -- 124 means the
// command timed out, 125 means the supervisor itself failed -- and borrowing
// it costs nothing and explains itself in any shell.
//
// BOTH ARE HINTS, not the answer. A supervised command can legitimately exit
// with any status, 124 included, so these can never be trusted to
// distinguish "the command timed out" from "the command said 124". The
// verdict line vmsync-bridge-helper writes on stderr is what decides that;
// these exist so a human reading a bare exit status, or a shell wrapper with
// no parser, is pointed the right way.
