/*
	Copyright (C) 2026  Orsiris de Jong <ozy@netpower.fr>

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

// Package inventory reads what a hypervisor knows about its own
// replication state and turns it into a health assessment.
//
// There is no separate database of pairs. vmsync already records the
// topology in each domain's own libvirt metadata -- replica_source on a
// target, replica_targets on a source, plus the checkpoint, timestamp,
// failure count and role -- so the estate's configuration is discovered by
// reading the domains themselves rather than by consulting a registry that
// could disagree with reality.
//
// Scan needs a live libvirt connection; Assess does not. That split is
// deliberate: which conditions count as unhealthy is the part worth being
// able to test exhaustively, and it is pure data in, verdict out.
package inventory

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"vmsync/pkg/disk"
	"vmsync/pkg/libvirtsync"

	"libvirt.org/go/libvirt"
)

// Domain is one libvirt domain as seen by the agent running on its own
// host, with whatever vmsync metadata it carries already parsed out.
//
// A domain with no vmsync metadata at all still appears here. Reporting
// only the replicated ones would make "this vm is not protected" and "this
// vm was not looked at" indistinguishable, which is the single most
// dangerous ambiguity an availability view can have.
type Domain struct {
	Name       string `json:"name"`
	UUID       string `json:"uuid"`
	Active     bool   `json:"active"`
	Persistent bool   `json:"persistent"`

	// Role is the replication_role metadata field, "" when none is
	// recorded (which is every domain predating that feature).
	Role string `json:"role,omitempty"`

	LastCheckpoint string `json:"last_checkpoint,omitempty"`
	// LastSyncUnix is 0 when the domain has never completed a sync.
	LastSyncUnix int64 `json:"last_sync_unix,omitempty"`
	FailureCount int   `json:"failure_count"`

	// ReplicaSource is set on a TARGET: "host:domain" of where it is
	// replicated from. ReplicaTargets is set on a SOURCE: every target it
	// has ever been replicated to.
	ReplicaSource  string   `json:"replica_source,omitempty"`
	ReplicaTargets []string `json:"replica_targets,omitempty"`

	// The promotion record, present only on a domain that was failed over
	// to. PromotedFrom is the "host:domain" it was promoted away from, and
	// it is load-bearing rather than decorative: it is what identifies WHICH
	// source a promotion displaced, and therefore which one must not keep
	// running alongside it.
	PromotedFrom   string `json:"promoted_from,omitempty"`
	PromotedAtUnix int64  `json:"promoted_at_unix,omitempty"`
	PromotedBy     string `json:"promoted_by,omitempty"`
	PromotionMode  string `json:"promotion_mode,omitempty"`

	// The fence this promotion armed, present only on a promoted domain and
	// only when the promotion was explicitly asked to arm one.
	//
	// Reported so the control plane can say a failover authorised stopping
	// its old source, rather than leaving that visible nowhere but on the
	// hypervisor. Its absence is meaningful too: a promotion with no fence
	// authorised nothing, which is what a DR drill looks like.
	FenceID          string `json:"fence_id,omitempty"`
	FenceSource      string `json:"fence_source,omitempty"`
	FenceArmedAtUnix int64  `json:"fence_armed_at_unix,omitempty"`
	FenceArmedBy     string `json:"fence_armed_by,omitempty"`

	// LastReplicatedAtUnix / LastReplicatedTo are written on a SOURCE: when
	// it last replicated successfully, and to where. Distinct from
	// LastSyncUnix, which lives on a TARGET and means "when was I last
	// written" -- the same fact seen from the two sides.
	//
	// They exist for the disaster case: from the source host alone, with the
	// other site unreachable, the question is when this VM last replicated
	// and where to.
	LastReplicatedAtUnix int64  `json:"last_replicated_at_unix,omitempty"`
	LastReplicatedTo     string `json:"last_replicated_to,omitempty"`

	// The restore record: this replica's disks were rolled back to one of
	// its restore points rather than being what the last sync copied.
	//
	// Reported because it is the only thing that survives a promotion to
	// explain an unusually wide data-loss window. Every other signal is
	// ambiguous with an ordinary lagging replica.
	RestoredFrom   string `json:"restored_from,omitempty"`
	RestoredAtUnix int64  `json:"restored_at_unix,omitempty"`
	RestoredBy     string `json:"restored_by,omitempty"`

	// Disks is what this domain occupies on this host's storage. Reported
	// so an operator can answer "is there room to keep the old copy?"
	// before an inversion, rather than after the disk fills.
	Disks []DiskInfo `json:"disks,omitempty"`

	// RestorePoints is what this replica can be rolled back to.
	//
	// The one piece of a domain's state that is not in its metadata: restore
	// points are directories beside the disks, deliberately, because vmsync
	// keeps no inventory of its own for them. Reported because a restore
	// operation names a TAG, and unlike every other operation's parameter
	// -- a role, a peer -- a tag cannot be derived from anything already
	// reported. Without this the control plane could only ask for a restore
	// point it has never seen.
	//
	// Nil on the ordinary host, which is any host not using -retention.
	RestorePoints []RestorePointInfo `json:"restore_points,omitempty"`
}

// IsTarget reports whether this domain is the receiving side of a pair.
func (d Domain) IsTarget() bool { return d.ReplicaSource != "" }

// IsSource reports whether this domain is the sending side of a pair.
func (d Domain) IsSource() bool { return len(d.ReplicaTargets) > 0 }

// Participates reports whether vmsync has any relationship with this
// domain at all. A domain that participates in nothing is not a problem --
// it is simply not replicated -- but it is worth listing so an operator can
// see that it isn't.
func (d Domain) Participates() bool {
	return d.IsTarget() || d.IsSource() || d.Role != "" || d.LastCheckpoint != ""
}

// Status is a domain's replication health, ordered by how much attention it
// wants. Comparisons rely on that order (see Worse).
type Status int

const (
	// StatusUnreplicated means vmsync has no relationship with this domain.
	// Not a fault, but deliberately not "OK" either: a vm nobody configured
	// replication for should not sit in a green list.
	StatusUnreplicated Status = iota
	// StatusOK: replicating, recent, no failures.
	StatusOK
	// StatusPaused/StatusPromoted are administrative states, not faults.
	// They are ranked above OK only so they stay visible rather than
	// blending into a long healthy list.
	StatusPaused
	StatusPromoted
	// StatusWarning: something is degraded but replication is still working.
	StatusWarning
	// StatusCritical: replication is not delivering its promise.
	StatusCritical
)

func (s Status) String() string {
	switch s {
	case StatusUnreplicated:
		return "unreplicated"
	case StatusOK:
		return "ok"
	case StatusPaused:
		return "paused"
	case StatusPromoted:
		return "promoted"
	case StatusWarning:
		return "warning"
	case StatusCritical:
		return "critical"
	default:
		return fmt.Sprintf("status(%d)", int(s))
	}
}

func (s Status) MarshalJSON() ([]byte, error) {
	return []byte(strconv.Quote(s.String())), nil
}

// Worse returns whichever status wants more attention.
func Worse(a, b Status) Status {
	if a > b {
		return a
	}
	return b
}

// Assessment is a verdict plus every reason behind it, so a UI can show
// what is wrong rather than only that something is.
type Assessment struct {
	Status  Status   `json:"status"`
	Reasons []string `json:"reasons,omitempty"`
	// AgeSeconds is how long since the last successful sync, -1 when the
	// domain has never synced or is not a target.
	AgeSeconds int64 `json:"age_seconds"`
}

// Assess judges a target domain's replication health.
//
// cadence is how often this pair is expected to sync; zero means unknown,
// which disables the staleness checks rather than guessing a threshold. An
// agent that has a schedule knows its cadence; one running against
// cron-driven replication may not, and inventing a default would produce
// confident nonsense on a pair that legitimately syncs once a week.
//
// Only targets are judged on freshness. A source's own metadata records
// where it replicates TO, not when -- the timestamp lives on the target,
// written by the run that updated it -- so assessing a source on its own
// last_sync would report every source as permanently stale.
func Assess(d Domain, now time.Time, cadence time.Duration) Assessment {
	a := Assessment{Status: StatusOK, AgeSeconds: -1}

	if !d.Participates() {
		a.Status = StatusUnreplicated
		return a
	}

	// Administrative states short-circuit the freshness checks: a paused or
	// promoted domain is SUPPOSED to have a growing last_sync age, and
	// reporting that as staleness would bury the real signal under noise
	// for exactly the domains an operator is already watching closely.
	switch d.Role {
	case libvirtsync.RolePaused:
		a.Status = StatusPaused
		a.Reasons = append(a.Reasons, "replication administratively paused (replication_role=paused)")
		return a
	case libvirtsync.RolePromoted:
		a.Status = StatusPromoted
		a.Reasons = append(a.Reasons, "promoted to live after a failover (replication_role=promoted); no longer receiving replication")
		return a
	}

	if !d.IsTarget() {
		// A source. Its own health is its peers' business -- reported from
		// the target side, where the timestamps actually live.
		if d.IsSource() {
			a.Reasons = append(a.Reasons, fmt.Sprintf("replication source for %s", strings.Join(d.ReplicaTargets, ", ")))
		}
		return a
	}

	if d.FailureCount > 0 {
		a.Status = Worse(a.Status, StatusWarning)
		a.Reasons = append(a.Reasons, fmt.Sprintf("%d consecutive sync failure(s) recorded", d.FailureCount))
	}

	if d.LastSyncUnix <= 0 {
		a.Status = Worse(a.Status, StatusCritical)
		a.Reasons = append(a.Reasons, "no successful sync has ever completed against this target")
		return a
	}

	age := now.Sub(time.Unix(d.LastSyncUnix, 0))
	if age < 0 {
		// A timestamp in the future means clock skew between this host and
		// whichever ran the sync. Say so rather than reporting a negative
		// age or silently clamping it, since every freshness number below
		// is untrustworthy until it is fixed.
		a.Status = Worse(a.Status, StatusWarning)
		a.Reasons = append(a.Reasons, "last sync timestamp is in the future -- clock skew between this host and the one that ran the sync")
		a.AgeSeconds = 0
		return a
	}
	a.AgeSeconds = int64(age.Seconds())

	if cadence > 0 {
		switch {
		case age > 3*cadence:
			a.Status = Worse(a.Status, StatusCritical)
			a.Reasons = append(a.Reasons, fmt.Sprintf("last sync was %s ago, more than 3x the %s cadence", age.Round(time.Second), cadence))
		case age > cadence:
			a.Status = Worse(a.Status, StatusWarning)
			a.Reasons = append(a.Reasons, fmt.Sprintf("last sync was %s ago, past the %s cadence", age.Round(time.Second), cadence))
		}
	}

	if d.LastCheckpoint == "" {
		// A target with a sync timestamp but no checkpoint cannot be synced
		// incrementally: vmsync refuses to trust it (see
		// unverifiableCheckpointMetadataError) and every future run falls
		// back to a full copy.
		a.Status = Worse(a.Status, StatusWarning)
		a.Reasons = append(a.Reasons, "no last_checkpoint recorded, so the next sync cannot run incrementally")
	}

	if a.Status == StatusOK && len(a.Reasons) == 0 {
		a.Reasons = append(a.Reasons, fmt.Sprintf("replicating from %s", d.ReplicaSource))
	}
	return a
}

// Scan reads every domain libvirt knows about on this connection and parses
// out whatever vmsync metadata each carries.
//
// Inactive domains are included: a replication target is SUPPOSED to be
// shut off, so listing only running domains would hide every target in the
// estate.
func Scan(mgr *libvirtsync.Manager) ([]Domain, error) {
	doms, err := mgr.Conn.ListAllDomains(0)
	if err != nil {
		return nil, fmt.Errorf("list domains: %w", err)
	}
	defer func() {
		for i := range doms {
			doms[i].Free()
		}
	}()

	out := make([]Domain, 0, len(doms))
	for i := range doms {
		d, err := describe(&doms[i])
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func describe(dom *libvirt.Domain) (Domain, error) {
	name, err := dom.GetName()
	if err != nil {
		return Domain{}, fmt.Errorf("read domain name: %w", err)
	}
	d := Domain{Name: name}

	if uuid, err := dom.GetUUIDString(); err == nil {
		d.UUID = uuid
	}
	if active, err := dom.IsActive(); err == nil {
		d.Active = active
	}
	if persistent, err := dom.IsPersistent(); err == nil {
		d.Persistent = persistent
	}

	// The persistent definition, not the live one: every field below lives
	// in the stored XML, and a running domain's live XML carries runtime
	// additions that are irrelevant here.
	xml, err := dom.GetXMLDesc(libvirt.DOMAIN_XML_INACTIVE)
	if err != nil {
		// A transient domain has no inactive definition to read. That is
		// not a scan failure -- report what libvirt already told us and
		// leave the metadata fields empty.
		return d, nil
	}

	// RootSource, not Source: a domain sitting on an external snapshot has
	// its live Source pointing at an overlay, while the file that actually
	// holds the data -- and that a replica is named after -- is the base of
	// the chain. Sizing the overlay would report a few megabytes for a
	// hundred-gigabyte VM.
	if disks, derr := disk.ParseQcowDisks(xml); derr == nil {
		paths := make([]string, 0, len(disks))
		for _, qd := range disks {
			// Was this same fallback written out inline. It is QcowDisk.Path()
			// now, so the one place that knew RootSource is empty here is not
			// also the only place that knows it.
			paths = append(paths, qd.Path())
		}
		d.Disks = inspectDisks(paths)
		// After the disks, because it is derived from where they are -- the
		// same rule the sync path uses to place them, rather than any
		// configured target_disk_path, which agrees only when it happens to
		// name the directory the disks are really in.
		d.RestorePoints = RestorePointsFor(d)
	}

	d.Role, _ = libvirtsync.ParseMetadata(xml, libvirtsync.MetadataFieldReplicationRole)
	d.LastCheckpoint, _ = libvirtsync.ParseMetadata(xml, libvirtsync.MetadataFieldLastCheckpoint)
	d.ReplicaSource, _ = libvirtsync.ParseMetadata(xml, libvirtsync.MetadataFieldReplicaSource)
	d.PromotedFrom, _ = libvirtsync.ParseMetadata(xml, libvirtsync.MetadataFieldPromotedFrom)
	d.LastReplicatedTo, _ = libvirtsync.ParseMetadata(xml, libvirtsync.MetadataFieldLastReplicatedTo)
	d.RestoredFrom, _ = libvirtsync.ParseMetadata(xml, libvirtsync.MetadataFieldRestoredFrom)
	d.RestoredBy, _ = libvirtsync.ParseMetadata(xml, libvirtsync.MetadataFieldRestoredBy)
	if raw, err := libvirtsync.ParseMetadata(xml, libvirtsync.MetadataFieldLastReplicatedAt); err == nil && raw != "" {
		if n, convErr := strconv.ParseInt(raw, 10, 64); convErr == nil {
			d.LastReplicatedAtUnix = n
		}
	}
	if raw, err := libvirtsync.ParseMetadata(xml, libvirtsync.MetadataFieldRestoredAt); err == nil && raw != "" {
		if n, convErr := strconv.ParseInt(raw, 10, 64); convErr == nil {
			d.RestoredAtUnix = n
		}
	}
	d.PromotedBy, _ = libvirtsync.ParseMetadata(xml, libvirtsync.MetadataFieldPromotedBy)
	d.PromotionMode, _ = libvirtsync.ParseMetadata(xml, libvirtsync.MetadataFieldPromotionMode)
	if raw, err := libvirtsync.ParseMetadata(xml, libvirtsync.MetadataFieldPromotedAt); err == nil && raw != "" {
		if n, convErr := strconv.ParseInt(raw, 10, 64); convErr == nil {
			d.PromotedAtUnix = n
		}
	}

	d.FenceID, _ = libvirtsync.ParseMetadata(xml, libvirtsync.MetadataFieldFenceID)
	d.FenceSource, _ = libvirtsync.ParseMetadata(xml, libvirtsync.MetadataFieldFenceSource)
	d.FenceArmedBy, _ = libvirtsync.ParseMetadata(xml, libvirtsync.MetadataFieldFenceArmedBy)
	if raw, err := libvirtsync.ParseMetadata(xml, libvirtsync.MetadataFieldFenceArmedAt); err == nil && raw != "" {
		if n, convErr := strconv.ParseInt(raw, 10, 64); convErr == nil {
			d.FenceArmedAtUnix = n
		}
	}

	if raw, err := libvirtsync.ParseMetadata(xml, libvirtsync.MetadataFieldLastSync); err == nil && raw != "" {
		if ts, err := strconv.ParseInt(raw, 10, 64); err == nil {
			d.LastSyncUnix = ts
		}
	}
	if raw, err := libvirtsync.ParseMetadata(xml, libvirtsync.MetadataFieldFailureCount); err == nil && raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			d.FailureCount = n
		}
	}
	if raw, err := libvirtsync.ParseMetadata(xml, libvirtsync.MetadataFieldReplicaTargets); err == nil && raw != "" {
		for _, entry := range strings.Split(raw, ",") {
			if entry = strings.TrimSpace(entry); entry != "" {
				d.ReplicaTargets = append(d.ReplicaTargets, entry)
			}
		}
	}
	return d, nil
}
