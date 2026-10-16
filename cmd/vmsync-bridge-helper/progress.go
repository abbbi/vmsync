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
	"os"
	"strconv"
	"strings"
)

// WHY A PROGRESS RULE AND NOT A LONGER CLOCK.
//
// A wall-clock budget can only be set by guessing how slow the storage is
// allowed to be, and every such guess kills a healthy command on the day
// the array is busy. The digest solved this already: emit a beat every
// minute, carry progress in it when there is any, and call it dead only
// after several beats with none. This is that rule for an opaque child.
//
// WHAT COUNTS AS PROGRESS, and it is deliberately generous. Any movement in
// any of three counters means alive:
//
//	cpu  utime+stime     -- it is computing
//	rw   rchar+wchar     -- it is issuing read/write syscalls, cached or not
//	blk  read+write_bytes -- the block layer is completing I/O for it
//
// Three rather than one because each misses a real case on its own: a
// process servicing cached reads moves rw and not blk; one grinding on a
// single enormous read moves blk and neither of the others until it
// returns; one busy in userspace moves only cpu. A process that is wedged
// in uninterruptible I/O moves none of them, which is the whole signal.
//
// MEASURED OVER THE PROCESS GROUP, never the leader. The helper runs
// `/bin/sh -c`, and sh forks for any compound command -- so the pid it
// holds is a shell asleep in wait(), whose counters never move however hard
// the real worker is going. Measured on a Fedora 44 host: across two
// samples the shell sat at rchar=7866 cpu=0 while its child went from
// 17.7 GB to 49.4 GB and 105 to 306 ticks of CPU. Reading the leader would
// therefore report every compound command as stalled, immediately.
type progressSample struct {
	cpu uint64 // clock ticks
	rw  uint64 // bytes through read/write syscalls
	blk uint64 // bytes the block layer actually moved
}

// moved reports whether anything at all advanced between two samples.
//
// Counters are monotonic per process, but the SET of processes is not: one
// exiting makes the sum fall. A fall is still movement -- something
// happened -- so this compares for inequality rather than for growth.
func (p progressSample) moved(prev progressSample) bool { return p != prev }

func (p progressSample) add(q progressSample) progressSample {
	return progressSample{cpu: p.cpu + q.cpu, rw: p.rw + q.rw, blk: p.blk + q.blk}
}

// procPids lists the pids of every process in group pgid, plus any whose
// command line contains watchArgv.
//
// THE watchArgv HALF IS NOT BELT AND BRACES. `qemu-nbd --fork` calls
// setsid(): measured on this project's test host, the daemon's pgid and sid
// both equal its own pid, and it SURVIVED a kill(-pgid) aimed at the group
// it was started in. A daemon that has left the group is invisible to a
// group-wide measurement and immune to a group-wide signal, so the one
// process whose health actually matters would be the one process neither
// watched nor stopped. Matching its argv is how it is found again.
func procPids(pgid int, watchArgv string) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if procPgid(pid) == pgid || matchesArgv(pid, watchArgv) {
			pids = append(pids, pid)
		}
	}
	return pids
}

// procPgid is field 5 of /proc/pid/stat, counted from after the last ')'
// for the same reason parseProcState is: field 2 is the executable name in
// parentheses and can contain both spaces and parentheses.
func procPgid(pid int) int {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return -1
	}
	s := string(b)
	i := strings.LastIndex(s, ")")
	if i < 0 {
		return -1
	}
	// After "pid (comm) " the fields are state ppid pgrp ...
	f := strings.Fields(s[i+1:])
	if len(f) < 3 {
		return -1
	}
	pgid, err := strconv.Atoi(f[2])
	if err != nil {
		return -1
	}
	return pgid
}

// matchesArgv reports whether pid's command line contains want.
//
// Substring over the whole NUL-joined cmdline, so a caller can pass a
// socket path, an export name or any other argument distinctive enough to
// name one process. An empty want never matches: a helper invoked without
// -watch-argv must not adopt every process on the host.
func matchesArgv(pid int, want string) bool {
	if want == "" {
		return false
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return false
	}
	return strings.Contains(strings.ReplaceAll(string(b), "\x00", " "), want)
}

// procProgress sums the three counters for one pid. ok is false when the
// process is gone or /proc cannot be read for it.
func procProgress(pid int) (progressSample, bool) {
	var s progressSample
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return s, false
	}
	str := string(stat)
	if i := strings.LastIndex(str, ")"); i >= 0 {
		// After the comm field: state ppid pgrp session tty tpgid flags
		// minflt cminflt majflt cmajflt utime stime ...
		if f := strings.Fields(str[i+1:]); len(f) >= 13 {
			utime, _ := strconv.ParseUint(f[11], 10, 64)
			stime, _ := strconv.ParseUint(f[12], 10, 64)
			s.cpu = utime + stime
		}
	}
	// /proc/pid/io is absent without CONFIG_TASK_IO_ACCOUNTING and
	// unreadable for another user's process. Neither is an error: the cpu
	// field alone still detects a process that is computing, and a sample
	// that cannot see everything must never be the reason something is
	// killed.
	if io, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/io"); err == nil {
		for _, line := range strings.Split(string(io), "\n") {
			name, value, found := strings.Cut(line, ": ")
			if !found {
				continue
			}
			n, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
			if err != nil {
				continue
			}
			switch name {
			case "rchar", "wchar":
				s.rw += n
			case "read_bytes", "write_bytes":
				s.blk += n
			}
		}
	}
	return s, true
}

// groupProgress sums every process in the group, plus any argv-matched
// stray, into one sample.
//
// ok is false only when NOTHING could be read -- no such process, or no
// /proc at all. The caller treats that as "cannot tell", never as "stalled":
// a host without /proc would otherwise have every supervised command killed
// three minutes in.
func groupProgress(pgid int, watchArgv string) (progressSample, bool) {
	var total progressSample
	ok := false
	for _, pid := range procPids(pgid, watchArgv) {
		if s, got := procProgress(pid); got {
			total = total.add(s)
			ok = true
		}
	}
	return total, ok
}
