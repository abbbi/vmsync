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

const (
	metadataNamespace = `http://vmsync.org/xmlns/libvirt/domain/1.0`
	metadataStart     = `<vmsync:vmsync xmlns:vmsync="` + metadataNamespace + `">`
	metadataEnd       = `</vmsync:vmsync>`

	MetadataFieldLastCheckpoint = "last_checkpoint"
	MetadataFieldLastSync       = "last_sync_timestamp"
)

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

func DefineDomain(target *Manager, targetDomainName string, sourceDomainXML string, targetDiskPath string) error {
	exists, err := DomainExists(target.Conn, targetDomainName)
	if err != nil {
		return fmt.Errorf("check target domain existence: %w", err)
	}
	if exists {
		trace.Info("Undefining domain on target system", "vm", targetDomainName)
		d, _ := target.Conn.LookupDomainByName(targetDomainName)
		if err := d.Undefine(); err != nil {
			trace.Warning("Unable to undefine existing target domain, skipping redefine")
			return nil
		}
	}

	// Keep source XML intact (including UUID) unless libvirt rejects duplicate UUID.
	updatedXML, err := replaceDomainName(sourceDomainXML, targetDomainName)
	if err != nil {
		return fmt.Errorf("rewrite target domain xml: %w", err)
	}

	// Keep source XML intact (including UUID) unless libvirt rejects duplicate UUID.
	if targetDiskPath != "" {
		updatedXML, err = replaceDomainDiskPath(updatedXML, targetDiskPath)
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

func replaceDomainDiskPath(domainXML, targetDiskPath string) (string, error) {
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

		domcfg.Devices.Disks[i].Source.File.File = util.SetTargetPath(targetDiskPath, d.Source.File.File)
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

func AddMetadata(domainXML string, checkpoint string) (string, error) {
	domcfg := &libvirtxml.Domain{}
	err := domcfg.Unmarshal(domainXML)
	if err != nil {
		return "", err
	}

	entry := metadataEntry(checkpoint)
	if domcfg.Metadata == nil {
		domcfg.Metadata = &libvirtxml.DomainMetadata{XML: entry}
	} else if !strings.Contains(domcfg.Metadata.XML, `<vmsync:last_checkpoint`) {
		domcfg.Metadata.XML += entry
	}

	changed, err := domcfg.Marshal()
	if err != nil {
		return "", err
	}

	return changed, nil
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

func metadataEntry(checkpoint string) string {
	var b strings.Builder
	b.WriteString(metadataStart)
	b.WriteString("\n  <vmsync:")
	b.WriteString(MetadataFieldLastCheckpoint)
	b.WriteString(" id=\"")
	_ = xml.EscapeText(&b, []byte(checkpoint))
	b.WriteString("\"/>\n")
	b.WriteString("  <vmsync:")
	b.WriteString(MetadataFieldLastSync)
	b.WriteString(" id=\"")
	b.WriteString(strconv.FormatInt(time.Now().Unix(), 10))
	b.WriteString("\"/>\n")
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
		name, err := c.GetName()
		if err != nil {
			continue
		}
		if !strings.HasPrefix(name, CheckpointPrefix+"-") {
			continue
		}

		xmlDesc, err := c.GetXMLDesc(0)
		if err != nil {
			continue
		}

		cp := parseCheckpointXML(name, xmlDesc)
		out = append(out, cp)
		_ = c.Free()
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

func NextCheckpointName(existing []Checkpoint) (name string, parent string) {
	if len(existing) == 0 {
		return fmt.Sprintf("%s-%06d", CheckpointPrefix, 1), ""
	}
	latest := existing[len(existing)-1]

	re := regexp.MustCompile(`^(.*-)(\d+)$`)
	m := re.FindStringSubmatch(latest.Name)
	numStr := m[2]
	n, _ := strconv.Atoi(numStr)
	n = n + 1
	return fmt.Sprintf("%s-%0*d", CheckpointPrefix, len(numStr), n), latest.Name
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

func StopBackup(dom *libvirt.Domain) error {
	if err := dom.AbortJob(); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no current job") {
			return nil
		}
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

func DomainRunning(dom *libvirt.Domain) (bool, error) {
	tgtState, _, err := dom.GetState()
	if err != nil {
		return false, fmt.Errorf("unable to get get target domain state: %w", err)
	}
	if tgtState == libvirt.DOMAIN_RUNNING {
		return true, nil
	}
	return false, nil
}
