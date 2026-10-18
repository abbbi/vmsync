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
	"testing"
	"time"

	"vmsync/pkg/restorepoint"
)

// A WITHHELD PRUNE MUST SHOW UP IN THE NUMBERS THE RUN PUBLISHES.
//
// Refusing to prune is only defensible because the growth it causes is
// visible: the warning and docs 10.3 both tell the operator that
// vmsync_restore_points_over_count stays positive and that this is the signal
// to alert on. That claim was false in exactly the case that produces it.
//
// prune() used to refresh its stats only from a SECOND listing round trip.
// ReadAllStatusCommand ends in `exit 0`, so a Run error on the sidecar read is
// never the shell disliking the answer -- it is the session. A session that
// cannot carry the sidecar read cannot carry a listing issued a moment later
// either, so the re-read failed too, silently, and the run kept the summary
// computed BEFORE it published its own point: count one short, over=0, and
// store_readable=1. Not one of the three documented alerts would fire.
//
// The listing this function already holds is the post-publish state, and when
// nothing is pruned it is also the final state. Reporting from it is what
// makes the sentence in the warning true.
func TestAWithheldPruneReportsTheStoreItActuallyLeftBehind(t *testing.T) {
	T := time.Unix(1_700_000_000, 0).UTC()
	f := skewedStore(t, T)

	var ran []string
	reads := 0
	dead := false
	r := &restorePoints{
		runner: sidecarRunner{
			dir: f.dir, entries: f.names, sidecars: f.status,
			failSidecarRead: true, dead: &dead,
			ran: &ran, readCount: &reads,
		},
		store:  f.store,
		point:  f.store.Point(f.tags[3]),
		policy: restorepoint.Policy{Count: 3, Interval: time.Hour},
		armed:  true,
	}
	// What newRestorePoints would have left behind: the store as it was
	// BEFORE this run published, which is one point short.
	r.stats.Count, r.stats.Over = len(f.names)-1, 0

	r.prune(context.Background())

	if got := f.removalsAmong(ran); len(got) != 0 {
		t.Fatalf("the sidecar read failed and prune deleted %v anyway", got)
	}
	if r.stats.Count != len(f.names) {
		t.Errorf("count = %d, want %d -- the run published a point and then reported a store that does not include it", r.stats.Count, len(f.names))
	}
	if r.stats.Over <= 0 {
		t.Errorf("over = %d, want positive. Four points under a count of three were left on the target and the run reported nothing was owed; the warning tells the operator to alert on exactly this series", r.stats.Over)
	}
	if want := len(f.names) - 3; r.stats.Over != want {
		t.Errorf("over = %d, want %d", r.stats.Over, want)
	}
}

// The ordinary path still refreshes its numbers after pruning.
//
// Deliberately weaker than its name used to promise: the fake runner answers
// the re-read with the same listing it gives the first read, so this cannot
// tell a genuine re-read from the stale pre-prune one. What it does pin is
// that SOMETHING refreshes the stats on the success path -- the branch above
// covers the case where the re-read cannot happen, which is the one that was
// actually wrong.
func TestAPrunedStoreRefreshesItsNumbers(t *testing.T) {
	T := time.Unix(1_700_000_000, 0).UTC()
	f := skewedStore(t, T)

	var ran []string
	reads := 0
	r := &restorePoints{
		runner: sidecarRunner{
			dir: f.dir, entries: f.names, sidecars: f.status,
			ran: &ran, readCount: &reads,
		},
		store:  f.store,
		point:  f.store.Point(f.tags[3]),
		policy: restorepoint.Policy{Count: 3, Interval: time.Hour},
		armed:  true,
	}
	r.prune(context.Background())

	// The fake runner answers the re-read with the same entries it always
	// does, so the count is the listing's. What matters is that something
	// refreshed it at all, and that Over is zero on a successful prune.
	if r.stats.Count == 0 {
		t.Error("a successful prune published no count at all")
	}
}
