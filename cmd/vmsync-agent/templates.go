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
	if t.VerifyIntervalSeconds < 0 {
		return fmt.Errorf("schedule template %q has a negative verify_interval_seconds", t.Name)
	}
	if t.VerifyIntervalSeconds > 0 && t.Profile.Verify == "" {
		return fmt.Errorf("schedule template %q sets verify_interval_seconds but no verify mode: the cadence says how often to verify, not whether to", t.Name)
	}
	return t.Profile.Validate()
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
	if entry.VerifyIntervalSeconds <= 0 {
		entry.VerifyIntervalSeconds = t.VerifyIntervalSeconds
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
// is the behaviour that predates templates. That is the feature's off switch,
// and it matters on upgrade: an estate with targets recorded and no entries
// must not begin syncing because it installed a new agent.
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
