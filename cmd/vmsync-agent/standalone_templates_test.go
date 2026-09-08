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

// A standalone schedule file is the ONLY way to use templates without a
// control plane, and the agent-side resolution decision in
// docs/design/scheduling.md is pointless if it does not work. These pin that.
func goodTpl(name string) ScheduleTemplate {
	return ScheduleTemplate{Name: name, IntervalSeconds: 900, Enabled: true}
}

func TestValidateStandaloneConfigTemplates(t *testing.T) {
	t.Run("templates and NO entries is legitimate with a default", func(t *testing.T) {
		// The case that was impossible before: the whole point of a default
		// template is that it covers VMs with no entry, so requiring at least
		// one entry would forbid the tidiest way to run an estate.
		cfg := UIConfig{Templates: map[string]ScheduleTemplate{
			DefaultTemplateName: goodTpl(DefaultTemplateName),
		}}
		if err := validateStandaloneConfig(cfg); err != nil {
			t.Errorf("refused a file with a default template and no entries: %v", err)
		}
	})

	t.Run("no entries and no default is still refused", func(t *testing.T) {
		// Without a default this file really is the only thing telling the
		// agent anything, and an empty one is a misconfiguration.
		cfg := UIConfig{Templates: map[string]ScheduleTemplate{"nightly": goodTpl("nightly")}}
		err := validateStandaloneConfig(cfg)
		if err == nil {
			t.Fatal("accepted a file with no entries and no default template")
		}
		if !strings.Contains(err.Error(), DefaultTemplateName) {
			t.Errorf("error %q does not name the template that would have made it valid", err)
		}
	})

	t.Run("a bad template is caught before any entry", func(t *testing.T) {
		// A bad template is a bad hundred entries. Discovering it one VM at a
		// time at run time would name the VM rather than the template they
		// all share.
		bad := goodTpl("nightly")
		bad.IntervalSeconds = 0
		cfg := UIConfig{
			Schedule:  []ScheduleEntry{{VM: "web01", Enabled: true, Template: "nightly"}},
			Templates: map[string]ScheduleTemplate{"nightly": bad},
		}
		err := validateStandaloneConfig(cfg)
		if err == nil {
			t.Fatal("accepted a template with no cadence")
		}
		if !strings.Contains(err.Error(), "nightly") {
			t.Errorf("error %q does not name the template", err)
		}
	})

	t.Run("the map key and the inner name must agree", func(t *testing.T) {
		// An entry can only refer to a template by the key, so a mismatch
		// means the name inside is decorative and misleading.
		cfg := UIConfig{
			Schedule:  []ScheduleEntry{{VM: "web01", Enabled: true}},
			Templates: map[string]ScheduleTemplate{"nightly": goodTpl("weekly")},
		}
		if err := validateStandaloneConfig(cfg); err == nil {
			t.Error("accepted a template whose key and inner name disagree")
		}
	})

	t.Run("an omitted inner name is filled from the key", func(t *testing.T) {
		// Requiring it twice is a chance for the two to disagree, so writing
		// it once is allowed.
		anon := goodTpl("")
		cfg := UIConfig{
			Schedule:  []ScheduleEntry{{VM: "web01", Enabled: true, Template: "nightly"}},
			Templates: map[string]ScheduleTemplate{"nightly": anon},
		}
		if err := validateStandaloneConfig(cfg); err != nil {
			t.Errorf("refused a template that omitted its own name: %v", err)
		}
	})

	t.Run("an entry naming an undefined template is refused by name", func(t *testing.T) {
		// resolveEntry returns such an entry unchanged so it fails its own
		// validation later, which is right at run time and the wrong error
		// for a person editing a file: they want the typo named.
		cfg := UIConfig{
			Schedule:  []ScheduleEntry{{VM: "web01", Enabled: true, Template: "nightlyy"}},
			Templates: map[string]ScheduleTemplate{"nightly": goodTpl("nightly")},
		}
		err := validateStandaloneConfig(cfg)
		if err == nil {
			t.Fatal("accepted an entry naming a template the file does not define")
		}
		for _, want := range []string{"web01", "nightlyy"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q", err, want)
			}
		}
	})

	t.Run("a verify cadence no resolved entry can supply a mode for is refused", func(t *testing.T) {
		// The entry must actually REACH the cadence rule. It names the
		// template (so it inherits a usable interval) and supplies no verify
		// mode, so the resolved entry carries a cadence for something nothing
		// does -- which is the rule under test.
		//
		// Written this way after the rule moved: judging the template alone
		// would refuse the legitimate estate-wide-window shape, so the check
		// lives on the resolved entry. An entry with no template reference
		// would be refused here for having no interval at all, and this test
		// would pass without ever exercising the cadence.
		vt := goodTpl("nightly")
		vt.VerifyIntervalSeconds = 86400
		cfg := UIConfig{
			Schedule:  []ScheduleEntry{{VM: "web01", Enabled: true, Template: "nightly"}},
			Templates: map[string]ScheduleTemplate{"nightly": vt},
		}
		err := validateStandaloneConfig(cfg)
		if err == nil {
			t.Fatal("accepted a verify cadence with no verify mode")
		}
		if !strings.Contains(err.Error(), "verify mode") {
			t.Errorf("error %q is not about the missing verify mode, so this test is passing for the wrong reason", err)
		}
	})

	t.Run("a file with entries and no templates still validates", func(t *testing.T) {
		// The behaviour that predates templates must be untouched.
		cfg := UIConfig{Schedule: []ScheduleEntry{{VM: "web01", IntervalSeconds: 900, Enabled: true}}}
		if err := validateStandaloneConfig(cfg); err != nil {
			t.Errorf("refused a plain pre-templates file: %v", err)
		}
	})
}

// The verify CALENDAR in a hand-written file. Every case here is one a person
// can type and no UI will catch, which is the whole reason the agent
// validates rather than trusts.
func TestValidateStandaloneConfigVerifyCalendar(t *testing.T) {
	// The expression the calendar form exists for: the first Sunday of the
	// month, 02:00 to 12:00.
	calTemplate := func() ScheduleTemplate {
		vt := goodTpl("nightly")
		vt.Profile.Verify = "fast"
		vt.VerifyDays, vt.VerifyWindow = "Sun *-*-01..07", "02:00-12:00"
		return vt
	}

	t.Run("a template calendar an entry inherits", func(t *testing.T) {
		cfg := UIConfig{
			Schedule:  []ScheduleEntry{{VM: "web01", Enabled: true, Template: "nightly"}},
			Templates: map[string]ScheduleTemplate{"nightly": calTemplate()},
		}
		if err := validateStandaloneConfig(cfg); err != nil {
			t.Errorf("refused a valid verify calendar: %v", err)
		}
	})

	t.Run("an entry supplying the calendar under a template supplying the mode", func(t *testing.T) {
		// The combination templates exist to allow: neither object is
		// complete on its own, which is why the check runs on the RESOLVED
		// entry rather than the raw one.
		vt := goodTpl("nightly")
		vt.Profile.Verify = "full"
		cfg := UIConfig{
			Schedule: []ScheduleEntry{{VM: "web01", Enabled: true, Template: "nightly",
				VerifyDays: "Sun", VerifyWindow: "22:00-04:00"}},
			Templates: map[string]ScheduleTemplate{"nightly": vt},
		}
		if err := validateStandaloneConfig(cfg); err != nil {
			t.Errorf("refused an entry whose calendar and mode come from different places: %v", err)
		}
	})

	t.Run("an entry calendar does not collide with the template's interval", func(t *testing.T) {
		// The resolution rule under test: the cadence inherits as one unit,
		// so this entry must come out holding only its calendar. Judged from
		// the outside -- if resolveEntry grafted the template's interval on,
		// validateVerifyCadence would refuse the very file templates are for.
		vt := goodTpl("nightly")
		vt.Profile.Verify = "fast"
		vt.VerifyIntervalSeconds = 86400
		cfg := UIConfig{
			Schedule: []ScheduleEntry{{VM: "web01", Enabled: true, Template: "nightly",
				VerifyDays: "Sun *-*-01..07", VerifyWindow: "02:00-12:00"}},
			Templates: map[string]ScheduleTemplate{"nightly": vt},
		}
		if err := validateStandaloneConfig(cfg); err != nil {
			t.Errorf("an entry's calendar collided with its template's interval: %v", err)
		}
	})

	t.Run("stating both forms on one entry is refused", func(t *testing.T) {
		cfg := UIConfig{Schedule: []ScheduleEntry{{
			VM: "web01", IntervalSeconds: 900, Enabled: true,
			Profile:               SyncProfile{Verify: "fast"},
			VerifyIntervalSeconds: 86400,
			VerifyDays:            "Sun", VerifyWindow: "02:00-12:00",
		}}}
		err := validateStandaloneConfig(cfg)
		if err == nil {
			t.Fatal("accepted an entry with both a verify interval and a verify calendar")
		}
		// Both spellings named, because the operator has to be told which one
		// to delete and neither is wrong on its own.
		for _, want := range []string{"verify_interval_seconds", "verify_days"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %q", err, want)
			}
		}
	})

	t.Run("a calendar with no verify mode is refused", func(t *testing.T) {
		// The same rule the interval form follows: this says how often, not
		// whether.
		cfg := UIConfig{Schedule: []ScheduleEntry{{
			VM: "web01", IntervalSeconds: 900, Enabled: true,
			VerifyDays: "Sun", VerifyWindow: "02:00-12:00",
		}}}
		if err := validateStandaloneConfig(cfg); err == nil {
			t.Error("accepted a verify calendar with no verify mode")
		}
	})

	t.Run("a malformed calendar is refused with the VM named", func(t *testing.T) {
		// A person editing a file wants the typo and the VM, not a run-time
		// surprise on the first Frunday of the month.
		cfg := UIConfig{Schedule: []ScheduleEntry{{
			VM: "web01", IntervalSeconds: 900, Enabled: true,
			Profile:    SyncProfile{Verify: "fast"},
			VerifyDays: "Frunday", VerifyWindow: "02:00-12:00",
		}}}
		err := validateStandaloneConfig(cfg)
		if err == nil {
			t.Fatal("accepted a day name that does not exist")
		}
		if !strings.Contains(err.Error(), "web01") {
			t.Errorf("error %q does not name the VM", err)
		}
	})

	t.Run("real systemd this does not support is refused rather than half-understood", func(t *testing.T) {
		// The failure mode this grammar's strictness exists for: silently
		// ignoring the time component of a full OnCalendar expression would
		// verify at the right frequency on the wrong hours, and look like it
		// worked.
		cfg := UIConfig{Schedule: []ScheduleEntry{{
			VM: "web01", IntervalSeconds: 900, Enabled: true,
			Profile:    SyncProfile{Verify: "fast"},
			VerifyDays: "Sun *-*-01..07 02:00:00",
		}}}
		if err := validateStandaloneConfig(cfg); err == nil {
			t.Error("accepted a full systemd OnCalendar expression, which this grammar only half understands")
		}
	})
}

// The estate-wide-policy shape: the template owns the WINDOW, each entry owns
// its MODE. This is what templates are most worth having for, and it was
// refused outright because ScheduleTemplate.Validate judged the calendar
// against the template's own Profile.Verify.
func TestTemplateMayCarryTheWindowAlone(t *testing.T) {
	windowOnly := goodTpl("nightly")
	windowOnly.VerifyDays, windowOnly.VerifyWindow = "Sun *-*-01..07", "02:00-12:00"

	t.Run("accepted when the entries name a mode", func(t *testing.T) {
		cfg := UIConfig{
			Schedule: []ScheduleEntry{
				{VM: "web01", Enabled: true, Template: "nightly", Profile: SyncProfile{Verify: "fast"}},
				{VM: "db01", Enabled: true, Template: "nightly", Profile: SyncProfile{Verify: "full"}},
			},
			Templates: map[string]ScheduleTemplate{"nightly": windowOnly},
		}
		if err := validateStandaloneConfig(cfg); err != nil {
			t.Fatalf("refused a template carrying only the estate-wide window: %v", err)
		}
		// And each VM keeps its own mode on the shared window.
		for _, tc := range []struct{ vm, mode string }{{"web01", "fast"}, {"db01", "full"}} {
			var got ScheduleEntry
			for _, e := range cfg.Schedule {
				if e.VM == tc.vm {
					got = resolveEntry(e, cfg.Templates)
				}
			}
			if got.Profile.Verify != tc.mode || got.VerifyWindow != "02:00-12:00" {
				t.Errorf("%s = mode %q window %q, want %q and the template's window",
					tc.vm, got.Profile.Verify, got.VerifyWindow, tc.mode)
			}
		}
	})

	t.Run("still refused when nothing supplies a mode", func(t *testing.T) {
		// The rule did not go away, it moved to the resolved entry -- the only
		// object that can see both halves.
		cfg := UIConfig{
			Schedule:  []ScheduleEntry{{VM: "web01", Enabled: true, Template: "nightly"}},
			Templates: map[string]ScheduleTemplate{"nightly": windowOnly},
		}
		if err := validateStandaloneConfig(cfg); err == nil {
			t.Error("accepted an entry whose window has no verify mode anywhere")
		}
	})

	t.Run("the DEFAULT template must be complete on its own", func(t *testing.T) {
		// It synthesises entries for VMs that have none, and those have no
		// other source for a mode -- so a default with a window and no mode
		// would give every uncovered VM a schedule that can never verify.
		def := windowOnly
		def.Name = DefaultTemplateName
		cfg := UIConfig{Templates: map[string]ScheduleTemplate{DefaultTemplateName: def}}
		err := validateStandaloneConfig(cfg)
		if err == nil {
			t.Fatal("accepted a default template with a window and no verify mode")
		}
		if !strings.Contains(err.Error(), "synthesis") && !strings.Contains(err.Error(), "synthesises") {
			t.Errorf("error %q does not explain why the default is special", err)
		}
	})
}

// A negative verify_interval_seconds on an ENTRY passed every validator and
// then meant "verify on every single sync", because verifyDue reads
// interval <= 0 as "no cadence". The sign was checked on templates only.
func TestNegativeVerifyIntervalOnAnEntryIsRefused(t *testing.T) {
	cfg := UIConfig{Schedule: []ScheduleEntry{{
		VM: "db01", IntervalSeconds: 900, Enabled: true,
		Profile:               SyncProfile{Verify: "fast"},
		VerifyIntervalSeconds: -1,
	}}}
	err := validateStandaloneConfig(cfg)
	if err == nil {
		t.Fatal("accepted verify_interval_seconds -1; it would verify on every sync")
	}
	if !strings.Contains(err.Error(), "db01") {
		t.Errorf("error %q does not name the VM", err)
	}
}
