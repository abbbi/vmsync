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

import "testing"

func tpl(name string, interval int) ScheduleTemplate {
	return ScheduleTemplate{
		Name:            name,
		IntervalSeconds: interval,
		Enabled:         true,
		Profile:         SyncProfile{Compress: "s2", CompressLevel: "better", IODepth: 8},
	}
}

func TestResolveEntryInheritsUnsetFields(t *testing.T) {
	def := tpl(DefaultTemplateName, 900)
	def.VerifyIntervalSeconds = 86400
	def.Profile.Verify = "fast"
	templates := map[string]ScheduleTemplate{DefaultTemplateName: def}

	t.Run("an empty entry takes the whole template", func(t *testing.T) {
		got := resolveEntry(ScheduleEntry{VM: "web01"}, templates)
		if got.IntervalSeconds != 900 {
			t.Errorf("IntervalSeconds = %d, want 900", got.IntervalSeconds)
		}
		if got.VerifyIntervalSeconds != 86400 {
			t.Errorf("VerifyIntervalSeconds = %d, want 86400", got.VerifyIntervalSeconds)
		}
		if got.Profile.Compress != "s2" || got.Profile.Verify != "fast" || got.Profile.IODepth != 8 {
			t.Errorf("profile not inherited: %+v", got.Profile)
		}
	})

	t.Run("an override wins field by field, not whole-profile", func(t *testing.T) {
		// The case templates exist for: the same cadence and transport as
		// everything else, but this one VM matters enough for -verify=full.
		// Whole-profile substitution would make that override cost a full
		// restatement, and a restatement is what drifts.
		e := ScheduleEntry{VM: "db01", Profile: SyncProfile{Verify: "full"}}
		got := resolveEntry(e, templates)
		if got.Profile.Verify != "full" {
			t.Errorf("Verify = %q, want the entry's own %q", got.Profile.Verify, "full")
		}
		if got.Profile.Compress != "s2" || got.Profile.IODepth != 8 {
			t.Errorf("overriding one field lost the rest: %+v", got.Profile)
		}
		if got.IntervalSeconds != 900 {
			t.Errorf("IntervalSeconds = %d, want the template's 900", got.IntervalSeconds)
		}
	})

	t.Run("a compression level is inherited only with its algorithm", func(t *testing.T) {
		// A level belongs to the algorithm it was written for: "3" against s2
		// and "better" against zstd are each refused outright. Inheriting one
		// without the other manufactures exactly that pairing.
		e := ScheduleEntry{VM: "app01", Profile: SyncProfile{Compress: "zstd"}}
		got := resolveEntry(e, templates)
		if got.Profile.CompressLevel != "" {
			t.Errorf("CompressLevel = %q; the template's s2 level must not attach to the entry's zstd",
				got.Profile.CompressLevel)
		}
		if err := got.Profile.Validate(); err != nil {
			t.Errorf("the resolved profile does not validate: %v", err)
		}
	})

	t.Run("Enabled is never inherited", func(t *testing.T) {
		// false cannot be told from unset in a bool, and the field's whole job
		// is keeping an entry visible while stopping it -- which is also the
		// opt-out from an auto-applied default.
		got := resolveEntry(ScheduleEntry{VM: "web01", Enabled: false}, templates)
		if got.Enabled {
			t.Error("Enabled became true from the template; an explicit entry could never opt out")
		}
	})

	t.Run("an unknown template leaves the entry alone", func(t *testing.T) {
		// Not defaulted and not dropped. Defaulting would run a VM on
		// settings nobody chose; dropping would stop replicating a VM that
		// has an entry. Returned unchanged it fails its own validation, and
		// launchDue skips it loudly, naming the VM.
		e := ScheduleEntry{VM: "web01", Template: "nonexistent"}
		got := resolveEntry(e, templates)
		if got.IntervalSeconds != 0 || got.Profile.Compress != "" {
			t.Errorf("entry was modified despite naming an unknown template: %+v", got)
		}
	})
}

func TestResolveScheduleAppliesTheDefaultOnlyWhereSafe(t *testing.T) {
	templates := map[string]ScheduleTemplate{DefaultTemplateName: tpl(DefaultTemplateName, 900)}
	syncable := []string{"web01", "db01", "mail01"}

	t.Run("VMs with no entry are synthesised from the default", func(t *testing.T) {
		entries := []ScheduleEntry{{VM: "db01", IntervalSeconds: 300, Enabled: true}}
		got := ResolveSchedule(entries, templates, syncable)
		if len(got) != 3 {
			t.Fatalf("got %d entries, want 3 (one explicit, two synthesised): %+v", len(got), got)
		}
		byVM := map[string]ScheduleEntry{}
		for _, e := range got {
			byVM[e.VM] = e
		}
		if byVM["db01"].IntervalSeconds != 300 {
			t.Errorf("the explicit entry's own interval was overwritten: %d", byVM["db01"].IntervalSeconds)
		}
		for _, vm := range []string{"web01", "mail01"} {
			if byVM[vm].IntervalSeconds != 900 || !byVM[vm].Enabled {
				t.Errorf("%s = %+v, want the default's cadence and Enabled", vm, byVM[vm])
			}
		}
	})

	t.Run("no default template means no synthesis at all", func(t *testing.T) {
		// The feature's off switch. Auto-applying a schedule to VMs nobody
		// wrote an entry for has to be asked for, and the ask is the default
		// template existing at all.
		entries := []ScheduleEntry{{VM: "db01", IntervalSeconds: 300, Enabled: true}}
		got := ResolveSchedule(entries, map[string]ScheduleTemplate{"nightly": tpl("nightly", 900)}, syncable)
		if len(got) != 1 || got[0].VM != "db01" {
			t.Errorf("got %+v, want only the explicit entry", got)
		}
	})

	t.Run("a VM that is not syncable is never synthesised", func(t *testing.T) {
		// syncable is filtered to domains recording replica_targets, which is
		// written by a SUCCESSFUL sync -- so the default reaches only pairs
		// somebody established by hand.
		got := ResolveSchedule(nil, templates, nil)
		if len(got) != 0 {
			t.Errorf("got %+v, want nothing: no VM had a target recorded", got)
		}
	})

	t.Run("an explicit disabled entry opts a VM out of the default", func(t *testing.T) {
		entries := []ScheduleEntry{{VM: "web01", Enabled: false}}
		got := ResolveSchedule(entries, templates, syncable)
		byVM := map[string]ScheduleEntry{}
		for _, e := range got {
			byVM[e.VM] = e
		}
		if byVM["web01"].Enabled {
			t.Error("a disabled entry was re-enabled by the default; there would be no way to exclude a VM")
		}
		if len(got) != 3 {
			t.Errorf("got %d entries, want 3 -- the opt-out must not also drop the other two", len(got))
		}
	})

	t.Run("synthesised entries come out in a stable order", func(t *testing.T) {
		a := ResolveSchedule(nil, templates, []string{"mail01", "web01", "db01"})
		b := ResolveSchedule(nil, templates, []string{"web01", "db01", "mail01"})
		if len(a) != len(b) {
			t.Fatalf("different lengths: %d vs %d", len(a), len(b))
		}
		for i := range a {
			if a[i].VM != b[i].VM {
				t.Fatalf("order depends on the inventory's order: %v vs %v", a, b)
			}
		}
	})
}

// The verify cadence inherits as ONE unit because its two forms are
// alternatives. Field-by-field inheritance would manufacture the exact
// combination Validate refuses -- an entry holding both an interval and a
// calendar, a contradiction neither author ever wrote.
func TestVerifyCadenceInheritsAsAUnit(t *testing.T) {
	byInterval := tpl(DefaultTemplateName, 900)
	byInterval.VerifyIntervalSeconds = 86400
	byInterval.Profile.Verify = "fast"

	byCalendar := calTpl(DefaultTemplateName, "Sun *-*-01..07", "02:00-12:00")

	t.Run("an empty entry takes the template's calendar", func(t *testing.T) {
		got := resolveEntry(ScheduleEntry{VM: "web01"},
			map[string]ScheduleTemplate{DefaultTemplateName: byCalendar})
		if got.VerifyDays != "Sun *-*-01..07" || got.VerifyWindow != "02:00-12:00" {
			t.Errorf("calendar = %q %q, want the template's", got.VerifyDays, got.VerifyWindow)
		}
	})

	t.Run("an entry's calendar does not inherit the template's interval", func(t *testing.T) {
		// The case that matters. The template says "every 86400 seconds"; this
		// VM says "the first Sunday". It must come out holding only the
		// calendar, or it would fail its own validation for holding both.
		got := resolveEntry(
			ScheduleEntry{VM: "db01", VerifyDays: "Sun", VerifyWindow: "22:00-04:00"},
			map[string]ScheduleTemplate{DefaultTemplateName: byInterval})
		if got.VerifyIntervalSeconds != 0 {
			t.Errorf("VerifyIntervalSeconds = %d, want 0: an entry stating a calendar states its whole cadence",
				got.VerifyIntervalSeconds)
		}
		if got.VerifyDays != "Sun" || got.VerifyWindow != "22:00-04:00" {
			t.Errorf("calendar = %q %q, want the entry's own", got.VerifyDays, got.VerifyWindow)
		}
		if err := validateVerifyCadence(got.VerifyDays, got.VerifyWindow,
			got.VerifyIntervalSeconds, got.Profile.Verify); err != nil {
			t.Errorf("the resolved entry does not validate: %v", err)
		}
	})

	t.Run("an entry's interval does not inherit the template's calendar", func(t *testing.T) {
		// The mirror. A VM that wants a plain interval under a calendar
		// template must not come out with both.
		got := resolveEntry(
			ScheduleEntry{VM: "db01", VerifyIntervalSeconds: 3600},
			map[string]ScheduleTemplate{DefaultTemplateName: byCalendar})
		if got.VerifyDays != "" || got.VerifyWindow != "" {
			t.Errorf("calendar = %q %q, want empty: an entry stating an interval states its whole cadence",
				got.VerifyDays, got.VerifyWindow)
		}
		if got.VerifyIntervalSeconds != 3600 {
			t.Errorf("VerifyIntervalSeconds = %d, want the entry's 3600", got.VerifyIntervalSeconds)
		}
		if err := validateVerifyCadence(got.VerifyDays, got.VerifyWindow,
			got.VerifyIntervalSeconds, got.Profile.Verify); err != nil {
			t.Errorf("the resolved entry does not validate: %v", err)
		}
	})

	t.Run("half a calendar is still a whole statement", func(t *testing.T) {
		// An entry giving only a window means "every day in these hours". It
		// must not have the template's days grafted onto it, which would
		// silently narrow a daily verify to a monthly one.
		got := resolveEntry(
			ScheduleEntry{VM: "db01", VerifyWindow: "02:00-04:00"},
			map[string]ScheduleTemplate{DefaultTemplateName: byCalendar})
		if got.VerifyDays != "" {
			t.Errorf("VerifyDays = %q, want empty: the entry said which hours, not which days", got.VerifyDays)
		}
		if got.VerifyWindow != "02:00-04:00" {
			t.Errorf("VerifyWindow = %q, want the entry's own", got.VerifyWindow)
		}
	})

	t.Run("the verify MODE still inherits, because it is a different question", func(t *testing.T) {
		// verify_days says how often; Profile.Verify says what. An entry
		// supplying the calendar under a template supplying the mode is the
		// combination templates exist to allow.
		got := resolveEntry(
			ScheduleEntry{VM: "db01", VerifyDays: "Sun"},
			map[string]ScheduleTemplate{DefaultTemplateName: byCalendar})
		if got.Profile.Verify != "fast" {
			t.Errorf("Verify = %q, want the template's %q", got.Profile.Verify, "fast")
		}
	})
}

// calTpl is a template whose verify cadence is a calendar rather than an
// interval.
func calTpl(name, days, window string) ScheduleTemplate {
	t := tpl(name, 900)
	t.Profile.Verify = "fast"
	t.VerifyDays, t.VerifyWindow = days, window
	return t
}

func noModeCalTpl() ScheduleTemplate {
	t := calTpl("x", "Sun", "02:00-12:00")
	t.Profile.Verify = ""
	return t
}

func defaultNoModeCalTpl() ScheduleTemplate {
	t := noModeCalTpl()
	t.Name = DefaultTemplateName
	return t
}

func bothFormsTpl() ScheduleTemplate {
	t := calTpl("x", "Sun", "02:00-12:00")
	t.VerifyIntervalSeconds = 86400
	return t
}

func TestScheduleTemplateValidate(t *testing.T) {
	withVerify := tpl("x", 900)
	withVerify.VerifyIntervalSeconds = 86400
	withVerify.Profile.Verify = "fast"

	badProfile := tpl("x", 900)
	badProfile.Profile.Compress = "lzo"

	for _, tc := range []struct {
		name string
		in   ScheduleTemplate
		ok   bool
	}{
		{"a plain template", tpl("default", 900), true},
		{"no name", ScheduleTemplate{IntervalSeconds: 900}, false},
		{"no cadence", ScheduleTemplate{Name: "x"}, false},
		{"negative verify cadence", ScheduleTemplate{Name: "x", IntervalSeconds: 900, VerifyIntervalSeconds: -1}, false},
		// A NON-default template may carry the cadence alone. That is the
		// estate-wide-policy shape templates are most worth having for: the
		// window is the estate's, the mode is the VM's. Requiring the template
		// to name a mode as well refused exactly that idiom.
		{"a non-default template may carry the cadence alone", ScheduleTemplate{Name: "x", IntervalSeconds: 900, VerifyIntervalSeconds: 86400}, true},
		// The default is the exception, because it SYNTHESISES entries for
		// VMs that have none and those have no other source for a mode.
		{"the default template may not", ScheduleTemplate{Name: DefaultTemplateName, IntervalSeconds: 900, VerifyIntervalSeconds: 86400}, false},
		// A negative interval passed every validator and then meant "verify
		// on every sync", because verifyDue reads interval <= 0 as "no
		// cadence" -- the most expensive possible reading of a typo.
		{"a negative verify interval", ScheduleTemplate{Name: "x", IntervalSeconds: 900, VerifyIntervalSeconds: -1}, false},
		{"verify cadence with a mode", withVerify, true},
		// A bad profile must be caught here, not discovered when a hundred
		// inheriting entries all fail at launch.
		{"an invalid profile", badProfile, false},
		// The calendar form of the same cadence.
		{"a verify calendar with a mode", calTpl("x", "Sun *-*-01..07", "02:00-12:00"), true},
		{"a window with no days", calTpl("x", "", "02:00-12:00"), true},
		{"days with no window", calTpl("x", "Sun", ""), true},
		{"a non-default template may carry the window alone", noModeCalTpl(), true},
		{"the default template carrying a window alone may not", defaultNoModeCalTpl(), false},
		// Refused rather than resolved by precedence: there is no reading of
		// "every 86400 seconds, and also the first Sunday" an operator could
		// predict, and whichever half lost would be the one they meant.
		{"both an interval and a calendar", bothFormsTpl(), false},
		// A half-understood expression fires on the wrong days, which is
		// worse than not starting.
		{"a malformed calendar", calTpl("x", "Frunday", "02:00-12:00"), false},
		{"real systemd this does not support", calTpl("x", "Sun *-*-01..07 02:00:00", ""), false},
		{"a malformed window", calTpl("x", "Sun", "25:00-12:00"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.in.Validate()
			if tc.ok && err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
			if !tc.ok && err == nil {
				t.Error("Validate() = nil, want an error")
			}
		})
	}
}
