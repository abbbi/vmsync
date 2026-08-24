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
	"maps"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"vmsync/pkg/disk"
	"vmsync/pkg/failover"

	"github.com/beevik/etree"

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
// warnIfXMLElementsDropped's own doc comment describes. Those rewrites used
// to round-trip through libvirtxml.Domain's typed struct (unmarshal,
// mutate, marshal), so any element that struct did not model was silently
// dropped, with nothing in a minimalDomainXML-based test able to notice
// because there was nothing there to lose.
//
// They now patch a parsed tree instead (see domxml.go), so the loss should
// be impossible rather than merely reported. This fixture is what proves
// it: it gives a rewrite something real to lose, and
// TestDomainRewritesArePreserving asserts it does not.
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

// fieldTag renders the opening tag buildMetadataElement emits for one field,
// so tests that care about ORDER do not have to hard-code the prefix that
// carries vmsync's namespace.
func fieldTag(field string) string {
	return "<" + metadataPrefix + ":" + field
}

func TestBuildMetadataEntry(t *testing.T) {
	entry := buildMetadataElement(map[string]string{
		MetadataFieldFailureCount:   "3",
		MetadataFieldLastCheckpoint: "vmsync-cpt-000001",
	})
	// Fixed field order (metadataFieldOrder), regardless of map iteration
	// order: last_checkpoint is written before failure_count.
	idxCheckpoint := strings.Index(entry, fieldTag(MetadataFieldLastCheckpoint))
	idxFailure := strings.Index(entry, fieldTag(MetadataFieldFailureCount))
	if idxCheckpoint == -1 || idxFailure == -1 {
		t.Fatalf("expected both fields present, got: %s", entry)
	}
	if idxCheckpoint > idxFailure {
		t.Errorf("expected last_checkpoint before failure_count (fixed field order), got: %s", entry)
	}
	if !strings.HasPrefix(entry, metadataElementStart) || !strings.HasSuffix(entry, metadataElementEnd) {
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
	entry := buildMetadataElement(map[string]string{
		MetadataFieldFailureCount: "3",
		"zzz_unknown":             "z-value",
		"aaa_unknown":             "a-value",
	})
	idxFailure := strings.Index(entry, fieldTag(MetadataFieldFailureCount))
	idxAAA := strings.Index(entry, fieldTag("aaa_unknown"))
	idxZZZ := strings.Index(entry, fieldTag("zzz_unknown"))
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
	entry := buildMetadataElement(map[string]string{MetadataFieldLastCheckpoint: "vmsync-cpt-000001"})
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
	entry := buildMetadataElement(map[string]string{
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
		// A fence this source armed when IT was promoted. Inheriting it
		// would hand every replica a token authorising a shutdown of a
		// host that has nothing to do with them.
		MetadataFieldFenceID:      "f7c1e4a2",
		MetadataFieldFenceSource:  "old-primary:testvm",
		MetadataFieldFenceArmedAt: "1700000000",
		MetadataFieldFenceArmedBy: "alice",
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
		MetadataFieldFenceID,
		MetadataFieldFenceSource,
		MetadataFieldFenceArmedAt,
		MetadataFieldFenceArmedBy,
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

// TestDomainRewritesArePreserving is the guarantee the whole patching
// approach exists for: the XML handed to DomainDefineXML is the source's
// document with targeted edits, not a reconstruction.
//
// The failure this prevents is not cosmetic. The replica's definition is
// what boots when the replica is promoted, so an element dropped here is a
// DR failure discovered at the worst possible moment -- and silently, since
// the sync itself succeeds.
func TestDomainRewritesArePreserving(t *testing.T) {
	const (
		disk1 = "/var/lib/libvirt/images/vda.qcow2"
		disk2 = "/var/lib/libvirt/images/vdb.qcow2"
		uuid  = "4dea22b3-1d52-d8f3-2516-782e98ab3fa0"
	)
	src := richDomainXML("web01", uuid, disk1, disk2)

	out, err := replaceDomainName(src, "web01-replica")
	if err != nil {
		t.Fatalf("replaceDomainName: %v", err)
	}
	out, err = stripDomainUUID(out)
	if err != nil {
		t.Fatalf("stripDomainUUID: %v", err)
	}
	// disk1 sits on a backing chain, so its resolved root is the base file
	// the copy actually wrote under; disk2 is flat and resolves to itself.
	out, err = replaceDomainDiskPath(out, "/replicas", map[string]string{
		disk1: disk1,
		disk2: disk2,
	})
	if err != nil {
		t.Fatalf("replaceDomainDiskPath: %v", err)
	}
	out, err = SetMetadataFields(out, map[string]string{
		MetadataFieldReplicationRole: RoleTarget,
		MetadataFieldLastCheckpoint:  "vmsync-cpt-000003",
	})
	if err != nil {
		t.Fatalf("SetMetadataFields: %v", err)
	}

	// Nothing lost. backingStore is removed on purpose, so it is excluded
	// from the comparison the same way the production check excludes it.
	// uuid is the one deliberate loss in this chain -- stripDomainUUID
	// removes it so libvirt assigns the replica a fresh one. It is NOT in
	// intentionallyDroppedXMLElements on purpose: nothing in production
	// drops a uuid, so keeping the global tripwire strict about it is worth
	// more than the convenience of a blanket exclusion here.
	for _, name := range missingXMLElements(src, out) {
		if name == "uuid" {
			continue
		}
		t.Errorf("element lost through the rewrite chain: %s", name)
	}

	// The prefixed namespace is what a typed round-trip mangled worst, and
	// what libvirt actually reads to apply qemu passthrough arguments.
	for _, want := range []string{
		`xmlns:qemu=`, `<qemu:commandline>`, `<qemu:arg`,
		`<hostdev`, `<tpm`, `<nvram>`, `<someother:field>keep-me`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rewritten domain no longer contains %q:\n%s", want, out)
		}
	}

	// And the edits actually happened.
	if !strings.Contains(out, "web01-replica") {
		t.Error("the domain was not renamed")
	}
	if strings.Contains(out, uuid) {
		t.Error("the uuid was not stripped; libvirt would refuse a duplicate")
	}
	if !strings.Contains(out, "/replicas/vda.qcow2") || !strings.Contains(out, "/replicas/vdb.qcow2") {
		t.Errorf("a replicated disk path was not rewritten:\n%s", out)
	}
	if strings.Contains(out, "/var/lib/libvirt/images/base.qcow2") {
		t.Error("the backing chain survived; the target has no such file")
	}
	if got, _ := ParseMetadataField(out, MetadataFieldReplicationRole); got != RoleTarget {
		t.Errorf("replication_role = %q, want %q", got, RoleTarget)
	}
}

// TestReplaceDomainDiskPathRefusesAnUnresolvedDisk: writing a live overlay
// path into the target's definition would point the replica at a file that
// was never copied there.
func TestReplaceDomainDiskPathRefusesAnUnresolvedDisk(t *testing.T) {
	src := richDomainXML("web01", "4dea22b3-1d52-d8f3-2516-782e98ab3fa0",
		"/var/lib/libvirt/images/vda.qcow2", "/var/lib/libvirt/images/vdb.qcow2")
	if _, err := replaceDomainDiskPath(src, "/replicas", map[string]string{}); err == nil {
		t.Fatal("accepted a replicated disk with no resolved root source")
	}
}

// TestMergeMetadataFields covers the field-level merge that replaced the
// whole-domain round-trip. Same contract as SetMetadataFields -- preserve
// what you were not told about, removals win over updates -- but with
// nothing but vmsync's own fields in scope.
func TestMergeMetadataFields(t *testing.T) {
	// From nothing.
	got, err := mergeMetadataFields(nil, map[string]string{
		MetadataFieldReplicationRole: RoleTarget,
		MetadataFieldFailureCount:    "0",
	})
	if err != nil {
		t.Fatalf("mergeMetadataFields: %v", err)
	}
	if got[MetadataFieldReplicationRole] != RoleTarget || got[MetadataFieldFailureCount] != "0" {
		t.Fatalf("fields = %v", got)
	}

	// An update must leave untouched fields alone -- including one this
	// build does not know about, which is how a newer vmsync's metadata
	// survives an older one writing to the same domain.
	existing := map[string]string{
		MetadataFieldReplicationRole: RoleTarget,
		MetadataFieldLastCheckpoint:  "vmsync-cpt-000007",
		"invented_by_a_newer_build":  "keep me",
	}
	got, err = mergeMetadataFields(existing, map[string]string{MetadataFieldFailureCount: "3"})
	if err != nil {
		t.Fatalf("mergeMetadataFields: %v", err)
	}
	if got[MetadataFieldLastCheckpoint] != "vmsync-cpt-000007" {
		t.Error("an untouched field was lost")
	}
	if got["invented_by_a_newer_build"] != "keep me" {
		t.Error("a field this build does not model was dropped")
	}
	if got[MetadataFieldFailureCount] != "3" {
		t.Error("the update did not apply")
	}
	// The input map belongs to the caller and must come back unchanged --
	// SetDomainMetadataFields compares the merge against it to decide whether
	// there is anything to write at all.
	if _, mutated := existing[MetadataFieldFailureCount]; mutated {
		t.Error("the merge wrote into the caller's field map, so the no-op check can never fire")
	}

	// Removals win over updates, so "set these, drop those" needs no
	// ordering discipline from the caller.
	got, err = mergeMetadataFields(got,
		map[string]string{MetadataFieldFailureCount: "9"}, MetadataFieldFailureCount)
	if err != nil {
		t.Fatalf("mergeMetadataFields: %v", err)
	}
	if v := got[MetadataFieldFailureCount]; v != "" {
		t.Errorf("failure_count = %q, want it removed", v)
	}

	// Emptying every field yields nothing to write, which is how the element
	// gets dropped rather than left as a husk.
	got, err = mergeMetadataFields(map[string]string{MetadataFieldFailureCount: "1"},
		nil, MetadataFieldFailureCount)
	if err != nil {
		t.Fatalf("mergeMetadataFields: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("fields = %v, want empty once no fields remain", got)
	}

	// An unsafe field name is refused here exactly as SetMetadataFields
	// refuses it: the renderers interpolate names straight into a tag.
	if _, err := mergeMetadataFields(nil, map[string]string{"bad name": "x"}); err == nil {
		t.Error("accepted a field name that cannot be an XML element")
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
		{"fence_id", MetadataFieldFenceID, failover.FieldFenceID},
		{"fence_source", MetadataFieldFenceSource, failover.FieldFenceSource},
		{"fence_armed_at", MetadataFieldFenceArmedAt, failover.FieldFenceArmedAt},
		{"fence_armed_by", MetadataFieldFenceArmedBy, failover.FieldFenceArmedBy},
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
// it emits (metadataPrefix + ":" + field), and an element NAME -- unlike a value --
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

// --- vmsync's metadata element, and what libvirt does to it -----------------
//
// The tests below are pinned to a fragment read off a real domain, and to a
// model of libvirt's own set/get path transcribed from its source. Between
// them they cover the bug that made -reinit-after-failures never reach its
// threshold, and the two failed attempts at fixing it.

// theObservedMangling is what virDomainGetMetadata returned for a domain
// whose metadata had been written with a default namespace declaration.
// Verbatim, because its value is that it is real: the default declaration has
// become a prefixed one that nothing uses, the tag is bare, and the field is
// in no namespace at all.
const theObservedMangling = `<vmsync xmlns:vmsync="` + metadataNamespace + `">
  <failure_count id="1"/>
</vmsync>`

// libvirtNS models a namespace scope for the two functions below: etree keeps
// prefixes as literal text and resolves nothing, so resolution has to happen
// here, exactly as libxml2 would do it.
type libvirtNS struct {
	def      string
	prefixes map[string]string
}

func (s libvirtNS) extend(el *etree.Element) libvirtNS {
	out := libvirtNS{def: s.def, prefixes: map[string]string{}}
	maps.Copy(out.prefixes, s.prefixes)
	for _, a := range el.Attr {
		if a.Space == "" && a.Key == "xmlns" {
			out.def = a.Value
		}
		if a.Space == "xmlns" {
			out.prefixes[a.Key] = a.Value
		}
	}
	return out
}

func (s libvirtNS) href(el *etree.Element) string {
	if el.Space == "" {
		return s.def
	}
	return s.prefixes[el.Space]
}

func walkWithNS(el *etree.Element, scope libvirtNS, fn func(*etree.Element, libvirtNS)) {
	inner := scope.extend(el)
	fn(el, inner)
	for _, c := range el.ChildElements() {
		walkWithNS(c, inner, fn)
	}
}

// modelSetMetadata models virDomainSetMetadata for VIR_DOMAIN_METADATA_ELEMENT
// and returns the element as libvirt would store it. From virxml.c:
//
//	virXMLInjectNamespace:
//	    if (!(ns = xmlNewNs(node, uri, key)))    -> "failed to create a new
//	                                                XML namespace"
//	    virXMLForeachNode(node, virXMLAddElementNamespace, ns);
//	virXMLAddElementNamespace:
//	    if (!node->ns) xmlSetNs(node, ns);
//
// xmlNewNs rejects a duplicate PREFIX and never compares hrefs, which is why
// the fragment may declare the uri under any prefix but not under `vmsync` --
// and why libvirt binds every element of a fragment that binds none itself.
func modelSetMetadata(t *testing.T, fragment string) (string, error) {
	t.Helper()
	doc := etree.NewDocument()
	if err := doc.ReadFromString(fragment); err != nil {
		t.Fatalf("model: parse fragment: %v", err)
	}
	root := doc.Root()

	for _, a := range root.Attr {
		if a.Space == "xmlns" && a.Key == metadataPrefix {
			return "", fmt.Errorf("internal error: failed to create a new XML namespace")
		}
	}
	root.CreateAttr("xmlns:"+metadataPrefix, metadataNamespace)
	walkWithNS(root, libvirtNS{prefixes: map[string]string{}}, func(el *etree.Element, scope libvirtNS) {
		if scope.href(el) == "" {
			el.Space = metadataPrefix
		}
	})

	out, err := doc.WriteToString()
	if err != nil {
		t.Fatalf("model: serialise: %v", err)
	}
	return strings.TrimSpace(out), nil
}

// modelGetMetadata models virXMLExtractNamespaceXML, which is where the damage
// happens. From virxml.c:
//
//	virXMLForeachNode(nodeCopy, virXMLRemoveElementNamespace, uri);
//	for (actualNs = nodeCopy->nsDef; actualNs; actualNs = actualNs->next) {
//	    if (STREQ_NULLABLE(actualNs->href, uri)) { ...unlink...; break; }
//
// Every element in the uri is unbound, and exactly ONE declaration of it is
// removed. A fragment that declared the uri itself therefore leaves libvirt's
// own declaration behind, bound to nothing.
func modelGetMetadata(t *testing.T, stored string) string {
	t.Helper()
	doc := etree.NewDocument()
	if err := doc.ReadFromString(stored); err != nil {
		t.Fatalf("model: parse stored: %v", err)
	}
	root := doc.Root()

	walkWithNS(root, libvirtNS{prefixes: map[string]string{}}, func(el *etree.Element, scope libvirtNS) {
		if scope.href(el) == metadataNamespace {
			el.Space = ""
		}
	})

	kept := make([]etree.Attr, 0, len(root.Attr))
	removed := false
	for _, a := range root.Attr {
		isDecl := a.Value == metadataNamespace &&
			(a.Space == "xmlns" || (a.Space == "" && a.Key == "xmlns"))
		if isDecl && !removed {
			removed = true
			continue
		}
		kept = append(kept, a)
	}
	root.Attr = kept

	out, err := doc.WriteToString()
	if err != nil {
		t.Fatalf("model: serialise: %v", err)
	}
	return strings.TrimSpace(out)
}

// The model has to reproduce the fragment that was actually observed, from
// the shape that actually produced it, or it is not a model of anything.
func TestModelReproducesTheFragmentReadOffARealDomain(t *testing.T) {
	defaultNS := `<vmsync xmlns="` + metadataNamespace + `">` + "\n  " +
		`<failure_count id="1"/>` + "\n" + `</vmsync>`

	stored, err := modelSetMetadata(t, defaultNS)
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if got := modelGetMetadata(t, stored); got != theObservedMangling {
		t.Fatalf("the model does not reproduce the real fragment.\n got: %s\nwant: %s", got, theObservedMangling)
	}
}

// The three write shapes that do not work, and the one that does.
func TestOnlyANakedFragmentSurvivesTheMetadataAPI(t *testing.T) {
	t.Run("declaring the vmsync prefix is rejected outright", func(t *testing.T) {
		// The original shape. xmlNewNs returns NULL for a duplicate prefix,
		// so promotion, role changes, failure counting and replica_targets
		// all failed on this one.
		legacy := `<vmsync:vmsync xmlns:vmsync="` + metadataNamespace + `">` +
			`<vmsync:failure_count id="1"/></vmsync:vmsync>`
		if _, err := modelSetMetadata(t, legacy); err == nil {
			t.Fatal("expected the write to be refused")
		}
	})

	for name, fragment := range map[string]string{
		"a default declaration": `<vmsync xmlns="` + metadataNamespace + `">` + "\n  " +
			`<failure_count id="1"/>` + "\n" + `</vmsync>`,
		"a separate prefix": `<vms:vmsync xmlns:vms="` + metadataNamespace + `">` + "\n  " +
			`<vms:failure_count id="1"/>` + "\n" + `</vms:vmsync>`,
	} {
		t.Run(name+" is accepted but comes back stripped", func(t *testing.T) {
			stored, err := modelSetMetadata(t, fragment)
			if err != nil {
				t.Fatalf("set: %v", err)
			}
			if got := modelGetMetadata(t, stored); got != theObservedMangling {
				t.Fatalf("expected the same mangling either way.\n got: %s\nwant: %s", got, theObservedMangling)
			}
		})
	}

	t.Run("declaring nothing round trips byte for byte", func(t *testing.T) {
		fields := map[string]string{
			MetadataFieldReplicationRole: RoleTarget,
			MetadataFieldFailureCount:    "1",
		}
		fragment := buildMetadataFragment(fields)
		stored, err := modelSetMetadata(t, fragment)
		if err != nil {
			t.Fatalf("set: %v", err)
		}
		if got := modelGetMetadata(t, stored); got != fragment {
			t.Fatalf("not byte-identical.\nsent: %s\nback: %s", fragment, got)
		}
		// And what libvirt stores is exactly what the domain-document writer
		// emits: one on-disk spelling, reached by two different APIs.
		if want := buildMetadataElement(fields); stored != want {
			t.Fatalf("the stored form diverges from the document writer's.\nstored: %s\n  want: %s", stored, want)
		}
	})
}

// Every spelling a domain can be carrying, because a domain carries whichever
// was current when it was last written -- and because two intermediate builds
// wrote shapes libvirt then stripped.
var everyMetadataSpelling = map[string]string{
	"the form both writers produce now": `<vmsync:vmsync xmlns:vmsync="` + metadataNamespace + `">` +
		`<vmsync:replication_role id="target"/><vmsync:last_checkpoint id="cpt-1"/></vmsync:vmsync>`,
	"a default declaration, from the first attempt at the prefix collision": `<vmsync xmlns="` + metadataNamespace + `">` +
		`<replication_role id="target"/><last_checkpoint id="cpt-1"/></vmsync>`,
	"a separate prefix, from the second attempt": `<vms:vmsync xmlns:vms="` + metadataNamespace + `">` +
		`<vms:replication_role id="target"/><vms:last_checkpoint id="cpt-1"/></vms:vmsync>`,
	"what libvirt handed back for both of those": `<vmsync xmlns:vmsync="` + metadataNamespace + `">` +
		`<replication_role id="target"/><last_checkpoint id="cpt-1"/></vmsync>`,
	"a mixture, from a domain half-rewritten by two versions": `<vmsync:vmsync xmlns:vmsync="` + metadataNamespace + `">` +
		`<vmsync:replication_role id="target"/><last_checkpoint id="cpt-1"/></vmsync:vmsync>`,
	// ParseMetadata is handed the raw inner XML of <metadata>, so a
	// declaration that ended up on <domain> is not in the text at all.
	"the declaration hoisted onto an ancestor, out of the fragment": `<vmsync:vmsync>` +
		`<vmsync:replication_role id="target"/><vmsync:last_checkpoint id="cpt-1"/></vmsync:vmsync>`,
	"both declarations, as libvirt holds it mid-flight": `<vmsync:vmsync xmlns="` + metadataNamespace + `" xmlns:vmsync="` + metadataNamespace + `">` +
		`<replication_role id="target"/><last_checkpoint id="cpt-1"/></vmsync:vmsync>`,
}

func TestDocumentReaderAcceptsEverySpelling(t *testing.T) {
	for name, frag := range everyMetadataSpelling {
		t.Run(name, func(t *testing.T) {
			got := allMetadataFields(frag)
			if got[MetadataFieldReplicationRole] != RoleTarget || got[MetadataFieldLastCheckpoint] != "cpt-1" {
				t.Errorf("allMetadataFields = %v, want both fields", got)
			}
			if v := parseMetadataValue(frag, MetadataFieldReplicationRole); v != RoleTarget {
				t.Errorf("parseMetadataValue = %q, want %q", v, RoleTarget)
			}
		})
	}
}

// The fragment reader takes everything the document reader takes, plus the
// naked form -- which the document reader is right to refuse and this one
// must not, because libvirt strips the evidence on the way out.
func TestFragmentReaderAlsoAcceptsTheNakedForm(t *testing.T) {
	all := maps.Clone(everyMetadataSpelling)
	all["the naked form virDomainGetMetadata returns now"] = `<vmsync>` +
		`<replication_role id="target"/><last_checkpoint id="cpt-1"/></vmsync>`
	for name, frag := range all {
		t.Run(name, func(t *testing.T) {
			got, err := metadataFieldsFromFragment(frag)
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if got[MetadataFieldReplicationRole] != RoleTarget || got[MetadataFieldLastCheckpoint] != "cpt-1" {
				t.Errorf("fields = %v, want both", got)
			}
		})
	}
}

// The DOCUMENT reader must refuse an unmarked element. It is handed a whole
// <metadata> body, and on an ordinary host the first child of that is
// libosinfo's block -- whose <libosinfo:os id="..."/> would be harvested as a
// vmsync field and then written into vmsync's own element by the next merge,
// on the source, and onto every replica made from it afterwards.
func TestDocumentReaderRefusesEverythingThatIsNotOurs(t *testing.T) {
	for name, frag := range map[string]string{
		"a bare <vmsync>, which could be anybody's": `<vmsync><failure_count id="99"/></vmsync>`,
		"a <vmsync> in somebody else's namespace":   `<vmsync xmlns="http://example.org/other"><failure_count id="99"/></vmsync>`,
		"another tool's block":                      `<foo:bar xmlns:foo="http://example.org/foo"><failure_count id="99"/></foo:bar>`,
		"libosinfo, as it appears on any ordinary host": `<libosinfo:libosinfo xmlns:libosinfo="http://libosinfo.org/xmlns/libvirt/domain/1.0">` +
			`<libosinfo:os id="http://fedoraproject.org/fedora/38"/></libosinfo:libosinfo>`,
	} {
		t.Run(name, func(t *testing.T) {
			if got := allMetadataFields(frag); len(got) != 0 {
				t.Errorf("allMetadataFields = %v, want nothing -- this is not vmsync's metadata", got)
			}
		})
	}
}

// A neighbouring block must contribute nothing, on either side of ours.
func TestNeighbouringBlocksAreNotHarvested(t *testing.T) {
	libosinfo := `<libosinfo:libosinfo xmlns:libosinfo="http://libosinfo.org/xmlns/libvirt/domain/1.0">` +
		`<libosinfo:os id="http://fedoraproject.org/fedora/38"/></libosinfo:libosinfo>`
	ours := `<vmsync xmlns:vmsync="` + metadataNamespace + `"><failure_count id="3"/></vmsync>`

	for name, body := range map[string]string{
		"libosinfo first": libosinfo + ours,
		"ours first":      ours + libosinfo,
	} {
		t.Run(name, func(t *testing.T) {
			got := allMetadataFields(body)
			if len(got) != 1 || got[MetadataFieldFailureCount] != "3" {
				t.Errorf("allMetadataFields = %v, want only failure_count=3", got)
			}
		})
	}
}

// Only DIRECT children of the container are fields. vmsync writes a flat
// list, so a nested element belongs to a structure this version does not
// write, and flattening it in beside the real fields would invent one.
func TestNestedElementsAreNotFlattenedIntoFields(t *testing.T) {
	frag := `<vmsync xmlns:vmsync="` + metadataNamespace + `">` +
		`<failure_count id="3"><inner id="99"/></failure_count></vmsync>`
	got := allMetadataFields(frag)
	if len(got) != 1 || got[MetadataFieldFailureCount] != "3" {
		t.Errorf("allMetadataFields = %v, want only failure_count=3", got)
	}
}

// A foreign id attribute is not a field: `t:id` belongs to whatever declared
// `t`, and taking it would make the value depend on attribute order.
func TestAPrefixedIdAttributeIsNotAField(t *testing.T) {
	frag := `<vmsync xmlns:vmsync="` + metadataNamespace + `" xmlns:t="http://example.org/t">` +
		`<thing t:id="X"/><failure_count id="3"/></vmsync>`
	got := allMetadataFields(frag)
	if _, ok := got["thing"]; ok {
		t.Errorf("a foreign id attribute became a field: %v", got)
	}
	if got[MetadataFieldFailureCount] != "3" {
		t.Errorf("fields = %v", got)
	}
}

// A merge must never silently drop what it could not read. Losing a failure
// count is recoverable; losing replication_role=promoted is not.
func TestMetadataFieldsFromFragmentRefusesWhatItCannotRead(t *testing.T) {
	t.Run("an element that is not vmsync's at all", func(t *testing.T) {
		frag := `<other:thing xmlns:other="http://example.org/o"><a id="1"/></other:thing>`
		if _, err := metadataFieldsFromFragment(frag); err == nil {
			t.Error("accepted a fragment it could not read")
		}
	})
	t.Run("a nested container, which would otherwise read as cleanly empty", func(t *testing.T) {
		frag := `<vmsync xmlns:vmsync="` + metadataNamespace + `"><vmsync xmlns:vmsync="` + metadataNamespace + `">` +
			`<failure_count id="5"/></vmsync></vmsync>`
		if _, err := metadataFieldsFromFragment(frag); err == nil {
			t.Error("a nested container read as an empty block, so the merge would drop what is under it")
		}
	})
	// The two legitimately-empty cases must still read cleanly: a domain that
	// has never carried vmsync metadata is the ordinary first-run state.
	t.Run("an absent element", func(t *testing.T) {
		if _, err := metadataFieldsFromFragment(""); err != nil {
			t.Errorf("a domain with no vmsync metadata must read cleanly: %v", err)
		}
	})
	t.Run("a container with no fields in it", func(t *testing.T) {
		if _, err := metadataFieldsFromFragment(`<vmsync/>`); err != nil {
			t.Errorf("an empty container must read cleanly: %v", err)
		}
	})
}

// The etree writer matches on literal prefixes, so a spelling it misses is
// not merely skipped: the rebuilt element is added anyway, and libvirt's
// virXMLNodeSanitizeNamespaces then resolves two same-namespace children by
// deleting the LATER one -- keeping the stale block and discarding the write.
func TestDocumentWriterReplacesEverySpellingRatherThanDuplicatingIt(t *testing.T) {
	base := minimalDomainXML("testvm", "12345678-1234-1234-1234-123456789abc", "/var/lib/libvirt/images/x.qcow2")

	for name, existing := range everyMetadataSpelling {
		t.Run(name, func(t *testing.T) {
			seeded := strings.Replace(base, "</domain>", "<metadata>"+existing+"</metadata>\n</domain>", 1)
			if seeded == base {
				t.Fatal("fixture drift: could not seed metadata into the domain")
			}
			out, err := SetMetadataFields(seeded, map[string]string{MetadataFieldFailureCount: "9"})
			if err != nil {
				t.Fatalf("SetMetadataFields() error = %v", err)
			}
			if n := strings.Count(out, metadataNamespace); n != 1 {
				t.Errorf("the domain carries %d vmsync metadata blocks, want exactly 1:\n%s", n, out)
			}
			if !strings.Contains(out, metadataElementStart) {
				t.Errorf("the rewrite is not in the self-binding form, which a define would delete:\n%s", out)
			}
			for field, want := range map[string]string{
				MetadataFieldReplicationRole: RoleTarget,
				MetadataFieldLastCheckpoint:  "cpt-1",
				MetadataFieldFailureCount:    "9",
			} {
				if got, _ := ParseMetadataField(out, field); got != want {
					t.Errorf("%s = %q, want %q -- the existing element was not merged:\n%s", field, got, want, out)
				}
			}
		})
	}
}

// An element holding vmsync's PREFIX for somebody else's namespace is not
// vmsync's, and must be left where it is.
func TestDocumentWriterRefusesAnImpostorHoldingOurPrefix(t *testing.T) {
	base := minimalDomainXML("testvm", "12345678-1234-1234-1234-123456789abc", "/var/lib/libvirt/images/x.qcow2")
	impostor := `<vmsync:vmsync xmlns:vmsync="http://example.org/impostor"><vmsync:failure_count id="99"/></vmsync:vmsync>`
	seeded := strings.Replace(base, "</domain>", "<metadata>"+impostor+"</metadata>\n</domain>", 1)

	out, err := SetMetadataFields(seeded, map[string]string{MetadataFieldFailureCount: "1"})
	if err != nil {
		t.Fatalf("SetMetadataFields() error = %v", err)
	}
	if !strings.Contains(out, "http://example.org/impostor") {
		t.Errorf("somebody else's block was replaced:\n%s", out)
	}
	if !strings.Contains(out, metadataNamespace) {
		t.Errorf("vmsync's own block was not added beside it:\n%s", out)
	}
}

// The bench symptom, end to end through the modelled round trip: three
// induced failures each recorded consecutive_failures=1, so
// -reinit-after-failures never reached its threshold.
func TestFailureCountIncrementsThroughTheRealRoundTrip(t *testing.T) {
	fields := map[string]string{
		MetadataFieldReplicationRole: RoleTarget,
		MetadataFieldLastCheckpoint:  "vmsync-cpt-000001",
	}
	for want := 1; want <= 3; want++ {
		stored, err := modelSetMetadata(t, buildMetadataFragment(fields))
		if err != nil {
			t.Fatalf("round %d: set: %v", want, err)
		}
		read, err := metadataFieldsFromFragment(modelGetMetadata(t, stored))
		if err != nil {
			t.Fatalf("round %d: %v", want, err)
		}
		if read[MetadataFieldReplicationRole] != RoleTarget || read[MetadataFieldLastCheckpoint] != "vmsync-cpt-000001" {
			t.Fatalf("round %d lost a field that was not being updated: %v", want, read)
		}

		current := 0
		if v := read[MetadataFieldFailureCount]; v != "" {
			n, convErr := strconv.Atoi(v)
			if convErr != nil {
				t.Fatalf("round %d: stored failure_count %q does not parse: %v", want, v, convErr)
			}
			current = n
		}
		next := current + 1
		if next != want {
			t.Fatalf("increment %d read the stored count as %d, so it recorded %d -- the threshold is never reached", want, current, next)
		}

		merged, err := mergeMetadataFields(read, map[string]string{MetadataFieldFailureCount: strconv.Itoa(next)})
		if err != nil {
			t.Fatalf("round %d: merge: %v", want, err)
		}
		if maps.Equal(merged, read) {
			t.Fatalf("round %d compared equal to what was stored and would be skipped as a no-op", want)
		}
		fields = merged
	}
}

// The "nothing changed, skip the write" check compares FIELDS. Comparing the
// serialised fragments would compare the wrong thing -- what is sent and what
// comes back are different documents even when nothing changed -- so the
// check could never fire, and every sync rewrote the SOURCE domain's
// persistent definition for nothing.
func TestUnchangedFieldsCompareEqualAcrossTheRoundTrip(t *testing.T) {
	fields := map[string]string{
		MetadataFieldReplicationRole: RoleTarget,
		MetadataFieldLastCheckpoint:  "cpt-1",
	}
	stored, err := modelSetMetadata(t, buildMetadataFragment(fields))
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	read, err := metadataFieldsFromFragment(modelGetMetadata(t, stored))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}

	unchanged, err := mergeMetadataFields(read, map[string]string{MetadataFieldReplicationRole: RoleTarget})
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(unchanged, read) {
		t.Errorf("an update that changes nothing does not compare equal: %v vs %v", unchanged, read)
	}

	changed, err := mergeMetadataFields(read, map[string]string{MetadataFieldLastCheckpoint: "cpt-2"})
	if err != nil {
		t.Fatal(err)
	}
	if maps.Equal(changed, read) {
		t.Error("a changed field compares equal, so the write would be skipped")
	}
}
