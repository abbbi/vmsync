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
	"strings"
	"testing"
	"time"

	"vmsync/pkg/restorepoint"
)

// recordingListingRunner answers listings like fakeListingRunner does, and
// keeps every command it was asked to run so a test can assert on what was
// deleted rather than on what a plan said.
type recordingListingRunner struct {
	fakeListingRunner
	ran *[]string
}

func (r recordingListingRunner) Run(ctx context.Context, cmd string) (string, error) {
	*r.ran = append(*r.ran, cmd)
	// The sidecar read gets a well-formed answer with no records in it: this
	// fixture is a store whose points carry no publish instants, which is an
	// answer and not a failure, so the prune proceeds and the keep protection
	// is the only thing standing between this run and its own point.
	//
	// It must be answered explicitly. The listing names the same directory,
	// so falling through would hand the sidecar parser a listing -- which it
	// rejects, which skips the prune, which would make every assertion below
	// pass for the wrong reason.
	if strings.Contains(cmd, testMarkerStatusPoint) {
		return testMarkerStatusList + "\n", nil
	}
	return r.fakeListingRunner.Run(ctx, cmd)
}

// THROUGH prune, not through Prune: the protection is only worth anything if
// the caller hands over the tag it just published, and Prune's own tests
// cannot see a caller that passes the zero Tag. That mutation -- the one-line
// difference between the fix working and the fix being inert -- survived
// every test in the package until this one existed.
//
// The scenario is a source whose clock ran ahead and was corrected. Its older
// points carry future instants, the point taken now carries a real one, and
// ordering here is by the SOURCE's instant: the newest copy is the
// oldest-looking tag, and a full store cuts it.
func TestPruneDoesNotDeleteThePointThisRunJustPublished(t *testing.T) {
	store := rpTestStore(t)
	dir, _ := store.Path()

	now := time.Unix(1_700_000_000, 0).UTC()
	ahead := now.Add(48 * time.Hour)
	fresh, err := restorepoint.NewTag(now, "vmsync-cpt-000004")
	if err != nil {
		t.Fatalf("NewTag: %v", err)
	}
	stale := []restorepoint.Tag{}
	for i, at := range []time.Time{ahead, ahead.Add(-time.Hour), ahead.Add(-2 * time.Hour)} {
		tag, err := restorepoint.NewTag(at, "vmsync-cpt-00000"+string(rune('1'+i)))
		if err != nil {
			t.Fatalf("NewTag: %v", err)
		}
		stale = append(stale, tag)
	}

	entries := []string{fresh.String()}
	for _, tag := range stale {
		entries = append(entries, tag.String())
	}

	var ran []string
	r := &restorePoints{
		runner: recordingListingRunner{
			fakeListingRunner: fakeListingRunner{byDir: map[string][]string{dir: entries}},
			ran:               &ran,
		},
		store:  store,
		point:  store.Point(fresh),
		policy: restorepoint.Policy{Count: 3, Interval: time.Hour},
		armed:  true,
	}
	r.prune(context.Background())

	for _, cmd := range ran {
		if strings.Contains(cmd, fresh.String()) {
			t.Fatalf("prune ran a command naming the point this run just published (%s): %q\nthe run copied every disk, published the point, then deleted it -- and reported outcome=taken", fresh, cmd)
		}
	}
	// Not vacuous: the policy was over its count, so something had to go.
	removedOldest := false
	for _, cmd := range ran {
		if strings.Contains(cmd, stale[2].String()) {
			removedOldest = true
		}
	}
	if !removedOldest {
		t.Errorf("prune removed nothing for a store of 4 points under a count of 3; commands were %q", ran)
	}
}
