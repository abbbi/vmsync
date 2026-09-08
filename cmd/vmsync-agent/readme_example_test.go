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
	"testing"
)

// readmeTemplatesExample is the "Schedule templates" block from
// cmd/vmsync-agent/README.md, copied verbatim.
//
// Pinned because a standalone schedule file is decoded with
// DisallowUnknownFields and validated before anything runs, so a documented
// example with a stale key does not degrade -- it refuses to start. The
// operator's first contact with templates would be an agent that will not
// come up, and they have no reason to suspect the documentation rather than
// their own typing.
const readmeTemplatesExample = `{
  "config_version": 1,
  "templates": {
    "default": {
      "name": "default",
      "interval_seconds": 900,
      "enabled": true,
      "verify_days": "Sun *-*-01..07",
      "verify_window": "02:00-12:00",
      "profile": {
        "compress": "zstd",
        "compress_level": "5",
        "verify": "full",
        "verify_failure_reinit": true,
        "retention": "24,3h"
      }
    },
    "hourly": {
      "name": "hourly",
      "interval_seconds": 3600,
      "enabled": true,
      "profile": { "compress": "s2", "verify": "fast" }
    }
  },
  "schedule": [
    { "vm": "db01", "template": "hourly", "enabled": true },
    { "vm": "web01", "enabled": true, "interval_seconds": 300 },
    { "vm": "lab01", "enabled": false }
  ]
}`

func TestREADMETemplatesExampleLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schedule.json")
	if err := os.WriteFile(path, []byte(readmeTemplatesExample), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadStandaloneConfig(path)
	if err != nil {
		t.Fatalf("the README's templates example does not load: %v", err)
	}

	t.Run("the entries resolve to what the prose claims", func(t *testing.T) {
		got := map[string]ScheduleEntry{}
		for _, e := range cfg.Schedule {
			got[e.VM] = resolveEntry(e, cfg.Templates)
		}

		// "db01 takes the hourly template."
		if e := got["db01"]; e.IntervalSeconds != 3600 || e.Profile.Verify != "fast" {
			t.Errorf("db01 = interval %d verify %q, want 3600 and \"fast\"",
				e.IntervalSeconds, e.Profile.Verify)
		}
		// "web01 keeps the default's compression, verify mode and calendar and
		// changes only its interval." The sentence is the assertion.
		e := got["web01"]
		if e.IntervalSeconds != 300 {
			t.Errorf("web01 IntervalSeconds = %d, want its own 300", e.IntervalSeconds)
		}
		if e.Profile.Compress != "zstd" || e.Profile.Verify != "full" {
			t.Errorf("web01 profile = %q/%q, want the default's zstd/full",
				e.Profile.Compress, e.Profile.Verify)
		}
		if e.VerifyDays != "Sun *-*-01..07" || e.VerifyWindow != "02:00-12:00" {
			t.Errorf("web01 calendar = %q %q, want the default's", e.VerifyDays, e.VerifyWindow)
		}
		// "To exclude a VM the default would otherwise cover, give it an entry
		// with enabled false."
		if got["lab01"].Enabled {
			t.Error("lab01 is enabled; the documented opt-out does not opt out")
		}
	})

	t.Run("the default template covers a VM with no entry", func(t *testing.T) {
		// The claim that makes a default worth having. syncable stands in for
		// the replica_targets scan, which needs libvirt.
		out := ResolveSchedule(cfg.Schedule, cfg.Templates, []string{"db01", "web01", "lab01", "mail01"})
		var mail *ScheduleEntry
		for i := range out {
			if out[i].VM == "mail01" {
				mail = &out[i]
			}
		}
		if mail == nil {
			t.Fatal("a syncable VM with no entry was not covered by the default template")
		}
		if mail.IntervalSeconds != 900 || !mail.Enabled {
			t.Errorf("mail01 = interval %d enabled %v, want 900 and true", mail.IntervalSeconds, mail.Enabled)
		}
		if mail.VerifyDays != "Sun *-*-01..07" {
			t.Errorf("mail01 VerifyDays = %q, want the default's", mail.VerifyDays)
		}
	})

	t.Run("nothing is synthesised without a default template", func(t *testing.T) {
		// The feature's off switch: no default template, no auto-apply, so a
		// VM is only covered because somebody asked for it to be.
		noDefault := map[string]ScheduleTemplate{"hourly": cfg.Templates["hourly"]}
		out := ResolveSchedule(cfg.Schedule, noDefault, []string{"db01", "mail01"})
		if len(out) != len(cfg.Schedule) {
			t.Errorf("synthesised %d entries with no default template, want none",
				len(out)-len(cfg.Schedule))
		}
	})
}

// The README's plain (pre-templates) example, kept working for the same
// reason: it is what an operator without templates copies.
const readmePlainExample = `{
  "config_version": 1,
  "schedule": [
    {
      "vm": "web01",
      "interval_seconds": 900,
      "enabled": true,
      "target_host": "dr01",
      "profile": {
        "compress": "zstd",
        "compress_level": "5",
        "netbuffer": "128k,1G",
        "io_depth": 16,
        "verify": "full",
        "target_disk_path": "/data/replicas",
        "retention": "24,3h"
      },
      "shutdown_timeout_sec": 900
    }
  ],
  "max_concurrent_syncs": 4,
  "shutdown_timeout_sec": 300,
  "target_replication_slots": { "dr01": 2 }
}`

func TestREADMEPlainExampleLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schedule.json")
	if err := os.WriteFile(path, []byte(readmePlainExample), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadStandaloneConfig(path)
	if err != nil {
		t.Fatalf("the README's plain example does not load: %v", err)
	}
	if len(cfg.Schedule) != 1 || cfg.Schedule[0].VM != "web01" {
		t.Fatalf("schedule = %+v, want one entry for web01", cfg.Schedule)
	}
}
