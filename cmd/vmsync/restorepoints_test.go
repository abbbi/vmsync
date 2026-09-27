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

// fakeListingRunner answers a listing command with whatever the test put in it,
// keyed by the directory the command names.
type fakeListingRunner struct{ byDir map[string][]string }

func (f fakeListingRunner) Run(_ context.Context, cmd string) (string, error) {
	for dir, entries := range f.byDir {
		if strings.Contains(cmd, "'"+dir+"'") {
			out := "__VMSYNC_RP_LIST__\n"
			for _, e := range entries {
				out += e + "/\n"
			}
			return out, nil
		}
	}
	return "__VMSYNC_RP_NONE__\n", nil
}

func rpTestStore(t *testing.T) restorepoint.Store {
	t.Helper()
	s, err := restorepoint.StoreForDir("/data/replicas", "web01")
	if err != nil {
		t.Fatalf("StoreForDir: %v", err)
	}
	return s
}

// A tag is looked for in THIS target domain's store and nowhere else.
//
// The refusal is what matters. A copy left by a pre-change vmsync sits flat in
// the shared directory, and a lookup that fell back to it would sometimes clone
// or restore a machine from a different lineage than the tag named -- these
// verbs touch no libvirt, so they hold no replica_source to check a sidecar
// against and could not detect it. There is deliberately no way to reach one
// from here at all: the store addresses one domain, so a flat point is simply
// not a path this code can name.
func TestFindRestorePointLooksOnlyInThisDomainsStore(t *testing.T) {
	s := rpTestStore(t)
	tag, err := restorepoint.ParseTag("1756041600-vmsync-cpt-000042")
	if err != nil {
		t.Fatalf("ParseTag: %v", err)
	}
	shared := "/data/replicas/" + restorepoint.DirName
	mine, _ := s.Path()

	// The tag exists ONLY flat in the shared directory, one level above this
	// domain's store.
	if _, err := findRestorePoint(context.Background(), fakeListingRunner{byDir: map[string][]string{
		shared: {"1756041600-vmsync-cpt-000042"},
	}}, s, tag); err == nil {
		t.Fatal("a tag present only in the shared directory was found by a lookup in this domain's store")
	}

	// And it is found when it is genuinely this domain's.
	p, err := findRestorePoint(context.Background(), fakeListingRunner{byDir: map[string][]string{
		mine: {"1756041600-vmsync-cpt-000042"},
	}}, s, tag)
	if err != nil {
		t.Fatalf("the ordinary lookup could not find its own point: %v", err)
	}
	dir, err := p.Dir()
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	if want := mine + "/1756041600-vmsync-cpt-000042"; dir != want {
		t.Errorf("dir = %q, want %q", dir, want)
	}
}

// A store directory must never be offered as a restore point. The prefix is what
// prevents it, and this exercises it through the real listing parser.
func TestADomainStoreIsNotListedAsARestorePoint(t *testing.T) {
	s := rpTestStore(t)
	mine, _ := s.Path()
	runner := fakeListingRunner{byDir: map[string][]string{
		mine: {"1756041600-vmsync-cpt-000042", "vm-db01", ".replaced-vm-web01-1756000000"},
	}}
	l, err := listRestorePoints(context.Background(), runner, s)
	if err != nil {
		t.Fatalf("listRestorePoints: %v", err)
	}
	if len(l.Points) != 1 {
		t.Fatalf("points = %+v, want only the real tag", l.Points)
	}
	// The other two are reported, not silently dropped: prune warns about an
	// Unknown entry rather than deleting it.
	if len(l.Unknown) != 2 {
		t.Errorf("unknown = %+v, want both non-tag directories reported", l.Unknown)
	}
}

func TestApplyRestorePointSummaryLeavesAbsentInstantsAtZero(t *testing.T) {
	var s restorePointStats
	applyRestorePointSummary(&s, restorepoint.Summarize(restorepoint.Listing{}, restorepoint.Policy{Count: 24, Interval: time.Hour}))
	if s.Count != 0 || s.Newest != 0 || s.Oldest != 0 || s.Over != 0 {
		t.Errorf("an empty store summarised to %+v; a non-zero instant here would be published as a real timestamp", s)
	}

	tag, _ := restorepoint.ParseTag("1756041600-vmsync-cpt-000042")
	older, _ := restorepoint.ParseTag("1756000000-vmsync-cpt-000041")
	applyRestorePointSummary(&s, restorepoint.Summarize(
		restorepoint.Listing{Points: []restorepoint.Tag{tag, older}},
		restorepoint.Policy{Count: 1, Interval: time.Hour}))
	if s.Count != 2 || s.Over != 1 {
		t.Errorf("stats = %+v, want 2 points and 1 over a policy that keeps 1", s)
	}
	if s.Newest != 1756041600 || s.Oldest != 1756000000 {
		t.Errorf("newest/oldest = %d/%d, want 1756041600/1756000000", s.Newest, s.Oldest)
	}
}
