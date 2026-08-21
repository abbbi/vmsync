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
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"vmsync/pkg/disk"
	"vmsync/pkg/failover"

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

func TestIsUUIDCollisionError(t *testing.T) {
	const uuid = "84b40009-eb1f-4bc5-94ac-b9bbc12c4b3f"
	domainXML := minimalDomainXML("testvm", uuid, "/var/lib/libvirt/images/x.qcow2")

	cases := []struct {
		name      string
		err       error
		domainXML string
		want      bool
	}{
		{"nil error", nil, domainXML, false},
		{"not a libvirt.Error at all", errors.New("domain already defined with uuid " + uuid), domainXML, false},
		{
			"correct code, english message containing the uuid",
			libvirt.Error{Code: libvirt.ERR_OPERATION_FAILED, Message: "domain 'testvm' is already defined with uuid " + uuid},
			domainXML, true,
		},
		{
			// The exact regression this function exists to fix: a
			// French-locale libvirtd reporting this same condition in
			// French, which the old English-substring match could never
			// recognize regardless of wrapping.
			"correct code, french message containing the uuid",
			libvirt.Error{Code: libvirt.ERR_OPERATION_FAILED, Message: "opération échouée : Le domaine 'testvm' est déjà défini avec l'uuid " + uuid},
			domainXML, true,
		},
		{
			"correct code but message references a different uuid entirely",
			libvirt.Error{Code: libvirt.ERR_OPERATION_FAILED, Message: "domain 'testvm' is already defined with uuid 00000000-0000-0000-0000-000000000000"},
			domainXML, false,
		},
		{
			"uuid present but wrong error code -- not specific enough on its own",
			libvirt.Error{Code: libvirt.ERR_OPERATION_INVALID, Message: "domain 'testvm' is already defined with uuid " + uuid},
			domainXML, false,
		},
		{
			"malformed domainXML has no uuid to check against",
			libvirt.Error{Code: libvirt.ERR_OPERATION_FAILED, Message: "domain 'testvm' is already defined with uuid " + uuid},
			"not xml at all",
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUUIDCollisionError(tc.err, tc.domainXML); got != tc.want {
				t.Errorf("isUUIDCollisionError(%v, ...) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestIsCheckpointBlockedBySnapshot(t *testing.T) {
	// CreateCheckpoint's real, exact wrapping shape ("create checkpoint %s:
	// %w") -- used below to build the same shape of error this function
	// actually receives at its only real call site, rather than only ever
	// testing bare, unwrapped errors nothing in production ever produces.
	wrapAsCreateCheckpointErr := func(inner error) error {
		return fmt.Errorf("create checkpoint %s: %w", "vmsync-cpt-000002", inner)
	}

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
		// Regression coverage for the "second condition is vacuous at the
		// real call site" bug: CreateCheckpoint always wraps its real
		// failure behind a "create checkpoint %s: %w" prefix, so the outer,
		// still-wrapped error ALWAYS contains "checkpoint" regardless of
		// what actually failed underneath -- checking it directly (instead
		// of unwrapping first) would make the "requires both terms" guard
		// degrade to just "contains external snapshot", misclassifying any
		// unrelated inner failure that happens to mention it too.
		{
			"wrapped libvirt message still matches once unwrapped",
			wrapAsCreateCheckpointErr(errors.New("operation invalid: the creation of checkpoints when external snapshots exist is currently forbidden")),
			true,
		},
		{
			"wrapped UNRELATED inner error mentioning \"external snapshot\" must not match, even though the outer wrap text contains \"checkpoint\"",
			wrapAsCreateCheckpointErr(errors.New("cannot resize disk while an external snapshot exists")),
			false,
		},
		{
			"wrapped inner error unrelated to snapshots at all must not match",
			wrapAsCreateCheckpointErr(errors.New("permission denied")),
			false,
		},
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
	out, err := UpdateSyncMetadata(base, "vmsync-cpt-000005", "source-host.example.org", "sourcevm", "", 1700000000, false)
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

// TestUpdateSyncMetadataDoesNotInheritSourceSideFields guards the fact that
// makes this function subtle: the XML it is handed is the SOURCE's, and
// DefineDomain turns whatever comes out into the TARGET's persistent
// definition. Anything source-specific left in place is thereby stamped
// onto the replica.
//
// The role is the one that bites hardest. After a direction inversion the
// new source legitimately carries replication_role=source, and inheriting
// it makes the first sync mark the new TARGET as a source -- which
// TargetRoleAllowsSync then refuses forever, advising the operator to check
// whether the URIs are reversed, immediately after they deliberately
// reversed them.
func TestUpdateSyncMetadataDoesNotInheritSourceSideFields(t *testing.T) {
	base := minimalDomainXML("testvm", "12345678-1234-1234-1234-123456789abc", "/var/lib/libvirt/images/x.qcow2")

	// A source that is itself a former promoted target, replicating onward:
	// every field here belongs to the source and to no replica of it.
	srcXML, err := SetMetadataFields(base, map[string]string{
		MetadataFieldReplicationRole: RoleSource,
		MetadataFieldReplicaTargets:  "dr01:testvm,dr02:testvm",
		MetadataFieldPromotedAt:      "1700000000",
		MetadataFieldPromotedBy:      "alice",
		MetadataFieldPromotedFrom:    "old-primary:testvm",
		MetadataFieldPromotionMode:   "forced",
	})
	if err != nil {
		t.Fatalf("building source xml: %v", err)
	}

	out, err := UpdateSyncMetadata(srcXML, "vmsync-cpt-000009", "src-host", "testvm", "", 1700000000, false)
	if err != nil {
		t.Fatalf("UpdateSyncMetadata() error = %v", err)
	}

	for _, field := range []string{
		MetadataFieldReplicationRole,
		MetadataFieldReplicaTargets,
		MetadataFieldPromotedAt,
		MetadataFieldPromotedBy,
		MetadataFieldPromotedFrom,
		MetadataFieldPromotionMode,
	} {
		if got, _ := ParseMetadataField(out, field); got != "" {
			t.Errorf("%s = %q on the target, want it stripped -- that field describes the source", field, got)
		}
	}
	// The target's own bookkeeping must still be written.
	if got, _ := ParseMetadataField(out, MetadataFieldReplicaSource); got != "src-host:testvm" {
		t.Errorf("replica_source = %q, want src-host:testvm", got)
	}
}

// TestUpdateSyncMetadataPreservesTheTargetsOwnRole: a deliberate
// -update-role=target must survive a sync. Because the new definition is
// built from the source's XML, preserving it takes an explicit write --
// before this, setting a role on a target was silently undone by the very
// next successful run.
func TestUpdateSyncMetadataPreservesTheTargetsOwnRole(t *testing.T) {
	base := minimalDomainXML("testvm", "12345678-1234-1234-1234-123456789abc", "/var/lib/libvirt/images/x.qcow2")
	// The source carries a role of its own, to prove the value written is
	// the TARGET's and not whatever the source happened to have.
	srcXML, err := SetMetadataFields(base, map[string]string{MetadataFieldReplicationRole: RoleSource})
	if err != nil {
		t.Fatalf("building source xml: %v", err)
	}

	out, err := UpdateSyncMetadata(srcXML, "vmsync-cpt-000001", "src-host", "testvm", RoleTarget, 1700000000, false)
	if err != nil {
		t.Fatalf("UpdateSyncMetadata() error = %v", err)
	}
	if got, _ := ParseMetadataField(out, MetadataFieldReplicationRole); got != RoleTarget {
		t.Errorf("replication_role = %q, want %q -- the target's own role, not the source's", got, RoleTarget)
	}
}

// TestUpdateSyncMetadataRecordsWhetherTheSourceWasStopped: this flag is the
// only evidence a promotion has that a replica is complete, so a stale one
// would let a later failover claim a verified zero it has no right to.
func TestUpdateSyncMetadataRecordsWhetherTheSourceWasStopped(t *testing.T) {
	base := minimalDomainXML("testvm", "12345678-1234-1234-1234-123456789abc", "/var/lib/libvirt/images/x.qcow2")

	stopped, err := UpdateSyncMetadata(base, "vmsync-cpt-000001", "src", "testvm", "", 1700000000, true)
	if err != nil {
		t.Fatalf("UpdateSyncMetadata: %v", err)
	}
	if got, _ := ParseMetadataField(stopped, MetadataFieldSourceStoppedAtSync); got == "" {
		t.Error("a sync taken against a stopped source recorded nothing")
	}

	// A later incremental from a RUNNING source must clear it, or the
	// replica keeps claiming a completeness it no longer has.
	running, err := UpdateSyncMetadata(stopped, "vmsync-cpt-000002", "src", "testvm", "", 1700000100, false)
	if err != nil {
		t.Fatalf("UpdateSyncMetadata: %v", err)
	}
	if got, _ := ParseMetadataField(running, MetadataFieldSourceStoppedAtSync); got != "" {
		t.Errorf("%s = %q after a sync from a running source, want it cleared", MetadataFieldSourceStoppedAtSync, got)
	}
}

// TestRoleConstantsMatchFailover keeps pkg/failover's copies of these
// strings in step with the originals here.
//
// pkg/failover duplicates them deliberately: importing this package would
// drag in libvirt and destroy the property that makes it valuable, namely
// that the rules deciding whether a production VM gets overwritten compile
// and test anywhere. The cost of that choice is exactly this risk -- one
// side renamed and the other not -- so it is paid down here, in the package
// that owns the values, rather than left to be discovered when a role stops
// being recognised in production and every sync is refused.
func TestRoleConstantsMatchFailover(t *testing.T) {
	for _, tc := range []struct{ name, here, there string }{
		{"RoleSource", RoleSource, failover.RoleSource},
		{"RoleTarget", RoleTarget, failover.RoleTarget},
		{"RolePromoted", RolePromoted, failover.RolePromoted},
		{"RolePaused", RolePaused, failover.RolePaused},
		{"replication_role", MetadataFieldReplicationRole, failover.FieldReplicationRole},
		{"replica_source", MetadataFieldReplicaSource, failover.FieldReplicaSource},
		{"replica_targets", MetadataFieldReplicaTargets, failover.FieldReplicaTargets},
		{"last_checkpoint", MetadataFieldLastCheckpoint, failover.FieldLastCheckpoint},
		{"last_sync_timestamp", MetadataFieldLastSync, failover.FieldLastSync},
		{"failure_count", MetadataFieldFailureCount, failover.FieldFailureCount},
		{"promoted_at", MetadataFieldPromotedAt, failover.FieldPromotedAt},
		{"promoted_by", MetadataFieldPromotedBy, failover.FieldPromotedBy},
		{"promoted_from", MetadataFieldPromotedFrom, failover.FieldPromotedFrom},
		{"promotion_mode", MetadataFieldPromotionMode, failover.FieldPromotionMode},
	} {
		if tc.here != tc.there {
			t.Errorf("%s: libvirtsync has %q, failover has %q -- they must be identical", tc.name, tc.here, tc.there)
		}
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

// TestTargetRoleAllowsSync is the exhaustive check on the single predicate
// standing between a scheduled sync and overwriting a domain that was
// failed over to. Both directions matter: refusing too much breaks every
// existing deployment (none of which has a role recorded), and refusing too
// little is how live data gets replaced by a stale replica.
func TestTargetRoleAllowsSync(t *testing.T) {
	allowed := []struct {
		role string
		why  string
	}{
		{"", "no role recorded -- every deployment predating this field, and the default state of any new target"},
		{RoleTarget, "explicitly marked as a replication target, the normal permitted state"},
	}
	for _, tc := range allowed {
		t.Run("allows "+tc.role, func(t *testing.T) {
			if err := TargetRoleAllowsSync(tc.role); err != nil {
				t.Errorf("TargetRoleAllowsSync(%q) = %v, want nil (%s)", tc.role, err, tc.why)
			}
		})
	}

	refused := []struct {
		role string
		why  string
	}{
		{RoleSource, "direction reversed -- syncing in would overwrite the original with its own replica"},
		{RolePromoted, "failed over to and serving live, whether or not it is running right now"},
		{RolePaused, "replication administratively suspended"},
		{"role-from-a-newer-vmsync", "unrecognized roles must fail closed, not be silently ignored"},
		{"TARGET", "role matching is exact -- a differently-cased value is not the target role"},
	}
	for _, tc := range refused {
		t.Run("refuses "+tc.role, func(t *testing.T) {
			err := TargetRoleAllowsSync(tc.role)
			if err == nil {
				t.Fatalf("TargetRoleAllowsSync(%q) = nil, want an error (%s)", tc.role, tc.why)
			}
			// The operator's way out has to be in the message: this fires
			// from a cron job whose only output is a log line.
			if !strings.Contains(err.Error(), "-update-role") {
				t.Errorf("TargetRoleAllowsSync(%q) error %q does not tell the operator how to override it", tc.role, err.Error())
			}
			if !strings.Contains(err.Error(), tc.role) {
				t.Errorf("TargetRoleAllowsSync(%q) error %q does not name the role it refused", tc.role, err.Error())
			}
		})
	}
}

func TestValidateRole(t *testing.T) {
	for _, role := range ValidRoles {
		if err := ValidateRole(role); err != nil {
			t.Errorf("ValidateRole(%q) = %v, want nil -- every value in ValidRoles must be accepted", role, err)
		}
	}
	// "" is deliberately rejected: clearing is spelled RoleNone, so an
	// unset -update-role flag can never be mistaken for "clear the field".
	for _, role := range []string{"", "Target", "unknown", "target "} {
		if err := ValidateRole(role); err == nil {
			t.Errorf("ValidateRole(%q) = nil, want an error", role)
		}
	}
}

// TestReplicationRoleRoundTripsThroughMetadata checks the field behaves
// like every other vmsync metadata field through the real read/write path
// -- including that clearing it leaves the OTHER fields intact, which is
// what makes -update-role=none safe to run on a live replication pair.
func TestReplicationRoleRoundTripsThroughMetadata(t *testing.T) {
	base := minimalDomainXML("testvm", "12345678-1234-1234-1234-123456789abc", "/var/lib/libvirt/images/x.qcow2")

	withRole, err := SetMetadataFields(base, map[string]string{
		MetadataFieldReplicationRole: RolePromoted,
		MetadataFieldLastCheckpoint:  "vmsync-cpt-000007",
		MetadataFieldFailureCount:    "0",
	})
	if err != nil {
		t.Fatalf("SetMetadataFields() error = %v", err)
	}
	if got, _ := ParseMetadataField(withRole, MetadataFieldReplicationRole); got != RolePromoted {
		t.Fatalf("replication_role = %q, want %q", got, RolePromoted)
	}

	cleared, err := SetMetadataFields(withRole, nil, MetadataFieldReplicationRole)
	if err != nil {
		t.Fatalf("SetMetadataFields() error = %v", err)
	}
	if got, _ := ParseMetadataField(cleared, MetadataFieldReplicationRole); got != "" {
		t.Errorf("replication_role = %q after clearing, want empty", got)
	}
	if got, _ := ParseMetadataField(cleared, MetadataFieldLastCheckpoint); got != "vmsync-cpt-000007" {
		t.Errorf("last_checkpoint = %q after clearing the role, want it untouched", got)
	}
	if err := TargetRoleAllowsSync(""); err != nil {
		t.Errorf("a cleared role must return the domain to the permitted state, got %v", err)
	}
}

// TestSetMetadataFieldsRejectsUnsafeFieldNames pins the validation that
// stands in for what the fixed metadataFieldOrder list used to provide for
// free: buildMetadataEntry interpolates a field name straight into the tag
// it emits ("<vmsync:" + field), and an element NAME -- unlike a value --
// has no escaping available, so an unsafe name can only ever produce
// malformed XML. Before buildMetadataEntry started emitting unrecognized
// fields (so SetMetadataFields could keep its promise to preserve fields it
// doesn't know about), such a name was silently dropped and could never
// reach the output at all.
//
// Every real caller passes a metadataField* constant, so this is a guard
// against future misuse rather than a live bug -- the point is that it fails
// loudly, at the offending key, instead of surfacing as a confusing parse
// error against the whole domain document from Marshal or DomainDefineXML.
func TestSetMetadataFieldsRejectsUnsafeFieldNames(t *testing.T) {
	base := minimalDomainXML("testvm", "12345678-1234-1234-1234-123456789abc", "/var/lib/libvirt/images/x.qcow2")

	for _, field := range []string{
		"has space",
		"angle>bracket",
		"slash/name",
		"1leading_digit",
		"quote\"name",
		"ns:colon",
		"",
	} {
		t.Run(fmt.Sprintf("rejects %q", field), func(t *testing.T) {
			out, err := SetMetadataFields(base, map[string]string{field: "value"})
			if err == nil {
				t.Fatalf("SetMetadataFields() with field name %q returned no error -- full output: %s", field, out)
			}
			// Matched against the %q-quoted form, not the raw name: that is
			// how the error embeds it, so a field containing a quote or a
			// backslash appears escaped in the message. Comparing the raw
			// string agrees only for names that happen to need no escaping,
			// which is exactly the subset that does not need checking.
			quoted := fmt.Sprintf("%q", field)
			if !strings.Contains(err.Error(), quoted) {
				t.Errorf("error %q does not name the offending field %s", err.Error(), quoted)
			}
			if out != "" {
				t.Errorf("SetMetadataFields() returned XML alongside its error, want an empty string: %s", out)
			}
		})
	}

	t.Run("accepts the field-name shapes vmsync itself uses", func(t *testing.T) {
		for _, field := range metadataFieldOrder {
			if _, err := SetMetadataFields(base, map[string]string{field: "value"}); err != nil {
				t.Errorf("SetMetadataFields() rejected its own field name %q: %v", field, err)
			}
		}
	})

	// The validation deliberately covers only caller-supplied names. A field
	// already present in the domain's metadata came from a parsed document,
	// so it is a valid XML element name by construction -- validating those
	// too would risk rejecting, and thereby destroying, metadata written by a
	// newer vmsync that this build is meant to preserve untouched.
	t.Run("an unrecognized field already in the metadata survives an unrelated update", func(t *testing.T) {
		seeded, err := SetMetadataFields(base, map[string]string{"field_from_a_newer_vmsync": "keep-me"})
		if err != nil {
			t.Fatalf("seeding SetMetadataFields() error = %v", err)
		}
		out, err := SetMetadataFields(seeded, map[string]string{MetadataFieldFailureCount: "2"})
		if err != nil {
			t.Fatalf("SetMetadataFields() error = %v", err)
		}
		if v, _ := ParseMetadataField(out, "field_from_a_newer_vmsync"); v != "keep-me" {
			t.Errorf("field_from_a_newer_vmsync = %q after an unrelated update, want %q", v, "keep-me")
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
		out, err := stripDomainUUID(xmlStr)
		if err != nil {
			t.Fatalf("stripDomainUUID() error = %v, want nil for valid xml", err)
		}
		if out == "" {
			t.Fatal("stripDomainUUID() returned an empty string for valid xml")
		}
		if strings.Contains(out, "12345678-1234-1234-1234-123456789abc") {
			t.Errorf("expected the uuid to be stripped, got: %s", out)
		}
	})

	// Regression pin: this used to silently return "" on malformed input,
	// discarding the real parse error entirely -- a caller feeding that
	// empty string straight into DomainDefineXML would see a generic,
	// misleading "empty/malformed XML" failure from libvirt with nothing
	// pointing back at the actual problem being here, not there.
	t.Run("malformed xml returns the real error, not a silently empty string", func(t *testing.T) {
		out, err := stripDomainUUID("not xml at all")
		if err == nil {
			t.Fatal("stripDomainUUID(malformed) returned a nil error, want the real parse failure")
		}
		if out != "" {
			t.Errorf("stripDomainUUID(malformed) = %q, want empty string alongside the error", out)
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

// TestCheckpointDeletionOrder covers the ordering DeleteAllManagedCheckpoints
// depends on but never itself exercises without a live domain: newest-first,
// the reverse of ListManagedCheckpoints' own oldest-first contract -- see
// checkpointDeletionOrder's own doc comment for why this is kept even though
// it isn't required by virDomainCheckpointDelete's documented behavior.
// Reversing this by mistake would still be worth catching even so: it costs
// nothing to keep deleting leaf-to-root, and this test is what would notice
// if that ordering ever silently flipped.
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
			t.Fatalf("checkpointDeletionOrder() = %v, want %v (newest-first, leaf-to-root)", got, want)
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

	// The subtree skip that keeps replaceDomainDiskPath's deliberate
	// BackingStore = nil from being reported as lost configuration -- see
	// xmlElementCounts' own doc comment. The fixture is shaped exactly like
	// real libvirt output for a disk with a backing chain: <backingStore>
	// nests its own <format>, its own <source>, and a terminating empty
	// <backingStore/>. Only the outer backingStore itself and the disk's own
	// top-level <source> may be counted; the three nested elements must not
	// contribute at all, and <format> must not appear in the result.
	t.Run("counts a backingStore element itself but nothing nested inside it", func(t *testing.T) {
		xmlStr := `<domain>
  <devices>
    <disk>
      <source file="/var/lib/libvirt/images/vda.snap1"/>
      <backingStore type="file" index="1">
        <format type="qcow2"/>
        <source file="/var/lib/libvirt/images/vda.qcow2"/>
        <backingStore/>
      </backingStore>
    </disk>
  </devices>
</domain>`
		got := xmlElementCounts(xmlStr)
		want := map[string]int{"domain": 1, "devices": 1, "disk": 1, "source": 1, "backingStore": 1}
		for name, count := range want {
			if got[name] != count {
				t.Errorf("xmlElementCounts(...)[%q] = %d, want %d (full result: %v)", name, got[name], count, got)
			}
		}
		if _, ok := got["format"]; ok {
			t.Errorf("xmlElementCounts(...) counted %q from inside a backingStore subtree, want it skipped entirely (full result: %v)", "format", got)
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
	// The populated-backingStore counterpart to the empty-<backingStore/>
	// case just below. Clearing a real backing chain takes the subtree's
	// nested <format> and <source> with it; neither may be reported, or every
	// sync of a domain with an external snapshot or a permanent linked clone
	// logs a permanent false positive -- "format" appears nowhere else in a
	// typical domain (its count falls 1 -> 0), and "source" loses exactly the
	// one instance that lived inside backingStore while the disk's own
	// survives, which the occurrence-count comparison is otherwise precisely
	// sensitive enough to notice.
	t.Run("clearing a populated backingStore reports neither it nor its nested source/format", func(t *testing.T) {
		original := `<domain><devices><disk><source file="/vm/a.snap1"/>` +
			`<backingStore type="file" index="1"><format type="qcow2"/>` +
			`<source file="/vm/a.qcow2"/><backingStore/></backingStore></disk></devices></domain>`
		rewritten := `<domain><devices><disk><source file="/mnt/target/a.qcow2"/></disk></devices></domain>`
		if got := missingXMLElements(original, rewritten); got != nil {
			t.Errorf("missingXMLElements(...) = %v, want nil -- backingStore and everything nested in it are cleared on purpose", got)
		}
	})

	// The sensitivity half of the pin above: skipping backingStore's contents
	// must not blind the check to a disk genuinely losing its OWN <source>.
	// Here the first disk's top-level source disappears while its backing
	// chain's nested one is skipped on both sides, so the only counted change
	// is the real loss -- exactly what this check exists to surface.
	t.Run("a disk losing its own source outside any backingStore is still reported", func(t *testing.T) {
		original := `<domain><devices><disk><source file="/vm/a.qcow2"/>` +
			`<backingStore type="file"><source file="/vm/base.qcow2"/></backingStore></disk>` +
			`<disk><source file="/vm/b.qcow2"/></disk></devices></domain>`
		rewritten := `<domain><devices><disk></disk><disk><source file="/vm/b.qcow2"/></disk></devices></domain>`
		got := missingXMLElements(original, rewritten)
		want := []string{"source"}
		if len(got) != len(want) || got[0] != want[0] {
			t.Errorf("missingXMLElements(...) = %v, want %v -- a disk's own dropped <source> must still be caught", got, want)
		}
	})

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
	// NOTHING may be reported as dropped here, and that includes the whole
	// backingStore subtree this function deliberately clears. Two separate
	// mechanisms cover it, and this fixture exercises both:
	//
	//   - the "backingStore" name itself, via intentionallyDroppedXMLElements.
	//   - its CONTENTS, via xmlElementCounts skipping the subtree on both
	//     sides of the comparison. This fixture's cleared backingStore nests
	//     exactly one <format type="qcow2"/> and one
	//     <source file=".../base.qcow2"/> (the backing file's own source,
	//     distinct from disk1's own top-level <source>, which survives,
	//     rewritten). Without that skip, both would be reported on every
	//     single sync of any domain with an external snapshot or a permanent
	//     linked clone: "format" appears nowhere else in a typical domain, so
	//     its count falls 1 -> 0, and "source" -- which still exists four
	//     times over in the rewritten output (both disks' own, the
	//     interface's, the hostdev's) -- loses exactly the one instance that
	//     lived inside backingStore, which the occurrence-count comparison
	//     (see missingXMLElements' own doc comment) is otherwise precisely
	//     sensitive enough to notice.
	//
	// Skipping the subtree rather than denylisting "format"/"source" outright
	// is what keeps a genuinely dropped disk <source> -- the real loss this
	// check exists to catch -- still visible. Everything else in this fixture
	// (hostdev, tpm, qemu:commandline, network interface, graphics/video/
	// controllers, the ignored cdrom, CPU/features/clock, UEFI loader/nvram)
	// must still be there too.
	if missing := missingXMLElements(xmlStr, out); len(missing) != 0 {
		t.Errorf("replaceDomainDiskPath() against a feature-rich domain = missing %v, want none -- full output: %s", missing, out)
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
