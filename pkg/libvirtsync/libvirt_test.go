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
	"strconv"
	"strings"
	"testing"
	"time"

	"vmsync/pkg/disk"
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
		{"message mentions snapshot lowercase", errors.New("operation invalid: the creation of checkpoints when external snapshots exist is currently forbidden"), true},
		{"message mentions SNAPSHOT uppercase", errors.New("SNAPSHOT exists"), true},
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

func TestReplaceDomainDiskPath(t *testing.T) {
	t.Run("disk in rootSource map gets rewritten using the mapped root source", func(t *testing.T) {
		xmlStr := minimalDomainXML("testvm", "12345678-1234-1234-1234-123456789abc", "/var/lib/libvirt/images/testvm.snap1")
		rootMap := map[string]string{
			"/var/lib/libvirt/images/testvm.snap1": "/var/lib/libvirt/images/testvm.qcow2",
		}
		out, err := replaceDomainDiskPath(xmlStr, "testvm", "/mnt/target", rootMap)
		if err != nil {
			t.Fatalf("replaceDomainDiskPath() error = %v", err)
		}
		if !strings.Contains(out, `file="/mnt/target/testvm-vda-testvm.qcow2"`) {
			t.Errorf("expected rewritten path using mapped root source, prefixed with vm name and target dev, got: %s", out)
		}
	})

	t.Run("disk missing from a nil map falls back to its own live source, without panicking", func(t *testing.T) {
		xmlStr := minimalDomainXML("testvm", "12345678-1234-1234-1234-123456789abc", "/var/lib/libvirt/images/testvm.qcow2")
		out, err := replaceDomainDiskPath(xmlStr, "testvm", "/mnt/target", nil)
		if err != nil {
			t.Fatalf("replaceDomainDiskPath() error = %v", err)
		}
		if !strings.Contains(out, `file="/mnt/target/testvm-vda-testvm.qcow2"`) {
			t.Errorf("expected fallback to live source's own basename, prefixed with vm name and target dev, got: %s", out)
		}
	})

	t.Run("two vms sharing a basename resolve to distinct target paths", func(t *testing.T) {
		xmlStr := minimalDomainXML("vm1", "12345678-1234-1234-1234-123456789abc", "/var/lib/libvirt/images/vm1/disk.qcow2")
		out1, err := replaceDomainDiskPath(xmlStr, "vm1", "/mnt/target", nil)
		if err != nil {
			t.Fatalf("replaceDomainDiskPath() error = %v", err)
		}
		xmlStr2 := minimalDomainXML("vm2", "22345678-1234-1234-1234-123456789abc", "/var/lib/libvirt/images/vm2/disk.qcow2")
		out2, err := replaceDomainDiskPath(xmlStr2, "vm2", "/mnt/target", nil)
		if err != nil {
			t.Fatalf("replaceDomainDiskPath() error = %v", err)
		}
		if strings.Contains(out1, `file="/mnt/target/disk.qcow2"`) || strings.Contains(out2, `file="/mnt/target/disk.qcow2"`) {
			t.Fatalf("target path must not be the bare, colliding basename: vm1=%s vm2=%s", out1, out2)
		}
		if !strings.Contains(out1, `file="/mnt/target/vm1-vda-disk.qcow2"`) || !strings.Contains(out2, `file="/mnt/target/vm2-vda-disk.qcow2"`) {
			t.Errorf("expected distinct vm-name-prefixed target paths, got vm1=%s vm2=%s", out1, out2)
		}
	})

	t.Run("malformed xml returns an error", func(t *testing.T) {
		if _, err := replaceDomainDiskPath("not xml at all <<<", "testvm", "/mnt/target", nil); err == nil {
			t.Fatal("expected an error for malformed domain xml")
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

func TestParseMetadataValueMissingField(t *testing.T) {
	entry := buildMetadataEntry(map[string]string{MetadataFieldLastCheckpoint: "vmsync-cpt-000001"})
	if v := parseMetadataValue(entry, MetadataFieldFailureCount); v != "" {
		t.Errorf("parseMetadataValue() for an absent field = %q, want empty", v)
	}
}

func TestUpdateSyncMetadata(t *testing.T) {
	base := minimalDomainXML("testvm", "12345678-1234-1234-1234-123456789abc", "/var/lib/libvirt/images/x.qcow2")
	before := time.Now().Unix()
	out, err := UpdateSyncMetadata(base, "vmsync-cpt-000005")
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
