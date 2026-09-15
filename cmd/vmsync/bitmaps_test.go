package main

import (
	"strings"
	"testing"
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
		{"the removal command", "qemu-img bitmap --remove " + testDiskPath + " " + testBitmap},
		{"how to stop the domain first", "virsh shutdown " + testDomain},
		{"how to start it again", "virsh start " + testDomain},
		{"why vmsync will not do it itself", "qemu holds the image open"},
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
		"qemu-img bitmap --remove /vm/a.qcow2 vmsync-cpt-000001",
		"qemu-img bitmap --remove /vm/a.qcow2 vmsync-cpt-000002",
		"qemu-img bitmap --remove /vm/b.qcow2 vmsync-cpt-000001",
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
