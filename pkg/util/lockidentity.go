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
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// RunLockDir holds every run lock: the source-side one vmsync takes for
// itself, and the target-side one it takes over SSH on the far host.
//
// Here rather than in either command because it is a CONTRACT BETWEEN TWO
// BINARIES, exactly like ExitBusy: vmsync writes these files, vmsync-agent
// reads them to find out whether a sync it did not start is still running, and
// a path the two disagree about would make the agent silently see nothing
// forever.
//
// /run specifically, because it is tmpfs: no vmsync survives a reboot, so
// neither should any evidence that one was running.
const RunLockDir = "/run/vmsync-locks"

// RunLockIdentity is what the lock's holder writes into the (otherwise always
// empty) lock file, immediately after the lock is held.
//
// Why this file and not one of the agent's own: only the exclusive holder can
// write it, the kernel releases the lock with the holder by any means
// including SIGKILL, and /run is tmpfs so the whole namespace is cleared on
// reboot. That makes it the one record whose lifetime is tied to the thing it
// describes. Any file the agent writes about its children outlives them.
//
// PROVENANCE ONLY. Nothing about correctness may depend on this. A reader that
// cannot parse it, or cannot consult /proc, must fall through to launching and
// letting the engine's own lock decide -- see the comment on RunLockHeld.
type RunLockIdentity struct {
	PID int `json:"pid"`
	// BootID is /proc/sys/kernel/random/boot_id, and it is what makes "this
	// pid is gone" a FACT rather than a guess. Without it a reboot and a plain
	// crash are indistinguishable, and they need different answers.
	BootID string `json:"boot_id"`
	// StartTicks is field 22 of /proc/<pid>/stat. Together with BootID and PID
	// it is unique on a machine, which is what makes PID reuse safe to rule
	// out rather than hope about.
	StartTicks    uint64 `json:"start_ticks"`
	StartedAtUnix int64  `json:"started_at_unix"`
	Kind          string `json:"kind"` // sync | promote | restore | ...
	SourceDomain  string `json:"source_domain,omitempty"`
	TargetRef     string `json:"target_ref,omitempty"` // host:domain
	RunID         string `json:"run_id,omitempty"`     // joins to the agent's run log
}

// NewRunLockIdentity describes the CURRENT process.
func NewRunLockIdentity(kind, sourceDomain, targetRef, runID string, startedAtUnix int64) RunLockIdentity {
	id := RunLockIdentity{
		PID: os.Getpid(), Kind: kind,
		SourceDomain: sourceDomain, TargetRef: targetRef, RunID: runID,
		StartedAtUnix: startedAtUnix,
	}
	id.BootID, _ = CurrentBootID()
	id.StartTicks, _ = ProcStartTicks(id.PID)
	return id
}

// WriteRunLockIdentity writes id into an already-locked lock file.
//
// f must be the file returned by AcquireRunLock: this truncates and rewrites
// from the start, which is only safe because the caller holds the exclusive
// flock on it.
//
// Best-effort by contract. The caller logs a failure and proceeds -- losing
// provenance must never cost a sync, and everything downstream is built to
// fall back to launching when the identity is missing.
func WriteRunLockIdentity(f *os.File, id RunLockIdentity) error {
	data, err := json.Marshal(id)
	if err != nil {
		return fmt.Errorf("encode run lock identity: %w", err)
	}
	data = append(data, '\n')
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("truncate lock file: %w", err)
	}
	if _, err := f.WriteAt(data, 0); err != nil {
		return fmt.Errorf("write run lock identity: %w", err)
	}
	// Not fsynced. The file lives on tmpfs, where a sync buys nothing, and a
	// reader that finds a torn identity is required to fall through to
	// launching anyway.
	return nil
}

// ReadRunLockIdentity reads what a holder wrote.
//
// It NEVER opens the file for writing and NEVER calls Flock. That is the whole
// point of storing the identity rather than probing the lock: there is no
// non-destructive test of a flock -- fcntl(F_GETLK) cannot see flock() locks,
// and LOCK_EX|LOCK_NB succeeding IS acquisition -- so a probe would, between
// its own Flock and Close, make a concurrently launching vmsync take the
// "already running" branch. The probe would cause the very skip it exists to
// detect.
//
// Returns ok=false with no error when there is simply no lock file, which is
// the ordinary case for a domain nothing is syncing.
func ReadRunLockIdentity(dir, key string) (RunLockIdentity, bool, error) {
	data, err := os.ReadFile(RunLockPath(dir, key))
	if err != nil {
		if os.IsNotExist(err) {
			return RunLockIdentity{}, false, nil
		}
		return RunLockIdentity{}, false, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		// An empty lock file is what every vmsync before this feature left
		// behind, and what a holder that has not written its identity yet
		// looks like. Not an error, just no information.
		return RunLockIdentity{}, false, nil
	}
	var id RunLockIdentity
	if err := json.Unmarshal(data, &id); err != nil {
		return RunLockIdentity{}, false, fmt.Errorf("parse run lock identity in %s: %w", RunLockPath(dir, key), err)
	}
	return id, true, nil
}

// RunLockHeld reports whether the process described by id is still alive and
// still the same process.
//
// For vmsync this is equivalent to holding the lock, because the lock fd is
// held for the process's whole life -- so there is no window where the process
// is alive and the lock is not.
//
// The second return is a human-readable reason, for logging. It is never a
// reason to refuse anything.
//
// FAILS OPEN, and that direction is deliberate. Every "cannot tell" answer
// here returns false (not held), so the caller launches and lets the engine's
// own lock make the real decision -- costing one wasted process spawn and an
// honest ExitBusy record. Failing closed at a prediction layer is the
// dangerous direction: one permanent error (an unreadable /proc, a stat that
// fails for any reason other than the process being gone) would defer every VM
// on every tick forever, and nothing would go stale to reveal it.
func RunLockHeld(id RunLockIdentity, expectBinary string) (bool, string) {
	if id.PID <= 0 {
		return false, "the identity names no pid"
	}

	// Boot id first, before /proc is touched: one string compare settles
	// every lock left behind by a previous boot. "The pid is gone so it must
	// have been a reboot" is a guess that a plain crash produces identically.
	if id.BootID != "" {
		if boot, err := CurrentBootID(); err == nil && boot != id.BootID {
			return false, "the lock was taken before the last reboot"
		}
	}

	ticks, err := ProcStartTicks(id.PID)
	if err != nil {
		return false, fmt.Sprintf("pid %d is gone (%v)", id.PID, err)
	}
	// The PID-reuse guard. (BootID, PID, StartTicks) is unique on a machine,
	// so a pid recycled onto an unrelated process cannot masquerade as this
	// one.
	if id.StartTicks != 0 && ticks != id.StartTicks {
		return false, fmt.Sprintf("pid %d has been reused by a different process", id.PID)
	}

	// Last, and advisory: is it plausibly vmsync? A mismatch is reported but
	// still counts as HELD, because something is holding a vmsync lock and
	// launching into that is not an improvement.
	if expectBinary != "" {
		if exe, err := procExe(id.PID); err == nil && exe != "" && !sameBinary(exe, expectBinary) {
			return true, fmt.Sprintf("pid %d holds this lock but runs %s, not %s", id.PID, exe, expectBinary)
		}
	}
	return true, fmt.Sprintf("pid %d is still running", id.PID)
}

// CurrentBootID returns this boot's identifier.
func CurrentBootID() (string, error) {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// ProcStartTicks returns field 22 of /proc/<pid>/stat.
func ProcStartTicks(pid int) (uint64, error) {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, err
	}
	return ParseProcStatStartTicks(string(b))
}

// ParseProcStatStartTicks pulls starttime out of a /proc/<pid>/stat line.
//
// Split on the LAST ')' rather than on whitespace. Field 2 is the executable
// name in parentheses, it is not escaped, and it can contain both spaces and
// close-parens -- a process can rename itself to anything. Everything after
// the final ')' is field 3 onwards, so starttime (field 22) is index 19 there.
func ParseProcStatStartTicks(stat string) (uint64, error) {
	i := strings.LastIndex(stat, ")")
	if i < 0 {
		return 0, fmt.Errorf("malformed /proc stat line: no comm field")
	}
	fields := strings.Fields(stat[i+1:])
	const startTimeIndexAfterComm = 19 // field 22, counting from field 3 at 0
	if len(fields) <= startTimeIndexAfterComm {
		return 0, fmt.Errorf("malformed /proc stat line: %d fields after comm, need at least %d", len(fields), startTimeIndexAfterComm+1)
	}
	ticks, err := strconv.ParseUint(fields[startTimeIndexAfterComm], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse starttime from /proc stat: %w", err)
	}
	return ticks, nil
}

// procExe resolves /proc/<pid>/exe, falling back to argv[0] when the binary
// has been replaced underneath the running process -- which is precisely what
// a package upgrade leaves behind, and reads as "/usr/local/bin/vmsync
// (deleted)".
func procExe(pid int) (string, error) {
	exe, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
	if err == nil && !strings.HasSuffix(exe, " (deleted)") {
		return exe, nil
	}
	if err == nil {
		return strings.TrimSuffix(exe, " (deleted)"), nil
	}
	b, rerr := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if rerr != nil {
		return "", err
	}
	if i := strings.IndexByte(string(b), 0); i >= 0 {
		return string(b[:i]), nil
	}
	return strings.TrimSpace(string(b)), nil
}

// sameBinary compares two program paths by base name.
//
// Base name rather than the full path, because the two are legitimately
// different strings for the same program: the agent knows the binary as its
// configured -vmsync-path, while /proc/<pid>/exe reports the resolved target
// of every symlink along the way.
func sameBinary(a, b string) bool {
	return filepath.Base(a) == filepath.Base(b)
}
