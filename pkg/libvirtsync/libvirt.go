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
	"encoding/xml"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"vmsync/pkg/disk"
	"vmsync/pkg/trace"
	"vmsync/pkg/util"

	"libvirt.org/go/libvirt"
	"libvirt.org/go/libvirtxml"
)

const CheckpointPrefix = "vmsync-cpt"

// VerifyWindowCheckpointName names the ephemeral, throwaway checkpoint
// -verify-online creates right when its compare window opens, to find out
// afterward which regions the guest wrote to during the compare (see
// CreateVerifyWindowCheckpoint). Deliberately NOT prefixed with
// CheckpointPrefix+"-": ListManagedCheckpoints (and therefore
// NextCheckpointName, and -reinit's DeleteAllManagedCheckpoints) only ever
// look at names starting with "vmsync-cpt-", so this name is permanently
// invisible to all of them, unconditionally -- regardless of whether
// cleanup ever actually runs. That matters because NextCheckpointName hard
// -fails if the checkpoint it thinks is "latest" doesn't end in a numeric
// suffix; a leaked verify-window checkpoint must never be able to reach
// that code path at all, not just usually get cleaned up in time.
const VerifyWindowCheckpointName = "vmsync-verify-window"

const (
	metadataNamespace = `http://vmsync.org/xmlns/libvirt/domain/1.0`
	metadataStart     = `<vmsync:vmsync xmlns:vmsync="` + metadataNamespace + `">`
	metadataEnd       = `</vmsync:vmsync>`

	MetadataFieldLastCheckpoint = "last_checkpoint"
	MetadataFieldLastSync       = "last_sync_timestamp"
	MetadataFieldFailureCount   = "failure_count"
)

// metadataFieldOrder fixes the field order vmsync writes its own metadata
// entries in, purely for stable/readable XML output.
var metadataFieldOrder = []string{
	MetadataFieldLastCheckpoint,
	MetadataFieldLastSync,
	MetadataFieldFailureCount,
}

var vmsyncBlockRe = regexp.MustCompile(`(?s)<vmsync:vmsync[^>]*>.*?</vmsync:vmsync>`)

type Manager struct {
	Conn *libvirt.Connect
	URI  string
}

type Checkpoint struct {
	Name   string
	Parent string
	Time   time.Time
}

func Connect(uri string) (*Manager, error) {
	conn, err := libvirt.NewConnect(uri)
	if err != nil {
		return nil, fmt.Errorf("connect libvirt %s: %w", uri, err)
	}
	return &Manager{Conn: conn, URI: uri}, nil
}

// ExternalSnapshotCountViaReconnect is ExternalSnapshotCount for a caller
// that has no open connection to sourceURI yet -- used for the
// -ignore-external-snapshot preflight check in cmd/vmsync, which runs before
// run() ever opens its own source connection.
func ExternalSnapshotCountViaReconnect(sourceURI, domainName string) (int, error) {
	mgr, err := Connect(sourceURI)
	if err != nil {
		return 0, fmt.Errorf("reconnect source libvirt: %w", err)
	}
	defer mgr.Close()
	dom, err := mgr.LookupDomain(domainName)
	if err != nil {
		return 0, fmt.Errorf("lookup domain %s on reconnect: %w", domainName, err)
	}
	defer dom.Free()
	return ExternalSnapshotCount(dom)
}

func StopBackupViaReconnect(sourceURI, domainName string) error {
	mgr, err := Connect(sourceURI)
	if err != nil {
		return fmt.Errorf("reconnect source libvirt: %w", err)
	}
	defer mgr.Close()
	dom, err := mgr.LookupDomain(domainName)
	if err != nil {
		return fmt.Errorf("lookup domain %s on reconnect: %w", domainName, err)
	}
	defer dom.Free()
	return StopBackup(dom)
}

func DeleteCheckpointViaReconnect(sourceURI, domainName, checkpointName string) error {
	mgr, err := Connect(sourceURI)
	if err != nil {
		return fmt.Errorf("reconnect source libvirt: %w", err)
	}
	defer mgr.Close()
	dom, err := mgr.LookupDomain(domainName)
	if err != nil {
		return fmt.Errorf("lookup domain %s on reconnect: %w", domainName, err)
	}
	defer dom.Free()
	return DeleteCheckpointIfExists(dom, checkpointName)
}

func (m *Manager) Close() error {
	if m == nil || m.Conn == nil {
		return nil
	}
	_, err := m.Conn.Close()
	return err
}

func (m *Manager) LookupDomain(name string) (*libvirt.Domain, error) {
	dom, err := m.Conn.LookupDomainByName(name)
	if err != nil {
		return nil, fmt.Errorf("lookup domain %s: %w", name, err)
	}
	return dom, nil
}

func DomainExists(conn *libvirt.Connect, name string) (bool, error) {
	d, err := conn.LookupDomainByName(name)
	if err != nil {
		if lvErr, ok := err.(libvirt.Error); ok && lvErr.Code == libvirt.ERR_NO_DOMAIN {
			return false, nil
		}
		return false, err
	}
	_ = d.Free()
	return true, nil
}

// DefineDomain (re)defines targetDomainName on target from sourceDomainXML.
// rootSourceByLiveSource maps each disk's live source path to its resolved
// backing-chain root file (see disk.QcowDisk.RootSource) -- passed straight
// through to replaceDomainDiskPath so the domain definition names disks the
// same way the actual data copy does; pass nil/empty if targetDiskPath is
// also empty (no disk-path rewriting requested at all).
func DefineDomain(target *Manager, targetDomainName string, sourceDomainXML string, targetDiskPath string, rootSourceByLiveSource map[string]string) error {
	exists, err := DomainExists(target.Conn, targetDomainName)
	if err != nil {
		return fmt.Errorf("check target domain existence: %w", err)
	}
	if exists {
		trace.Info("Undefining domain on target system", "vm", targetDomainName)
		d, err := target.Conn.LookupDomainByName(targetDomainName)
		if err != nil {
			return fmt.Errorf("look up existing target domain %s for undefine: %w", targetDomainName, err)
		}
		defer d.Free()
		// KEEP_NVRAM: vmsync never copies or manages a domain's NVRAM/
		// varstore file itself (see DetectNvram -- it only checks the file
		// already exists on the target and warns if not), so undefining
		// here must not delete it out from under whatever provisioned it.
		// Undefine() (no flags) unconditionally refuses to undefine any
		// domain that has an NVRAM file present at all, which is exactly
		// why this previously failed -- silently, since the error was
		// swallowed -- for every UEFI/OVMF target domain.
		if err := d.UndefineFlags(libvirt.DOMAIN_UNDEFINE_KEEP_NVRAM); err != nil {
			return fmt.Errorf("undefine existing target domain %s: %w", targetDomainName, err)
		}
	}

	// Keep source XML intact (including UUID) unless libvirt rejects duplicate UUID.
	updatedXML, err := replaceDomainName(sourceDomainXML, targetDomainName)
	if err != nil {
		return fmt.Errorf("rewrite target domain xml: %w", err)
	}

	// Keep source XML intact (including UUID) unless libvirt rejects duplicate UUID.
	if targetDiskPath != "" {
		updatedXML, err = replaceDomainDiskPath(updatedXML, targetDiskPath, rootSourceByLiveSource)
		if err != nil {
			return fmt.Errorf("rewrite target domain xml: %w", err)
		}
	}

	dom, err := target.Conn.DomainDefineXML(updatedXML)
	if err != nil {
		// Fallback for cloning into same target where another domain already uses the UUID.
		if strings.Contains(strings.ToLower(err.Error()), "already defined with uuid") {
			withoutUUID := stripDomainUUID(updatedXML)
			dom, retryErr := target.Conn.DomainDefineXML(withoutUUID)
			if retryErr != nil {
				return fmt.Errorf("define target domain after uuid fallback: %w", retryErr)
			}
			return dom.Free()
		}
		return fmt.Errorf("define target domain: %w", err)
	}
	trace.Info("Redefined target vm with new configuration", "vm", targetDomainName)
	return dom.Free()
}

func ThawFs(srcDom *libvirt.Domain, freezed bool) {
	if !freezed {
		return
	}
	if err := srcDom.FSThaw(nil, 0); err != nil {
		trace.Warning("Filesystem thaw failed", "error", err)
	} else {
		trace.Info("Successfully thawed file systems using guest agent")
	}
}

// replaceDomainDiskPath rewrites each non-ignored disk's <source file> to its
// target-side path. rootSourceByLiveSource maps a disk's live Source path (as
// currently written in domainXML) to its resolved backing-chain root file --
// see disk.QcowDisk.RootSource's own doc comment for why this distinction
// matters: the live Source can point at an external-snapshot overlay that
// was never actually copied to the target under that name, while RootSource
// is the stable base filename the sync's own data-copy path always uses.
// Without this, the domain definition and the actual replicated file could
// silently disagree on the disk's name the moment an external snapshot
// exists. A disk missing from the map falls back to its own live Source --
// shouldn't happen for anything ParseQcowDisks would also have picked up,
// since both apply the same IgnoreDevice filter, but degrades safely rather
// than panicking on a nil map lookup if it ever does.
func replaceDomainDiskPath(domainXML, targetDiskPath string, rootSourceByLiveSource map[string]string) (string, error) {
	domcfg := &libvirtxml.Domain{}
	err := domcfg.Unmarshal(domainXML)
	if err != nil {
		return "", err
	}

	for i, d := range domcfg.Devices.Disks {
		if util.IgnoreDevice(d) == true {
			continue
		}
		// IgnoreDevice only guarantees a non-nil Driver; Source/Source.File
		// are separate pointers and could still be nil for a malformed disk.
		if d.Source == nil || d.Source.File == nil {
			continue
		}

		liveSource := d.Source.File.File
		rootSource := liveSource
		if resolved, ok := rootSourceByLiveSource[liveSource]; ok {
			rootSource = resolved
		}
		domcfg.Devices.Disks[i].Source.File.File = util.SetTargetPath(targetDiskPath, rootSource)
	}

	changed, err := domcfg.Marshal()
	if err != nil {
		return "", err
	}

	return changed, nil
}

func replace(domainXML, name string) (string, error) {
	domcfg := &libvirtxml.Domain{}
	err := domcfg.Unmarshal(domainXML)
	if err != nil {
		return "", err
	}

	domcfg.Name = name

	changed, err := domcfg.Marshal()
	if err != nil {
		return "", err
	}

	return changed, nil
}

func replaceDomainName(domainXML, name string) (string, error) {
	domcfg := &libvirtxml.Domain{}
	err := domcfg.Unmarshal(domainXML)
	if err != nil {
		return "", err
	}

	domcfg.Name = name

	changed, err := domcfg.Marshal()
	if err != nil {
		return "", err
	}

	return changed, nil
}

// SetMetadataFields merges the given vmsync:field->value pairs into
// domainXML's <metadata> block, preserving any existing vmsync fields not
// mentioned in updates (and any unrelated, non-vmsync metadata some other
// tool may have added) untouched.
func SetMetadataFields(domainXML string, updates map[string]string) (string, error) {
	domcfg := &libvirtxml.Domain{}
	err := domcfg.Unmarshal(domainXML)
	if err != nil {
		return "", err
	}

	current := map[string]string{}
	if domcfg.Metadata != nil {
		for _, field := range metadataFieldOrder {
			if v := parseMetadataValue(domcfg.Metadata.XML, field); v != "" {
				current[field] = v
			}
		}
	}
	for field, value := range updates {
		current[field] = value
	}
	entry := buildMetadataEntry(current)

	if domcfg.Metadata == nil {
		domcfg.Metadata = &libvirtxml.DomainMetadata{XML: entry}
	} else if vmsyncBlockRe.MatchString(domcfg.Metadata.XML) {
		domcfg.Metadata.XML = vmsyncBlockRe.ReplaceAllLiteralString(domcfg.Metadata.XML, entry)
	} else {
		domcfg.Metadata.XML += entry
	}

	changed, err := domcfg.Marshal()
	if err != nil {
		return "", err
	}

	return changed, nil
}

// UpdateSyncMetadata records a fresh checkpoint/timestamp and resets
// failure_count to 0; called once a sync completes successfully.
func UpdateSyncMetadata(domainXML string, checkpoint string) (string, error) {
	return SetMetadataFields(domainXML, map[string]string{
		MetadataFieldLastCheckpoint: checkpoint,
		MetadataFieldLastSync:       strconv.FormatInt(time.Now().Unix(), 10),
		MetadataFieldFailureCount:   "0",
	})
}

// ReadTargetFailureCount reconnects to the target and returns the
// failure_count currently recorded in its domain metadata. Returns 0 (no
// error) if the target domain doesn't exist yet or has no such field.
func ReadTargetFailureCount(targetURI, targetDomain string) (int, error) {
	mgr, err := Connect(targetURI)
	if err != nil {
		return 0, fmt.Errorf("reconnect target libvirt: %w", err)
	}
	defer mgr.Close()

	dom, err := mgr.Conn.LookupDomainByName(targetDomain)
	if err != nil {
		return 0, nil
	}
	defer dom.Free()

	domXML, err := dom.GetXMLDesc(0)
	if err != nil {
		return 0, fmt.Errorf("read target domain xml: %w", err)
	}
	value, err := ParseMetadata(domXML, MetadataFieldFailureCount)
	if err != nil || value == "" {
		return 0, nil
	}
	count, err := strconv.Atoi(value)
	if err != nil {
		return 0, nil
	}
	return count, nil
}

// RecordTargetSyncFailure reconnects to the target, increments
// failure_count in its domain metadata (leaving the rest of its definition
// untouched) and returns the new count. A target domain that doesn't exist
// yet has nothing to record against and is treated as a no-op.
func RecordTargetSyncFailure(targetURI, targetDomain string) (int, error) {
	mgr, err := Connect(targetURI)
	if err != nil {
		return 0, fmt.Errorf("reconnect target libvirt: %w", err)
	}
	defer mgr.Close()

	dom, err := mgr.Conn.LookupDomainByName(targetDomain)
	if err != nil {
		return 0, nil
	}
	defer dom.Free()

	domXML, err := dom.GetXMLDesc(0)
	if err != nil {
		return 0, fmt.Errorf("read target domain xml: %w", err)
	}

	current := 0
	if value, err := ParseMetadata(domXML, MetadataFieldFailureCount); err == nil && value != "" {
		if n, err := strconv.Atoi(value); err == nil {
			current = n
		}
	}
	next := current + 1

	updatedXML, err := SetMetadataFields(domXML, map[string]string{
		MetadataFieldFailureCount: strconv.Itoa(next),
	})
	if err != nil {
		return 0, fmt.Errorf("update failure_count metadata: %w", err)
	}
	newDom, err := mgr.Conn.DomainDefineXML(updatedXML)
	if err != nil {
		return 0, fmt.Errorf("redefine target domain with updated failure_count: %w", err)
	}
	defer newDom.Free()

	return next, nil
}

func DetectNvram(domainXML string) (string, error) {
	domcfg := &libvirtxml.Domain{}
	err := domcfg.Unmarshal(domainXML)
	if err != nil {
		return "", err
	}
	if domcfg.OS != nil && domcfg.OS.NVRam != nil {

		nvram := domcfg.OS.NVRam
		return nvram.NVRam, nil

	}
	return "", nil
}

func DetectLoader(domainXML string) (string, error) {
	domcfg := &libvirtxml.Domain{}
	err := domcfg.Unmarshal(domainXML)
	if err != nil {
		return "", err
	}
	if domcfg.OS != nil && domcfg.OS.Loader != nil {

		loader := domcfg.OS.Loader
		return loader.Path, nil

	}
	return "", nil
}

func ParseMetadata(domainXML string, metadataField string) (string, error) {
	domcfg := &libvirtxml.Domain{}
	err := domcfg.Unmarshal(domainXML)
	if err != nil {
		return "", err
	}
	if domcfg.Metadata == nil {
		return "", nil
	}

	return parseMetadataValue(domcfg.Metadata.XML, metadataField), nil
}

func ParseMetadataField(domainXML string, field string) (string, error) {
	domcfg := &libvirtxml.Domain{}
	err := domcfg.Unmarshal(domainXML)
	if err != nil {
		return "", err
	}
	if domcfg.Metadata == nil {
		return "", nil
	}

	return parseMetadataValue(domcfg.Metadata.XML, field), nil
}

// buildMetadataEntry renders a full <vmsync:vmsync> block from the given
// field values, in the fixed order defined by metadataFieldOrder. Fields
// absent from the map are simply omitted.
func buildMetadataEntry(fields map[string]string) string {
	var b strings.Builder
	b.WriteString(metadataStart)
	for _, field := range metadataFieldOrder {
		value, ok := fields[field]
		if !ok {
			continue
		}
		b.WriteString("\n  <vmsync:")
		b.WriteString(field)
		b.WriteString(" id=\"")
		_ = xml.EscapeText(&b, []byte(value))
		b.WriteString("\"/>")
	}
	b.WriteString("\n")
	b.WriteString(metadataEnd)
	return b.String()
}

func parseMetadataValue(metadataXML string, field string) string {
	decoder := xml.NewDecoder(strings.NewReader("<metadata>" + metadataXML + "</metadata>"))
	for {
		token, err := decoder.Token()
		if err != nil {
			return ""
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Space != metadataNamespace || start.Name.Local != field {
			continue
		}

		for _, attr := range start.Attr {
			if attr.Name.Local == "id" {
				return attr.Value
			}
		}

		return ""
	}
}

func stripDomainUUID(domainXML string) string {
	domcfg := &libvirtxml.Domain{}
	err := domcfg.Unmarshal(domainXML)
	if err != nil {
		return ""
	}

	domcfg.UUID = ""
	changed, err := domcfg.Marshal()
	if err != nil {
		return ""
	}

	return changed
}

func ListManagedCheckpoints(dom *libvirt.Domain) ([]Checkpoint, error) {
	cpts, err := dom.ListAllCheckpoints(0)
	if err != nil {
		return nil, fmt.Errorf("list checkpoints: %w", err)
	}

	var out []Checkpoint
	for _, c := range cpts {
		// ListAllCheckpoints returns every checkpoint on the domain, not
		// just vmsync's own -- the prefix check below is what filters those
		// out. Each entry's handle must be freed regardless of which path
		// is taken, so this runs the per-checkpoint logic in its own
		// closure with a single defer covering all of them, rather than
		// needing a Free() call before every continue.
		cp, ok := func() (Checkpoint, bool) {
			defer c.Free()
			name, err := c.GetName()
			if err != nil {
				return Checkpoint{}, false
			}
			if !strings.HasPrefix(name, CheckpointPrefix+"-") {
				return Checkpoint{}, false
			}

			xmlDesc, err := c.GetXMLDesc(0)
			if err != nil {
				return Checkpoint{}, false
			}

			return parseCheckpointXML(name, xmlDesc), true
		}()
		if ok {
			out = append(out, cp)
		}
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].Time.Before(out[j].Time)
	})
	return out, nil
}

func parseCheckpointXML(name, desc string) Checkpoint {
	type cpXML struct {
		Creation string `xml:"creationTime"`
		Parent   struct {
			Name string `xml:"name"`
		} `xml:"parent"`
	}
	var cp cpXML
	_ = xml.Unmarshal([]byte(desc), &cp)

	var t time.Time
	if cp.Creation != "" {
		if sec, err := time.ParseDuration(cp.Creation + "s"); err == nil {
			t = time.Unix(int64(sec.Seconds()), 0)
		}
	}

	return Checkpoint{Name: name, Parent: cp.Parent.Name, Time: t}
}

func CreateCheckpoint(dom *libvirt.Domain, checkpointName, parent string, diskTargets []disk.QcowDisk) error {
	xmlBody := buildCheckpointXML(checkpointName, parent, diskTargets)
	cp, err := dom.CreateCheckpointXML(xmlBody, 0)
	if err != nil {
		return fmt.Errorf("create checkpoint %s: %w", checkpointName, err)
	}
	return cp.Free()
}

// CreateVerifyWindowCheckpoint creates the ephemeral, domain-wide checkpoint
// -verify-online uses to detect (after the fact) which regions the guest
// wrote to during its compare window -- see VerifyWindowCheckpointName.
// Standalone (parent=""): it has no lineage relationship to the regular
// vmsync-cpt-NNNNNN chain, and since nothing ever nominates it as another
// checkpoint's parent, it can never have children either -- so
// DeleteCheckpointIfExists's "children must be deleted before their parent"
// constraint never applies to it; it's always safe to delete on its own.
func CreateVerifyWindowCheckpoint(dom *libvirt.Domain, diskTargets []disk.QcowDisk) error {
	return CreateCheckpoint(dom, VerifyWindowCheckpointName, "", diskTargets)
}

// DeleteVerifyWindowCheckpoint removes the ephemeral verify-window
// checkpoint if it exists, tolerating the case where it doesn't (already
// cleaned up, or never created this run). Called both defensively (self-
// healing a prior crashed -verify-online run, unconditionally, regardless
// of whether -verify-online is requested this run) and as real cleanup once
// a -verify-online run's compare window closes.
func DeleteVerifyWindowCheckpoint(dom *libvirt.Domain) error {
	return DeleteCheckpointIfExists(dom, VerifyWindowCheckpointName)
}

func buildCheckpointXML(name, parent string, diskTargets []disk.QcowDisk) string {
	var b strings.Builder
	b.WriteString("<domaincheckpoint>\n")
	b.WriteString("  <name>" + name + "</name>\n")
	if parent != "" {
		b.WriteString("  <parent><name>" + parent + "</name></parent>\n")
	}
	b.WriteString("  <description>vmsync checkpoint</description>\n")
	b.WriteString("  <disks>\n")
	for _, dev := range diskTargets {
		b.WriteString("    <disk name=\"" + dev.TargetDev + "\" checkpoint=\"bitmap\" bitmap=\"" + name + "\"/>\n")
	}
	b.WriteString("  </disks>\n")
	b.WriteString("</domaincheckpoint>")
	return b.String()
}

func NextCheckpointName(existing []Checkpoint) (name string, parent string, err error) {
	if len(existing) == 0 {
		return fmt.Sprintf("%s-%06d", CheckpointPrefix, 1), "", nil
	}
	latest := existing[len(existing)-1]

	re := regexp.MustCompile(`^(.*-)(\d+)$`)
	m := re.FindStringSubmatch(latest.Name)
	if m == nil {
		return "", "", fmt.Errorf("checkpoint name %q does not end in a numeric suffix, cannot determine next checkpoint name", latest.Name)
	}
	numStr := m[2]
	n, _ := strconv.Atoi(numStr)
	n = n + 1
	return fmt.Sprintf("%s-%0*d", CheckpointPrefix, len(numStr), n), latest.Name, nil
}

func FailIfBlockJobActive(dom *libvirt.Domain, qcowDisks []disk.QcowDisk) error {
	for _, disk := range qcowDisks {
		info, err := dom.GetBlockJobInfo(disk.TargetDev, 0)
		if err != nil {
			return fmt.Errorf("check block job on disk %s: %w", disk.TargetDev, err)
		}

		// With no active job, libvirt typically returns unknown type and zero progress.
		if info.Type != libvirt.DOMAIN_BLOCK_JOB_TYPE_UNKNOWN || info.Cur > 0 || info.End > 0 {
			return fmt.Errorf("active block job detected on disk %s (type=%d cur=%d end=%d)", disk.TargetDev, info.Type, info.Cur, info.End)
		}
	}
	return nil
}

// AbortActiveBlockJobs cancels any running block job (typically a backup job
// left over from a previous, interrupted sync) on each of qcowDisks. Used by
// -reinit to clear the state that would otherwise make FailIfBlockJobActive
// permanently refuse to proceed -- a stuck job is exactly the kind of broken
// state -reinit is meant to recover from (see the "Bitmap already exists"
// failure in https://github.com/abbbi/vmsync/issues/9).
func AbortActiveBlockJobs(dom *libvirt.Domain, qcowDisks []disk.QcowDisk) error {
	for _, d := range qcowDisks {
		info, err := dom.GetBlockJobInfo(d.TargetDev, 0)
		if err != nil {
			return fmt.Errorf("check block job on disk %s: %w", d.TargetDev, err)
		}
		// Same "no active job" signature FailIfBlockJobActive checks for.
		if info.Type == libvirt.DOMAIN_BLOCK_JOB_TYPE_UNKNOWN && info.Cur == 0 && info.End == 0 {
			continue
		}
		trace.Warning("reinit: aborting active block job", "disk", d.TargetDev, "type", info.Type)
		if err := dom.BlockJobAbort(d.TargetDev, 0); err != nil {
			return fmt.Errorf("abort block job on disk %s: %w", d.TargetDev, err)
		}
	}
	return nil
}

func DeleteCheckpointIfExists(dom *libvirt.Domain, checkpointName string) error {
	cp, err := dom.CheckpointLookupByName(checkpointName, 0)
	if err != nil {
		if lvErr, ok := err.(libvirt.Error); ok && lvErr.Code == libvirt.ERR_NO_DOMAIN_CHECKPOINT {
			return nil
		}
		return fmt.Errorf("lookup checkpoint %s for deletion: %w", checkpointName, err)
	}
	defer cp.Free()
	if err := cp.Delete(0); err != nil {
		return fmt.Errorf("delete checkpoint %s: %w", checkpointName, err)
	}
	return nil
}

// DeleteAllManagedCheckpoints removes every vmsync-managed checkpoint on dom,
// used by -reinit to recover from a broken checkpoint chain (e.g. the
// "Bitmap already exists" failure in
// https://github.com/abbbi/vmsync/issues/9) by discarding it entirely and
// letting the next sync start over as a fresh full sync.
func DeleteAllManagedCheckpoints(dom *libvirt.Domain) error {
	existing, err := ListManagedCheckpoints(dom)
	if err != nil {
		return err
	}
	// Newest-to-oldest: a checkpoint with children still attached to it
	// cannot be deleted, so always remove children before their parent.
	// ListManagedCheckpoints sorts oldest-first.
	for i := len(existing) - 1; i >= 0; i-- {
		if err := DeleteCheckpointIfExists(dom, existing[i].Name); err != nil {
			return fmt.Errorf("delete checkpoint %s: %w", existing[i].Name, err)
		}
	}
	return nil
}

func StartPullBackupTCP(
	dom *libvirt.Domain,
	incrementalCheckpoint,
	exportBitmap,
	bindAddr string,
	port int,
	diskTargets []disk.QcowDisk,
) error {
	backupXML := buildPullBackupXML(incrementalCheckpoint, exportBitmap, bindAddr, port, diskTargets)
	if err := dom.BackupBegin(backupXML, "", 0); err != nil {
		return fmt.Errorf("start pull backup (tcp %s:%d): %w (xml=%s)", bindAddr, port, err, backupXML)
	}
	return nil
}

// ExternalSnapshotCount returns how many external snapshots (as opposed to
// internal, in-file ones) currently exist on dom -- the condition
// IsCheckpointBlockedBySnapshot exists to react to after the fact. This is a
// direct query (VIR_DOMAIN_SNAPSHOT_LIST_EXTERNAL), not an inference from a
// failed call, so it's accurate on its own even on runs that never attempt
// CreateCheckpoint at all -- used for the vmsync_external_snapshot_count
// metric.
func ExternalSnapshotCount(dom *libvirt.Domain) (int, error) {
	n, err := dom.SnapshotNum(libvirt.DOMAIN_SNAPSHOT_LIST_EXTERNAL)
	if err != nil {
		return 0, fmt.Errorf("count external snapshots: %w", err)
	}
	return n, nil
}

// IsCheckpointBlockedBySnapshot reports whether err is libvirt's specific,
// documented rejection of checkpoint creation while an external snapshot
// exists on the domain: "the creation of checkpoints when external
// snapshots exist is currently forbidden" (see
// https://libvirt.org/formatcheckpoint.html). Matched on the error text
// rather than a dedicated error code, since libvirt reports this as a
// generic invalid-operation error with no code of its own specific to this
// case -- unlike StopBackup just below, which has a structured alternative
// available and uses that instead.
func IsCheckpointBlockedBySnapshot(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "snapshot")
}

// StopBackup aborts any pull-backup job in progress on dom, tolerating the
// case where none is running (already stopped, or never started -- e.g. the
// retry-via-reconnect path after a primary stop that actually succeeded
// server-side but timed out client-side). Checks GetJobInfo first rather
// than relying solely on pattern-matching AbortJob's own error text: whether
// there's currently no job at all is exposed as a stable, structured enum
// (DomainJobType, verified directly against libvirt-go's own source), not a
// message string whose exact wording can vary across libvirt versions --
// which is exactly why the previous version of this function's text match
// ("no current job") never actually matched libvirt's real message and this
// short-circuit could never fire.
func StopBackup(dom *libvirt.Domain) error {
	if info, err := dom.GetJobInfo(); err == nil && info.Type == libvirt.DOMAIN_JOB_NONE {
		return nil
	}
	if err := dom.AbortJob(); err != nil {
		// Fallback for the rare case GetJobInfo above didn't catch it (e.g.
		// it errored itself) -- kept, but not relied upon. If this still
		// doesn't match in practice, the Debug log captures the real text
		// so it can be fixed with actual data instead of another guess.
		if strings.Contains(strings.ToLower(err.Error()), "no current job") {
			return nil
		}
		trace.Debug("abort backup job failed", "error", err)
		return fmt.Errorf("abort backup job: %w", err)
	}
	return nil
}

func buildPullBackupXML(
	incrementalCheckpoint,
	exportBitmap,
	bindAddr string,
	port int,
	diskTargets []disk.QcowDisk,
) string {
	if bindAddr == "" {
		bindAddr = "0.0.0.0"
	}
	if port <= 0 {
		port = 10809
	}

	var b strings.Builder
	b.WriteString("<domainbackup mode=\"pull\">\n")
	if incrementalCheckpoint != "" {
		b.WriteString("  <incremental>" + incrementalCheckpoint + "</incremental>\n")
	}
	b.WriteString(fmt.Sprintf("  <server transport=\"tcp\" name=\"%s\" port=\"%d\"/>\n", bindAddr, port))
	b.WriteString("  <disks>\n")
	for _, dev := range diskTargets {
		b.WriteString("    <disk name=\"" + dev.TargetDev + "\" exportname=\"" + dev.TargetDev + "\"")
		if exportBitmap != "" {
			b.WriteString(" exportbitmap=\"" + exportBitmap + "\"")
		}
		b.WriteString("/>\n")
	}
	b.WriteString("  </disks>\n")
	b.WriteString("</domainbackup>")
	return b.String()
}

// DomainActive reports whether dom is active in libvirt's own sense --
// anything other than shut off, which includes paused/suspended domains,
// not just ones actively executing. This is the right check both for
// deciding whether a domain needs starting (Create()/CreateWithFlags() only
// work on a shut-off domain -- calling them on an already-paused one fails
// with "domain is already running", exactly the class of error this
// replaces a check that used to miss) and for the safety checks that refuse
// to touch a domain's disk files while it's active: a paused domain still
// holds those files open exactly like a running one does, so treating it as
// safe to delete/overwrite under -- as a naive "state == DOMAIN_RUNNING"
// check would -- is a real risk, not just an inconvenience.
func DomainActive(dom *libvirt.Domain) (bool, error) {
	state, _, err := dom.GetState()
	if err != nil {
		return false, fmt.Errorf("unable to get domain state: %w", err)
	}
	return state != libvirt.DOMAIN_SHUTOFF, nil
}
