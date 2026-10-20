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

import "strings"

// VerifiedKill is a kill that has to prove its target before it signals.
//
// WHY A PIDFILE IS NOT ENOUGH, in this codebase's own words from the site that
// already got this right: "kill -9 succeeds against any valid PID, recycled or
// not, so || true can't catch or prevent that". Removing the pidfile after the
// kill makes a REPLAY harmless, which three call sites relied on -- but it does
// nothing for the first kill, when the process died and the OS handed its
// number to something else before the teardown ran. The signal then lands on
// whatever now answers to that number, as root, on a hypervisor.
//
// THE PROOF IS TWO THINGS, both read from /proc, and neither is sufficient
// alone.
//
// Comm, because the argv word is not as unique as it looks. Every process this
// is aimed at carries the pidfile's path as one of its own argv words
// (vmsync-bridge-helper takes "-pidfile <path>", qemu-nbd takes
// "--pid-file <path>"), and that path carries the domain plus the device or
// port -- but vmsync's own commands put that same path in the argv of the
// short-lived `cat`, `grep` and `rm` they run against it. A pid recycled onto
// one of those during a concurrent teardown would match on argv alone, and in
// the group form that would signal the whole process group the transient
// helper happens to sit in. Comm tells a qemu-nbd from a cat.
//
// Argv, because comm alone names a KIND of process, not an instance: every
// other export on the host is also qemu-nbd.
//
// The pairing is lifted from reclaimAbandonedExportCmd in cmd/vmsync, which
// scans /proc and takes exactly these two readings in this order before it
// kills anything. This exists so the four sites stop keeping their own version
// of it, and so the next one cannot be written without it.
//
// Fails SAFE in every direction: an unreadable or absent pidfile, a pidfile
// holding anything but digits, a pid with no /proc entry, a comm that does not
// match, or an argv that does not, all skip the kill and still clean up the
// files. The files go either way, because a pidfile naming a process that no
// longer exists is what makes the next run's reclaim a no-op.
type VerifiedKill struct {
	// PidFile is read for the pid, matched against the target's argv, and
	// removed whether or not anything was signalled.
	PidFile string

	// Comm is the expected executable name, compared against
	// /proc/<pid>/comm. The kernel truncates that to 15 bytes and so does
	// this, so a long binary name still compares equal. Empty means the
	// check is skipped, which only an argv that is unique by construction
	// should ever ask for.
	Comm string

	// Group kills the process GROUP rather than the single process, which
	// vmsync-bridge-helper needs: it is setsid-detached and its own group
	// leader, and the relay children it forked have to go with it. A group
	// kill against the wrong pid is MORE dangerous than a single one, not
	// less, which is why it is only reachable past the proof.
	Group bool

	// Remove names further files to delete alongside the pidfile.
	Remove []string
}

// commLimit is TASK_COMM_LEN-1: the kernel stores a 16-byte comm including its
// NUL, so /proc/<pid>/comm never holds more than 15 characters and a longer
// binary name is silently truncated there. Comparing an untruncated name would
// refuse every kill against a process whose name is long -- which
// vmsync-bridge-helper's is.
const commLimit = 15

// Command returns the shell command.
func (k VerifiedKill) Command() string {
	q := ShQuote(k.PidFile)

	var b strings.Builder
	b.WriteString("p=$(cat " + q + " 2>/dev/null); ")

	// Digits only -- and kept although no test can distinguish its absence.
	// Every non-digit string is refused by the /proc readings below anyway,
	// either because the path does not exist or because it holds somebody
	// else's process, so deleting this line is an equivalent mutation and was
	// the one mutant the suite could not catch.
	//
	// It stays for what it is worth when those readings are WEAKENED, which
	// mutation testing showed to be the plausible edit: dropping the -x for a
	// substring match, or dropping a check altogether. With them gone but this
	// line present, a pidfile holding "-1" still signals nothing. With both
	// gone it becomes `kill -9 -1` -- every process the caller owns, which on
	// a target running as root is the machine. The bridge helper writes its
	// pidfile atomically so a torn read cannot be read as a number; this is
	// the reader's half of the same worry.
	b.WriteString(`case "${p:-x}" in *[!0-9]*) p= ;; esac; `)

	b.WriteString(`if [ -n "$p" ]`)
	if k.Comm != "" {
		comm := k.Comm
		if len(comm) > commLimit {
			comm = comm[:commLimit]
		}
		// `read` rather than `cat`, as the reclaim does, so no process is
		// spawned to read one short line -- and the redirection is silenced
		// BEFORE the open, for the reason the argv read below documents.
		b.WriteString(` && read -r c 2>/dev/null < "/proc/$p/comm" && [ "$c" = ` + ShQuote(comm) + ` ]`)
	}
	// The ORDER of the two redirections is load-bearing. Redirections are
	// applied left to right, so with `< "/proc/$p/cmdline"` first the shell
	// attempts the open while stderr is still the caller's, and a pid with no
	// /proc entry produces "No such file or directory" on stderr -- on the
	// COMMON path, since a pidfile naming a process that has already gone is
	// exactly what this is usually pointed at. Silencing stderr first puts
	// that message where it belongs. The command is meant to be completely
	// silent: callers surface its output, and a teardown that mutters is a
	// teardown that gets logged as a fault.
	b.WriteString(` && tr '\0' '\n' 2>/dev/null < "/proc/$p/cmdline" | grep -qxF ` + q + "; then ")

	// Written out per branch rather than interpolated from a variable, so the
	// operand is visible in the source as a literal. The repo-wide rule in
	// verifiedkillrule_test.go requires every kill in this tree to aim at a
	// double-quoted shell expansion, and it reads string literals: a kill
	// assembled as "kill -9 " + target is exactly the shape it cannot see
	// into, and this function is not entitled to an exemption from its own
	// rule.
	if k.Group {
		// A BARE negative pid, with no `--` separator, and that is the
		// opposite of what the defensive instinct says.
		//
		// `kill -9 -- "-$p"` looks safer: the separator should stop a negative
		// pid being taken for an option bundle. In dash it does the reverse.
		// dash's kill builtin has no `--` at all, so it reads the separator as
		// the pid operand and fails on it -- "kill: Illegal number: -", exit 2
		// -- and THE GROUP IS NEVER SIGNALLED.
		//
		// Which shell that is depends on the path. These commands reach a
		// target over ssh as an exec request, so the target ACCOUNT'S LOGIN
		// SHELL interprets them, not necessarily /bin/sh; the local
		// commandRunner runs its commands through /bin/sh outright. dash is
		// /bin/sh on Debian and Ubuntu and is an ordinary login shell
		// anywhere, and busybox ash is both on an appliance. So there is
		// nothing to weigh here: the bare form works in every one of them, and
		// the separator buys no safety in any of them while losing the kill in
		// some.
		//
		// Measured, not reasoned, in debian:stable-slim against a real process
		// group. WITH the separator, under dash: the command exits ZERO, the
		// group leader and its child are both still running, and the pidfile
		// has been removed -- a leak that reports success and deletes the
		// evidence the next reclaim would have used. Without it the group dies.
		// busybox ash prints "invalid number '--'" and carries on to the real
		// operand; bash accepts both spellings, which is why a suite running
		// on Fedora or RHEL cannot see any of this.
		b.WriteString(`kill -9 "-$p" 2>/dev/null || true; `)
	} else {
		b.WriteString(`kill -9 "$p" 2>/dev/null || true; `)
	}

	// Waited for, so a caller that goes on to remove the image or commit into
	// it is not racing a process that still has it open.
	//
	// TWO SECONDS, and the bound is chosen against the budget this runs under,
	// not plucked. Every stop command registered for the interrupt path gets
	// its own five-second context (cmd/vmsync's cleanupTargetNBD), and the
	// polling loop around it keeps a five-second window open for commands that
	// workers register late. A wait that can itself reach five seconds spends
	// the whole of both: the command is cancelled before its `rm -f` runs, so
	// the pidfile survives to confuse the next run, and the straggler window
	// the loop exists to provide is gone, which is how a late-registered export
	// ends up with nothing left to stop it.
	//
	// Two seconds costs nothing in exchange, because of what the wait is for.
	// A process that has been SIGKILLed is gone in milliseconds unless it is
	// wedged in uninterruptible I/O, and one that is wedged will not leave in
	// five seconds either -- this tree says so where it sets the helper's own
	// grace period: "everything bounded here either exits on SIGTERM at once or
	// is wedged in uninterruptible I/O and will never exit at all. Waiting
	// longer does not change which of those it was, and only delays the report
	// somebody is waiting for."
	b.WriteString(`n=0; while kill -0 "$p" 2>/dev/null && [ "$n" -lt 20 ]; do n=$((n+1)); sleep 0.1; done; `)
	b.WriteString("fi; ")

	files := []string{q}
	for _, f := range k.Remove {
		files = append(files, ShQuote(f))
	}
	b.WriteString("rm -f " + strings.Join(files, " "))
	return b.String()
}
