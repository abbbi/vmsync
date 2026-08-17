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
)

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
