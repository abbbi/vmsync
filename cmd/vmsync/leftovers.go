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
	"context"
	"path"
	"strings"
	"time"

	"vmsync/pkg/libvirtsync"
	"vmsync/pkg/restorepoint"
	"vmsync/pkg/trace"
)

// The displaced sets a -reinit and an interrupted restore leave behind, found
// and -- when an operator has said how old is old enough -- removed.
//
// Two halves, deliberately unequal. Every sync REPORTS what it found, because
// the files are made by default and nothing has ever named them: a full-size
// copy of every disk per rebuild, sharing extents the day it is made and costing
// a whole replica by the time anyone notices, and one of the three kinds
// invisible to every listing there is. Removal happens only when
// -reclaim-leftovers-after says so, because the aside set is the documented
// recovery for a rebuild that died half way, and losing it by not knowing about
// it is the failure this pair exists to end -- not one to swap for losing it to
// a sweep nobody asked for.

// leftoverSweep is what one run did about displaced sets, for the log and the
// journal.
type leftoverSweep struct {
	// Found is everything attributed to this domain.
	Found []restorepoint.Leftover
	// FoundBytes is what all of Found occupies, allocated.
	FoundBytes int64
	// Removed is what the sweep actually deleted, empty when reclaiming is off.
	Removed []restorepoint.Leftover
	// RemovedBytes is what that freed.
	RemovedBytes int64
	// Refused says why nothing was removed although reclaiming was on, and is
	// empty when that did not happen.
	Refused string
}

// leftoversToReclaim decides which displaced sets this run may delete.
//
// Pure, and separate from every side effect, because this is the decision that
// deletes a full-size copy of production data: it is the one part of this file
// that has to be provable without a target host. Both halves of the decision are
// here rather than one here and one at the call site, so that "what is allowed to
// be deleted" is answered in exactly one function and tested in one place.
//
// after <= 0 means reclaiming is off and nothing is a candidate. A set whose age
// cannot be established from its name is judged on its mtime, which
// AttributeLeftovers has already substituted -- so there is no third outcome
// here, and in particular nothing is ever removed for having an unreadable age.
//
// replicaIncomplete is the target's own marker, and any non-empty value refuses
// the whole sweep. Non-empty rather than parsed, deliberately: the field's
// PRESENCE is the finding, exactly as pkg/failover's refusal treats it, so a
// value this build cannot read still stops the sweep. What it means is that a
// full copy did not finish, and the aside set beside these disks is that
// finding's documented recovery -- the only complete replica there is. Deleting
// it would leave a half-written image whose metadata still looks healthy, which
// is precisely what the marker exists to prevent.
func leftoversToReclaim(found []restorepoint.Leftover, after time.Duration, now time.Time, replicaIncomplete string) (reclaim, keep []restorepoint.Leftover, refused string) {
	if after <= 0 {
		return nil, found, ""
	}
	cutoff := now.Add(-after).Unix()
	for _, l := range found {
		// <= cutoff, and AtUnix == 0 falls on the keep side: a zero stamp means
		// neither the name nor the filesystem could say when this was displaced,
		// and "no idea how old it is" must not read as "older than anything".
		if l.AtUnix > 0 && l.AtUnix <= cutoff {
			reclaim = append(reclaim, l)
			continue
		}
		keep = append(keep, l)
	}
	if strings.TrimSpace(replicaIncomplete) != "" {
		// Everything moves to keep, so a caller that ignored refused still
		// deletes nothing.
		return nil, found, "the replica is marked incomplete"
	}
	return reclaim, keep, ""
}

// sweepDisplacedSets finds this target's displaced sets, reports them, and
// removes the ones older than cfg.ReclaimLeftoversAfter.
//
// Called inside the target-side run lock and BEFORE any disk is copied. Before
// on purpose: the harm this addresses is an ENOSPC on the DR host, and space
// freed after the copy that ran out of it is space freed too late.
//
// Never fails the run. A scan that cannot read a directory, a du that races a
// file being removed, an rm that hits a read-only mount -- none of those is a
// reason to throw away a sync, and all of them are reasons to say something.
func sweepDisplacedSets(ctx context.Context, cfg syncConfig, runner remoteRunner, tgtMgr *libvirtsync.Manager, targetDiskPaths []string, now time.Time) leftoverSweep {
	var sweep leftoverSweep
	if len(targetDiskPaths) == 0 {
		return sweep
	}
	dirs := make([]string, 0, len(targetDiskPaths))
	seen := map[string]bool{}
	for _, p := range targetDiskPaths {
		d := path.Dir(p)
		if !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	// The aside stores sit one level up from the store root, beside the
	// per-domain stores rather than inside them, which is why no listing has
	// ever shown them. See restorepoint.RenameStoreCommand.
	storesParent := ""
	if store, err := restorepoint.StoreFor(targetDiskPaths[0], cfg.TargetDomain); err == nil {
		if root, err := store.Path(); err == nil {
			storesParent = path.Dir(root)
		}
	}

	scanCmd, err := restorepoint.LeftoverScanCommand(dirs, storesParent)
	if err != nil {
		trace.Warning("could not build the scan for displaced sets; this run will not report them", "error", err)
		return sweep
	}
	out, err := runner.Run(ctx, scanCmd)
	if err != nil {
		trace.Warning("could not look for displaced sets beside the replica; they will go unreported until this succeeds", "error", err)
		return sweep
	}
	entries, err := restorepoint.ParseLeftoverScan(out)
	if err != nil {
		trace.Warning("could not read the scan for displaced sets", "error", err)
		return sweep
	}
	found, unparsed := restorepoint.AttributeLeftovers(entries, targetDiskPaths, cfg.TargetDomain)
	for _, ln := range unparsed {
		trace.Warning("an entry beside the replica could not be read as a path (it contains a tab or a newline); vmsync will not act on it", "entry", ln)
	}
	if len(found) == 0 {
		return sweep
	}

	// Sizes in one call, for everything found rather than only for what is
	// about to be removed: the number an operator needs in order to DECIDE on a
	// duration is what the sets they are keeping cost.
	paths := make([]string, 0, len(found))
	for _, l := range found {
		paths = append(paths, l.Path)
	}
	if cmd, err := restorepoint.LeftoverSizeCommand(paths); err == nil {
		if out, err := runner.Run(ctx, cmd); err == nil {
			if sizes, err := restorepoint.ParseLeftoverSizes(out); err == nil {
				for i := range found {
					found[i].Bytes = sizes[found[i].Path]
				}
			}
		}
	}
	for _, l := range found {
		sweep.FoundBytes += l.Bytes
	}
	sweep.Found = found

	after := cfg.ReclaimLeftoversAfterDur

	// One line an operator can act on, whether or not reclaiming is on.
	oldest := found[len(found)-1]
	if after <= 0 {
		trace.Info("displaced sets beside the replica, which nothing removes on its own",
			"sets", len(found), "bytes", sweep.FoundBytes,
			"oldest", time.Unix(oldest.AtUnix, 0).UTC().Format(time.RFC3339), "oldest_path", oldest.Path,
			"note", "these are the disks a -reinit renamed aside, restore copies an interrupted restore staged, and restore-point stores a -reinit moved aside. Set -reclaim-leftovers-after to a duration (e.g. 720h) to have runs remove the ones older than it, or use -replaced-disk-action=delete to stop making the disk asides at all")
		return sweep
	}

	// The interlock's input, read here rather than taken from a value the run
	// computed earlier: it is the reason this may delete nothing, so it has to
	// be the state as of the moment of deciding.
	//
	// Fails CLOSED. An unreadable marker might be a set marker, and the cost of
	// being wrong is asymmetric to the point of not being a judgement call: keep
	// the asides and the worst case is that a disk stays full one more day;
	// delete them while the marker was set and the only complete copy of the
	// replica is gone.
	fields, err := libvirtsync.ReadDomainMetadataFields(tgtMgr, cfg.TargetDomain, libvirtsync.MetadataFieldReplicaIncomplete)
	if err != nil {
		sweep.Refused = "could not read the target's replica_incomplete marker"
		trace.Warning("not reclaiming any displaced set: the target's replica_incomplete marker could not be read, and while it might be set these asides are the only complete replica there is",
			"sets", len(found), "error", err)
		return sweep
	}
	raw := fields[libvirtsync.MetadataFieldReplicaIncomplete]

	reclaim, keep, refused := leftoversToReclaim(found, after, now, raw)
	if refused != "" {
		sweep.Refused = refused
		trace.Warning("REFUSING to reclaim displaced sets: this replica is marked incomplete, so a full copy did not finish and these aside files are the complete replica it replaced -- putting them back over the disks is the recovery. Clear the finding (re-run the sync, which repairs it) and the next run will reclaim them",
			"sets", len(found), "bytes", sweep.FoundBytes, "replica_incomplete", raw)
		return sweep
	}

	for _, l := range reclaim {
		cmd, err := restorepoint.LeftoverRemoveCommand(l)
		if err != nil {
			// Refused by the naming re-check, which is the safety net working:
			// something reached the removal list that this package will not
			// point rm -rf at.
			trace.Warning("leaving a displaced set in place", "path", l.Path, "kind", l.Kind, "error", err)
			continue
		}
		if out, err := runner.Run(ctx, cmd); err != nil {
			trace.Warning("could not remove a displaced set; it will be retried on the next run", "path", l.Path, "kind", l.Kind, "error", err, "output", out)
			continue
		}
		sweep.Removed = append(sweep.Removed, l)
		sweep.RemovedBytes += l.Bytes
		trace.Info("reclaimed a displaced set", "path", l.Path, "kind", l.Kind, "bytes", l.Bytes,
			"displaced", time.Unix(l.AtUnix, 0).UTC().Format(time.RFC3339), "stamp_from_name", l.StampFromName)
	}
	trace.Info("displaced sets after reclaiming", "removed", len(sweep.Removed), "freed", sweep.RemovedBytes,
		"kept", len(keep), "kept_bytes", sweep.FoundBytes-sweep.RemovedBytes, "older_than", after.String())
	return sweep
}

// bytesOf sums what a set of leftovers occupies.
func bytesOf(ls []restorepoint.Leftover) int64 {
	var n int64
	for _, l := range ls {
		n += l.Bytes
	}
	return n
}
