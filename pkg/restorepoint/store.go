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
	"strings"
)

// Addressing. Everything in this package that names a directory on the target
// goes through the types in this file, and the reason is the defect they make
// unreachable.
//
// Keying restore points by the DIRECTORY the replica's disks live in -- a
// Root(diskPath) returning <dir>/.vmsync-rp, with every decision (is another
// point due, which ones are surplus, which staging directories are junk, what
// does a -reinit discard) taken over everything in it -- gives two domains
// replicated into one -target-disk-path a single shared store, which is what a
// template-level target_disk_path in the shipped agent example hands every VM
// that does not override it. The consequences are not subtle: one domain's
// point satisfies the other's interval floor, so a domain can go for days
// taking none; one domain's prune evicts the other's history to stay inside a
// count meant for one machine; a prune sweeps a concurrently running sibling's
// in-flight staging directory, failing a sync that has already copied its
// data; and one domain's -reinit deletes or orphans every co-located domain's
// entire history in a single rm -rf.
//
// What makes all four reachable is `root string` as the currency. A directory
// is a legal value for it, so every call site is one forgotten argument away
// from addressing the shared store, and nothing can tell the difference. The
// currency is therefore a Store: a struct with unexported fields that only the
// constructors here can produce, and which cannot be built without naming a
// domain. There is no expression outside this package that addresses the
// directory two domains share at all.
//
// Nothing here knows about the pre-change layout, where points sat directly in
// DirName. There is no migration and no compatibility path: a flat point is
// outside every store, so no command in this package can name it, which means
// none can read, prune or delete one either. Copies from before are left exactly
// where they are, and an operator who wants them gone removes them by hand.

// DomainPrefix marks a per-domain store inside DirName.
//
// It exists so a domain directory can never be read as a tag. ParseTag
// requires <unix int>-<checkpoint> and "vm" is not an integer, so
// ParseTag("vm-web01") always fails -- while a domain legally named
// "1756041600-web01" would otherwise be indistinguishable from a point left
// flat in the shared root by a pre-change run.
//
// It is NOT a path-safety mechanism. ".." survives SafeKey unchanged, and is
// refused by DomainSegment by name, so that this prefix keeps doing exactly one
// job and a later change to it cannot quietly become a traversal.
const DomainPrefix = "vm-"

// AsidePrefix marks a store that -reinit moved out of the way instead of
// deleting.
//
// A leading dot, and therefore something DomainSegment can never produce, so
// "starts with DomainPrefix" means exactly "a live store" and an aside can
// never be mistaken for one -- nor a store for an aside, which is the direction
// that would matter: it would put a rename's destination inside the set of
// things a later reinit believes it owns.
const AsidePrefix = ".replaced-"

// MaxSegmentBytes bounds one path component: NAME_MAX on ext4, XFS and btrfs.
//
// Checked because SafeKey EXPANDS -- "/" becomes three bytes and "%" two -- so
// a domain name an operator reads as comfortably short can encode past the
// limit. Without this the first symptom is an opaque mkdir failure on a
// production target in the middle of a sync, instead of a refusal naming the
// domain before anything is written.
const MaxSegmentBytes = 255

// safeKeyReplacer is util.SafeKey's encoding, duplicated rather than imported.
//
// pkg/util pulls in syscall.Flock, which would make this package build only on
// Linux and take its tests down with it -- being testable anywhere is the
// entire reason this package exists. This package already duplicates
// util.ShQuote for the same reason and says so at shQuote; pkg/actionlog
// duplicates this same encoding, for the same reason, in the same words.
//
// The duplication is only safe if it behaves identically, so TestSafeKey pins
// it against the same cases pkg/util's own test uses. If util.SafeKey ever
// changed, a domain's restore points would move to a different directory and
// its whole history would read as empty -- which is why the two are pinned
// rather than merely kept in step.
var safeKeyReplacer = strings.NewReplacer("%", "%%", "/", "%2f", " ", "%20")

// SafeKey makes a domain name safe as ONE filename component, reversibly.
//
// Reversible percent-style encoding rather than lossy substitution, because
// mapping both "/" and " " to "_" would make "web server" and "web/server"
// share one store -- two machines' restore points in one directory, which is
// the exact defect this layout exists to fix, rebuilt one level down.
//
// What this does NOT do is make a name safe for a SHELL. A domain may legally
// contain a quote, a newline or a "$(", and this leaves all three alone; every
// path built from it is shell-quoted where it enters a command (see shQuote in
// remote.go). Confusing the two jobs is how a domain name becomes a command on
// a production target.
func SafeKey(key string) string { return safeKeyReplacer.Replace(key) }

// DomainSegment is the one path component that holds a target domain's restore
// points.
//
// Encoding is not enough on its own, so this refuses as well as encodes. The
// cases below are the ones SafeKey deliberately leaves alone: it is a
// filename encoder, not a path validator, and "." and ".." are perfectly good
// filenames that happen to mean something else to a filesystem. A domain named
// ".." would encode to ".." and address the replica's own directory -- which is
// the argument a -reinit hands to rm -rf.
func DomainSegment(domain string) (string, error) {
	if domain == "" {
		return "", fmt.Errorf("refusing to locate restore points without a target domain name: they are kept per domain, so there is no directory to name (-target-domain)")
	}
	switch domain {
	case ".", "..":
		return "", fmt.Errorf("refusing %q as a target domain name for restore points: it names a directory rather than a machine, and every path built from it would leave the restore point store", domain)
	}
	if strings.ContainsRune(domain, 0) {
		return "", fmt.Errorf("refusing a target domain name containing a NUL byte")
	}
	seg := DomainPrefix + SafeKey(domain)
	// Nothing below should be reachable, because SafeKey encodes "/" and the
	// cases above cover the rest. Checked anyway: this value becomes a path
	// component in a command that can be rm -rf, and "the encoder handles it"
	// is an argument, not a guarantee.
	if strings.ContainsAny(seg, "/\\") {
		return "", fmt.Errorf("refusing target domain name %q: it does not encode to a single path component", domain)
	}
	if len(seg) > MaxSegmentBytes {
		return "", fmt.Errorf("refusing target domain name %q: its restore point directory name is %d bytes once encoded, over the %d-byte filesystem limit -- shorten the domain name or give this pair its own -target-disk-path",
			domain, len(seg), MaxSegmentBytes)
	}
	return seg, nil
}

// Store is the restore point directory of ONE target domain.
//
// Unexported fields on purpose: a Store can only come from StoreFor or
// StoreForDir, both of which insist on a domain, so no caller can produce the
// value that addresses the directory two domains share. That is the whole
// mechanism -- see the note at the top of this file for the defect it replaces.
type Store struct {
	// replicaDir is the directory the replica's disks live in: absolute and
	// already clean, checked by the constructors.
	replicaDir string
	// domain is the target domain name as libvirt knows it, unencoded, kept
	// for messages and for recomputing segment as a guard.
	domain string
	// segment is DomainSegment(domain): the one path component below DirName.
	segment string
}

// StoreFor is the store holding the restore points of one replica disk's
// domain.
//
// Takes a disk PATH, which is how the sync path and the restore path both
// derive it -- from a disk the target domain actually refers to, rather than
// from a configured -target-disk-path that may name a different directory. See
// restoreRootFor in cmd/vmsync for why deriving is the more correct answer and
// not merely the convenient one.
func StoreFor(diskPath, targetDomain string) (Store, error) {
	if diskPath == "" {
		return Store{}, fmt.Errorf("refusing to locate restore points from an empty disk path")
	}
	return StoreForDir(path.Dir(diskPath), targetDomain)
}

// StoreForDir is the store holding one target domain's restore points, given
// the directory its disks live in.
//
// Separate from StoreFor because the difference is one path.Dir and getting it
// wrong is silent: StoreFor("/data/replicas", d) would answer
// /data/.vmsync-rp/vm-d -- one directory too high, beside whatever else lives
// there, and it would work. The two read-only verbs hold a DIRECTORY
// (-target-disk-path) rather than a disk, and before this constructor existed
// they hand-joined DirName themselves, which is how the layout came to be
// spelled out in three places.
func StoreForDir(replicaDir, targetDomain string) (Store, error) {
	seg, err := DomainSegment(targetDomain)
	if err != nil {
		return Store{}, err
	}
	if !path.IsAbs(replicaDir) {
		return Store{}, fmt.Errorf("refusing %q as the directory holding %s's replica disks: restore point commands run on the target with an unknown working directory, so the path has to be absolute",
			replicaDir, targetDomain)
	}
	if path.Clean(replicaDir) != replicaDir {
		return Store{}, fmt.Errorf("refusing %q as the directory holding %s's replica disks: it is not in canonical form (%q), and a path with a %q or a trailing slash in it is not one this package will build an rm -rf from",
			replicaDir, targetDomain, path.Clean(replicaDir), "..")
	}
	if replicaDir == "/" {
		return Store{}, fmt.Errorf("refusing / as the directory holding %s's replica disks", targetDomain)
	}
	// The operator pasted a restore point path where the replica's own
	// directory belongs. Reachable from -target-disk-path, and the result
	// would be a store nested inside a store -- which the next -reinit would
	// then rm -rf, taking the outer one's history with it.
	//
	// The test is whether the path lies INSIDE a DirName, walked to the root.
	// It is deliberately not "the last component starts with DomainPrefix",
	// which is what this did first and which refused /data/vm-images,
	// /srv/vm-disks and every other perfectly ordinary replica directory whose
	// name happens to begin with "vm-" -- permanently, on every run, for a
	// -target-disk-path that worked before. A component named DirName is the
	// only thing that actually means "somebody handed us a restore point path".
	for p := replicaDir; ; p = path.Dir(p) {
		if path.Base(p) == DirName {
			return Store{}, fmt.Errorf("refusing %q as the directory holding %s's replica disks: it is inside a %s, so this is a restore point path rather than a replica directory. Name the directory the replica's own disks are in; %s is appended to it",
				replicaDir, targetDomain, DirName, DirName)
		}
		if parent := path.Dir(p); parent == p {
			break
		}
	}
	return Store{replicaDir: replicaDir, domain: targetDomain, segment: seg}, nil
}

// Domain is the target domain this store belongs to, unencoded.
func (s Store) Domain() string { return s.domain }

// ReplicaDir is the directory the replica's disks live in.
func (s Store) ReplicaDir() string { return s.replicaDir }

// Path is this domain's restore point directory.
//
// Returns an error rather than a string so that a zero Store -- the only
// invalid one a caller can hold, since the fields are unexported -- cannot
// become a relative path in a command. path.Join("", DirName, DomainPrefix)
// yields ".vmsync-rp/vm-", which would resolve against whatever directory the
// SSH login lands in.
func (s Store) Path() (string, error) {
	if err := s.check(); err != nil {
		return "", err
	}
	return path.Join(s.replicaDir, DirName, s.segment), nil
}

// String is for logs and error text, and never for a command -- which is why
// it cannot fail where Path can. A caller that has a path to print has one
// whether or not the Store is usable; a caller building a command must deal
// with the error.
func (s Store) String() string {
	if s.replicaDir == "" || s.segment == "" {
		return "<unset restore point store>"
	}
	return path.Join(s.replicaDir, DirName, s.segment)
}

// check is the confinement guard, and it is what every destructive command in
// this package is gated on.
//
// It replaces checkRoot, which asked only whether a path's last component was
// named DirName. That was enough while the root WAS DirName; it is not enough
// now, and a guard that merely looked plausible would be worse than none,
// because the paths it protects reach rm -rf and mv.
//
// Each condition closes a way in:
//
//   - a zero or forged Store (empty fields) would build a relative path
//   - segment must recompute from domain, so a Store whose fields disagree --
//     the only way to get one is to construct it inside this package and then
//     change a field -- cannot address a directory the domain does not name
//   - the parent must be DirName, so the path is one vmsync built
//   - the leaf must be the segment, so it is this domain's and not a sibling's
//
// Symlinks need no condition of their own: rm -rf and mv act on the link they
// are given, not on what it points at, so a symlinked replicaDir, DirName or
// segment costs the link and nothing beyond it.
func (s Store) check() error {
	if s.replicaDir == "" || s.domain == "" || s.segment == "" {
		return fmt.Errorf("refusing to build a restore point command from an unset store: it has no domain, so there is no directory it could mean")
	}
	seg, err := DomainSegment(s.domain)
	if err != nil {
		return err
	}
	if seg != s.segment {
		return fmt.Errorf("refusing a restore point store whose directory %q is not the one target domain %q encodes to (%q)", s.segment, s.domain, seg)
	}
	full := path.Join(s.replicaDir, DirName, s.segment)
	if path.Base(full) != s.segment || path.Base(path.Dir(full)) != DirName {
		return fmt.Errorf("refusing to act on %q as target domain %q's restore point directory: it is not a %q inside a %q", full, s.domain, DomainPrefix+"…", DirName)
	}
	return nil
}

// Point addresses one finished restore point in one domain's store.
//
// A value rather than a path, for the same reason Store is: RemoveCommand emits
// rm -rf, and the only way to reach it is through a Tag that has already been
// validated inside a Store that has already been checked. There is no signature
// in this package that deletes an arbitrary directory.
type Point struct {
	store Store
	tag   Tag
}

// Point is this store's restore point for a tag.
func (s Store) Point(t Tag) Point { return Point{store: s, tag: t} }

// Tag is which restore point this is.
func (p Point) Tag() Tag { return p.tag }

// Store is which domain's store it lives in.
func (p Point) Store() Store { return p.store }

// dir is where a finished restore point lives.
func (p Point) dir() (string, error) {
	root, err := p.store.Path()
	if err != nil {
		return "", err
	}
	name, err := p.tagName()
	if err != nil {
		return "", err
	}
	return path.Join(root, name), nil
}

// tagName is the validated directory name for this point's tag.
//
// It REVALIDATES rather than trusting that the tag came from NewTag or
// ParseTag. Tag is an exported struct with exported fields, so any caller can
// write Tag{At: t, Checkpoint: "../../etc"} without going through a
// constructor, and that value reaches rm -rf through RemoveCommand. "The only
// way here is through a validated tag" was an assertion about how callers
// behave, not a property of the code; running the same checkpointRe NewTag uses
// makes it one.
//
// It is NOT a path traversal, and it is worth being exact about that rather
// than alarming: String() always prefixes the instant, so the first path
// component is "<unix>-<whatever>" and can never itself be "..". A forged
// "../../etc" cleans to <store>/etc -- a wrongly named directory INSIDE the
// store, not an escape from it. Refused anyway, because a command built from a
// tag nothing validated is a command nobody can reason about, and the next
// change to String() or to DiskPath would be the one that made it an escape.
func (p Point) tagName() (string, error) {
	if p.tag.At.IsZero() || p.tag.Checkpoint == "" {
		return "", fmt.Errorf("refusing to address a restore point with no tag in %s", p.store)
	}
	valid, err := NewTag(p.tag.At, p.tag.Checkpoint)
	if err != nil {
		return "", fmt.Errorf("refusing to address a restore point in %s: %w", p.store, err)
	}
	// The rendered name, not the caller's, so a tag whose fields survive
	// validation but render differently cannot slip a second spelling of one
	// directory past the checks above.
	return valid.String(), nil
}

// Dir is where a finished restore point lives, for a caller that needs to name
// it -- a message, or a path handed to something outside this package.
func (p Point) Dir() (string, error) { return p.dir() }

// staging is where one is assembled before being renamed into place.
//
// Unexported, and so is every path that reaches it: nothing outside this
// package can name a staging directory, which is what makes it impossible for
// one domain's run to address another's in-flight set. That was the third of
// the four ways the shared store went wrong.
func (p Point) staging() (string, error) {
	root, err := p.store.Path()
	if err != nil {
		return "", err
	}
	name, err := p.tagName()
	if err != nil {
		return "", err
	}
	return path.Join(root, StagingPrefix+name), nil
}
