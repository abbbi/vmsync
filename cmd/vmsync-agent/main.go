/*
	Copyright (C) 2026  Michael Ablassmeier <abi@grinser.de>

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
// This is phase 1 of the control plane and it is READ-ONLY BY
// CONSTRUCTION: there is no code path here that starts a sync, changes a
// replication role, or touches a domain in any way. The configuration the
// UI hands out (see UIConfig) carries no executable instruction to ignore
// -- scheduling and operations arrive in later phases. That is what makes
// this safe to install on production hypervisors before the rest exists.
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
	"vmsync/pkg/version"
)

type agentConfig struct {
	UIBase      string
	EnrolToken  string
	CAFile      string
	StateDir    string
	LibvirtURI  string
	Hostname    string
	HTTPTimeout time.Duration
	Once        bool
	Debug       bool
	ShowVersion bool
}

func main() {
	var cfg agentConfig
	flag.StringVar(&cfg.UIBase, "ui", "", "Control-plane UI base address, https:// (required)")
	flag.StringVar(&cfg.EnrolToken, "enrol-token", "", "Single-use enrolment token generated in the UI for this host. Only needed until enrolment succeeds; it is spent by that call and can be removed afterwards")
	flag.StringVar(&cfg.CAFile, "ui-ca", "", "PEM bundle to verify the UI's certificate against, for a self-signed or private-CA UI. When unset the system trust store is used. There is deliberately no option to skip verification")
	flag.StringVar(&cfg.StateDir, "state-dir", "/var/lib/vmsync-agent", "Directory holding the agent's credential and its cached UI configuration")
	flag.StringVar(&cfg.LibvirtURI, "libvirt-uri", "qemu:///system", "Local libvirt URI to inventory. You should not need to change this: the agent reports the host it runs on")
	flag.StringVar(&cfg.Hostname, "hostname", "", "Name to report as. Defaults to the system hostname")
	flag.DurationVar(&cfg.HTTPTimeout, "http-timeout", 2*time.Minute, "Timeout for a single UI request. Must exceed the UI's long-poll hold time, or every config poll ends in a client-side timeout")
	flag.BoolVar(&cfg.Once, "once", false, "Report once and exit, instead of running as a daemon. For verifying a new install")
	flag.BoolVar(&cfg.Debug, "debug", false, "Enable debug logging")
	flag.BoolVar(&cfg.ShowVersion, "v", false, "Show version and exit")
	flag.BoolVar(&cfg.ShowVersion, "version", false, "Show version and exit")
	flag.Parse()

	if cfg.ShowVersion {
		fmt.Println(version.Version)
		os.Exit(0)
	}
	// Same reasoning as both other binaries: this takes no positional
	// arguments, so leftovers mean a flag was mistyped and silently dropped.
	if flag.NArg() > 0 {
		trace.Error("invalid command line", "error", fmt.Errorf("unexpected extra argument(s) %v", flag.Args()))
		os.Exit(2)
	}
	if cfg.UIBase == "" {
		trace.Error("invalid configuration", "error", errors.New("-ui is required"))
		os.Exit(2)
	}
	if cfg.Hostname == "" {
		h, err := os.Hostname()
		if err != nil {
			trace.Error("could not determine hostname; pass -hostname", "error", err)
			os.Exit(2)
		}
		cfg.Hostname = h
	}
	trace.SetDebug(cfg.Debug)

	if err := run(cfg); err != nil {
		trace.Error("agent stopped", "error", err)
		os.Exit(1)
	}
}

func run(cfg agentConfig) error {
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

	creds, err := ensureEnrolled(ctx, client, store, cfg)
	if err != nil {
		return err
	}
	client.Creds = creds
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

	// Two independent loops sharing the cached configuration. Reporting is
	// on a timer; polling blocks in a long poll and returns as soon as the
	// UI has something to say, so an operator's change lands in seconds
	// without the agent accepting inbound connections.
	state := &sharedState{cached: cached}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); reportLoop(ctx, client, cfg, state) }()
	go func() { defer wg.Done(); pollLoop(ctx, client, store, state) }()
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
	report, err := buildReport(cfg, cached)
	if err != nil {
		return err
	}
	if err := client.SendReport(ctx, report); err != nil {
		return fmt.Errorf("send report: %w", err)
	}
	trace.Info("reported", "domains", len(report.Domains))
	return nil
}

func reportLoop(ctx context.Context, client *Client, cfg agentConfig, state *sharedState) {
	for {
		cached := state.get()
		report, err := buildReport(cfg, cached)
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
			}
		} else {
			trace.Debug("reported", "domains", len(report.Domains))
		}

		interval := time.Duration(cached.Config.ReportIntervalSeconds) * time.Second
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

func pollLoop(ctx context.Context, client *Client, store Store, state *sharedState) {
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
		trace.Info("configuration updated from the UI", "report_interval_s", cfg.ReportIntervalSeconds, "poll_wait_s", cfg.PollWaitSeconds, "cadences", len(cfg.CadenceSeconds))
		backoff = minBackoff
	}
}

// buildReport inventories the local host and assesses every domain.
func buildReport(cfg agentConfig, cached CachedConfig) (Report, error) {
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

	for _, d := range domains {
		// A domain with no configured cadence is not judged on freshness;
		// see inventory.Assess for why guessing one is worse than not.
		var cadence time.Duration
		if s, ok := cached.Config.CadenceSeconds[d.Name]; ok && s > 0 {
			cadence = time.Duration(s) * time.Second
		}
		a := inventory.Assess(d, now, cadence)
		report.Domains = append(report.Domains, ReportDomain{
			Name:           d.Name,
			UUID:           d.UUID,
			Active:         d.Active,
			Role:           d.Role,
			LastCheckpoint: d.LastCheckpoint,
			LastSyncUnix:   d.LastSyncUnix,
			FailureCount:   d.FailureCount,
			ReplicaSource:  d.ReplicaSource,
			ReplicaTargets: d.ReplicaTargets,
			Status:         a.Status.String(),
			Reasons:        a.Reasons,
			AgeSeconds:     a.AgeSeconds,
		})
	}
	return report, nil
}
