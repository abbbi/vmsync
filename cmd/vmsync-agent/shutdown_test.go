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
	"strings"
	"testing"
)

// The agent's half of the resolution, which must match the UI's.
//
// Both paths shut a domain down in exactly the same way, so a VM that stops
// fine when an operator asks but fails when a fence does would be a
// genuinely baffling thing to debug. The UI's TestResolveShutdownTimeout is
// the matching half.
func TestShutdownTimeoutFor(t *testing.T) {
	cached := UIConfig{
		ShutdownTimeoutSec: 600,
		Schedule: []ScheduleEntry{
			{VM: "db01", ShutdownTimeoutSec: 1200},
			{VM: "web01"}, // no override
		},
	}

	for _, tc := range []struct {
		name, vm string
		want     int
	}{
		{"a VM with its own value", "db01", 1200},
		{"a VM with an entry but no override falls back to the estate", "web01", 600},
		{"a VM with no entry at all falls back to the estate", "nowhere01", 600},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := shutdownTimeoutFor(cached, tc.vm); got != tc.want {
				t.Errorf("shutdownTimeoutFor(%q) = %d, want %d", tc.vm, got, tc.want)
			}
		})
	}

	t.Run("no config at all still yields vmsync's default", func(t *testing.T) {
		if got := shutdownTimeoutFor(UIConfig{}, "web01"); got != defaultShutdownTimeoutSec {
			t.Errorf("got %d, want %d -- an empty config is what a fence sees before the first poll",
				got, defaultShutdownTimeoutSec)
		}
	})
}

// The config arrives from a separately-versioned program over the network,
// and this number decides how long a production VM is given before its
// shutdown is called a failure. A nonsense value must not be honoured.
func TestShutdownTimeoutIsClamped(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  UIConfig
		want int
	}{
		{
			"an absurdly short estate default would declare failure before the guest began",
			UIConfig{ShutdownTimeoutSec: 1}, minShutdownTimeoutSec,
		},
		{
			"an absurdly long one would hang the sweep behind a single domain",
			UIConfig{ShutdownTimeoutSec: 999999}, maxShutdownTimeoutSec,
		},
		{
			"a per-VM override is clamped too, not just the estate default",
			UIConfig{Schedule: []ScheduleEntry{{VM: "web01", ShutdownTimeoutSec: 999999}}},
			maxShutdownTimeoutSec,
		},
		{
			"a negative estate default is treated as unset rather than clamped up from below",
			UIConfig{ShutdownTimeoutSec: -30}, defaultShutdownTimeoutSec,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := shutdownTimeoutFor(tc.cfg, "web01"); got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

// The operation path passes what the UI resolved, rather than resolving
// again -- see Operation.ShutdownTimeoutSec.
func TestShutdownOperationPassesItsTimeout(t *testing.T) {
	cfg := agentConfig{LibvirtURI: "qemu:///system"}

	args, err := operationArgs(cfg, Operation{
		ID: "op-1", Kind: OpShutdown, VM: "web01", ShutdownTimeoutSec: 900,
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-shutdown-domain", "-target-uri qemu:///system", "-target-domain web01",
		"-shutdown-timeout-sec 900",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv %q is missing %q", joined, want)
		}
	}

	// An older UI sends no timeout. vmsync's own default is then the right
	// answer, and passing an explicit 0 would override it with nonsense.
	args, err = operationArgs(cfg, Operation{ID: "op-2", Kind: OpShutdown, VM: "web01"})
	if err != nil {
		t.Fatal(err)
	}
	if joined := strings.Join(args, " "); strings.Contains(joined, "-shutdown-timeout-sec") {
		t.Errorf("argv %q passes a timeout the UI never set; vmsync's own default should stand", joined)
	}
}

// The outer bound on vmsync must exceed the guest's own, or a fence kills
// the process mid-wait: a `running` ledger entry, a domain in an unknown
// state, and a latched fence nothing will ever retry.
func TestTheFenceProcessBoundOutlastsTheGuestTimeout(t *testing.T) {
	for _, sec := range []int{minShutdownTimeoutSec, 300, 900, maxShutdownTimeoutSec} {
		bound := shutdownProcessBound(sec)
		if bound <= 0 {
			t.Fatalf("bound for %ds is not positive", sec)
		}
		if bound.Seconds() <= float64(sec) {
			t.Errorf("a guest timeout of %ds gets a process bound of %v, which would cut vmsync off "+
				"just as the guest ran out", sec, bound)
		}
	}
}

// A hand-written standalone file gets the same scrutiny the UI applies
// before publishing an entry -- there is nothing else in front of it.
func TestStandaloneRefusesAnOutOfRangeShutdownTimeout(t *testing.T) {
	base := func() UIConfig {
		return UIConfig{
			ReportIntervalSeconds: 60, PollWaitSeconds: 30,
			Schedule: []ScheduleEntry{{VM: "web01", IntervalSeconds: 900, Enabled: true}},
		}
	}

	t.Run("a sane per-VM value is accepted", func(t *testing.T) {
		cfg := base()
		cfg.Schedule[0].ShutdownTimeoutSec = 900
		if err := validateStandaloneConfig(cfg); err != nil {
			t.Errorf("validateStandaloneConfig() = %v, want accepted", err)
		}
	})

	t.Run("zero means inherit and must not be an error", func(t *testing.T) {
		if err := validateStandaloneConfig(base()); err != nil {
			t.Errorf("validateStandaloneConfig() = %v, want accepted", err)
		}
	})

	t.Run("a per-VM value out of range is named, not clamped", func(t *testing.T) {
		cfg := base()
		cfg.Schedule[0].ShutdownTimeoutSec = 30000
		err := validateStandaloneConfig(cfg)
		if err == nil {
			t.Fatal("an out-of-range value was accepted; it would be silently clamped instead")
		}
		if !strings.Contains(err.Error(), "web01") {
			t.Errorf("the error should name the offending entry, got: %v", err)
		}
	})

	t.Run("the file-level default is checked too", func(t *testing.T) {
		cfg := base()
		cfg.ShutdownTimeoutSec = 2
		if err := validateStandaloneConfig(cfg); err == nil {
			t.Error("an out-of-range file default was accepted")
		}
	})
}
