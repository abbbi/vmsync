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
	"syscall"

	"vmsync/pkg/trace"
)

// dumpInventory scans libvirt fresh and logs one INFO block describing every
// domain found and what the agent makes of it.
//
// Per-cycle chatter lives elsewhere on purpose: per-device skip lines are
// debug-level (a cdrom on every domain would otherwise repeat on every scan)
// and per-tick scheduler decisions are debug too. This is the INFO-level
// record, emitted once at startup and again on every SIGUSR1 -- the two
// moments an operator is actually asking "what is on this host".
func dumpInventory(cfg agentConfig, cached CachedConfig, reason string) {
	fences := newFenceLedger(cfg.StateDir)
	if err := fences.Load(); err != nil {
		// A status dump must not fail because the audit trail is unreadable;
		// note the gap and show the inventory anyway.
		trace.Warning("inventory dump proceeding without fence attributions; the fence ledger could not be loaded", "error", err)
		fences = nil
	}
	// Verbose scan: the per-device skip lines (cdroms included) are part of
	// the asked-for record. Timer-driven reports scan quiet; see ScanVerbose.
	report, err := buildReport(cfg, cached, nil, nil, fences, true)
	if err != nil {
		trace.Error("inventory dump failed", "reason", reason, "error", err)
		return
	}
	trace.Info("inventory", "reason", reason, "host", cfg.Hostname, "domains", len(report.Domains))
	for _, d := range report.Domains {
		trace.Info("inventory entry", formatDomainSummary(d, cached.Config.CadenceSeconds[d.Name])...)
	}
}

// formatDomainSummary renders one assessed domain as trace key/value pairs.
//
// Split out from dumpInventory so the wording has a test that needs no
// libvirt: the interesting cases are all in how absent values read -- no
// role yet, no cadence configured, no replica metadata -- since those are
// exactly what a fresh or half-configured host looks like.
func formatDomainSummary(d ReportDomain, cadenceSeconds int) []any {
	disks := make([]string, 0, len(d.Disks))
	for _, dk := range d.Disks {
		disks = append(disks, dk.Path)
	}
	cadence := "none"
	if cadenceSeconds > 0 {
		cadence = fmt.Sprintf("%ds", cadenceSeconds)
	}
	return []any{
		"vm", d.Name,
		"active", d.Active,
		"role", d.Role,
		"status", d.Status,
		"cadence", cadence,
		"disks", "[" + strings.Join(disks, ",") + "]",
		"source", d.ReplicaSource,
		"targets", "[" + strings.Join(d.ReplicaTargets, ",") + "]",
		"reasons", strings.Join(d.Reasons, "; "),
	}
}

// watchStatusSignals logs a fresh inventory dump on every SIGUSR1 and exits
// with the context.
//
// Signal ONLY the main process (systemctl kill --kill-whom=main -s SIGUSR1,
// or kill -USR1 $MAINPID): the default KillMode signals the whole control
// group, and in-flight vmsync children do not handle SIGUSR1, so a group-wide
// delivery would terminate every running sync. SIGHUP keeps its own watcher
// in the reload path; this one never touches configuration.
func watchStatusSignals(ctx context.Context, lv *live, cached func() CachedConfig) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGUSR1)
	go func() {
		defer signal.Stop(ch)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ch:
				cfg := *lv.get()
				dumpInventory(cfg, cached(), "signal SIGUSR1")
			}
		}
	}()
}
