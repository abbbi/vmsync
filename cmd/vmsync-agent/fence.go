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
	"sync"
	"syscall"
	"time"

	"vmsync/pkg/failover"
	"vmsync/pkg/inventory"
	"vmsync/pkg/libvirtsync"
	"vmsync/pkg/trace"
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
	// fenceShutdownTimeout bounds the shutdown that follows. Generous: it is
	// a guest ACPI shutdown, and the alternative to waiting is destroying a
	// running VM, which this design never does.
	fenceShutdownTimeout = 10 * time.Minute
	// fenceLedgerKept bounds the durable record. One entry per failover per
	// VM, so this is effectively unbounded for any real estate while still
	// refusing to grow without limit.
	fenceLedgerKept = 1000
)

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

	if err := writeJSONAtomic(l.path, snapshot, 0o644); err != nil {
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
func fenceLoop(ctx context.Context, cfg agentConfig, ledger *fenceLedger) {
	ticker := time.NewTicker(fenceTickInterval)
	defer ticker.Stop()

	for {
		sweepFences(ctx, cfg, ledger)
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
func sweepFences(ctx context.Context, cfg agentConfig, ledger *fenceLedger) {
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
	split := map[string]bool{}
	completed := false
	defer func() {
		if completed {
			cfg.metrics.setSplitBrain(split)
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
		// Only where this host is the source. A promoted domain is the one
		// doing the displacing, and a target is not serving anything.
		if len(d.ReplicaTargets) == 0 {
			continue
		}
		switch d.Role {
		case libvirtsync.RoleSource, "":
		default:
			// paused (already fenced, or administratively stopped), promoted
			// (this host is the one that took over), target (not serving).
			// None of them is a source that could still be writing.
			continue
		}
		for _, ref := range d.ReplicaTargets {
			if ctx.Err() != nil {
				return
			}
			if checkOneFence(ctx, cfg, ledger, d, ref) {
				split[d.Name] = true
			}
		}
	}
	completed = true
}

// checkOneFence asks one peer about one VM, and acts if it must. It reports
// whether this VM is currently in split brain, for the caller's metric.
func checkOneFence(ctx context.Context, cfg agentConfig, ledger *fenceLedger, d inventory.Domain, peerRef string) bool {
	host, peerVM := splitReplicaRef(peerRef)
	if host == "" || peerVM == "" {
		trace.Error("fence check skipped a replica target that is not in host:domain form", "vm", d.Name, "target", peerRef)
		return false
	}

	rep, err := readPeerFence(ctx, cfg, host, peerVM)
	if err != nil {
		// Could not even run the query. Logged, never escalated: not being
		// able to ask is not evidence of anything, and the safe reading of
		// silence is "keep serving".
		trace.Error("fence check could not query a peer", "vm", d.Name, "peer", peerRef, "error", err)
		return false
	}
	if !rep.Reachable {
		trace.Debug("fence check: peer unreachable, continuing to serve", "vm", d.Name, "peer", peerRef, "error", rep.Error)
		return false
	}

	self := libvirtsync.ReplicaEntry(cfg.Hostname, d.Name)
	verdict := failover.AssessFence(failover.FenceObservation{
		Token:        rep.Fence,
		TargetRole:   rep.TargetRole,
		TargetActive: rep.TargetActive,
		TargetRef:    rep.TargetRef,
	}, self, ledger.Acted(rep.Fence.ID))

	if !verdict.Fence {
		// Debug, not warning: the overwhelmingly common answer is "no fence
		// was armed", and logging that at any louder level once a minute per
		// VM would bury everything else.
		trace.Debug("fence check: not fencing", "vm", d.Name, "peer", peerRef, "reason", verdict.Reason)

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
		return verdict.Alarm
	}

	if cfg.NoAutoFence {
		// Asked not to act, so it does not -- but this is exactly the
		// situation the operator needs told about, and it is NOT latched:
		// nothing was acted on, so the warning repeats every cycle until
		// somebody resolves it. That repetition is the feature.
		trace.Warning("SPLIT BRAIN: this host is still running a VM that has been failed over, and -no-autofence is set, so nothing will be done about it automatically",
			"vm", d.Name, "peer", peerRef, "reason", verdict.Reason)
		return true
	}

	fenceOneDomain(ctx, cfg, ledger, d.Name, peerRef, rep, verdict)

	// Whether the shutdown succeeded is deliberately NOT what decides this.
	// The domain was running in two places when this pass looked, which is
	// what the metric reports; the next sweep is what establishes whether it
	// still is -- by finding the domain inactive and leaving it out of the
	// rebuilt set, or by finding it running and alarming again.
	return true
}

// fenceOneDomain records intent, shuts the domain down, and records what
// happened.
func fenceOneDomain(ctx context.Context, cfg agentConfig, ledger *fenceLedger, vm, peerRef string, rep fenceReport, verdict failover.FenceVerdict) {
	now := time.Now()
	rec := fenceRecord{
		FenceID: rep.Fence.ID,
		VM:      vm,
		PeerRef: peerRef,
		AtUnix:  now.Unix(),
		ArmedBy: rep.Fence.ArmedBy,
	}

	// Intent first, durably. If this write fails nothing is shut down: a
	// shutdown with no record of it could be performed again on the next
	// pass, and "again" for a fence means a second unattended shutdown of a
	// production VM that somebody may have deliberately restarted.
	if err := ledger.Begin(rec); err != nil {
		trace.Error("not fencing, because the intent could not be recorded", "vm", vm, "error", err)
		return
	}

	trace.Warning("FENCING: shutting this domain down because it has been failed over to another host",
		"vm", vm, "peer", peerRef, "fence_id", rep.Fence.ID, "reason", verdict.Reason)

	cctx, cancel := context.WithTimeout(ctx, fenceShutdownTimeout)
	defer cancel()

	// The existing -shutdown-domain mode, unchanged: a clean guest shutdown
	// that never falls back to destroying the domain, followed by
	// replication being set to paused. Reused rather than reimplemented so
	// that a fenced domain ends in exactly the state a deliberate planned
	// failover would leave it in -- there is no second, subtly different
	// shutdown path to keep in step.
	args := []string{"-shutdown-domain", "-target-uri", cfg.LibvirtURI, "-target-domain", vm}
	cmd := exec.CommandContext(cctx, cfg.VmsyncPath, args...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 60 * time.Second
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out

	rec.Attempts = 1
	err := cmd.Run()
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
