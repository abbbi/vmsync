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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"vmsync/pkg/atomicjson"
	"vmsync/pkg/failover"
	"vmsync/pkg/inventory"
	"vmsync/pkg/libvirtsync"
	"vmsync/pkg/trace"
	"vmsync/pkg/util"
)

const (
	// fenceTickInterval is how often this host asks its peers whether it has
	// been displaced.
	//
	// Much slower than the scheduler's tick because each pass costs a remote
	// libvirt connection per running replicated VM, and because the window
	// this closes is measured against a human failover -- somebody promoting
	// at the DR site and then wondering whether the old primary is still
	// serving. A minute of overlap in that scenario is not what makes the
	// difference; an unbounded one is.
	fenceTickInterval = 60 * time.Second
	// fenceReadTimeout bounds one peer query. A partition is the single most
	// likely condition during a failover, so this path must degrade to "I
	// could not ask" quickly rather than wedging the whole sweep behind one
	// unreachable host.
	fenceReadTimeout = 30 * time.Second
	// fenceShutdownGrace is how much longer vmsync gets than the guest does.
	//
	// vmsync has to connect to libvirt, read state and write metadata either
	// side of the wait itself, so bounding it at exactly the guest timeout
	// would kill it just as the guest ran out -- turning "the guest did not
	// stop in time", which is a clear diagnosis, into "the process
	// disappeared", which is not.
	fenceShutdownGrace = 2 * time.Minute
	// fenceLedgerKept bounds the durable record. One entry per failover per
	// VM, so this is effectively unbounded for any real estate while still
	// refusing to grow without limit.
	fenceLedgerKept = 1000
)

// replicaSelfRef is how this host appears in another domain's replication
// metadata: the "host:domain" a peer's replica_source or fence token names
// when it means us.
//
// It MUST be derived exactly the way the value it is compared against was
// written, and this function exists so that is enforced by construction
// rather than by two call sites happening to agree.
//
// Building it from cfg.Hostname alone does not do that. The string it is
// matched against is stamped by vmsync as
// util.ReplicaHost(-source-uri, -local-host-name) -- and the agent passes
// exactly cfg.LibvirtURI and cfg.Hostname for those two (scheduler.go, where
// plan.SourceURI and plan.LocalHostName are set). ReplicaHost PREFERS the
// URI's host and ignores the local name entirely whenever the URI has one
// (pkg/util/util.go), so the two agree only for a URI with no host --
// qemu:///system, the default, which is what makes such a mismatch invisible.
//
// With a remote -libvirt-uri they disagree permanently: every fence token
// naming this host fails to match, verdict.Fence is false forever, and
// split-brain protection is silently off with nothing logged, because a
// non-matching token is the ordinary case the check exists to filter out.
func replicaSelfRef(cfg agentConfig, domain string) string {
	return libvirtsync.ReplicaEntry(util.ReplicaHost(cfg.LibvirtURI, cfg.Hostname), domain)
}

// fenceRecord is the durable proof that one fence token was acted on.
type fenceRecord struct {
	FenceID  string `json:"fence_id"`
	VM       string `json:"vm"`
	PeerRef  string `json:"peer_ref"`
	AtUnix   int64  `json:"at_unix"`
	State    string `json:"state"`
	Error    string `json:"error,omitempty"`
	ArmedBy  string `json:"armed_by,omitempty"`
	Attempts int    `json:"attempts,omitempty"`
}

// fenceLedger is the agent's memory of every fence it has acted on.
//
// Deliberately NOT the operationLedger, despite the strong family
// resemblance. That one is bounded by Forget(stillPublished): a record is
// dropped once the UI stops publishing the operation, on the reasoning that
// the UI removing it IS the acknowledgement. A fence has no such
// acknowledgement -- the whole point is that it works with no UI at all --
// so entries there would be forgotten on the very next report, and a token
// that had already been acted on would fire again. Sharing the type would
// have silently destroyed the single-use property this design rests on.
type fenceLedger struct {
	path string

	mu      sync.Mutex
	records map[string]fenceRecord
}

func newFenceLedger(stateDir string) *fenceLedger {
	return &fenceLedger{
		path:    filepath.Join(stateDir, "fences.json"),
		records: map[string]fenceRecord{},
	}
}

func (l *fenceLedger) Load() error {
	var records map[string]fenceRecord
	ok, err := readJSON(l.path, &records)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !ok || records == nil {
		l.records = map[string]fenceRecord{}
		return nil
	}
	// Nothing is promoted to `unknown` the way an operation is, because
	// nothing needs to be: any record at all, in any state, already means
	// "never act on this token again". A fence interrupted mid-shutdown is
	// covered by exactly the same rule as one that failed outright.
	l.records = records
	return nil
}

// Acted reports whether this fence has been acted on before -- in ANY state,
// including a failed attempt.
//
// This is the latch. A fence that failed is not retried: the realistic
// failure is a guest ignoring ACPI, and retrying that on a timer means
// either an unbounded queue of pending shutdowns or an escalation to
// destroying a running VM. Neither is a decision an unattended agent should
// reach by repetition, so the second attempt belongs to a person.
func (l *fenceLedger) Acted(fenceID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.records[fenceID]
	return ok
}

// Begin records intent BEFORE the shutdown is attempted, for the same reason
// the operation ledger does: a crash between deciding and acting must not
// leave a token that looks untouched.
func (l *fenceLedger) Begin(rec fenceRecord) error {
	rec.State = OpStateRunning
	return l.put(rec)
}

func (l *fenceLedger) Finish(rec fenceRecord) error { return l.put(rec) }

// put records in memory FIRST and never rolls that back when the disk write
// fails. That is deliberate here, and it is the opposite of what
// operationLedger.put does -- see its comment.
//
// The difference is what each ledger's caller does on a failed Begin. A fence
// proceeds anyway (fenceOneDomain, below), because not fencing means one
// workload live in two places writing to two diverging copies. So the shutdown
// really is attempted, the in-memory latch really does describe something that
// happened, and keeping it is what stops the next 60-second sweep attempting
// it again. An operation, by contrast, refuses to run when its intent cannot
// be recorded -- so there its latch would describe nothing, and it is undone.
//
// The cost is bounded and only spans a restart: the record was never
// persisted, so a new process does not know the fence happened. See
// fenceOneDomain for what that exposure actually amounts to.
func (l *fenceLedger) put(rec fenceRecord) error {
	l.mu.Lock()
	l.records[rec.FenceID] = rec
	// Oldest-first eviction, well above any plausible number of real
	// failovers. Evicting the oldest is safe in a way evicting the newest
	// would not be: a token old enough to fall off the end belongs to a
	// failover long since resolved, and the peer's role check would refuse
	// it anyway.
	for len(l.records) > fenceLedgerKept {
		var oldestID string
		var oldestAt int64
		for id, r := range l.records {
			if oldestID == "" || r.AtUnix < oldestAt {
				oldestID, oldestAt = id, r.AtUnix
			}
		}
		delete(l.records, oldestID)
	}
	snapshot := make(map[string]fenceRecord, len(l.records))
	for k, v := range l.records {
		snapshot[k] = v
	}
	l.mu.Unlock()

	if err := atomicjson.Write(l.path, snapshot, 0o644); err != nil {
		return fmt.Errorf("record fence %s in the ledger: %w", rec.FenceID, err)
	}
	return nil
}

// Records returns every stored fence, newest activity first is not
// guaranteed -- callers that care sort. Used for reporting and metrics.
func (l *fenceLedger) Records() []fenceRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]fenceRecord, 0, len(l.records))
	for _, r := range l.records {
		out = append(out, r)
	}
	return out
}

// LatestByVM returns the most recent fence acted on for each VM.
//
// Most recent because a VM can be fenced more than once over its life --
// failed over, recovered, failed over again -- and what a report should
// carry is the state this domain is in NOW, not an archaeology of every
// failover it has been through. The older records stay in the ledger, where
// their only remaining job is to keep those tokens from firing again.
func (l *fenceLedger) LatestByVM() map[string]fenceRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]fenceRecord, len(l.records))
	for _, r := range l.records {
		if prev, ok := out[r.VM]; ok && prev.AtUnix >= r.AtUnix {
			continue
		}
		out[r.VM] = r
	}
	return out
}

// fenceLoop asks this host's peers whether any of them has displaced it.
//
// Its own loop, and deliberately independent of both the schedule and the
// operations channel:
//
//   - Independent of the SCHEDULE because a displaced source is very often
//     one whose replication was disabled -- by the operator, or by the
//     failover itself. Gating the check on Enabled would switch off the
//     split-brain protection at precisely the moment it is needed.
//
//   - Independent of the UI because a partition is the ordinary case here.
//     A UI-issued fence operation is an instruction to go and CHECK; this
//     loop is what makes the answer arrive when there is no UI to issue one.
//     Either way the token is read from the peer's own libvirt, never taken
//     on the control plane's word.
//
// One configuration snapshot per SWEEP, taken at the top and used for the
// whole of it. A sweep that started under one libvirt_uri must not finish
// under another: half its peer queries would name a different host.
// fenceVerdict is what one peer query established about one VM.
//
// THREE VALUES, because the two questions "is this VM in split brain" and
// "could we find out" have different answers and only one of them may clear
// an alarm. This was a bool, and both "the peer says it is not promoted" and
// "the peer did not answer" were false -- so a VM already known to be split
// dropped out of the gauge the moment its peer stopped responding, which is
// exactly when the partition that caused the failover sets in. Two live
// copies, taking writes, and the alert resolving itself.
//
// sweepFences already states the rule at the host level and follows it
// there: when local libvirt cannot be reached it returns without touching
// the metric, because "failing to look is not evidence that the problem went
// away". This type is that rule applied per VM.
type fenceVerdict int

const (
	// fenceClear: the peer answered, and it is not displacing this VM.
	fenceClear fenceVerdict = iota
	// fenceSplit: the peer answered, and both copies are live.
	fenceSplit
	// fenceUnknown: no answer. Says nothing in either direction, so it
	// neither raises an alarm nor clears one.
	fenceUnknown
)

func fenceLoop(ctx context.Context, lv *live, state *sharedState, ledger *fenceLedger) {
	ticker := time.NewTicker(fenceTickInterval)
	defer ticker.Stop()

	// The previous pass's findings, carried across ticks so that a VM whose
	// peer has gone quiet keeps the last answer anybody actually got
	// instead of being silently cleared. Owned here rather than read back
	// out of the metrics, so the gauge stays a report of this state and not
	// also its storage.
	var lastSplit map[string]bool

	for {
		lastSplit = sweepFences(ctx, *lv.get(), state, ledger, lastSplit)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// sweepFences performs one pass over every VM this host could be serving.
//
// The split-brain metric is REBUILT from this pass rather than updated as
// findings arrive. That is deliberate and load-bearing: the ordinary way a
// split brain ends is that the domain gets shut down, after which the sweep
// skips it as inactive and would never emit a "no longer split" update. An
// incrementally maintained gauge would therefore latch at 1 and stay there
// forever after the first successful fence -- an alert nobody could clear,
// which trains people to ignore the one metric that must never be ignored.
// prevSplit is what the last completed pass found, and unreachable peers
// inherit their entry from it: silence must not clear an alarm. The return
// value is what this pass published, or prevSplit unchanged when it
// published nothing, so the caller can carry it forward again.
//
// Carrying forward does NOT reintroduce the latching this function's comment
// above warns about. A VM that gets shut down -- the ordinary way a split
// brain ends -- is skipped as inactive and appears in neither set, so it
// leaves the gauge on the next pass however long its peer has been quiet.
// Only a VM that is still running AND whose peer cannot be asked keeps its
// last answer, which is the one case where clearing would be a lie.
func sweepFences(ctx context.Context, cfg agentConfig, state *sharedState, ledger *fenceLedger, prevSplit map[string]bool) (published map[string]bool) {
	// Unchanged unless this pass completes; every early return below is a
	// pass that looked at some VMs and not others.
	published = prevSplit
	mgr, err := libvirtsync.Connect(cfg.LibvirtURI)
	if err != nil {
		trace.Error("fence check could not reach local libvirt", "error", err)
		// Returning WITHOUT touching the metric: failing to look is not
		// evidence that the problem went away, and clearing an alert
		// because the check broke is the worst of both.
		return
	}
	domains, err := inventory.Scan(mgr)
	mgr.Close()
	if err != nil {
		trace.Error("fence check could not scan local domains", "error", err)
		return
	}

	// Published only if the pass COMPLETES. A sweep cut short -- by
	// shutdown, or by a cancelled context -- has looked at some VMs and not
	// others, and publishing that would clear the split-brain state of every
	// VM it never reached. Same reasoning as the error returns above: a
	// partial answer is not a negative one.
	// Read once per sweep. The schedule carries per-VM shutdown timeouts,
	// and re-reading it for every domain would take the lock repeatedly to
	// get the same answer.
	cached := state.get().Config

	// Read once per sweep, and used only to put WORDS on a finding -- never to
	// establish one. LatestByVM takes the mutex and copies the map, which a pass
	// over every domain on the host must not pay for per domain.
	var acted map[string]fenceRecord
	if ledger != nil {
		acted = ledger.LatestByVM()
	}

	split := map[string]bool{}
	// VMs this pass could not get an answer about. Its own set rather than
	// folded into split, for the same reason fencedRunningVMs is its own:
	// the two call for different actions. A split brain asks which copy
	// wins; this says the question could not be put, and the thing to fix
	// is the link to the peer -- or, if that link is down because the site
	// is down, it is the strongest hint available that the other copy may
	// be live and unreachable rather than stopped.
	unknown := map[string]bool{}
	completed := false
	defer func() {
		if completed {
			cfg.metrics.setSplitBrain(split)
			cfg.metrics.setSplitBrainUnknown(unknown)
			published = split
		}
	}()

	for _, d := range domains {
		if ctx.Err() != nil {
			return
		}
		// A domain that is not running cannot be half of a split brain, and
		// this is the check that keeps the sweep cheap: it reduces the work
		// to the VMs actually capable of causing the problem, rather than
		// one remote connection per defined domain per minute.
		if !d.Active {
			continue
		}
		// An excluded domain is not fenced, and fencing is the sharpest
		// case for the rule: it STOPS A RUNNING GUEST. An operator who
		// wrote "leave this alone" must not find vmsync shutting it down
		// because a peer looked promoted -- least of all a domain that is
		// role=fenced AND running: a fence suspended this domain's replication
		// and did not stop the domain. Either the shutdown never took -- the
		// realistic case, a guest that ignores ACPI -- or somebody started it
		// again afterwards, which a host reboot does on its own for any domain
		// whose autostart flag survived the fence.
		//
		// Reported HERE, above both guards below, and each of them is a reason
		// this had to move up rather than a filter worth reusing:
		//
		//   - replica_targets is what makes a PEER QUERY worth paying for, and
		//     this finding needs none. It is two fields of this host's own
		//     metadata, which is the whole point: during a DR event the peer is
		//     the half that is usually unreachable, and a detector that had to
		//     ask it would go blind exactly when it is needed.
		//   - the role filter skips `fenced` deliberately, so a fence is not
		//     attempted again every minute. That is right about the ACTION and
		//     wrong about the REPORT -- this is the one state in which every
		//     gauge otherwise reads clear, and what silenced them was the very
		//     record written to make the failure safe.
		//
		// Nothing is retried and nothing here can retry: the finding is made and
		// the domain is skipped on the next line. The ledger is consulted only
		// to put words on it, never to establish it -- the role lives on the
		// domain and outlives losing fences.json, and a fence run by hand with
		// `vmsync -fence-domain` leaves no ledger entry at all.
		if fenceFailedOpen(d.Role, d.Active) {
			split[d.Name] = true
			fields := []any{"vm", d.Name}
			if rec, ok := acted[d.Name]; ok {
				fields = append(fields, "fence_id", rec.FenceID, "peer", rec.PeerRef, "ledger_state", rec.State)
			}
			trace.Warning("FENCED BUT STILL RUNNING: this domain is marked replication_role=fenced and is running, so a fence suspended its replication without stopping the domain -- and the peer that displaced it is serving the same VM. Which copy wins is already decided, so this is not that question: stop THIS domain by hand, then run -invert if the failover stands, or -update-role=source if the fence was wrong. It will NOT be retried automatically",
				fields...)
			continue
		}
		// Only where this host is the source. A promoted domain is the one
		// doing the displacing, and a target is not serving anything.
		//
		// Below the fenced check above, deliberately: this guard decides whether
		// a PEER QUERY is worth paying for, and that finding needs no peer. A
		// fenced domain whose replica_targets have been cleared -- by an
		// inversion, or by hand -- is still reported.
		if len(d.ReplicaTargets) == 0 {
			continue
		}
		switch d.Role {
		case libvirtsync.RoleSource, "":
		default:
			// paused (administratively stopped), promoted (this host is the one
			// that took over), target (not serving), or a role a newer build
			// wrote. None of them is a source that could still be writing.
			//
			// `fenced` never reaches here at all: one that stopped is
			// skipped as inactive above, and one still running is reported and
			// skipped by the branch above that. Both still end in a skip, which
			// is what stops a domain being swept again every minute after it has
			// been fenced.
			continue
		}
		verdicts := make([]fenceVerdict, 0, len(d.ReplicaTargets))
		for _, ref := range d.ReplicaTargets {
			if ctx.Err() != nil {
				return
			}
			verdicts = append(verdicts, checkOneFence(ctx, cfg, cached, ledger, d, ref))
		}
		isSplit, isUnknown := splitStateFor(verdicts, prevSplit[d.Name])
		if isSplit {
			split[d.Name] = true
		}
		if isUnknown {
			unknown[d.Name] = true
		}
	}
	completed = true
	return published
}

// splitStateFor decides one VM's two gauge entries from this pass's peer
// verdicts and what the last completed pass concluded.
//
// Pure, and its own function, for the reason droppedFenceToReport gives
// just below: inline, this was the one decision in this file that nothing
// could check, and it is the decision an alarm clears on.
//
// A source may fan out, so the verdicts have to be combined. One peer
// saying "I have taken over" settles it. Otherwise a single peer that could
// not be reached makes the whole answer unknown -- concluding "not split"
// from the peers that did reply would be the same mistake at a smaller
// scale, since the silent one is exactly where a promotion would be.
//
// wasSplit is carried forward only into the unknown case. That is the whole
// fix: a VM known to be split, whose peer then goes quiet, keeps its entry
// instead of being cleared by silence -- while a VM that gets shut down is
// never passed here at all (the sweep skips it as inactive), so the gauge
// still clears itself the ordinary way and does not latch.
func splitStateFor(verdicts []fenceVerdict, wasSplit bool) (split, unknown bool) {
	anyUnknown := false
	for _, v := range verdicts {
		switch v {
		case fenceSplit:
			// Settled, and no amount of silence elsewhere softens it.
			return true, false
		case fenceUnknown:
			anyUnknown = true
		}
	}
	if anyUnknown {
		return wasSplit, true
	}
	// Every peer answered, and none is displacing this VM. The one
	// outcome allowed to clear the alarm.
	return false, false
}

// checkOneFence asks one peer about one VM, and acts if it must. It reports
// whether this VM is currently in split brain, for the caller's metric.
func checkOneFence(ctx context.Context, cfg agentConfig, cached UIConfig, ledger *fenceLedger, d inventory.Domain, peerRef string) fenceVerdict {
	host, peerVM := splitReplicaRef(peerRef)
	if host == "" || peerVM == "" {
		trace.Error("fence check skipped a replica target that is not in host:domain form", "vm", d.Name, "target", peerRef)
		// Unknown, not clear: a reference nobody can parse is a peer that
		// can never be asked, so it must not be the thing that clears an
		// alarm about this VM.
		return fenceUnknown
	}

	rep, err := readPeerFence(ctx, cfg, host, peerVM)
	if err != nil {
		// Could not even run the query. Logged, never escalated: not being
		// able to ask is not evidence of anything, and the safe reading of
		// silence is "keep serving".
		trace.Error("fence check could not query a peer", "vm", d.Name, "peer", peerRef, "error", err)
		return fenceUnknown
	}
	if !rep.Reachable {
		trace.Debug("fence check: peer unreachable, continuing to serve", "vm", d.Name, "peer", peerRef, "error", rep.Error)
		return fenceUnknown
	}

	self := replicaSelfRef(cfg, d.Name)
	verdict := failover.AssessFence(failover.FenceObservation{
		Token:        rep.Fence,
		TargetRole:   rep.TargetRole,
		TargetActive: rep.TargetActive,
		TargetRef:    rep.TargetRef,
	}, self, ledger.Acted(rep.Fence.ID))

	if !verdict.Fence {
		// Said only when a fence actually EXISTS and is not being acted on.
		//
		// An unarmed token is not a state worth reporting: it is what every
		// healthy pair looks like, on every sweep, forever. Logging it meant
		// -debug produced a line per source VM per replica target per cycle
		// saying nothing had been configured -- which the operator already
		// knows, since not arming a fence is the default and is what a
		// promotion does unless explicitly asked otherwise. The lines an
		// operator turns -debug on to find were buried under it.
		//
		// The other refusals are kept, because each describes a fence that
		// was armed and is deliberately not firing: a token naming another
		// pair, a peer no longer promoted, a promoted peer that is not
		// running. Those are the answers to "why did my fence not fire",
		// which is the question -debug gets used for here.
		if rep.Fence.Armed() {
			trace.Debug("fence check: not fencing", "vm", d.Name, "peer", peerRef, "reason", verdict.Reason)
		}

		// One case among the refusals is NOT benign, and it is the reason
		// the metric is set here rather than only where a fence fires: an
		// armed token naming this host, on a live promoted peer, that was
		// already acted on. Acted on and yet this domain is still running
		// means the shutdown did not take -- or somebody started it again --
		// and the split brain the fence existed to prevent is happening now.
		// Latching refuses to retry; it must not also make it invisible.
		if verdict.Alarm {
			trace.Warning("SPLIT BRAIN: this host is running a VM that has been failed over, and its fence was already acted on -- it will NOT be retried; resolve this by hand",
				"vm", d.Name, "peer", peerRef, "reason", verdict.Reason)
		}
		if verdict.Alarm {
			return fenceSplit
		}
		// The peer answered and is not displacing this VM: a positive
		// clear, which is the one thing allowed to drop it from the gauge.
		return fenceClear
	}

	// REPORTED BUT NOT ACTED ON, in monitor mode.
	//
	// The mode's contract is that this host does not act, and fencing is
	// the one thing this agent does that stops a running guest -- so it
	// must not. But REPORTING is not acting, and a mode whose entire job is
	// to report was the one mode that said nothing: the sweep never ran, so
	// vmsync_agent_split_brain_vms was emitted as 0 for the life of the
	// process. Not absent -- zero, which is an affirmative "both copies are
	// not running" about a question nobody asked, and an alert on it could
	// never fire.
	//
	// Same shape as -no-autofence below and as the exclusion above: the
	// finding stands, the shutdown does not happen, and the warning repeats
	// every cycle because nothing has resolved it.
	if !cfg.Mode.actsOnItsOwn() {
		trace.Warning("SPLIT BRAIN: this host is still running a VM that has been failed over, and this agent is in monitor mode, so nothing will be done about it automatically -- stop the domain by hand, or run the agent --controlled with \"features.schedule\": false to let it fence without scheduling",
			"vm", d.Name, "peer", peerRef, "reason", verdict.Reason)
		return fenceSplit
	}

	if cfg.NoAutoFence {
		// Asked not to act, so it does not -- but this is exactly the
		// situation the operator needs told about, and it is NOT latched:
		// nothing was acted on, so the warning repeats every cycle until
		// somebody resolves it. That repetition is the feature.
		trace.Warning("SPLIT BRAIN: this host is still running a VM that has been failed over, and -no-autofence is set, so nothing will be done about it automatically",
			"vm", d.Name, "peer", peerRef, "reason", verdict.Reason)
		return fenceSplit
	}

	// REPORTED, NOT ACTED ON -- the same shape as -no-autofence above, and
	// for a sharper reason.
	//
	// An exclusion says vmsync must not act, and fencing is the most
	// consequential act there is: it stops a running guest. But
	// report-only also says the domain stays reported, and a split brain
	// is the one finding where silence is dangerous: two hosts are serving
	// the same VM and both are taking writes. Skipping the domain earlier
	// in the sweep -- which is what this did at first -- suppressed the
	// finding AND cleared its gauge, so an excluded VM in a split brain
	// read as healthy for ever.
	//
	// Returning fenceSplit is what keeps it in the split-brain gauge.
	if excluded := libvirtsync.Excluded(d.Exclude); excluded.ReportOnly || excluded.Hidden {
		trace.Warning("SPLIT BRAIN: this host is still running a VM that has been failed over, and the domain carries vmsync metadata exclude, so nothing will be done about it automatically -- stop it by hand, or remove the metadata field `exclude` to let autofencing act",
			"vm", d.Name, "peer", peerRef, "exclude", d.Exclude, "reason", verdict.Reason)
		return fenceSplit
	}

	fenceOneDomain(ctx, cfg, cached, ledger, d.Name, peerRef, rep, verdict)

	// Whether the shutdown succeeded is deliberately NOT what decides this.
	// The domain was running in two places when this pass looked, which is
	// what the metric reports; the next sweep is what establishes whether it
	// still is -- by finding the domain inactive and leaving it out of the
	// rebuilt set, or by finding it running and alarming again.
	return fenceSplit
}

// fenceOneDomain records intent, shuts the domain down, and records what
// happened.
func fenceOneDomain(ctx context.Context, cfg agentConfig, cached UIConfig, ledger *fenceLedger, vm, peerRef string, rep fenceReport, verdict failover.FenceVerdict) {
	now := time.Now()
	rec := fenceRecord{
		FenceID: rep.Fence.ID,
		VM:      vm,
		PeerRef: peerRef,
		AtUnix:  now.Unix(),
		ArmedBy: rep.Fence.ArmedBy,
	}

	// Intent first, durably -- but the fence is NOT conditional on that write
	// succeeding.
	//
	// Returning here would be wrong in two compounding ways. The harm ranking
	// is the first: a shutdown performed twice costs one more ACPI request to
	// a guest that is already ignoring the first, and ShutdownDomain never
	// falls back to destroying a domain; a fence that does not fire leaves one
	// workload live in two places writing to two diverging copies, which is
	// unrecoverable and is the entire reason this mechanism exists.
	//
	// The second is that refusing would not even buy the protection it claims.
	// put() latches in memory before it writes (see its comment), with no
	// rollback, so a fence refused here is still marked acted-on: every later
	// sweep takes the alreadyActed branch and warns that the fence "was
	// already acted on -- it will NOT be retried" about a shutdown that never
	// happened, for the life of the process, unaffected by the disk clearing.
	// Fencing anyway keeps that message true.
	//
	// What is genuinely lost is the record surviving a restart, so a new
	// process may fence the same token again. Bounded: if the shutdown
	// succeeded the domain is off and its role is `fenced`, and the sweep
	// skips it on both counts, so a repeat needs somebody to restart the VM
	// and put its role back while the peer is still promoted and armed -- by
	// which point a real split brain exists and shutting down is correct.
	if err := ledger.Begin(rec); err != nil {
		// Loud, and counted separately: this is the one path that acts on a
		// production VM without a durable record of having done so, and an
		// audit gap must be visible rather than inferred.
		trace.Error("FENCING WITHOUT A DURABLE RECORD: the intent could not be written, and this fence is proceeding anyway because a split brain is the worse outcome -- if this agent restarts, the same fence may be attempted again",
			"vm", vm, "peer", peerRef, "fence_id", rep.Fence.ID, "error", err)
		cfg.metrics.fenceUnrecorded()
	}

	trace.Warning("FENCING: shutting this domain down because it has been failed over to another host",
		"vm", vm, "peer", peerRef, "fence_id", rep.Fence.ID, "reason", verdict.Reason)

	// How long the guest gets, and how long vmsync itself gets.
	//
	// The second must exceed the first, or this kills vmsync mid-wait --
	// leaving a `running` ledger entry, a domain in an unknown state, and a
	// fence that is latched and will never be tried again. That was a real
	// hazard while the outer bound was a fixed ten minutes: any per-VM
	// timeout above it would have been cut off before it could expire.
	guestSec := shutdownTimeoutFor(cached, vm)
	cctx, cancel := context.WithTimeout(ctx, shutdownProcessBound(guestSec))
	defer cancel()

	// -fence-domain: the same clean guest shutdown -shutdown-domain performs,
	// never falling back to destroying the domain, followed by the
	// replication role being recorded. They share one implementation
	// (shutdownAndMark) so there is no second, subtly different shutdown path
	// to keep in step; they differ only in the role written, which is the one
	// thing only the caller knows. A planned failover records `paused` -- a
	// person chose it. This records `fenced` -- nobody here chose anything,
	// and the operator who finds it needs to know a peer took over.
	//
	// It also records that role even if the shutdown FAILS, which matters most
	// here: the peer is already serving, so a guest that ignores ACPI leaves a
	// live split brain, and the role is then the only thing refusing a sync.
	args := []string{
		"-fence-domain", "-target-uri", cfg.LibvirtURI, "-target-domain", vm,
		"-shutdown-timeout-sec", strconv.Itoa(guestSec),
	}
	cmd := exec.CommandContext(cctx, cfg.VmsyncPath, args...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 60 * time.Second
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out

	// EXEMPT from the fail-closed run-log contract every other launch site
	// obeys: attempted, and the fence proceeds whatever it returns.
	//
	// The harm ranking decides it. Everywhere else a refused launch costs one
	// interval of replication, which the next tick recovers. Here it would
	// leave one workload live in two places writing to two diverging copies --
	// unrecoverable, and the precise thing this mechanism exists to prevent.
	//
	// Note what this does NOT buy: fenceLedger.Begin above needs strictly more
	// of the filesystem than an append does (a new inode, a directory write, a
	// rename), so on a full or read-only state_dir the ledger has already
	// failed and this exemption changes nothing. It covers failures specific
	// to runs.jsonl itself.
	runID := newRunID()
	if lerr := cfg.runLog.Append(runLogRecord{
		Event: runEventLaunch, RunID: runID, Origin: runOriginFence,
		VM: vm, FenceID: rep.Fence.ID,
		Binary: cfg.VmsyncPath, Args: redactArgs(args),
	}); lerr != nil {
		trace.Error("fencing without a run-log record: the record could not be written, and a fence is not refused for that",
			"vm", vm, "fence_id", rep.Fence.ID, "error", lerr)
	}

	rec.Attempts = 1
	err := cmd.Run()
	fenceCode := -1
	if cmd.ProcessState != nil {
		fenceCode = cmd.ProcessState.ExitCode()
	}
	fenceOutcome := "success"
	if err != nil {
		fenceOutcome = "failure"
	}
	_ = cfg.runLog.Append(runLogRecord{
		Event: runEventExit, RunID: runID, VM: vm,
		ExitCode: &fenceCode, Outcome: fenceOutcome,
	})
	cfg.metrics.fenceActed(err == nil)
	if err != nil {
		rec.State, rec.Error = OpStateFailed, err.Error()
		trace.Error("FENCING FAILED: this domain is still running while another host serves the same VM -- a person needs to resolve this; it will NOT be retried automatically",
			"vm", vm, "peer", peerRef, "error", err, "output", tail(out.String(), logTailBytes))
	} else {
		rec.State = OpStateDone
		trace.Warning("fenced: domain shut down and replication paused", "vm", vm, "peer", peerRef)
	}
	rec.AtUnix = time.Now().Unix()
	if err := ledger.Finish(rec); err != nil {
		trace.Error("fence outcome could not be recorded", "vm", vm, "error", err)
	}
}

// shutdownTimeoutFor picks how long a guest gets to stop cleanly: this VM's
// own setting, then the estate default, then vmsync's own.
//
// The same order the UI resolves in when it creates a shutdown operation,
// and it has to be: these two paths shut a domain down in identical ways,
// and a VM that stops fine when an operator asks but fails when a fence does
// would be a baffling thing to debug.
//
// Clamped rather than trusted. The config comes from a separately-versioned
// program over the network, and this number decides how long a production VM
// is given before its shutdown is called a failure -- a nonsense value would
// either declare failure before the guest had begun or hang the sweep behind
// one domain for hours.
func shutdownTimeoutFor(cached UIConfig, vm string) int {
	sec := 0
	for _, e := range cached.Schedule {
		if e.VM == vm && e.ShutdownTimeoutSec > 0 {
			sec = e.ShutdownTimeoutSec
			break
		}
	}
	if sec <= 0 {
		sec = cached.ShutdownTimeoutSec
	}
	if sec <= 0 {
		sec = defaultShutdownTimeoutSec
	}
	if sec < minShutdownTimeoutSec {
		sec = minShutdownTimeoutSec
	}
	if sec > maxShutdownTimeoutSec {
		sec = maxShutdownTimeoutSec
	}
	return sec
}

// fenceFailedOpen reports the one state a fence that did not take leaves behind:
// the role says a fence happened, and the domain is still running.
//
// Pure, and its own function rather than an inline condition, because three
// callers must agree about it -- the sweep that warns, the feeder that publishes
// the gauge, and inventory.Assess by way of the same rule. Two spellings of this
// would let the console and the metric disagree about whether a VM is live in two
// places.
//
// role=fenced is the whole of the evidence, and it is enough: nothing writes it
// except a fence (or an operator saying a fence happened), and it carries the
// claim that a PEER TOOK OVER. That is why this does not generalise to
// paused-and-running, which looks identical and is not: `paused` claims nothing
// about a peer, so a running domain whose replication was paused by hand is one
// live copy and an ordinary state. Alarming on it would train an operator to
// ignore the series that matters.
//
// Nor does it consult the ledger. The role is on the domain, so it survives
// losing fences.json, an agent that never ran, and a fence performed by hand with
// `vmsync -fence-domain`; the ledger records only fences THIS agent performed.
func fenceFailedOpen(role string, active bool) bool {
	return active && role == libvirtsync.RoleFenced
}

// shutdownProcessBound is how long vmsync itself gets, given how long the
// guest gets.
//
// Always strictly longer, and that is the whole point of it being its own
// function. vmsync connects to libvirt, reads state and writes metadata
// either side of the wait, so a bound equal to the guest timeout kills it
// exactly as the guest runs out -- leaving a `running` ledger entry, a
// domain in an unknown state, and a latched fence nothing will ever retry.
// It also replaces a fixed ten-minute bound that any longer per-VM timeout
// would silently have been cut off by.
func shutdownProcessBound(guestSec int) time.Duration {
	return time.Duration(guestSec)*time.Second + fenceShutdownGrace
}

// Bounds on a guest shutdown, mirroring the UI's own. Duplicated rather than
// shared because the two are separately-versioned programs -- and because
// the agent has to hold the line whatever a UI of any vintage sends.
const (
	defaultShutdownTimeoutSec = 300
	minShutdownTimeoutSec     = 30
	maxShutdownTimeoutSec     = 3600
)

// validateShutdownTimeoutSec accepts 0 (meaning "inherit") or a value in
// range, for a configuration a PERSON wrote. Anything arriving from a UI is
// clamped by shutdownTimeoutFor instead: one is a mistake to point out, the
// other is input to survive.
func validateShutdownTimeoutSec(sec int) error {
	if sec == 0 {
		return nil
	}
	if sec < minShutdownTimeoutSec || sec > maxShutdownTimeoutSec {
		return fmt.Errorf("shutdown_timeout_sec %d is out of range: use 0 to inherit, or %d to %d seconds",
			sec, minShutdownTimeoutSec, maxShutdownTimeoutSec)
	}
	return nil
}

// readPeerFence runs vmsync -read-fence against a peer and parses the answer.
func readPeerFence(ctx context.Context, cfg agentConfig, host, peerVM string) (fenceReport, error) {
	cctx, cancel := context.WithTimeout(ctx, fenceReadTimeout)
	defer cancel()

	args := []string{
		"-read-fence",
		"-target-uri", fmt.Sprintf(cfg.TargetURIPattern, host),
		"-target-domain", peerVM,
	}
	cmd := exec.CommandContext(cctx, cfg.VmsyncPath, args...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	if err := cmd.Run(); err != nil {
		return fenceReport{}, fmt.Errorf("%w (%s)", err, tail(stderr.String(), 400))
	}
	return parseFenceReport(stdout.Bytes())
}

// fenceReport mirrors what vmsync -read-fence prints. Declared here rather
// than shared through a package because the two binaries are deliberately
// separable: the agent runs whatever vmsync is installed, which may be a
// different build, so this is a wire format and is treated as one.
type fenceReport struct {
	Reachable    bool                `json:"reachable"`
	Error        string              `json:"error,omitempty"`
	TargetRef    string              `json:"target_ref"`
	TargetRole   string              `json:"target_role,omitempty"`
	TargetActive bool                `json:"target_active"`
	Fence        failover.FenceToken `json:"fence"`
}

// parseFenceReport reads the last JSON object on stdout.
//
// vmsync logs through the standard log package, which writes to stderr, so
// stdout should hold the report and nothing else. This scans for it anyway:
// the two binaries are separately versioned and separately installed, and a
// fence check that broke because some future build wrote one extra line
// would fail silently in the direction of protecting nothing. Tolerating a
// stray line costs nothing; not tolerating one costs the whole mechanism.
func parseFenceReport(out []byte) (fenceReport, error) {
	lines := bytes.Split(bytes.TrimSpace(out), []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var rep fenceReport
		if err := json.Unmarshal(line, &rep); err == nil {
			return rep, nil
		}
	}
	return fenceReport{}, fmt.Errorf("no fence report found in the output of -read-fence (%s)", tail(string(out), 400))
}
