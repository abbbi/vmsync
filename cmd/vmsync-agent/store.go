/*
	Copyright (C) 2026  Michael Ablassmeier <abi@grinser.de>

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

// UIConfig is the configuration the UI hands out. Phase 1 carries no
// executable instruction at all -- the agent reports and does nothing else
// -- so this holds only what shapes reporting. Schedules and operations
// arrive in later phases.
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
	return writeJSONAtomic(s.credentialsPath(), c, 0o600)
}

// LoadCache returns the last-known-good UI configuration. A missing file
// yields the defaults with ok=false, so a caller can tell "never reached
// the UI" from "reached it and it said this".
func (s Store) LoadCache() (CachedConfig, bool, error) {
	var c CachedConfig
	ok, err := readJSON(s.cachePath(), &c)
	if !ok || err != nil {
		return CachedConfig{Config: DefaultUIConfig()}, ok, err
	}
	c.Config = c.Config.Normalize()
	return c, true, nil
}

// SaveCache records a configuration the UI confirmed. 0644 rather than
// 0600: it holds no secret, and being readable makes an incident easier to
// diagnose from a shell.
func (s Store) SaveCache(c CachedConfig) error {
	return writeJSONAtomic(s.cachePath(), c, 0o644)
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

// writeJSONAtomic writes to a temporary file in the same directory and
// renames it into place.
//
// The rename is the point: the config cache is what the agent falls back on
// when the UI is unreachable, so a crash or a full disk partway through a
// plain write would replace a working fallback with a truncated file at
// exactly the moment it is needed. rename(2) within a directory is atomic,
// so a reader sees either the old contents or the new ones.
func writeJSONAtomic(path string, value any, perm os.FileMode) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state dir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("create temp file for %s: %w", path, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename below succeeds

	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp file for %s: %w", path, err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	// Flush to disk before the rename. Without this the rename can be
	// durable while the contents are not, leaving a valid-looking but empty
	// file after a power loss -- the same failure the atomic write is here
	// to prevent.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file for %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("install %s: %w", path, err)
	}
	return nil
}
