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
	skipTargetBudget    = "target_budget"
	skipAlreadyRunning  = "already_running"
	skipInvalidProfile  = "invalid_profile"
	skipNoTarget        = "no_target"
)

var skipReasons = []string{
	skipHostConcurrency,
	skipTargetBudget,
	skipAlreadyRunning,
	skipInvalidProfile,
	skipNoTarget,
}

// agentMetrics is what the agent knows about itself.
//
// Deliberately separate from the per-VM metrics vmsync writes. Those say
// whether a sync that RAN succeeded; these say whether one was ever
// attempted -- and the failure this exists to catch is the silent one, a VM
// that is configured, never runs, and therefore never writes a per-VM file
// to go stale in the first place.
type agentMetrics struct {
	version    string
	hostname   string
	standalone bool
	startedAt  time.Time

	runsOK   atomic.Uint64
	runsFail atomic.Uint64
	skips    map[string]*atomic.Uint64 // fixed keys; never written after construction

	uiLastContact atomic.Int64 // unix seconds, 0 = never
	uiFailures    atomic.Uint64

	mu           sync.Mutex
	domainTotal  int
	domainStatus map[string]int
	lastAttempt  map[string]int64
}

func newAgentMetrics(version, hostname string, standalone bool) *agentMetrics {
	m := &agentMetrics{
		version:      version,
		hostname:     hostname,
		standalone:   standalone,
		startedAt:    time.Now(),
		skips:        make(map[string]*atomic.Uint64, len(skipReasons)),
		domainStatus: map[string]int{},
		lastAttempt:  map[string]int64{},
	}
	for _, r := range skipReasons {
		m.skips[r] = &atomic.Uint64{}
	}
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
	g("vmsync_agent_standalone", "1 when scheduling from a local file with no control plane.", boolGauge(m.standalone), "")

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
	g("vmsync_agent_config_age_seconds", "Age of the configuration in force. -1 when it was never fetched.", configAge, "")

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
func metricsLoop(ctx context.Context, cfg agentConfig, state *sharedState, sched *Scheduler, m *agentMetrics, scanInventory bool) {
	ticker := time.NewTicker(metricsInterval)
	defer ticker.Stop()

	var lastErr string
	for {
		if scanInventory {
			if total, byStatus, err := scanDomainStatus(cfg); err == nil {
				m.setDomains(total, byStatus)
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

// scanDomainStatus inventories the host and counts domains by assessed
// status.
//
// Only used in standalone mode. With a control plane, reportLoop already
// scans on its own interval and feeds the result in, so doing it here as
// well would double the libvirt work for the same numbers.
func scanDomainStatus(cfg agentConfig) (int, map[string]int, error) {
	mgr, err := libvirtsync.Connect(cfg.LibvirtURI)
	if err != nil {
		return 0, nil, err
	}
	defer mgr.Close()

	domains, err := inventory.Scan(mgr)
	if err != nil {
		return 0, nil, err
	}
	byStatus := map[string]int{}
	now := time.Now()
	for _, d := range domains {
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
	}
	return len(domains), byStatus, nil
}

// statusCounts tallies a report's domains by status, for the gauge.
func statusCounts(domains []ReportDomain) map[string]int {
	out := map[string]int{}
	for _, d := range domains {
		out[d.Status]++
	}
	return out
}
