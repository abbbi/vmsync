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
	"io"
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
// -verify=online creates right when its compare window opens, to find out
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
	// MetadataFieldReplicaSource is written on the TARGET: "<host>:<domain>"
	// of the source it's currently being replicated from.
	MetadataFieldReplicaSource = "replica_source"
	// MetadataFieldReplicaTargets is written on the SOURCE: a comma-
	// separated, deduplicated, ever-growing list of "<host>:<domain>"
	// entries for every distinct target this source has ever been
	// replicated to (a source can fan out to more than one target, unlike
	// a target, which only ever has the one source it was last defined
	// from).
	MetadataFieldReplicaTargets = "replica_targets"
)

// metadataFieldOrder fixes the field order vmsync writes its own metadata
// entries in, purely for stable/readable XML output.
var metadataFieldOrder = []string{
	MetadataFieldLastCheckpoint,
	MetadataFieldLastSync,
	MetadataFieldFailureCount,
	MetadataFieldReplicaSource,
	MetadataFieldReplicaTargets,
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
// same way the actual data copy does. This runs independently of
// targetDiskPath (which only controls relocation to a different directory):
// pass nil/empty only when every disk's live source is already the correct
// target-side name (no external snapshot/linked clone in play for any
// disk) -- targetDiskPath being empty is not by itself a reason to pass an
// empty map too.
//
// If targetDomainName already exists, it is undefined first (persistent
// definitions can't be replaced in place) and its prior XML is kept in
// memory so a subsequent failure to define the replacement -- a transient
// libvirtd error, or the rewritten XML itself being rejected -- can restore
// it instead of leaving the target permanently undefined.
func DefineDomain(target *Manager, targetDomainName string, sourceDomainXML string, targetDiskPath string, rootSourceByLiveSource map[string]string) error {
	exists, err := DomainExists(target.Conn, targetDomainName)
	if err != nil {
		return fmt.Errorf("check target domain existence: %w", err)
	}

	var originalXML string
	if exists {
		trace.Info("Undefining domain on target system", "vm", targetDomainName)
		d, err := target.Conn.LookupDomainByName(targetDomainName)
		if err != nil {
			return fmt.Errorf("look up existing target domain %s for undefine: %w", targetDomainName, err)
		}
		defer d.Free()
		// Captured before undefining -- see rollback below. Failing to even
		// read it is treated as fatal rather than undefining a domain with
		// no way back to its current definition.
		originalXML, err = d.GetXMLDesc(0)
		if err != nil {
			return fmt.Errorf("read existing target domain %s xml before undefine: %w", targetDomainName, err)
		}
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

	// rollback restores the target domain to its pre-undefine definition
	// (best effort) whenever cause is about to make this function return
	// without having left a valid replacement defined -- a no-op if the
	// domain didn't already exist, since there's nothing to roll back to.
	rollback := func(cause error) error {
		if !exists {
			return cause
		}
		restored, rbErr := target.Conn.DomainDefineXML(originalXML)
		if rbErr != nil {
			return fmt.Errorf("%w (also failed to restore target domain's previous definition: %v)", cause, rbErr)
		}
		restored.Free()
		trace.Warning("restored target domain to its previous definition after redefine failure", "vm", targetDomainName, "cause", cause)
		return cause
	}

	// Keep source XML intact (including UUID) unless libvirt rejects duplicate UUID.
	updatedXML, err := replaceDomainName(sourceDomainXML, targetDomainName)
	if err != nil {
		return rollback(fmt.Errorf("rewrite target domain xml: %w", err))
	}

	if shouldRewriteDiskPaths(targetDiskPath, rootSourceByLiveSource) {
		updatedXML, err = replaceDomainDiskPath(updatedXML, targetDiskPath, rootSourceByLiveSource)
		if err != nil {
			return rollback(fmt.Errorf("rewrite target domain xml: %w", err))
		}
	}

	warnIfXMLElementsDropped("DefineDomain", sourceDomainXML, updatedXML)

	dom, err := target.Conn.DomainDefineXML(updatedXML)
	if err != nil {
		// Fallback for cloning into same target where another domain already uses the UUID.
		if strings.Contains(strings.ToLower(err.Error()), "already defined with uuid") {
			withoutUUID := stripDomainUUID(updatedXML)
			dom, retryErr := target.Conn.DomainDefineXML(withoutUUID)
			if retryErr != nil {
				return rollback(fmt.Errorf("define target domain after uuid fallback: %w", retryErr))
			}
			return dom.Free()
		}
		return rollback(fmt.Errorf("define target domain: %w", err))
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

// shouldRewriteDiskPaths decides whether DefineDomain needs to run
// replaceDomainDiskPath at all: either there's a relocation to apply
// (targetDiskPath set) or a live-source-to-root-source substitution to
// apply (rootSourceByLiveSource non-empty). Gating this on targetDiskPath
// alone -- the previous behavior -- skipped the whole rewrite, root-source
// substitution included, for the common case of no -target-disk-path. That
// was exactly wrong for an external-snapshot/linked-clone source: its live
// Source (an overlay vmsync's data copy never writes to under that name)
// would then survive unchanged into the target's own definition, pointing
// at a file that doesn't exist on the target at all. SetTargetPath's own
// empty-targetDiskPath branch already returns rootSource verbatim, so
// running this with targetDiskPath == "" is exactly the "keep the same
// path, just rename to root" case, not a no-op to be skipped.
func shouldRewriteDiskPaths(targetDiskPath string, rootSourceByLiveSource map[string]string) bool {
	return targetDiskPath != "" || len(rootSourceByLiveSource) > 0
}

// replaceDomainDiskPath rewrites each non-ignored disk's <source file> to its
// target-side path, and clears any <backingStore> the live domain XML
// carried for it. rootSourceByLiveSource maps a disk's live Source path (as
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
//
// Clearing BackingStore is required for the same reason, not an unrelated
// cleanup: whatever the live domain XML's backing chain says (an external
// snapshot's parent, or a permanent linked clone's shared base image) names
// a file on the *source* host, which vmsync never copies over -- only the
// resolved root file itself is copied, and always as a complete, standalone
// image: cmd/vmsync's main.go creates the target file flat for a full sync,
// and for an incremental sync commits the temporary delta overlay straight
// into that same root file with `qemu-img commit` before deleting the
// overlay, so by the time this runs the target's on-disk file never depends
// on any backing file at all. Left as copied verbatim from the source XML,
// a stale <backingStore> would describe a chain that either doesn't exist
// on the target host or doesn't match its actual (backing-file-free) disk
// -- something libvirt/qemu can refuse to start the domain over, or worse,
// misinterpret.
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
		domcfg.Devices.Disks[i].BackingStore = nil
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

// xmlElementNames returns the set of distinct element tag names (local
// name only -- "hostdev", "commandline", etc. -- namespace prefixes aren't
// distinguished, since the same element can legitimately round-trip
// through a different prefix with no actual loss) appearing anywhere in
// domainXML. Returns nil if domainXML doesn't even parse as XML, rather
// than produce a false "elements are missing" signal for something that
// was never valid to begin with.
func xmlElementNames(domainXML string) map[string]bool {
	names := map[string]bool{}
	dec := xml.NewDecoder(strings.NewReader(domainXML))
	for {
		tok, err := dec.Token()
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil
		}
		if start, ok := tok.(xml.StartElement); ok {
			names[start.Name.Local] = true
		}
	}
	return names
}

// intentionallyDroppedXMLElements lists element names missingXMLElements
// must never report, because this package removes them itself, on purpose,
// every time they're present -- not an accidental casualty of the
// unmarshal/marshal round-trip warnIfXMLElementsDropped exists to catch.
// Currently just backingStore: replaceDomainDiskPath clears it on every
// disk it touches (see that function's own doc comment for why), so it
// would otherwise be reported as "dropped" on literally every sync of a
// domain with an external snapshot or a permanent linked clone -- a
// guaranteed, permanent false positive on every such run, unlike the rare,
// genuinely-worth-a-look struct-modeling gaps this warning is meant to
// surface.
var intentionallyDroppedXMLElements = map[string]bool{
	"backingStore": true,
}

// missingXMLElements returns the sorted list of distinct element names
// present in original but absent from rewritten -- empty (nil) when
// original doesn't parse, rewritten doesn't parse, or nothing is missing.
// Split out from warnIfXMLElementsDropped below purely so this actual
// comparison logic is directly testable without needing to capture log
// output.
func missingXMLElements(original, rewritten string) []string {
	before := xmlElementNames(original)
	if before == nil {
		return nil
	}
	after := xmlElementNames(rewritten)
	if after == nil {
		return nil
	}
	var missing []string
	for name := range before {
		if intentionallyDroppedXMLElements[name] {
			continue
		}
		if !after[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}

// warnIfXMLElementsDropped logs a warning (tagged with context, e.g. a
// function/call-site name) listing any element names missingXMLElements
// finds went missing between original and rewritten.
//
// This exists because replaceDomainName, replaceDomainDiskPath, and
// SetMetadataFields all go through a full libvirtxml.Domain
// unmarshal-then-marshal round-trip rather than editing the XML text
// surgically, despite their own doc comments' promise to "keep source XML
// intact" -- any element that struct doesn't model (hostdev passthrough,
// TPM/launchSecurity, <qemu:commandline>, and similar less-common domain
// features) would otherwise be silently dropped on marshal with nothing to
// indicate anything went wrong, possibly not discovered until whatever
// that configuration was for turns out to be missing on a failed-over
// target VM. A real, exhaustive fix would mean editing the XML text
// surgically instead of round-tripping it through a typed struct at all --
// a much larger, higher-risk change to the single most-used code path in
// this tool (every disk's target definition, every sync) that isn't
// something to take on without the ability to test it against a wide
// range of real, complex domain definitions first.
//
// This check can't tell a genuine loss apart from a legitimate omission
// (an empty or default-valued element the struct correctly normalizes
// away on marshal), so it only warns rather than failing the sync outright
// -- but it turns an otherwise completely invisible risk into something an
// operator can actually notice and investigate the first time it would
// matter for a real domain, instead of silent, permanent, possibly
// never-discovered configuration loss.
func warnIfXMLElementsDropped(context, original, rewritten string) {
	missing := missingXMLElements(original, rewritten)
	if len(missing) == 0 {
		return
	}
	trace.Warning("domain xml elements present before this rewrite are missing afterward -- the domain-xml round-trip (unmarshal into a typed struct, then re-marshal) may have silently dropped configuration this tool doesn't model; verify the affected domain's definition still has everything you expect", "context", context, "missing_elements", strings.Join(missing, ", "))
}

// SetMetadataFields merges the given vmsync:field->value pairs into
// domainXML's <metadata> block, preserving any existing vmsync fields not
// mentioned in updates or removeFields (and any unrelated, non-vmsync
// metadata some other tool may have added) untouched. Fields named in
// removeFields are dropped entirely -- winning over updates if a field
// somehow appears in both -- used to strip metadata that's become
// semantically meaningless for a domain's current role (e.g. a target's
// last_checkpoint/failure_count once that domain becomes a replication
// SOURCE instead, which has no checkpoint chain of its own to report).
func SetMetadataFields(domainXML string, updates map[string]string, removeFields ...string) (string, error) {
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
	for _, field := range removeFields {
		delete(current, field)
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

	warnIfXMLElementsDropped("SetMetadataFields", domainXML, changed)
	return changed, nil
}

// UpdateSyncMetadata records a fresh checkpoint/timestamp, resets
// failure_count to 0, and records sourceHost:sourceDomain as this
// (target) domain's current replica_source -- called on the TARGET's new
// definition once a sync completes successfully.
func UpdateSyncMetadata(domainXML, checkpoint, sourceHost, sourceDomain string) (string, error) {
	return SetMetadataFields(domainXML, map[string]string{
		MetadataFieldLastCheckpoint: checkpoint,
		MetadataFieldLastSync:       strconv.FormatInt(time.Now().Unix(), 10),
		MetadataFieldFailureCount:   "0",
		MetadataFieldReplicaSource:  ReplicaEntry(sourceHost, sourceDomain),
	})
}

// ReplicaEntry formats a host+domain pair the same way on both sides of a
// replica_source/replica_targets metadata field, so the two are directly
// comparable (e.g. by replicaListContains) and consistently readable by a
// human or external tooling inspecting either domain's XML by hand.
func ReplicaEntry(host, domain string) string {
	return host + ":" + domain
}

// replicaListContains reports whether entry is already present in a
// comma-separated replica_targets-style list. Pure and side-effect-free
// so the exact dedup logic RecordReplicaTarget depends on is directly
// testable without a live domain.
func replicaListContains(list, entry string) bool {
	if list == "" {
		return false
	}
	for _, e := range strings.Split(list, ",") {
		if e == entry {
			return true
		}
	}
	return false
}

// appendReplicaTarget adds entry to a comma-separated replica_targets-style
// list, returning list unchanged if entry is already present -- so
// repeated syncs to the same target never grow the list, only a genuinely
// new distinct target does.
func appendReplicaTarget(list, entry string) string {
	if list == "" {
		return entry
	}
	if replicaListContains(list, entry) {
		return list
	}
	return list + "," + entry
}

// RecordReplicaTarget updates the SOURCE domain's own persistent
// definition (sourceDomainName, looked up on mgr) to add targetHost:
// targetDomain to its replica_targets metadata list (deduplicated -- a
// repeat sync to the same target is a no-op), and strips
// last_checkpoint/last_sync_timestamp/failure_count from it if present:
// those three fields describe a domain's state as a replication TARGET,
// and are meaningless -- and actively misleading to a human or external
// tool reading this domain's XML -- once it's acting as a SOURCE instead,
// which this call establishes it as. This is the one place vmsync ever
// writes to the source's own definition; unlike DefineDomain, it never
// undefines anything first, since it's the exact same domain (same name,
// same UUID) being patched in place -- DomainDefineXML updates a
// persistent definition that already matches by UUID directly, the same
// safe, already-established pattern RecordTargetSyncFailure uses to patch
// a live target's failure_count.
func RecordReplicaTarget(mgr *Manager, sourceDomainName, targetHost, targetDomain string) error {
	dom, err := mgr.Conn.LookupDomainByName(sourceDomainName)
	if err != nil {
		return fmt.Errorf("look up source domain %s to record replica target: %w", sourceDomainName, err)
	}
	defer dom.Free()

	domXML, err := dom.GetXMLDesc(0)
	if err != nil {
		return fmt.Errorf("read source domain %s xml: %w", sourceDomainName, err)
	}

	entry := ReplicaEntry(targetHost, targetDomain)
	existing, _ := ParseMetadata(domXML, MetadataFieldReplicaTargets)
	updatedList := appendReplicaTarget(existing, entry)

	staleFieldPresent := false
	for _, field := range []string{MetadataFieldLastCheckpoint, MetadataFieldLastSync, MetadataFieldFailureCount} {
		if v, _ := ParseMetadata(domXML, field); v != "" {
			staleFieldPresent = true
			break
		}
	}
	if updatedList == existing && !staleFieldPresent {
		// This target is already recorded and there's no stale
		// target-role metadata left to strip -- checked BEFORE calling
		// SetMetadataFields specifically to skip both its XML round-trip
		// and the redefine below once steady state is reached, rather
		// than comparing the round-tripped XML against the original
		// afterward (a full libvirtxml unmarshal-then-marshal cycle
		// essentially never reproduces its input byte-for-byte, even when
		// nothing semantic changed, so that comparison would never
		// actually skip anything).
		return nil
	}

	updatedXML, err := SetMetadataFields(domXML, map[string]string{
		MetadataFieldReplicaTargets: updatedList,
	}, MetadataFieldLastCheckpoint, MetadataFieldLastSync, MetadataFieldFailureCount)
	if err != nil {
		return fmt.Errorf("update replica_targets metadata: %w", err)
	}

	newDom, err := mgr.Conn.DomainDefineXML(updatedXML)
	if err != nil {
		return fmt.Errorf("redefine source domain %s with updated replica_targets metadata: %w", sourceDomainName, err)
	}
	defer newDom.Free()
	return nil
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
//
// The run-lock (pkg/util/lock.go) is keyed only by source domain, so it
// gives this function no protection at all against a concurrent writer of
// the *target* domain -- another vmsync invocation misconfigured to point
// at the same target, or any external tool (virsh define, another
// orchestration layer) redefining it. libvirt's domain-definition API has
// no atomic compare-and-swap primitive to close that race outright, so
// instead this re-reads the domain's XML immediately before writing and
// refuses to proceed if it no longer matches what the increment above was
// actually computed from -- narrowing the window from this whole
// function's read-then-write span down to a single extra round-trip, and
// turning what would otherwise be a silent, last-write-wins clobber into a
// loud, diagnosable error.
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

	latestXML, err := dom.GetXMLDesc(0)
	if err != nil {
		return 0, fmt.Errorf("re-read target domain xml before write: %w", err)
	}
	if latestXML != domXML {
		return 0, fmt.Errorf("target domain %s was redefined concurrently by something else while recording this failure; refusing to overwrite it -- check whether another vmsync invocation or an external tool is also managing this domain", targetDomain)
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

// ListManagedCheckpoints lists dom's vmsync-managed checkpoints. Callers
// (NextCheckpointName's chain-parent selection, DeleteAllManagedCheckpoints'
// -reinit cleanup) act on the assumption that this list is complete -- a
// silently incomplete one is worse than an error: NextCheckpointName could
// pick a stale parent or a name that collides with a checkpoint it never
// saw, and -reinit could leave a real vmsync checkpoint behind while
// believing it wiped the chain clean. So any lookup failure on an entry
// this can't positively rule out as one of vmsync's own aborts the whole
// listing instead of just skipping that entry -- a checkpoint whose name
// can't even be read might still be one of ours, and one whose name
// matches the vmsync prefix but whose XML can't be read definitely is.
// Only a successfully-read name that doesn't match the prefix is a
// legitimate, silent skip (some other tool's checkpoint on the same
// domain).
func ListManagedCheckpoints(dom *libvirt.Domain) ([]Checkpoint, error) {
	cpts, err := dom.ListAllCheckpoints(0)
	if err != nil {
		return nil, fmt.Errorf("list checkpoints: %w", err)
	}

	var out []Checkpoint
	var lookupErr error
	for _, c := range cpts {
		// ListAllCheckpoints returns every checkpoint on the domain, not
		// just vmsync's own -- the prefix check below is what filters those
		// out. Each entry's handle must be freed regardless of which path
		// is taken, so this runs the per-checkpoint logic in its own
		// closure with a single defer covering all of them, rather than
		// needing a Free() call before every continue. The range loop
		// itself always runs to completion, even once a failure has been
		// recorded, so every remaining handle still gets freed -- only the
		// final return value is affected.
		cp, ok, err := func() (Checkpoint, bool, error) {
			defer c.Free()
			name, err := c.GetName()
			if err != nil {
				return Checkpoint{}, false, fmt.Errorf("get checkpoint name: %w", err)
			}
			if !strings.HasPrefix(name, CheckpointPrefix+"-") {
				return Checkpoint{}, false, nil
			}

			xmlDesc, err := c.GetXMLDesc(0)
			if err != nil {
				return Checkpoint{}, false, fmt.Errorf("get xml for checkpoint %s: %w", name, err)
			}

			return parseCheckpointXML(name, xmlDesc), true, nil
		}()
		if err != nil && lookupErr == nil {
			lookupErr = err
		}
		if ok {
			out = append(out, cp)
		}
	}
	if lookupErr != nil {
		return nil, fmt.Errorf("list checkpoints: incomplete listing, at least one entry could not be read: %w", lookupErr)
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
// -verify=online uses to detect (after the fact) which regions the guest
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
// healing a prior crashed -verify=online run, unconditionally, regardless
// of whether -verify=online is requested this run) and as real cleanup once
// a -verify=online run's compare window closes.
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
	for _, name := range checkpointDeletionOrder(existing) {
		if err := DeleteCheckpointIfExists(dom, name); err != nil {
			return fmt.Errorf("delete checkpoint %s: %w", name, err)
		}
	}
	return nil
}

// checkpointDeletionOrder returns existing's checkpoint names in the order
// DeleteAllManagedCheckpoints must delete them: newest-first. A checkpoint
// with children still attached to it cannot be deleted (its bitmap can
// only be merged into a child on delete, never discarded while a child
// still references it), so children must always go before their parent --
// and since ListManagedCheckpoints sorts its result oldest-first, that's
// simply the reverse of what it returns. Kept as a standalone, pure
// function (taking and returning plain data, not a live domain handle)
// specifically so this exact ordering is directly testable: reversing it
// by mistake would permanently wedge the one recovery path -reinit
// provides for a broken checkpoint chain, since every deletion after the
// first would then fail against a checkpoint that still has a child.
func checkpointDeletionOrder(existing []Checkpoint) []string {
	order := make([]string, len(existing))
	for i, c := range existing {
		order[len(existing)-1-i] = c.Name
	}
	return order
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
//
// Requires both "checkpoint" and "external snapshot" to appear, rather than
// just the single generic word "snapshot" (this function's own previous
// implementation) -- libvirt/qemu use that word pervasively for entirely
// unrelated conditions, any of which would otherwise get misclassified as
// this specific, tolerated case. The caller only tolerates a true result by
// proceeding with an incremental sync against a checkpoint whose validity
// was never actually re-established this run (see its own comment) -- a
// false positive here is the dangerous direction, so requiring both terms
// together, closely matching libvirt's actual documented wording, is worth
// the (much smaller) risk of a false negative on some future, differently
// worded version of this same message; that failure mode is safe by
// comparison; it just falls back to failing the run outright, same as any
// other unrecognized CreateCheckpoint error.
func IsCheckpointBlockedBySnapshot(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "checkpoint") && strings.Contains(msg, "external snapshot")
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
