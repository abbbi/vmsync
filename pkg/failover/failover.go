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

// Package failover holds the decision rules for promoting a replica to
// serve live, and for inverting a pair's direction afterwards.
//
// Deliberately free of any libvirt import. Every function here takes a
// plain description of what was observed and returns what to do about it,
// so the rules that decide whether a production VM gets overwritten are
// ordinary Go values that can be exhaustively tested anywhere -- including
// on a machine with no libvirt headers, where pkg/libvirtsync itself cannot
// even be compiled. The libvirt-facing code is then a thin shell whose only
// job is to gather these inputs and carry out the returned plan.
package failover

import (
	"fmt"
	"sort"
	"strings"
)

// Roles, mirroring pkg/libvirtsync's own constants. Duplicated rather than
// imported because importing libvirtsync would drag in libvirt and defeat
// the entire point of this package; the two are kept in step by
// TestRoleConstantsMatchLibvirtsync in the libvirtsync package.
const (
	RoleSource   = "source"
	RoleTarget   = "target"
	RolePromoted = "promoted"
	RolePaused   = "paused"
)

// Mode is how a promotion reached the point of being performed.
type Mode string

const (
	// ModePlanned means the source was reached and cleanly shut down first,
	// so nothing was being written when the replica became authoritative.
	ModePlanned Mode = "planned"
	// ModeForced means the source was never contacted. Everything written
	// there since the last sync is lost, and if it is merely partitioned
	// rather than down, both copies are now live.
	ModeForced Mode = "forced"
)

// TargetState is everything observed about the domain being promoted.
//
// DisksPresent and OverlayPresent come from stat-ing the actual files on
// the target host, not from metadata. That distinction is the whole reason
// they are here: -reinit deletes a target's disks while deliberately
// leaving its definition alone, so role, last_checkpoint and
// last_sync_timestamp all survive intact with nothing behind them.
type TargetState struct {
	Role           string
	LastCheckpoint string
	// LastSyncUnix is the target's last_sync_timestamp, written by the
	// source at the END of a successful copy, using the SOURCE's clock.
	LastSyncUnix int64
	// CheckpointAtUnix is when the checkpoint the replica's contents
	// actually correspond to was taken -- the START of that copy. Zero when
	// the target was last written by a vmsync too old to record it.
	CheckpointAtUnix int64
	ReplicaSource    string
	FailureCount     int
	// DisksPresent is false when any expected disk file is missing from the
	// target host.
	DisksPresent bool
	// OverlayPresent means an incremental overlay was left behind, which
	// means a copy was interrupted before being committed.
	OverlayPresent bool
	// SyncInFlight means a sync is writing this target right now.
	SyncInFlight bool
	// Active is the domain's current runtime state.
	Active bool
}

// PromotePlan is what to do, decided before anything is written.
type PromotePlan struct {
	// WriteMetadata is false when the domain is already promoted: the
	// original promotion's record must not be overwritten with a second,
	// later timestamp and a different actor.
	WriteMetadata bool
	// StartDomain is whether to boot it, which stays true even when the
	// metadata write is skipped -- see AssessPromote's doc comment.
	StartDomain     bool
	AlreadyPromoted bool
	PromotedFrom    string
	DataLoss        DataLoss
	// Notes are things the operator should be told about a promotion that
	// is proceeding anyway.
	Notes []string
}

// DataLoss is how much data a promotion accepts losing.
//
// A struct rather than a number because "unknown" is a real and common
// answer, and rendering it as 0 or -1 invites exactly the misreading that
// matters most: an operator choosing between "sync first" and "promote now"
// on the strength of a figure that was never measured.
type DataLoss struct {
	Known bool
	// Seconds is a LOWER BOUND. It is measured to the checkpoint the
	// replica's contents correspond to when that is recorded, and otherwise
	// to the end of the last copy -- which understates the true loss by the
	// duration of that copy.
	Seconds int64
	// LowerBoundOnly marks the understating case above.
	LowerBoundOnly bool
	// Reason explains an unknown, for display.
	Reason string
	// ClockSkew is set when the arithmetic produced a negative interval,
	// which means the two hosts' clocks disagree and every figure derived
	// from them is suspect.
	ClockSkew bool
}

// String renders a data-loss window the way it should appear to a person.
func (d DataLoss) String() string {
	if !d.Known {
		if d.Reason != "" {
			return "unknown (" + d.Reason + ")"
		}
		return "unknown"
	}
	s := fmt.Sprintf("%ds", d.Seconds)
	if d.LowerBoundOnly {
		s = "at least " + s
	}
	if d.ClockSkew {
		s += " (clocks disagree; treat as unreliable)"
	}
	return s
}

// PromoteOptions are the caller's intent.
type PromoteOptions struct {
	Mode Mode
	// Start boots the domain once the metadata is written.
	Start bool
	// Force bypasses the evidence checks -- NOT the role checks, which are
	// never bypassable. Forcing makes the data-loss window unknown rather
	// than producing a number from metadata that has been contradicted.
	Force bool
	// NowUnix is the promoting host's clock.
	NowUnix int64
}

// AssessPromote decides whether a replica may be promoted.
//
// Two independent gates, in order. First the role, which is an explicit
// administrative statement and is never bypassable. Then evidence that a
// usable replica actually exists, which IS bypassable with Force, because
// during a real outage an operator may knowingly choose to boot a
// questionable copy rather than nothing at all -- but doing so must make
// the reported data-loss window unknown rather than fabricate one.
//
// An already-promoted domain is a success, not an error, but only the
// metadata write is skipped. Starting it is still honoured: the design
// deliberately writes metadata before booting, so "promoted but not
// running" is a state a failed promotion legitimately leaves behind, and if
// re-issuing the promotion could not then start it, that state would be
// unrecoverable through the only control an operator has.
func AssessPromote(st TargetState, opt PromoteOptions) (PromotePlan, error) {
	plan := PromotePlan{StartDomain: opt.Start}

	switch st.Role {
	case RolePromoted:
		plan.AlreadyPromoted = true
		plan.WriteMetadata = false
		plan.StartDomain = opt.Start && !st.Active
		plan.DataLoss = DataLoss{Reason: "already promoted; the original promotion's record is kept"}
		return plan, nil
	case RoleSource:
		return PromotePlan{}, fmt.Errorf(
			"domain is marked replication_role=%q, meaning it is the SOURCE of a replication pair, not a replica -- promoting it is meaningless and suggests the pair is the wrong way round",
			RoleSource)
	case RoleTarget, RolePaused, "":
		// Proceed. paused is deliberately allowed: pausing replication and
		// then failing over is an ordinary sequence, and refusing it would
		// turn paused into a trap at the worst possible moment.
	default:
		return PromotePlan{}, fmt.Errorf(
			"domain has an unrecognized replication_role=%q -- refusing to promote a domain whose role this vmsync build does not understand (it was most likely written by a newer version)",
			st.Role)
	}

	// A sync writing this target right now is refused outright, Force or
	// not. Promoting mid-copy means booting a guest on disks another
	// process holds open and is still writing; there is no version of that
	// an operator can usefully consent to.
	if st.SyncInFlight {
		return PromotePlan{}, fmt.Errorf(
			"a sync is currently writing this domain's disks -- refusing to promote a half-written replica; wait for it to finish or stop it first")
	}

	if problems := evidenceProblems(st); len(problems) > 0 {
		if !opt.Force {
			return PromotePlan{}, fmt.Errorf(
				"no usable replica found on this target: %s -- promoting would boot an incomplete or absent image; pass the explicit override if you intend to promote anyway",
				strings.Join(problems, "; "))
		}
		plan.Notes = append(plan.Notes, "promoted despite: "+strings.Join(problems, "; "))
		plan.DataLoss = DataLoss{Reason: "replica could not be corroborated: " + strings.Join(problems, "; ")}
		plan.WriteMetadata = true
		plan.PromotedFrom = st.ReplicaSource
		return plan, nil
	}

	plan.WriteMetadata = true
	plan.PromotedFrom = st.ReplicaSource
	plan.DataLoss = computeDataLoss(st, opt)
	return plan, nil
}

// evidenceProblems lists the reasons this target does not look like a
// replica a sync has actually landed on. Empty means it does.
func evidenceProblems(st TargetState) []string {
	var problems []string
	if !st.DisksPresent {
		problems = append(problems, "one or more disk files are missing from the target host")
	}
	if st.LastCheckpoint == "" {
		problems = append(problems, "no last_checkpoint recorded, so no sync has ever completed")
	}
	if st.LastSyncUnix <= 0 {
		problems = append(problems, "no last_sync_timestamp recorded")
	}
	if st.ReplicaSource == "" {
		problems = append(problems, "no replica_source recorded, so this domain is not known to be a replica of anything")
	}
	if st.OverlayPresent {
		problems = append(problems, "an uncommitted incremental overlay is present, so the last copy was interrupted")
	}
	if st.FailureCount > 0 {
		problems = append(problems, fmt.Sprintf("failure_count is %d, so the last sync attempt did not succeed", st.FailureCount))
	}
	return problems
}

// computeDataLoss measures the window from the point the replica's contents
// actually correspond to.
//
// Prefers CheckpointAtUnix, because a checkpoint is taken BEFORE any data
// moves: everything the guest writes from that instant belongs to the next
// checkpoint, so that is the moment the replica is frozen at.
// last_sync_timestamp is written at the END of the copy, so measuring from
// it understates the loss by the whole copy duration -- minutes for a small
// delta, hours for a full sync over a WAN, and biased in the unsafe
// direction exactly when the difference is largest.
func computeDataLoss(st TargetState, opt PromoteOptions) DataLoss {
	if opt.Mode == ModePlanned {
		// The source was shut down before this, so nothing was written
		// after the last sync. Nothing was lost, and that is a fact about
		// the procedure rather than an arithmetic result.
		return DataLoss{Known: true, Seconds: 0}
	}

	from, lowerBound := st.CheckpointAtUnix, false
	if from <= 0 {
		from, lowerBound = st.LastSyncUnix, true
	}
	if from <= 0 {
		return DataLoss{Reason: "the target records no sync time"}
	}

	d := DataLoss{Known: true, Seconds: opt.NowUnix - from, LowerBoundOnly: lowerBound}
	if d.Seconds < 0 {
		// The target was last written by a host whose clock is ahead of
		// this one. Clamping to zero without saying so would report a
		// stale replica as perfectly current.
		d.Seconds = 0
		d.ClockSkew = true
	}
	return d
}

// --- inversion -----------------------------------------------------------

// PairState is both ends of a pair as observed, for an inversion.
type PairState struct {
	// OldSource is the domain that has been the source until now and is
	// about to become the target.
	OldSource DomainEnd
	// Promoted is the domain that was failed over to and is about to become
	// the source.
	Promoted DomainEnd
}

// DomainEnd is one end of a pair.
type DomainEnd struct {
	Host           string
	Domain         string
	Role           string
	Active         bool
	ReplicaSource  string
	ReplicaTargets []string
	// HasCheckpoints is whether real libvirt checkpoint objects exist on
	// this domain. Distinct from the last_checkpoint metadata string: the
	// objects are what a later sync would try to chain onto, and they are
	// meaningless once the direction reverses.
	HasCheckpoints bool
}

// Ref renders this end the way replica_source/replica_targets spell it.
func (e DomainEnd) Ref() string { return e.Host + ":" + e.Domain }

// InvertPlan is the set of changes to make, and where.
type InvertPlan struct {
	// AlreadyInverted means the pair is already in the post-inversion
	// arrangement, so there is nothing to do and that is a success.
	AlreadyInverted bool
	// DropCheckpointsOn is true when the domain becoming the target still
	// carries real checkpoint objects that must be deleted first.
	DropCheckpointsOnOldSource bool
	// NewTargetUpdates / NewTargetRemovals apply to the old source.
	NewTargetUpdates  map[string]string
	NewTargetRemovals []string
	// NewSourceUpdates / NewSourceRemovals apply to the promoted domain.
	NewSourceUpdates  map[string]string
	NewSourceRemovals []string
}

// Metadata field names, mirroring pkg/libvirtsync. See the note on the role
// constants above for why these are duplicated rather than imported.
const (
	FieldReplicationRole = "replication_role"
	FieldReplicaSource   = "replica_source"
	FieldReplicaTargets  = "replica_targets"
	FieldLastCheckpoint  = "last_checkpoint"
	FieldLastSync        = "last_sync_timestamp"
	FieldFailureCount    = "failure_count"
	FieldPromotedAt      = "promoted_at"
	FieldPromotedBy      = "promoted_by"
	FieldPromotedFrom    = "promoted_from"
	FieldPromotionMode   = "promotion_mode"
)

// AssessInvert decides whether a pair's direction may be reversed, and
// returns exactly what to write where.
//
// Converges rather than merely validating: a pair already in the
// post-inversion arrangement reports success with nothing to do. That
// matters because an inversion that completed but whose result was never
// recorded WILL be re-issued, and reporting a hard failure for work that
// actually succeeded would leave the control plane believing a correct pair
// is broken.
func AssessInvert(st PairState) (InvertPlan, error) {
	// Already done? Both ends must agree, or this is some third state.
	if st.Promoted.Role == RoleSource && st.OldSource.Role == RoleTarget &&
		containsRef(st.Promoted.ReplicaTargets, st.OldSource.Ref()) &&
		st.OldSource.ReplicaSource == st.Promoted.Ref() {
		return InvertPlan{AlreadyInverted: true}, nil
	}

	if st.Promoted.Role != RolePromoted {
		return InvertPlan{}, fmt.Errorf(
			"the domain to become the new source is marked replication_role=%q, not %q -- inversion reverses a pair that has been failed over, and this one has not",
			st.Promoted.Role, RolePromoted)
	}
	// The domain about to become a replication target must be down. A
	// running target is one scheduled sync away from being overwritten
	// under a live workload, and vmsync will not shut a production VM down
	// as a side effect of a metadata command.
	if st.OldSource.Active {
		return InvertPlan{}, fmt.Errorf(
			"%s is still running, and inversion would make it a replication target -- shut it down first; vmsync will not stop a running domain as a side effect of reversing a pair",
			st.OldSource.Ref())
	}

	// Remove only this peer from the fan-out, never the whole list.
	remaining, removed := removeRef(st.OldSource.ReplicaTargets, st.Promoted.Ref())
	if !removed && len(st.OldSource.ReplicaTargets) > 0 {
		return InvertPlan{}, fmt.Errorf(
			"%s does not list %s among its replica_targets (%s) -- refusing to invert a pair the source does not record",
			st.OldSource.Ref(), st.Promoted.Ref(), strings.Join(st.OldSource.ReplicaTargets, ", "))
	}
	if len(remaining) > 0 {
		// A domain cannot be both a replication target and the live source
		// of a fan-out to other hosts. Picking an interpretation silently
		// would either orphan those targets or leave a target replicating
		// onward; making the operator resolve it is the only honest option.
		return InvertPlan{}, fmt.Errorf(
			"%s also replicates to %s -- it cannot become a replication target while it is the source for other targets; remove those relationships first",
			st.OldSource.Ref(), strings.Join(remaining, ", "))
	}

	plan := InvertPlan{
		DropCheckpointsOnOldSource: st.OldSource.HasCheckpoints,

		// The old source becomes the target.
		NewTargetUpdates: map[string]string{
			FieldReplicationRole: RoleTarget,
			FieldReplicaSource:   st.Promoted.Ref(),
		},
		// Its checkpoint bookkeeping described a chain running the other
		// way and is now meaningless. failure_count too: it counted
		// failures against a target that no longer exists in that role.
		NewTargetRemovals: []string{
			FieldReplicaTargets,
			FieldLastCheckpoint,
			FieldLastSync,
			FieldFailureCount,
			FieldPromotedAt, FieldPromotedBy, FieldPromotedFrom, FieldPromotionMode,
		},

		// The promoted domain becomes the source.
		NewSourceUpdates: map[string]string{
			FieldReplicationRole: RoleSource,
			FieldReplicaTargets:  st.OldSource.Ref(),
		},
		// It is no longer anyone's replica, and it is no longer promoted --
		// it is simply the primary now, which is what role=source says.
		NewSourceRemovals: []string{
			FieldReplicaSource,
			FieldLastCheckpoint,
			FieldLastSync,
			FieldFailureCount,
			FieldPromotedAt, FieldPromotedBy, FieldPromotedFrom, FieldPromotionMode,
		},
	}
	return plan, nil
}

// removeRef drops one entry from a replica_targets list, returning what is
// left and whether anything was removed. Comparison is case-insensitive on
// the host, matching how hostnames are compared elsewhere.
func removeRef(list []string, ref string) (remaining []string, removed bool) {
	for _, e := range list {
		if strings.EqualFold(strings.TrimSpace(e), ref) {
			removed = true
			continue
		}
		if s := strings.TrimSpace(e); s != "" {
			remaining = append(remaining, s)
		}
	}
	sort.Strings(remaining)
	return remaining, removed
}

func containsRef(list []string, ref string) bool {
	for _, e := range list {
		if strings.EqualFold(strings.TrimSpace(e), ref) {
			return true
		}
	}
	return false
}
