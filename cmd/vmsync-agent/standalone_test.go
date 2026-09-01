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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeStandalone(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "schedule.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestLoadStandaloneConfig(t *testing.T) {
	path := writeStandalone(t, `{
	  "config_version": 1,
	  "schedule": [
	    {"vm": "web01", "interval_seconds": 900, "enabled": true,
	     "target_host": "dr01",
	     "profile": {"compress": "zstd", "compress_level": "5",
	                 "netbuffer": "128k,1G", "io_depth": 16,
	                 "verify": "full", "target_disk_path": "/data/replicas"}}
	  ],
	  "max_concurrent_syncs": 3,
	  "target_replication_slots": {"dr01": 2}
	}`)

	cfg, err := loadStandaloneConfig(path)
	if err != nil {
		t.Fatalf("loadStandaloneConfig: %v", err)
	}
	if len(cfg.Schedule) != 1 {
		t.Fatalf("got %d entries, want 1", len(cfg.Schedule))
	}
	e := cfg.Schedule[0]
	if e.VM != "web01" || e.IntervalSeconds != 900 || !e.Enabled || e.TargetHost != "dr01" {
		t.Errorf("entry = %+v", e)
	}
	if e.Profile.Compress != "zstd" || e.Profile.CompressLevel != "5" || e.Profile.IODepth != 16 {
		t.Errorf("profile = %+v", e.Profile)
	}
	if cfg.MaxConcurrentSyncs != 3 || cfg.TargetReplicationSlots["dr01"] != 2 {
		t.Errorf("limits = %d / %v", cfg.MaxConcurrentSyncs, cfg.TargetReplicationSlots)
	}
	// Normalize fills the intervals the daemon paths need, so the scheduler
	// sees the same shape whether the config came from a file or the UI.
	if cfg.ReportIntervalSeconds <= 0 || cfg.PollWaitSeconds <= 0 {
		t.Errorf("Normalize did not run: %+v", cfg)
	}
}

// TestLoadStandaloneConfigRejectsUnknownFields is the whole reason this
// decode is strict. The file is written by a person, and a typo'd key that
// is silently ignored does not look like a mistake -- it looks like the
// scheduler not working, which is a much longer afternoon.
func TestLoadStandaloneConfigRejectsUnknownFields(t *testing.T) {
	path := writeStandalone(t, `{
	  "config_version": 1,
	  "schedule": [{"vm": "web01", "interval_secondss": 900, "enabled": true}]
	}`)
	_, err := loadStandaloneConfig(path)
	if err == nil {
		t.Fatal("a misspelled key was accepted; the entry would have run on a zero interval")
	}
	if !strings.Contains(err.Error(), "interval_secondss") {
		t.Errorf("error = %q, want it to name the offending key", err)
	}
}

func TestLoadStandaloneConfigErrors(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		if _, err := loadStandaloneConfig(filepath.Join(t.TempDir(), "nope.json")); err == nil {
			t.Fatal("a missing schedule file was accepted")
		}
	})
	t.Run("not json", func(t *testing.T) {
		if _, err := loadStandaloneConfig(writeStandalone(t, "schedule: web01")); err == nil {
			t.Fatal("a non-JSON file was accepted")
		}
	})
}

func TestValidateStandaloneConfig(t *testing.T) {
	ok := UIConfig{Schedule: []ScheduleEntry{
		{VM: "web01", IntervalSeconds: 900, Enabled: true},
	}}
	if err := validateStandaloneConfig(ok); err != nil {
		t.Fatalf("a minimal valid schedule was rejected: %v", err)
	}

	for _, tc := range []struct {
		name string
		cfg  UIConfig
		want string
	}{
		{
			// Nothing else tells the agent what to sync, so an empty file is
			// almost certainly a mistake rather than an intent.
			"no entries",
			UIConfig{},
			"no schedule entries",
		},
		{
			"no vm",
			UIConfig{Schedule: []ScheduleEntry{{IntervalSeconds: 900}}},
			"has no vm",
		},
		{
			// Both would fire, and the second would find the first still
			// running: an intermittent half-failure rather than an error.
			"duplicate vm",
			UIConfig{Schedule: []ScheduleEntry{
				{VM: "web01", IntervalSeconds: 900},
				{VM: "web01", IntervalSeconds: 1800},
			}},
			"more than once",
		},
		{
			"zero interval",
			UIConfig{Schedule: []ScheduleEntry{{VM: "web01"}}},
			"greater than 0",
		},
		{
			// The UI validates profiles before publishing them; a
			// hand-written file has nothing in front of it, so the same
			// check has to happen here.
			"invalid profile",
			UIConfig{Schedule: []ScheduleEntry{
				{VM: "web01", IntervalSeconds: 900,
					Profile: SyncProfile{Compress: "gzip"}},
			}},
			// The JSON field name, which is what SyncProfile.Validate uses
			// and what the operator is looking at in the file. Naming the
			// concept instead ("compression") would read well and match
			// nothing.
			`compress "gzip"`,
		},
		{
			"bad verify mode",
			UIConfig{Schedule: []ScheduleEntry{
				{VM: "web01", IntervalSeconds: 900,
					Profile: SyncProfile{Verify: "vigorously"}},
			}},
			`verify "vigorously"`,
		},
		{
			"negative replication slots",
			UIConfig{
				Schedule:               []ScheduleEntry{{VM: "web01", IntervalSeconds: 900}},
				TargetReplicationSlots: map[string]int{"dr01": -1},
			},
			"cannot be negative",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateStandaloneConfig(tc.cfg)
			if err == nil {
				t.Fatalf("accepted: %+v", tc.cfg)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestValidateStandaloneConfigNamesTheOffendingEntry: a file with a dozen
// VMs in it is the normal case, and "entry 7 (db03)" is the difference
// between a fix and a hunt.
func TestValidateStandaloneConfigNamesTheOffendingEntry(t *testing.T) {
	cfg := UIConfig{Schedule: []ScheduleEntry{
		{VM: "web01", IntervalSeconds: 900},
		{VM: "db03", IntervalSeconds: 0},
	}}
	err := validateStandaloneConfig(cfg)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "entry 2") || !strings.Contains(err.Error(), "db03") {
		t.Errorf("error = %q, want it to name both the position and the VM", err)
	}
}

// TestEffectiveMaxConcurrent pins the direction of the host-local ceiling.
// A control plane must be able to ask this host for LESS parallelism than
// it allows, and must never be able to ask for more: how much concurrent
// I/O a hypervisor absorbs is a property of that machine, and the UI cannot
// know it.
func TestEffectiveMaxConcurrent(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		fromConfig, hostLimit int
		want                  int
	}{
		{"neither set falls back to the default", 0, 0, defaultMaxConcurrent},
		{"config alone is honoured", 5, 0, 5},
		{"host ceiling lowers the config", 8, 3, 3},
		{"host ceiling never raises the config", 2, 9, 2},
		{"host ceiling lowers the default too", 0, 1, 1},
		{"hard clamp applies to the config", 5000, 0, hardMaxConcurrent},
		{"hard clamp applies before the host ceiling", 5000, 4, 4},
		{"a negative config is treated as unset", -3, 0, defaultMaxConcurrent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectiveMaxConcurrent(tc.fromConfig, tc.hostLimit); got != tc.want {
				t.Errorf("effectiveMaxConcurrent(%d, %d) = %d, want %d", tc.fromConfig, tc.hostLimit, got, tc.want)
			}
		})
	}
}
