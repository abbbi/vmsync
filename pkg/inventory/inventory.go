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

	// The record a -verify run leaves behind when it found this replica's
	// contents differing from its source. Presence IS the state, exactly as
	// libvirtsync.MetadataFieldVerifyState defines it: empty is every
	// replica that has never failed a verification, and the only value ever
	// written is libvirtsync.VerifyStateFailed.
	//
	// This is the one field here that says the replica is WRONG, as opposed
	// to merely old. Everything else on this struct describes whether
	// replication RAN -- an age, a failure count, a missing checkpoint, all
	// of which mean recent writes may be missing. This one means a
	// comparison against the source read back what was already copied and
	// found it different, so the copy that IS there cannot be trusted
	// either. Failing to report it is how a replica known not to match its
	// source gets chosen as a failover target by an operator looking at a
	// screen that showed nothing wrong.
	//
	// VerifyFailedAtUnix is 0 when the finding carries no timestamp, which
	// is reported as such rather than guessed at: a verification failure of
	// unknown date is still a verification failure.
	VerifyState        string `json:"verify_state,omitempty"`
	VerifyFailedAtUnix int64  `json:"verify_failed_at_unix,omitempty"`

	// ReplicaIncomplete is the armed-and-never-cleared marker a full rebuild
	// writes on this domain BEFORE it renames the good disks aside and
	// starts writing new base images over them. Presence IS the state, like
	// VerifyState: the run that finishes clears it in the same
	// DomainDefineXML that records the sync, so a value surviving here means
	// no run ever recorded success.
	//
	// Kept RAW and unparsed on purpose. What the value says -- which verb,
	// when, which action id, which host, and the .vmsync-replaced-<unix>
	// stamp naming the complete copy that was set aside -- is pkg/failover's
	// to read (see ParseReplicaIncomplete), because the refusal decision
	// belongs there. This package's job is to make sure the field reaches a
	// screen at all.
	//
	// It is the one field here that says the disks are not a COPY OF
	// ANYTHING. VerifyState says the copy is wrong; every other field says
	// the copy is merely old. This says a rebuild was interrupted between
	// "the good replica has been renamed aside" and "the new one is
	// finished", so what the target's metadata still calls a replica is a
	// half-written image whose last_checkpoint, last_sync_timestamp and
	// failure_count all describe the run BEFORE the rebuild and therefore
	// look perfectly healthy. Failing to report it is how an operator
	// promotes a partial image believing the reported data-loss window.
	ReplicaIncomplete string `json:"replica_incomplete,omitempty"`

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
//
// A recorded verification failure is applied on top of that verdict rather
// than inside it, which is why this function is a wrapper. The replication
// verdict below is built from a chain of early returns -- unreplicated,
// paused, fenced, promoted, never-synced -- and every one of them is a path
// on which a "your replica is wrong" finding would never be reached. Laying
// it over the result instead makes the finding independent of that chain by
// construction, including for early returns added later.
//
// An interrupted rebuild (replica_incomplete) is laid over the same way and
// for the same reason, and it needs that treatment even more than the
// verification verdict does: the whole danger of an interrupted rebuild is
// that every OTHER field on the domain still describes the run before it, so
// the replication verdict underneath is a confident "ok".
func Assess(d Domain, now time.Time, cadence time.Duration) Assessment {
	a := assessReplication(d, now, cadence)

	// Presence is the state -- see libvirtsync.MetadataFieldVerifyState --
	// so any value at all is a failure, including one this build does not
	// recognise. An unknown value is treated as bad news on purpose: the
	// alternative is a future value silently reading as healthy.
	if d.VerifyState != "" {
		// Outranks everything assessReplication can have concluded, and the
		// ordering of Status does that on its own: critical is the maximum,
		// so "known wrong" wins over "stale" (warning), over the
		// administrative paused/promoted states, and over unreplicated.
		//
		// That precedence is the whole point. A paused, fenced or promoted
		// replica is exactly where this finding is easiest to lose -- those
		// are the states an operator reads as "nothing to do here" -- and a
		// promoted domain that failed its last verification is a live
		// service running on data known not to match what it replaced.
		// Administrative calm must not outrank evidence of corruption.
		a.Status = Worse(a.Status, StatusCritical)

		when := "at an unrecorded time"
		if d.VerifyFailedAtUnix > 0 {
			when = "on " + time.Unix(d.VerifyFailedAtUnix, 0).UTC().Format(time.RFC3339)
		}
		// Prepended, not appended: this is the reason that changes what an
		// operator does, and a UI showing only the first one must not show
		// "promoted to live after a failover" while the copy is known wrong.
		a.Reasons = append([]string{fmt.Sprintf(
			"the replica's contents were found to differ from its source %s and the finding has not been cleared (verify_state=%s), so this copy is known WRONG rather than merely stale -- see that run's log for which blocks differed",
			when, d.VerifyState)}, a.Reasons...)
	}

	// Presence is the state again, and again any value counts, including one
	// this build cannot read: the marker is armed before the disks are
	// touched and cleared only by the define that records success, so
	// whatever is still here means no run ever got that far.
	//
	// Laid over the replication verdict rather than checked inside it, for
	// the reason given above -- and applied AFTER the verification overlay
	// so that when both are present this one is prepended in front of it.
	// That order is a judgement about which sentence an operator must read
	// first: "wrong" still describes a whole copy of something, while this
	// describes disks that were being overwritten when the writer died and
	// are not a consistent image of any point in time.
	if d.ReplicaIncomplete != "" {
		a.Status = Worse(a.Status, StatusCritical)
		a.Reasons = append([]string{fmt.Sprintf(
			"a full rebuild of this replica was STARTED and never recorded as finished (replica_incomplete=%s), so these disks are a partial copy: the previous complete replica was renamed aside as .vmsync-replaced-<unix> and new base images were being written over it when the run stopped. Everything else recorded here -- last_checkpoint, last_sync_timestamp, failure_count -- describes the sync BEFORE that rebuild and must not be read as describing what is on disk now. Re-run the sync to completion, or recover the set-aside copy; do NOT promote this domain",
			d.ReplicaIncomplete)}, a.Reasons...)
	}
	return a
}

// assessReplication is the replication-health half of Assess: everything
// judged from whether syncing ran and how recently. Kept separate so the
// verification finding cannot be swallowed by its early returns.
func assessReplication(d Domain, now time.Time, cadence time.Duration) Assessment {
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
	case libvirtsync.RoleFenced:
		// A fence that did not stop the domain is CRITICAL, and it is the one
		// case this branch used to bury. The role is written even when the ACPI
		// shutdown fails -- deliberately, because it is the only thing stopping
		// replication resuming into the split brain -- so "fenced" alone says
		// nothing about whether the domain actually stopped. Still running means
		// it is live beside the peer that displaced it and both are taking
		// writes, which is the worst state in the product; reporting it as an
		// expected administrative pause is how it stayed invisible.
		//
		// Same predicate the agent's fence sweep and its gauge use, so the
		// console and the metrics cannot disagree about which VMs are live twice.
		if d.Active {
			a.Status = StatusCritical
			a.Reasons = append(a.Reasons, "FENCED BUT STILL RUNNING: a fence suspended this domain's replication and did not stop the domain (replication_role=fenced, and it is running), so it is live beside the peer that was promoted over it and both are taking writes. A fence is never retried: stop this domain by hand, then run -invert if the failover stands, or -update-role=source if the fence was wrong")
			return a
		}
		// Stopped, as a fence intends. Reported as paused because the
		// consequence is the same -- nothing is replicating into it, and that is
		// expected rather than broken -- but with its own reason, because the
		// cause and the fix are not the same at all. Somebody paused a paused
		// domain and will unpause it; nobody chose this one, a peer took over,
		// and the usual repair is -invert.
		a.Status = StatusPaused
		a.Reasons = append(a.Reasons, "stopped by an automatic fence after a peer was promoted over it (replication_role=fenced); run -invert if the failover stands, or -update-role=target if the fence was wrong")
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
//
// Expected cdrom skips log at debug level; see ScanVerbose for the loud
// variant. Every periodic scan in the agent uses this one.
func Scan(mgr *libvirtsync.Manager) ([]Domain, error) {
	return scan(mgr, false)
}

// ScanVerbose is Scan with the per-device skip lines (cdroms included) at
// INFO. For operator-requested records -- the agent's startup inventory and
// its on-demand (SIGUSR1) dump -- not for anything that runs on a timer.
func ScanVerbose(mgr *libvirtsync.Manager) ([]Domain, error) {
	return scan(mgr, true)
}

func scan(mgr *libvirtsync.Manager, verboseSkips bool) ([]Domain, error) {
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
		d, err := describe(&doms[i], verboseSkips)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func describe(dom *libvirt.Domain, verboseSkips bool) (Domain, error) {
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
	parse := disk.ParseQcowDisksQuiet
	if verboseSkips {
		parse = disk.ParseQcowDisks
	}
	if disks, derr := parse(xml); derr == nil {
		paths := make([]string, 0, len(disks))
		for _, qd := range disks {
			// Was this same fallback written out inline. It is QcowDisk.Path()
			// now, so the one place that knew RootSource is empty here is not
			// also the only place that knows it.
			paths = append(paths, qd.Path())
		}
		d.Disks = inspectDisks(paths)
		// After the disks, because the store is derived from where they are --
		// the same rule the sync path uses to place them, rather than any
		// configured target_disk_path, which agrees only when it happens to
		// name the directory the disks are really in. It also needs d.Name,
		// which libvirt gave us before this function read any XML: restore
		// points are kept per target domain.
		d.RestorePoints = RestorePointsFor(d)
	}

	applyDomainMetadata(xml, &d)
	return d, nil
}

// applyDomainMetadata fills in everything a Domain takes from its vmsync
// metadata element, and touches nothing that needs a libvirt connection.
//
// Split out of describe so this mapping can be tested at all. Inline, the
// only way to reach these lines was a live libvirt holding a real domain, so
// a field that was never parsed here looked exactly like a field no domain
// had -- and a test that built a Domain by hand went on passing while the
// scan reported an empty value for it. That is not hypothetical: the verify
// verdict reached the promotion gate's own rules and was never populated by
// the reader in front of them, which is the defect this seam exists to keep
// from recurring. A field added to Domain that comes from metadata belongs
// here, where a test can see whether it arrives.
//
// Every parse failure is swallowed into the zero value, matching how the
// rest of this package reads metadata: an absent field is the ordinary case,
// so it must not turn an otherwise good scan into an error. The value fails
// closed, never the whole read.
func applyDomainMetadata(xml string, d *Domain) {
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
	// Read unconditionally rather than only for targets: the field is written
	// on whatever domain a -verify compared, and a domain carrying one that
	// its current role does not explain is itself something an operator needs
	// to see rather than something this scan should quietly drop.
	d.VerifyState, _ = libvirtsync.ParseMetadata(xml, libvirtsync.MetadataFieldVerifyState)
	if raw, err := libvirtsync.ParseMetadata(xml, libvirtsync.MetadataFieldVerifyFailedAt); err == nil && raw != "" {
		if n, convErr := strconv.ParseInt(raw, 10, 64); convErr == nil {
			d.VerifyFailedAtUnix = n
		}
		// A value that will not parse leaves the timestamp at 0, which Assess
		// reports as an unrecorded time. Dropping the whole finding because
		// its date is unreadable would discard the part that matters.
	}
	// Read unconditionally and kept verbatim, for two reasons. The marker is
	// armed on whatever domain a full rebuild is about to overwrite, so a
	// role gate here would drop it on exactly the domain whose role was left
	// describing the state before the rebuild. And the value is pkg/failover's
	// to interpret: this reader must not turn an unparsable one into an empty
	// one, because empty is the only value that means "no interrupted
	// rebuild" -- the one reading that lets a promotion go ahead.
	d.ReplicaIncomplete, _ = libvirtsync.ParseMetadata(xml, libvirtsync.MetadataFieldReplicaIncomplete)
	if raw, err := libvirtsync.ParseMetadata(xml, libvirtsync.MetadataFieldReplicaTargets); err == nil && raw != "" {
		for _, entry := range strings.Split(raw, ",") {
			if entry = strings.TrimSpace(entry); entry != "" {
				d.ReplicaTargets = append(d.ReplicaTargets, entry)
			}
		}
	}
}
