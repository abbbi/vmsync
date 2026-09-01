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

// vmsync-agent runs on every hypervisor and reports what that host knows
// about its own replication state to the control-plane UI.
//
// Through phase 2 this binary was read-only by construction. Phase 3 adds
// scheduling: it now runs vmsync for the VMs the UI's schedule names. What
// it still cannot do is change a replication role or touch a domain
// directly -- and vmsync's own replication_role check remains the backstop
// under all of it, refusing to overwrite a promoted or paused target no
// matter what any schedule says.
//
// The schedule is typed data, never a command line. The agent owns the flag
// vocabulary (see SyncProfile.CommandArgs), validates every field before
// building anything, supplies credentials from its own local configuration,
// and executes via exec.Command with no shell -- so there is nothing for a
// compromised UI to inject into. Install with -no-schedule to keep a host
// read-only until you are ready for it to run syncs.
//
// The agent dials out and never listens, so a hypervisor needs no inbound
// port. It caches the UI's configuration on disk and keeps running from
// that cache when the UI is unreachable: the UI lives at the DR site, a WAN
// partition is an ordinary event, and an agent that stopped working without
// it would make the control plane a single point of failure for the thing
// it exists to protect.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"vmsync/pkg/inventory"
	"vmsync/pkg/libvirtsync"
	"vmsync/pkg/trace"
	"vmsync/pkg/util"
	"vmsync/pkg/version"
)

type agentConfig struct {
	// Gen is which generation of the configuration file this is: 0 at
	// startup, incremented by every accepted reload. Inside the struct rather
	// than a counter beside it, so a reader that holds a pointer can always
	// say WHICH configuration it is acting on.
	Gen uint64
	// ConfigPath is the file this configuration came from, kept so a reload
	// knows what to re-read and so errors can name it.
	ConfigPath  string
	UIBase      string
	EnrolToken  string
	CAFile      string
	StateDir    string
	LibvirtURI  string
	Hostname    string
	HTTPTimeout time.Duration
	Once        bool
	Debug       bool

	// Everything below is local host configuration used when running a
	// scheduled sync. None of it comes from the UI: how to reach another
	// hypervisor, and which binary to run, is the host's own business, and a
	// UI compromise must not be able to redirect either.
	VmsyncPath       string
	BridgeHelperPath string
	TargetURIPattern string
	PrometheusDir    string
	SSHUser          string
	SSHKey           string
	SSHPort          int
	SSHKnownHosts    string
	NoSchedule       bool
	NoAutoFence      bool
	// MaxConcurrentSyncs is this host's own ceiling on parallel syncs. See
	// effectiveMaxConcurrent: it can only lower what the schedule asks for,
	// never raise it, because how much concurrent I/O this machine can
	// absorb is something only this machine knows.
	MaxConcurrentSyncs int
	// StandaloneFile, when set, makes this a scheduler and nothing else:
	// the schedule is read from that path and no control plane is involved
	// at any point. Mutually exclusive with -ui and everything that only
	// means something alongside one.
	StandaloneFile string

	// metrics is this agent's own metric state, carried here so the
	// scheduler and both run paths reach the same instance without a
	// package-level variable. Nil when -prometheus-dir is unset, and every
	// method on it is nil-guarded, so no use site needs to branch.
	metrics *agentMetrics

	// runLog is the durable record of every vmsync this agent starts,
	// carried here for the same reason metrics is: the scheduler, the
	// operations loop and the fence loop must all write to one instance.
	//
	// Unlike metrics it is NEVER nil in a running agent, and its methods are
	// not nil-guarded for launches. A launch that cannot be recorded does not
	// happen (see runLog.Append), so a nil here would silently turn the
	// fail-closed contract into a no-op -- exactly the class of "reports
	// success while doing nothing" this agent exists to refuse.
	runLog *runLog
}

// Everything else this agent needs now lives in the config file. These four
// remain because none of them is a SETTING: they select a file, or they say
// what this one invocation is for.
//
// The nineteen that went are not merely relocated. A flag can only be changed
// by restarting the process, and a daemon whose settings can only be changed
// by restarting it is one an operator hesitates to touch during an incident --
// which is when they most need to.
func main() {
	var (
		configPath     = flag.String("config", "/etc/vmsync/agent.json", "Path to this agent's configuration. Everything except the flags listed here lives in that file; see the agent README")
		once           = flag.Bool("once", false, "Report once and exit, instead of running as a daemon. For verifying a new install")
		debug          = flag.Bool("debug", false, `Force debug logging on, whatever "log.debug" says, until this agent is restarted`)
		enrolTokenFile = flag.String("enrol-token-file", "", "Path to a file holding a single-use enrolment token. Read once and then DELETED, so the token does not outlive its use. Only needed until enrolment succeeds")
		showVersion    = flag.Bool("v", false, "Show version and exit")
		showVersionL   = flag.Bool("version", false, "Show version and exit")
	)
	flag.Parse()

	if *showVersion || *showVersionL {
		fmt.Println(version.Version)
		os.Exit(0)
	}
	// Same reasoning as both other binaries: this takes no positional
	// arguments, so leftovers mean a flag was mistyped and silently dropped.
	if flag.NArg() > 0 {
		trace.Error("invalid command line", "error", fmt.Errorf("unexpected extra argument(s) %v", flag.Args()))
		os.Exit(2)
	}

	// Debug is turned on BEFORE the config is read, so that a file which fails
	// to load can be diagnosed with the flag that exists for diagnosing it.
	trace.SetDebug(*debug)

	af, warnings, err := LoadAgentFile(*configPath)
	if err != nil {
		trace.Error("invalid configuration", "error", err)
		os.Exit(2)
	}
	for _, w := range warnings {
		trace.Warning("configuration hygiene", "detail", w)
	}

	cfg, err := resolveAgentConfig(af, *configPath, *once, *debug, *enrolTokenFile)
	if err != nil {
		trace.Error("invalid configuration", "error", err)
		os.Exit(2)
	}
	trace.SetDebug(cfg.Debug)
	if *debug && !af.Log.Debug {
		trace.Warning(`--debug on the command line overrides "log.debug"; it stays on until this agent is restarted, and a reload cannot turn it off`)
	}

	// This host has TWO names when libvirt_uri points somewhere else, and
	// they are used for different things: hostname is what the UI knows this
	// agent as, while replication metadata identifies it by the URI's host,
	// because util.ReplicaHost prefers that and ignores the local name
	// whenever the URI has one.
	//
	// Not an error -- both names are doing their job -- but it is worth saying
	// once, because everything about a pair (fence tokens, replica_source,
	// which row in the console) then keys off a name that is not the one in
	// the file. It is also exactly the configuration in which the fence check
	// used to compare the wrong string and silently never fire.
	if h := util.ReplicaHost(cfg.LibvirtURI, cfg.Hostname); h != cfg.Hostname {
		trace.Warning("this agent reports under one name and appears in replication metadata under another, because libvirt_uri names a remote host",
			"reports_as", cfg.Hostname, "replica_identity", h, "libvirt_uri", cfg.LibvirtURI)
	}
	if cfg.PrometheusDir != "" {
		cfg.metrics = newAgentMetrics(version.Version, cfg.Hostname, cfg.StandaloneFile != "")
	}

	// Opened before anything can launch, and fatal if it cannot be.
	//
	// Refusing to start is the right failure here precisely because the
	// alternative is worse than an outage: an agent that starts without a run
	// log cannot record what it runs, and under the fail-closed contract it
	// would then refuse every sync -- looking healthy in every other respect
	// while replicating nothing. A host that will not start says so.
	//
	// The session id ties every record this process writes to this process,
	// which is what lets a later agent tell "a run I started" from "a run
	// somebody else's instance started and may still be running".
	cfg.runLog = newRunLog(cfg.StateDir, newRunID(), cfg.metrics)
	if err := cfg.runLog.Open(); err != nil {
		trace.Error("cannot open the run log, refusing to start: every vmsync this agent launches must be recorded before it is started, so without this file nothing can run",
			"state_dir", cfg.StateDir, "file", runLogFile, "error", err)
		os.Exit(2)
	}
	// Deliberately not deferred. Both exits below are os.Exit, which does not
	// run deferred functions, so a defer here would be decoration. Nothing is
	// lost by that: every Append fsyncs before it returns, so there is never
	// buffered data waiting on a Close.

	// One live configuration, shared by every loop, replaced wholesale by a
	// reload. Built here so both entry points get the same handle.
	lv := newLive(cfg)

	// The digest of the bytes that produced generation 0, so the first poll
	// does not mistake "unchanged" for "changed".
	initial, _ := os.ReadFile(*configPath)
	reloads := newReloader(lv, *configPath, configDigest(initial), *once, *debug)

	runner := run
	if cfg.StandaloneFile != "" {
		runner = runStandalone
	}
	if err := runner(lv, reloads); err != nil {
		trace.Error("agent stopped", "error", err)
		os.Exit(1)
	}
}

func run(lv *live, reloads *reloader) error {
	cfg := *lv.get()
	client, err := NewClient(cfg.UIBase, cfg.CAFile, cfg.HTTPTimeout)
	if err != nil {
		return err
	}
	store := Store{Dir: cfg.StateDir}

	// SIGTERM is how systemd stops this, and it must be prompt: there is
	// nothing to unwind, since the agent holds no libvirt job, no export and
	// no lock. Cancelling the context aborts an in-flight long poll too,
	// which would otherwise hold the shutdown open for its full wait.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		trace.Info("signal received, shutting down", "signal", sig.String())
		cancel()
	}()

	// SIGHUP is a RELOAD, not a shutdown. Registering it matters beyond the
	// feature: Go terminates on an unhandled SIGHUP, so before this,
	// `systemctl reload` was `systemctl kill` -- and the natural fallback,
	// `systemctl kill -s HUP`, signals the whole control group and would take
	// every in-flight vmsync down without its unwind path.
	hupCh := make(chan os.Signal, 1)
	signal.Notify(hupCh, syscall.SIGHUP)
	go reloads.Run(ctx, hupCh)

	creds, err := ensureEnrolled(ctx, client, store, cfg)
	if err != nil {
		return err
	}
	client.Creds = creds
	// Free: every HTTP response already carries a Date header, so the
	// control plane doubles as a clock reference without an extra request.
	client.OnClockSkew = cfg.metrics.recordUIClockSkew
	trace.Info("agent ready", "agent_id", creds.AgentID, "ui", client.Base, "hostname", cfg.Hostname, "libvirt_uri", cfg.LibvirtURI)

	cached, everFetched, err := store.LoadCache()
	if err != nil {
		return fmt.Errorf("load cached configuration: %w", err)
	}
	if !everFetched {
		trace.Info("no cached configuration yet; running on defaults until the UI answers")
	}

	if cfg.Once {
		return reportOnce(ctx, client, cfg, cached)
	}

	// Four independent loops sharing the cached configuration. Reporting is
	// on a timer; polling blocks in a long poll and returns as soon as the
	// UI has something to say, so an operator's change lands in seconds
	// without the agent accepting inbound connections; the scheduler runs
	// whatever the cached configuration says, including while the UI is
	// unreachable; and the operations loop executes one-shot instructions.
	state := &sharedState{cached: cached}
	var wg sync.WaitGroup

	// Loaded before anything can execute. A ledger that failed to load
	// would look like an agent that has never done anything, which would
	// re-run every operation the UI is still publishing -- so this is
	// fatal rather than best-effort.
	ledger := newOperationLedger(cfg.StateDir)
	if err := ledger.Load(); err != nil {
		return fmt.Errorf("load the operation ledger: %w", err)
	}

	var sched *Scheduler
	if !cfg.NoSchedule {
		sched = NewScheduler(lv, state)
		wg.Add(1)
		go func() { defer wg.Done(); sched.Run(ctx) }()
		trace.Info("scheduler running", "vmsync", cfg.VmsyncPath, "target_uri_pattern", cfg.TargetURIPattern)
	} else {
		trace.Info("scheduling disabled by -no-schedule; this agent will report and nothing else")
	}

	if cfg.metrics != nil {
		wg.Add(1)
		// scanInventory false: reportLoop already scans on its own
		// interval and feeds the counts in, so doing it here too would
		// double the libvirt work for the same numbers.
		go func() { defer wg.Done(); metricsLoop(ctx, lv, state, sched, cfg.metrics, false) }()
	}

	// Started regardless of -no-schedule. That flag means "do not run the
	// schedule", and a DR-site target host is exactly the machine most
	// likely to carry it -- it has no schedule of its own, so nobody ever
	// removes it -- while also being the machine a failover must run on.
	// Tying the two together would deliver a promotion to a visibly healthy
	// agent that silently ignores it.
	wg.Add(1)
	go func() { defer wg.Done(); operationsLoop(ctx, lv, state, ledger) }()

	// Also started regardless of -no-schedule, and for a sharper reason
	// than the operations loop: a source whose replication was disabled --
	// by the operator, or by the failover that displaced it -- is MORE
	// likely to be a split-brain risk, not less. Gating this on the
	// schedule would switch off the protection exactly where it is needed.
	fences := newFenceLedger(cfg.StateDir)
	if err := fences.Load(); err != nil {
		// Fatal for the same reason the operation ledger is: an agent that
		// cannot read which fences it has already acted on would act on
		// them again, and this ledger is the only thing making a fence
		// single-use.
		return fmt.Errorf("load the fence ledger: %w", err)
	}
	wg.Add(1)
	go func() { defer wg.Done(); fenceLoop(ctx, lv, state, fences) }()

	wg.Add(2)
	go func() { defer wg.Done(); reportLoop(ctx, client, lv, state, sched, ledger, fences) }()
	go func() { defer wg.Done(); pollLoop(ctx, client, store, state, cfg.metrics) }()
	wg.Wait()
	return nil
}

// sharedState guards the cached configuration, which the poll loop replaces
// and the report loop reads.
type sharedState struct {
	mu     sync.Mutex
	cached CachedConfig
}

func (s *sharedState) get() CachedConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cached
}

func (s *sharedState) set(c CachedConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cached = c
}

// ensureEnrolled returns a usable credential, enrolling first if needed.
//
// A stored credential issued by a DIFFERENT UI is refused rather than
// presented: pointing an agent at a new UI needs a fresh enrolment token,
// and silently sending the old token would produce a confusing stream of
// 401s instead of naming the actual problem.
func ensureEnrolled(ctx context.Context, client *Client, store Store, cfg agentConfig) (Credentials, error) {
	creds, ok, err := store.LoadCredentials()
	if err != nil {
		return Credentials{}, err
	}
	if ok {
		if creds.UIBase != "" && creds.UIBase != client.Base {
			return Credentials{}, fmt.Errorf("stored credential was issued by %s but -ui is %s: enrol again with a fresh token from the new UI, or remove %s to start over",
				creds.UIBase, client.Base, store.credentialsPath())
		}
		return creds, nil
	}

	if cfg.EnrolToken == "" {
		return Credentials{}, fmt.Errorf("this agent has not enrolled yet and no -enrol-token was given: generate one for %q in the UI and pass it once", cfg.Hostname)
	}
	trace.Info("enrolling with the control-plane UI", "ui", client.Base, "hostname", cfg.Hostname)
	creds, err = client.Enrol(ctx, cfg.Hostname, cfg.EnrolToken, version.Version)
	if err != nil {
		return Credentials{}, fmt.Errorf("enrol with %s: %w", client.Base, err)
	}
	if err := store.SaveCredentials(creds); err != nil {
		// Refusing to continue is deliberate: an agent running on a
		// credential it failed to persist would work until the next restart
		// and then need an enrolment token nobody knows is required.
		return Credentials{}, fmt.Errorf("enrolment succeeded but the credential could not be saved: %w", err)
	}
	trace.Info("enrolled", "agent_id", creds.AgentID)
	return creds, nil
}

func reportOnce(ctx context.Context, client *Client, cfg agentConfig, cached CachedConfig) error {
	// nil operation ledger: -once is an install check, and re-reporting
	// stored operation results from a one-shot invocation would acknowledge
	// work the daemon has not been running to do.
	//
	// The FENCE ledger is passed, though, and the difference matters. That
	// one is not an acknowledgement protocol, it is state -- and a report
	// carries the whole picture of a host, replacing what the UI holds. A
	// -once run that omitted it would quietly erase the console's record of
	// which VMs this host has fenced, exactly when somebody is poking at a
	// machine mid-incident.
	fences := newFenceLedger(cfg.StateDir)
	if err := fences.Load(); err != nil {
		return fmt.Errorf("load the fence ledger: %w", err)
	}
	report, err := buildReport(cfg, cached, nil, nil, fences)
	if err != nil {
		return err
	}
	if err := client.SendReport(ctx, report); err != nil {
		return fmt.Errorf("send report: %w", err)
	}
	trace.Info("reported", "domains", len(report.Domains))
	return nil
}

// One snapshot per report, so a report cannot mix the hostname of one
// generation with the domains inventoried under another.
func reportLoop(ctx context.Context, client *Client, lv *live, state *sharedState, sched *Scheduler, ledger *operationLedger, fences *fenceLedger) {
	for {
		cfg := *lv.get()
		cached := state.get()
		report, err := buildReport(cfg, cached, sched, ledger, fences)
		if err != nil {
			// A libvirt failure is worth logging loudly but is not fatal:
			// libvirtd restarts, and an agent that exited would then need
			// systemd to bring it back rather than simply recovering.
			trace.Error("could not inventory the local host", "error", err)
		} else if err := client.SendReport(ctx, report); err != nil {
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, ErrRevoked) {
				trace.Error("the UI rejected this agent's credential; reporting will keep failing until it is enrolled again", "error", err)
			} else {
				trace.Warning("could not send report to the UI; will retry", "error", err)
				cfg.metrics.uiFailed()
			}
		} else {
			trace.Debug("reported", "domains", len(report.Domains))
			cfg.metrics.uiContacted(time.Now())
			cfg.metrics.setDomains(len(report.Domains), statusCounts(report.Domains))
		}

		interval := time.Duration(cached.Config.ReportIntervalSeconds) * time.Second
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

func pollLoop(ctx context.Context, client *Client, store Store, state *sharedState, m *agentMetrics) {
	// Backoff applies only to failures. A successful poll returns
	// immediately into the next one, which is what makes long-polling feel
	// instant to an operator.
	const minBackoff, maxBackoff = 5 * time.Second, 5 * time.Minute
	backoff := minBackoff

	for {
		if ctx.Err() != nil {
			return
		}
		cached := state.get()
		wait := time.Duration(cached.Config.PollWaitSeconds) * time.Second

		cfg, etag, err := client.PollConfig(ctx, cached.ETag, wait)
		switch {
		case ctx.Err() != nil:
			return
		case errors.Is(err, ErrUnchanged):
			// A 304 IS a successful exchange: the UI answered, it simply
			// had nothing new to say. Counting only changed configs would
			// make a healthy, stable estate look like an unreachable one.
			m.uiContacted(time.Now())
			// Nothing changed within the hold; refresh the confirmation
			// timestamp so the UI can still see this agent is current.
			cached.FetchedAtUnix = time.Now().Unix()
			state.set(cached)
			if err := store.SaveCache(cached); err != nil {
				trace.Warning("could not update the cached configuration timestamp", "error", err)
			}
			backoff = minBackoff
			continue
		case err != nil:
			if errors.Is(err, ErrRevoked) {
				trace.Error("the UI rejected this agent's credential", "error", err)
			} else {
				trace.Warning("could not poll the UI for configuration; continuing on the cached one", "error", err, "retry_in", backoff.String())
				m.uiFailed()
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff *= 2; backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}

		updated := CachedConfig{ETag: etag, FetchedAtUnix: time.Now().Unix(), Config: cfg}
		state.set(updated)
		if err := store.SaveCache(updated); err != nil {
			// Not fatal: the agent has the new configuration in memory and
			// keeps working. It just would not survive a restart, so this
			// has to be visible.
			trace.Warning("new configuration could not be cached to disk; it will be lost on restart", "error", err)
		}
		m.uiContacted(time.Now())
		trace.Info("configuration updated from the UI", "report_interval_s", cfg.ReportIntervalSeconds, "poll_wait_s", cfg.PollWaitSeconds, "cadences", len(cfg.CadenceSeconds))
		backoff = minBackoff
	}
}

// buildReport inventories the local host and assesses every domain.
func buildReport(cfg agentConfig, cached CachedConfig, sched *Scheduler, ledger *operationLedger, fences *fenceLedger) (Report, error) {
	mgr, err := libvirtsync.Connect(cfg.LibvirtURI)
	if err != nil {
		return Report{}, fmt.Errorf("connect to %s: %w", cfg.LibvirtURI, err)
	}
	defer mgr.Close()

	domains, err := inventory.Scan(mgr)
	if err != nil {
		return Report{}, err
	}

	now := time.Now()
	report := Report{
		ReportedAtUnix: now.Unix(),
		AgentVersion:   version.Version,
		Hostname:       cfg.Hostname,
		LibvirtURI:     cfg.LibvirtURI,
		Domains:        make([]ReportDomain, 0, len(domains)),
	}
	if cached.FetchedAtUnix > 0 {
		report.ConfigAgeSeconds = now.Unix() - cached.FetchedAtUnix
	} else {
		// Never confirmed with the UI at all -- distinct from "confirmed a
		// moment ago", which 0 would otherwise mean.
		report.ConfigAgeSeconds = -1
	}
	if sched != nil {
		report.Syncs = sched.Results()
	}
	if ledger != nil {
		// Every stored result, on every report -- not just newly finished
		// ones. The UI removing an operation from the config is what
		// acknowledges it, and the ledger drops the record once that
		// happens, so this list is self-limiting and a single lost report
		// costs nothing.
		report.OperationResults = ledger.Results()
	}

	// What this host has fenced, keyed by VM. Read once rather than per
	// domain: it is a lock and a map copy, and a report iterating hundreds
	// of domains should not pay for it hundreds of times.
	var fenced map[string]fenceRecord
	if fences != nil {
		fenced = fences.LatestByVM()
	}

	for _, d := range domains {
		// A domain with no configured cadence is not judged on freshness;
		// see inventory.Assess for why guessing one is worse than not.
		var cadence time.Duration
		if s, ok := cached.Config.CadenceSeconds[d.Name]; ok && s > 0 {
			cadence = time.Duration(s) * time.Second
		}
		a := inventory.Assess(d, now, cadence)
		report.Domains = append(report.Domains, ReportDomain{
			Name:                 d.Name,
			UUID:                 d.UUID,
			Active:               d.Active,
			Role:                 d.Role,
			LastCheckpoint:       d.LastCheckpoint,
			LastSyncUnix:         d.LastSyncUnix,
			FailureCount:         d.FailureCount,
			ReplicaSource:        d.ReplicaSource,
			ReplicaTargets:       d.ReplicaTargets,
			PromotedFrom:         d.PromotedFrom,
			PromotedAtUnix:       d.PromotedAtUnix,
			PromotedBy:           d.PromotedBy,
			PromotionMode:        d.PromotionMode,
			LastReplicatedAtUnix: d.LastReplicatedAtUnix,
			LastReplicatedTo:     d.LastReplicatedTo,
			FenceID:              d.FenceID,
			FenceSource:          d.FenceSource,
			FenceArmedAtUnix:     d.FenceArmedAtUnix,
			FenceArmedBy:         d.FenceArmedBy,
			Fenced:               reportFenced(fenced, d.Name),
			Disks:                reportDisks(d.Disks),
			RestoredFrom:         d.RestoredFrom,
			RestoredAtUnix:       d.RestoredAtUnix,
			RestoredBy:           d.RestoredBy,
			RestorePoints:        reportRestorePoints(d.RestorePoints),
			Status:               a.Status.String(),
			Reasons:              a.Reasons,
			AgeSeconds:           a.AgeSeconds,
		})
	}
	report.Filesystems = reportFilesystems(inventory.FilesystemsFor(domains))
	return report, nil
}

// reportDisks and reportFilesystems convert pkg/inventory's types to the
// wire ones. Written out rather than reusing inventory's structs directly
// for the same reason ReportDomain is: the wire format changes only when
// the protocol does, not when an internal struct is refactored.
// reportFenced converts this host's ledger entry for one VM, or nil when it
// has never fenced that domain -- which is the ordinary case for almost
// every domain on almost every host, and is why the field is a pointer.
func reportFenced(fenced map[string]fenceRecord, vm string) *ReportFenced {
	rec, ok := fenced[vm]
	if !ok {
		return nil
	}
	return &ReportFenced{
		FenceID: rec.FenceID,
		State:   rec.State,
		AtUnix:  rec.AtUnix,
		PeerRef: rec.PeerRef,
		ArmedBy: rec.ArmedBy,
		Error:   rec.Error,
	}
}

func reportDisks(in []inventory.DiskInfo) []ReportDisk {
	if len(in) == 0 {
		return nil
	}
	out := make([]ReportDisk, 0, len(in))
	for _, d := range in {
		out = append(out, ReportDisk{
			Path:           d.Path,
			ApparentBytes:  d.ApparentBytes,
			AllocatedBytes: d.AllocatedBytes,
			Missing:        d.Missing,
		})
	}
	return out
}

func reportRestorePoints(in []inventory.RestorePointInfo) []ReportRestorePoint {
	if len(in) == 0 {
		return nil
	}
	out := make([]ReportRestorePoint, 0, len(in))
	for _, r := range in {
		out = append(out, ReportRestorePoint{
			Tag:              r.Tag,
			TakenAtUnix:      r.TakenAtUnix,
			CheckpointAtUnix: r.CheckpointAtUnix,
			Checkpoint:       r.Checkpoint,
			Source:           r.Source,
			Verify:           r.Verify,
			Disks:            r.Disks,
			Incomplete:       r.Incomplete,
		})
	}
	return out
}

func reportFilesystems(in []inventory.Filesystem) []ReportFilesystem {
	if len(in) == 0 {
		return nil
	}
	out := make([]ReportFilesystem, 0, len(in))
	for _, f := range in {
		out = append(out, ReportFilesystem{
			Path:       f.Path,
			TotalBytes: f.TotalBytes,
			FreeBytes:  f.FreeBytes,
			UsedBytes:  f.UsedBytes,
		})
	}
	return out
}
