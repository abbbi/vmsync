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

package inventory

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"vmsync/pkg/failover"
	"vmsync/pkg/restorepoint"
)

// What a replica can be rolled back TO, reported so the control plane can
// offer it.
//
// This exists because a restore is the one operation whose parameter cannot be
// derived from anything the UI already knows: every other operation names a VM
// and a role or a peer, all of which come from metadata already reported. A
// restore names a TAG, and a tag is a directory on the target's filesystem --
// so without this the UI can only ask for a restore point it has never seen.
//
// Read straight off the local filesystem rather than over SSH, for the same
// reason the disk sizes beside it are: the agent runs on the host holding these
// files. That also makes it honest about cost -- one ReadDir per replicated
// domain per report, plus a small ReadFile per restore point.

// RestorePointInfo is one restore point as it sits on this host's storage.
//
// Verify is deliberately reported even when it is "not-run", which is the
// ordinary state rather than a fault: restore points are taken before -verify
// runs, and an operator choosing between them during an incident needs to know
// which of them was ever actually checked. Reporting nothing would make "never
// checked" and "checked and clean" indistinguishable.
type RestorePointInfo struct {
	// Tag is the directory name, and the identifier an operation names.
	Tag string `json:"tag"`
	// TakenAtUnix is when the copy was made.
	TakenAtUnix int64 `json:"taken_at_unix"`
	// CheckpointAtUnix is the instant the CONTENTS correspond to, which is
	// earlier than TakenAt and is what a promotion would measure data loss
	// from. Zero on a sidecar written before the field existed.
	CheckpointAtUnix int64 `json:"checkpoint_at_unix,omitempty"`
	// Checkpoint is the sync checkpoint these contents belong to.
	Checkpoint string `json:"checkpoint,omitempty"`
	// Source is the "host:domain" this was replicating from when taken.
	Source string `json:"source,omitempty"`
	// Verify is "not-run", "passed" or "failed".
	Verify string `json:"verify,omitempty"`
	// Disks are the basenames the point holds, so the UI can say what a
	// restore would replace without a second round trip.
	Disks []string `json:"disks,omitempty"`
	// Incomplete marks a sidecar that could not be read. The point is listed
	// anyway: the directory is the inventory, and hiding an entry that exists
	// on disk would make the UI disagree with -list-restore-points.
	Incomplete bool `json:"incomplete,omitempty"`
}

// RestorePointsFor lists what a domain can be rolled back to.
//
// The store is derived the same way the sync path derives it --
// restorepoint.StoreFor of an actual disk path, plus this domain's name -- and
// NOT from a configured target_disk_path. Those agree only when the configured
// value names the directory the disks are really in, so deriving is what makes
// this agree with what a sync actually wrote.
//
// A domain whose disks span several directories has no single restore point
// set (retention refuses such a domain outright, because a restore point is a
// SET and one disk from this sync beside another from a different one is not a
// recoverable machine), so nothing is reported rather than a partial answer
// from whichever directory happened to be first.
//
// Every failure is silent by design. This runs on every report for every
// domain, and a host where the directory does not exist -- which is every host
// not using -retention -- is the ordinary case, not an error worth a line in
// the log on every cycle.
func RestorePointsFor(d Domain) []RestorePointInfo {
	store, ok := restorePointStoreFor(d)
	if !ok {
		return nil
	}
	root, err := store.Path()
	if err != nil {
		return nil
	}

	out := readRestorePointDir(root)

	// Newest first: an operator reaching for one of these is usually asking
	// "what is the most recent copy from before the damage", and reads down
	// until they reach it.
	sort.Slice(out, func(i, j int) bool { return out[i].TakenAtUnix > out[j].TakenAtUnix })
	if len(out) == 0 {
		return nil
	}
	return out
}

// readRestorePointDir reads one store's restore points.
//
// Split out of RestorePointsFor so the predicate that decides what counts as a
// restore point is stated once, and stated the same way -list-restore-points
// states it -- the two views cannot then disagree about what is there.
func readRestorePointDir(dir string) []RestorePointInfo {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]RestorePointInfo, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// ParseTag is what separates a restore point from staging left by an
		// interrupted run (".incomplete-"), from a per-domain store ("vm-"),
		// from a set an earlier reinit moved aside (".replaced-") and from
		// anything else that found its way in. Same predicate
		// -list-restore-points uses, so the two views cannot disagree about
		// what counts.
		tag, err := restorepoint.ParseTag(e.Name())
		if err != nil {
			continue
		}
		info := RestorePointInfo{Tag: tag.String(), TakenAtUnix: tag.At.Unix(), Checkpoint: tag.Checkpoint}

		b, err := os.ReadFile(filepath.Join(dir, e.Name(), restorepoint.StatusName))
		if err != nil {
			info.Incomplete = true
			out = append(out, info)
			continue
		}
		st, err := restorepoint.DecodeStatus(b)
		if err != nil {
			info.Incomplete = true
			out = append(out, info)
			continue
		}
		info.CheckpointAtUnix = st.CheckpointAt
		info.Source = st.Source
		info.Verify = st.Verify
		info.Disks = st.Disks
		if st.Checkpoint != "" {
			info.Checkpoint = st.Checkpoint
		}
		out = append(out, info)
	}
	return out
}

// restorePointStoreFor finds the one store a domain's restore points would be
// in, or reports that there is no single one.
//
// Keyed by this domain's NAME as well as by the directory, which is the fix for
// the defect this reader shared with the sync path: reading the directory alone
// reported every co-located domain's points as this one's, so the control plane
// offered another pair's copies as a rollback target for this machine.
func restorePointStoreFor(d Domain) (restorepoint.Store, bool) {
	var dir string
	for _, disk := range d.Disks {
		// A missing disk file still names a directory, and that directory is
		// still where its restore points would be -- which is exactly the
		// case an operator most wants listed.
		this := filepath.Dir(disk.Path)
		if dir == "" {
			dir = this
			continue
		}
		if this != dir {
			return restorepoint.Store{}, false
		}
	}
	if dir == "" {
		return restorepoint.Store{}, false
	}
	// filepath.ToSlash so a Store built here is spelled the same way the sync
	// path spells it. The agent runs on Linux, where the two already agree, but
	// the store's own validation insists on a clean slash path and a test run on
	// any other OS would otherwise fail for a reason that says nothing.
	store, err := restorepoint.StoreForDir(filepath.ToSlash(dir), d.Name)
	if err != nil {
		return restorepoint.Store{}, false
	}
	return store, true
}

// LeftoverInfo is one set of files a rebuild or a restore displaced and nobody
// has removed: a full-size copy of a replica's disk, or a whole restore-point
// store moved out of the way.
//
// Reported because the alternative is what CI-54 describes: they are created by
// the default -replaced-disk-action, nothing reaps them, and until now nothing
// named them either. They share extents with the live file at the moment they
// are made -- so `du` shows almost nothing and an operator reasonably concludes
// the space is free -- and then diverge as the replica is written, until a
// reinit that renames a fresh set aside on a host that has run out of room
// fails a commit for every VM on it.
//
// They are deliberately NOT reaped automatically. These are the recovery copy:
// the whole documented cure for an interrupted rebuild is to move a
// .vmsync-replaced-<stamp> set back over the half-written disks
// (MetadataFieldReplicaIncomplete says so at length), and a sweep that deleted
// them would delete exactly the thing an operator in trouble is reaching for.
// What was missing is not a reaper, it is the number.
type LeftoverInfo struct {
	// Path is the file or directory, as this host spells it.
	Path string `json:"path"`
	// Kind says which mechanism produced it, because the recovery differs:
	// "replaced-disk" is a displaced replica disk (move it back), "aside-store"
	// is a whole restore-point store a reinit set aside (its points are outside
	// every store, so no command can name them), "restore-staging" is a copy a
	// restore staged and did not consume.
	Kind string `json:"kind"`
	// Bytes is what it actually occupies now, not its apparent size: the shared
	// extents are the reason a fresh aside looks free, and the divergence is the
	// thing worth watching.
	Bytes int64 `json:"bytes"`
	// AtUnix is the stamp in the name, 0 when it carries none. Age is what tells
	// an operator whether this is yesterday's rebuild or one from six months ago.
	AtUnix int64 `json:"at_unix,omitempty"`
}

// LeftoversFor finds every displaced set beside a domain's disks.
//
// Silent on every failure, for the reason RestorePointsFor is: this runs for
// every domain on every report cycle, and a host with nothing to find is the
// ordinary case rather than an error.
//
// Scoped to the directories the domain's own disks live in, plus its restore
// point root. That is deliberately narrow -- it will not find leftovers beside
// disks the domain no longer references, which is exactly what an interrupted
// -force-clean can leave (it undefines the domain before rewriting the disks, so
// there may be no domain to scan from at all). -explain-domain covers that case
// with -target-disk-path; this covers the one the fleet can see by itself.
// Everything reported is attributed to THIS domain, which on a shared
// directory means matching names against this domain's own disks and store
// rather than reading the directory as if the domain owned it. That is the
// defect CI-06 recorded at this end: two replicas in one directory saw each
// other's restore points, and a scan written the obvious way makes the same
// mistake with displaced sets -- every co-located replica's aside counted in
// every co-located replica's bytes, an operator told web01 is holding 200 GiB
// that belongs to db01, and a path pointing at another machine's only good
// copy offered as this machine's reclaimable space.
func LeftoversFor(d Domain) []LeftoverInfo {
	// Per directory, the basenames of the disks THIS domain has there. A
	// displaced set is named after the disk it displaced, so the basename is
	// what ties it to a domain; nothing else in the name does.
	dirs := map[string]map[string]bool{}
	for _, disk := range d.Disks {
		dir := filepath.Dir(disk.Path)
		if dirs[dir] == nil {
			dirs[dir] = map[string]bool{}
		}
		dirs[dir][filepath.Base(disk.Path)] = true
	}
	var out []LeftoverInfo
	for dir, bases := range dirs {
		out = append(out, scanLeftoverDir(dir, bases)...)
	}
	// The aside restore-point stores sit one level up from the store root, beside
	// the per-domain stores rather than inside them -- which is why no listing has
	// ever shown them. See restorepoint.RenameStoreCommand.
	if store, ok := restorePointStoreFor(d); ok {
		if root, err := store.Path(); err == nil {
			out = append(out, scanAsideStores(filepath.Dir(root), d.Name)...)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].AtUnix != out[j].AtUnix {
			return out[i].AtUnix > out[j].AtUnix
		}
		return out[i].Path < out[j].Path
	})
	return out
}

// scanLeftoverDir finds displaced disk files and restore staging copies in one
// directory, for the disks named in bases and no others.
//
// bases is what keeps a shared directory's answer per-domain: both namings are
// "<the disk's own name><suffix><stamp>", so the part before the suffix has to
// be one of this domain's disks. A name that merely CONTAINS the suffix belongs
// to whichever domain owns that disk, and on a directory holding ten replicas
// that is nine times out of ten not this one.
func scanLeftoverDir(dir string, bases map[string]bool) []LeftoverInfo {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []LeftoverInfo
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		kind := ""
		var at int64
		switch {
		case displacedFrom(name, failover.ReplicaReplacedSuffix, bases):
			kind = "replaced-disk"
			at = trailingUnix(name, failover.ReplicaReplacedSuffix)
		case displacedFrom(name, restorepoint.RestoreTempSuffix, bases):
			kind = "restore-staging"
		default:
			continue
		}
		// The allocated size, not the apparent one: a fresh reflink aside shares
		// every extent with the live file, so its apparent size is a full disk
		// image and its real cost is nothing. Reporting the apparent size would
		// cry wolf on the day it is made and understate nothing afterwards.
		out = append(out, LeftoverInfo{Path: filepath.Join(dir, name), Kind: kind, Bytes: allocatedBytes(filepath.Join(dir, name)), AtUnix: at})
	}
	return out
}

// scanAsideStores finds the restore-point stores a reinit moved out of the way
// for THIS domain.
//
// These are the ones RST-021 calls invisible to every listing, and it is right:
// they are named `.replaced-<segment>-<unix>` and they sit BESIDE the stores, so
// -list-restore-points -- which reads inside one store -- cannot see them, and
// the points inside them are outside every store, so no command can name them.
// Only the directory's size says they are there at all.
//
// The stores parent holds every co-located domain's stores and every co-located
// domain's asides, so "starts with AsidePrefix" is not this domain's answer --
// it is the whole directory's. The name is matched in full instead: this
// domain's own segment, then a dash, then the stamp and nothing else. Exact
// rather than a prefix test, because a prefix test also matches the asides of
// every domain whose name merely STARTS with this one's -- web01 would be
// charged for web01-old and web01-staging. The price is that an aside somebody
// renamed by hand goes unreported, which is the right way round: a set this
// scan misses still shows in df, while one it misattributes sends an operator
// to delete another machine's restore points.
func scanAsideStores(storesParent, domain string) []LeftoverInfo {
	segment, err := restorepoint.DomainSegment(domain)
	if err != nil {
		return nil
	}
	prefix := restorepoint.AsidePrefix + segment + "-"
	entries, err := os.ReadDir(storesParent)
	if err != nil {
		return nil
	}
	var out []LeftoverInfo
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || !strings.HasPrefix(name, prefix) {
			continue
		}
		stamp, err := strconv.ParseInt(name[len(prefix):], 10, 64)
		if err != nil {
			continue
		}
		p := filepath.Join(storesParent, name)
		out = append(out, LeftoverInfo{
			Path:   p,
			Kind:   "aside-store",
			Bytes:  dirAllocatedBytes(p),
			AtUnix: stamp,
		})
	}
	return out
}

// displacedFrom reports whether name is one of bases displaced with sep.
//
// Anchored on the base: the suffix has to come immediately after a disk this
// domain owns, so a longer name that happens to contain both is not a match.
func displacedFrom(name, sep string, bases map[string]bool) bool {
	i := strings.Index(name, sep)
	if i <= 0 {
		return false
	}
	return bases[name[:i]]
}

// trailingUnix reads the unix stamp a displaced name ends with, after the last
// occurrence of sep. 0 when there is none to read -- an older build's naming, or
// a name somebody changed by hand.
func trailingUnix(name, sep string) int64 {
	i := strings.LastIndex(name, sep)
	if i < 0 {
		return 0
	}
	n, err := strconv.ParseInt(name[i+len(sep):], 10, 64)
	if err != nil {
		return 0
	}
	return n
}
