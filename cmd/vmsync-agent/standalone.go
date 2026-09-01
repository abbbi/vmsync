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
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"vmsync/pkg/trace"
)

// runStandalone runs the scheduler from a file on disk, with no control
// plane at all: no enrolment, no credential, no reporting, no polling.
//
// The scheduler was always capable of this -- it reads a cached
// configuration and keeps running it while the UI is unreachable, which is
// the whole partition-tolerance design -- and the only thing standing in
// the way was a startup path that insisted on enrolling first. This is that
// path made optional.
//
// The result is a scheduler with parallelism limits, per-target replication
// slots, staggering, skip-if-still-running and per-VM outcome logging, for a
// host that will never have a control plane. An agent installed this way can
// be enrolled later without changing anything about how it runs syncs.
func runStandalone(lv *live, reloads *reloader) error {
	cfg := *lv.get()
	uiCfg, err := loadStandaloneConfig(cfg.StandaloneFile)
	if err != nil {
		return err
	}
	// validateStandaloneConfig already refused what it can refuse; this
	// reports what is merely clamped or ignored, which a hand-written file is
	// just as capable of containing.
	complainAbout(uiCfg, cfg.StandaloneFile)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		trace.Info("signal received, shutting down", "signal", sig.String())
		cancel()
	}()

	// A standalone agent reloads exactly like a control-plane one: the file
	// is the only way to configure either, so it must be the only way to
	// reconfigure either.
	hupCh := make(chan os.Signal, 1)
	signal.Notify(hupCh, syscall.SIGHUP)
	go reloads.Run(ctx, hupCh)

	enabled := 0
	for _, e := range uiCfg.Schedule {
		if e.Enabled {
			enabled++
		}
	}
	trace.Info("running standalone: no control plane, scheduling from file",
		"file", cfg.StandaloneFile, "entries", len(uiCfg.Schedule), "enabled", enabled,
		"vmsync", cfg.VmsyncPath,
		// The resolved number, not the requested one: a -max-concurrent-syncs
		// that silently overrode the file would otherwise be invisible here.
		"max_concurrent", effectiveMaxConcurrent(uiCfg.MaxConcurrentSyncs, cfg.MaxConcurrentSyncs))
	if enabled == 0 {
		// Not an error -- a file with everything disabled is a legitimate
		// way to park a host -- but silence here would look identical to a
		// schedule that is not being read at all, which is the thing an
		// operator would waste an afternoon on.
		trace.Warning("no schedule entry is enabled, so nothing will run", "file", cfg.StandaloneFile)
	}

	// FetchedAtUnix is deliberately left zero: nothing fetched this, and
	// stamping a time here would report a config age that means nothing.
	state := &sharedState{cached: CachedConfig{Config: uiCfg}}

	var wg sync.WaitGroup
	sched := NewScheduler(lv, state)
	if cfg.metrics != nil {
		wg.Add(1)
		// scanInventory true: there is no reportLoop here to do it.
		go func() { defer wg.Done(); metricsLoop(ctx, lv, state, sched, cfg.metrics, true) }()
	}
	wg.Add(1)
	// Synchronously, and BEFORE Run -- see the same call in main.go.
	sched.Reconcile(ctx)
	go func() { defer wg.Done(); sched.Run(ctx) }()

	// Split-brain protection runs here too. A standalone agent is the case
	// with NO control plane to notice a failover and issue anything, so it
	// is the one that most needs a host able to work out on its own that it
	// has been displaced. The token still comes from the peer's libvirt, so
	// nothing about the mechanism depends on a UI existing.
	fences := newFenceLedger(cfg.StateDir)
	if err := fences.Load(); err != nil {
		return fmt.Errorf("load the fence ledger: %w", err)
	}
	wg.Add(1)
	go func() { defer wg.Done(); fenceLoop(ctx, lv, state, fences) }()

	wg.Wait()
	return nil
}

// loadStandaloneConfig reads and validates a hand-written schedule.
//
// Strict about unknown fields on purpose. This file is written by a person,
// and the failure it protects against is a typo'd key being silently
// ignored -- which does not look like a mistake, it looks like the
// scheduler not working.
func loadStandaloneConfig(path string) (UIConfig, error) {
	// ScheduleDoc, which has no Operations field.
	//
	// This used to decode UIConfig, operations and all -- and runStandalone
	// starts no operations loop, so an "operations" block in a hand-written
	// file parsed cleanly, was accepted, and then vanished without a word. An
	// operator could put one there and watch nothing happen, indefinitely.
	// Now the strict decoder reports it as an unknown key, by name.
	doc, err := LoadScheduleFile(path)
	if err != nil {
		return UIConfig{}, err
	}
	cfg := doc.toUIConfig().Normalize()
	if err := validateStandaloneConfig(cfg); err != nil {
		return UIConfig{}, fmt.Errorf("standalone schedule %s: %w", path, err)
	}
	return cfg, nil
}

// validateStandaloneConfig rejects a schedule that would misbehave rather
// than discovering it one VM at a time at run time.
//
// The UI validates the same things before publishing an entry; a
// hand-written file has nothing in front of it, so this is where the
// equivalent check has to happen.
func validateStandaloneConfig(cfg UIConfig) error {
	if len(cfg.Schedule) == 0 {
		return fmt.Errorf("no schedule entries: this file is the only thing telling the agent what to sync")
	}
	seen := map[string]bool{}
	for i, e := range cfg.Schedule {
		where := fmt.Sprintf("entry %d", i+1)
		if e.VM != "" {
			where = fmt.Sprintf("entry %d (%s)", i+1, e.VM)
		}
		if strings.TrimSpace(e.VM) == "" {
			return fmt.Errorf("%s has no vm", where)
		}
		if seen[e.VM] {
			// Two entries for one VM would both fire, and the second would
			// find the first still running -- an intermittent, confusing
			// half-failure rather than a clean error.
			return fmt.Errorf("%s appears more than once; a VM can have only one entry", where)
		}
		seen[e.VM] = true

		if e.IntervalSeconds <= 0 {
			return fmt.Errorf("%s has interval_seconds %d; it must be greater than 0", where, e.IntervalSeconds)
		}
		if err := e.Profile.Validate(); err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
		// Refused rather than clamped. shutdownTimeoutFor clamps whatever a
		// UI sends, because a separately-versioned program's output is input
		// to survive -- but this file was typed by a person, and silently
		// turning their 30000 into 3600 is the failure this function exists
		// to prevent: it does not look like a mistake, it looks like the
		// setting not working.
		if err := validateShutdownTimeoutSec(e.ShutdownTimeoutSec); err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
	}
	if err := validateShutdownTimeoutSec(cfg.ShutdownTimeoutSec); err != nil {
		return err
	}
	if cfg.MaxConcurrentSyncs < 0 {
		return fmt.Errorf("max_concurrent_syncs cannot be negative")
	}
	for host, n := range cfg.TargetReplicationSlots {
		if n < 0 {
			return fmt.Errorf("target_replication_slots for %s cannot be negative", host)
		}
	}
	return nil
}
