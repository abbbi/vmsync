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
