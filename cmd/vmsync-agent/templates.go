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
	"fmt"
	"sort"
	"time"

	"vmsync/pkg/schedcal"
)

// syncableTTL is how long the syncable-VM list is reused before rescanning.
//
// The scheduler ticks every 10s and does no libvirt work at all until it
// launches something, so scanning per tick would add six round trips a minute
// to a loop whose whole virtue is being cheap when nothing is due. A minute
// is far finer than the thing it feeds: a VM becomes syncable when a sync
// records replica_targets on it, which is a once-ever event per pair, so
// noticing it up to a minute late costs nothing.
//
// The scan happens only when a default template exists (see
// ResolveSchedule's off switch), so an estate not using templates pays
// nothing at all.
const syncableTTL = time.Minute

// DefaultTemplateName is the template a schedule entry inherits from when it
// names none, and the one synthesised entries are built from.
//
// A reserved name rather than a flag, so "is there a default" is answered by
// the same lookup as any other template.
const DefaultTemplateName = "default"

// ScheduleTemplate is a named cadence and profile that entries inherit from.
//
// It exists for a safety reason before a convenience one. Without it, a newly
// discovered VM is not replicated until somebody creates its entry -- so the
// cost of forgetting is "not protected", silently. A default template inverts
// that to "protected with generic settings", which is the better failure mode.
// Saving typing across an estate is real, and secondary.
//
// It carries a concrete Profile rather than naming a preset, and that is
// forced rather than preferred: presets are a UI concept and the agent has no
// preset table (ScheduleEntry.Preset is `json:"-"` for exactly that reason).
// The UI fills this Profile from a preset at authoring time and keeps the
// preset name for its own "re-apply" offer. So presets resolve at edit time
// and templates resolve here, at run time -- see docs/design/scheduling.md.
type ScheduleTemplate struct {
	Name string `json:"name"`
	// IntervalSeconds is how often to sync. Required: a template with no
	// cadence has nothing to contribute, and inheriting 0 would mean "every
	// tick".
	IntervalSeconds int `json:"interval_seconds"`
	// VerifyIntervalSeconds is how often a sync should ALSO verify. 0 means
	// every sync, which is what a profile naming a verify mode has always
	// done. See ScheduleEntry.VerifyIntervalSeconds.
	VerifyIntervalSeconds int `json:"verify_interval_seconds,omitempty"`
	// VerifyDays and VerifyWindow are the calendar form of the same cadence --
	// "Sun *-*-01..07" plus "02:00-12:00". See
	// ScheduleEntry.VerifyDays, and pkg/schedcal for the grammar.
	//
	// This is the field templates were most worth building for. A verify
	// window is an estate-wide policy ("first Sunday of the month, overnight")
	// far more often than a per-VM one, and restating a calendar expression on
	// every entry is how the estate ends up with three subtly different ones.
	VerifyDays   string `json:"verify_days,omitempty"`
	VerifyWindow string `json:"verify_window,omitempty"`
	// Profile is the transport and integrity configuration.
	Profile SyncProfile `json:"profile"`
	// Enabled is what a SYNTHESISED entry gets. An explicit entry's own
	// Enabled always wins -- see resolveEntry.
	Enabled bool `json:"enabled"`
}

// Validate rejects a template that cannot produce a runnable entry.
//
// Checked here rather than trusted from the wire: a standalone agent reads
// these from a file a person edited, with no UI in front of it, and a
// template with a zero interval would resolve to an entry due on every tick.
func (t ScheduleTemplate) Validate() error {
	if t.Name == "" {
		return fmt.Errorf("a schedule template needs a name")
	}
	if t.IntervalSeconds <= 0 {
		return fmt.Errorf("schedule template %q has interval_seconds %d: a template must state a cadence, or every entry inheriting it would be due on every tick",
			t.Name, t.IntervalSeconds)
	}
	// Shape only. A template carrying just the estate-wide window, with each
	// entry naming its own verify mode, is legitimate -- see
	// validateVerifyCadenceShape. The mode requirement is enforced on the
	// resolved entry, where both halves are visible.
	if err := validateVerifyCadenceShape(t.VerifyDays, t.VerifyWindow, t.VerifyIntervalSeconds); err != nil {
		return fmt.Errorf("schedule template %q %w", t.Name, err)
	}
	// The one template that must be complete on its own. A default SYNTHESISES
	// entries for VMs that have none, and a synthesised entry has no other
	// source for a verify mode -- so a default with a cadence and no mode
	// would give every uncovered VM a schedule that can never verify, quietly.
	if t.Name == DefaultTemplateName {
		if err := validateVerifyCadence(t.VerifyDays, t.VerifyWindow, t.VerifyIntervalSeconds, t.Profile.Verify); err != nil {
			return fmt.Errorf("schedule template %q %w (it is the default, so it also has to be complete on its own: the entries it synthesises have nothing else to supply one)", t.Name, err)
		}
	}
	return t.Profile.Validate()
}

// validateVerifyCadence checks the two ways a verify cadence can be written,
// against each other and against the verify mode they modify.
//
// Shared by ScheduleTemplate.Validate and validateStandaloneConfig because the
// rules are the same wherever the cadence is written and a second copy is a
// second set of rules -- the UI publishing an entry a hand-written file would
// have refused is precisely the divergence templates make cheap to create.
//
// Both forms set at once is REFUSED rather than resolved by precedence. There
// is no reading of "every 86400 seconds, and also the first Sunday of the
// month" that an operator could predict, and whichever one silently lost would
// be the one they thought they were configuring.
func validateVerifyCadence(days, window string, intervalSec int, verifyMode string) error {
	if err := validateVerifyCadenceShape(days, window, intervalSec); err != nil {
		return err
	}
	if days == "" && window == "" && intervalSec == 0 {
		return nil
	}
	if verifyMode == "" {
		what := "verify_interval_seconds"
		if days != "" || window != "" {
			what = "a verify calendar"
		}
		return fmt.Errorf("sets %s but no verify mode is set on it or its template: the cadence says how often to verify, not whether to", what)
	}
	return nil
}

// validateVerifyCadenceShape checks a cadence as WRITTEN, without asking
// whether anything supplies a verify mode.
//
// Split from validateVerifyCadence because a TEMPLATE cannot answer that
// question and must not be judged on it. A template carrying only the
// estate-wide window -- "everything verifies on the first Sunday", with each
// entry naming its own mode -- is the shape templates were most worth
// building for, and requiring the template to name a mode as well refused
// exactly that. The mode requirement therefore lives on the RESOLVED entry,
// which is the only object that can see both halves.
//
// Both forms set at once is REFUSED rather than resolved by precedence. There
// is no reading of "every 86400 seconds, and also the first Sunday of the
// month" that an operator could predict, and whichever one silently lost
// would be the one they thought they were configuring.
func validateVerifyCadenceShape(days, window string, intervalSec int) error {
	hasCal := days != "" || window != ""
	// Checked here rather than only on the template. A NEGATIVE interval on an
	// entry used to pass every validator and then mean "verify on every single
	// sync", because verifyDue tests `interval <= 0` and treats that as "no
	// cadence" -- the opposite of what somebody typing -1 intends, and the
	// most expensive possible reading of it.
	if intervalSec < 0 {
		return fmt.Errorf("has verify_interval_seconds %d: negative is meaningless, and would be read as \"verify on every sync\"", intervalSec)
	}
	if hasCal && intervalSec > 0 {
		return fmt.Errorf("sets both verify_interval_seconds and a verify calendar (verify_days/verify_window): these are two ways to write one cadence, so set one or the other")
	}
	if hasCal {
		if _, err := schedcal.Parse(days, window); err != nil {
			return fmt.Errorf("has an invalid verify calendar: %w", err)
		}
	}
	return nil
}

// resolveEntry fills an entry's unset fields from its template.
//
// Resolution happens HERE, in the agent, and not in the UI. The agent is the
// only component present in every deployment -- a standalone agent reads a
// schedule file with no control plane at all (see AgentFile.ScheduleFile,
// "STANDALONE ONLY") -- so a UI-side resolver would make templates
// unavailable in exactly the deployment that benefits most from them. The
// consequence is that the UI must not PREDICT what this returns: the agent
// reports its effective schedule back and the console displays that.
//
// Zero and "" mean inherit, with one deliberate exception. Enabled is taken
// from the entry unmodified, because false cannot be told from unset in a
// bool and the field's whole purpose is to keep an entry visible while
// stopping it from running. That also supplies the opt-out from an
// auto-applied default: an explicit entry with Enabled false.
//
// An entry naming a template that does not exist is returned UNCHANGED rather
// than dropped or defaulted. It will then fail its own validation and be
// skipped loudly by launchDue, which names the VM -- whereas silently giving
// it the default's cadence would run a VM on settings nobody chose, and
// silently dropping it would stop replicating a VM that has an entry.
func resolveEntry(entry ScheduleEntry, templates map[string]ScheduleTemplate) ScheduleEntry {
	name := entry.Template
	if name == "" {
		name = DefaultTemplateName
	}
	t, ok := templates[name]
	if !ok {
		return entry
	}

	if entry.IntervalSeconds <= 0 {
		entry.IntervalSeconds = t.IntervalSeconds
	}
	// The verify cadence inherits as ONE unit, not field by field, because its
	// two forms are alternatives: an entry stating either form states its whole
	// verify cadence and inherits neither half of the other.
	//
	// Field-by-field would manufacture the exact combination the validator
	// refuses -- an entry saying "verify on the first Sunday" under a template
	// saying "verify every 86400 seconds" would come out holding both, a
	// contradiction neither the template's author nor the entry's ever wrote.
	if entry.VerifyDays == "" && entry.VerifyWindow == "" && entry.VerifyIntervalSeconds <= 0 {
		entry.VerifyIntervalSeconds = t.VerifyIntervalSeconds
		entry.VerifyDays = t.VerifyDays
		entry.VerifyWindow = t.VerifyWindow
	}
	entry.Profile = resolveProfile(entry.Profile, t.Profile)
	return entry
}

// resolveProfile fills a profile's unset fields from a template's.
//
// Field by field rather than "empty profile takes the template's whole
// profile", because the useful case is a VM that differs in ONE respect --
// the same cadence and transport as everything else, but -verify=full because
// it is the one that matters. Whole-profile substitution would make that
// override cost a full restatement, and a restatement is what drifts.
//
// NoChecksum is the exception and it is inverted on purpose: it is a
// NEGATIVE flag, so false means "the check is on" and is indistinguishable
// from unset. An entry therefore cannot re-enable a check its template
// disabled. That direction is the safe one -- the failure is a check running
// where the template said it need not, which costs a little I/O, rather than
// a check silently not running where an operator believes it does.
func resolveProfile(entry, tpl SyncProfile) SyncProfile {
	if entry.Compress == "" {
		entry.Compress = tpl.Compress
		// Carried together: a level belongs to the algorithm it was written
		// for, and "3" against s2 or "better" against zstd is refused
		// outright. Inheriting one without the other manufactures exactly
		// that pairing.
		if entry.CompressLevel == "" {
			entry.CompressLevel = tpl.CompressLevel
		}
	}
	if entry.NetBuffer == "" {
		entry.NetBuffer = tpl.NetBuffer
	}
	if !entry.UseSSH {
		entry.UseSSH = tpl.UseSSH
	}
	if entry.IODepth == 0 {
		entry.IODepth = tpl.IODepth
	}
	if !entry.NoChecksum {
		entry.NoChecksum = tpl.NoChecksum
	}
	if entry.Verify == "" {
		entry.Verify = tpl.Verify
	}
	if !entry.VerifyFailureReinit {
		entry.VerifyFailureReinit = tpl.VerifyFailureReinit
	}
	if entry.ReinitAfterFailures == 0 {
		entry.ReinitAfterFailures = tpl.ReinitAfterFailures
	}
	if entry.TargetDiskPath == "" {
		entry.TargetDiskPath = tpl.TargetDiskPath
	}
	if entry.TimestampToleranceSec == 0 {
		entry.TimestampToleranceSec = tpl.TimestampToleranceSec
	}
	if entry.Retention == "" {
		entry.Retention = tpl.Retention
	}
	if entry.SourcePortRange == "" {
		entry.SourcePortRange = tpl.SourcePortRange
	}
	if entry.TargetPortRange == "" {
		entry.TargetPortRange = tpl.TargetPortRange
	}
	return entry
}

// ResolveSchedule turns the published schedule plus the templates into the
// entries the scheduler should act on, including entries synthesised from the
// default template for VMs that have none.
//
// syncable is the VMs this host could sync -- from its own inventory, filtered
// to those whose domain records replica_targets. That filter is what makes
// auto-applying a default safe rather than reckless: replica_targets is
// written by a SUCCESSFUL sync, so the default reaches only pairs somebody
// established by hand at least once. A VM nobody ever synced is not
// synthesised, and would in any case be refused by buildSyncRequest with a
// message saying so.
//
// With no default template this returns the explicit entries unchanged, which
// That is the feature's off switch, and it is deliberately the ABSENCE of a
// default rather than a flag: auto-applying a schedule to VMs nobody wrote an
// entry for has to be something somebody asked for, and creating a template
// called "default" is that request, stated once and visible in the file.
func ResolveSchedule(entries []ScheduleEntry, templates map[string]ScheduleTemplate, syncable []string) []ScheduleEntry {
	out := make([]ScheduleEntry, 0, len(entries))
	have := make(map[string]bool, len(entries))
	for _, e := range entries {
		have[e.VM] = true
		out = append(out, resolveEntry(e, templates))
	}

	def, ok := templates[DefaultTemplateName]
	if !ok {
		return out
	}
	// Sorted so the synthesised entries are in a stable order run to run.
	// Nothing downstream depends on it, but a schedule that reorders itself
	// makes two reports gratuitously hard to diff.
	add := make([]string, 0, len(syncable))
	for _, vm := range syncable {
		if !have[vm] {
			add = append(add, vm)
		}
	}
	sort.Strings(add)
	for _, vm := range add {
		out = append(out, resolveEntry(ScheduleEntry{VM: vm, Enabled: def.Enabled}, templates))
	}
	return out
}

// templateNameOf is the template an entry inherits from, for an error message.
// Empty means the default, and saying "default" is more use to a person
// editing a file than saying nothing.
func templateNameOf(e ScheduleEntry) string {
	if e.Template == "" {
		return DefaultTemplateName
	}
	return e.Template
}
