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

// Package restorepoint holds the rules and the shell commands for keeping
// point-in-time copies of a replica on the target host.
//
// Deliberately free of any libvirt import, and of pkg/util -- the first would
// need libvirt headers to compile and the second needs syscall.Flock, and
// either would mean this package could only be built on a Linux box with a
// hypervisor toolchain. Everything here is plain values and plain strings, so
// the retention arithmetic and every command that reaches a production target
// can be exhaustively tested anywhere. pkg/failover is the same shape for the
// same reason. The libvirt- and SSH-facing code is a thin shell whose only job
// is to run what this package builds.
//
// The design this implements, including why reflink copies rather than qcow2
// snapshots and what a restore must do about last_checkpoint, is written up in
// docs/design/restore-points.md.
package restorepoint

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DirName is the directory, beside the replica's own disks, that holds every
// restore point for those disks.
//
// A subdirectory rather than a sibling suffix, and that is not cosmetic:
// cmd/vmsync/failover.go globs <disk>_* beside the replica to detect an
// uncommitted incremental overlay, and a match BLOCKS promotion. A restore
// point named next to the disk would make the replica unpromotable. It also
// has to be on the same filesystem as the disks, which a subdirectory
// guarantees and an operator-chosen path would not -- reflink cannot cross a
// mount point.
const DirName = ".vmsync-rp"

// StagingPrefix marks a restore point that is still being built. The set is
// assembled under this name and renamed into place only once every disk has
// been copied, so a half-made restore point can never be mistaken for a usable
// one: a rename within a filesystem is atomic, a directory tree of copies is
// not. Anything left wearing this prefix is junk from an interrupted run.
const StagingPrefix = ".incomplete-"

// Verification outcomes recorded in a restore point's sidecar.
//
// VerifyNotRun is the common case, not a degenerate one, and is worth
// recording explicitly: an operator choosing between restore points during an
// incident needs to know which of them was ever actually checked.
const (
	VerifyNotRun = "not-run"
	VerifyPassed = "passed"
	VerifyFailed = "failed"
)

// checkpointRe bounds what may appear in a tag, and therefore in a directory
// name that is interpolated into remote shell commands including rm -rf.
// Anything with a path separator, a shell metacharacter or a parent reference
// is refused before it can be built into a path at all.
var checkpointRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Policy is a parsed -retention value: how many restore points to keep, and
// how much time must pass before another is taken.
type Policy struct {
	Count    int
	Interval time.Duration
}

// Enabled reports whether any restore point should be taken at all.
func (p Policy) Enabled() bool { return p.Count > 0 }

// ParsePolicy reads a -retention value, e.g. "24,3h" for twenty-four restore
// points at least three hours apart.
//
// Only an ABSENT flag disables the feature. Every value that is present must
// describe something the caller actually wants, so a count of zero is refused
// rather than quietly honoured: passing -retention says "keep restore points",
// and asking to keep none of them is a contradiction, not a configuration. The
// whole point of a retention flag is that the operator finds out at startup
// whether they are getting what they asked for -- discovering months later
// that a typo meant no restore points were ever taken is the failure this
// refuses to allow.
//
// The comma form matches -netbuffer's existing "128k,1G". The duration is
// lowercased first because Go's own parser is case-sensitive and rejects "3H",
// which is exactly what somebody copying the documented example would type.
func ParsePolicy(s string) (Policy, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Policy{}, nil
	}
	count, interval, ok := strings.Cut(s, ",")
	if !ok {
		return Policy{}, fmt.Errorf("invalid -retention %q: want COUNT,INTERVAL (for example 24,3h); omit -retention entirely to keep no restore points", s)
	}

	n, err := strconv.Atoi(strings.TrimSpace(count))
	if err != nil {
		return Policy{}, fmt.Errorf("invalid -retention %q: count %q is not a number", s, strings.TrimSpace(count))
	}
	if n < 1 {
		return Policy{}, fmt.Errorf("invalid -retention %q: count must be at least 1; omit -retention entirely to keep no restore points", s)
	}

	d, err := time.ParseDuration(strings.ToLower(strings.TrimSpace(interval)))
	if err != nil {
		return Policy{}, fmt.Errorf("invalid -retention %q: interval %q is not a duration (for example 3h, 90m)", s, strings.TrimSpace(interval))
	}
	if d < 0 {
		return Policy{}, fmt.Errorf("invalid -retention %q: interval may not be negative", s)
	}
	return Policy{Count: n, Interval: d}, nil
}

// String renders a Policy back into the flag syntax that produced it.
func (p Policy) String() string {
	if !p.Enabled() {
		return ""
	}
	return strconv.Itoa(p.Count) + "," + p.Interval.String()
}

// Tag identifies one restore point: when the source was checkpointed, and
// which checkpoint that was.
//
// The instant leads, and the checkpoint name alone would not do. -reinit
// restarts the source's chain at vmsync-cpt-000001, so checkpoint names repeat
// across reinits and across inversions; the source-clock instant keeps restore
// points sortable and unambiguous for the life of a pair.
type Tag struct {
	At         time.Time
	Checkpoint string
}

// String renders the directory name for a tag.
func (t Tag) String() string {
	return strconv.FormatInt(t.At.Unix(), 10) + "-" + t.Checkpoint
}

// NewTag builds a validated tag.
func NewTag(at time.Time, checkpoint string) (Tag, error) {
	if !checkpointRe.MatchString(checkpoint) {
		return Tag{}, fmt.Errorf("refusing checkpoint name %q as a restore point tag: only letters, digits, '.', '_' and '-' are allowed, and it must not start with '.'", checkpoint)
	}
	if at.IsZero() {
		return Tag{}, fmt.Errorf("refusing a restore point tag with no checkpoint time")
	}
	// BEFORE THE EPOCH IS THE OTHER HALF OF THE ROUND-TRIP INVARIANT.
	//
	// ParseTag requires that a name render back to itself. Nothing enforced
	// the same on the mint side, and a negative time breaks it in a way no
	// amount of parsing care can repair: NewTag(time.Unix(-100, 0), "cpt")
	// rendered "-100-cpt", and ParseTag cuts at the FIRST '-', leaving an
	// empty unix half that it then refuses. So a tag could be minted whose
	// own name is unparseable -- a restore point that could be written and
	// never listed again.
	//
	// Refused rather than escaped, because a checkpoint time before 1970 is
	// not a spelling problem: it means the source's clock was wrong by
	// decades, and a restore point stamped with it is not something to
	// quietly accept a workaround for.
	if at.Unix() < 0 {
		return Tag{}, fmt.Errorf("refusing a restore point tag stamped %s: a checkpoint time before 1970 means the source's clock is wrong by decades, and the name it would produce (%q) cannot be read back as a tag at all",
			at.UTC().Format(time.RFC3339), strconv.FormatInt(at.Unix(), 10)+"-"+checkpoint)
	}
	return Tag{At: at.UTC().Truncate(time.Second), Checkpoint: checkpoint}, nil
}

// ParseTag reads a directory name back into a Tag. Cut at the FIRST separator,
// because checkpoint names contain hyphens of their own.
// parseTagShape reads a name as <unix>-<checkpoint> and validates both halves,
// WITHOUT requiring that the name be the canonical spelling of what it parses
// to.
//
// Split out from ParseTag because the two sites need different things from the
// same string, and conflating them was a regression: RemoveStagingCommand
// removes path.Join(root, name) -- the caller's VERBATIM string, never a
// re-rendering -- so what it needs from a name is that the characters are
// bounded, which is what checkpointRe gives it. Requiring canonicality there
// made ParseListing offer a staging directory that the sweep then refused on
// every run, for ever: the two halves of one subsystem disagreeing about what
// a staging directory is.
//
// Canonicality is ParseTag's business, and only because Point.tagName
// re-renders. See ParseTag.
func parseTagShape(name string) (Tag, error) {
	unix, checkpoint, ok := strings.Cut(name, "-")
	if !ok {
		return Tag{}, fmt.Errorf("%q is not a restore point tag: no separator", name)
	}
	secs, err := strconv.ParseInt(unix, 10, 64)
	if err != nil {
		return Tag{}, fmt.Errorf("%q is not a restore point tag: %q is not a unix time", name, unix)
	}
	return NewTag(time.Unix(secs, 0).UTC(), checkpoint)
}

func ParseTag(name string) (Tag, error) {
	t, err := parseTagShape(name)
	if err != nil {
		return Tag{}, err
	}
	// A TAG MUST ROUND-TRIP, and this is the only thing that makes the
	// re-rendering downstream safe.
	//
	// Point.tagName deliberately addresses a directory by the name its tag
	// RENDERS to rather than by the string a caller supplied -- "so a tag
	// whose fields survive validation but render differently cannot slip a
	// second spelling of one directory past the checks above" -- and
	// RemoveCommand builds rm -rf from that. The two together are only sound
	// if parsing a name yields a tag that renders back to it.
	//
	// strconv.ParseInt does not give that. It accepts a leading '+' and
	// leading zeros, so "+1700000000-cpt" and "01700000000-cpt" both parsed,
	// both rendered as "1700000000-cpt", and every later command addressed
	// THAT directory instead: ParseListing counted the odd one as a point,
	// Prune could select it, and the rm named its canonical sibling -- which,
	// if a point by that name existed, is one the retention policy had decided
	// to KEEP. With both spellings present Prune put one rendered name in both
	// buckets. Measured on 2026-10-19.
	//
	// Refused here rather than papered over at the removal, because this is
	// where the asymmetry lives: checkpointRe already bounds the checkpoint
	// half for exactly this reason -- it "bounds what may appear in a tag, and
	// therefore in a directory name that is interpolated into remote shell
	// commands including rm -rf" -- and the unix half had no equivalent guard.
	// A name this rejects is reported as Unknown by ParseListing, like any
	// other directory vmsync did not write, which is the honest answer: vmsync
	// writes only the canonical spelling, so it did not create this.
	if rendered := t.String(); rendered != name {
		return Tag{}, fmt.Errorf("%q is not a restore point tag: it is a non-canonical spelling of %q, and vmsync writes only the canonical one -- addressing a point by this name would send every later command, including rm -rf, to %q instead", name, rendered, rendered)
	}
	return t, nil
}

// Addressing a store, a point or a staging directory lives in store.go, behind
// types that cannot be built without naming a target domain. Free functions
// here -- Root(diskPath), Dir(root, t), StagingDir(root, t) -- would take a
// bare directory, which is what lets one domain read, prune and delete
// another's restore points; see the note at the top of store.go.

// DiskPath is where one replica disk's copy lives inside a restore point.
// Named after the disk's own basename, so a restore point is a drop-in set.
func DiskPath(dir, diskPath string) string { return path.Join(dir, path.Base(diskPath)) }

// StatusName is the sidecar inside each restore point.
const StatusName = "status.json"

// Status is what is actually known about a restore point.
//
// It exists because a restore point is taken BEFORE -verify runs, so its
// contents are not yet known to be good -- and because verify runs before the
// sync's own metadata is written, a restore point can outlive a run that then
// failed. The image is still valid; what varies is how much confidence to
// place in it, and that is what this records.
type Status struct {
	Checkpoint   string `json:"checkpoint"`
	CheckpointAt int64  `json:"checkpoint_at"`
	TakenAt      int64  `json:"taken_at"`
	// PublishSeq is the order this point was published in, counting from 1.
	//
	// RETENTION ORDERS BY THIS AND BY NOTHING ELSE, and it exists because
	// every instant in this struct comes from a clock that can be wrong.
	// CheckpointAt is stamped by the host running vmsync; TakenAt by the
	// target. Ordering by either means a clock step reorders the store: a
	// host whose clock ran 48h fast leaves points that rank above every
	// honestly-dated copy until real time catches up, so the store deletes
	// real history to keep mislabelled directories while every published
	// number stays healthy. Measured over eight publish-and-prune cycles,
	// that converges on two ghosts plus a single real point.
	//
	// A counter cannot be stepped. Each run takes the highest value already
	// in the store and adds one, so the sequence is monotonic by
	// construction whatever any clock does -- the same guarantee a Lamport
	// clock gives, and for the same reason: when you cannot trust a clock
	// you can still guarantee order.
	//
	// This is also why TakenAt stays exactly what its name says. An earlier
	// design bent TakenAt into this role, which would have made an
	// operator-visible timestamp read ahead of real time under skew. Two
	// fields, each true: this one orders, TakenAt reports.
	//
	// ZERO MEANS UNKNOWN -- a point written before this field existed. Prune
	// ranks those below every point that has one; see sortTagsNewestFirst.
	PublishSeq int64    `json:"publish_seq"`
	Source     string   `json:"source"`
	Verify     string   `json:"verify"`
	Disks      []string `json:"disks"`
}

// MaxPublishSeq is the highest sequence any of these points carries, or 0
// when none of them does.
//
// What a publishing run adds one to. Over the whole store rather than over
// the newest point alone: the newest BY SEQUENCE is the one with the highest
// value by definition, and asking for the maximum directly means no reader
// has to agree with any other about which point that is.
func MaxPublishSeq(statuses map[string]Status) int64 {
	var max int64
	for _, st := range statuses {
		if st.PublishSeq > max {
			max = st.PublishSeq
		}
	}
	return max
}

// Encode renders a sidecar. Indented because the first reader of one of these
// is an operator during an incident, cat-ing it over SSH.
func (s Status) Encode() ([]byte, error) {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode restore point status: %w", err)
	}
	return append(b, '\n'), nil
}

// DecodeStatus reads a sidecar back.
func DecodeStatus(b []byte) (Status, error) {
	var s Status
	if err := json.Unmarshal(b, &s); err != nil {
		return Status{}, fmt.Errorf("decode restore point status: %w", err)
	}
	return s, nil
}

// Due reports whether another restore point should be taken now.
//
// The interval is a FLOOR, not a schedule, and the distinction is the one
// thing about this feature most likely to be misread. vmsync does not decide
// when it runs -- cron or the agent does -- so this can only answer "has
// enough time passed", never "take one every three hours". If syncs run every
// four hours, restore points are four hours apart; if replication is paused
// for a day, there is a day-long gap. The COUNT is the guarantee; the window
// it nominally covers is not.
//
// A zero latest means none exists yet, which is always due.
func Due(latest time.Time, now time.Time, p Policy) bool {
	if !p.Enabled() {
		return false
	}
	if latest.IsZero() {
		return true
	}
	// A newest point dated in the FUTURE is due, and this is the one place that
	// can be fixed. The instant comes from the SOURCE's checkpoint, so a source
	// whose clock runs two days ahead dates every point two days ahead -- and
	// "has enough time passed since the newest" then answers NO for two days
	// plus the interval. The replica takes no restore point for that whole
	// window, the run reports not_due, and every other number about it stays
	// healthy: this is the silent starvation the whole feature exists to avoid,
	// arriving by way of NTP rather than by way of a co-located domain.
	//
	// Taking one is the fail-closed answer. The cost is extra points while the
	// skew lasts, bounded by the retention count and pruned like any other; the
	// cost of the alternative is a replica with no recoverable history and
	// nothing saying so. It is visible either way -- the newest timestamp this
	// publishes is in the future, which no healthy pair ever shows.
	if latest.After(now) {
		return true
	}
	return !now.Before(latest.Add(p.Interval))
}

// Plan is what to do with the restore points that already exist.
type Plan struct {
	// Keep, newest first.
	Keep []Tag
	// Remove, oldest first, so a caller that deletes them in order frees the
	// least useful history first and stays recoverable if it is interrupted.
	Remove []Tag
}

// Prune decides which restore points to keep and which to delete, counting a
// tag that is about to be created.
//
// Pruning AFTER the new one is in place, never before, so an interrupted run
// leaves too many rather than too few. Too many costs disk; too few costs the
// thing the feature exists to provide.
//
// RETENTION IS ORDERED BY seq, NOT BY ANY CLOCK, and that is the whole
// reason this function takes a map.
//
// Tag.At is the checkpoint instant: it says when the DATA is from, which is
// the right answer for "how far back can this replica be taken" and the wrong
// one for "which copy should go". Ordering by it means a clock step reorders
// the store, because a host whose clock runs ahead dates points in the future
// and they then rank above every honestly-dated copy until real time catches
// up. Measured over eight publish-and-prune cycles with a corrected 48h skew,
// that left the store holding two ghosts plus a single real point, with every
// intervening run paying for a reflink copy the next run deleted.
//
// Two earlier fixes each moved the problem rather than removing it. Ordering
// by the sidecar's taken_at stamped with vmsync's own clock was INERT, since
// that is the clock that also stamps Tag.At -- skewing it moved both together.
// Ordering by taken_at read from the TARGET's clock was better but still rested
// on one wall clock: driven over 18 cycles with a target clock that stepped and
// was corrected, it reproduced the same steady state.
//
// seq is Status.PublishSeq: a counter each run raises by one, monotonic by
// construction, with no clock in it anywhere. See Status.PublishSeq.
//
// A point MISSING from seq ranks below every point that has one, ordered among
// its peers by Tag.At. Not defaulted to zero, and not trusted at face value: a
// point written before the field existed has no place in the sequence, and an
// unknown must not be able to outrank a known. With seq nil -- nothing
// readable at all -- every point is in that tier and the ordering degrades to
// what it was before, which is the correct behaviour for a store this run
// could not learn anything about.
//
// A point MISSING from publishedAt ranks below every point that has an entry,
// ordered among its peers by Tag.At. Not defaulted to zero, and not trusted at
// face value: a sidecar that cannot be read is a point whose place in the
// order is unknown, and an unknown must not be able to outrank a known. With
// publishedAt nil -- no sidecars readable at all -- every point is in that
// tier and the ordering degrades to exactly what it was before, which is the
// correct behaviour for a store this run could not learn anything about.
//
// keep is the tag this run has just published, and it is NEVER in Remove.
// Pass the zero Tag when pruning without having published one. That is not
// redundant with the ordering above: the sidecar of a point published moments
// ago may not be readable yet on a target that has not fsynced, and the one
// copy a run must never delete is the one it just made.
//
// Reserving its slot rather than appending it keeps the policy honest: the
// store still ends at Count, with the newest-by-policy filling what is left.
func Prune(existing []Tag, keep Tag, seq map[string]int64, p Policy) Plan {
	sorted := sortTagsNewestFirst(existing, seq)
	protected := ""
	if !keep.At.IsZero() {
		protected = keep.String()
	}
	isKept := func(t Tag) bool { return protected != "" && t.String() == protected }

	if !p.Enabled() {
		// Retention off: nothing is kept by policy, so everything present is
		// somebody's leftovers. The caller decides whether to act on that;
		// this only reports it -- except for a point this very run made,
		// which is not somebody's leftovers by any reading.
		var rm []Tag
		for _, t := range sorted {
			if isKept(t) {
				continue
			}
			rm = append(rm, t)
		}
		return Plan{Remove: oldestFirst(rm)}
	}
	if len(sorted) <= p.Count {
		return Plan{Keep: sorted}
	}

	// One slot reserved for the new point, the rest filled newest-first.
	budget := p.Count
	if protected != "" {
		budget--
	}
	kept := make([]Tag, 0, p.Count)
	var remove []Tag
	for _, t := range sorted {
		switch {
		case isKept(t):
			// Placed below, so it cannot be spent out of the budget.
		case budget > 0:
			kept = append(kept, t)
			budget--
		default:
			remove = append(remove, t)
		}
	}
	if protected != "" {
		kept = append(kept, keep)
		kept = sortTagsNewestFirst(kept, seq)
	}
	return Plan{Keep: kept, Remove: oldestFirst(remove)}
}

// oldestFirst orders what is being deleted by the instant its CONTENTS are
// from, oldest first, which is what Plan.Remove promises.
//
// Not the reverse of the rank order, which is what this used to be. The rank
// has two tiers -- points with a recorded publish instant, then points
// without -- so reversing it frees least-useful-BY-RANK first, and with both
// tiers populated that is not oldest-first by any clock. On a bulk prune
// (a store that grew while the sidecar read was failing, or while -retention
// was off) the two orders diverge badly: a deletion interrupted part-way
// through had destroyed a copy from an hour ago while copies from days
// earlier were still on disk, which is the opposite of the "stays
// recoverable if it is interrupted" the contract is there for.
//
// The rank decides WHAT goes. It does not have to decide the order the caller
// frees it in.
func oldestFirst(in []Tag) []Tag {
	if len(in) == 0 {
		return nil
	}
	out := make([]Tag, len(in))
	copy(out, in)
	sort.Slice(out, func(i, j int) bool {
		if out[i].At.Equal(out[j].At) {
			return out[i].Checkpoint < out[j].Checkpoint
		}
		return out[i].At.Before(out[j].At)
	})
	return out
}

// sortTagsNewestFirst orders newest first, with the checkpoint breaking a tie
// -- two points in the same second are ordered by the chain that produced
// them.
//
// Two tiers, and the tier beats the value. A point with a publish sequence is
// ordered by THAT and always ranks above one without, which falls back to
// Tag.At. See Prune for why no clock can be allowed to decide this.
//
// The two tiers are deliberately not comparable: a sequence counts publishes
// and an instant counts seconds, so there is no arithmetic that relates them.
// Ranking every known value above every unknown one is the only ordering that
// does not invent a relationship between the two.
func sortTagsNewestFirst(in []Tag, seq map[string]int64) []Tag {
	out := make([]Tag, len(in))
	copy(out, in)
	// tier 0 carries a sequence and is ranked by it; tier 1 does not and is
	// ranked by the checkpoint instant, in seconds.
	rank := func(t Tag) (int64, int) {
		if s, ok := seq[t.String()]; ok && s > 0 {
			return s, 0
		}
		return t.At.Unix(), 1
	}
	sort.Slice(out, func(i, j int) bool {
		ai, ti := rank(out[i])
		aj, tj := rank(out[j])
		if ti != tj {
			return ti < tj
		}
		if ai == aj {
			return out[i].Checkpoint > out[j].Checkpoint
		}
		return ai > aj
	})
	return out
}

// Latest returns the newest tag, or the zero time when there are none.
func Latest(existing []Tag) time.Time {
	var newest time.Time
	for _, t := range existing {
		if t.At.After(newest) {
			newest = t.At
		}
	}
	return newest
}

// Oldest returns the oldest tag, or the zero time when there are none.
//
// Latest's twin, and the one an operator actually reaches for during an
// incident: time() minus this is how far back the replica can be taken, which
// is the question a retention policy is bought to answer. Exported so the
// metric and the console can report the same number rather than each deriving
// its own.
func Oldest(existing []Tag) time.Time {
	var oldest time.Time
	for _, t := range existing {
		if oldest.IsZero() || t.At.Before(oldest) {
			oldest = t.At
		}
	}
	return oldest
}

// Summary is what one domain's store holds, reduced to the numbers that are
// worth publishing.
//
// Pure, and computed from a Listing the caller already has, so the metrics cost
// no extra round trip to the target.
type Summary struct {
	// Count is published restore points in this domain's store. Directories,
	// including any whose sidecar cannot be read: the depth the store holds
	// and the depth that is provably restorable are different questions, and
	// conflating them is how a gauge comes to disagree with what an operator
	// sees listed.
	Count int
	// Staging is abandoned staging directories. Non-zero means runs are dying
	// between the first reflink and the rename.
	Staging int
	// Newest and Oldest are the instants the extremes correspond to, zero when
	// there are none.
	Newest time.Time
	Oldest time.Time
	// Over is how many points are beyond what the policy asks to keep. It
	// should be zero after any successful prune; a number that stays positive
	// means pruning is failing, which is only a warning on the run itself.
	Over int
}

// Summarize reduces a listing under a policy.
func Summarize(l Listing, p Policy) Summary {
	s := Summary{
		Count:   len(l.Points),
		Staging: len(l.Staging),
		Newest:  Latest(l.Points),
		Oldest:  Oldest(l.Points),
	}
	if p.Enabled() && len(l.Points) > p.Count {
		s.Over = len(l.Points) - p.Count
	}
	return s
}

// OverdueBy is how long ago another restore point became due and was not
// produced.
//
// Zero unless the floor had genuinely elapsed, which is what makes it safe to
// alert on directly: it is NOT derived from the interval by anybody guessing a
// multiple of it. The interval is a floor and not a cadence (see Due), so a
// healthy pair whose syncs are far apart legitimately has points far apart, and
// any staleness threshold computed from the policy would fire on it. This
// answers the only question that has one answer -- "had the floor passed, and
// did a point appear" -- and the caller zeroes it once one has.
func OverdueBy(latest time.Time, now time.Time, p Policy) time.Duration {
	if !p.Enabled() || latest.IsZero() {
		// No policy means nothing is owed. No points at all is reported by the
		// count being zero, which is unambiguous on its own; calling that
		// "infinitely overdue" would need an instant to measure from that
		// nobody has.
		return 0
	}
	// A newest point dated in the FUTURE is not overdue, and does not need to be:
	// Due treats that case as due, so points keep being taken and the store keeps
	// advancing. See Due for why.
	if latest.After(now) {
		return 0
	}
	due := latest.Add(p.Interval)
	if now.Before(due) {
		return 0
	}
	return now.Sub(due)
}
