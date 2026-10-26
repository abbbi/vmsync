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
	// Unreclaimable is what is old enough to go but will never go on its own,
	// because its age came from a filesystem mtime rather than from a stamp in
	// its name. Separate from both Removed and the kept remainder, because it
	// is the one outcome here that needs a person: nothing will change about
	// these on any future run, and until this field existed nothing said so.
	Unreclaimable []restorepoint.Leftover
	// UnreclaimableBytes is what those occupy, allocated.
	UnreclaimableBytes int64
}

// reclaimDecision is what leftoversToReclaim decided, as named fields rather
// than a positional list.
//
// Three same-typed slices in a return list is one transposition away from
// pointing rm -rf at the sets an operator said were too young to touch, and
// that is not hypothetical: measured on 2026-10-19, swapping the first two
// returns at the call site compiled, sent the keep bucket to the removal loop,
// and left the entire suite green. The call site cannot be covered by a unit
// test for the reason sweepDisplacedSets' own signature used to guarantee, so
// the accident is removed rather than tested for.
type reclaimDecision struct {
	// Reclaim is old enough, carries a stamp proving vmsync made it, and may
	// be deleted.
	Reclaim []restorepoint.Leftover
	// Keep is not old enough yet, or has no readable age at all.
	Keep []restorepoint.Leftover
	// Unstamped is old enough by its mtime but carries no timestamp in its
	// name, so LeftoverRemoveCommand refuses it and no future run will remove
	// it either. The one outcome here that needs a person.
	Unstamped []restorepoint.Leftover
	// Refused says why the whole sweep stood down, and is empty otherwise.
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
// after <= 0 means reclaiming is off and nothing is a candidate. Nothing is ever
// removed for having an unreadable age, and nothing is removed on the strength
// of an mtime either -- see the unstamped case below, which is the third
// outcome this function used to lack.
//
// replicaIncomplete is the target's own marker, and any non-empty value refuses
// the whole sweep. Non-empty rather than parsed, deliberately: the field's
// PRESENCE is the finding, exactly as pkg/failover's refusal treats it, so a
// value this build cannot read still stops the sweep. What it means is that a
// full copy did not finish, and the aside set beside these disks is that
// finding's documented recovery -- the only complete replica there is. Deleting
// it would leave a half-written image whose metadata still looks healthy, which
// is precisely what the marker exists to prevent.
func leftoversToReclaim(found []restorepoint.Leftover, after time.Duration, now time.Time, replicaIncomplete string) reclaimDecision {
	if after <= 0 {
		return reclaimDecision{Keep: found}
	}
	var d reclaimDecision
	cutoff := now.Add(-after).Unix()
	for _, l := range found {
		switch {
		case l.AtUnix <= 0, l.AtUnix > cutoff:
			// A zero stamp means neither the name nor the filesystem could say
			// when this was displaced, and "no idea how old it is" must not
			// read as "older than anything". The boundary is inclusive, so a
			// daily run with a 30-day duration reclaims on day 30.
			//
			// BEFORE the unstamped case below, deliberately: a set that is not
			// old enough yet is simply not old enough, whatever its age came
			// from, and filing it as unreclaimable would tell an operator to
			// go and delete something that this sweep would have removed on
			// its own a fortnight later.
			d.Keep = append(d.Keep, l)

		case !l.StampFromName:
			// OLD ENOUGH BY ITS MTIME, WHICH IS NOT EVIDENCE ENOUGH TO POINT
			// rm -rf AT IT. Selecting these anyway is what made this an rm -rf
			// that never ran.
			//
			// LeftoverRemoveCommand refuses any path not ending in its kind's
			// suffix followed by a unix timestamp, because that stamp is the
			// only evidence vmsync made the thing. So a set without one was
			// chosen here on every run and refused there on every run, logging
			// "leaving a displaced set in place" each time while the space was
			// never freed and nothing said it never would be. Measured on
			// 2026-10-19: 42 GiB selected, zero removed, indefinitely.
			//
			// Aligned on the CONSERVATIVE side rather than by relaxing the
			// remover, because of what an unstamped set actually IS. Every
			// name vmsync writes ends in its suffix followed by the unix time
			// it was displaced, and AttributeLeftovers only attributes a path
			// carrying that suffix at all -- so a set reaching here has
			// vmsync's suffix with something other than a timestamp after it.
			// Nothing in this program produces that. A hand rename does, and
			// an operator who renamed a disk aside to keep it has said the
			// opposite of "delete this".
			//
			// Reachable for replaced disks and restore staging copies only:
			// an aside restore-point store whose trailer is not all digits is
			// not attributed as a leftover at all (sameDirAside), so that kind
			// never arrives here.
			//
			// StampFromName exists "so a caller can say so rather than
			// presenting a guess as a fact". This is that caller.
			d.Unstamped = append(d.Unstamped, l)

		default:
			d.Reclaim = append(d.Reclaim, l)
		}
	}
	if strings.TrimSpace(replicaIncomplete) != "" {
		// Everything moves to Keep, so a caller that ignored Refused still
		// deletes nothing -- including the unstamped ones, which are not a
		// separate decision while the whole sweep is refused.
		return reclaimDecision{Keep: found, Refused: "the replica is marked incomplete"}
	}
	return d
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
func sweepDisplacedSets(ctx context.Context, cfg syncConfig, runner remoteRunner, replicaIncomplete string, markerErr error, targetDiskPaths []string, now time.Time) leftoverSweep {
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
			"note", "these are the disks a -reinit renamed aside, restore copies an interrupted restore staged, and restore-point stores a -reinit moved aside. Set -reclaim-leftovers-after to a duration (e.g. 720h) to have runs remove the ones older than it, or use -replaced-disk-action=delete to stop making the disk asides at all. The date above is the stamp in the name where there is one and the filesystem mtime where there is not; a set with no stamp is never removed by any duration, and a run with reclaiming on says which those are")
		return sweep
	}

	// The interlock's input is READ BY THE CALLER, on the line before this is
	// called, and passed in with whatever error that read produced.
	//
	// Taken at the TOP of the run lock, before the caller arms the field for
	// its own copy. That is the whole point of it being a parameter: read
	// beside this call it would be the value the same run had just written on
	// every rebuild, and the refusal below would stand the sweep down on the
	// one run that both creates aside sets and writes full-size bases. What
	// this function is asking is whether a PREVIOUS copy left the replica
	// unusable -- the state the field's own doc describes -- so the answer has
	// to predate this run's arm.
	//
	// It is a parameter for a second reason: the read is the only thing in
	// here that needs a libvirt connection, and keeping it inside made
	// everything past this point -- including the loop that runs rm -rf --
	// unreachable to any test, because a nil Manager panics inside
	// ReadDomainMetadataFields rather than returning the error the branch
	// below is written to absorb.
	//
	// Fails CLOSED. An unreadable marker might be a set marker, and the cost of
	// being wrong is asymmetric to the point of not being a judgement call: keep
	// the asides and the worst case is that a disk stays full one more day;
	// delete them while the marker was set and the only complete copy of the
	// replica is gone.
	if markerErr != nil {
		sweep.Refused = "could not read the target's replica_incomplete marker"
		trace.Warning("not reclaiming any displaced set: the target's replica_incomplete marker could not be read, and while it might be set these asides are the only complete replica there is",
			"sets", len(found), "error", markerErr)
		return sweep
	}

	decided := leftoversToReclaim(found, after, now, replicaIncomplete)
	reclaim, keep, unstamped := decided.Reclaim, decided.Keep, decided.Unstamped
	if decided.Refused != "" {
		sweep.Refused = decided.Refused
		trace.Warning("REFUSING to reclaim displaced sets: this replica is marked incomplete, so a full copy did not finish and these aside files are the complete replica it replaced -- putting them back over the disks is the recovery. Clear the finding (re-run the sync, which repairs it) and the next run will reclaim them",
			"sets", len(found), "bytes", sweep.FoundBytes, "replica_incomplete", replicaIncomplete)
		return sweep
	}

	// Said BEFORE the removals, because it is the half of the sweep that will
	// read the same on every future run: the removals below change what is
	// there, and these do not change at all.
	if len(unstamped) > 0 {
		sweep.Unreclaimable = unstamped
		sweep.UnreclaimableBytes = bytesOf(unstamped)
		oldestUnstamped := unstamped[len(unstamped)-1]
		trace.Warning("displaced sets old enough to reclaim that vmsync will NEVER reclaim on its own, and the date below is a guess: these carry vmsync's own suffix with something other than a unix timestamp after it, so there is no record of when they were displaced and their age here comes from the filesystem mtime. Deal with them by hand -- no future run will",
			"sets", len(unstamped), "bytes", sweep.UnreclaimableBytes,
			"mtime_of_oldest", time.Unix(oldestUnstamped.AtUnix, 0).UTC().Format(time.RFC3339),
			"oldest_path", oldestUnstamped.Path,
			"note", "vmsync does not create these: every name it writes ends in the suffix followed by the time of the displacement, so the likeliest explanation is that somebody renamed a set aside deliberately -- in which case it is being kept on purpose and should not be deleted at all. vmsync will not rm -rf something it cannot prove it created, and an mtime is not that proof: it is whenever the file was last written, which for a disk aside is the moment the replica it replaced was last synced and not the moment it was set aside")
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
	// kept_bytes counts only the keep bucket, so the three figures beside it
	// add up to what was found: freed + kept_bytes + unreclaimable_bytes.
	// FoundBytes-RemovedBytes was printed here beside a `kept` count that
	// excludes the unstamped sets, so the two disagreed by exactly their size.
	trace.Info("displaced sets after reclaiming", "removed", len(sweep.Removed), "freed", sweep.RemovedBytes,
		"kept", len(keep), "kept_bytes", bytesOf(keep),
		"unreclaimable", len(unstamped), "unreclaimable_bytes", sweep.UnreclaimableBytes,
		"older_than", after.String())
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
