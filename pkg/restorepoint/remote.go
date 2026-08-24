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

package restorepoint

import (
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"
)

// Every command that reaches the target host is built here, as a string, so
// that what runs against a production replica is an ordinary value a test can
// assert on. Nothing in this file executes anything.
//
// Two conventions, both inherited from util.RemotePathExists:
//
//   - A command that ASKS something always exits 0 and answers with a marker
//     on stdout, so a non-nil error from the runner means exclusively "the
//     question could not be put" -- a wedged connection, a permission problem
//     -- and can never be silently read as "no".
//   - A command that DOES something reports through its exit status, because
//     for an action there is no useful difference between "it failed" and "we
//     could not tell whether it failed": both mean do not proceed.

const (
	markerReflinkOK   = "__VMSYNC_RP_REFLINK_OK__"
	markerReflinkNo   = "__VMSYNC_RP_REFLINK_NO__"
	markerListing     = "__VMSYNC_RP_LIST__"
	markerListingNone = "__VMSYNC_RP_NONE__"
)

// shQuote is util.ShQuote, duplicated rather than imported.
//
// pkg/util pulls in syscall.Flock, which would make this package build only on
// Linux and take its tests down with it -- and being testable anywhere is the
// entire reason this package exists. pkg/failover duplicates libvirtsync's
// role constants for the same reason. Three lines is a fair price; the
// behaviour is pinned by TestShQuote.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// ProbeCommand asks whether dir's filesystem supports reflink copies.
//
// dir should be the directory the replica's disks already live in, not the
// restore point directory: asking a question must not have the side effect of
// creating something, and the two are the same filesystem anyway, which is all
// the probe is actually measuring.
//
// It writes a real 4 KiB file rather than an empty one and copies that: the
// point is to exercise the same FICLONE the replica copies will use, and a
// zero-length copy is not a convincing rehearsal of one.
//
// --reflink=always, never =auto. =auto silently falls back to a full
// byte-for-byte copy when the filesystem cannot share extents, which is the
// one failure this whole probe exists to prevent: it would turn twenty-four
// restore points of a 1 TB replica into twenty-four real terabytes without a
// word in the log.
func ProbeCommand(dir string) string {
	q := shQuote(dir)
	return fmt.Sprintf(
		"s=%s/.vmsync-reflink-probe.$$; "+
			"if dd if=/dev/zero of=\"$s.src\" bs=4096 count=1 2>/dev/null && "+
			"cp --reflink=always \"$s.src\" \"$s.dst\" 2>/dev/null; "+
			"then echo %s; else echo %s; fi; rm -f \"$s.src\" \"$s.dst\" 2>/dev/null; exit 0",
		q, markerReflinkOK, markerReflinkNo,
	)
}

// ParseProbe reads ProbeCommand's answer. An output carrying neither marker is
// an error rather than a "no": it means the command did not run as written,
// and refusing retention because of a garbled answer is better than silently
// disabling it.
func ParseProbe(out string) (bool, error) {
	switch {
	case strings.Contains(out, markerReflinkOK):
		return true, nil
	case strings.Contains(out, markerReflinkNo):
		return false, nil
	default:
		return false, fmt.Errorf("could not tell whether the target filesystem supports reflink copies; the probe answered neither way: %q", strings.TrimSpace(out))
	}
}

// StageCommand creates the staging directory for a restore point.
func StageCommand(root string, t Tag) string {
	return "mkdir -p " + shQuote(StagingDir(root, t))
}

// CopyCommand reflinks one replica disk into a staging restore point.
//
// This is the only command here that moves data, and it moves none: cp
// --reflink=always shares extents rather than reading and rewriting, so it
// costs milliseconds on an image of any size. It also never touches the
// SOURCE file, which is what keeps it clear of vmsync's own staleness guard --
// an internal qcow2 snapshot would bump the replica's mtime and make every
// subsequent incremental sync refuse to run.
func CopyCommand(root string, t Tag, diskPath string) string {
	dst := DiskPath(StagingDir(root, t), diskPath)
	return "cp --reflink=always " + shQuote(diskPath) + " " + shQuote(dst)
}

// StatusCommand writes the sidecar into a staging restore point.
func StatusCommand(root string, t Tag, s Status) (string, error) {
	b, err := s.Encode()
	if err != nil {
		return "", err
	}
	dst := path.Join(StagingDir(root, t), StatusName)
	// printf with the payload as an ARGUMENT, not as the format string, so a
	// '%' anywhere in a disk path cannot be read as a verb.
	return "printf '%s' " + shQuote(string(b)) + " > " + shQuote(dst), nil
}

// CommitCommand publishes a staged restore point.
//
// A rename within one filesystem is atomic, which is what makes a half-copied
// set impossible to mistake for a usable one. Everything before this point is
// under StagingPrefix and is self-evidently junk if a run dies.
func CommitCommand(root string, t Tag) string {
	return "mv " + shQuote(StagingDir(root, t)) + " " + shQuote(Dir(root, t))
}

// ListCommand enumerates what is under the restore point directory.
func ListCommand(root string) string {
	q := shQuote(root)
	return fmt.Sprintf("if [ -d %s ]; then echo %s; ls -1A %s 2>/dev/null; else echo %s; fi; exit 0",
		q, markerListing, q, markerListingNone)
}

// Listing is what ListCommand found.
type Listing struct {
	// Points, in the order the target reported them.
	Points []Tag
	// Staging directories left behind by interrupted runs.
	Staging []string
	// Unrecognised entries, reported rather than deleted: this package will
	// not propose rm -rf on something it could not identify.
	Unknown []string
}

// ParseListing reads ListCommand's output.
func ParseListing(out string) (Listing, error) {
	var l Listing
	lines := strings.Split(out, "\n")

	started := false
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if line == markerListingNone {
			return Listing{}, nil
		}
		if line == markerListing {
			started = true
			continue
		}
		if !started {
			continue
		}
		if strings.HasPrefix(line, StagingPrefix) {
			l.Staging = append(l.Staging, line)
			continue
		}
		t, err := ParseTag(line)
		if err != nil {
			l.Unknown = append(l.Unknown, line)
			continue
		}
		l.Points = append(l.Points, t)
	}
	if !started {
		return Listing{}, fmt.Errorf("could not read the restore point directory; the listing answered neither way: %q", strings.TrimSpace(out))
	}
	return l, nil
}

// RemoveCommand deletes one restore point.
//
// It takes a Tag, never a path, and that is a deliberate constraint rather
// than a convenience: this function emits rm -rf, and the only way to reach it
// is through a value that has already been validated as a tag. There is no
// signature here that will delete an arbitrary directory.
func RemoveCommand(root string, t Tag) string {
	return "rm -rf " + shQuote(Dir(root, t))
}

// RemoveStagingCommand deletes one abandoned staging directory.
//
// name must be something ParseListing reported as staging, and is re-checked
// here rather than trusted: it is about to be interpolated into rm -rf.
func RemoveStagingCommand(root, name string) (string, error) {
	if !strings.HasPrefix(name, StagingPrefix) {
		return "", fmt.Errorf("refusing to remove %q: not a staging directory", name)
	}
	rest := strings.TrimPrefix(name, StagingPrefix)
	if _, err := ParseTag(rest); err != nil {
		return "", fmt.Errorf("refusing to remove %q: %w", name, err)
	}
	return "rm -rf " + shQuote(path.Join(root, name)), nil
}

// AsideSuffix marks a restore point directory that -reinit moved out of the
// way instead of deleting.
const AsideSuffix = ".replaced-"

// RemoveRootCommand deletes every restore point for a replica, as -reinit does
// when -replaced-disk-action=delete.
//
// root is re-checked rather than trusted even though Root built it: this emits
// rm -rf on a directory derived from an operator-supplied path, and the tag
// validation that protects RemoveCommand does not apply one level up.
// Refusing anything not named DirName means the worst a wrong -target-disk-path
// can do is delete a directory that is, by its own name, vmsync's.
func RemoveRootCommand(root string) (string, error) {
	if err := checkRoot(root); err != nil {
		return "", err
	}
	return "rm -rf " + shQuote(root), nil
}

// RenameRootCommand moves every restore point aside instead of deleting them,
// as -reinit does when -replaced-disk-action=rename. Returns the command and
// the path the set was moved to, so the caller can name it in a warning.
//
// Renaming is the safe default for a replica disk, and it is the EXPENSIVE
// option here. The aside copies keep sharing extents among themselves, so the
// set still costs about one base image plus its deltas -- but the replica
// rebuilt by this reinit shares nothing with them, so the target now carries a
// second full base image, permanently, because nothing reaps these.
func RenameRootCommand(root string, at time.Time) (cmd, aside string, err error) {
	if err := checkRoot(root); err != nil {
		return "", "", err
	}
	aside = root + AsideSuffix + strconv.FormatInt(at.UTC().Unix(), 10)
	return "mv " + shQuote(root) + " " + shQuote(aside), aside, nil
}

func checkRoot(root string) error {
	if path.Base(root) != DirName {
		return fmt.Errorf("refusing to act on %q as a restore point directory: it is not named %q", root, DirName)
	}
	return nil
}

// ReadStatusCommand fetches one restore point's sidecar.
func ReadStatusCommand(root string, t Tag) string {
	return "cat " + shQuote(path.Join(Dir(root, t), StatusName))
}

// CloneCommand materialises a restore point's disks at a path the operator
// chose, without touching the replica or any replication state.
//
// The whole of phase 1 exists for this: during an incident the question is
// "is this copy clean", not "make the replica be this", and answering it by
// booting a scratch domain from a clone reconciles no metadata and changes no
// role.
//
// =auto here, deliberately, where the retention path insists on =always. The
// operator chose dest and it may well be on another filesystem -- a scratch
// volume, somewhere with room to boot a copy -- where =always would simply
// refuse. Falling back to a real copy is the correct answer for one clone the
// operator asked for by name, and a catastrophic one for twenty-four copies
// taken automatically, which is why the two differ.
func CloneCommand(root string, t Tag, diskPath, dest string) string {
	src := DiskPath(Dir(root, t), diskPath)
	return "cp --reflink=auto " + shQuote(src) + " " + shQuote(dest)
}
