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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"vmsync/pkg/atomicjson"
)

// Credentials is what enrolment leaves behind: the agent's identity and the
// long-lived token it authenticates with afterward.
//
// Deliberately separate from the enrolment token the operator pastes into
// the config. That one is single-use and short-lived; this is the real
// credential, and keeping them in different files means the config can be
// managed by configuration management while this stays machine-owned.
type Credentials struct {
	AgentID string `json:"agent_id"`
	Token   string `json:"token"`
	// UIBase records which UI issued this credential, so pointing the agent
	// at a different UI is detected as needing re-enrolment rather than
	// silently presenting a token the new UI has never heard of.
	UIBase string `json:"ui_base"`
}

// CachedConfig is the last configuration successfully fetched from the UI.
//
// This file is what keeps replication running when the UI is unreachable,
// which is the control plane's central invariant: the UI lives at the DR
// site, a WAN partition is an ordinary event, and an agent that stops
// working without it would make the control plane a single point of failure
// for the thing it exists to protect.
type CachedConfig struct {
	// ETag is echoed back on the next poll so the UI can answer 304 rather
	// than resend an unchanged configuration.
	ETag string `json:"etag,omitempty"`
	// FetchedAtUnix records when this was last confirmed with the UI, so the
	// agent can report how stale its own instructions are.
	FetchedAtUnix int64    `json:"fetched_at_unix"`
	Config        UIConfig `json:"config"`
}

// ScheduleEntry is one VM the UI wants this agent to sync, and how.
//
// Note what is NOT here: no flag names, no command line, no credentials.
// The UI describes intent; the agent decides how that is spelled (see
// SyncProfile.CommandArgs) and supplies the credentials from its own local
// configuration.
type ScheduleEntry struct {
	VM              string `json:"vm"`
	IntervalSeconds int    `json:"interval_seconds"`
	// Enabled false keeps an entry visible in the UI while stopping it from
	// running -- distinct from deleting it, and distinct again from
	// -update-role=paused, which is enforced by vmsync itself and survives
	// the UI being unreachable.
	Enabled bool        `json:"enabled"`
	Profile SyncProfile `json:"profile"`
	// TargetHost pins which target to sync to when a source fans out to
	// more than one. Empty means "the only one", and an entry with an empty
	// TargetHost against a multi-target source is refused rather than
	// guessed at.
	TargetHost string `json:"target_host,omitempty"`
	// ShutdownTimeoutSec overrides UIConfig.ShutdownTimeoutSec for this VM,
	// 0 to inherit it. How long a guest takes to stop cleanly is a property
	// of what it runs, not of the estate.
	ShutdownTimeoutSec int `json:"shutdown_timeout_sec,omitempty"`
}

// UIConfig is the configuration the UI hands out.
//
// Phases 1 and 2 carried no executable instruction at all. Phase 3 adds
// Schedule, which is the first thing here the agent acts on -- and the
// reason SyncProfile validates every field before a command is built.
type UIConfig struct {
	// ReportIntervalSeconds is how often to send an inventory report.
	ReportIntervalSeconds int `json:"report_interval_seconds"`
	// PollWaitSeconds is how long the UI may hold a config poll open before
	// answering, so an agent behind a firewall gets near-immediate delivery
	// without accepting inbound connections.
	PollWaitSeconds int `json:"poll_wait_seconds"`
	// CadenceSeconds maps a domain name to how often it is expected to sync,
	// used only to judge staleness. A domain absent from this map has an
	// unknown cadence and is not judged on freshness at all.
	CadenceSeconds map[string]int `json:"cadence_seconds,omitempty"`

	// Schedule is what this agent should sync, and how often.
	Schedule []ScheduleEntry `json:"schedule,omitempty"`
	// MaxConcurrentSyncs caps how many run at once on this host. Zero means
	// the agent's own default; the agent also clamps this, since a UI is a
	// separately-versioned program whose answers are input to validate.
	MaxConcurrentSyncs int `json:"max_concurrent_syncs,omitempty"`
	// TargetReplicationSlots caps concurrent syncs INTO a given target host. No
	// single agent can see that four others are writing to the same target,
	// so this is the one limit only the UI can compute.
	TargetReplicationSlots map[string]int `json:"target_replication_slots,omitempty"`
	// ShutdownTimeoutSec is the estate default for a clean guest shutdown.
	//
	// Needed here as well as on each operation because the agent shuts
	// domains down on its OWN initiative: a fence has no operation behind it,
	// and during the partition that usually causes one there is no control
	// plane to ask. Zero leaves vmsync its own default.
	ShutdownTimeoutSec int `json:"shutdown_timeout_sec,omitempty"`

	// Operations are one-shot instructions -- a promotion, an inversion --
	// as opposed to the standing desired state above.
	//
	// They travel in the same document but are emphatically NOT the same
	// kind of thing, and the agent treats them differently at every step:
	// the schedule is replayed happily from the on-disk cache during a
	// partition, while an operation is executed only from a config received
	// over the wire in this process lifetime, exactly once ever, against a
	// durable ledger. See operations.go.
	Operations []Operation `json:"operations,omitempty"`
}

// DefaultUIConfig is what the agent runs with before it has ever reached
// the UI: report every minute, hold polls for 30 seconds, judge nothing on
// freshness. Chosen so a freshly-installed agent that cannot reach its UI
// still produces useful local logs instead of sitting idle.
func DefaultUIConfig() UIConfig {
	return UIConfig{
		ReportIntervalSeconds: 60,
		PollWaitSeconds:       30,
	}
}

// Normalize replaces nonsensical values with the defaults. The UI is a
// separate program that may be a different version, so its answers are
// treated as input to validate rather than as trusted internal state -- a
// zero or negative interval here would otherwise turn the poll loop into a
// busy loop against the UI.
func (c UIConfig) Normalize() UIConfig {
	d := DefaultUIConfig()
	if c.ReportIntervalSeconds <= 0 {
		c.ReportIntervalSeconds = d.ReportIntervalSeconds
	}
	if c.PollWaitSeconds <= 0 {
		c.PollWaitSeconds = d.PollWaitSeconds
	}
	// Capped as well as floored. Nothing bounded this before, and a wait
	// longer than the agent's own HTTP timeout makes every poll end in a
	// client-side timeout -- after which no configuration change ever
	// arrives again, silently.
	if c.PollWaitSeconds > maxPollWaitSeconds {
		c.PollWaitSeconds = maxPollWaitSeconds
	}
	if c.ReportIntervalSeconds > maxReportIntervalSeconds {
		c.ReportIntervalSeconds = maxReportIntervalSeconds
	}
	return c
}

// Store persists the agent's own state under a single directory.
type Store struct{ Dir string }

func (s Store) credentialsPath() string { return filepath.Join(s.Dir, "credentials.json") }
func (s Store) cachePath() string       { return filepath.Join(s.Dir, "config-cache.json") }

// LoadCredentials returns the stored credentials, or ok=false when the
// agent has not enrolled yet. A missing file is the normal first-boot state
// and is not an error.
func (s Store) LoadCredentials() (Credentials, bool, error) {
	var c Credentials
	ok, err := readJSON(s.credentialsPath(), &c)
	return c, ok, err
}

// SaveCredentials writes the credentials 0600. Losing this file means
// re-enrolling, which needs a fresh token from the UI, so it is written
// atomically like everything else here.
func (s Store) SaveCredentials(c Credentials) error {
	return atomicjson.Write(s.credentialsPath(), c, 0o600)
}

// LoadCache returns the last-known-good UI configuration. A missing file
// yields the defaults with ok=false, so a caller can tell "never reached
// the UI" from "reached it and it said this".
func (s Store) LoadCache() (CachedConfig, bool, error) {
	data, err := os.ReadFile(s.cachePath())
	if os.IsNotExist(err) {
		return CachedConfig{Config: DefaultUIConfig()}, false, nil
	}
	if err != nil {
		return CachedConfig{Config: DefaultUIConfig()}, false, fmt.Errorf("read %s: %w", s.cachePath(), err)
	}
	// Decoded as a ScheduleDoc, which has NO Operations field.
	//
	// This used to decode CachedConfig -- which carries them -- and then set
	// c.Config.Operations = nil immediately afterwards. That line was correct
	// and well-reasoned, and it was a runtime guard on a type that permitted
	// exactly what it guarded against, which every future decoder of that
	// type had to remember. Now the type cannot express an operation at all,
	// so no edit here can start replaying failovers off a disk.
	//
	// Lenient about unknown keys, unlike the standalone file: this copy is
	// written by this same binary, so an unknown key means a downgrade, and
	// refusing to read it would strand the host with no schedule during
	// precisely the partition the cache exists for.
	sd, err := decodeScheduleDoc(data, false, s.cachePath())
	if err != nil {
		return CachedConfig{Config: DefaultUIConfig()}, false, err
	}
	var env struct {
		Source ScheduleSource `json:"source"`
	}
	// Best-effort: an envelope this cannot read costs one unconditional poll,
	// not a startup failure.
	_ = json.Unmarshal(data, &env)

	return CachedConfig{
		ETag:          env.Source.ETag,
		FetchedAtUnix: env.Source.FetchedAtUnix,
		Config:        sd.toUIConfig().Normalize(),
	}, true, nil
}

// SaveCache records a configuration the UI confirmed. 0644 rather than
// 0600: it holds no secret, and being readable makes an incident easier to
// diagnose from a shell.
func (s Store) SaveCache(c CachedConfig) error {
	// Written as a StoredSchedule, so operations are dropped on the way OUT
	// by construction too: there is no field to copy them into. Previously
	// the whole CachedConfig went to disk, operations and all, and only the
	// READ side removed them -- which meant a live failover instruction sat
	// in a 0644 file on the host for as long as the UI kept publishing it.
	return atomicjson.Write(s.cachePath(), StoredSchedule{
		ScheduleDoc: scheduleDocFrom(c.Config),
		Source:      ScheduleSource{ETag: c.ETag, FetchedAtUnix: c.FetchedAtUnix},
	}, 0o644)
}

func readJSON(path string, into any) (bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, into); err != nil {
		return false, fmt.Errorf("parse %s: %w", path, err)
	}
	return true, nil
}
