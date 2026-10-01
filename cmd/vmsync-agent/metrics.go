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
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"vmsync/pkg/inventory"
	"vmsync/pkg/libvirtsync"
	"vmsync/pkg/trace"
)

// agentMetricsFile is the agent's own textfile, alongside the per-VM ones
// vmsync writes.
//
// Hyphenated on purpose: the per-VM files are "vmsync_<vm>.prom", so a
// domain innocently named "agent" would otherwise land on exactly this
// path and the two would overwrite each other -- intermittently, and only
// on the one host where somebody made that naming choice.
const agentMetricsFile = "vmsync-agent.prom"

// metricsInterval is how often the file is rewritten. Frequent enough that
// a scrape never sees state more than this stale, cheap enough to ignore:
// it is a few hundred bytes and no libvirt call in control-plane mode.
const metricsInterval = 15 * time.Second

// Reasons a due VM did not run. Fixed rather than free-form so every series
// exists from the first write, at zero: a counter that only appears after
// the event cannot be alerted on with increase() over a window that starts
// before it, which is precisely the window an alert covers.
const (
	skipHostConcurrency = "host_concurrency"
	skipTargetSlots     = "target_replication_slots"
	skipAlreadyRunning  = "already_running"
	skipInvalidProfile  = "invalid_profile"
	skipNoTarget        = "no_target"
	// skipRunLogUnwritable is a launch refused because its record could not be
	// written. Fail-closed by decision: an unrecorded vmsync is a process
	// nothing can later account for. Its own reason because the remedy is
	// unlike every other skip here -- it is a disk, not a schedule.
	skipRunLogUnwritable = "run_log_unwritable"
	// skipForeignRun is a due VM whose lock is held by a vmsync this agent did
	// not start -- almost always its own previous instance's child, still
	// running across a restart. Distinct from already_running, which is this
	// process's own bookkeeping: this one is what that bookkeeping cannot see.
	skipForeignRun = "foreign_run"
)

var skipReasons = []string{
	skipHostConcurrency,
	skipTargetSlots,
	skipAlreadyRunning,
	skipInvalidProfile,
	skipNoTarget,
	skipRunLogUnwritable,
	skipForeignRun,
}

// agentMetrics is what the agent knows about itself.
//
// Deliberately separate from the per-VM metrics vmsync writes. Those say
// whether a sync that RAN succeeded; these say whether one was ever
// attempted -- and the failure this exists to catch is the silent one, a VM
// that is configured, never runs, and therefore never writes a per-VM file
// to go stale in the first place.
type agentMetrics struct {
	version   string
	hostname  string
	mode      agentMode
	startedAt time.Time

	runsOK   atomic.Uint64
	runsFail atomic.Uint64
	runsBusy atomic.Uint64
	// runLogWritable starts true: an agent that could not open the run log
	// never reaches a metrics write, because it refuses to start.
	runLogWritable atomic.Bool
	configRejects  atomic.Uint64
	configGen      atomic.Uint64
	skips          map[string]*atomic.Uint64 // fixed keys; never written after construction

	uiLastContact atomic.Int64 // unix seconds, 0 = never
	uiFailures    atomic.Uint64

	mu           sync.Mutex
	domainTotal  int
	domainStatus map[string]int
	lastAttempt  map[string]int64

	// uiClockSkew is how far this host's clock is from the control
	// plane's, positive when the UI is ahead. Guarded by mu with the maps
	// above rather than an atomic, because the warning decision needs the
	// value and the last-warned time together.
	uiClockSkew      time.Duration
	uiClockSkewKnown bool
	uiSkewWarnedAt   time.Time

	// Split-brain state. splitBrainVMs is the set of VMs this host is still
	// running that a peer says it has been failed over from -- a gauge and
	// not a counter, because what an operator needs to alert on is "is this
	// true RIGHT NOW", and it clears by itself when the condition does.
	//
	// It is populated whether or not the agent acts: with -no-autofence, or
	// after a fence that failed, the VM stays in the set and the metric
	// stays up. That is the whole point -- the case where nothing was done
	// about it is exactly the case somebody must be told about.
	splitBrainVMs    map[string]bool
	fencesActed      atomic.Uint64
	fencesFailed     atomic.Uint64
	fencesUnrecorded atomic.Uint64

	// incompleteVMs is the set of VMs on this host whose domain carries
	// replica_incomplete: a full rebuild was started on them and never
	// recorded as finished, so their disks are a partial copy.
	//
	// A gauge for the same reason the split-brain set is one -- the question
	// is "is this true RIGHT NOW", and it clears by itself when a sync
	// completes and the define that records it removes the marker. Nothing
	// here counts occurrences: a rebuild interrupted twice is still one
	// broken replica, and a counter would keep alerting after the repair.
	//
	// This is the only place the condition is visible without a control
	// plane. A standalone host has no console to show a report on, and every
	// other number it publishes about such a domain -- its status, its last
	// sync -- describes the sync BEFORE the interrupted rebuild.
	incompleteVMs map[string]bool

	// servedLiveVMs is the set of VMs on this host that carry
	// last_promoted_at and are no longer marked promoted: copies that have
	// served live, have been demoted, and whose promotion record nobody has
	// released.
	//
	// A gauge, like the two sets around it, because the question is "is this
	// true right now". It clears when a person runs -release-promotion or an
	// -invert makes the copy the source, and it is the one set here that
	// nothing clears by itself -- which is precisely why it is worth
	// publishing. The copy is holding data that was serving; the source
	// facing it is having every sync refused; and none of that shows up
	// anywhere else a standalone host can see, because the refusals are
	// exempt from failure_count and the domain's own role reads `paused`
	// like any replica somebody paused for maintenance.
	//
	// Promoted-and-still-serving domains are deliberately EXCLUDED. Those are
	// not waiting on anybody -- a failover in progress is the expected state
	// for hours -- and counting them would make this gauge fire on every
	// successful DR test, which is how a signal stops being read.
	servedLiveVMs map[string]bool

	// fencedRunningVMs is the set of VMs on this host marked
	// replication_role=fenced that are STILL RUNNING: a fence suspended their
	// replication without stopping the domain, while the peer that displaced
	// them serves the same VM.
	//
	// Its own set rather than folded into splitBrainVMs, because the two need
	// different remedies. Split brain asks which copy wins; this is past that --
	// the decision is already made and the shutdown did not take, so the answer
	// is always "stop this domain by hand". They overlap deliberately: a VM here
	// is also counted as split brain, so alerts already written keep firing.
	fencedRunningVMs map[string]bool

	// restorePoints is what each replica on this host can be rolled back to.
	//
	// The fleet half of the restore-point metrics, and the half that answers
	// the question the engine's per-run file cannot: that file is rewritten by
	// each invocation, so it describes the last RUN, while an operator during an
	// incident is asking about the STORE -- how deep it is and how far back it
	// goes. It is also the only source for a pair whose syncs are not running at
	// all, which is precisely when the depth stops growing.
	//
	// Replaced wholesale from one complete sweep, like the two sets above, so a
	// replica that goes away stops being reported instead of latching forever.
	restorePointVMs map[string]restorePointGauge
	// leftoverVMs is the displaced-set cost per replica: full-size copies a
	// rebuild or a restore left behind that nothing reaps. See
	// inventory.LeftoverInfo for why they are reported rather than deleted.
	leftoverVMs map[string]leftoverGauge
}

// restorePointGauge is one replica's restore point store, reduced to numbers.
type restorePointGauge struct {
	// Count is published restore points in this domain's own store:
	// directories, including any whose sidecar cannot be read.
	Count int
	// Unreadable is how many of Count have a sidecar that could not be read.
	// Count minus this is the depth that is provably restorable, which is why
	// both are published rather than one netted figure.
	Unreadable int
	// VerifyFailed is how many recorded a FAILED verification. Not a reason to
	// discard them -- they may still be the only copies from before the damage
	// -- but an operator choosing one needs to know, and "every copy I have
	// failed verification" is worth an alert of its own.
	VerifyFailed int
	// NewestUnix and OldestUnix are the tag instants of the extremes, zero when
	// there are none.
	NewestUnix int64
	OldestUnix int64
}

// setSplitBrain replaces the whole set from one complete sweep.
//
// Replace rather than merge, because that is what lets the condition clear
// on its own: a VM that has been fenced is no longer running, so the next
// sweep simply does not include it, and the gauge falls to zero without
// anything having to remember to say so. A merge-based version would latch
// forever after the first detection.
func (m *agentMetrics) setSplitBrain(vms map[string]bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.splitBrainVMs = make(map[string]bool, len(vms))
	for vm := range vms {
		m.splitBrainVMs[vm] = true
	}
}

// setReplicaIncomplete replaces the whole set from one complete sweep, for
// the same reason setSplitBrain does: replacing is what lets the condition
// clear on its own when a rebuild finally completes and clears the marker,
// where a merge would latch the alert forever after one bad run.
//
// Fed from whoever last inventoried this host -- reportLoop with a control
// plane, the metrics loop in standalone -- so libvirt is never walked twice
// for the same answer.
func (m *agentMetrics) setReplicaIncomplete(vms map[string]bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.incompleteVMs = make(map[string]bool, len(vms))
	for vm := range vms {
		m.incompleteVMs[vm] = true
	}
}

// setServedLiveUnreleased replaces the whole set from one complete sweep, for
// the reason every setter here does: replacing is what lets a copy that has
// been released, inverted or removed stop being reported, where a merge would
// keep alerting about a decision somebody already made.
func (m *agentMetrics) setServedLiveUnreleased(vms map[string]bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.servedLiveVMs = make(map[string]bool, len(vms))
	for vm := range vms {
		m.servedLiveVMs[vm] = true
	}
}

// setRestorePoints replaces the whole map from one complete sweep, for the same
// reason setSplitBrain and setReplicaIncomplete do: replacing is what lets a
// replica that has gone away -- inverted, removed, failed over -- stop being
// reported, where a merge would keep publishing an age that never advances for a
// store nothing writes to any more.
func (m *agentMetrics) setRestorePoints(vms map[string]restorePointGauge) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.restorePointVMs = make(map[string]restorePointGauge, len(vms))
	for vm, g := range vms {
		m.restorePointVMs[vm] = g
	}
}

// setFencedRunning replaces the whole set from one complete sweep, for the same
// reason setSplitBrain and setReplicaIncomplete do: replacing is what lets the
// condition clear on its own once somebody stops the domain or changes its role.
//
// This one clears differently from the others, and the difference is the point. A
// displaced source leaves the split-brain set because the agent's own fence
// stopped it. A fenced-and-running domain leaves this set only when a PERSON acts,
// because a fence is never retried -- so a gauge that has not moved in a day is
// not a stuck metric, it is an unresolved split brain, and that it persists is
// exactly what was missing.
func (m *agentMetrics) setFencedRunning(vms map[string]bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fencedRunningVMs = make(map[string]bool, len(vms))
	for vm := range vms {
		m.fencedRunningVMs[vm] = true
	}
}

// fenceActed counts a completed fence attempt.
func (m *agentMetrics) fenceActed(ok bool) {
	if m == nil {
		return
	}
	if ok {
		m.fencesActed.Add(1)
	} else {
		m.fencesFailed.Add(1)
	}
}

// fenceUnrecorded counts fences that went ahead without a durable record,
// because the ledger write failed and a split brain is the worse outcome.
//
// Its own series rather than a label on fencesActed/fencesFailed, which
// describe how the SHUTDOWN went and are incremented only after cmd.Run()
// returns -- a fence can be unrecorded and still succeed, or be unrecorded and
// fail, and the two facts are independent. This one says the audit trail has a
// hole in it, which is a thing an operator must be told directly rather than
// have to infer from a ledger that is missing an entry it never got to write.
func (m *agentMetrics) fenceUnrecorded() {
	if m == nil {
		return
	}
	m.fencesUnrecorded.Add(1)
}

func newAgentMetrics(version, hostname string, mode agentMode) *agentMetrics {
	m := &agentMetrics{
		version:      version,
		hostname:     hostname,
		mode:         mode,
		startedAt:    time.Now(),
		skips:        make(map[string]*atomic.Uint64, len(skipReasons)),
		domainStatus: map[string]int{},
		lastAttempt:  map[string]int64{},
	}
	for _, r := range skipReasons {
		m.skips[r] = &atomic.Uint64{}
	}
	m.runLogWritable.Store(true)
	return m
}

func (m *agentMetrics) skip(reason string) {
	if m == nil {
		return
	}
	if c, ok := m.skips[reason]; ok {
		c.Add(1)
	}
}

func (m *agentMetrics) runStarted(vm string, at time.Time) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.lastAttempt[vm] = at.Unix()
	m.mu.Unlock()
}

func (m *agentMetrics) runFinished(ok bool) {
	if m == nil {
		return
	}
	if ok {
		m.runsOK.Add(1)
		return
	}
	m.runsFail.Add(1)
}

// runBusy counts a launch that stood down on lock contention without doing
// anything (util.ExitBusy).
//
// Its own result label rather than folding into success or failure, because
// it answers a question neither of those can: a rising busy count with a flat
// success count is a VM whose sync outlasts its interval, or an agent that
// restarted into a run it did not start. Both look like healthy replication
// on every other series this agent emits.
func (m *agentMetrics) runBusy() {
	if m == nil {
		return
	}
	m.runsBusy.Add(1)
}

// configRejected counts reloads refused because the new file asked for
// something a running agent cannot do -- today, moving state_dir or changing
// mode. Its own series because the remedy is a restart, not an edit: an
// operator watching only the generation gauge would see it stop advancing and
// have no idea why.
func (m *agentMetrics) configRejected() {
	if m == nil {
		return
	}
	m.configRejects.Add(1)
}

// setConfigGeneration publishes which configuration is in force, so a scrape
// can tell an agent that adopted an edit from one that is still running the
// file as it was at boot -- the difference an operator most wants after
// changing something and seeing no effect.
func (m *agentMetrics) setConfigGeneration(gen uint64) {
	if m == nil {
		return
	}
	m.configGen.Store(gen)
}

// setRunLogWritable records whether the run log can currently be written.
//
// A GAUGE, not only the skip counter beside it, and the difference matters:
// under the fail-closed contract an unwritable run log stops every sync on
// this host, and a counter that has stopped incrementing looks exactly like a
// host with nothing due. One of those is idleness and the other is an outage.
func (m *agentMetrics) setRunLogWritable(ok bool) {
	if m == nil {
		return
	}
	m.runLogWritable.Store(ok)
}

func (m *agentMetrics) uiContacted(at time.Time) {
	if m == nil {
		return
	}
	m.uiLastContact.Store(at.Unix())
}

func (m *agentMetrics) uiFailed() {
	if m == nil {
		return
	}
	m.uiFailures.Add(1)
}

// setDomains records the host inventory. Fed by whoever last scanned:
// reportLoop in control-plane mode, the metrics loop in standalone, so the
// libvirt work is never done twice for the same purpose.
func (m *agentMetrics) setDomains(total int, byStatus map[string]int) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.domainTotal = total
	m.domainStatus = byStatus
	m.mu.Unlock()
}

// render produces the textfile body.
func (m *agentMetrics) render(cached CachedConfig, sched *Scheduler, hostLimit int, now time.Time) string {
	var b strings.Builder
	host := m.hostname

	g := func(name, help string, value any, labels string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n%s{host=%q%s} %v\n", name, help, name, name, host, labels, value)
	}
	c := func(name, help string, value any, labels string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n%s{host=%q%s} %v\n", name, help, name, name, host, labels, value)
	}

	fmt.Fprintf(&b, "# HELP vmsync_agent_build_info Agent version, always 1.\n# TYPE vmsync_agent_build_info gauge\n")
	fmt.Fprintf(&b, "vmsync_agent_build_info{host=%q,version=%q} 1\n", host, m.version)

	g("vmsync_agent_start_timestamp_seconds", "When this agent process started.", m.startedAt.Unix(), "")

	// One series per mode, exactly one of them 1 -- the state-set pattern,
	// written out here rather than through g() because g() emits its own
	// HELP and TYPE and a metric family may only declare those once.
	//
	// It replaces a boolean that could only ask "is there a control plane?",
	// a question with three answers now. The one worth alerting on is
	// vmsync_agent_mode{mode="monitor"} == 1: a host that reports beautifully
	// and replicates nothing on its own is invisible in every other series
	// here, which is exactly how a site everybody assumed was covered turns
	// out not to be.
	fmt.Fprintf(&b, "# HELP vmsync_agent_mode Which mode this agent runs in; exactly one of these series is 1.\n# TYPE vmsync_agent_mode gauge\n")
	for _, mode := range []agentMode{modeStandalone, modeMonitor, modeControlled} {
		fmt.Fprintf(&b, "vmsync_agent_mode{host=%q,mode=%q} %d\n", host, string(mode), boolGauge(m.mode == mode))
	}

	// --- schedule ---------------------------------------------------------
	var enabled, disabled int
	for _, e := range cached.Config.Schedule {
		if e.Enabled && e.IntervalSeconds > 0 && e.VM != "" {
			enabled++
		} else {
			disabled++
		}
	}
	g("vmsync_agent_scheduled_vms", "VMs this agent is configured to sync.", enabled, "")
	g("vmsync_agent_scheduled_vms_disabled", "Schedule entries present but not runnable.", disabled, "")
	g("vmsync_agent_max_concurrent_syncs", "Effective ceiling on parallel syncs, after every limit is applied.",
		effectiveMaxConcurrent(cached.Config.MaxConcurrentSyncs, hostLimit), "")

	running, nextRun := 0, map[string]int64{}
	if sched != nil {
		running, nextRun = sched.metricsSnapshot()
	}
	g("vmsync_agent_syncs_running", "Syncs running right now.", running, "")

	c("vmsync_agent_sync_runs_total", "Scheduled syncs that finished, by result.", m.runsOK.Load(), `,result="success"`)
	fmt.Fprintf(&b, "vmsync_agent_sync_runs_total{host=%q,result=\"failure\"} %d\n", host, m.runsFail.Load())
	// Emitted unconditionally, like every other series here, so it exists at
	// zero from the first write and increase() works over a window that starts
	// before the first stand-down.
	fmt.Fprintf(&b, "vmsync_agent_sync_runs_total{host=%q,result=\"busy\"} %d\n", host, m.runsBusy.Load())

	// The metric the question "is anything actually replicating?" is asked
	// of. A schedule that never gets a slot, or a profile that never
	// validates, produces no per-VM file at all -- so nothing else here goes
	// stale to reveal it.
	fmt.Fprintf(&b, "# HELP vmsync_agent_skipped_runs_total Due syncs that did not start, by reason.\n# TYPE vmsync_agent_skipped_runs_total counter\n")
	for _, r := range skipReasons {
		fmt.Fprintf(&b, "vmsync_agent_skipped_runs_total{host=%q,reason=%q} %d\n", host, r, m.skips[r].Load())
	}

	m.mu.Lock()
	lastAttempt := make(map[string]int64, len(m.lastAttempt))
	for k, v := range m.lastAttempt {
		lastAttempt[k] = v
	}
	domainTotal := m.domainTotal
	uiSkew, uiSkewKnown := m.uiClockSkew, m.uiClockSkewKnown
	domainStatus := make(map[string]int, len(m.domainStatus))
	for k, v := range m.domainStatus {
		domainStatus[k] = v
	}
	m.mu.Unlock()

	fmt.Fprintf(&b, "# HELP vmsync_agent_last_attempt_timestamp_seconds When this agent last STARTED a sync for a VM, whatever the outcome.\n# TYPE vmsync_agent_last_attempt_timestamp_seconds gauge\n")
	for _, vm := range sortedKeys(lastAttempt) {
		fmt.Fprintf(&b, "vmsync_agent_last_attempt_timestamp_seconds{host=%q,vm=%q} %d\n", host, vm, lastAttempt[vm])
	}
	fmt.Fprintf(&b, "# HELP vmsync_agent_next_run_timestamp_seconds When each scheduled VM is next due.\n# TYPE vmsync_agent_next_run_timestamp_seconds gauge\n")
	for _, vm := range sortedKeys(nextRun) {
		fmt.Fprintf(&b, "vmsync_agent_next_run_timestamp_seconds{host=%q,vm=%q} %d\n", host, vm, nextRun[vm])
	}

	// --- inventory --------------------------------------------------------
	g("vmsync_agent_domains_total", "Domains on this host.", domainTotal, "")
	fmt.Fprintf(&b, "# HELP vmsync_agent_domains Domains by assessed replication status.\n# TYPE vmsync_agent_domains gauge\n")
	for _, st := range sortedKeys(domainStatus) {
		fmt.Fprintf(&b, "vmsync_agent_domains{host=%q,status=%q} %d\n", host, st, domainStatus[st])
	}

	// --- control plane ----------------------------------------------------
	// Emitted in standalone too, as an explicit zero. A missing series and a
	// UI that has never answered look identical to a query otherwise, and
	// the difference matters: one is a design choice, the other an outage.
	g("vmsync_agent_ui_last_contact_timestamp_seconds", "Last successful exchange with the control-plane UI. 0 when there is none.",
		m.uiLastContact.Load(), "")
	c("vmsync_agent_ui_failures_total", "Failed exchanges with the control-plane UI.", m.uiFailures.Load(), "")
	// Age of the instructions actually in force. Distinct from the contact
	// time above: an agent can be talking to the UI happily while running a
	// configuration from before a partition it has not noticed ended.
	configAge := int64(-1)
	if cached.FetchedAtUnix > 0 {
		configAge = now.Unix() - cached.FetchedAtUnix
	}
	// Emitted only once measured: a hardcoded 0 would be indistinguishable
	// from "perfectly in sync", which is the one reading nobody should get
	// for free.
	if uiSkewKnown {
		g("vmsync_agent_ui_clock_skew_seconds", "Control-plane clock minus this host's, in seconds. Timestamps written by different machines are compared throughout vmsync, so drift here makes those comparisons quietly wrong.", int64(uiSkew.Seconds()), "")
	}
	g("vmsync_agent_config_age_seconds", "Age of the configuration in force. -1 when it was never fetched.", configAge, "")

	// --- split brain ------------------------------------------------------
	//
	// The most alertable condition this agent can report. A non-zero value
	// means one VM is running in two places at once, which corrupts nothing
	// visibly and silently diverges two copies of the same data.
	m.mu.Lock()
	split := len(m.splitBrainVMs)
	splitVMs := sortedKeys(m.splitBrainVMs)
	incompleteVMs := sortedKeys(m.incompleteVMs)
	servedLiveVMs := sortedKeys(m.servedLiveVMs)
	fencedRunningVMs := sortedKeys(m.fencedRunningVMs)
	rpVMs := sortedKeys(m.restorePointVMs)
	lvVMs := sortedKeys(m.leftoverVMs)
	lvGauges := make(map[string]leftoverGauge, len(m.leftoverVMs))
	for vm, gg := range m.leftoverVMs {
		lvGauges[vm] = gg
	}
	rpGauges := make(map[string]restorePointGauge, len(m.restorePointVMs))
	for vm, gg := range m.restorePointVMs {
		rpGauges[vm] = gg
	}
	m.mu.Unlock()

	g("vmsync_agent_split_brain_vms", "VMs running on this host that a peer reports having been failed over from, OR that this host has already marked replication_role=fenced and is still running. Non-zero means one VM is live in two places; alert on it. The second case needs no peer query, so it is reported even when the peer is unreachable -- see vmsync_agent_fenced_running, which says what to DO about it.", split, "")
	// HELP and TYPE once, then one sample line per VM -- NOT g() per VM.
	//
	// g() emits its own HELP and TYPE on every call, and the Prometheus text
	// format allows each only once per metric family: a second HELP line for
	// the same name makes node_exporter reject THE WHOLE TEXTFILE, not the
	// one series. Two split-brain VMs on one host would therefore have taken
	// down every metric this agent publishes, including the one being alerted
	// on, and the file would have looked fine to anyone reading it by eye.
	//
	// The same rule governs vmsync_agent_mode and the per-VM timestamps
	// above, which is why they are written out longhand too.
	if len(splitVMs) > 0 {
		fmt.Fprintf(&b, "# HELP vmsync_agent_split_brain 1 while this host still runs a VM another host has been promoted for -- either because a reachable peer says so, or because this host already marked the VM replication_role=fenced and it is still running.\n# TYPE vmsync_agent_split_brain gauge\n")
		for _, vm := range splitVMs {
			fmt.Fprintf(&b, "vmsync_agent_split_brain{host=%q,vm=%q} 1\n", host, vm)
		}
	}

	c("vmsync_agent_fences_total", "Fences this agent has acted on, by result. A failure here needs a person: fences are never retried automatically.", m.fencesActed.Load(), `,result="success"`)
	fmt.Fprintf(&b, "vmsync_agent_fences_total{host=%q,result=\"failure\"} %d\n", host, m.fencesFailed.Load())
	// Independent of the two above: a fence can be unrecorded and still
	// succeed. Non-zero means this host shut a production VM down without a
	// record that survives a restart, so the same fence may be attempted
	// again -- and it means the ledger's filesystem is in trouble.
	g("vmsync_agent_config_generation", "Which generation of the configuration file is in force: 0 at startup, +1 per accepted reload.", m.configGen.Load(), "")
	c("vmsync_agent_config_rejected_total", "Reloads refused because the new file asked for something a running agent cannot do, such as moving state_dir. Needs a restart, not another edit.", m.configRejects.Load(), "")
	g("vmsync_agent_run_log_writable", "1 when the run log can be written. At 0 this host launches no syncs at all, because an unrecorded vmsync is refused.", boolGauge(m.runLogWritable.Load()), "")
	c("vmsync_agent_fences_unrecorded_total", "Fences that proceeded without a durable ledger record, because writing it failed and a split brain is the worse outcome.", m.fencesUnrecorded.Load(), "")

	// --- interrupted rebuilds ---------------------------------------------
	//
	// A full rebuild renamed a replica's good disks aside, began writing new
	// base images over them, and died. The domain's own metadata still says
	// last_checkpoint=<old>, last_sync_timestamp=<old>, failure_count=0, so
	// vmsync_agent_domains counts it healthy and nothing else in this file
	// moves at all. This is the only series that says otherwise, which is
	// what makes it worth alerting on as directly as split brain is.
	//
	// The count is emitted unconditionally, at zero when nothing is wrong,
	// because an alert cannot fire on the absence of a series that exists
	// only while the estate is broken.
	g("vmsync_agent_replica_incomplete_vms", "Replicas on this host whose full rebuild was started and never recorded as finished, so their disks are a partial copy. Non-zero means promoting one of them would bring up a half-written image; alert on it.", len(incompleteVMs), "")
	// HELP and TYPE once, then one sample line per VM, for the reason spelled
	// out at the split-brain gauge above: g() per VM would emit a second HELP
	// line for one family and node_exporter would reject the entire file.
	if len(incompleteVMs) > 0 {
		fmt.Fprintf(&b, "# HELP vmsync_agent_replica_incomplete 1 while a replica on this host carries an interrupted full rebuild (replica_incomplete). It clears by itself when a sync completes, because the define that records the sync clears the marker.\n# TYPE vmsync_agent_replica_incomplete gauge\n")
		for _, vm := range incompleteVMs {
			fmt.Fprintf(&b, "vmsync_agent_replica_incomplete{host=%q,vm=%q} 1\n", host, vm)
		}
	}

	// --- a copy that served live and nobody has decided about --------------
	//
	// The state that is silent everywhere else. A copy failed over to and then
	// shut down records replication_role=paused and loses the whole promotion
	// record, so its status reads `paused`, its last sync reads recent, and its
	// failure count reads zero -- and the source facing it has every sync
	// refused while none of those refusals touch failure_count, deliberately
	// (they are administrative, and a reinit cannot fix them). So the pair goes
	// quiet in both directions, and the only thing holding the data is a
	// replica that looks like somebody paused it for maintenance.
	//
	// It is the one gauge in this file that nothing clears by itself. A person
	// runs -release-promotion, or an -invert makes the copy the source. That is
	// the point rather than a defect: a value that has not moved in a week is a
	// decision nobody has made about production data, not a stuck series.
	//
	// Both of those must actually CLEAR it, which is what servedLiveUnresolved
	// is for and why presence of the record is not the test. An inversion KEEPS
	// the trace on the copy it makes the source; that copy is running, and
	// -release-promotion refuses a running domain. So a gauge keyed on the record
	// minus `promoted` would sit at 1 for ever on a fully resolved pair -- which
	// on a gauge whose whole claim is "this is not a stuck series" is the one
	// thing it must never do.
	//
	// Emitted unconditionally, at zero when there is nothing to say, for the
	// reason given at the split-brain gauge: an alert cannot use a `for:` clause
	// over a window that begins before the series it reads came into existence.
	g("vmsync_agent_served_live_unreleased_vms", "Copies on this host that have been promoted at some point (last_promoted_at), are NOT the authoritative copy of their pair, and whose promotion record has not been released. Their disks may hold the only copy of data that was serving, so vmsync refuses to sync, restore or force-clean over them -- force-clean included -- and replication into them is stopped with failure_count deliberately left at 0, so this is the only series that says so. Alert on it with a long for:. Two things clear it, both deliberate: -release-promotion on the copy while it is shut down, or an -invert that makes this copy the primary of its pair. Excluded: a domain marked promoted (a failover in force), and one marked source that is nobody's replica (the primary an inversion produced) -- both are resolved states, not pending ones. A source that still records a replica_source is NOT excluded: that is -update-role source typed at a promoted copy rather than a real inversion, both ends then read source, and nothing replicates between them. A DR drill that was promoted and then demoted DOES register, and does block replication until somebody decides -- the engine cannot know whether that copy was ever booted, so it protects the disks either way.", len(servedLiveVMs), "")
	// HELP and TYPE once, then one sample line per VM -- NOT g() per VM, for the
	// reason spelled out at the split-brain gauge: a second HELP line for one
	// family makes node_exporter reject the whole file.
	if len(servedLiveVMs) > 0 {
		fmt.Fprintf(&b, "# HELP vmsync_agent_served_live_unreleased 1 while that VM has served live, is not the authoritative copy of its pair, and its promotion record has not been released. Decide: -invert if the failover stands and this copy is the real one (which makes it the source and clears this), or shut it down and run -release-promotion here if its data is disposable.\n# TYPE vmsync_agent_served_live_unreleased gauge\n")
		for _, vm := range servedLiveVMs {
			fmt.Fprintf(&b, "vmsync_agent_served_live_unreleased{host=%q,vm=%q} 1\n", host, vm)
		}
	}

	// --- a fence that did not stop the domain ------------------------------
	//
	// The state no other signal here can express. A fence writes
	// replication_role=fenced even when the ACPI shutdown fails -- deliberately,
	// because that role is the only thing stopping replication resuming into the
	// split brain -- so a sweep that skipped fenced domains would let the
	// split-brain gauge fall back to zero about a minute later while one VM
	// stayed live in two places. The alternative trace is a failure COUNTER,
	// which says "once, at some point" and resets when the agent restarts.
	//
	// Emitted unconditionally, at zero when nothing is wrong, for the reason
	// given at the split-brain gauge above: an alert cannot use a `for:` clause
	// over a window that begins before the series it reads came into existence.
	g("vmsync_agent_fenced_running_vms", "VMs on this host that a fence marked replication_role=fenced and that are STILL RUNNING. The fence suspended replication but did not stop the domain, so it is live beside the peer that displaced it and both are taking writes. This is NOT the same question as split brain: which copy wins is already decided, so the remedy is always to stop THIS domain by hand and then set its role. It is never retried automatically, so it does not clear by itself -- a value that has not moved in a day is an unresolved split brain, not a stuck metric. Alert on it.", len(fencedRunningVMs), "")
	// HELP and TYPE once, then one sample line per VM -- NOT g() per VM, for the
	// reason spelled out at the split-brain gauge: a second HELP line for one
	// family makes node_exporter reject the whole file.
	if len(fencedRunningVMs) > 0 {
		fmt.Fprintf(&b, "# HELP vmsync_agent_fenced_running 1 while that VM is marked replication_role=fenced and still running. Stop the domain by hand, then -invert if the failover stands or -update-role=source if the fence was wrong.\n# TYPE vmsync_agent_fenced_running gauge\n")
		for _, vm := range fencedRunningVMs {
			fmt.Fprintf(&b, "vmsync_agent_fenced_running{host=%q,vm=%q} 1\n", host, vm)
		}
	}

	// --- restore points ---------------------------------------------------
	//
	// The fleet view of what each replica can be rolled back to. The engine's
	// own textfile describes the last RUN and is rewritten by the next one;
	// these describe the STORE, and they keep describing it when no sync is
	// running at all -- which is exactly when the depth stops growing and
	// nothing else notices.
	//
	// Emitted for every replica INCLUDING at zero, and that is the point: a
	// series that appears only once a replica has restore points cannot carry a
	// `for:` clause over a window that starts before it did, so the alert for
	// "this replica has none" could never fire. Gated on being a target, because
	// a source has no store and a zero for one would read as a fault.
	//
	// One HELP and TYPE per family, then one sample line per VM, for the reason
	// spelled out at the split-brain gauge above: g() per VM would emit a second
	// HELP for one family and node_exporter would reject the entire file.
	if len(rpVMs) > 0 {
		for _, fam := range []struct {
			name, help string
			value      func(restorePointGauge) int64
		}{
			{"vmsync_agent_restore_points", "How many restore points each replica on this host holds in its own per-domain store. Directories, including any whose sidecar cannot be read -- subtract vmsync_agent_restore_points_unreadable for the depth that is provably restorable. Zero for a replica with -retention configured means the history has stopped growing: alert on it together with the engine's vmsync_restore_point_overdue_seconds.", func(g restorePointGauge) int64 { return int64(g.Count) }},
			{"vmsync_agent_restore_points_unreadable", "How many of a replica's restore points have a sidecar that could not be read. Still counted in vmsync_agent_restore_points, because the directory is the inventory -- but a restore from one is refused, so this is the gap between what the store holds and what can actually be used.", func(g restorePointGauge) int64 { return int64(g.Unreadable) }},
			{"vmsync_agent_restore_points_verify_failed", "How many of a replica's restore points recorded a FAILED verification. Not a reason to delete them -- they may be the only copies from before the damage -- but an operator choosing one must know, and a replica where this equals vmsync_agent_restore_points has no copy that was ever shown to be clean.", func(g restorePointGauge) int64 { return int64(g.VerifyFailed) }},
			{"vmsync_agent_restore_point_newest_timestamp_seconds", "The instant the NEWEST restore point's contents correspond to, per replica. The tag instant, the same value the engine publishes as vmsync_restore_point_last_taken_timestamp_seconds, so a dashboard and an alert cannot disagree about the age of one copy. Zero when there are none.", func(g restorePointGauge) int64 { return g.NewestUnix }},
			{"vmsync_agent_restore_point_oldest_timestamp_seconds", "The instant the OLDEST restore point's contents correspond to, per replica. time() minus this is how far back the replica can actually be taken, which is the number an operator reaches for during an incident. Zero when there are none.", func(g restorePointGauge) int64 { return g.OldestUnix }},
		} {
			fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", fam.name, fam.help, fam.name)
			for _, vm := range rpVMs {
				fmt.Fprintf(&b, "%s{host=%q,vm=%q} %d\n", fam.name, host, vm, fam.value(rpGauges[vm]))
			}
		}
	}

	// --- displaced sets nothing reaps ---------------------------------------
	//
	// Made by the DEFAULT replaced-disk action, removed by no sweep, and until now
	// named by nothing: the aside restore-point STORES in particular are invisible
	// to every listing vmsync has, because they sit beside the per-domain stores
	// and the points inside them are outside every store. They also share extents
	// when new, so a df or du on the day one is made reports almost nothing and an
	// operator reasonably concludes the space is free. A number that grows is the
	// only warning available, and the failure it ends in is an ENOSPC on the DR
	// host that fails a commit for every VM on it.
	//
	// Emitted for every replica INCLUDING at zero, like the restore-point families
	// above and for the same reason: a series that appears only once a host has
	// leftovers cannot alert on them appearing, and cannot show the flat line that
	// says a cleanup held.
	if len(lvVMs) > 0 {
		for _, fam := range []struct {
			name, help string
			value      func(leftoverGauge) int64
		}{
			{"vmsync_agent_replaced_sets", "Displaced sets beside each replica on this host that nothing has removed: disks a rebuild renamed aside, whole restore-point stores a reinit set aside, and restore copies staged and never used. No sweep reaps them, deliberately -- the aside disks are the documented recovery for an interrupted rebuild, so deleting them automatically would delete what an operator in trouble reaches for. Alert on the bytes below rather than on this count.", func(g leftoverGauge) int64 { return int64(g.Count) }},
			{"vmsync_agent_replaced_bytes", "What those displaced sets actually occupy per replica: allocated, not apparent. A fresh aside is a reflink sharing every extent with the live file, so its apparent size is a whole disk image while its real cost is nothing; this is the divergence, and it grows as the replica is written. THE ONE TO ALERT ON, against the free space on that filesystem -- the end state is an ENOSPC on the DR host that fails a commit for every VM there, and the first symptom is a reinit that cannot rename its aside.", func(g leftoverGauge) int64 { return g.Bytes }},
			{"vmsync_agent_replaced_oldest_timestamp_seconds", "The earliest stamp among a replica's displaced sets, 0 when none carries one. time() minus this is how long the oldest has been sitting there, which separates last night's rebuild from one nobody has looked at in months.", func(g leftoverGauge) int64 { return g.OldestUnix }},
		} {
			fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", fam.name, fam.help, fam.name)
			for _, vm := range lvVMs {
				fmt.Fprintf(&b, "%s{host=%q,vm=%q} %d\n", fam.name, host, vm, fam.value(lvGauges[vm]))
			}
		}
	}

	return b.String()
}

func boolGauge(b bool) int {
	if b {
		return 1
	}
	return 0
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// writeMetrics replaces the agent's textfile atomically.
//
// The temp file is created in the same directory and renamed into place, so
// node_exporter -- which may read at any moment -- sees either the previous
// content or the new one, never a half-written file it would report as a
// parse error.
func (m *agentMetrics) writeMetrics(dir string, cached CachedConfig, sched *Scheduler, hostLimit int, now time.Time) error {
	body := m.render(cached, sched, hostLimit, now)
	path := filepath.Join(dir, agentMetricsFile)

	tmp, err := os.CreateTemp(dir, ".vmsync-agent-metrics-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp metrics file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.WriteString(body); err != nil {
		tmp.Close()
		return fmt.Errorf("write metrics: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod metrics: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close metrics: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("install %s: %w", path, err)
	}
	return nil
}

// metricsLoop rewrites the textfile on a timer.
//
// A timer rather than an event: the values that matter most here age on
// their own -- how long since the UI answered, how overdue a sync is -- so
// they must be refreshed even when nothing at all is happening, which is
// exactly the state being alerted on.
func metricsLoop(ctx context.Context, lv *live, state *sharedState, sched *Scheduler, m *agentMetrics, scanInventory bool) {
	ticker := time.NewTicker(metricsInterval)
	defer ticker.Stop()

	var lastErr string
	for {
		// One snapshot per write, so prometheus_dir and the concurrency
		// ceiling in a single file always come from the same generation.
		cfg := *lv.get()
		if scanInventory {
			if scan, err := scanDomainStatus(cfg); err == nil {
				m.setDomains(scan.total, scan.byStatus)
				// From the same sweep, so the two can never describe
				// different moments -- and so a standalone host, which has
				// no console to show a report on, still publishes the one
				// signal that a replica's disks are a partial copy.
				m.setReplicaIncomplete(scan.incomplete)
				m.setFencedRunning(scan.fencedRunning)
				m.setServedLiveUnreleased(scan.servedLive)
				// Likewise the restore point depth. A standalone host is the
				// one place these numbers have no other route out: there is
				// no control plane to send a report to, so without this a
				// replica quietly taking no restore points is invisible.
				m.setRestorePoints(scan.restorePoints)
				m.setLeftovers(scan.leftovers)
			}
		}
		if err := m.writeMetrics(cfg.PrometheusDir, state.get(), sched, cfg.MaxConcurrentSyncs, time.Now()); err != nil {
			// Logged once per distinct message: a full disk or a bad path
			// would otherwise fill the journal faster than the thing it is
			// reporting on.
			if msg := err.Error(); msg != lastErr {
				trace.Error("could not write agent metrics", "dir", cfg.PrometheusDir, "error", err)
				lastErr = msg
			}
		} else {
			lastErr = ""
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// inventoryScan is one sweep of this host, reduced to what the metrics need.
//
// A struct rather than a list of unnamed returns: it gained a fourth reading and
// the call site was assigning positionally, which is how a feeder comes to
// publish the right numbers under the wrong name.
//
// The incomplete set is carried by NAME rather than folded into the status
// counts, because the alert an operator writes is about a specific VM: "one
// of your replicas is a partial copy" without saying which is an alert that
// cannot be acted on at 3am. restorePoints is per VM for the same reason.
type inventoryScan struct {
	total         int
	byStatus      map[string]int
	incomplete    map[string]bool
	fencedRunning map[string]bool
	servedLive    map[string]bool
	restorePoints map[string]restorePointGauge
	// leftovers is the displaced-set cost per replica, from the same walk: on a
	// standalone host nothing else will ever report it.
	leftovers map[string]leftoverGauge
}

// scanDomainStatus inventories the host: domains counted by assessed status, the
// ones carrying an interrupted full rebuild named, and each replica's restore
// point depth.
//
// Only used in standalone mode. With a control plane, reportLoop already
// scans on its own interval and feeds the result in, so doing it here as
// well would double the libvirt work for the same numbers.
func scanDomainStatus(cfg agentConfig) (inventoryScan, error) {
	mgr, err := libvirtsync.Connect(cfg.LibvirtURI)
	if err != nil {
		return inventoryScan{}, err
	}
	defer mgr.Close()

	// inventory.Scan fills RestorePoints as part of describing each domain, so
	// the depth below costs no extra walk -- see pkg/inventory/restorepoints.go.
	domains, err := inventory.Scan(mgr)
	if err != nil {
		return inventoryScan{}, err
	}
	byStatus := map[string]int{}
	incomplete := map[string]bool{}
	fencedRunning := map[string]bool{}
	servedLive := map[string]bool{}
	restorePoints := map[string]restorePointGauge{}
	leftovers := map[string]leftoverGauge{}
	now := time.Now()
	for _, d := range domains {
		// Presence is the state, exactly as pkg/inventory documents it: any
		// value at all means a rebuild was armed and no run ever recorded
		// finishing it. Matching on a known value here would let a newer
		// engine's marker publish a zero.
		if d.ReplicaIncomplete != "" {
			incomplete[d.Name] = true
		}
		// A copy that served live, is not the authoritative one, and that nobody
		// has decided about. Standalone matters most for this one: there is no
		// console to show the row on, the refusals it causes are exempt from
		// failure_count, and the domain's own role reads `paused` -- so without
		// this gauge nothing on the host says anything at all.
		//
		// Through the SAME function the report path uses, so a standalone host
		// and a controlled one cannot disagree about which copies are waiting.
		// Two copies of the role test would be two places to fix the next time
		// the resolved states change.
		if servedLiveUnresolved(d.LastPromotedAtRaw, d.Role, d.ReplicaSource) {
			servedLive[d.Name] = true
		}
		// A fence that suspended replication without stopping the domain. In
		// standalone mode there is no control plane and no report, so this sweep
		// is the only thing that would ever notice.
		if fenceFailedOpen(d.Role, d.Active) {
			fencedRunning[d.Name] = true
		}
		// .String(), not a string() conversion: inventory.Status is an int
		// enum, so converting it would compile and silently yield a one-rune
		// control character as the label value.
		//
		// cadence 0 means "no expected interval", which is right here: the
		// cadences the UI distributes are for judging whether a TARGET is
		// behind, and in standalone mode there is no estate-wide view to
		// draw them from. Freshness is then simply not judged, rather than
		// judged against a number invented locally.
		byStatus[inventory.Assess(d, now, 0).Status.String()]++

		// Through the same reduction the report path uses, fed by the same
		// conversion, so a standalone host and a controlled one cannot count a
		// store differently. Targets only: a source has no store, and a zero
		// for one would read as a starved replica.
		if d.IsTarget() {
			restorePoints[d.Name] = restorePointGaugeFor(reportRestorePoints(d.RestorePoints))
			// Through the same reducer the report path uses, so a standalone host
			// and a controlled one cannot disagree about what is sitting there.
			for _, g := range leftoverGauges([]ReportDomain{{Name: d.Name, ReplicaSource: d.ReplicaSource, Leftovers: reportLeftovers(d.Leftovers)}}) {
				leftovers[d.Name] = g
			}
		}
	}
	return inventoryScan{total: len(domains), byStatus: byStatus, incomplete: incomplete, fencedRunning: fencedRunning, servedLive: servedLive, restorePoints: restorePoints, leftovers: leftovers}, nil
}

// statusCounts tallies a report's domains by status, for the gauge.
func statusCounts(domains []ReportDomain) map[string]int {
	out := map[string]int{}
	for _, d := range domains {
		out[d.Status]++
	}
	return out
}

// incompleteReplicaVMs names the domains in a report whose full rebuild was
// started and never recorded as finished.
//
// Taken from the REPORT rather than from a second libvirt walk, so the
// metric and the report a control-plane agent just sent describe the same
// sweep. A host publishing a gauge that disagrees with the console for the
// same minute is worse than one publishing neither.
//
// Presence is the state: any value counts, including one this build cannot
// parse. Reading an unfamiliar marker as "nothing wrong" is the failure mode
// this whole field exists to close.
func incompleteReplicaVMs(domains []ReportDomain) map[string]bool {
	out := map[string]bool{}
	for _, d := range domains {
		if d.ReplicaIncomplete != "" {
			out[d.Name] = true
		}
	}
	return out
}

// servedLiveUnreleasedVMs is the same shape for the promotion trace, taken from
// the same report for the same reason.
//
// Presence is the state, including a value this build cannot parse: reading an
// unfamiliar record as "never promoted" is the failure this field exists to
// close, and it is the direction that overwrites production data.
//
// The `promoted` exclusion is the whole of the predicate's judgement. A domain
// still marked promoted is a failover in progress or a drill: nothing is waiting
// on anybody, the role interlock already refuses everything, and counting it
// would make this gauge non-zero for the duration of every successful failover
// -- which is how a signal that matters stops being read.
func servedLiveUnreleasedVMs(domains []ReportDomain) map[string]bool {
	out := map[string]bool{}
	for _, d := range domains {
		if servedLiveUnresolved(d.LastPromotedAt, d.Role, d.ReplicaSource) {
			out[d.Name] = true
		}
	}
	return out
}

// servedLiveUnresolved reports whether a copy has served live and is STILL
// WAITING ON A DECISION. Pure, and shared by both feeders, because the two used
// to carry the role test separately and that is how they would drift apart.
//
// Presence of the record is necessary but not sufficient, and getting that wrong
// was a real defect in this gauge's first version. It reported on the record
// alone minus `promoted`, which made it fire FOR EVER on the primary of every
// correctly inverted pair: an inversion deliberately keeps the trace on the copy
// it makes the source (pkg/failover's NewSourceRemovals says so in as many
// words), that copy is running by definition, and -release-promotion refuses a
// running domain -- so the only way to clear the series was to shut production
// down. On a metric documented as "a value that has not moved in days is an
// undecided failover", that is the worst possible failure.
//
// Two states mean this copy IS the authoritative one and nothing is pending:
//
//   - `promoted`: a failover in force. The role interlock already refuses
//     everything, and counting it would make the gauge non-zero for the whole
//     duration of every successful failover.
//   - `source` AND no replica_source: the primary of its pair, which is where an
//     -invert leaves the copy that served. The pair is RESOLVED -- that is what
//     the inversion did. TargetRoleAllowsSync refuses `source` outright and
//     -force-clean does not override it, so the trace guards nothing here that
//     the role does not guard harder.
//
// THE replica_source CLAUSE IS NOT DECORATION, and leaving it out was a real
// hole. `source` alone is a claim, and an operator can write it by hand: a
// promoted copy demoted with `-update-role source` gets the role and keeps its
// replica_source, because SetReplicationRole strips only promotionFields. Both
// ends of the pair then read `source`, every sync between them is refused, that
// refusal is exempt from failure_count -- and if this function trusted the role
// alone, every signal would go quiet on a pair that had stopped replicating in
// both directions. A real -invert is what clears replica_source (pkg/failover's
// NewSourceRemovals lists it first), so requiring it empty is exactly the
// difference between "an inversion resolved this" and "somebody typed a word
// at it".
//
// Every other state -- paused, fenced, target, no role recorded, or a `source`
// that is still somebody's replica -- is a copy that holds data which served
// live while something else is, or is about to be, the authoritative copy. That
// is the state nothing else on the host reports.
//
// The trace is still KEPT on a source, and this function deliberately does not
// change that: it gates -update-role target, and making the live primary of a
// pair into a replica is exactly as destructive as overwriting a promoted copy.
// What is decided here is the REPORTING, not the protection.
func servedLiveUnresolved(lastPromotedAt, role, replicaSource string) bool {
	if lastPromotedAt == "" {
		return false
	}
	if role == libvirtsync.RolePromoted {
		return false
	}
	return !(role == libvirtsync.RoleSource && replicaSource == "")
}

// clockSkewWarnAt is when a UI clock difference stops being noise.
//
// Generous because the HTTP Date header has one-second granularity, so a
// reading is never better than about a second, and because a couple of
// seconds changes no decision anyone makes. What it must catch is the case
// where NTP has genuinely stopped working somewhere: tens of seconds or
// more, which is enough to invert the comparison between a target's
// last_sync and a source's last_replicated and make the wrong copy look
// newer.
const clockSkewWarnAt = 30 * time.Second

// clockSkewWarnEvery bounds how often the warning repeats. The metric
// carries the value continuously; the log line exists to be noticed once,
// not to fill a journal every poll.
const clockSkewWarnEvery = time.Hour

// recordUIClockSkew stores the offset between this host's clock and the
// control plane's, and warns when it grows large enough to matter.
func (m *agentMetrics) recordUIClockSkew(d time.Duration) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.uiClockSkew = d
	m.uiClockSkewKnown = true
	warn := false
	if d < -clockSkewWarnAt || d > clockSkewWarnAt {
		if m.uiSkewWarnedAt.IsZero() || time.Since(m.uiSkewWarnedAt) > clockSkewWarnEvery {
			m.uiSkewWarnedAt = time.Now()
			warn = true
		}
	}
	m.mu.Unlock()

	if warn {
		trace.Warning("this host's clock disagrees with the control plane's; vmsync compares timestamps written by different machines, so replication ages and failover data-loss windows will be wrong until NTP is fixed",
			"skew_seconds", int64(d.Seconds()), "threshold_seconds", int64(clockSkewWarnAt.Seconds()))
	}
}

// leftoverGauge is one replica's displaced-set cost.
type leftoverGauge struct {
	// Count is how many displaced sets there are, and Bytes what they actually
	// occupy right now -- allocated, not apparent, because a fresh reflink aside
	// shares every extent with the live file and only diverges as the replica is
	// written. Apparent size would alarm on the day the aside was made and never
	// move; allocated size is the number that grows.
	Count int
	Bytes int64
	// Oldest is the earliest stamp among them, 0 when none carries one. Age is
	// what separates yesterday's rebuild from one nobody has looked at since
	// spring.
	OldestUnix int64
}

// setLeftovers replaces the whole map from one sweep, for the reason every setter
// here does: a replica whose leftovers were cleaned up by hand must stop being
// reported, and a merge would keep publishing a cost that is no longer there.
func (m *agentMetrics) setLeftovers(vms map[string]leftoverGauge) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.leftoverVMs = make(map[string]leftoverGauge, len(vms))
	for vm, g := range vms {
		m.leftoverVMs[vm] = g
	}
}

// leftoverGauges reduces a report's domains to the per-VM cost.
//
// From the REPORT rather than a second filesystem walk, like every other gauge
// here, so the number the console shows and the number the metric publishes come
// from one sweep and cannot disagree for the same minute.
func leftoverGauges(domains []ReportDomain) map[string]leftoverGauge {
	out := map[string]leftoverGauge{}
	for _, d := range domains {
		// Emitted for every replica, INCLUDING at zero, and that is the point: a
		// series that appears only once a host has leftovers cannot alert on them
		// appearing, and cannot show the flat line that says a cleanup held.
		if d.ReplicaSource == "" {
			continue
		}
		g := leftoverGauge{}
		for _, l := range d.Leftovers {
			g.Count++
			g.Bytes += l.Bytes
			if l.AtUnix > 0 && (g.OldestUnix == 0 || l.AtUnix < g.OldestUnix) {
				g.OldestUnix = l.AtUnix
			}
		}
		out[d.Name] = g
	}
	return out
}
