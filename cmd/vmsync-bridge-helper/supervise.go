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
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"vmsync/pkg/trace"
	"vmsync/pkg/util"
)

// The helper's third mode: run one shell command on this host, under a
// runtime limit, and report what became of it.
//
// WHY THIS EXISTS, which is not obvious from here. vmsync drives the target
// by running shell commands over SSH, and those commands all touch the
// replica's storage: starting a qemu-nbd export has to open the image,
// committing an overlay rewrites the base. If that storage wedges -- a
// stalled multipath or iSCSI path, an NFS server gone away, a thin pool out
// of metadata -- the command never returns, and nothing on vmsync's side can
// end it. Its SSH keepalives keep being answered, because sshd does not touch
// the disk; its context has no deadline; and even if it had one, cancelling
// it would only stop vmsync LISTENING. The command would keep running, and if
// the storage recovered an hour later it would finish -- binding a port
// vmsync had long since given to another run.
//
// Only a process on this host can signal it. That is the whole reason this
// mode exists, and the reason it is worth a second process per command.
//
// WHAT IT DOES NOT PROMISE. A process wedged in uninterruptible I/O (state D)
// cannot be killed by anything, including this. SIGKILL pends until the I/O
// completes or errors. So what is guaranteed is a BOUNDED, HONEST ANSWER plus
// a kill attempt -- never termination. "I signalled it and it is still there"
// is a legitimate outcome, reported as such, and it is still the difference
// that matters: vmsync stops waiting, releases the target's run lock and
// aborts the source's backup job, instead of parking forever.

// superviseVerdictPrefix starts the one line that says what happened.
//
// The verdict is AUTHORITATIVE and the exit code is a hint. A supervised
// command can legitimately exit with any code, including whichever one this
// program might choose to mean "timed out", so the code alone can never be
// trusted to distinguish them -- while a line this process writes itself can.
const superviseVerdictPrefix = "supervise: "

// Outcomes, as they appear in the verdict line.
const (
	// superviseCompleted: the child ran to completion on its own. exit= is
	// then the child's, and is the whole story.
	superviseCompleted = "completed"
	// superviseTimeout: the runtime limit ran out and this process signalled. The
	// child may or may not have died; reaped= says which.
	superviseTimeout = "timeout"
	// superviseStalled: the command went -stall-after without any of its
	// processes computing or moving a byte, and this process signalled.
	//
	// Distinct from a timeout in the verdict line although both share
	// ExitSupervisedTimeout, because the two say different things to whoever
	// reads the log: a timeout means "took longer than this command ever
	// should", a stall means "was still there and doing nothing at all".
	// Only the second is evidence about the storage.
	superviseStalled = "stalled"
	// superviseStartFailed: the child never started. Distinct from a child
	// that started and failed, because the remedies are different ones.
	superviseStartFailed = "startfailed"
)

// superviseConfig is one supervised command.
type superviseConfig struct {
	// MaxRuntime is how long the command may run before it is signalled. ZERO
	// MEANS UNBOUNDED, and that is a setting vmsync uses deliberately: a
	// `qemu-img commit` must never be abandoned part-way, because the base
	// image is then in a state nobody can describe. Such a command still
	// gets the beat, so a hang is audible even where it must not be
	// interrupted.
	MaxRuntime time.Duration
	// StallAfter is how long the command may make NO progress before it is
	// signalled. Zero means never, and the two bounds are independent: a
	// command may have both, either, or neither.
	//
	// This is the bound to prefer for anything whose runtime legitimately
	// scales with the storage -- creating an overlay, opening a large image
	// -- because it cannot be wrong about how slow the array is allowed to
	// be today. It only ever fires on a command doing nothing whatsoever.
	// See progress.go for what counts as doing something.
	StallAfter time.Duration
	// StallWarnOnly measures exactly as StallAfter does and never signals.
	//
	// SEPARATING THE MEASUREMENT FROM THE KILL is the point. A qemu-img
	// commit must never be abandoned -- the base image would be left in a
	// state no later run can describe -- but "must not be killed" was never
	// a reason not to LOOK. Supervising it for a bare "still alive" line
	// answered the question nobody asked; counting its progress turns the
	// same line into the diagnosis the supervision was added for, at no
	// risk at all, because nothing is signalled either way.
	StallWarnOnly bool
	// WatchArgv is a substring of the command line of a process that must be
	// measured and signalled along with the process group, for the case of a
	// child that daemonises out of it. Empty matches nothing at all.
	WatchArgv string
	// Grace is how long the child has to honour SIGTERM before SIGKILL, and
	// then how long this process waits for a killed child to be reaped
	// before reporting that it was not. One knob for both because they are
	// the same question asked twice -- how long is a dying process allowed
	// to take -- and two would be two numbers to get wrong.
	Grace time.Duration
	// Label is what the caller calls this command, for the beat line. The
	// command itself is not logged: it is already in vmsync's own log, and
	// repeating a shell line that contains paths and pidfiles in every beat
	// would bury the one field that changes.
	Label string
	// Interval is how often to beat, or zero for trace.ProgressInterval.
	// Settable only so tests need not wait a minute; nothing in main.go
	// sets it. See the identical field on checksumConfig.
	Interval time.Duration
	// Progress is where beats and the verdict go -- stderr in production,
	// never the same stream as the child's own output.
	Progress io.Writer
}

// superviseResult is what the verdict line says, as data.
type superviseResult struct {
	Outcome  string
	ExitCode int
	// Signals is what was sent, in order, so the line distinguishes a child
	// that honoured SIGTERM from one that had to be killed.
	Signals []string
	// Reaped is whether the child was actually collected. False after a
	// SIGKILL that did not land is the signature of wedged storage, and is
	// the single most diagnostic field here.
	Reaped bool
	// State is the child's /proc state letter at the end, when it could be
	// read. "D" is uninterruptible I/O: the one that explains an unreaped
	// child to anybody who does not already know.
	State string
	// StalledBeats is how many consecutive beats showed no progress at the
	// moment the command was given up on, and is set only for a stall. It
	// goes in the line because "stalled" without a count invites the
	// question the count answers: for how long.
	StalledBeats int
	Duration     time.Duration
}

// Line renders the verdict.
//
// key=value pairs on one line, in a fixed order, because the far side greps
// for the prefix and parses what follows -- and because an operator reading
// the raw log gets the same sentence in the same shape every time.
func (r superviseResult) Line() string {
	var b strings.Builder
	b.WriteString(superviseVerdictPrefix)
	fmt.Fprintf(&b, "outcome=%s exit=%d duration=%s", r.Outcome, r.ExitCode, r.Duration.Round(time.Millisecond))
	if len(r.Signals) > 0 {
		fmt.Fprintf(&b, " signalled=%s reaped=%t", strings.Join(r.Signals, ","), r.Reaped)
	}
	if r.StalledBeats > 0 {
		fmt.Fprintf(&b, " stalled_beats=%d", r.StalledBeats)
	}
	if r.State != "" {
		fmt.Fprintf(&b, " child_state=%s", r.State)
	}
	return b.String()
}

// runSupervise runs command, streaming its output to out, and returns what
// happened to it.
//
// The child's combined output goes to out VERBATIM -- vmsync puts it in the
// error it reports, exactly as it did when it ran the command over SSH
// directly, so a failure still says what the command printed. Nothing of this
// process's own ever goes there: beats and the verdict go to cfg.Progress, and
// mixing them would corrupt the one thing the caller is reading for.
func runSupervise(cfg superviseConfig, command string, out io.Writer) superviseResult {
	interval := cfg.Interval
	if interval <= 0 {
		interval = trace.ProgressInterval
	}
	grace := cfg.Grace
	if grace <= 0 {
		grace = defaultSuperviseGrace
	}

	// /bin/sh, not the login shell: these commands come from vmsync and are
	// POSIX, and a deterministic interpreter is one less thing that can
	// differ between two target hosts.
	cmd := exec.Command("/bin/sh", "-c", command)
	start := time.Now()

	// The child must not inherit this process's own stdout and stderr.
	//
	// Those two descriptors are the SSH channel. Handed to the child they are
	// dup'd into it, so an orphan left behind by the give-up path below goes
	// on holding them after this process exits: sshd cannot then close the
	// channel, and the vmsync waiting on the other end never returns. The
	// bound would have bounded nothing.
	//
	// A pipe this process owns instead. The child writes into it, this
	// process copies it out, and once this process stops reading, the orphan
	// gets EPIPE on its next write and holds nothing of ours.
	pipeR, pipeW, err := os.Pipe()
	if err != nil {
		return superviseFailed(cfg, start, fmt.Errorf("could not create the output pipe: %w", err))
	}
	cmd.Stdout = pipeW
	cmd.Stderr = pipeW
	// Its own process group, which is what makes the kill reach the right
	// thing. Every one of these commands is a shell line, so the child here
	// is /bin/sh; signalling it alone would leave whatever it had started --
	// a qemu-nbd still blocked opening the replica image, say -- running with
	// nobody left to signal it.
	startInOwnProcessGroup(cmd)

	if err := cmd.Start(); err != nil {
		_ = pipeR.Close()
		_ = pipeW.Close()
		return superviseFailed(cfg, start, fmt.Errorf("could not start the command: %w", err))
	}
	pid := cmd.Process.Pid
	// This process's own copy of the write end, dropped now that the child
	// holds one: left open, it would itself be a writer that never writes,
	// and the copier below would never see EOF.
	_ = pipeW.Close()

	copied := make(chan struct{})
	go func() {
		defer close(copied)
		_, _ = io.Copy(out, pipeR)
	}()

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	// Beats, not seconds: the loop's only clock is the tick, and rounding up
	// means a bound shorter than one beat still takes one beat rather than
	// firing on the first one with nothing to compare against.
	stallBeats := 0
	if cfg.StallAfter > 0 {
		stallBeats = int((cfg.StallAfter + interval - 1) / interval)
		if stallBeats < 1 {
			stallBeats = 1
		}
	}

	ticker := time.NewTicker(interval)
	res := superviseLoop(superviseClock{
		tick:       ticker.C,
		stop:       ticker.Stop,
		maxRuntime:    cfg.MaxRuntime,
		grace:         grace,
		stallBeats:    stallBeats,
		stallWarnOnly: cfg.StallWarnOnly,
	}, waitCh, superviseActions{
		beat: func(phase string, elapsed time.Duration, p progressSample, stalledBeats int) {
			if cfg.Progress == nil {
				return
			}
			fmt.Fprint(cfg.Progress, formatSuperviseBeat(cfg, pid, phase, elapsed, procState(pid), p, stalledBeats))
		},
		// The group AND the stray, for the reason procPids documents: a
		// daemon that called setsid() is outside the group and survives a
		// kill aimed at it, so signalling only the group would leave the
		// one process worth stopping running.
		signal: func(sig string) error {
			err := signalProcessGroup(pid, sig)
			for _, stray := range procPids(pid, cfg.WatchArgv) {
				if procPgid(stray) != pid {
					_ = signalPid(stray, sig)
				}
			}
			return err
		},
		state:    func() string { return procState(pid) },
		progress: func() (progressSample, bool) { return groupProgress(pid, cfg.WatchArgv) },
	})
	// Drain what the child wrote, then stop reading whatever happens.
	//
	// A child that exited closes its write end, the copier sees EOF and this
	// returns at once. An orphan still holds that end open, so the copier
	// would otherwise block here for as long as the orphan lives -- which is
	// the hang again. One grace to finish, then the read end is closed under
	// it and the copier returns with an error nobody needs.
	select {
	case <-copied:
	case <-time.After(grace):
		_ = pipeR.Close()
		<-copied
	}
	_ = pipeR.Close()

	res.Duration = time.Since(start)
	if res.Outcome == superviseCompleted {
		res.ExitCode = cmd.ProcessState.ExitCode()
		if res.ExitCode < 0 {
			// Killed by a signal this process did not send -- an OOM kill, an
			// operator, a crash. Reported as a shell reports it, 128+signal,
			// rather than the -1 Go uses: the number travels back to a human
			// who knows that convention.
			res.ExitCode = superviseSignalExitCode(cmd.ProcessState)
		}
	}
	if cfg.Progress != nil {
		fmt.Fprintf(cfg.Progress, "%s\n", res.Line())
	}
	return res
}

// superviseFailed reports a supervision that never got as far as a child.
//
// Distinct from a child that started and failed, because the remedies have
// nothing in common: this one is a broken helper or a broken host, and the
// command never ran at all. It goes to cfg.Progress like every other word
// this process says, never to out, which carries the child's bytes and must
// stay free of ours even when there are none.
func superviseFailed(cfg superviseConfig, start time.Time, err error) superviseResult {
	res := superviseResult{
		Outcome:  superviseStartFailed,
		ExitCode: util.ExitSuperviseFailed,
		Duration: time.Since(start),
	}
	if cfg.Progress != nil {
		fmt.Fprintf(cfg.Progress, "vmsync-bridge-helper: %v\n", err)
		fmt.Fprintf(cfg.Progress, "%s\n", res.Line())
	}
	return res
}

// superviseClock is everything time-dependent about one supervision, so the
// state machine below can be driven by a test without sleeping.
type superviseClock struct {
	// tick is the beat clock, and stop ends it. A channel rather than a
	// *time.Ticker so the loop below can be driven a tick at a time by a
	// test, which is the only way its transitions are provable without
	// spending a real minute on each one.
	tick       <-chan time.Time
	stop       func()
	maxRuntime time.Duration
	grace      time.Duration
	// stallBeats is how many consecutive beats with no progress at all end
	// the command, or zero to never end it that way. Counted in BEATS, not
	// in time, because the beat is the only clock the loop has and a bound
	// expressed in anything else could not be checked without a second one.
	stallBeats int
	// stallWarnOnly counts and reports the same way but never signals, for
	// a command that must not be abandoned and still deserves watching.
	stallWarnOnly bool
	// after is how the deadlines are armed, or nil for time.After. Only a
	// test sets it.
	after func(time.Duration) <-chan time.Time
}

func (c superviseClock) arm(d time.Duration) <-chan time.Time {
	if c.after != nil {
		return c.after(d)
	}
	return time.After(d)
}

// superviseActions is what the state machine does to the world.
type superviseActions struct {
	beat   func(phase string, elapsed time.Duration, p progressSample, stalledBeats int)
	signal func(sig string) error
	state  func() string
	// progress samples every process being supervised. ok false means the
	// counters could not be read at all, which the loop treats as movement:
	// nothing is ever killed on the strength of a measurement that failed.
	progress func() (progressSample, bool)
}

// The phases, which are also what the beat line calls them.
const (
	superviseRunning     = "running"
	superviseTerminating = "terminating" // SIGTERM sent, inside the grace
	superviseKilled      = "killed"      // SIGKILL sent, waiting to reap
)

// superviseLoop is the runtime-limit state machine: run, terminate, kill, give up
// waiting.
//
// GIVING UP WAITING IS THE POINT, and it is the part that is easy to leave
// out. cmd.Wait() on a child that cannot die never returns -- waitpid cannot
// collect a process that is still there, and a process in uninterruptible I/O
// is still there however many times it has been sent SIGKILL. A supervisor
// that waits for the child it just killed therefore inherits exactly the hang
// it was built to bound, one level further down, and the caller is no better
// off than before. So the third deadline is not an afterthought: after the
// kill, this waits one more grace and then reports reaped=false and returns.
//
// The child is then orphaned, which is correct and is all that can be done.
// It holds no lock of ours; what it holds is the wedged storage, and the only
// thing that frees that is the storage recovering or the host rebooting.
//
// Pure with respect to the clock and the process -- both arrive as arguments
// -- so every transition is provable without starting anything or waiting for
// a real second.
func superviseLoop(clock superviseClock, waitCh <-chan error, act superviseActions) superviseResult {
	if clock.stop != nil {
		defer clock.stop()
	}
	res := superviseResult{Outcome: superviseCompleted}
	phase := superviseRunning
	started := time.Now()

	var runtimeCh, deadlineCh <-chan time.Time
	if clock.maxRuntime > 0 {
		runtimeCh = clock.arm(clock.maxRuntime)
	}

	// Progress state. haveSample guards the first beat: with no previous
	// sample to compare against there has been no observed stillness yet,
	// so the first beat can never be the first strike.
	var lastSample progressSample
	haveSample := false
	stalledBeats := 0

	for {
		select {
		case <-waitCh:
			// It ended. Whether on its own or because of a signal we sent,
			// the distinction is already recorded in res.Signals.
			if phase != superviseRunning {
				res.Reaped = true
			}
			return res

		case <-clock.tick:
			sample, readable := progressNow(act)
			switch {
			case !readable || !haveSample:
				// Cannot tell, or nothing to compare with yet. Either way
				// this beat is not evidence of stillness.
				stalledBeats = 0
			case sample.moved(lastSample):
				stalledBeats = 0
			default:
				stalledBeats++
			}
			lastSample, haveSample = sample, readable
			act.beat(phase, time.Since(started), sample, stalledBeats)

			// Recorded whenever the bound is crossed, signalled or not, so
			// a command that froze for five minutes and then finished says
			// so in its verdict instead of reporting a clean completion.
			if clock.stallBeats > 0 && stalledBeats >= clock.stallBeats {
				res.StalledBeats = stalledBeats
			}

			// Only while the command is still running: once SIGTERM is out,
			// a process making no progress is a process doing as it was
			// told, and the grace deadline already governs what follows.
			if phase == superviseRunning && clock.stallBeats > 0 && !clock.stallWarnOnly && stalledBeats >= clock.stallBeats {
				// The same second look the deadlines take: a command that
				// finished in the instant this beat fired must not be
				// reported as stalled.
				if reaped(waitCh) {
					return res
				}
				runtimeCh = nil
				res.Outcome = superviseStalled
				res.ExitCode = util.ExitSupervisedTimeout
				// StalledBeats is already set by the recorder above.
				res.Signals = append(res.Signals, "TERM")
				_ = act.signal("TERM")
				phase = superviseTerminating
				deadlineCh = clock.arm(clock.grace)
			}

		case <-runtimeCh:
			// A deadline and the child's exit can both be ready, and select
			// then picks between them at random -- so a command that
			// finished in the same instant its runtime limit expired would be
			// signalled, and reported as a timeout, about half the time.
			// The same second look keepaliveLoop and digestBeat take, for
			// the same reason: the cheaper mistake is noticing one tick
			// late, never acting on a race.
			if reaped(waitCh) {
				return res
			}
			// The runtime limit is spent. SIGTERM first, and the grace that follows is not
			// politeness: qemu-nbd on SIGTERM closes its export and removes
			// its pidfile, where a SIGKILL leaves that pidfile behind for a
			// later teardown to kill -9 whatever process has since inherited
			// the number.
			runtimeCh = nil
			res.Outcome = superviseTimeout
			res.ExitCode = util.ExitSupervisedTimeout
			res.Signals = append(res.Signals, "TERM")
			_ = act.signal("TERM")
			phase = superviseTerminating
			deadlineCh = clock.arm(clock.grace)

		case <-deadlineCh:
			// Same second look, and it matters more here: a child that
			// honoured SIGTERM just as its grace ran out would otherwise be
			// killed anyway, and reported unreaped when it had in fact
			// exited cleanly.
			if reaped(waitCh) {
				res.Reaped = true
				return res
			}
			switch phase {
			case superviseTerminating:
				res.Signals = append(res.Signals, "KILL")
				_ = act.signal("KILL")
				phase = superviseKilled
				deadlineCh = clock.arm(clock.grace)
			default:
				// Killed, and still here. Stop waiting and say so.
				res.Reaped = false
				res.State = act.state()
				return res
			}
		}
	}
}

// progressNow samples, tolerating a loop driven by a test that supplied no
// sampler at all. No sampler means no measurement, which means no stall --
// the same answer an unreadable /proc gives.
func progressNow(act superviseActions) (progressSample, bool) {
	if act.progress == nil {
		return progressSample{}, false
	}
	return act.progress()
}

// reaped reports whether the child has already exited, without waiting for
// it. Beside the loop that uses it, like stopped() in pkg/remotessh and
// beatStopped() above.
func reaped(waitCh <-chan error) bool {
	select {
	case <-waitCh:
		return true
	default:
		return false
	}
}

// defaultSuperviseGrace is how long a signalled child has, both to honour
// SIGTERM and then to be reaped after SIGKILL.
//
// Five seconds because everything supervised here either exits immediately on
// SIGTERM (qemu-nbd closes its export; sh forwards and exits) or is not going
// to exit at all. A longer grace does not help the first case and only
// delays the report in the second, which is the one somebody is waiting for.
const defaultSuperviseGrace = 5 * time.Second

// formatSuperviseBeat is the line that makes a hang audible.
//
// It names the child's /proc state because that is the field which turns
// "the target is slow" into a diagnosis: D is uninterruptible I/O, which on a
// command that only touches the replica's storage means that storage has
// stopped answering. An operator who sees that once stops looking at vmsync.
//
// Bare, with no timestamp and no level, for the reason the digest beat gives:
// vmsync stamps each line as it reads it, so the time on the line is when the
// WAITING side saw it rather than two clocks that may disagree.
func formatSuperviseBeat(cfg superviseConfig, pid int, phase string, elapsed time.Duration, state string, p progressSample, stalledBeats int) string {
	limit := "no runtime limit"
	if cfg.MaxRuntime > 0 {
		limit = "max runtime " + cfg.MaxRuntime.String()
	}
	where := ""
	if state != "" {
		where = ", child " + strconv.Itoa(pid) + " in state " + state + " (" + procStateMeaning(state) + ")"
	}
	// The counters go in every beat, not just a stalling one. A reader
	// comparing two consecutive beats can then see for themselves whether a
	// slow command is advancing, which is the question a bare "alive" line
	// left them guessing at -- and it is the same question this process
	// answers mechanically a few beats later.
	progress := ""
	if cfg.StallAfter > 0 {
		progress = fmt.Sprintf(", cpu=%d rw=%dB blk=%dB", p.cpu, p.rw, p.blk)
		if stalledBeats > 0 {
			ends := "giving up after " + cfg.StallAfter.String()
			if cfg.StallWarnOnly {
				// Said plainly, because an operator seeing "NO PROGRESS"
				// on a commit needs to know nothing is about to be killed
				// -- and that the decision to intervene is theirs.
				ends = "NOT signalled, this command is never abandoned"
			}
			progress += fmt.Sprintf(" (NO PROGRESS for %d beat(s), %s)", stalledBeats, ends)
		}
	}
	return fmt.Sprintf("supervise alive (%s) %s %s elapsed%s%s, %s\n",
		cfg.Label, phase, elapsed.Round(time.Second), where, progress, limit)
}

// procState is the child's state letter from /proc, or "" when it cannot be
// read -- which is the normal answer on a host that has no /proc, and the
// right one for a line that is only ever supplementary.
func procState(pid int) string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return ""
	}
	return parseProcState(string(b))
}

// parseProcState pulls the state letter out of one /proc/pid/stat line.
//
// From after the LAST ')' rather than by splitting the line on spaces:
// field 2 is the executable name in parentheses and may itself contain
// spaces and parentheses, which is the classic way a stat parser reads the
// wrong field and reports a healthy process as something else.
func parseProcState(stat string) string {
	i := strings.LastIndex(stat, ")")
	if i < 0 || i+2 >= len(stat) {
		return ""
	}
	f := strings.Fields(stat[i+1:])
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// procStateMeaning puts the letter in words, because the letter alone is
// jargon and this line exists to be read in a hurry.
func procStateMeaning(state string) string {
	switch state {
	case "R":
		return "running"
	case "S":
		return "sleeping"
	case "D":
		return "uninterruptible I/O"
	case "Z":
		return "zombie, not yet reaped"
	case "T", "t":
		return "stopped"
	case "X", "x":
		return "dead"
	default:
		return "unrecognised"
	}
}

// superviseSignalExitCode renders a child killed by a signal the way a shell
// does, 128+n, rather than as Go's -1. The number goes back to a person who
// knows that convention, and -1 tells them nothing.
func superviseSignalExitCode(ps *os.ProcessState) int {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return util.ExitSuperviseFailed
}

// startInOwnProcessGroup puts the child in a process group of its own, so a
// signal can reach everything it started and not just the shell.
func startInOwnProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// signalProcessGroup signals the child's whole group.
//
// The negative pid is the entire point of this function: kill(2) with a
// negative pid signals the process GROUP. Sent to the pid alone, SIGKILL
// reaps the /bin/sh wrapper and leaves whatever it had started -- a qemu-nbd
// blocked opening the replica image -- running, unsupervised and unsignalled,
// which is the exact state this mode exists to prevent.
func signalProcessGroup(pid int, sig string) error {
	s := syscall.SIGTERM
	if sig == "KILL" {
		s = syscall.SIGKILL
	}
	return syscall.Kill(-pid, s)
}

// signalPid signals ONE process, for the daemon that left the group and so
// cannot be reached by the negative pid above.
func signalPid(pid int, sig string) error {
	s := syscall.SIGTERM
	if sig == "KILL" {
		s = syscall.SIGKILL
	}
	return syscall.Kill(pid, s)
}
