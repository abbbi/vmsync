/*
	Copyright (C) 2026  Michael Ablassmeier <abi@grinser.de>

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

package disk

import (
	"bytes"
	"log"
	"os"
	"path"
	"strings"
	"testing"

	"vmsync/pkg/trace"
)

// TestResolveRootSource pins down QcowDisk.RootSource's actual resolution
// logic -- previously inline in cmd/vmsync/main.go's disk-info loop, with no
// test coverage at all despite it deciding what every target file is named
// (see ResolveRootSource's and QcowDisk.RootSource's own doc comments).
func TestResolveRootSource(t *testing.T) {
	tests := []struct {
		name   string
		chain  []QemuImgInfo
		source string
		want   string
	}{
		{
			name:   "no backing file: single-element chain resolves to itself, matching source",
			chain:  []QemuImgInfo{{Filename: "/var/lib/libvirt/images/vm.qcow2"}},
			source: "/var/lib/libvirt/images/vm.qcow2",
			want:   "/var/lib/libvirt/images/vm.qcow2",
		},
		{
			// The external-snapshot/linked-clone case: chain is ordered top
			// (source, the domain's currently active file) to base, so the
			// disk's stable target-side name comes from the LAST element,
			// not source itself.
			name: "external snapshot or linked clone: two-element chain resolves to the base, not the active source",
			chain: []QemuImgInfo{
				{Filename: "/var/lib/libvirt/images/vm.qcow2.snap1"},
				{Filename: "/var/lib/libvirt/images/vm.qcow2"},
			},
			source: "/var/lib/libvirt/images/vm.qcow2.snap1",
			want:   "/var/lib/libvirt/images/vm.qcow2",
		},
		{
			name: "deeper chain (multiple stacked overlays) still resolves to the final base",
			chain: []QemuImgInfo{
				{Filename: "/var/lib/libvirt/images/vm.qcow2.snap2"},
				{Filename: "/var/lib/libvirt/images/vm.qcow2.snap1"},
				{Filename: "/var/lib/libvirt/images/vm-base.qcow2"},
			},
			source: "/var/lib/libvirt/images/vm.qcow2.snap2",
			want:   "/var/lib/libvirt/images/vm-base.qcow2",
		},
		{
			// Defensive-only path (see ResolveRootSource's own doc comment):
			// the real callers never actually produce an empty chain without
			// also returning an error first.
			name:   "empty chain falls back to source instead of panicking",
			chain:  nil,
			source: "/var/lib/libvirt/images/vm.qcow2",
			want:   "/var/lib/libvirt/images/vm.qcow2",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolveRootSource(tt.chain, tt.source); got != tt.want {
				t.Errorf("ResolveRootSource(%v, %q) = %q, want %q", tt.chain, tt.source, got, tt.want)
			}
		})
	}
}

// QcowDisk.Path exists because RootSource is empty for every caller except the
// sync path, and nothing about the field says so. Reading it directly is what
// silently disabled cmd/vmsync's reversed-disk-path warning: path.Dir("") is
// "." on both ends of a pair, so the two always compared equal and the warning
// never fired for anyone.
func TestQcowDiskPathFallsBackToSourceWhenTheChainIsUnresolved(t *testing.T) {
	// What ParseQcowDisks produces: Source set, RootSource never populated.
	unresolved := QcowDisk{Source: "/localdata/web01.qcow2"}
	if got := unresolved.Path(); got != "/localdata/web01.qcow2" {
		t.Errorf("Path() = %q, want the domain's own source -- RootSource is empty unless the sync path resolved it", got)
	}

	// What the sync path produces once it has run qemu-img: the chain's base,
	// which is the name the target file was created under and stays stable as
	// more overlays are stacked on top.
	resolved := QcowDisk{Source: "/localdata/web01.snap1.qcow2", RootSource: "/localdata/web01.qcow2"}
	if got := resolved.Path(); got != "/localdata/web01.qcow2" {
		t.Errorf("Path() = %q, want the resolved backing-chain root", got)
	}

	// Both empty is a malformed disk entry, and "" is the honest answer --
	// better than a path that looks real.
	if got := (QcowDisk{}).Path(); got != "" {
		t.Errorf("Path() = %q for a disk with neither field, want empty", got)
	}
}

// The concrete regression: two ends whose disks live in different directories
// must not look identical just because neither chain has been resolved.
func TestQcowDiskPathDistinguishesDirectoriesAcrossAPair(t *testing.T) {
	newSource := QcowDisk{Source: "/data/vmsync-bench/web01.qcow2"}
	newTarget := QcowDisk{Source: "/localdata/web01.qcow2"}

	if path.Dir(newSource.Path()) == path.Dir(newTarget.Path()) {
		t.Fatal("two ends with disks in different directories compared equal; this is what suppressed the inversion warning")
	}
	// And the value is usable in the warning itself: an operator has to be
	// told which directory to pass, not merely that something differs.
	if path.Dir(newTarget.Path()) != "/localdata" {
		t.Errorf("Dir = %q, want /localdata", path.Dir(newTarget.Path()))
	}
}

// BitmapNames reads what a libvirt checkpoint actually IS on disk. The
// inversion's offline cleanup deletes exactly what this returns, so a wrong
// answer either leaves a bitmap whose checkpoint is gone -- which blocks every
// later sync with "Bitmap already exists" -- or removes one belonging to
// something else.
func TestBitmapNames(t *testing.T) {
	// The nested shape qemu-img actually emits.
	withTwo := QcowFormatSpecFixture([]string{"vmsync-cpt-000001", "vmsync-cpt-000002"})
	got := BitmapNames(QemuImgInfo{FormatSpec: withTwo})
	if len(got) != 2 || got[0] != "vmsync-cpt-000001" || got[1] != "vmsync-cpt-000002" {
		t.Fatalf("BitmapNames = %v, want both bitmaps in order", got)
	}

	// An ordinary image has no bitmaps key at all. That is "none", not a
	// fault -- most images vmsync touches are in exactly this state.
	noKey := map[string]interface{}{"data": map[string]interface{}{"compat": "1.1"}}
	if got := BitmapNames(QemuImgInfo{FormatSpec: noKey}); got != nil {
		t.Errorf("BitmapNames = %v for an image with no bitmaps, want nil", got)
	}

	// A raw image has no format-specific block at all.
	if got := BitmapNames(QemuImgInfo{}); got != nil {
		t.Errorf("BitmapNames = %v for an image with no format-specific data, want nil", got)
	}
}

func TestBitmapNamesToleratesMalformedEntries(t *testing.T) {
	// This walks map[string]interface{} decoded from another program's
	// output. Every level has to be guarded, and a surprise must yield "no
	// bitmaps" rather than a panic in the middle of an inversion.
	spec := map[string]interface{}{
		"data": map[string]interface{}{
			"bitmaps": []interface{}{
				"not-an-object",
				map[string]interface{}{"granularity": 65536},        // no name
				map[string]interface{}{"name": ""},                  // empty name
				map[string]interface{}{"name": 42},                  // wrong type
				map[string]interface{}{"name": "vmsync-cpt-000001"}, // the only good one
			},
		},
	}
	got := BitmapNames(QemuImgInfo{FormatSpec: spec})
	if len(got) != 1 || got[0] != "vmsync-cpt-000001" {
		t.Fatalf("BitmapNames = %v, want just the one well-formed entry", got)
	}

	// bitmaps present but not a list.
	bad := map[string]interface{}{"data": map[string]interface{}{"bitmaps": "nonsense"}}
	if got := BitmapNames(QemuImgInfo{FormatSpec: bad}); got != nil {
		t.Errorf("BitmapNames = %v, want nil", got)
	}
	// data present but not a map.
	bad2 := map[string]interface{}{"data": "nonsense"}
	if got := BitmapNames(QemuImgInfo{FormatSpec: bad2}); got != nil {
		t.Errorf("BitmapNames = %v, want nil", got)
	}
}

// A cdrom is expected on nearly every domain, so the quiet variant -- the
// one repeated automated scans use -- logs those skips at debug level only.
// The verbose variant keeps the historical behaviour (everything at INFO)
// for one-shot runs. A disk with an unexpected driver may be a VM somebody
// believes is being replicated and is not, so it stays loud in both.
func TestParseQcowDisksLogsSkipsAtTheRightLevel(t *testing.T) {
	const cdromXML = `<domain type='kvm'>
  <name>vm01</name>
  <devices>
    <disk type='file' device='cdrom'>
      <target dev='sda'/>
    </disk>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='/data/vm01.qcow2'/>
      <target dev='vda'/>
    </disk>
  </devices>
</domain>`
	const rawXML = `<domain type='kvm'>
  <name>vm02</name>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='raw'/>
      <source file='/data/vm02.raw'/>
      <target dev='vda'/>
    </disk>
  </devices>
</domain>`

	t.Run("a cdrom skip is debug-quiet in quiet scans by default", func(t *testing.T) {
		out := captureTrace(t, false)
		disks, err := ParseQcowDisksQuiet(cdromXML)
		if err != nil {
			t.Fatalf("ParseQcowDisksQuiet: %v", err)
		}
		if len(disks) != 1 {
			t.Fatalf("ParseQcowDisksQuiet returned %d disks, want just vda", len(disks))
		}
		if strings.Contains(out.String(), "skipping") {
			t.Errorf("a cdrom skip was logged at INFO; it should be debug-only, got:\n%s", out.String())
		}
	})

	t.Run("a cdrom skip is still visible with debug on", func(t *testing.T) {
		out := captureTrace(t, true)
		if _, err := ParseQcowDisksQuiet(cdromXML); err != nil {
			t.Fatalf("ParseQcowDisksQuiet: %v", err)
		}
		for _, want := range []string{"skipping", "vm01", "cdrom"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("debug output does not mention %q, got:\n%s", want, out.String())
			}
		}
	})

	t.Run("a cdrom skip is INFO in verbose scans", func(t *testing.T) {
		out := captureTrace(t, false)
		if _, err := ParseQcowDisks(cdromXML); err != nil {
			t.Fatalf("ParseQcowDisks: %v", err)
		}
		for _, want := range []string{"skipping", "vm01", "cdrom"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("verbose output does not mention %q, got:\n%s", want, out.String())
			}
		}
	})

	t.Run("a non-qcow2 disk skip stays INFO even when quiet", func(t *testing.T) {
		out := captureTrace(t, false)
		disks, err := ParseQcowDisksQuiet(rawXML)
		if err != nil {
			t.Fatalf("ParseQcowDisksQuiet: %v", err)
		}
		if len(disks) != 0 {
			t.Fatalf("ParseQcowDisksQuiet returned %d disks, want none -- raw is not replicated", len(disks))
		}
		for _, want := range []string{"skipping", "vm02", "raw"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("INFO output does not mention %q; a silently unreplicated disk looks healthy, got:\n%s", want, out.String())
			}
		}
	})
}

// captureTrace diverts the trace package's log output into a buffer for one
// test. Colour is forced off so assertions read plain level names.
func captureTrace(t *testing.T, debug bool) *bytes.Buffer {
	t.Helper()
	t.Setenv("NO_COLOR", "1")
	trace.SetDebug(debug)
	t.Cleanup(func() { trace.SetDebug(false) })
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

// QcowFormatSpecFixture builds the format-specific block qemu-img emits for a
// qcow2 carrying the named persistent bitmaps.
func QcowFormatSpecFixture(names []string) map[string]interface{} {
	bitmaps := make([]interface{}, 0, len(names))
	for _, n := range names {
		bitmaps = append(bitmaps, map[string]interface{}{
			"flags":       []interface{}{"auto"},
			"name":        n,
			"granularity": float64(65536),
		})
	}
	return map[string]interface{}{
		"type": "qcow2",
		"data": map[string]interface{}{"compat": "1.1", "bitmaps": bitmaps},
	}
}
