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
	"strconv"
	"strings"
	"time"
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
	// RoleFenced is libvirtsync.RoleFenced: a domain an automatic fence
	// stopped. Promotable, unlike what its name suggests -- see AssessPromote.
	RoleFenced = "fenced"
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
	// RestoredFrom is the restore point this replica's disks were rolled
	// back to, empty for the overwhelming majority of replicas.
	//
	// It changes nothing about whether a promotion is allowed -- a rolled
	// back replica is a perfectly promotable one, which is the entire point
	// of restoring. What it changes is the EXPLANATION offered alongside the
	// data-loss window: without it, a wide window on a planned failover is
	// reported as a missing final sync, and an operator goes looking for one
	// that never went missing.
	RestoredFrom string
	// Active is the domain's current runtime state.
	Active bool
	// VerifyState is a recorded verification failure (see
	// libvirtsync.MetadataFieldVerifyState), empty for a replica with no such
	// finding -- which is almost all of them.
	//
	// Unlike everything else here it is not about whether the sync
	// MECHANISM worked. FailureCount says the last attempt did not finish;
	// this says an attempt finished and the resulting replica did not match
	// its source. A promotion needs to know the difference: the first means
	// the replica may be stale, the second means it may be wrong.
	VerifyState    string
	VerifyFailedAt int64
	// ReplicaIncomplete is the raw libvirtsync.MetadataFieldReplicaIncomplete
	// value: a full copy was started against these disks and never recorded
	// as finished. Empty on every replica that is not in that state.
	//
	// It is unlike every other field here, and the difference is why CI-02
	// existed. The others are read to work out whether the metadata
	// CORROBORATES a replica; this one says the metadata describes a
	// DIFFERENT replica -- the one the interrupted copy renamed aside and was
	// part-way through replacing. So last_checkpoint, last_sync_timestamp,
	// replica_source and failure_count all read perfectly healthy while the
	// disks underneath them hold a half-written image, and without this field
	// evidenceProblems finds nothing to say.
	//
	// Kept raw and parsed by ParseReplicaIncomplete rather than arriving
	// structured, so that an unreadable value still reaches the gate: the
	// presence of the field is the finding, and the parse decides only how
	// the refusal is worded.
	ReplicaIncomplete string
	// SourceStoppedAtSync records that the SOURCE domain was already shut
	// off when the checkpoint behind this replica was taken.
	//
	// This is the only honest basis for claiming a failover loses nothing.
	// A stopped source cannot have written anything after that instant, so
	// the replica is complete -- not "probably complete because someone
	// followed the right procedure". It is written by the sync itself, on
	// the target, so a promotion can read it locally without contacting the
	// site it may be failing away from.
	SourceStoppedAtSync bool
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
	// Verified means the figure rests on evidence about the DATA rather than
	// on arithmetic against the current clock: the source was recorded as
	// stopped when the replica's checkpoint was taken, so nothing could have
	// been written after it.
	//
	// Only a verified zero is a real zero. An unverified one would be a
	// claim about a procedure somebody says they followed.
	Verified bool
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
	if d.Verified && d.Seconds == 0 {
		return "none (the source was already stopped when this replica's checkpoint was taken)"
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
	case RoleTarget, RolePaused, RoleFenced, "":
		// Proceed. paused is deliberately allowed: pausing replication and
		// then failing over is an ordinary sequence, and refusing it would
		// turn paused into a trap at the worst possible moment.
		//
		// fenced is allowed for a sharper reason. A fence acts on the evidence
		// that a peer was promoted, and that evidence can be wrong -- a
		// mistaken failover, a DR drill somebody forgot to unwind, a partition
		// that has since healed. Refusing to promote a fenced domain would
		// make a wrong fence unrecoverable through the one control an operator
		// has, which is the same trap the paused case above describes, reached
		// by a route nobody chose.
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

	// A planned failover is supposed to be: stop the source, run one last
	// sync, then promote. If that happened, the replica's checkpoint was
	// taken against a stopped source and the window above is a verified
	// zero. When it is not, the sequence was not completed -- most often
	// because the final sync was skipped -- and the operator is told rather
	// than left with a figure that quietly means something else.
	if opt.Mode == ModePlanned && !plan.DataLoss.Verified {
		// A rolled-back replica reaches here too, and the note above would
		// misdescribe it: the final sync may well have run: what makes the
		// window wide is that somebody deliberately replaced these contents
		// with an older copy. Same conclusion, different cause, and an
		// operator sent looking for a missing sync would not find one.
		if st.RestoredFrom != "" {
			plan.Notes = append(plan.Notes,
				"this replica's disks were rolled back to restore point "+st.RestoredFrom+
					", so the window below is the age of THAT copy rather than replication lag -- everything written since it was taken is being given up deliberately")
		} else {
			plan.Notes = append(plan.Notes,
				"this was requested as a planned failover, but the replica's checkpoint was taken while the source was still running: "+
					"no final sync ran after the source stopped, so the window below is measured against the clock and covers writes that were never replicated")
		}
	}
	return plan, nil
}

// ReplicaReplacedSuffix is the prefix of the name an interrupted full copy
// renamed the good replica disks to: "<disk>.vmsync-replaced-<unix>".
//
// Duplicated from cmd/vmsync's own replacedDiskSuffix rather than imported --
// a package cannot import a main package at all -- and pinned against it by a
// test over there. It is here because the refusal below has to NAME the
// suffix: it is the single piece of information that turns an interrupted
// -reinit from "this replica is unusable" into a one-line recovery, and it
// exists nowhere else on the replica's own host.
const ReplicaReplacedSuffix = ".vmsync-replaced-"

// ReplicaIncomplete is a parsed libvirtsync.MetadataFieldReplicaIncomplete
// value.
//
// Parsed is the honest half of it. The refusal never depends on this struct
// being readable -- the field's PRESENCE is the finding -- so a value this
// build cannot make sense of comes back with Parsed false, Raw intact, and is
// still refused. That is failing closed: the alternative, treating an
// unreadable record as no record, promotes exactly the half-written replica
// the field was written to stop.
type ReplicaIncomplete struct {
	// Parsed is true when the value carried at least a verb and a usable
	// start time -- enough for the refusal to say what was started and when.
	Parsed bool
	// Raw is what the domain actually held, always, so an unreadable value
	// can be quoted back to whoever has to work out what wrote it.
	Raw string
	// Verb is reinit, force-clean, full-sync or restore -- or whatever a
	// newer vmsync wrote, taken as found. Deliberately NOT validated against
	// a known set: refusing to parse a verb this build has not heard of would
	// discard a record written by a newer engine, and a newer engine's
	// interrupted copy leaves exactly the same wreckage as this one's.
	Verb string
	// AtUnix is when the copy started, on the clock of the host that started
	// it.
	AtUnix int64
	// ActionID correlates this with the run's journal and log. Empty when the
	// engine that wrote it had none.
	ActionID string
	// Host is the machine that started the copy and was expected to finish
	// it.
	Host string
	// AsideStamp is the <unix> in ".vmsync-replaced-<unix>", empty on the
	// paths that rename nothing aside (a full sync into an empty target, or a
	// reinit run with -replaced-disk-action=delete).
	AsideStamp string
}

// ParseReplicaIncomplete reads the single-line comma-separated k=v value
// libvirtsync.ReplicaIncompleteValue writes.
//
// Pure and stdlib-only, like everything else in this package, so the reading
// that decides whether a promotion is refused can be exercised exhaustively
// on any machine.
//
// UNKNOWN KEYS ARE IGNORED, which is what lets a newer vmsync add one without
// every older engine on the estate suddenly failing to read the field at all.
// Missing keys are not an error either; they only leave Parsed false when
// what is missing is the verb or the time, because those two are what the
// refusal is written around.
func ParseReplicaIncomplete(raw string) ReplicaIncomplete {
	ri := ReplicaIncomplete{Raw: raw}
	if raw == "" {
		return ri
	}
	for _, pair := range strings.Split(raw, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok {
			continue
		}
		switch key {
		case "verb":
			ri.Verb = value
		case "at":
			// A time that will not parse leaves AtUnix zero and Parsed
			// false, and the value is still refused -- the missing date only
			// costs the refusal its "started at" clause.
			if n, err := strconv.ParseInt(value, 10, 64); err == nil && n > 0 {
				ri.AtUnix = n
			}
		case "action":
			ri.ActionID = value
		case "host":
			ri.Host = value
		case "aside":
			ri.AsideStamp = value
		}
	}
	ri.Parsed = ri.Verb != "" && ri.AtUnix > 0
	return ri
}

// KeepExistingReplicaIncomplete answers whether a record already on the
// target must be left where it is rather than overwritten by the one an
// engine is about to arm.
//
// The rule is one line and it is about RECOVERY, not tidiness: a record
// naming an aside stamp is never replaced by one that names none.
//
// What the stamp is. A -reinit renames the good disks to
// .vmsync-replaced-<stamp> and arms verb=reinit carrying that number. The
// number exists nowhere else on the replica's host -- not in the domain XML,
// not in the disks' own names -- so it is the single thing that turns an
// interrupted rebuild from "this replica is unusable" into a one-line
// recovery.
//
// What would eat it. If that reinit dies, the next ordinary sync finds no
// checkpoint chain, concludes it must do a full copy, and arms verb=full-sync
// -- which carries no stamp, because a full sync renames nothing aside.
// Overwriting would change nothing about the promotion, which is refused on
// either record, and would cost the operator the only line that says where
// the last COMPLETE copy went. Keeping the older one is free: the field is
// single-valued, a successful sync clears it either way, and refusing on a
// stale verb name is no weaker than refusing on a fresh one.
//
// The decision rests on the stamp ALONE and deliberately not on Parsed. A
// value this build cannot make sense of -- written by a newer engine, or
// truncated -- that still carries a readable aside= is still the only pointer
// to those files, and discarding it because the rest of the line is unreadable
// would throw away recovery information over a vocabulary difference. Same
// failing-closed instinct as the refusal itself, applied to the evidence.
//
// newAsideStamp non-empty means the incoming record names files of its own, so
// nothing is lost by replacing: the caller is arming a rebuild that is itself
// setting a complete copy aside.
//
// Pure and stdlib-only like the rest of this package, so the engine's call
// site is a lookup and a branch, and the rule itself is exercised here on a
// machine with no libvirt at all.
func KeepExistingReplicaIncomplete(existingRaw, newAsideStamp string) bool {
	if newAsideStamp != "" {
		return false
	}
	return ParseReplicaIncomplete(existingRaw).AsideStamp != ""
}

// evidenceProblems lists the reasons this target does not look like a
// replica a sync has actually landed on. Empty means it does.
func evidenceProblems(st TargetState) []string {
	var problems []string
	// FIRST, before every other check, because it is the only one that says
	// the rest of them are answering questions about a DIFFERENT replica.
	// last_checkpoint, last_sync_timestamp, replica_source and failure_count
	// below all still describe the copy this interrupted run renamed aside
	// and was part-way through replacing, so every one of them reads clean
	// and an operator scanning the list would otherwise reach this last, if
	// at all.
	if st.ReplicaIncomplete != "" {
		problems = append(problems, replicaIncompleteProblem(st.ReplicaIncomplete))
	}
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
	// The one problem here that is about the replica's CONTENTS rather than
	// about whether replication ran. Everything above says the replica may
	// be stale or incomplete; this says a comparison against its own source
	// found it different -- so promoting it serves data that was already
	// known not to match.
	//
	// Without this, a replica that failed verify last night promoted with a
	// clean bill of health today, which is the single most valuable reason
	// to persist the verdict at all.
	if st.VerifyState != "" {
		when := "at an unrecorded time"
		if st.VerifyFailedAt > 0 {
			when = "on " + time.Unix(st.VerifyFailedAt, 0).UTC().Format(time.RFC3339)
		}
		problems = append(problems, fmt.Sprintf("verification found this replica's contents differing from its source %s and the finding has not been cleared, so the replica is known not to match (see that run's log for which blocks)", when))
	}
	return problems
}

// replicaIncompleteProblem words the refusal for a replica a full copy
// started rewriting and never finished.
//
// Three things it must say, and each one exists because leaving it out costs
// an operator an hour in the middle of an outage:
//
//   - WHAT was started, WHEN and FROM WHERE, so they can go and find out why
//     it stopped instead of guessing;
//   - that the metadata above these disks describes the replica the copy
//     REPLACED, because otherwise the obvious reading of a domain whose every
//     other field is healthy is that this refusal is spurious and should be
//     forced past;
//   - the ASIDE SUFFIX, spelled out, because the complete replica is sitting
//     in those files and nothing else on this host records their names. That
//     is what turns "this replica is unusable" into one rename.
//
// An unreadable value is worded differently rather than dropped: the finding
// survives, the raw text is quoted so somebody can work out what wrote it,
// and the recovery is described by its shape ("the sibling files carrying
// the suffix") since no stamp can be read out of it.
func replicaIncompleteProblem(raw string) string {
	ri := ParseReplicaIncomplete(raw)

	var b strings.Builder
	b.WriteString("a full copy of this replica was STARTED and never recorded as finished")
	if ri.Parsed {
		fmt.Fprintf(&b, " (%s, begun %s", ri.Verb, time.Unix(ri.AtUnix, 0).UTC().Format(time.RFC3339))
		if ri.Host != "" {
			fmt.Fprintf(&b, " from %s", ri.Host)
		}
		if ri.ActionID != "" {
			fmt.Fprintf(&b, ", action %s", ri.ActionID)
		}
		b.WriteString(")")
	} else {
		fmt.Fprintf(&b, ", and the record it left could not be read (%s=%q)", FieldReplicaIncomplete, raw)
	}
	b.WriteString(" -- so these disk files are a HALF-WRITTEN image, while last_checkpoint, last_sync_timestamp and replica_source on this domain still describe the replica that copy replaced, which is why every other check here looks healthy")

	switch {
	case ri.AsideStamp != "":
		fmt.Fprintf(&b, ". That complete replica was not destroyed: it is in the files beside each disk named with the suffix %s%s, and putting them back over the disks is the recovery",
			ReplicaReplacedSuffix, ri.AsideStamp)
	case ri.Parsed && ri.Verb == "full-sync":
		// Nothing was renamed aside because there was nothing there to
		// rename: a full sync only runs against a target whose disk files do
		// not exist. Saying "look for the aside files" here would send an
		// operator hunting for something that was never created.
		b.WriteString(". No previous contents were renamed aside on this path -- a full sync writes bases where no disk file existed -- so there is nothing to put back: re-run the sync, which is what repairs it")
	default:
		fmt.Fprintf(&b, ". If that copy renamed the previous contents aside, they are in the files beside each disk carrying a %s<unix> suffix, and putting them back over the disks is the recovery; if it was run with -replaced-disk-action=delete there are none, and re-running the sync is",
			ReplicaReplacedSuffix)
	}
	return b.String()
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
	// A source that was already stopped when the checkpoint was taken could
	// not have written anything afterwards, so the replica is complete.
	//
	// This deliberately does NOT key off Mode. Mode is a label the caller
	// passes -- anyone can type -promote-mode=planned -- and an earlier
	// version of this function returned a hard zero for it on the strength
	// of that word alone. A shutdown with no final sync after it, or the
	// flag passed with no shutdown at all, then reported "0s lost" while
	// discarding everything written since the last scheduled run. The
	// procedure is worth recording for the audit trail; it is not evidence.
	if st.SourceStoppedAtSync {
		return DataLoss{Known: true, Verified: true, Seconds: 0}
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
	// Mirrors libvirtsync.MetadataFieldReplicaWrittenAt; kept in step by
	// pkg/libvirtsync/duplicated_names_test.go.
	FieldReplicaWrittenAt = "replica_written_at"
	// Mirrors libvirtsync.MetadataFieldPendingCheckpoint; same pinning.
	FieldPendingCheckpoint = "pending_checkpoint"
	// Mirrors libvirtsync.MetadataFieldReplicaIncomplete; same pinning. This
	// is the field a full copy arms before it starts destroying the replica
	// that is there, and the only one that contradicts the rest of the
	// metadata after such a copy is interrupted.
	FieldReplicaIncomplete = "replica_incomplete"
	// Mirrors libvirtsync.MetadataFieldVerifyState/VerifyFailedAt; same pinning.
	FieldVerifyState    = "verify_state"
	FieldVerifyFailedAt = "verify_failed_at"

	// VerifyStateFailedValue is libvirtsync.VerifyStateFailed, duplicated for
	// the same reason the roles are: importing libvirtsync would drag in
	// libvirt and defeat this package being testable without it.
	VerifyStateFailedValue = "failed"
	FieldFailureCount      = "failure_count"
	FieldPromotedAt        = "promoted_at"
	FieldPromotedBy        = "promoted_by"
	FieldPromotedFrom      = "promoted_from"
	FieldPromotionMode     = "promotion_mode"

	// The fence a promotion armed, written on the PROMOTED domain. See
	// fence.go for why the decision is armed rather than inferred.
	FieldFenceID      = "fence_id"
	FieldFenceSource  = "fence_source"
	FieldFenceArmedAt = "fence_armed_at"
	FieldFenceArmedBy = "fence_armed_by"
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
			FieldReplicaWrittenAt,
			FieldPendingCheckpoint,
			// A copy interrupted while this domain was the SOURCE has
			// nothing to say about it as a target, and left standing it
			// would refuse the promotion of a replica that is about to be
			// built here from scratch.
			FieldReplicaIncomplete,
			FieldVerifyState, FieldVerifyFailedAt,
			FieldFailureCount,
			FieldPromotedAt, FieldPromotedBy, FieldPromotedFrom, FieldPromotionMode,
			FieldFenceID, FieldFenceSource, FieldFenceArmedAt, FieldFenceArmedBy,
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
			FieldReplicaWrittenAt,
			FieldPendingCheckpoint,
			// This domain is the primary now. A record of a copy that was
			// being written INTO it, back when it was somebody's replica,
			// would be inherited onto its own future targets by
			// UpdateSyncMetadata and refuse their promotions.
			FieldReplicaIncomplete,
			FieldVerifyState, FieldVerifyFailedAt,
			FieldFailureCount,
			FieldPromotedAt, FieldPromotedBy, FieldPromotedFrom, FieldPromotionMode,
			FieldFenceID, FieldFenceSource, FieldFenceArmedAt, FieldFenceArmedBy,
		},
	}
	return plan, nil
}

// removeRef drops one entry from a replica_targets list, returning what is
// left and whether anything was removed. Comparison is case-insensitive on
// the host, matching how hostnames are compared elsewhere.
func removeRef(list []string, ref string) (remaining []string, removed bool) {
	for _, e := range list {
		if equalRef(e, ref) {
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
		if equalRef(e, ref) {
			return true
		}
	}
	return false
}
