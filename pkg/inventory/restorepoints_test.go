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
	"strings"
	"testing"

	"vmsync/pkg/failover"
	"vmsync/pkg/restorepoint"
)

// replicaDir is a temporary directory named the way a replica directory is: an
// absolute, slash-separated path.
//
// The normalisation is a no-op on Linux, where filepath.VolumeName is empty and
// ToSlash changes nothing, and it exists so these tests can also be run on a
// machine with drive letters: a restore point store insists on a clean
// slash-absolute path, because every command it builds runs on a target host
// with an unknown working directory. A Windows temp dir is "C:/..." , which
// path.IsAbs rejects -- and stripping the volume leaves a path that names the
// same directory on the current drive.
func replicaDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	return strings.TrimPrefix(filepath.ToSlash(dir), filepath.VolumeName(dir))
}

// writePoint materialises one restore point directory with a sidecar, the way a
// sync would.
func writePoint(t *testing.T, dir, tag, source string) {
	t.Helper()
	p := filepath.Join(dir, tag)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", p, err)
	}
	if source == "" {
		// No sidecar at all: an interrupted run, or a point written by
		// something else. Read as unattributable rather than as ours.
		return
	}
	b, err := restorepoint.Status{Source: source, Verify: restorepoint.VerifyNotRun, Checkpoint: "vmsync-cpt-000042"}.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := os.WriteFile(filepath.Join(p, restorepoint.StatusName), b, 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
}

// domainIn builds a target Domain whose one disk lives in dir.
func domainIn(t *testing.T, dir, name, source string) Domain {
	t.Helper()
	return Domain{
		Name:          name,
		ReplicaSource: source,
		Disks:         []DiskInfo{{Path: filepath.Join(dir, name+"-disk0.qcow2")}},
	}
}

// Two replicas in one directory must not see each other's restore points.
//
// This is the reporting end of the shared-store defect recorded as CI-06: a
// reader that walks the shared directory returns every co-located domain's
// points as this domain's -- and the control plane then offers another pair's
// copies as a rollback target for this machine. The only thing that would stop
// a wrong restore is a check on the target that runs after the operator has
// already committed to the operation.
func TestCoLocatedDomainsDoNotSeeEachOthersRestorePoints(t *testing.T) {
	dir := replicaDir(t)
	root := filepath.Join(dir, restorepoint.DirName)

	web := domainIn(t, dir, "web01", "prod01:web01")
	db := domainIn(t, dir, "db01", "prod01:db01")

	webStore, ok := restorePointStoreFor(web)
	if !ok {
		t.Fatal("no store for web01")
	}
	dbStore, ok := restorePointStoreFor(db)
	if !ok {
		t.Fatal("no store for db01")
	}
	webRoot, err := webStore.Path()
	if err != nil {
		t.Fatalf("web01 store path: %v", err)
	}
	dbRoot, err := dbStore.Path()
	if err != nil {
		t.Fatalf("db01 store path: %v", err)
	}
	if webRoot == dbRoot {
		t.Fatalf("both domains address %q", webRoot)
	}
	writePoint(t, webRoot, "1756041600-vmsync-cpt-000042", "prod01:web01")
	writePoint(t, webRoot, "1756052400-vmsync-cpt-000043", "prod01:web01")
	writePoint(t, dbRoot, "1756060000-vmsync-cpt-000007", "prod01:db01")

	got := RestorePointsFor(web)
	if len(got) != 2 {
		t.Fatalf("web01 sees %d restore points, want its own 2 -- %+v", len(got), got)
	}
	for _, p := range got {
		if p.Source != "prod01:web01" {
			t.Errorf("web01 was offered a point belonging to %s", p.Source)
		}
	}
	if n := len(RestorePointsFor(db)); n != 1 {
		t.Errorf("db01 sees %d restore points, want its own 1", n)
	}
	// And the shared root itself holds no points, only the two stores.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read %s: %v", root, err)
	}
	for _, e := range entries {
		if _, err := restorepoint.ParseTag(e.Name()); err == nil {
			t.Errorf("%s is a restore point directly in the shared root", e.Name())
		}
	}
}

// A domain whose disks span several directories has no single store, and
// reporting one directory's contents would be a partial answer to a question
// with no answer -- retention refuses such a domain for the same reason.
func TestADomainWithScatteredDisksHasNoStore(t *testing.T) {
	a, b := replicaDir(t), replicaDir(t)
	d := Domain{
		Name:          "web01",
		ReplicaSource: "prod01:web01",
		Disks: []DiskInfo{
			{Path: filepath.Join(a, "web01-disk0.qcow2")},
			{Path: filepath.Join(b, "web01-disk1.qcow2")},
		},
	}
	if _, ok := restorePointStoreFor(d); ok {
		t.Error("a domain with disks in two directories was given a single restore point store")
	}
	if got := RestorePointsFor(d); got != nil {
		t.Errorf("got %+v, want nothing rather than one directory's half of a set", got)
	}
}

// --- displaced sets (CI-54) ------------------------------------------------

func TestTrailingUnix(t *testing.T) {
	for _, tc := range []struct {
		name, sep string
		want      int64
	}{
		{"web01.qcow2.vmsync-replaced-1758441600", failover.ReplicaReplacedSuffix, 1758441600},
		{".replaced-vm-web01-1758441600", "-", 1758441600},
		// An older build's naming, or a name somebody changed by hand. Reported as
		// having no stamp rather than refused: the set is still there and still
		// costs, and the age is the only thing unknown.
		{"web01.qcow2.vmsync-replaced-", failover.ReplicaReplacedSuffix, 0},
		{"web01.qcow2.vmsync-replaced-yesterday", failover.ReplicaReplacedSuffix, 0},
		{"web01.qcow2", failover.ReplicaReplacedSuffix, 0},
	} {
		if got := trailingUnix(tc.name, tc.sep); got != tc.want {
			t.Errorf("trailingUnix(%q, %q) = %d, want %d", tc.name, tc.sep, got, tc.want)
		}
	}
}

// Only this domain's displaced sets, and only the ones vmsync made.
//
// The negative cases matter as much as the positive ones. This runs for every
// domain on every report cycle, and a scan that mistook a live replica disk for a
// leftover would report the working set as reclaimable space.
//
// web02's aside is the sharpest of them. Co-located replicas share one images
// directory, so a scan anchored on the directory rather than on this domain's own
// disks charges every displaced set to every domain in it -- a tenfold
// over-report on a busy host, and a path offered as this machine's reclaimable
// space that is another machine's only good copy.
func TestScanLeftoverDirFindsOnlyDisplacedSets(t *testing.T) {
	dir := t.TempDir()
	write := func(name string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("web01.qcow2")                                                 // the live replica
	write("web01.qcow2" + failover.ReplicaReplacedSuffix + "1758441600") // a rebuild's aside
	write("web02.qcow2" + failover.ReplicaReplacedSuffix + "1758441700") // a co-located domain's
	write("web01.qcow2" + restorepoint.RestoreTempSuffix + "1758441800") // a staged restore copy
	write("web01.qcow2.bak")                                             // somebody else's backup: not ours

	got := scanLeftoverDir(dir, map[string]bool{"web01.qcow2": true})
	kinds := map[string]int{}
	for _, l := range got {
		kinds[l.Kind]++
		if l.Bytes <= 0 {
			t.Errorf("%s reported %d bytes; a set that costs nothing would never need reporting", l.Path, l.Bytes)
		}
		if strings.Contains(l.Path, "web02") {
			t.Errorf("%s is web02's displaced disk, reported as web01's: the bytes are counted against the "+
				"wrong machine and the path offered as this one's reclaimable space is another one's only "+
				"good copy", l.Path)
		}
	}
	if kinds["replaced-disk"] != 1 {
		t.Errorf("replaced-disk = %d, want 1: %+v", kinds["replaced-disk"], got)
	}
	if kinds["restore-staging"] != 1 {
		t.Errorf("restore-staging = %d, want 1: %+v", kinds["restore-staging"], got)
	}
	if len(got) != 2 {
		t.Errorf("got %d leftovers, want 2 -- the live disk and an unrelated .bak must not be reported as "+
			"reclaimable: %+v", len(got), got)
	}
	// The stamp has to survive, because age is what separates last night's
	// rebuild from one nobody has looked at in months.
	for _, l := range got {
		if l.Kind == "replaced-disk" && l.AtUnix == 0 {
			t.Errorf("%s lost its stamp", l.Path)
		}
	}
}

// TestScanAsideStoresFindsWhatNoListingCanSee is the sub-case RST-021 calls
// invisible, and it is the sharpest of the four: these directories sit BESIDE the
// per-domain stores, so -list-restore-points -- which reads inside one store --
// cannot see them, and the points inside them are outside every store, so no
// command can name them. Only the directory's size ever said they were there.
func TestScanAsideStoresFindsWhatNoListingCanSee(t *testing.T) {
	parent := t.TempDir()
	// The live store for this domain, which must NOT be reported.
	live := filepath.Join(parent, restorepoint.DomainPrefix+"web01")
	if err := os.MkdirAll(filepath.Join(live, "1758441600-vmsync-cpt-000001"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// And one a reinit set aside.
	aside := filepath.Join(parent, restorepoint.AsidePrefix+"vm-web01-1758441600")
	if err := os.MkdirAll(filepath.Join(aside, "1758441500-vmsync-cpt-000000"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(aside, "1758441500-vmsync-cpt-000000", "web01.qcow2"), []byte("data"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	// And two that belong to somebody else, in the same parent, because that is
	// where every co-located domain's asides also live: db01's, and the one
	// belonging to a domain whose name merely starts with this one's -- the case
	// a prefix test passes and a whole-name test does not.
	for _, other := range []string{
		restorepoint.AsidePrefix + restorepoint.DomainPrefix + "db01-1758441700",
		restorepoint.AsidePrefix + restorepoint.DomainPrefix + "web01-old-1758441800",
	} {
		if err := os.MkdirAll(filepath.Join(parent, other), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	got := scanAsideStores(parent, "web01")
	if len(got) != 1 {
		t.Fatalf("got %d aside stores, want exactly 1 -- the live store, another domain's aside and the "+
			"aside of a domain whose name starts with this one's must all stay out: %+v", len(got), got)
	}
	if got[0].Kind != "aside-store" {
		t.Errorf("Kind = %q", got[0].Kind)
	}
	if got[0].Path != aside {
		t.Errorf("Path = %q, want %q -- this is the path an operator is about to delete", got[0].Path, aside)
	}
	if got[0].AtUnix != 1758441600 {
		t.Errorf("AtUnix = %d, want the stamp in the directory name", got[0].AtUnix)
	}
	if got[0].Bytes <= 0 {
		t.Error("an aside store holding a disk image reported 0 bytes; the whole point is the space it occupies")
	}
}

// LeftoversFor's own scope, end to end on a shared directory: the case the fleet
// actually runs, since co-locating replicas in one images directory is the
// default rather than the exception.
//
// TestCoLocatedDomainsDoNotSeeEachOthersRestorePoints above is this same claim
// about restore points, and it is here for the same reason: the report this
// feeds drives a gauge an operator alerts on and a path an operator deletes, so
// a set attributed to the wrong domain is both a wrong number and a wrong
// instruction.
func TestLeftoversForAttributesNothingToTheWrongDomain(t *testing.T) {
	dir := replicaDir(t)
	web := domainIn(t, dir, "web01", "prod01:web01")
	db := domainIn(t, dir, "db01", "prod01:db01")

	for _, d := range []Domain{web, db} {
		p := d.Disks[0].Path + failover.ReplicaReplacedSuffix + "1758441600"
		if err := os.WriteFile(p, []byte("xxxx"), 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	store, ok := restorePointStoreFor(web)
	if !ok {
		t.Fatal("no store for web01")
	}
	root, err := store.Path()
	if err != nil {
		t.Fatalf("store path: %v", err)
	}
	// One aside store for each domain, side by side in the shared stores parent.
	for _, seg := range []string{"vm-web01", "vm-db01"} {
		if err := os.MkdirAll(filepath.Join(filepath.Dir(root), restorepoint.AsidePrefix+seg+"-1758441500"), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	for _, tc := range []struct {
		d             Domain
		want, notWant string
	}{
		{web, "web01", "db01"},
		{db, "db01", "web01"},
	} {
		got := LeftoversFor(tc.d)
		if len(got) != 2 {
			t.Errorf("%s: got %d leftovers, want its own 2 (one displaced disk, one aside store): %+v", tc.want, len(got), got)
		}
		for _, l := range got {
			if !strings.Contains(l.Path, tc.want) || strings.Contains(l.Path, tc.notWant) {
				t.Errorf("%s was told %q is its leftover", tc.want, l.Path)
			}
		}
		// Newest first, so the console's first row is the one worth acting on.
		for i := 1; i < len(got); i++ {
			if got[i-1].AtUnix < got[i].AtUnix {
				t.Errorf("%s: leftovers came back oldest-first: %+v", tc.want, got)
			}
		}
	}
}

// The agent's scan and the engine's sweep must agree about what is a displaced
// set, and this is the only place that can be asserted: this package reads the
// files directly on the host it runs on, pkg/restorepoint builds the command that
// reads them over SSH, and nothing else imports both.
//
// Disagreement is silent in both directions and both are bad. A file this scan
// reports and that sweep will not remove is charged to a VM in
// vmsync_agent_replaced_bytes for ever, with no command able to clear it -- an
// alert nobody can action. A file that sweep removes and this scan never reported
// is a full-size copy of production data deleted without ever appearing in the
// series an operator was watching.
func TestTheAgentScanAndTheEngineSweepAgreeOnWhatIsDisplaced(t *testing.T) {
	dir := replicaDir(t)
	d := domainIn(t, dir, "web01", "prod01:web01")
	disk := d.Disks[0].Path

	// Every shape worth disagreeing about, positive and negative.
	names := []string{
		disk, // the live disk
		disk + failover.ReplicaReplacedSuffix + "1758441600",                                                 // an ordinary aside
		disk + failover.ReplicaReplacedSuffix + "1758441600" + failover.ReplicaReplacedSuffix + "1758441700", // displaced twice, by hand
		disk + failover.ReplicaReplacedSuffix + "notastamp",                                                  // no readable stamp
		disk + restorepoint.RestoreTempSuffix + "1758441800",                                                 // a staged restore copy
		disk + ".bak", // somebody else's
		filepath.Join(dir, "db01-disk0.qcow2") + failover.ReplicaReplacedSuffix + "1758441600", // a co-located domain's
	}
	var scan []restorepoint.ScanEntry
	for _, n := range names {
		if err := os.WriteFile(n, []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", n, err)
		}
		// The same entries LeftoverScanCommand would have produced for them.
		scan = append(scan, restorepoint.ScanEntry{Path: filepath.ToSlash(n), MtimeUnix: 1799999999})
	}

	mine := map[string]string{}
	for _, l := range LeftoversFor(d) {
		mine[filepath.ToSlash(l.Path)] = l.Kind
	}
	theirsList, _ := restorepoint.AttributeLeftovers(scan, []string{filepath.ToSlash(disk)}, "web01")
	theirs := map[string]string{}
	for _, l := range theirsList {
		theirs[l.Path] = l.Kind
	}

	for p, kind := range mine {
		if theirs[p] == "" {
			t.Errorf("the agent reports %s as a %s and the engine sweep does not see it at all: its bytes are "+
				"charged to this VM for ever with nothing able to reclaim them", p, kind)
			continue
		}
		if theirs[p] != kind {
			t.Errorf("%s is %q to the agent and %q to the sweep; Kind is what decides which recovery an "+
				"operator reaches for", p, kind, theirs[p])
		}
	}
	for p, kind := range theirs {
		if mine[p] == "" {
			t.Errorf("the engine sweep would remove %s as a %s and the agent never reported it: a full-size "+
				"copy of production data deleted without appearing in the series anybody was watching", p, kind)
		}
	}
	if len(mine) == 0 {
		t.Fatal("neither reader found anything, so this proved nothing")
	}
}
