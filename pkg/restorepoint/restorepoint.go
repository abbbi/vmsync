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

// Package restorepoint holds the rules and the shell commands for keeping
// point-in-time copies of a replica on the target host.
//
// Deliberately free of any libvirt import, and of pkg/util -- the first would
// need libvirt headers to compile and the second needs syscall.Flock, and
// either would mean this package could only be built on a Linux box with a
// hypervisor toolchain. Everything here is plain values and plain strings, so
// the retention arithmetic and every command that reaches a production target
// can be exhaustively tested anywhere. pkg/failover is the same shape for the
// same reason. The libvirt- and SSH-facing code is a thin shell whose only job
// is to run what this package builds.
//
// The design this implements, including why reflink copies rather than qcow2
// snapshots and what a restore must do about last_checkpoint, is written up in
// docs/design/restore-points.md.
package restorepoint

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DirName is the directory, beside the replica's own disks, that holds every
// restore point for those disks.
//
// A subdirectory rather than a sibling suffix, and that is not cosmetic:
// cmd/vmsync/failover.go globs <disk>_* beside the replica to detect an
// uncommitted incremental overlay, and a match BLOCKS promotion. A restore
// point named next to the disk would make the replica unpromotable. It also
// has to be on the same filesystem as the disks, which a subdirectory
// guarantees and an operator-chosen path would not -- reflink cannot cross a
// mount point.
const DirName = ".vmsync-rp"

// StagingPrefix marks a restore point that is still being built. The set is
// assembled under this name and renamed into place only once every disk has
// been copied, so a half-made restore point can never be mistaken for a usable
// one: a rename within a filesystem is atomic, a directory tree of copies is
// not. Anything left wearing this prefix is junk from an interrupted run.
const StagingPrefix = ".incomplete-"

// Verification outcomes recorded in a restore point's sidecar.
//
// VerifyNotRun is the common case, not a degenerate one, and is worth
// recording explicitly: an operator choosing between restore points during an
// incident needs to know which of them was ever actually checked.
const (
	VerifyNotRun = "not-run"
	VerifyPassed = "passed"
	VerifyFailed = "failed"
)

// checkpointRe bounds what may appear in a tag, and therefore in a directory
// name that is interpolated into remote shell commands including rm -rf.
// Anything with a path separator, a shell metacharacter or a parent reference
// is refused before it can be built into a path at all.
var checkpointRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Policy is a parsed -retention value: how many restore points to keep, and
// how much time must pass before another is taken.
type Policy struct {
	Count    int
	Interval time.Duration
}

// Enabled reports whether any restore point should be taken at all.
func (p Policy) Enabled() bool { return p.Count > 0 }

// ParsePolicy reads a -retention value, e.g. "24,3h" for twenty-four restore
// points at least three hours apart. An empty value, or a count of zero, means
// the feature is off.
//
// The comma form matches -netbuffer's existing "128k,1G". The duration is
// lowercased first because Go's own parser is case-sensitive and rejects "3H",
// which is exactly what somebody copying the documented example would type.
func ParsePolicy(s string) (Policy, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Policy{}, nil
	}
	count, interval, ok := strings.Cut(s, ",")
	if !ok {
		return Policy{}, fmt.Errorf("invalid -retention %q: want COUNT,INTERVAL (for example 24,3h)", s)
	}

	n, err := strconv.Atoi(strings.TrimSpace(count))
	if err != nil {
		return Policy{}, fmt.Errorf("invalid -retention %q: count %q is not a number", s, strings.TrimSpace(count))
	}
	if n < 0 {
		return Policy{}, fmt.Errorf("invalid -retention %q: count may not be negative", s)
	}

	d, err := time.ParseDuration(strings.ToLower(strings.TrimSpace(interval)))
	if err != nil {
		return Policy{}, fmt.Errorf("invalid -retention %q: interval %q is not a duration (for example 3h, 90m)", s, strings.TrimSpace(interval))
	}
	if d < 0 {
		return Policy{}, fmt.Errorf("invalid -retention %q: interval may not be negative", s)
	}
	return Policy{Count: n, Interval: d}, nil
}

// String renders a Policy back into the flag syntax that produced it.
func (p Policy) String() string {
	if !p.Enabled() {
		return ""
	}
	return strconv.Itoa(p.Count) + "," + p.Interval.String()
}

// Tag identifies one restore point: when the source was checkpointed, and
// which checkpoint that was.
//
// The instant leads, and the checkpoint name alone would not do. -reinit
// restarts the source's chain at vmsync-cpt-000001, so checkpoint names repeat
// across reinits and across inversions; the source-clock instant keeps restore
// points sortable and unambiguous for the life of a pair.
type Tag struct {
	At         time.Time
	Checkpoint string
}

// String renders the directory name for a tag.
func (t Tag) String() string {
	return strconv.FormatInt(t.At.Unix(), 10) + "-" + t.Checkpoint
}

// NewTag builds a validated tag.
func NewTag(at time.Time, checkpoint string) (Tag, error) {
	if !checkpointRe.MatchString(checkpoint) {
		return Tag{}, fmt.Errorf("refusing checkpoint name %q as a restore point tag: only letters, digits, '.', '_' and '-' are allowed, and it must not start with '.'", checkpoint)
	}
	if at.IsZero() {
		return Tag{}, fmt.Errorf("refusing a restore point tag with no checkpoint time")
	}
	return Tag{At: at.UTC().Truncate(time.Second), Checkpoint: checkpoint}, nil
}

// ParseTag reads a directory name back into a Tag. Cut at the FIRST separator,
// because checkpoint names contain hyphens of their own.
func ParseTag(name string) (Tag, error) {
	unix, checkpoint, ok := strings.Cut(name, "-")
	if !ok {
		return Tag{}, fmt.Errorf("%q is not a restore point tag: no separator", name)
	}
	secs, err := strconv.ParseInt(unix, 10, 64)
	if err != nil {
		return Tag{}, fmt.Errorf("%q is not a restore point tag: %q is not a unix time", name, unix)
	}
	return NewTag(time.Unix(secs, 0).UTC(), checkpoint)
}

// Root is the restore point directory for a replica disk.
func Root(diskPath string) string {
	return path.Join(path.Dir(diskPath), DirName)
}

// Dir is where a finished restore point lives.
func Dir(root string, t Tag) string { return path.Join(root, t.String()) }

// StagingDir is where one is assembled before being renamed into place.
func StagingDir(root string, t Tag) string { return path.Join(root, StagingPrefix+t.String()) }

// DiskPath is where one replica disk's copy lives inside a restore point.
// Named after the disk's own basename, so a restore point is a drop-in set.
func DiskPath(dir, diskPath string) string { return path.Join(dir, path.Base(diskPath)) }

// StatusName is the sidecar inside each restore point.
const StatusName = "status.json"

// Status is what is actually known about a restore point.
//
// It exists because a restore point is taken BEFORE -verify runs, so its
// contents are not yet known to be good -- and because verify runs before the
// sync's own metadata is written, a restore point can outlive a run that then
// failed. The image is still valid; what varies is how much confidence to
// place in it, and that is what this records.
type Status struct {
	Checkpoint   string   `json:"checkpoint"`
	CheckpointAt int64    `json:"checkpoint_at"`
	TakenAt      int64    `json:"taken_at"`
	Source       string   `json:"source"`
	Verify       string   `json:"verify"`
	Disks        []string `json:"disks"`
}

// Encode renders a sidecar. Indented because the first reader of one of these
// is an operator during an incident, cat-ing it over SSH.
func (s Status) Encode() ([]byte, error) {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode restore point status: %w", err)
	}
	return append(b, '\n'), nil
}

// DecodeStatus reads a sidecar back.
func DecodeStatus(b []byte) (Status, error) {
	var s Status
	if err := json.Unmarshal(b, &s); err != nil {
		return Status{}, fmt.Errorf("decode restore point status: %w", err)
	}
	return s, nil
}

// Due reports whether another restore point should be taken now.
//
// The interval is a FLOOR, not a schedule, and the distinction is the one
// thing about this feature most likely to be misread. vmsync does not decide
// when it runs -- cron or the agent does -- so this can only answer "has
// enough time passed", never "take one every three hours". If syncs run every
// four hours, restore points are four hours apart; if replication is paused
// for a day, there is a day-long gap. The COUNT is the guarantee; the window
// it nominally covers is not.
//
// A zero latest means none exists yet, which is always due.
func Due(latest time.Time, now time.Time, p Policy) bool {
	if !p.Enabled() {
		return false
	}
	if latest.IsZero() {
		return true
	}
	return !now.Before(latest.Add(p.Interval))
}

// Plan is what to do with the restore points that already exist.
type Plan struct {
	// Keep, newest first.
	Keep []Tag
	// Remove, oldest first, so a caller that deletes them in order frees the
	// least useful history first and stays recoverable if it is interrupted.
	Remove []Tag
}

// Prune decides which restore points to keep and which to delete, counting a
// tag that is about to be created.
//
// Pruning AFTER the new one is in place, never before, so an interrupted run
// leaves too many rather than too few. Too many costs disk; too few costs the
// thing the feature exists to provide.
func Prune(existing []Tag, p Policy) Plan {
	sorted := make([]Tag, len(existing))
	copy(sorted, existing)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].At.Equal(sorted[j].At) {
			return sorted[i].Checkpoint > sorted[j].Checkpoint
		}
		return sorted[i].At.After(sorted[j].At)
	})

	if !p.Enabled() {
		// Retention off: nothing is kept by policy, so everything present is
		// somebody's leftovers. The caller decides whether to act on that;
		// this only reports it.
		return Plan{Remove: reversed(sorted)}
	}
	if len(sorted) <= p.Count {
		return Plan{Keep: sorted}
	}
	return Plan{Keep: sorted[:p.Count], Remove: reversed(sorted[p.Count:])}
}

func reversed(in []Tag) []Tag {
	if len(in) == 0 {
		return nil
	}
	out := make([]Tag, len(in))
	for i, t := range in {
		out[len(in)-1-i] = t
	}
	return out
}

// Latest returns the newest tag, or the zero time when there are none.
func Latest(existing []Tag) time.Time {
	var newest time.Time
	for _, t := range existing {
		if t.At.After(newest) {
			newest = t.At
		}
	}
	return newest
}
