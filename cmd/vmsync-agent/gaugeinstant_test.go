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
	"testing"

	"vmsync/pkg/inventory"
)

// The newest/oldest gauges report HOW OLD THE DATA IS, which is the checkpoint
// instant, and must not drift onto the publish instant.
//
// The two were the same number until the target began stamping taken_at, and
// the comment on this code said so. They come apart exactly when a clock is
// wrong -- which is when an operator is most likely to be reading the gauge.
// Moving these onto taken_at would silently re-base every staleness alert and
// put them at odds with the engine's
// vmsync_restore_point_last_taken_timestamp_seconds, which is derived from the
// tag. The LIST is ordered by taken_at; these gauges are not.
func TestRestorePointGaugesReportTheDataInstantNotThePublishInstant(t *testing.T) {
	const dataAt, publishedAt = 1700172800, 1699996400 // a clock 48h fast
	g := restorePointGaugeFor([]ReportRestorePoint{
		{Tag: "1700172800-vmsync-cpt-000001", TagAtUnix: dataAt, TakenAtUnix: publishedAt},
	})
	if g.NewestUnix != dataAt {
		t.Errorf("newest = %d, want the checkpoint instant %d. This gauge is documented as the instant the contents correspond to, and the engine publishes the same number from the tag", g.NewestUnix, dataAt)
	}
	if g.OldestUnix != dataAt {
		t.Errorf("oldest = %d, want the checkpoint instant %d", g.OldestUnix, dataAt)
	}
	if g.NewestUnix == publishedAt {
		t.Error("the gauges moved onto taken_at, which re-bases every staleness alert onto a different clock than the engine's gauge of the same name")
	}
}

// And the field has to survive the hop from the inventory to the report, or
// the gauges above read zero for every point.
func TestReportRestorePointsCarriesBothInstants(t *testing.T) {
	got := reportRestorePoints([]inventory.RestorePointInfo{
		{Tag: "1700172800-vmsync-cpt-000001", TagAtUnix: 1700172800, TakenAtUnix: 1699996400},
	})
	if len(got) != 1 {
		t.Fatalf("got %d reports, want 1", len(got))
	}
	if got[0].TagAtUnix != 1700172800 {
		t.Errorf("tag instant = %d, want it carried through; at zero every gauge derived from it reads 1970", got[0].TagAtUnix)
	}
	if got[0].TakenAtUnix != 1699996400 {
		t.Errorf("publish instant = %d, want it carried through", got[0].TakenAtUnix)
	}
}
