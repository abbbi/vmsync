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
// This is the defect CI-06 recorded, at the reporting end of it: this reader
// used to walk the shared directory, so every co-located domain's points came
// back as this domain's -- and the control plane then offered another pair's
// copies as a rollback target for this machine. The only thing that stopped a
// wrong restore was a check on the target that runs after the operator has
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
