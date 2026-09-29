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
	"sort"
	"strconv"
	"strings"
	"time"
)

// The displaced sets vmsync's own operations leave on a target, and the
// commands that find and remove them.
//
// Three namings, made by three operations, and nothing has ever removed any of
// them:
//
//   - "<disk>.vmsync-replaced-<unix>" -- a -reinit renamed the previous replica
//     disks aside before writing new ones. A FULL-SIZE file per disk. It is the
//     documented recovery for a rebuild that died half way, which is why it is
//     made and why it must not be swept on sight.
//   - "<disk>.vmsync-restoring-<unix>" -- a restore staged a copy beside the
//     replica and did not get as far as promoting it. Junk from an interrupted
//     run, the same class as a staging directory.
//   - ".replaced-<segment>-<unix>" beside the per-domain stores -- a -reinit
//     moved a whole restore-point store out of the way. This one is invisible
//     to every listing there is: -list-restore-points reads INSIDE one store,
//     and the points in here are outside every store, so no command has ever
//     been able to name them. Only the directory's size said they were there.
//
// A fresh aside shares every extent with the file beside it, so the day it is
// made it costs nothing and the day it matters it costs a full replica. That is
// what makes it accumulate unnoticed until a commit fails with ENOSPC for every
// VM on the host.
const (
	// LeftoverReplacedDisk is a disk a -reinit renamed aside.
	LeftoverReplacedDisk = "replaced-disk"
	// LeftoverRestoreStaging is a restore's staged copy, never promoted.
	LeftoverRestoreStaging = "restore-staging"
	// LeftoverAsideStore is a restore-point store a -reinit moved aside.
	LeftoverAsideStore = "aside-store"
)

// ReplicaReplacedSuffix precedes the unix stamp on a disk a -reinit renamed
// aside: "<disk>.vmsync-replaced-<unix>".
//
// A third copy of one string, which wants justifying. cmd/vmsync owns the
// original (replacedDiskSuffix) and creates the files; pkg/failover holds the
// second because its refusal has to NAME the suffix and a package cannot import
// a main package; this is the third because the commands that find and remove
// those files are built here, where every other remote command and the one
// audited quoting function live. Importing pkg/failover for one constant would
// couple two leaf packages that otherwise share nothing. All three copies are
// pinned against each other by TestReplacedDiskSuffixIsOneString in cmd/vmsync.
const ReplicaReplacedSuffix = ".vmsync-replaced-"

const (
	markerScan     = "__VMSYNC_LEFTOVER_SCAN__"
	markerScanNone = "__VMSYNC_LEFTOVER_NONE__"
)

// MinReclaimAfter is the shortest age a reclaim may be asked to use.
//
// 24h rather than something smaller, because of what is being deleted. A disk a
// -reinit renamed aside is the complete replica that rebuild replaced, and the
// window in which somebody notices the rebuild went wrong and wants it back is
// measured in hours to days -- an operator who set "1h" would have automated away
// the recovery they were relying on. There is no ceiling: a year is a perfectly
// sensible answer to "how long do I want to be able to undo a rebuild".
//
// It lives here, in the package neither binary owns, because BOTH check it:
// vmsync at startup, and vmsync-agent when a profile is saved, so a duration
// nobody can obey is refused at the moment it is written rather than at 02:00 on
// the night the schedule next fires. One definition rather than a copy each --
// the suffix above needs three copies and a test to hold them together, and that
// is a price worth paying once, not twice.
const MinReclaimAfter = 24 * time.Hour

// Leftover is one displaced set.
type Leftover struct {
	// Path is absolute, as the target reported it.
	Path string
	// Kind is one of the three Leftover* constants.
	Kind string
	// AtUnix is when it was displaced: the stamp in the name, and the entry's
	// mtime only when the name carries none. The stamp is preferred because it
	// is what the operation wrote and cannot drift -- a directory's mtime
	// changes when anything inside it is touched, including by the very sweep
	// that is deciding whether it is old.
	AtUnix int64
	// StampFromName records which of the two AtUnix came from, so a caller can
	// say so rather than presenting a guess as a fact.
	StampFromName bool
	// Bytes is what it occupies now, allocated rather than apparent, and 0
	// until LeftoverSizeCommand has been run for it. Allocated because a fresh
	// reflink aside's apparent size is a whole disk image and its real cost is
	// nothing: the gap between the two IS the thing worth reporting.
	Bytes int64
}

// LeftoverScanCommand lists everything directly inside each of dirs, and inside
// storesParent, for a caller to attribute and filter.
//
// It deliberately does NOT filter on the target. Every name that decides
// whether a file is deleted is matched in Go, by ParseLeftoverScan and
// AttributeLeftovers, where it is covered by tests -- rather than in a shell
// glob, where a disk filename containing a glob metacharacter would silently
// widen what matches. This command is the one that could not be wrong: it
// reports, and something else decides.
//
// One find per directory rather than one find over all of them, so a directory
// that has gone (a disk removed from the domain since) produces one skipped
// entry rather than failing the scan.
func LeftoverScanCommand(dirs []string, storesParent string) (string, error) {
	var all []string
	seen := map[string]bool{}
	for _, d := range append(append([]string{}, dirs...), storesParent) {
		if d == "" {
			continue
		}
		if !path.IsAbs(d) {
			// Refused rather than skipped: a relative directory would be
			// resolved against whatever directory the SSH login lands in, and
			// this scan's output decides what rm -rf is pointed at.
			return "", fmt.Errorf("refusing to scan %q for displaced sets: it is not an absolute path, so it names a different directory depending on where the login lands", d)
		}
		c := path.Clean(d)
		if seen[c] {
			continue
		}
		seen[c] = true
		all = append(all, c)
	}
	if len(all) == 0 {
		return "", fmt.Errorf("refusing to scan for displaced sets without a directory to look in")
	}
	// Sorted so the command is deterministic, which is what lets a test pin it
	// and an operator diff two runs.
	sort.Strings(all)

	var b strings.Builder
	fmt.Fprintf(&b, "echo %s; ", markerScan)
	for _, d := range all {
		// -mindepth 1 -maxdepth 1: this level only. A displaced disk sits beside
		// its replica and an aside store beside the live stores; nothing below
		// either is a separate finding, and descending into an aside store would
		// report every restore point inside it as its own leftover.
		//
		// %y type, %T@ mtime, %p path -- tab separated, in that order.
		fmt.Fprintf(&b, "find %s -mindepth 1 -maxdepth 1 -printf '%%y\\t%%T@\\t%%p\\n' 2>/dev/null; ", shQuote(d))
	}
	// exit 0: a directory that is gone is not an error, and the marker already
	// distinguishes "nothing found" from "the command never ran".
	b.WriteString("exit 0")
	return b.String(), nil
}

// ParseLeftoverScan reads LeftoverScanCommand's output into raw entries.
//
// Nothing is interpreted here beyond the line format: what each entry IS is
// AttributeLeftovers' job, because that decision needs to know whose disks
// these are.
func ParseLeftoverScan(out string) ([]ScanEntry, error) {
	lines := strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n")
	started := false
	var entries []ScanEntry
	for _, ln := range lines {
		ln = strings.TrimRight(ln, "\r")
		if !started {
			if strings.TrimSpace(ln) == markerScan {
				started = true
			}
			continue
		}
		if strings.TrimSpace(ln) == "" {
			continue
		}
		f := strings.Split(ln, "\t")
		if len(f) != 3 {
			// Not parseable, so not identifiable, so not a candidate for
			// removal. Reported so it is visible rather than dropped: the one
			// way to produce this is a path containing a tab or a newline, and
			// an operator should hear that such a file exists.
			entries = append(entries, ScanEntry{Unparsed: ln})
			continue
		}
		mtime, _ := strconv.ParseFloat(f[1], 64)
		entries = append(entries, ScanEntry{
			Dir:       f[0] == "d",
			MtimeUnix: int64(mtime),
			Path:      f[2],
		})
	}
	if !started {
		return nil, fmt.Errorf("scanning for displaced sets produced no output at all (no %s marker): the command did not run", markerScan)
	}
	return entries, nil
}

// ScanEntry is one line of LeftoverScanCommand's output, uninterpreted.
type ScanEntry struct {
	Dir       bool
	MtimeUnix int64
	Path      string
	// Unparsed holds the whole line when it did not have the expected shape,
	// and Path is then empty. Such an entry is never a removal candidate.
	Unparsed string
}

// AttributeLeftovers decides which scanned entries are THIS domain's displaced
// sets.
//
// diskPaths are the domain's own replica disks, absolute. domain is the target
// domain name, used to rebuild its own store segment.
//
// Attribution is the whole difficulty, and getting it wrong is worse than not
// scanning at all. Co-locating replicas in one images directory is the default,
// so a scan reads a directory holding a dozen machines' disks and a stores
// parent holding a dozen machines' asides. "Contains the suffix" would charge
// every displaced set to every domain -- a twelve-fold over-count, and a path
// offered as this machine's reclaimable space that is another machine's only
// good copy. Both namings are therefore anchored:
//
//   - a displaced disk must be exactly one of THIS domain's disk paths followed
//     by the suffix, and
//   - an aside store must be exactly this domain's own segment followed by a
//     dash and a stamp.
//
// The aside-store match is whole-name rather than a prefix test on purpose: a
// prefix test also matches every domain whose name merely starts with this
// one's, so web01 would be charged for web01-old and web01-staging. An aside
// somebody renamed by hand is missed as a result, which is the right way round
// -- a set this misses still shows up in df, while one it misattributes sends an
// operator to delete another machine's restore points.
func AttributeLeftovers(entries []ScanEntry, diskPaths []string, domain string) ([]Leftover, []string) {
	disks := map[string]bool{}
	for _, p := range diskPaths {
		if p != "" {
			disks[path.Clean(p)] = true
		}
	}
	asidePrefix := ""
	if seg, err := DomainSegment(domain); err == nil {
		asidePrefix = AsidePrefix + seg + "-"
	}

	var out []Leftover
	var unparsed []string
	for _, e := range entries {
		if e.Unparsed != "" {
			unparsed = append(unparsed, e.Unparsed)
			continue
		}
		p := path.Clean(e.Path)
		l := Leftover{Path: p}
		switch {
		case !e.Dir && displacedDisk(p, disks, ReplicaReplacedSuffix):
			l.Kind = LeftoverReplacedDisk
			l.AtUnix, l.StampFromName = stampAfter(p, ReplicaReplacedSuffix)
		case !e.Dir && displacedDisk(p, disks, RestoreTempSuffix):
			l.Kind = LeftoverRestoreStaging
			l.AtUnix, l.StampFromName = stampAfter(p, RestoreTempSuffix)
		case e.Dir && asidePrefix != "" && sameDirAside(p, asidePrefix):
			l.Kind = LeftoverAsideStore
			l.AtUnix, l.StampFromName = stampAfter(p, asidePrefix)
		default:
			continue
		}
		if !l.StampFromName {
			l.AtUnix = e.MtimeUnix
		}
		out = append(out, l)
	}
	// Newest first: the first line is the one an operator is most likely to
	// still want, so a truncated listing truncates the safe end.
	sort.Slice(out, func(i, j int) bool {
		if out[i].AtUnix != out[j].AtUnix {
			return out[i].AtUnix > out[j].AtUnix
		}
		return out[i].Path < out[j].Path
	})
	return out, unparsed
}

// displacedDisk reports whether p is one of disks displaced with sep.
//
// The FIRST occurrence of sep, deliberately, and it has to match
// inventory.displacedFrom -- the agent's scan, which reports the same files this
// sweep removes. A name carrying the suffix twice can only come from a hand
// rename, since vmsync renames a live disk aside and never an aside; but with
// LastIndex here and Index there, such a file would be reported by every agent
// report for ever and reclaimed by nothing, which is the one outcome worse than
// either answer on its own. The stamp still comes from the LAST occurrence: what
// is being asked there is "when was this displaced", and the newest stamp is the
// answer.
func displacedDisk(p string, disks map[string]bool, sep string) bool {
	i := strings.Index(p, sep)
	if i <= 0 {
		return false
	}
	return disks[p[:i]]
}

// sameDirAside reports whether p's last component is prefix followed by a stamp
// and nothing else.
func sameDirAside(p, prefix string) bool {
	name := path.Base(p)
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	_, ok := allDigits(name[len(prefix):])
	return ok
}

// stampAfter reads the unix stamp following the last occurrence of sep.
func stampAfter(p, sep string) (int64, bool) {
	i := strings.LastIndex(p, sep)
	if i < 0 {
		return 0, false
	}
	return allDigits(p[i+len(sep):])
}

func allDigits(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// LeftoverSizeCommand asks what each of these occupies, allocated.
//
// du rather than stat, because an aside store is a directory and what matters
// is the tree. --block-size=1 so the answer is bytes and nothing has to guess
// at a unit; du reports DISK USAGE, which is the allocated size, so a fresh
// reflink aside correctly reports near nothing and the same aside reports a
// full replica once the live disk has been rewritten around it.
func LeftoverSizeCommand(paths []string) (string, error) {
	if len(paths) == 0 {
		return "", fmt.Errorf("refusing to measure nothing")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "echo %s; ", markerScan)
	for _, p := range paths {
		if err := checkLeftoverPath(p); err != nil {
			return "", err
		}
		// One du per path rather than one du over all of them: du with several
		// arguments still prints one line each, but a single missing path makes
		// it exit non-zero, and a path that has gone between the scan and here
		// is ordinary rather than a failure.
		fmt.Fprintf(&b, "du -s --block-size=1 -- %s 2>/dev/null; ", shQuote(p))
	}
	b.WriteString("exit 0")
	return b.String(), nil
}

// ParseLeftoverSizes reads LeftoverSizeCommand's output into a path→bytes map.
func ParseLeftoverSizes(out string) (map[string]int64, error) {
	sizes := map[string]int64{}
	started := false
	for _, ln := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
		if !started {
			if strings.TrimSpace(ln) == markerScan {
				started = true
			}
			continue
		}
		f := strings.SplitN(strings.TrimRight(ln, "\r"), "\t", 2)
		if len(f) != 2 {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(f[0]), 10, 64)
		if err != nil {
			continue
		}
		sizes[path.Clean(f[1])] = n
	}
	if !started {
		return nil, fmt.Errorf("measuring displaced sets produced no output at all (no %s marker): the command did not run", markerScan)
	}
	return sizes, nil
}

// LeftoverRemoveCommand deletes one displaced set.
//
// l must be something AttributeLeftovers produced, and is re-checked here
// rather than trusted, exactly as RemoveStagingCommand re-checks a staging name:
// this value is about to be interpolated into rm -rf, and "the caller filtered
// it" is an argument rather than a guarantee. The re-check is against the
// NAMING, not against the caller's say-so, so a path that reached here through
// any other route than a scan is refused too.
func LeftoverRemoveCommand(l Leftover) (string, error) {
	if err := checkLeftoverPath(l.Path); err != nil {
		return "", err
	}
	var sep string
	switch l.Kind {
	case LeftoverReplacedDisk:
		sep = ReplicaReplacedSuffix
	case LeftoverRestoreStaging:
		sep = RestoreTempSuffix
	case LeftoverAsideStore:
		if !strings.HasPrefix(path.Base(l.Path), AsidePrefix) {
			return "", fmt.Errorf("refusing to remove %q as a moved-aside restore point store: its name does not start with %q", l.Path, AsidePrefix)
		}
		sep = "-"
	default:
		return "", fmt.Errorf("refusing to remove %q: %q is not a kind of displaced set vmsync makes", l.Path, l.Kind)
	}
	if _, ok := stampAfter(l.Path, sep); !ok {
		return "", fmt.Errorf("refusing to remove %q: a %s vmsync made ends in %q followed by a unix timestamp, and this does not, so it is not something this sweep created", l.Path, l.Kind, sep)
	}
	return "rm -rf -- " + shQuote(l.Path), nil
}

// checkLeftoverPath refuses anything that must not reach a shell command.
func checkLeftoverPath(p string) error {
	switch {
	case p == "":
		return fmt.Errorf("refusing to act on an empty path")
	case !path.IsAbs(p):
		return fmt.Errorf("refusing to act on %q: it is not an absolute path, so it names a different file depending on where the login lands", p)
	case p != path.Clean(p):
		return fmt.Errorf("refusing to act on %q: it is not a clean path", p)
	case strings.ContainsAny(p, "\n\r\t"):
		return fmt.Errorf("refusing to act on a path containing a newline or a tab: %q", p)
	case path.Base(p) == "." || path.Base(p) == "..":
		return fmt.Errorf("refusing to act on %q: it names a directory rather than a displaced set", p)
	}
	return nil
}
