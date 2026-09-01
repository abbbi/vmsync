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
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

// scheduleDocVersion is the config_version a schedule file must declare.
//
// Only the standalone file carries one in practice -- the control-plane copy
// is written by this agent, so it is always current by construction -- but
// both go through the same parser, and a version the parser did not expect
// must be an error rather than a silently ignored key.
const scheduleDocVersion = 1

// ScheduleDoc is the control-plane-owned half of the configuration, and the
// ONLY type any FILE decoder is ever instantiated with, in either mode.
//
// Note what is not here: Operations. That is not an omission to remember, it
// IS the invariant.
//
// An operation is a one-shot instruction to promote or fail over a production
// VM. It must arrive over the wire, in this process's lifetime, exactly once;
// replaying one off a disk means an agent that was killed mid-promotion, or
// merely restarted by a package upgrade, performing a failover from an
// instruction nobody re-issued and which may be hours stale.
//
// That used to be enforced by a line of code -- LoadCache set
// c.Config.Operations = nil on every load. Correct, well-reasoned, and a
// runtime guard on a type that permitted the very thing it guarded against,
// which every future decoder had to remember. A type with no such field
// cannot carry an operation off a disk at all, however the file got there.
// DisallowUnknownFields then turns an "operations" key in a hand-written file
// into a parse error NAMING it -- which standalone mode silently swallowed
// before, decoding them into a struct and then never starting an operations
// loop to run them.
type ScheduleDoc struct {
	ConfigVersion int `json:"config_version"`

	ReportIntervalSeconds int            `json:"report_interval_seconds,omitempty"`
	PollWaitSeconds       int            `json:"poll_wait_seconds,omitempty"`
	CadenceSeconds        map[string]int `json:"cadence_seconds,omitempty"`

	Schedule               []ScheduleEntry `json:"schedule,omitempty"`
	MaxConcurrentSyncs     int             `json:"max_concurrent_syncs,omitempty"`
	TargetReplicationSlots map[string]int  `json:"target_replication_slots,omitempty"`
	ShutdownTimeoutSec     int             `json:"shutdown_timeout_sec,omitempty"`
}

// ScheduleSource is the envelope the state dir keeps beside the document: how
// this copy was obtained, so the next poll can ask for changes only.
type ScheduleSource struct {
	ETag          string `json:"etag,omitempty"`
	FetchedAtUnix int64  `json:"fetched_at_unix,omitempty"`
}

// StoredSchedule is what the state dir holds.
//
// A VALUE, not a pointer. On first boot there is no file, LoadSchedule
// returns a zero StoredSchedule, and the first poll reads Source.ETag
// unconditionally -- a pointer here would be a nil dereference on every new
// host, on the one code path nobody tests twice.
type StoredSchedule struct {
	ScheduleDoc
	Source ScheduleSource `json:"source"`
}

// toUIConfig converts a document read from a FILE into the in-memory shape
// the loops use.
//
// Operations is left nil, and cannot be anything else: ScheduleDoc has no
// field to carry one. This is where "an operation never survives a restart"
// stopped being a line somebody has to remember.
func (d ScheduleDoc) toUIConfig() UIConfig {
	return UIConfig{
		ReportIntervalSeconds:  d.ReportIntervalSeconds,
		PollWaitSeconds:        d.PollWaitSeconds,
		CadenceSeconds:         d.CadenceSeconds,
		Schedule:               d.Schedule,
		MaxConcurrentSyncs:     d.MaxConcurrentSyncs,
		TargetReplicationSlots: d.TargetReplicationSlots,
		ShutdownTimeoutSec:     d.ShutdownTimeoutSec,
	}
}

// scheduleDocFrom converts an in-memory configuration into the document that
// gets written to disk.
//
// The one place operations are dropped on the way OUT, and again structurally:
// there is no field to copy them into, so no future edit here can start
// persisting them by accident.
func scheduleDocFrom(c UIConfig) ScheduleDoc {
	return ScheduleDoc{
		ConfigVersion:          scheduleDocVersion,
		ReportIntervalSeconds:  c.ReportIntervalSeconds,
		PollWaitSeconds:        c.PollWaitSeconds,
		CadenceSeconds:         c.CadenceSeconds,
		Schedule:               c.Schedule,
		MaxConcurrentSyncs:     c.MaxConcurrentSyncs,
		TargetReplicationSlots: c.TargetReplicationSlots,
		ShutdownTimeoutSec:     c.ShutdownTimeoutSec,
	}
}

// decodeScheduleDoc parses a schedule document from bytes.
//
// strict says whether an unknown key is an error. It is TRUE for a file a
// person wrote, where a misspelled key that silently keeps its default does
// not look like a mistake -- it looks like the scheduler not working. It is
// FALSE for the copy this agent wrote itself, because that file is only ever
// produced by a newer or equal version of this same binary, and refusing to
// read it after a downgrade would strand a host with no schedule during
// exactly the partition the cache exists for.
func decodeScheduleDoc(data []byte, strict bool, where string) (ScheduleDoc, error) {
	// The version pre-pass runs BEFORE the strict decode, for the same reason
	// the agent file's does: DisallowUnknownFields returns on the first
	// unknown field, so a future document would be reported as
	// `unknown field "..."` naming whatever key happens to be new, rather
	// than "this agent is too old for this file".
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return ScheduleDoc{}, fmt.Errorf("parse %s: %w", where, err)
	}
	if v, ok := raw["config_version"]; ok {
		var n int
		if err := json.Unmarshal(v, &n); err != nil {
			return ScheduleDoc{}, fmt.Errorf(`%s: "config_version" is not a number: %w`, where, err)
		}
		if n != scheduleDocVersion {
			return ScheduleDoc{}, fmt.Errorf(`%s: "config_version" is %d, but this vmsync-agent understands %d`, where, n, scheduleDocVersion)
		}
	} else if strict {
		return ScheduleDoc{}, fmt.Errorf(`%s: no "config_version". Add "config_version": %d`, where, scheduleDocVersion)
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	if strict {
		dec.DisallowUnknownFields()
	}
	var d ScheduleDoc
	if err := dec.Decode(&d); err != nil {
		return ScheduleDoc{}, fmt.Errorf("parse %s: %w", where, err)
	}
	return d, nil
}

// LoadScheduleFile reads a hand-written schedule, strictly.
func LoadScheduleFile(path string) (ScheduleDoc, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ScheduleDoc{}, fmt.Errorf("open standalone schedule %s: %w", path, err)
	}
	return decodeScheduleDoc(data, true, path)
}
