package main

import (
	"errors"
	"strings"
	"testing"

	"vmsync/pkg/disk"
	"vmsync/pkg/libvirtsync"
)

const (
	testDomain   = "immich01l.npf.local"
	testDiskPath = "/vm_data/npf/immich01l.npf.local-disk0.qcow2"
	testBitmap   = "vmsync-cpt-000001"
)

// No leftovers is the overwhelmingly common case and must not produce an error.
func TestNoLeftoversIsNotARefusal(t *testing.T) {
	if err := leftoverBitmapRefusal(testDomain, nil); err != nil {
		t.Fatalf("an empty result produced a refusal: %v", err)
	}
	if err := leftoverBitmapRefusal(testDomain, map[string][]string{}); err != nil {
		t.Fatalf("an empty map produced a refusal: %v", err)
	}
}

// The message is the whole feature. It replaces qemu's "Bitmap already exists:
// vmsync-cpt-000001", which names a thing the operator cannot find, on a host
// it does not identify, with no remedy. Everything asserted here is something
// that message lacked.
func TestRefusalCarriesEverythingNeededToFixIt(t *testing.T) {
	err := leftoverBitmapRefusal(testDomain, map[string][]string{testDiskPath: {testBitmap}})
	if err == nil {
		t.Fatal("a leftover bitmap did not produce a refusal")
	}
	msg := err.Error()

	for _, want := range []struct{ what, s string }{
		{"the domain", testDomain},
		{"the disk file the bitmap is in", testDiskPath},
		{"the bitmap's name", testBitmap},
		{"the removal command", "qemu-img bitmap --remove -f qcow2 " + testDiskPath + " " + testBitmap},
		{"how to stop the domain first", "virsh shutdown " + testDomain},
		{"how to start it again", "virsh start " + testDomain},
		{"why vmsync will not do it itself", "qemu holds the image open"},
		// The no-downtime alternative, which is the only one an operator can
		// run without stopping the guest. It was added once libvirt was shown
		// to accept it, and a refusal that omits it sends people to a reboot
		// they do not need.
		{"the no-downtime route", "--redefine"},
		{"what that route requires", "creationTime"},
		{"the qemu error this explains", "Bitmap already exists"},
		{"why virsh shows nothing", "checkpoint-list"},
	} {
		if !strings.Contains(msg, want.s) {
			t.Errorf("the refusal does not mention %s (%q)", want.what, want.s)
		}
	}
}

// Multiple disks and multiple bitmaps must each get their own removal command,
// or an operator fixes one and hits the next on the following run.
func TestEveryBitmapGetsItsOwnCommand(t *testing.T) {
	err := leftoverBitmapRefusal(testDomain, map[string][]string{
		"/vm/a.qcow2": {"vmsync-cpt-000001", "vmsync-cpt-000002"},
		"/vm/b.qcow2": {"vmsync-cpt-000001"},
	})
	if err == nil {
		t.Fatal("no refusal")
	}
	msg := err.Error()
	for _, want := range []string{
		"qemu-img bitmap --remove -f qcow2 /vm/a.qcow2 vmsync-cpt-000001",
		"qemu-img bitmap --remove -f qcow2 /vm/a.qcow2 vmsync-cpt-000002",
		"qemu-img bitmap --remove -f qcow2 /vm/b.qcow2 vmsync-cpt-000001",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing removal command: %q", want)
		}
	}
	if n := strings.Count(msg, "qemu-img bitmap --remove"); n != 3 {
		t.Errorf("got %d removal commands, want 3", n)
	}
	if !strings.Contains(msg, "3 dirty bitmap(s)") {
		t.Errorf("the count is wrong or missing:\n%s", msg)
	}
}

// Map iteration order is random. The same fault must read identically every
// time, or two operators comparing notes see two different messages.
func TestMessageIsStableAcrossRuns(t *testing.T) {
	in := map[string][]string{
		"/vm/z.qcow2": {"vmsync-cpt-000002"},
		"/vm/a.qcow2": {"vmsync-cpt-000001"},
		"/vm/m.qcow2": {"vmsync-cpt-000003"},
	}
	first := leftoverBitmapRefusal(testDomain, in).Error()
	for i := 0; i < 50; i++ {
		if got := leftoverBitmapRefusal(testDomain, in).Error(); got != first {
			t.Fatalf("the message varies between runs:\n%s\n---\n%s", first, got)
		}
	}
	// And sorted, so the order is predictable rather than merely consistent.
	a := strings.Index(first, "/vm/a.qcow2")
	m := strings.Index(first, "/vm/m.qcow2")
	z := strings.Index(first, "/vm/z.qcow2")
	if !(a < m && m < z) {
		t.Errorf("disks are not in sorted order (a=%d m=%d z=%d)", a, m, z)
	}
}

// Which bitmaps are orphaned decides whether a rebuild can proceed at all, so
// both directions matter: calling a tracked bitmap an orphan refuses a healthy
// rebuild, and missing a real orphan lets the run destroy the target and the
// source's baseline before failing on it.
func TestOrphanBitmapsIsTheSetWithNoCheckpointBehindIt(t *testing.T) {
	cps := checkpointNames([]libvirtsync.Checkpoint{{Name: "vmsync-cpt-000002"}, {Name: "vmsync-cpt-000003"}})

	got := orphanBitmaps(map[string][]string{
		"/vm/web01-vda.qcow2": {"vmsync-cpt-000001", "vmsync-cpt-000002"},
		"/vm/web01-vdb.qcow2": {"vmsync-cpt-000002", "vmsync-cpt-000003"},
	}, cps)

	if len(got) != 1 {
		t.Fatalf("got %+v, want only vda's untracked bitmap", got)
	}
	if n := got["/vm/web01-vda.qcow2"]; len(n) != 1 || n[0] != "vmsync-cpt-000001" {
		t.Errorf("vda orphans = %v, want just vmsync-cpt-000001", n)
	}
	if _, ok := got["/vm/web01-vdb.qcow2"]; ok {
		t.Error("a disk whose every bitmap has a checkpoint was reported as holding orphans; that refuses a rebuild with nothing wrong with it")
	}

	// Nothing tracked at all is the state an interrupted run or a
	// metadata-only delete leaves, and every bitmap is then an orphan.
	all := orphanBitmaps(map[string][]string{"/vm/web01-vda.qcow2": {"vmsync-cpt-000001"}}, nil)
	if len(all["/vm/web01-vda.qcow2"]) != 1 {
		t.Errorf("with no checkpoints known, got %+v, want every bitmap orphaned", all)
	}

	// And the clean cases produce nothing, so the caller's len()==0 branch is
	// the one that runs on a healthy source.
	if n := len(orphanBitmaps(nil, cps)); n != 0 {
		t.Errorf("no bitmaps gave %d orphans", n)
	}
	if n := len(orphanBitmaps(map[string][]string{"/vm/a.qcow2": {"vmsync-cpt-000002"}}, cps)); n != 0 {
		t.Errorf("a fully tracked disk gave %d orphans", n)
	}
}

// --- clearing orphans on a running source ---------------------------------

// The disks a fixture domain owns, with the two shapes that matter: a second
// disk the chain may or may not cover, and a path no disk claims.
func testDisks() []disk.QcowDisk {
	return []disk.QcowDisk{
		{TargetDev: "vda", Source: "/vm/a.qcow2"},
		{TargetDev: "vdb", Source: "/vm/b.qcow2"},
	}
}

// One checkpoint puts ONE bitmap name on every disk it covers, so the plan has
// to be per name and not per disk: adopting per disk would redefine the same
// checkpoint twice and libvirt refuses the second for already existing.
func TestAdoptionPlanIsPerBitmapNotPerDisk(t *testing.T) {
	plan, unmapped := adoptionPlan(map[string][]string{
		"/vm/a.qcow2": {"vmsync-cpt-000001", "vmsync-cpt-000002"},
		"/vm/b.qcow2": {"vmsync-cpt-000001"},
	}, testDisks())

	if len(unmapped) != 0 {
		t.Fatalf("disks the domain owns came back unmapped: %v", unmapped)
	}
	if len(plan) != 2 {
		t.Fatalf("got %d adoptions, want 2 (one per bitmap name): %+v", len(plan), plan)
	}
	// Sorted, so this is deterministic rather than lucky.
	if plan[0].Bitmap != "vmsync-cpt-000001" || plan[1].Bitmap != "vmsync-cpt-000002" {
		t.Fatalf("adoptions are not in name order: %+v", plan)
	}
	if got := strings.Join(plan[0].Devs, ","); got != "vda,vdb" {
		t.Errorf("the bitmap on both disks got devs %q, want \"vda,vdb\"", got)
	}
	if got := strings.Join(plan[1].Devs, ","); got != "vda" {
		t.Errorf("the bitmap on one disk got devs %q, want \"vda\"", got)
	}
}

// A bitmap in a file the domain does not list cannot be adopted -- there is no
// target device to put in the xml -- and must not be silently dropped, or the
// sweep reports success over a bitmap still sitting there.
func TestAdoptionPlanSetsAsideABitmapOnADiskTheDomainDoesNotList(t *testing.T) {
	plan, unmapped := adoptionPlan(map[string][]string{
		"/vm/a.qcow2":        {"vmsync-cpt-000001"},
		"/vm/stranger.qcow2": {"vmsync-cpt-000009"},
	}, testDisks())

	if len(plan) != 1 || plan[0].Bitmap != "vmsync-cpt-000001" {
		t.Fatalf("the adoptable bitmap did not survive on its own: %+v", plan)
	}
	if got := unmapped["/vm/stranger.qcow2"]; len(got) != 1 || got[0] != "vmsync-cpt-000009" {
		t.Errorf("the unmappable bitmap was not set aside: %v", unmapped)
	}
}

// Independent orphans: one that cannot be removed must not stop the others, and
// the error has to name every failure rather than the first.
func TestClearingAttemptsEveryOrphanAndNamesEveryFailure(t *testing.T) {
	var asked []string
	_, err := clearOrphanBitmapsOnline(testDomain, map[string][]string{
		"/vm/a.qcow2": {"vmsync-cpt-000001", "vmsync-cpt-000002", "vmsync-cpt-000003"},
	}, testDisks(), func(bitmap string, devs []string) error {
		asked = append(asked, bitmap)
		if bitmap == "vmsync-cpt-000002" {
			return errors.New("libvirt said no")
		}
		return nil
	})

	if len(asked) != 3 {
		t.Fatalf("attempted %d of 3 orphans (%v) -- one failure stopped the rest", len(asked), asked)
	}
	if err == nil {
		t.Fatal("a failed removal did not produce an error")
	}
	if !strings.Contains(err.Error(), "vmsync-cpt-000002") || !strings.Contains(err.Error(), "libvirt said no") {
		t.Errorf("the error does not name the failure or its cause: %v", err)
	}
	if strings.Contains(err.Error(), "vmsync-cpt-000001") {
		t.Errorf("the error blames a bitmap that was removed fine: %v", err)
	}
}

// The count is what tells a run that cleared nothing apart from a run that had
// nothing to clear, so it counts removals and not attempts.
func TestClearingCountsOnlyWhatItRemoved(t *testing.T) {
	cleared, err := clearOrphanBitmapsOnline(testDomain, map[string][]string{
		"/vm/a.qcow2": {"vmsync-cpt-000001", "vmsync-cpt-000002"},
	}, testDisks(), func(bitmap string, devs []string) error { return nil })
	if err != nil {
		t.Fatalf("every removal succeeded and it still errored: %v", err)
	}
	if cleared != 2 {
		t.Errorf("cleared=%d, want 2", cleared)
	}

	cleared, err = clearOrphanBitmapsOnline(testDomain, map[string][]string{
		"/vm/a.qcow2": {"vmsync-cpt-000001"},
	}, testDisks(), func(bitmap string, devs []string) error { return errors.New("no") })
	if err == nil {
		t.Fatal("a total failure did not error")
	}
	if cleared != 0 {
		t.Errorf("cleared=%d after removing nothing, want 0", cleared)
	}
}

// A bitmap the domain does not own is a failure of the sweep even though
// nothing was attempted for it: reporting success would be the clean sweep of
// something still there.
func TestClearingFailsOnABitmapItCannotAddress(t *testing.T) {
	cleared, err := clearOrphanBitmapsOnline(testDomain, map[string][]string{
		"/vm/stranger.qcow2": {"vmsync-cpt-000009"},
	}, testDisks(), func(bitmap string, devs []string) error {
		t.Errorf("a bitmap with no target device was attempted anyway: %s", bitmap)
		return nil
	})
	if err == nil {
		t.Fatal("an unaddressable bitmap was reported as cleared")
	}
	if cleared != 0 {
		t.Errorf("cleared=%d, want 0", cleared)
	}
	if !strings.Contains(err.Error(), "/vm/stranger.qcow2") {
		t.Errorf("the error does not name the disk: %v", err)
	}
}

func TestCountBitmapsTotalsAcrossDisks(t *testing.T) {
	if n := countBitmaps(nil); n != 0 {
		t.Errorf("countBitmaps(nil) = %d, want 0", n)
	}
	n := countBitmaps(map[string][]string{
		"/vm/a.qcow2": {"vmsync-cpt-000001", "vmsync-cpt-000002"},
		"/vm/b.qcow2": {"vmsync-cpt-000001"},
	})
	if n != 3 {
		t.Errorf("countBitmaps = %d, want 3 (bitmaps, not disks)", n)
	}
}
