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

package libvirtsync

import (
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"vmsync/pkg/disk"

	"libvirt.org/go/libvirt"
)

// minimalDomainXML builds the smallest domain XML that survives
// libvirtxml.Domain's Unmarshal/Marshal round trip and satisfies
// util.IgnoreDevice (a qcow2 driver present, device type not "cdrom") --
// enough for every function under test here, none of which touch anything
// beyond devices/disks, name, uuid, os, or metadata.
func minimalDomainXML(name, uuid, sourcePath string) string {
	return `<domain type="kvm">
  <name>` + name + `</name>
  <uuid>` + uuid + `</uuid>
  <devices>
    <disk type="file" device="disk">
      <driver name="qemu" type="qcow2"/>
      <source file="` + sourcePath + `"/>
      <target dev="vda" bus="virtio"/>
    </disk>
  </devices>
</domain>`
}

// richDomainXML builds a far more feature-rich domain definition than
// minimalDomainXML: multiple disks (one with its own backingStore, one
// plain, plus a cdrom that must be correctly ignored), a network
// interface, a hostdev PCI passthrough device, a TPM, a qemu:commandline
// block, graphics/video/controller devices, CPU/features/clock elements,
// a UEFI loader/nvram pair, and a pre-existing non-vmsync metadata entry
// from some other tool. minimalDomainXML is deliberately the smallest
// thing that satisfies util.IgnoreDevice and exercises each function's
// own logic -- exactly why it can never catch the risk
// warnIfXMLElementsDropped's own doc comment describes: replaceDomainName,
// replaceDomainDiskPath, and SetMetadataFields all round-trip through
// libvirtxml.Domain's typed struct (unmarshal, mutate, marshal), and any
// element that struct doesn't model would be silently dropped with
// nothing in minimalDomainXML-based tests ever able to notice, since
// there'd be nothing there to lose in the first place. This fixture gives
// the round-trip something real to actually lose.
func richDomainXML(name, uuid, disk1Path, disk2Path string) string {
	return `<domain type="kvm" xmlns:qemu="http://libvirt.org/schemas/domain/qemu/1.0">
  <name>` + name + `</name>
  <uuid>` + uuid + `</uuid>
  <memory unit="KiB">4194304</memory>
  <currentMemory unit="KiB">4194304</currentMemory>
  <vcpu placement="static">4</vcpu>
  <os>
    <type arch="x86_64" machine="pc-q35-6.2">hvm</type>
    <loader readonly="yes" type="pflash">/usr/share/OVMF/OVMF_CODE.fd</loader>
    <nvram>/var/lib/libvirt/qemu/nvram/testvm_VARS.fd</nvram>
    <boot dev="hd"/>
  </os>
  <features>
    <acpi/>
    <apic/>
    <vmport state="off"/>
  </features>
  <cpu mode="host-passthrough" check="none" migratable="on"/>
  <clock offset="utc">
    <timer name="rtc" tickpolicy="catchup"/>
    <timer name="pit" tickpolicy="delay"/>
    <timer name="hpet" present="no"/>
  </clock>
  <on_poweroff>destroy</on_poweroff>
  <on_reboot>restart</on_reboot>
  <on_crash>destroy</on_crash>
  <devices>
    <emulator>/usr/bin/qemu-system-x86_64</emulator>
    <disk type="file" device="disk">
      <driver name="qemu" type="qcow2" discard="unmap"/>
      <source file="` + disk1Path + `"/>
      <backingStore type="file" index="1">
        <format type="qcow2"/>
        <source file="/var/lib/libvirt/images/base.qcow2"/>
        <backingStore/>
      </backingStore>
      <target dev="vda" bus="virtio"/>
      <address type="pci" domain="0x0000" bus="0x04" slot="0x00" function="0x0"/>
    </disk>
    <disk type="file" device="disk">
      <driver name="qemu" type="qcow2"/>
      <source file="` + disk2Path + `"/>
      <target dev="vdb" bus="virtio"/>
    </disk>
    <disk type="file" device="cdrom">
      <driver name="qemu" type="raw"/>
      <target dev="sda" bus="sata"/>
      <readonly/>
    </disk>
    <controller type="usb" index="0" model="qemu-xhci"/>
    <controller type="pci" index="0" model="pcie-root"/>
    <interface type="network">
      <mac address="52:54:00:12:34:56"/>
      <source network="default"/>
      <model type="virtio"/>
    </interface>
    <hostdev mode="subsystem" type="pci" managed="yes">
      <source>
        <address domain="0x0000" bus="0x01" slot="0x00" function="0x0"/>
      </source>
    </hostdev>
    <tpm model="tpm-crb">
      <backend type="emulator" version="2.0"/>
    </tpm>
    <graphics type="vnc" port="-1" autoport="yes" listen="0.0.0.0"/>
    <video>
      <model type="virtio" heads="1" primary="yes"/>
    </video>
    <memballoon model="virtio"/>
  </devices>
  <qemu:commandline>
    <qemu:arg value="-machine"/>
    <qemu:arg value="kernel_irqchip=on"/>
  </qemu:commandline>
  <metadata>
    <someother:tool xmlns:someother="http://example.org/someother/1.0">
      <someother:field>keep-me</someother:field>
    </someother:tool>
  </metadata>
</domain>`
}

func TestBuildPullBackupXML(t *testing.T) {
	disks := []disk.QcowDisk{{TargetDev: "vda"}, {TargetDev: "vdb"}}

	t.Run("full backup omits incremental element", func(t *testing.T) {
		xmlStr := buildPullBackupXML("", "", "0.0.0.0", 10809, disks)
		if strings.Contains(xmlStr, "<incremental>") {
			t.Errorf("full backup should not include an <incremental> element, got: %s", xmlStr)
		}
	})

	t.Run("incremental backup includes checkpoint name", func(t *testing.T) {
		xmlStr := buildPullBackupXML("vmsync-cpt-000001", "", "0.0.0.0", 10809, disks)
		if !strings.Contains(xmlStr, "<incremental>vmsync-cpt-000001</incremental>") {
			t.Errorf("expected <incremental> element with checkpoint name, got: %s", xmlStr)
		}
	})

	t.Run("empty bindAddr defaults to 0.0.0.0", func(t *testing.T) {
		xmlStr := buildPullBackupXML("", "", "", 10809, disks)
		if !strings.Contains(xmlStr, `name="0.0.0.0"`) {
			t.Errorf("expected default bind 0.0.0.0, got: %s", xmlStr)
		}
	})

	t.Run("non-positive port defaults to 10809", func(t *testing.T) {
		xmlStr := buildPullBackupXML("", "", "0.0.0.0", 0, disks)
		if !strings.Contains(xmlStr, `port="10809"`) {
			t.Errorf("expected default port 10809, got: %s", xmlStr)
		}
	})

	t.Run("multiple disks each get their own disk element", func(t *testing.T) {
		xmlStr := buildPullBackupXML("", "", "0.0.0.0", 10809, disks)
		if !strings.Contains(xmlStr, `name="vda"`) || !strings.Contains(xmlStr, `name="vdb"`) {
			t.Errorf("expected both disks present, got: %s", xmlStr)
		}
	})

	t.Run("exportBitmap set adds attribute", func(t *testing.T) {
		xmlStr := buildPullBackupXML("vmsync-cpt-000002", "vmsync-cpt-000001", "0.0.0.0", 10809, disks)
		if !strings.Contains(xmlStr, `exportbitmap="vmsync-cpt-000001"`) {
			t.Errorf("expected exportbitmap attribute, got: %s", xmlStr)
		}
	})

	t.Run("exportBitmap empty omits attribute", func(t *testing.T) {
		xmlStr := buildPullBackupXML("", "", "0.0.0.0", 10809, disks)
		if strings.Contains(xmlStr, "exportbitmap=") {
			t.Errorf("did not expect exportbitmap attribute, got: %s", xmlStr)
		}
	})
}

func TestIsCheckpointBlockedBySnapshot(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"libvirt's actual documented message, lowercase", errors.New("operation invalid: the creation of checkpoints when external snapshots exist is currently forbidden"), true},
		{"same message, uppercase", errors.New("OPERATION INVALID: THE CREATION OF CHECKPOINTS WHEN EXTERNAL SNAPSHOTS EXIST IS CURRENTLY FORBIDDEN"), true},
		{"singular \"external snapshot\" phrasing still matches", errors.New("checkpoint creation blocked: an external snapshot is present"), true},
		{"mentions snapshot but not checkpoint must not match", errors.New("SNAPSHOT exists"), false},
		{"mentions checkpoint but not snapshot must not match", errors.New("checkpoint already exists with that name"), false},
		{"unrelated error", errors.New("connection refused"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsCheckpointBlockedBySnapshot(tc.err); got != tc.want {
				t.Errorf("IsCheckpointBlockedBySnapshot(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestShouldRewriteDiskPaths is a pure-function regression test for the bug
// this was extracted to fix: DefineDomain used to gate the entire disk-path
// rewrite on targetDiskPath alone, so an external-snapshot source with no
// -target-disk-path set (targetDiskPath == "", rootSourceByLiveSource
// populated with a real live-to-root mapping) silently skipped the rewrite
// and left the target definition pointing at a file that was never copied.
func TestShouldRewriteDiskPaths(t *testing.T) {
	cases := []struct {
		name                   string
		targetDiskPath         string
		rootSourceByLiveSource map[string]string
		want                   bool
	}{
		{name: "both empty -- nothing to rewrite", targetDiskPath: "", rootSourceByLiveSource: nil, want: false},
		{name: "targetDiskPath set, no root map -- relocation only", targetDiskPath: "/mnt/target", rootSourceByLiveSource: nil, want: true},
		{
			name:                   "targetDiskPath empty but root map populated -- the exact external-snapshot regression",
			targetDiskPath:         "",
			rootSourceByLiveSource: map[string]string{"/images/vm.snap1": "/images/vm.qcow2"},
			want:                   true,
		},
		{
			name:                   "both set",
			targetDiskPath:         "/mnt/target",
			rootSourceByLiveSource: map[string]string{"/images/vm.snap1": "/images/vm.qcow2"},
			want:                   true,
		},
		{name: "empty (non-nil) root map behaves like nil", targetDiskPath: "", rootSourceByLiveSource: map[string]string{}, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldRewriteDiskPaths(tc.targetDiskPath, tc.rootSourceByLiveSource); got != tc.want {
				t.Errorf("shouldRewriteDiskPaths(%q, %v) = %v, want %v", tc.targetDiskPath, tc.rootSourceByLiveSource, got, tc.want)
			}
		})
	}
}

func TestReplaceDomainDiskPath(t *testing.T) {
	t.Run("disk in rootSource map gets rewritten using the mapped root source", func(t *testing.T) {
		xmlStr := minimalDomainXML("testvm", "12345678-1234-1234-1234-123456789abc", "/var/lib/libvirt/images/testvm.snap1")
		rootMap := map[string]string{
			"/var/lib/libvirt/images/testvm.snap1": "/var/lib/libvirt/images/testvm.qcow2",
		}
		out, err := replaceDomainDiskPath(xmlStr, "/mnt/target", rootMap)
		if err != nil {
			t.Fatalf("replaceDomainDiskPath() error = %v", err)
		}
		if !strings.Contains(out, `file="/mnt/target/testvm.qcow2"`) {
			t.Errorf("expected rewritten path using mapped root source, got: %s", out)
		}
	})

	t.Run("disk missing from a nil map is a hard error, not a silent fallback to its live source", func(t *testing.T) {
		xmlStr := minimalDomainXML("testvm", "12345678-1234-1234-1234-123456789abc", "/var/lib/libvirt/images/testvm.qcow2")
		if _, err := replaceDomainDiskPath(xmlStr, "/mnt/target", nil); err == nil {
			t.Fatal("expected an error for a disk missing from rootSourceByLiveSource, got nil")
		}
	})

	t.Run("malformed xml returns an error", func(t *testing.T) {
		if _, err := replaceDomainDiskPath("not xml at all <<<", "/mnt/target", nil); err == nil {
			t.Fatal("expected an error for malformed domain xml")
		}
	})

	// Regression pin for the bug where a stale <backingStore> -- describing
	// either an external snapshot's parent or a permanent linked clone's
	// shared base image, either way a file that lives on the *source* host
	// -- survived verbatim into the target's own domain definition, even
	// though the target's actual file is always a complete, standalone
	// image with no backing dependency (see this function's own doc
	// comment for why). A domain with a live <backingStore> is exactly what
	// ParseQcowDisks/disk.QcowDisk.RootSource resolve down through, so this
	// is the realistic shape, not a synthetic one.
	t.Run("backingStore is cleared even when the disk's path also gets rewritten", func(t *testing.T) {
		xmlStr := `<domain type="kvm">
  <name>testvm</name>
  <uuid>12345678-1234-1234-1234-123456789abc</uuid>
  <devices>
    <disk type="file" device="disk">
      <driver name="qemu" type="qcow2"/>
      <source file="/var/lib/libvirt/images/testvm.snap1"/>
      <backingStore type="file" index="1">
        <format type="qcow2"/>
        <source file="/var/lib/libvirt/images/testvm.qcow2"/>
        <backingStore/>
      </backingStore>
      <target dev="vda" bus="virtio"/>
    </disk>
  </devices>
</domain>`
		rootMap := map[string]string{
			"/var/lib/libvirt/images/testvm.snap1": "/var/lib/libvirt/images/testvm.qcow2",
		}
		out, err := replaceDomainDiskPath(xmlStr, "/mnt/target", rootMap)
		if err != nil {
			t.Fatalf("replaceDomainDiskPath() error = %v", err)
		}
		if !strings.Contains(out, `file="/mnt/target/testvm.qcow2"`) {
			t.Errorf("expected rewritten path using mapped root source, got: %s", out)
		}
		if strings.Contains(out, "backingStore") {
			t.Errorf("expected <backingStore> to be cleared entirely, got: %s", out)
		}
		if strings.Contains(out, "testvm.snap1") {
			t.Errorf("expected the stale source-host live path to be gone entirely, got: %s", out)
		}
	})

	// Same regression, but with no path relocation requested at all -- the
	// exact combination the DefineDomain-level fix for shouldRewriteDiskPaths
	// now guarantees reaches this function.
	t.Run("backingStore is cleared even with an empty targetDiskPath", func(t *testing.T) {
		xmlStr := `<domain type="kvm">
  <name>testvm</name>
  <uuid>12345678-1234-1234-1234-123456789abc</uuid>
  <devices>
    <disk type="file" device="disk">
      <driver name="qemu" type="qcow2"/>
      <source file="/var/lib/libvirt/images/testvm.snap1"/>
      <backingStore type="file" index="1">
        <format type="qcow2"/>
        <source file="/var/lib/libvirt/images/testvm.qcow2"/>
        <backingStore/>
      </backingStore>
      <target dev="vda" bus="virtio"/>
    </disk>
  </devices>
</domain>`
		rootMap := map[string]string{
			"/var/lib/libvirt/images/testvm.snap1": "/var/lib/libvirt/images/testvm.qcow2",
		}
		out, err := replaceDomainDiskPath(xmlStr, "", rootMap)
		if err != nil {
			t.Fatalf("replaceDomainDiskPath() error = %v", err)
		}
		if strings.Contains(out, "backingStore") {
			t.Errorf("expected <backingStore> to be cleared entirely, got: %s", out)
		}
	})

	// Regression pin for the DefineDomain bug this function's own caller had:
	// an empty targetDiskPath (no relocation requested) must NOT be treated
	// as "skip the rewrite" -- an external-snapshot source's live path still
	// needs to become its root path even when staying in the same
	// directory, since that root path is what the data copy actually wrote.
	t.Run("empty targetDiskPath still substitutes the mapped root source, without relocating", func(t *testing.T) {
		xmlStr := minimalDomainXML("testvm", "12345678-1234-1234-1234-123456789abc", "/var/lib/libvirt/images/testvm.snap1")
		rootMap := map[string]string{
			"/var/lib/libvirt/images/testvm.snap1": "/var/lib/libvirt/images/testvm.qcow2",
		}
		out, err := replaceDomainDiskPath(xmlStr, "", rootMap)
		if err != nil {
			t.Fatalf("replaceDomainDiskPath() error = %v", err)
		}
		if !strings.Contains(out, `file="/var/lib/libvirt/images/testvm.qcow2"`) {
			t.Errorf("expected the live snapshot path rewritten to its mapped root source in the same directory, got: %s", out)
		}
		if strings.Contains(out, "testvm.snap1") {
			t.Errorf("expected the stale live snapshot path to be gone entirely, got: %s", out)
		}
	})
}

func TestParseCheckpointXML(t *testing.T) {
	t.Run("valid xml with creation time and parent", func(t *testing.T) {
		desc := `<domaincheckpoint>
  <name>vmsync-cpt-000002</name>
  <creationTime>1700000000</creationTime>
  <parent><name>vmsync-cpt-000001</name></parent>
</domaincheckpoint>`
		cp := parseCheckpointXML("vmsync-cpt-000002", desc)
		if cp.Name != "vmsync-cpt-000002" {
			t.Errorf("Name = %q, want vmsync-cpt-000002", cp.Name)
		}
		if cp.Parent != "vmsync-cpt-000001" {
			t.Errorf("Parent = %q, want vmsync-cpt-000001", cp.Parent)
		}
		want := time.Unix(1700000000, 0)
		if !cp.Time.Equal(want) {
			t.Errorf("Time = %v, want %v", cp.Time, want)
		}
	})

	t.Run("malformed xml produces zero-value fields, not an error (none returned)", func(t *testing.T) {
		cp := parseCheckpointXML("vmsync-cpt-000003", "not xml at all")
		if cp.Name != "vmsync-cpt-000003" {
			t.Errorf("Name = %q, want vmsync-cpt-000003", cp.Name)
		}
		if cp.Parent != "" {
			t.Errorf("Parent = %q, want empty", cp.Parent)
		}
		if !cp.Time.IsZero() {
			t.Errorf("Time = %v, want zero value", cp.Time)
		}
	})

	t.Run("empty description", func(t *testing.T) {
		cp := parseCheckpointXML("vmsync-cpt-000004", "")
		if cp.Name != "vmsync-cpt-000004" || cp.Parent != "" || !cp.Time.IsZero() {
			t.Errorf("unexpected result for empty description: %+v", cp)
		}
	})
}

func TestReplaceDomainName(t *testing.T) {
	xmlStr := minimalDomainXML("sourcevm", "12345678-1234-1234-1234-123456789abc", "/var/lib/libvirt/images/x.qcow2")
	out, err := replaceDomainName(xmlStr, "targetvm")
	if err != nil {
		t.Fatalf("replaceDomainName() error = %v", err)
	}
	if !strings.Contains(out, "<name>targetvm</name>") {
		t.Errorf("expected new name in output, got: %s", out)
	}
	if strings.Contains(out, "<name>sourcevm</name>") {
		t.Errorf("old name should not remain, got: %s", out)
	}

	t.Run("malformed xml returns an error", func(t *testing.T) {
		if _, err := replaceDomainName("not xml", "targetvm"); err == nil {
			t.Fatal("expected an error for malformed domain xml")
		}
	})
}

func TestSetMetadataFieldsAndParseMetadata(t *testing.T) {
	base := minimalDomainXML("testvm", "12345678-1234-1234-1234-123456789abc", "/var/lib/libvirt/images/x.qcow2")

	t.Run("creates a metadata block when none exists", func(t *testing.T) {
		out, err := SetMetadataFields(base, map[string]string{
			MetadataFieldLastCheckpoint: "vmsync-cpt-000001",
		})
		if err != nil {
			t.Fatalf("SetMetadataFields() error = %v", err)
		}
		v, err := ParseMetadataField(out, MetadataFieldLastCheckpoint)
		if err != nil {
			t.Fatalf("ParseMetadataField() error = %v", err)
		}
		if v != "vmsync-cpt-000001" {
			t.Errorf("ParseMetadataField() = %q, want vmsync-cpt-000001", v)
		}
	})

	t.Run("a second call updates only the changed field, preserves the rest", func(t *testing.T) {
		withOne, err := SetMetadataFields(base, map[string]string{
			MetadataFieldLastCheckpoint: "vmsync-cpt-000001",
			MetadataFieldFailureCount:   "2",
		})
		if err != nil {
			t.Fatalf("SetMetadataFields() error = %v", err)
		}
		withTwo, err := SetMetadataFields(withOne, map[string]string{
			MetadataFieldLastCheckpoint: "vmsync-cpt-000002",
		})
		if err != nil {
			t.Fatalf("SetMetadataFields() second call error = %v", err)
		}

		checkpoint, err := ParseMetadataField(withTwo, MetadataFieldLastCheckpoint)
		if err != nil || checkpoint != "vmsync-cpt-000002" {
			t.Errorf("last_checkpoint = %q, err=%v, want vmsync-cpt-000002", checkpoint, err)
		}
		failureCount, err := ParseMetadataField(withTwo, MetadataFieldFailureCount)
		if err != nil || failureCount != "2" {
			t.Errorf("failure_count = %q, err=%v, want 2 (should be untouched)", failureCount, err)
		}
	})

	t.Run("ParseMetadata and ParseMetadataField agree", func(t *testing.T) {
		out, err := SetMetadataFields(base, map[string]string{MetadataFieldLastSync: "1700000000"})
		if err != nil {
			t.Fatalf("SetMetadataFields() error = %v", err)
		}
		v1, err1 := ParseMetadata(out, MetadataFieldLastSync)
		v2, err2 := ParseMetadataField(out, MetadataFieldLastSync)
		if err1 != nil || err2 != nil {
			t.Fatalf("unexpected errors: %v, %v", err1, err2)
		}
		if v1 != v2 || v1 != "1700000000" {
			t.Errorf("ParseMetadata=%q ParseMetadataField=%q, want both = 1700000000", v1, v2)
		}
	})

	t.Run("an unrecognized vmsync field survives untouched by a later update", func(t *testing.T) {
		// Simulates a field written by a different vmsync version (older or
		// newer) than this build, which metadataFieldOrder doesn't
		// enumerate -- SetMetadataFields's own updates map has no
		// restriction to the named MetadataField* constants, so this is a
		// realistic way to create one via the real, public API rather than
		// hand-crafting raw XML.
		withUnknown, err := SetMetadataFields(base, map[string]string{
			MetadataFieldLastCheckpoint: "vmsync-cpt-000001",
			"some_future_field":         "value-from-a-different-version",
		})
		if err != nil {
			t.Fatalf("SetMetadataFields() error = %v", err)
		}
		// A later update touching a completely different, known field must
		// not silently drop the unrecognized one -- this is the exact
		// regression: SetMetadataFields used to only ever read back fields
		// in metadataFieldOrder, so anything else vanished the moment
		// metadata was next written, contradicting its own doc comment's
		// "preserving any existing vmsync fields not mentioned in updates
		// or removeFields... untouched".
		updated, err := SetMetadataFields(withUnknown, map[string]string{
			MetadataFieldFailureCount: "1",
		})
		if err != nil {
			t.Fatalf("SetMetadataFields() second call error = %v", err)
		}
		v, err := ParseMetadataField(updated, "some_future_field")
		if err != nil {
			t.Fatalf("ParseMetadataField() error = %v", err)
		}
		if v != "value-from-a-different-version" {
			t.Errorf("unrecognized field = %q, want %q -- SetMetadataFields dropped a field it doesn't itself enumerate", v, "value-from-a-different-version")
		}
	})

	t.Run("no metadata block returns empty string, no error", func(t *testing.T) {
		v, err := ParseMetadataField(base, MetadataFieldLastCheckpoint)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if v != "" {
			t.Errorf("expected empty string for an absent field, got %q", v)
		}
	})

	t.Run("malformed xml returns an error", func(t *testing.T) {
		if _, err := SetMetadataFields("not xml", map[string]string{"x": "y"}); err == nil {
			t.Fatal("expected an error for malformed domain xml")
		}
	})
}

func TestBuildMetadataEntry(t *testing.T) {
	entry := buildMetadataEntry(map[string]string{
		MetadataFieldFailureCount:   "3",
		MetadataFieldLastCheckpoint: "vmsync-cpt-000001",
	})
	// Fixed field order (metadataFieldOrder), regardless of map iteration
	// order: last_checkpoint is written before failure_count.
	idxCheckpoint := strings.Index(entry, "vmsync:"+MetadataFieldLastCheckpoint)
	idxFailure := strings.Index(entry, "vmsync:"+MetadataFieldFailureCount)
	if idxCheckpoint == -1 || idxFailure == -1 {
		t.Fatalf("expected both fields present, got: %s", entry)
	}
	if idxCheckpoint > idxFailure {
		t.Errorf("expected last_checkpoint before failure_count (fixed field order), got: %s", entry)
	}
	if !strings.HasPrefix(entry, metadataStart) || !strings.HasSuffix(entry, metadataEnd) {
		t.Errorf("expected entry to start/end with the vmsync block markers, got: %s", entry)
	}
}

// TestBuildMetadataEntryUnknownFields covers the write-side half of the fix
// for the metadata writer silently dropping fields outside
// metadataFieldOrder: a field it doesn't itself enumerate must still be
// emitted (after every known field, sorted alphabetically among
// themselves so their own relative order is deterministic too), not
// silently omitted the way it used to be even if allMetadataFields had
// correctly read it back into the map in the first place.
func TestBuildMetadataEntryUnknownFields(t *testing.T) {
	entry := buildMetadataEntry(map[string]string{
		MetadataFieldFailureCount: "3",
		"zzz_unknown":             "z-value",
		"aaa_unknown":             "a-value",
	})
	idxFailure := strings.Index(entry, "vmsync:"+MetadataFieldFailureCount)
	idxAAA := strings.Index(entry, "vmsync:aaa_unknown")
	idxZZZ := strings.Index(entry, "vmsync:zzz_unknown")
	if idxFailure == -1 || idxAAA == -1 || idxZZZ == -1 {
		t.Fatalf("expected all three fields present, got: %s", entry)
	}
	if idxFailure > idxAAA || idxFailure > idxZZZ {
		t.Errorf("expected the known field (failure_count) before any unknown ones, got: %s", entry)
	}
	if idxAAA > idxZZZ {
		t.Errorf("expected unknown fields sorted alphabetically among themselves (aaa_unknown before zzz_unknown), got: %s", entry)
	}
	if !strings.Contains(entry, `id="a-value"`) || !strings.Contains(entry, `id="z-value"`) {
		t.Errorf("expected both unknown fields' values present, got: %s", entry)
	}
}

func TestParseMetadataValueMissingField(t *testing.T) {
	entry := buildMetadataEntry(map[string]string{MetadataFieldLastCheckpoint: "vmsync-cpt-000001"})
	if v := parseMetadataValue(entry, MetadataFieldFailureCount); v != "" {
		t.Errorf("parseMetadataValue() for an absent field = %q, want empty", v)
	}
}

// TestAllMetadataFields is the read-side regression pin for the metadata
// writer's own fix: every field actually present in a <vmsync:vmsync>
// block must come back, including ones metadataFieldOrder doesn't
// enumerate -- not just the known ones parseMetadataValue looks up one at
// a time.
func TestAllMetadataFields(t *testing.T) {
	entry := buildMetadataEntry(map[string]string{
		MetadataFieldLastCheckpoint: "vmsync-cpt-000001",
		MetadataFieldFailureCount:   "3",
		"some_future_field":         "future-value",
	})
	got := allMetadataFields(entry)
	want := map[string]string{
		MetadataFieldLastCheckpoint: "vmsync-cpt-000001",
		MetadataFieldFailureCount:   "3",
		"some_future_field":         "future-value",
	}
	if len(got) != len(want) {
		t.Fatalf("allMetadataFields() = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("allMetadataFields()[%q] = %q, want %q (full result: %v)", k, got[k], v, got)
		}
	}
}

func TestAllMetadataFieldsNoBlock(t *testing.T) {
	if got := allMetadataFields(""); len(got) != 0 {
		t.Errorf("allMetadataFields(\"\") = %v, want empty", got)
	}
}

func TestUpdateSyncMetadata(t *testing.T) {
	base := minimalDomainXML("testvm", "12345678-1234-1234-1234-123456789abc", "/var/lib/libvirt/images/x.qcow2")
	before := time.Now().Unix()
	out, err := UpdateSyncMetadata(base, "vmsync-cpt-000005", "source-host.example.org", "sourcevm")
	after := time.Now().Unix()
	if err != nil {
		t.Fatalf("UpdateSyncMetadata() error = %v", err)
	}

	checkpoint, err := ParseMetadataField(out, MetadataFieldLastCheckpoint)
	if err != nil || checkpoint != "vmsync-cpt-000005" {
		t.Errorf("last_checkpoint = %q, err=%v, want vmsync-cpt-000005", checkpoint, err)
	}

	failureCount, err := ParseMetadataField(out, MetadataFieldFailureCount)
	if err != nil || failureCount != "0" {
		t.Errorf("failure_count = %q, err=%v, want 0", failureCount, err)
	}

	lastSync, err := ParseMetadataField(out, MetadataFieldLastSync)
	if err != nil {
		t.Fatalf("ParseMetadataField(last_sync) error = %v", err)
	}
	ts, convErr := strconv.ParseInt(lastSync, 10, 64)
	if convErr != nil {
		t.Fatalf("last_sync_timestamp %q is not an integer: %v", lastSync, convErr)
	}
	if ts < before || ts > after {
		t.Errorf("last_sync_timestamp = %d, want between %d and %d", ts, before, after)
	}

	replicaSource, err := ParseMetadataField(out, MetadataFieldReplicaSource)
	if err != nil || replicaSource != "source-host.example.org:sourcevm" {
		t.Errorf("replica_source = %q, err=%v, want source-host.example.org:sourcevm", replicaSource, err)
	}
}

func TestSetMetadataFieldsRemoveFields(t *testing.T) {
	base := minimalDomainXML("testvm", "12345678-1234-1234-1234-123456789abc", "/var/lib/libvirt/images/x.qcow2")

	withTargetRole, err := SetMetadataFields(base, map[string]string{
		MetadataFieldLastCheckpoint: "vmsync-cpt-000001",
		MetadataFieldLastSync:       "1700000000",
		MetadataFieldFailureCount:   "1",
	})
	if err != nil {
		t.Fatalf("SetMetadataFields() error = %v", err)
	}

	t.Run("removeFields drops the named fields even with no updates", func(t *testing.T) {
		out, err := SetMetadataFields(withTargetRole, nil, MetadataFieldLastCheckpoint, MetadataFieldLastSync, MetadataFieldFailureCount)
		if err != nil {
			t.Fatalf("SetMetadataFields() error = %v", err)
		}
		for _, field := range []string{MetadataFieldLastCheckpoint, MetadataFieldLastSync, MetadataFieldFailureCount} {
			if v, _ := ParseMetadataField(out, field); v != "" {
				t.Errorf("%s = %q after removal, want empty", field, v)
			}
		}
	})

	t.Run("removeFields wins over updates naming the same field", func(t *testing.T) {
		out, err := SetMetadataFields(withTargetRole, map[string]string{
			MetadataFieldLastCheckpoint: "vmsync-cpt-000002",
		}, MetadataFieldLastCheckpoint)
		if err != nil {
			t.Fatalf("SetMetadataFields() error = %v", err)
		}
		if v, _ := ParseMetadataField(out, MetadataFieldLastCheckpoint); v != "" {
			t.Errorf("last_checkpoint = %q, want empty (removeFields must win over updates)", v)
		}
	})

	t.Run("fields not named in removeFields are preserved", func(t *testing.T) {
		out, err := SetMetadataFields(withTargetRole, nil, MetadataFieldLastCheckpoint)
		if err != nil {
			t.Fatalf("SetMetadataFields() error = %v", err)
		}
		if v, _ := ParseMetadataField(out, MetadataFieldFailureCount); v != "1" {
			t.Errorf("failure_count = %q, want 1 (untouched by removing a different field)", v)
		}
	})
}

// TestReplicaListContains and TestAppendReplicaTarget cover the pure
// list-manipulation logic RecordReplicaTarget depends on for deduplication
// -- directly testable without a live domain, unlike RecordReplicaTarget
// itself.
func TestReplicaListContains(t *testing.T) {
	cases := []struct {
		list  string
		entry string
		want  bool
	}{
		{list: "", entry: "target1.example.org:vm01", want: false},
		{list: "target1.example.org:vm01", entry: "target1.example.org:vm01", want: true},
		{list: "target1.example.org:vm01", entry: "target2.example.org:vm01", want: false},
		{list: "target1.example.org:vm01,target2.example.org:vm01", entry: "target2.example.org:vm01", want: true},
		{list: "target1.example.org:vm01,target2.example.org:vm01", entry: "target3.example.org:vm01", want: false},
	}
	for _, c := range cases {
		if got := replicaListContains(c.list, c.entry); got != c.want {
			t.Errorf("replicaListContains(%q, %q) = %v, want %v", c.list, c.entry, got, c.want)
		}
	}
}

func TestAppendReplicaTarget(t *testing.T) {
	t.Run("empty list becomes just the new entry", func(t *testing.T) {
		got := appendReplicaTarget("", "target1.example.org:vm01")
		if got != "target1.example.org:vm01" {
			t.Errorf("appendReplicaTarget(\"\", entry) = %q, want target1.example.org:vm01", got)
		}
	})

	t.Run("a genuinely new entry is appended", func(t *testing.T) {
		got := appendReplicaTarget("target1.example.org:vm01", "target2.example.org:vm01")
		want := "target1.example.org:vm01,target2.example.org:vm01"
		if got != want {
			t.Errorf("appendReplicaTarget() = %q, want %q", got, want)
		}
	})

	t.Run("re-appending an already-present entry does not duplicate or grow the list", func(t *testing.T) {
		list := "target1.example.org:vm01,target2.example.org:vm01"
		got := appendReplicaTarget(list, "target2.example.org:vm01")
		if got != list {
			t.Errorf("appendReplicaTarget() = %q, want unchanged %q (repeat sync to the same target must not grow the list)", got, list)
		}
	})
}

func TestReplicaEntry(t *testing.T) {
	if got := ReplicaEntry("host.example.org", "myvm"); got != "host.example.org:myvm" {
		t.Errorf("ReplicaEntry() = %q, want host.example.org:myvm", got)
	}
}

func TestStripDomainUUID(t *testing.T) {
	t.Run("removes the uuid element", func(t *testing.T) {
		xmlStr := minimalDomainXML("testvm", "12345678-1234-1234-1234-123456789abc", "/var/lib/libvirt/images/x.qcow2")
		out := stripDomainUUID(xmlStr)
		if out == "" {
			t.Fatal("stripDomainUUID() returned an empty string for valid xml")
		}
		if strings.Contains(out, "12345678-1234-1234-1234-123456789abc") {
			t.Errorf("expected the uuid to be stripped, got: %s", out)
		}
	})

	t.Run("malformed xml returns an empty string, not an error", func(t *testing.T) {
		if out := stripDomainUUID("not xml at all"); out != "" {
			t.Errorf("expected an empty string for malformed xml, got: %q", out)
		}
	})
}

func TestDetectNvramAndLoader(t *testing.T) {
	t.Run("present", func(t *testing.T) {
		xmlStr := `<domain type="kvm">
  <name>testvm</name>
  <os>
    <loader readonly="yes" type="pflash">/usr/share/OVMF/OVMF_CODE.fd</loader>
    <nvram>/var/lib/libvirt/qemu/nvram/testvm_VARS.fd</nvram>
  </os>
</domain>`
		nvram, err := DetectNvram(xmlStr)
		if err != nil {
			t.Fatalf("DetectNvram() error = %v", err)
		}
		if nvram != "/var/lib/libvirt/qemu/nvram/testvm_VARS.fd" {
			t.Errorf("DetectNvram() = %q, want the nvram path", nvram)
		}

		loader, err := DetectLoader(xmlStr)
		if err != nil {
			t.Fatalf("DetectLoader() error = %v", err)
		}
		if loader != "/usr/share/OVMF/OVMF_CODE.fd" {
			t.Errorf("DetectLoader() = %q, want the loader path", loader)
		}
	})

	t.Run("absent returns empty string, no error", func(t *testing.T) {
		xmlStr := minimalDomainXML("testvm", "12345678-1234-1234-1234-123456789abc", "/var/lib/libvirt/images/x.qcow2")
		nvram, err := DetectNvram(xmlStr)
		if err != nil || nvram != "" {
			t.Errorf("DetectNvram() = %q, err=%v, want empty/no error", nvram, err)
		}
		loader, err := DetectLoader(xmlStr)
		if err != nil || loader != "" {
			t.Errorf("DetectLoader() = %q, err=%v, want empty/no error", loader, err)
		}
	})

	t.Run("malformed xml returns an error", func(t *testing.T) {
		if _, err := DetectNvram("not xml"); err == nil {
			t.Error("DetectNvram() expected an error for malformed xml")
		}
		if _, err := DetectLoader("not xml"); err == nil {
			t.Error("DetectLoader() expected an error for malformed xml")
		}
	})
}

func TestBuildCheckpointXML(t *testing.T) {
	disks := []disk.QcowDisk{{TargetDev: "vda"}, {TargetDev: "vdb"}}

	t.Run("no parent element for a root checkpoint", func(t *testing.T) {
		xmlStr := buildCheckpointXML("vmsync-cpt-000001", "", disks)
		if strings.Contains(xmlStr, "<parent>") {
			t.Errorf("expected no <parent> element for a root checkpoint, got: %s", xmlStr)
		}
		if !strings.Contains(xmlStr, "<name>vmsync-cpt-000001</name>") {
			t.Errorf("expected the checkpoint name, got: %s", xmlStr)
		}
	})

	t.Run("parent element included when set", func(t *testing.T) {
		xmlStr := buildCheckpointXML("vmsync-cpt-000002", "vmsync-cpt-000001", disks)
		if !strings.Contains(xmlStr, "<parent><name>vmsync-cpt-000001</name></parent>") {
			t.Errorf("expected a parent element, got: %s", xmlStr)
		}
	})

	t.Run("every disk gets its own bitmap entry named after the checkpoint", func(t *testing.T) {
		xmlStr := buildCheckpointXML("vmsync-cpt-000003", "", disks)
		if !strings.Contains(xmlStr, `name="vda" checkpoint="bitmap" bitmap="vmsync-cpt-000003"`) {
			t.Errorf("expected a vda bitmap entry, got: %s", xmlStr)
		}
		if !strings.Contains(xmlStr, `name="vdb" checkpoint="bitmap" bitmap="vmsync-cpt-000003"`) {
			t.Errorf("expected a vdb bitmap entry, got: %s", xmlStr)
		}
	})
}

func TestNextCheckpointName(t *testing.T) {
	t.Run("empty existing produces the first name", func(t *testing.T) {
		name, parent, err := NextCheckpointName(nil)
		if err != nil {
			t.Fatalf("NextCheckpointName() error = %v", err)
		}
		if name != "vmsync-cpt-000001" {
			t.Errorf("name = %q, want vmsync-cpt-000001", name)
		}
		if parent != "" {
			t.Errorf("parent = %q, want empty", parent)
		}
	})

	t.Run("increments from the latest (last) existing checkpoint", func(t *testing.T) {
		existing := []Checkpoint{
			{Name: "vmsync-cpt-000001"},
			{Name: "vmsync-cpt-000002"},
		}
		name, parent, err := NextCheckpointName(existing)
		if err != nil {
			t.Fatalf("NextCheckpointName() error = %v", err)
		}
		if name != "vmsync-cpt-000003" {
			t.Errorf("name = %q, want vmsync-cpt-000003", name)
		}
		if parent != "vmsync-cpt-000002" {
			t.Errorf("parent = %q, want vmsync-cpt-000002", parent)
		}
	})

	t.Run("preserves the zero-padding width of the existing numeric suffix", func(t *testing.T) {
		existing := []Checkpoint{{Name: "vmsync-cpt-000099"}}
		name, _, err := NextCheckpointName(existing)
		if err != nil {
			t.Fatalf("NextCheckpointName() error = %v", err)
		}
		if name != "vmsync-cpt-000100" {
			t.Errorf("name = %q, want vmsync-cpt-000100 (padding preserved)", name)
		}
	})

	t.Run("a latest checkpoint name with no numeric suffix is a hard error", func(t *testing.T) {
		existing := []Checkpoint{{Name: "some-other-checkpoint"}}
		if _, _, err := NextCheckpointName(existing); err == nil {
			t.Fatal("expected an error when the latest checkpoint name has no numeric suffix")
		}
	})
}

// TestCheckpointDeletionOrder covers the invariant DeleteAllManagedCheckpoints
// depends on but never itself exercised without a live domain: a checkpoint
// with children attached can't be deleted, so deletion order must be
// newest-first -- the reverse of ListManagedCheckpoints' own oldest-first
// contract. Reversing this by mistake would permanently wedge -reinit's one
// recovery path for a broken checkpoint chain.
func TestCheckpointDeletionOrder(t *testing.T) {
	t.Run("empty input produces no deletions", func(t *testing.T) {
		got := checkpointDeletionOrder(nil)
		if len(got) != 0 {
			t.Fatalf("checkpointDeletionOrder(nil) = %v, want empty", got)
		}
	})

	t.Run("single checkpoint", func(t *testing.T) {
		got := checkpointDeletionOrder([]Checkpoint{{Name: "vmsync-cpt-000001"}})
		want := []string{"vmsync-cpt-000001"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("checkpointDeletionOrder() = %v, want %v", got, want)
		}
	})

	t.Run("multiple checkpoints delete newest-first, reversing the oldest-first input", func(t *testing.T) {
		existing := []Checkpoint{
			{Name: "vmsync-cpt-000001"},
			{Name: "vmsync-cpt-000002"},
			{Name: "vmsync-cpt-000003"},
		}
		got := checkpointDeletionOrder(existing)
		want := []string{"vmsync-cpt-000003", "vmsync-cpt-000002", "vmsync-cpt-000001"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("checkpointDeletionOrder() = %v, want %v (newest-first, so children are always deleted before their parent)", got, want)
		}
	})
}

func TestXMLElementCounts(t *testing.T) {
	t.Run("collects every distinct element name with its occurrence count, ignoring namespace prefixes", func(t *testing.T) {
		xmlStr := `<domain>
  <name>testvm</name>
  <devices>
    <hostdev/>
    <hostdev/>
  </devices>
  <qemu:commandline xmlns:qemu="http://libvirt.org/schemas/domain/qemu/1.0">
    <qemu:arg value="-foo"/>
  </qemu:commandline>
</domain>`
		got := xmlElementCounts(xmlStr)
		want := map[string]int{"domain": 1, "name": 1, "devices": 1, "hostdev": 2, "commandline": 1, "arg": 1}
		for name, count := range want {
			if got[name] != count {
				t.Errorf("xmlElementCounts(...)[%q] = %d, want %d (full result: %v)", name, got[name], count, got)
			}
		}
		if len(got) != len(want) {
			t.Errorf("xmlElementCounts(...) = %v (%d distinct names), want exactly %d", got, len(got), len(want))
		}
	})

	t.Run("malformed xml returns nil, not an empty map", func(t *testing.T) {
		if got := xmlElementCounts("not xml at all <<<"); got != nil {
			t.Errorf("xmlElementCounts(malformed) = %v, want nil", got)
		}
	})

	t.Run("empty string returns an empty, non-nil map", func(t *testing.T) {
		got := xmlElementCounts("")
		if got == nil {
			t.Fatal("xmlElementCounts(\"\") = nil, want a non-nil empty map (empty input parses fine, it just has no elements)")
		}
		if len(got) != 0 {
			t.Errorf("xmlElementCounts(\"\") = %v, want empty", got)
		}
	})
}

func TestMissingXMLElements(t *testing.T) {
	t.Run("nothing missing when every element survives the rewrite", func(t *testing.T) {
		original := `<domain><name>testvm</name><devices><disk/></devices></domain>`
		rewritten := `<domain><name>renamed</name><devices><disk/></devices></domain>`
		if got := missingXMLElements(original, rewritten); got != nil {
			t.Errorf("missingXMLElements(...) = %v, want nil (nothing missing)", got)
		}
	})

	t.Run("reports elements present in the original but dropped from the rewrite", func(t *testing.T) {
		original := `<domain><devices><hostdev/><disk/></devices><tpm/></domain>`
		rewritten := `<domain><devices><disk/></devices></domain>` // hostdev and tpm silently dropped
		got := missingXMLElements(original, rewritten)
		want := []string{"hostdev", "tpm"} // sorted
		if len(got) != len(want) {
			t.Fatalf("missingXMLElements(...) = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("missingXMLElements(...)[%d] = %q, want %q (full result: %v)", i, got[i], want[i], got)
			}
		}
	})

	t.Run("an element gained (not lost) by the rewrite is not reported as missing", func(t *testing.T) {
		original := `<domain><devices><disk/></devices></domain>`
		rewritten := `<domain><devices><disk/></devices><metadata/></domain>`
		if got := missingXMLElements(original, rewritten); got != nil {
			t.Errorf("missingXMLElements(...) = %v, want nil -- a newly added element is not a loss", got)
		}
	})

	// This is the instance-count case a pure present/absent name comparison
	// can't see: "disk" is still present in rewritten (one instance
	// survives), but a second instance was silently dropped -- e.g. one of
	// several disks vanishing from the domain definition entirely, while a
	// same-named sibling remains and would otherwise mask the loss.
	t.Run("a repeated element losing one of several instances is still reported, even though the name survives", func(t *testing.T) {
		original := `<domain><devices><disk id="a"/><disk id="b"/><disk id="c"/></devices></domain>`
		rewritten := `<domain><devices><disk id="a"/><disk id="b"/></devices></domain>`
		got := missingXMLElements(original, rewritten)
		want := []string{"disk"}
		if len(got) != len(want) || got[0] != want[0] {
			t.Errorf("missingXMLElements(...) = %v, want %v -- losing one of three <disk> instances must be reported", got, want)
		}
	})

	t.Run("a repeated element keeping the same count across the rewrite is not reported", func(t *testing.T) {
		original := `<domain><devices><disk id="a"/><disk id="b"/></devices></domain>`
		rewritten := `<domain><devices><disk id="a"/><disk id="renamed-b"/></devices></domain>`
		if got := missingXMLElements(original, rewritten); got != nil {
			t.Errorf("missingXMLElements(...) = %v, want nil -- the count of \"disk\" elements is unchanged", got)
		}
	})

	t.Run("an unparsable original or rewrite yields no report rather than a false positive", func(t *testing.T) {
		// "not xml" alone (no "<" at all) is NOT enough to exercise this --
		// encoding/xml's tokenizer is lenient about plain character data
		// with no markup, and happily reports it as zero elements rather
		// than erroring, which xmlElementCounts treats the same as a
		// genuinely empty document (not a parse failure). "<<<" is what
		// actually breaks tag syntax and triggers a real decode error.
		if got := missingXMLElements("not xml at all <<<", `<domain/>`); got != nil {
			t.Errorf("missingXMLElements(unparsable original, ...) = %v, want nil", got)
		}
		if got := missingXMLElements(`<domain><hostdev/></domain>`, "not xml at all <<<"); got != nil {
			t.Errorf("missingXMLElements(..., unparsable rewrite) = %v, want nil", got)
		}
	})

	// Regression pin: backingStore is cleared by replaceDomainDiskPath on
	// purpose, every time it's present -- it must never show up as a
	// "dropped" element, or every sync of a domain with an external
	// snapshot or linked clone would log a permanent false-positive
	// warning. A genuinely unrelated dropped element in the same document
	// must still be reported, so this isn't just suppressing everything.
	t.Run("backingStore is never reported as dropped, but an unrelated dropped element still is", func(t *testing.T) {
		original := `<domain><devices><disk><backingStore/></disk><hostdev/></devices></domain>`
		rewritten := `<domain><devices><disk/></devices></domain>`
		got := missingXMLElements(original, rewritten)
		want := []string{"hostdev"}
		if len(got) != len(want) {
			t.Fatalf("missingXMLElements(...) = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("missingXMLElements(...)[%d] = %q, want %q (full result: %v)", i, got[i], want[i], got)
			}
		}
	})
}

// The three tests below drive replaceDomainName/replaceDomainDiskPath/
// SetMetadataFields against richDomainXML instead of minimalDomainXML --
// see richDomainXML's own doc comment for why minimalDomainXML can never
// exercise the exact risk warnIfXMLElementsDropped exists to catch. Each
// uses missingXMLElements itself (already independently tested above) as
// the oracle: rather than hand-asserting that some specific element like
// <hostdev> or <qemu:commandline> individually survives -- which would
// just be re-implementing a worse version of that same check -- these
// assert on its actual verdict for a real function run against a real,
// complex fixture. If libvirtxml's vendored version genuinely fails to
// round-trip something these fixtures include, these tests will fail and
// name exactly what -- which is the point: that's a real, previously
// invisible configuration-loss bug minimalDomainXML-based tests could
// never have surfaced, not a false alarm to silence.

func TestReplaceDomainNamePreservesRichConfiguration(t *testing.T) {
	xmlStr := richDomainXML("sourcevm", "12345678-1234-1234-1234-123456789abc", "/var/lib/libvirt/images/vda.qcow2", "/var/lib/libvirt/images/vdb.qcow2")
	out, err := replaceDomainName(xmlStr, "targetvm")
	if err != nil {
		t.Fatalf("replaceDomainName() error = %v", err)
	}
	if !strings.Contains(out, "<name>targetvm</name>") {
		t.Errorf("expected new name in output, got: %s", out)
	}
	if missing := missingXMLElements(xmlStr, out); len(missing) != 0 {
		t.Errorf("replaceDomainName() against a feature-rich domain dropped element(s) %v -- the unmarshal/marshal round trip lost real configuration; full output: %s", missing, out)
	}
}

func TestReplaceDomainDiskPathPreservesRichConfiguration(t *testing.T) {
	xmlStr := richDomainXML("testvm", "12345678-1234-1234-1234-123456789abc", "/var/lib/libvirt/images/vda.qcow2", "/var/lib/libvirt/images/vdb.qcow2")
	rootMap := map[string]string{
		"/var/lib/libvirt/images/vda.qcow2": "/var/lib/libvirt/images/vda.qcow2",
		"/var/lib/libvirt/images/vdb.qcow2": "/var/lib/libvirt/images/vdb.qcow2",
	}
	out, err := replaceDomainDiskPath(xmlStr, "/mnt/target", rootMap)
	if err != nil {
		t.Fatalf("replaceDomainDiskPath() error = %v", err)
	}
	if !strings.Contains(out, `file="/mnt/target/vda.qcow2"`) {
		t.Errorf("expected first disk rewritten to /mnt/target/vda.qcow2, got: %s", out)
	}
	if !strings.Contains(out, `file="/mnt/target/vdb.qcow2"`) {
		t.Errorf("expected second disk rewritten to /mnt/target/vdb.qcow2, got: %s", out)
	}
	// backingStore itself is the element this function deliberately drops
	// (see its own doc comment) -- but it will never show up in missing
	// below, because missingXMLElements already suppresses it globally via
	// intentionallyDroppedXMLElements (added specifically so a real sync of
	// any domain with a backing chain doesn't log a permanent false-positive
	// "dropped configuration" warning on every run). "format" and "source"
	// are the visible side effects that denylist does NOT cover: this
	// fixture's cleared backingStore nests exactly one <format type="qcow2"/>
	// and one <source file=".../base.qcow2"/> (the backing file's own
	// source, distinct from disk1's own top-level <source>, which survives,
	// rewritten) -- a disk's own format is a <driver type="qcow2">
	// attribute, not this element, and <format> is specifically a
	// backingStore/mirror-job convention -- so once backingStore is gone,
	// both have nowhere left to appear as that one instance. missingXMLElements
	// compares occurrence counts (see its own doc comment), not mere
	// presence, specifically so a repeated element losing one instance is
	// still caught even though other same-named elements survive elsewhere
	// in the document -- "source" still exists four times over in the
	// rewritten output (both disks' own, the interface's, the hostdev's),
	// so a presence-only check would never have surfaced this one instance
	// going missing at all. intentionallyDroppedXMLElements deliberately
	// does NOT also suppress "format"/"source" themselves (see its own doc
	// comment) -- broadening that denylist risks masking a genuinely
	// dropped instance of either in some unrelated context -- so this
	// expected-missing list, not production code, is what accounts for it
	// here. Everything else in this fixture (hostdev, tpm, qemu:commandline,
	// network interface, graphics/video/controllers, the ignored cdrom,
	// CPU/features/clock, UEFI loader/nvram) must still be there.
	missing := missingXMLElements(xmlStr, out)
	want := []string{"format", "source"}
	if len(missing) != len(want) {
		t.Fatalf("replaceDomainDiskPath() against a feature-rich domain = missing %v, want exactly %v -- full output: %s", missing, want, out)
	}
	for i := range want {
		if missing[i] != want[i] {
			t.Errorf("replaceDomainDiskPath() against a feature-rich domain = missing %v, want exactly %v -- full output: %s", missing, want, out)
			break
		}
	}
}

func TestSetMetadataFieldsPreservesRichConfiguration(t *testing.T) {
	xmlStr := richDomainXML("testvm", "12345678-1234-1234-1234-123456789abc", "/var/lib/libvirt/images/vda.qcow2", "/var/lib/libvirt/images/vdb.qcow2")
	out, err := SetMetadataFields(xmlStr, map[string]string{
		MetadataFieldLastCheckpoint: "vmsync-cpt-000001",
	})
	if err != nil {
		t.Fatalf("SetMetadataFields() error = %v", err)
	}
	if !strings.Contains(out, "vmsync-cpt-000001") {
		t.Errorf("expected new vmsync metadata field in output, got: %s", out)
	}
	if !strings.Contains(out, "keep-me") {
		t.Errorf("expected pre-existing non-vmsync metadata from another tool to survive untouched, got: %s", out)
	}
	// SetMetadataFields never touches disks at all, so unlike
	// replaceDomainDiskPath's own test above, backingStore should survive
	// here too -- nothing in this fixture should be lost.
	if missing := missingXMLElements(xmlStr, out); len(missing) != 0 {
		t.Errorf("SetMetadataFields() against a feature-rich domain dropped element(s) %v -- the unmarshal/marshal round trip lost real configuration; full output: %s", missing, out)
	}
}

// TestDomainJobOperationName is the only part of StopBackup's new
// job-identity check that's directly testable without a live *libvirt.Domain
// (StopBackup itself needs a real GetJobStats/GetJobInfo/AbortJob call and
// stays out of scope for unit tests, same as every other live-libvirt
// function in this package).
func TestDomainJobOperationName(t *testing.T) {
	cases := []struct {
		op   libvirt.DomainJobOperationType
		want string
	}{
		{libvirt.DOMAIN_JOB_OPERATION_BACKUP, "backup"},
		{libvirt.DOMAIN_JOB_OPERATION_MIGRATION_OUT, "migration (outgoing)"},
		{libvirt.DOMAIN_JOB_OPERATION_MIGRATION_IN, "migration (incoming)"},
		{libvirt.DOMAIN_JOB_OPERATION_SAVE, "save"},
		{libvirt.DOMAIN_JOB_OPERATION_UNKNOWN, "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			if got := domainJobOperationName(tc.op); got != tc.want {
				t.Errorf("domainJobOperationName(%v) = %q, want %q", tc.op, got, tc.want)
			}
		})
	}

	t.Run("an operation constant not in the map falls back to a numeric name instead of panicking or going blank", func(t *testing.T) {
		unmapped := libvirt.DomainJobOperationType(999)
		want := "operation type 999"
		if got := domainJobOperationName(unmapped); got != want {
			t.Errorf("domainJobOperationName(999) = %q, want %q", got, want)
		}
	})
}
