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
//
// A third convention, added with the per-domain layout: every builder that
// names a directory takes a Store or a Point and returns an error. Not for the
// sake of symmetry -- the alternative is that an unset value silently becomes a
// RELATIVE path, resolved against whatever directory the SSH login lands in, in
// a command that can be rm -rf. See Store.check.

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
//
// mkdir -p, so it creates the domain's store and DirName above it on the first
// run of a pair. That is the only place either is created: a read never makes a
// directory, which is why ListCommand answers "none" instead.
func StageCommand(p Point) (string, error) {
	dir, err := p.staging()
	if err != nil {
		return "", err
	}
	return "mkdir -p " + shQuote(dir), nil
}

// CopyCommand reflinks one replica disk into a staging restore point.
//
// This is the only command here that moves data, and it moves none: cp
// --reflink=always shares extents rather than reading and rewriting, so it
// costs milliseconds on an image of any size. It also never touches the
// SOURCE file, which is what keeps it clear of vmsync's own staleness guard --
// an internal qcow2 snapshot would bump the replica's mtime and make every
// subsequent incremental sync refuse to run.
func CopyCommand(p Point, diskPath string) (string, error) {
	staging, err := p.staging()
	if err != nil {
		return "", err
	}
	dst := DiskPath(staging, diskPath)
	return "cp --reflink=always " + shQuote(diskPath) + " " + shQuote(dst), nil
}

// StatusCommand writes the sidecar into a staging restore point.
func StatusCommand(p Point, s Status) (string, error) {
	staging, err := p.staging()
	if err != nil {
		return "", err
	}
	b, err := s.Encode()
	if err != nil {
		return "", err
	}
	dst := path.Join(staging, StatusName)
	// printf with the payload as an ARGUMENT, not as the format string, so a
	// '%' anywhere in a disk path cannot be read as a verb.
	return "printf '%s' " + shQuote(string(b)) + " > " + shQuote(dst), nil
}

// CommitCommand publishes a staged restore point.
//
// A rename within one filesystem is atomic, which is what makes a half-copied
// set impossible to mistake for a usable one. Everything before this point is
// under StagingPrefix and is self-evidently junk if a run dies.
func CommitCommand(p Point) (string, error) {
	staging, err := p.staging()
	if err != nil {
		return "", err
	}
	dir, err := p.dir()
	if err != nil {
		return "", err
	}
	return "mv " + shQuote(staging) + " " + shQuote(dir), nil
}

// ListCommand enumerates what is under ONE DOMAIN's restore point directory.
//
// -p so the answer says which entries are directories. Without it the reader
// classifies by name alone, and a regular FILE named like a tag would be
// reported as a restore point -- then staged into, pruned, and handed to
// rm -rf. pkg/inventory's local reader has always checked IsDir; this is the
// remote reader catching up to it.
func ListCommand(s Store) (string, error) {
	root, err := s.Path()
	if err != nil {
		return "", err
	}
	q := shQuote(root)
	return fmt.Sprintf("if [ -d %s ]; then echo %s; ls -1Ap %s 2>/dev/null; else echo %s; fi; exit 0",
		q, markerListing, q, markerListingNone), nil
}

// Listing is what ListCommand found in one domain's store.
type Listing struct {
	// Points, in the order the target reported them.
	Points []Tag
	// Staging directories left behind by interrupted runs.
	Staging []string
	// Unrecognised entries, reported rather than deleted: this package will
	// not propose rm -rf on something it could not identify. Anything that is
	// not a directory lands here too, whatever it is named.
	Unknown []string
}

// ParseListing reads ListCommand's output.
//
// Output carrying NEITHER marker is already an error rather than an empty
// store, because parseEntries refuses it -- see the end of that function.
// Worth knowing when reading ParseAllStatus, which had to grow the same
// refusal of its own: a sidecar read is a different command against the same
// directory, and the two answers must not be interchangeable.
func ParseListing(out string) (Listing, error) {
	var l Listing
	entries, none, err := parseEntries(out)
	if err != nil || none {
		return Listing{}, err
	}
	for _, e := range entries {
		if !e.dir {
			l.Unknown = append(l.Unknown, e.name)
			continue
		}
		if strings.HasPrefix(e.name, StagingPrefix) {
			l.Staging = append(l.Staging, e.name)
			continue
		}
		t, err := ParseTag(e.name)
		if err != nil {
			l.Unknown = append(l.Unknown, e.name)
			continue
		}
		l.Points = append(l.Points, t)
	}
	return l, nil
}

type entry struct {
	name string
	dir  bool
}

// parseEntries reads a listing command's output into names plus whether each is
// a directory. none is true for "the directory does not exist", which is not an
// error: a host that has never used -retention is the ordinary case.
func parseEntries(out string) (entries []entry, none bool, err error) {
	started := false
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if line == markerListingNone {
			return nil, true, nil
		}
		if line == markerListing {
			started = true
			continue
		}
		if !started {
			continue
		}
		// ls -1Ap appends "/" to a directory and to nothing else. A symlink,
		// even one pointing at a directory, arrives without it and is
		// therefore Unknown -- which is the safe answer: rm -rf and mv act on
		// the link rather than on what it points at, so vmsync should not be
		// treating one as a restore point it owns.
		if name := strings.TrimSuffix(line, "/"); name != line {
			entries = append(entries, entry{name: name, dir: true})
		} else {
			entries = append(entries, entry{name: line})
		}
	}
	if !started {
		return nil, false, fmt.Errorf("could not read the restore point directory; the listing answered neither way: %q", strings.TrimSpace(out))
	}
	return entries, false, nil
}

// RemoveCommand deletes one restore point.
//
// It takes a Point, never a path, and that is a deliberate constraint rather
// than a convenience: this function emits rm -rf, and the only way to reach it
// is through a Tag that has already been validated inside a Store that has
// already been checked. There is no signature here that will delete an
// arbitrary directory -- and no signature at all that deletes a LEGACY point,
// which is how "a pre-change point is never pruned" is a compile error rather
// than a rule.
func RemoveCommand(p Point) (string, error) {
	if err := p.store.check(); err != nil {
		return "", err
	}
	dir, err := p.dir()
	if err != nil {
		return "", err
	}
	return "rm -rf " + shQuote(dir), nil
}

// RemoveStagingCommand deletes one abandoned staging directory from THIS
// domain's store.
//
// name must be something ParseListing reported as staging, and is re-checked
// here rather than trusted: it is about to be interpolated into rm -rf.
//
// The store it is removed from is this domain's, which closes the third of the
// four ways a shared store goes wrong: a sweep over the directory every
// co-located domain stages into removes a concurrently running sibling's
// in-flight set and fails that sync after it has already copied its data.
func RemoveStagingCommand(s Store, name string) (string, error) {
	if err := s.check(); err != nil {
		return "", err
	}
	if !strings.HasPrefix(name, StagingPrefix) {
		return "", fmt.Errorf("refusing to remove %q: not a staging directory", name)
	}
	rest := strings.TrimPrefix(name, StagingPrefix)
	if _, err := ParseTag(rest); err != nil {
		return "", fmt.Errorf("refusing to remove %q: %w", name, err)
	}
	root, err := s.Path()
	if err != nil {
		return "", err
	}
	return "rm -rf " + shQuote(path.Join(root, name)), nil
}

// RemoveStoreCommand deletes every restore point of ONE target domain, as
// -reinit does when -replaced-disk-action=delete.
//
// Scoped to one store, which closes the fourth way a shared store goes wrong:
// an rm -rf of the whole shared root means reinitialising one domain destroys
// every co-located domain's entire history in one command -- while the log
// says only that it removed "the restore points belonging to the replaced
// replica".
//
// The store is re-checked rather than trusted even though a constructor built
// it: this emits rm -rf on a path derived from an operator-supplied
// -target-disk-path, and the tag validation that protects RemoveCommand does
// not apply one level up. See Store.check for what each condition closes.
func RemoveStoreCommand(s Store) (string, error) {
	if err := s.check(); err != nil {
		return "", err
	}
	root, err := s.Path()
	if err != nil {
		return "", err
	}
	return "rm -rf " + shQuote(root), nil
}

// RenameStoreCommand moves one target domain's restore points aside instead of
// deleting them, as -reinit does when -replaced-disk-action=rename. Returns the
// command and the path the set was moved to, so the caller can name it in a
// warning.
//
// Renaming is the safe default for a replica disk, and it is the EXPENSIVE
// option here. The aside copies keep sharing extents among themselves, so the
// set still costs about one base image plus its deltas -- but the replica
// rebuilt by this reinit shares nothing with them, so the target now carries a
// second full base image, permanently, because nothing reaps these.
//
// The aside is named with AsidePrefix, one level up beside the stores rather
// than inside this one: it must not be a path any later classification reads as
// a live store, or a second reinit would believe it owns it.
//
// It refuses rather than overwrites when the destination is already there. Two
// reinits in the same second would otherwise have mv put the second set INSIDE
// the first -- mv's behaviour when the destination is an existing directory --
// which buries one history inside another under a name nothing looks in.
func RenameStoreCommand(s Store, at time.Time) (cmd, aside string, err error) {
	if err := s.check(); err != nil {
		return "", "", err
	}
	root, err := s.Path()
	if err != nil {
		return "", "", err
	}
	aside = path.Join(path.Dir(root), AsidePrefix+s.segment+"-"+strconv.FormatInt(at.UTC().Unix(), 10))
	q := shQuote(aside)
	return "if [ -e " + q + " ]; then echo 'restore point aside path already exists: " + AsidePrefix + "' >&2; exit 1; fi; " +
		"mv -- " + shQuote(root) + " " + q, aside, nil
}

// ReadStatusCommand fetches one restore point's sidecar.
//
// Takes a pointRef, so it serves this domain's points and legacy ones alike:
// reading is the one thing a pre-change point must still support, because it is
// the sidecar that says whose it was and whether it was ever verified.
func ReadStatusCommand(p Point) (string, error) {
	dir, err := p.dir()
	if err != nil {
		return "", err
	}
	return "cat " + shQuote(path.Join(dir, StatusName)), nil
}

// Markers for ReadAllStatusCommand.
//
// Distinct from the listing's, INCLUDING the outer pair. An earlier version of
// this bracketed the sidecar read with markerListing/markerListingNone and
// claimed in this comment that the two could not be confused -- which was
// false in the direction that mattered: ParseListing read a sidecar dump as a
// successful listing of a store with no points, and ParseAllStatus read a
// listing as a store whose points have no sidecars. Both answers are wrong and
// neither is an error, which is the shape of mistake this package's marker
// convention exists to make impossible.
const (
	markerStatus      = "__VMSYNC_RP_SC_LIST__"
	markerStatusPoint = "__VMSYNC_RP_SC__"
	// markerStatusEnd terminates a record. REDUNDANT BY CONSTRUCTION today
	// and kept deliberately: the next header closes the record before it,
	// and the end of the stream closes the last one, so removing its echo
	// changes no behaviour and no test can tell the difference. It is here
	// because a record whose extent is stated is one a future change cannot
	// silently extend -- anything added between records would otherwise be
	// read as the previous record's body. Do not write a test asserting it
	// is load-bearing; it is not, and that comment would be the defect.
	markerStatusEnd = "__VMSYNC_RP_SC_END__"
)

// ReadAllStatusCommand fetches every sidecar in one domain's store, in a
// single round trip, each preceded by the directory it came from.
//
// ONE command rather than ReadStatusCommand per point, because this runs on
// the prune path once per sync: a store at -retention=24 would otherwise pay
// twenty-four SSH round trips to answer a question the caller needs in full
// before it can decide anything.
//
// The shell only cats. Every byte between the markers is decoded in Go by
// DecodeStatus -- nothing here reads JSON in sh, which would be fragile and a
// second implementation of a format Status already owns.
//
// A point whose sidecar is missing or unreadable yields an EMPTY body rather
// than failing the command, and that distinction matters: "this point has no
// recorded publish time" and "the store could not be read at all" are
// different answers, and Prune treats them differently.
func ReadAllStatusCommand(s Store, points []Tag) (string, error) {
	root, err := s.Path()
	if err != nil {
		return "", err
	}
	if len(points) == 0 {
		// Nothing to ask about. Answered rather than skipped so the caller
		// still gets "the store was read" instead of a failure.
		return "echo " + markerStatus + "; exit 0", nil
	}

	// NAMED, NOT GLOBBED, and the record header carries the TAG rather than
	// the directory. This is the whole defence of the framing.
	//
	// The framing is newline-delimited, so whatever sits on a header line
	// decides where one record ends and the next begins. A shell glob puts a
	// FILESYSTEM path there -- built from an operator-supplied
	// -target-disk-path and -target-domain, neither of which rejects a
	// newline (see store.go's SafeKey, which says so outright). One newline
	// in that path splits every header in two, every record is dropped, and
	// ParseAllStatus has still seen its opening marker: it returns an empty
	// map and NO error, the caller reads that as "the store has no publish
	// instants", and retention silently falls back to ordering by the
	// checkpoint instant -- which is audit item 4.9 restored in full, with
	// every number healthy. printf instead of echo does not help: it stops
	// the two-character sequence being expanded, not a literal newline.
	//
	// A Tag cannot do that. It is unix seconds, a hyphen, and a checkpoint
	// name that checkpointRe has already constrained to [A-Za-z0-9._-], so a
	// header built from one holds no newline, no marker and no shell
	// metacharacter. The caller has the tags already: they come from the
	// listing it just parsed, which takes the path only as a quoted argument
	// to ls and is immune to all of this.
	//
	// Concatenated rather than Sprintf'd, because the command contains a
	// printf format of its own and the two sets of verbs would have to be
	// escaped past each other to no benefit. The tag still goes through
	// printf as an ARGUMENT, never inside the format -- StatusCommand's rule,
	// kept here even though a validated tag could not exploit it, because the
	// next person to edit this line should not have to re-derive why.
	var b strings.Builder
	b.WriteString("echo " + markerStatus + "; ")
	for _, t := range points {
		b.WriteString("printf '%s %s\\n' " + shQuote(markerStatusPoint) + " " + shQuote(t.String()) + "; ")
		b.WriteString("cat " + shQuote(path.Join(root, t.String(), StatusName)) + " 2>/dev/null; echo; ")
		b.WriteString("echo " + markerStatusEnd + "; ")
	}
	b.WriteString("exit 0")
	return b.String(), nil
}

// ParseAllStatus reads ReadAllStatusCommand's output into the instant THIS
// host published each point, keyed by tag.
//
// Best-effort per point, and OMISSION is the failure mode rather than a
// default: a sidecar that cannot be decoded, or that carries no taken_at, is
// left out of the map entirely. Prune distinguishes "published at this
// instant" from "no recorded instant", and a zero time would be neither --
// it would claim the point was published in 1970 and make it the first thing
// deleted.
//
// A directory name that is not a tag is skipped in silence. The listing is
// what reports unrecognised entries; doing it here too would warn twice about
// the same directory on every single sync.
//
// AN ERROR AND AN EMPTY MAP ARE DIFFERENT ANSWERS, which is why this returns
// both. "The store holds no points, or none with sidecars" is a fact the
// caller can prune on. "This output is not mine" -- no opening marker, so a
// listing, an error banner, an SSH MOTD or a truncated stream -- is not: it
// is indistinguishable from the first if both come back as an empty map, and
// the caller would then prune by the checkpoint instant believing it had
// looked. That is the defect this whole change exists to stop, reachable by
// a mismatched marker rather than by a skewed clock.
func ParseAllStatus(out string, asked []Tag) (map[string]time.Time, error) {
	published := map[string]time.Time{}

	// POSITION DECIDES WHICH POINT A RECORD IS ABOUT. The header is only
	// checked against it.
	//
	// The framing is in-band: a line inside a sidecar BODY that begins with
	// the point marker opens a new record. If the header were authoritative,
	// a hand-edited or foreign status.json could name any point it liked --
	// and naming one that WAS asked about is the case that matters, because
	// the command asks in listing order and a tag begins with unix seconds,
	// so the oldest point is read first and its body can speak for every
	// point after it. A forged instant in the future pins that point at the
	// top of the order and gets the honest copies around it deleted: audit
	// 4.9's steady state with no clock wrong anywhere.
	//
	// An earlier version here kept a set of asked-for names and took the
	// first record per point. That stops a forgery that FOLLOWS the genuine
	// record and not one that precedes it -- and preceding is the reachable
	// direction. Correlating by position removes the question: the command
	// is built from this same slice, in this order, one record each.
	//
	// A record count that does not match is an ERROR, not a partial answer.
	// An injected header shifts every record after it, so refusing the whole
	// read is both the only safe reading and the one that fails closed: the
	// caller withholds the prune rather than deleting by an order it cannot
	// account for.
	type record struct {
		header string
		body   []string
	}
	var records []record
	var cur *record
	started := false

	closeRecord := func() {
		if cur != nil {
			records = append(records, *cur)
			cur = nil
		}
	}

	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if !started {
			if trimmed == markerStatus {
				started = true
			}
			continue
		}
		switch {
		case trimmed == markerStatusEnd:
			closeRecord()
		case strings.HasPrefix(trimmed, markerStatusPoint+" "):
			// Opening a record closes the one before it, so a transfer cut
			// mid-body costs that record and not the one after it.
			closeRecord()
			cur = &record{header: strings.TrimSpace(strings.TrimPrefix(trimmed, markerStatusPoint+" "))}
		case cur != nil:
			cur.body = append(cur.body, line)
		}
	}
	closeRecord()

	if !started {
		return nil, fmt.Errorf("the target's answer did not begin with %s, so it is not a restore point sidecar read", markerStatus)
	}
	if len(records) != len(asked) {
		return nil, fmt.Errorf("asked the target about %d restore points and its answer held %d records; refusing to guess which is which", len(asked), len(records))
	}
	for i, rec := range records {
		want := asked[i].String()
		if rec.header != want {
			return nil, fmt.Errorf("record %d of the target's answer is headed %q where %q was asked for", i+1, rec.header, want)
		}
		st, err := DecodeStatus([]byte(strings.Join(rec.body, "\n")))
		if err != nil || st.TakenAt <= 0 {
			// Omitted, not defaulted. A zero would claim the point was
			// published in 1970 and make it the first thing deleted.
			continue
		}
		published[want] = time.Unix(st.TakenAt, 0).UTC()
	}
	return published, nil
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
func CloneCommand(p Point, diskPath, dest string) (string, error) {
	dir, err := p.dir()
	if err != nil {
		return "", err
	}
	src := DiskPath(dir, diskPath)
	return "cp --reflink=auto " + shQuote(src) + " " + shQuote(dest), nil
}
