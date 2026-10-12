/*
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

package trace

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// ProgressInterval is how often a long-running phase reports how far along it
// is. Every phase that can run for minutes uses it, so an operator watching a
// run never has to learn that one phase reports every second and the next
// says nothing for half an hour.
//
// It lives here, in the smallest package both sides can import, because the
// two sides of a digest exchange report in turn: vmsync hashes the source and
// vmsync-bridge-helper hashes the target, and a reader comparing the two
// needs them on the same cadence. A local copy in each would be two chances
// to drift.
//
// A minute rather than a second: these phases are measured in tens of minutes
// against multi-gigabyte disks, so per-second lines are not progress -- they
// are thousands of entries that push everything else out of a journal and
// make the log unreadable exactly when something has gone wrong and somebody
// is reading it. A minute is frequent enough to tell "moving" from "wedged",
// and rare enough that an hour-long phase leaves sixty lines.
const ProgressInterval = time.Minute

// DueForProgress reports whether a progress line should be emitted now.
//
// done forces one regardless of the interval, so every phase logs its own
// completion state rather than potentially stopping at the last tick -- a
// pass that finishes 3 seconds after a log would otherwise never report 100%.
func DueForProgress(lastLog, now time.Time, done bool) bool {
	return done || now.Sub(lastLog) >= ProgressInterval
}

// QuietWatch is the give-up half of a reported phase. DueForProgress decides
// when the WORKING side speaks; this decides how long the WAITING side keeps
// listening to nothing. Both halves live here so that the cadence and the
// patience cannot be changed independently: a bound is only meaningful as a
// multiple of the interval the other side reports on, and a reader who
// changes one must see the other.
//
// Returns true ONLY when it gives up, so what giving up means stays with the
// caller. remark fires on every tick past grace but short of bound, which is
// what makes a long wait audible before it becomes a failure.
//
// quiet >= bound, not >. A silence sitting exactly on the bound has to trip:
// with a strict comparison, a tick that lands precisely there remarks instead
// and the wait continues to the next one, which is the off-by-one that turns
// a bound into a bound-plus-an-interval.
//
// Pure with respect to the clock and to the peer -- both arrive as arguments
// -- so the boundary is provable with no peer to wedge and no sleeping, the
// way pkg/remotessh proves its keepalive strike logic.
func QuietWatch(stop <-chan struct{}, tick <-chan time.Time, quietFor func(now time.Time) time.Duration, grace, bound time.Duration, remark func(quiet time.Duration)) bool {
	for {
		select {
		case <-stop:
			return false
		case now, open := <-tick:
			if !open {
				return false
			}
			// Stop again, for the reason pkg/remotessh's keepaliveLoop gives
			// for its own second check: when a stop and a tick are both
			// ready, select picks between them uniformly at random, so a
			// wait that has just FINISHED loses that race about half the
			// time. Losing it means declaring a stall on an exchange that
			// already answered -- a healthy run failed at the last instant,
			// which is worse than noticing a real stall one tick later.
			if stopped(stop) {
				return false
			}
			quiet := quietFor(now)
			if quiet >= bound {
				return true
			}
			if quiet >= grace && remark != nil {
				remark(quiet)
			}
		}
	}
}

// stopped reports whether stop has been closed, without waiting for it.
func stopped(stop <-chan struct{}) bool {
	select {
	case <-stop:
		return true
	default:
		return false
	}
}

var (
	debugEnabled atomic.Bool
	warningCount atomic.Uint64
	errorCount   atomic.Uint64
)

func init() {
	log.SetPrefix("")
	log.SetFlags(log.LstdFlags)
}

func SetDebug(enabled bool) {
	debugEnabled.Store(enabled)
}

func Info(msg string, args ...any) {
	logWithLevel("INFO", msg, args...)
}

func Warning(msg string, args ...any) {
	warningCount.Add(1)
	logWithLevel("WARNING", msg, args...)
}

func Error(msg string, args ...any) {
	errorCount.Add(1)
	logWithLevel("ERROR", msg, args...)
}

// WarningCount returns how many times Warning has been called so far in
// this process's lifetime. vmsync is a one-shot CLI invocation, not a
// long-running daemon, so there is deliberately no reset: each process
// starts fresh at 0, and by the time cmd/vmsync reads this to build its
// own end-of-run Prometheus textfile, it reflects the whole run.
func WarningCount() uint64 {
	return warningCount.Load()
}

// ErrorCount is WarningCount's counterpart for Error.
func ErrorCount() uint64 {
	return errorCount.Load()
}

func Debug(msg string, args ...any) {
	if !debugEnabled.Load() {
		return
	}
	logWithLevel("DEBUG", msg, args...)
}

func DebugError(msg string, err error, args ...any) {
	if err == nil || !debugEnabled.Load() {
		return
	}
	args = append(args, "error", err)
	logWithLevel("DEBUG", msg, args...)
}

func logWithLevel(level, msg string, args ...any) {
	level = formatLevel(level)
	if len(args) == 0 {
		log.Printf("%s %s", level, msg)
		return
	}
	log.Printf("%s %s %s", level, msg, formatArgs(args...))
}

func formatLevel(level string) string {
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return level
	}

	switch level {
	case "INFO":
		return "\033[32m" + level + "\033[0m"
	case "WARNING":
		return "\033[33m" + level + "\033[0m"
	case "ERROR":
		return "\033[31m" + level + "\033[0m"
	default:
		return level
	}
}

func formatArgs(args ...any) string {
	var b strings.Builder
	for i := 0; i < len(args); i += 2 {
		if i > 0 {
			b.WriteByte(' ')
		}
		if i+1 < len(args) {
			b.WriteString(fmt.Sprintf("%v=%v", args[i], args[i+1]))
		} else {
			b.WriteString(fmt.Sprint(args[i]))
		}
	}
	return b.String()
}
