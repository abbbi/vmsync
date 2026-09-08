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
	"reflect"
	"strings"
	"testing"
)

// THE invariant: no type a file decoder is instantiated with may be able to
// carry an operation.
//
// Asserted against the TYPE rather than against behaviour, because behaviour
// can be restored by a later edit while the type cannot. If somebody adds an
// Operations field to ScheduleDoc, this fails immediately and says why --
// which is the whole reason the guard moved out of LoadCache and into the
// type system.
func TestScheduleDocCannotCarryAnOperation(t *testing.T) {
	rt := reflect.TypeOf(ScheduleDoc{})
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if strings.Contains(strings.ToLower(f.Name), "operation") {
			t.Fatalf("ScheduleDoc has a field %q: this type is decoded from FILES, and an operation read off a disk is a failover nobody re-issued", f.Name)
		}
	}
	// And the same for what actually reaches the disk.
	rs := reflect.TypeOf(StoredSchedule{})
	for i := 0; i < rs.NumField(); i++ {
		if strings.Contains(strings.ToLower(rs.Field(i).Name), "operation") {
			t.Fatalf("StoredSchedule has a field %q; operations must never be written to disk", rs.Field(i).Name)
		}
	}
}

// An "operations" key in a hand-written schedule is now an ERROR naming the
// key. It used to decode into UIConfig, be accepted, and then vanish -- since
// standalone starts no operations loop -- so an operator could put one there
// and watch nothing happen, indefinitely.
func TestScheduleFileRefusesAnOperationsKey(t *testing.T) {
	body := `{"config_version":1,"schedule":[{"vm":"web01","interval_seconds":900,"enabled":true}],
	          "operations":[{"id":"op-1","kind":"promote","vm":"web01"}]}`
	_, err := decodeScheduleDoc([]byte(body), true, "schedule.json")
	if err == nil {
		t.Fatal("an operations block in a schedule file was accepted; it would be silently ignored")
	}
	if !strings.Contains(err.Error(), "operations") {
		t.Errorf("error %q does not name the offending key", err)
	}
}

// The cache this agent writes itself is read LENIENTLY. An unknown key there
// means a downgrade, and refusing it would strand the host with no schedule
// during exactly the partition the cache exists for.
func TestCachedScheduleToleratesAnUnknownKey(t *testing.T) {
	body := `{"config_version":1,"schedule":[],"a_key_from_a_newer_agent":true}`
	if _, err := decodeScheduleDoc([]byte(body), false, "config-cache.json"); err != nil {
		t.Errorf("the agent's own cache was refused over an unknown key: %v", err)
	}
	// ...but strictly, the same bytes are an error.
	if _, err := decodeScheduleDoc([]byte(body), true, "schedule.json"); err == nil {
		t.Error("a hand-written file accepted an unknown key")
	}
}

// A hand-written file must declare its version; the agent's own cache need
// not, because a file written before this field existed is still readable and
// refusing it would cost a schedule for no safety gain.
func TestScheduleVersionRequiredOnlyForHandWrittenFiles(t *testing.T) {
	body := `{"schedule":[]}`
	if _, err := decodeScheduleDoc([]byte(body), true, "schedule.json"); err == nil {
		t.Error("a hand-written schedule with no config_version was accepted")
	}
	if _, err := decodeScheduleDoc([]byte(body), false, "config-cache.json"); err != nil {
		t.Errorf("a cache written before config_version existed was refused: %v", err)
	}
}

func TestScheduleVersionMismatchIsNamed(t *testing.T) {
	_, err := decodeScheduleDoc([]byte(`{"config_version":2,"schedule":[]}`), true, "schedule.json")
	if err == nil || !strings.Contains(err.Error(), "config_version") {
		t.Errorf("error = %v, want it to name config_version", err)
	}
}

// The round trip must not quietly drop a setting. Every field an operator can
// write has to survive being stored and read back, or a restart silently
// changes behaviour.
func TestScheduleDocRoundTripsEverySetting(t *testing.T) {
	in := UIConfig{
		ReportIntervalSeconds:  120,
		PollWaitSeconds:        45,
		CadenceSeconds:         map[string]int{"web01": 900},
		Schedule:               []ScheduleEntry{{VM: "web01", IntervalSeconds: 900, Enabled: true}},
		MaxConcurrentSyncs:     3,
		TargetReplicationSlots: map[string]int{"dr01": 2},
		ShutdownTimeoutSec:     300,
		// The one field that must NOT survive.
		Operations: []Operation{{ID: "op-1", Kind: OpPromote, VM: "web01"}},
	}

	data, err := json.Marshal(StoredSchedule{
		ScheduleDoc: scheduleDocFrom(in),
		Source:      ScheduleSource{ETag: `"abc"`, FetchedAtUnix: 1_800_000_000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "op-1") {
		t.Fatalf("an operation reached the serialized form: %s", data)
	}

	back, err := decodeScheduleDoc(data, false, "config-cache.json")
	if err != nil {
		t.Fatal(err)
	}
	got := back.toUIConfig()
	if len(got.Operations) != 0 {
		t.Errorf("operations survived a disk round trip: %+v", got.Operations)
	}
	// Everything else must be intact.
	in.Operations = nil
	if !reflect.DeepEqual(got, in) {
		t.Errorf("round trip lost a setting\n got %+v\nwant %+v", got, in)
	}
}

// Every silently-coerced value must now say so. The failure this closes is an
// operator typing a setting, the agent quietly substituting something else,
// and nothing anywhere saying the typed value never applied -- which does not
// look like a mistake, it looks like the feature not working.
func TestComplaintsNamesEverySilentCoercion(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cfg    UIConfig
		wantIn string
	}{
		{"a zero report interval", UIConfig{ReportIntervalSeconds: 0, PollWaitSeconds: 30}, "report_interval_seconds"},
		{"a zero poll wait", UIConfig{ReportIntervalSeconds: 60}, "poll_wait_seconds"},
		{"a poll wait over the cap", UIConfig{ReportIntervalSeconds: 60, PollWaitSeconds: 99999}, "poll_wait_seconds"},
		{"a negative concurrency limit", UIConfig{ReportIntervalSeconds: 60, PollWaitSeconds: 30, MaxConcurrentSyncs: -1}, "max_concurrent_syncs"},
		{"a concurrency limit over the clamp", UIConfig{ReportIntervalSeconds: 60, PollWaitSeconds: 30, MaxConcurrentSyncs: 5000}, "clamped"},
		{
			// admit() tests `slots > 0`, so a negative reads as "no limit" --
			// the exact opposite of what -1 is meant to express.
			"a negative replication slot count",
			UIConfig{ReportIntervalSeconds: 60, PollWaitSeconds: 30, TargetReplicationSlots: map[string]int{"dr01": -1}},
			"IGNORED",
		},
		{
			"an entry that can never run",
			UIConfig{ReportIntervalSeconds: 60, PollWaitSeconds: 30,
				Schedule: []ScheduleEntry{{VM: "web01", IntervalSeconds: 0, Enabled: true}}},
			"never run",
		},
		{
			"an entry with no vm",
			UIConfig{ReportIntervalSeconds: 60, PollWaitSeconds: 30,
				Schedule: []ScheduleEntry{{IntervalSeconds: 900, Enabled: true}}},
			"no vm",
		},
		{
			"the same vm twice",
			UIConfig{ReportIntervalSeconds: 60, PollWaitSeconds: 30,
				Schedule: []ScheduleEntry{
					{VM: "web01", IntervalSeconds: 900, Enabled: true},
					{VM: "web01", IntervalSeconds: 900, Enabled: true},
				}},
			"more than once",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.cfg.Complaints()
			joined := strings.Join(got, "\n")
			if !strings.Contains(joined, tc.wantIn) {
				t.Errorf("Complaints() = %v, want something mentioning %q", got, tc.wantIn)
			}
		})
	}
}

// A sound document must produce silence, or the warnings become noise nobody
// reads and the whole mechanism is worse than nothing.
func TestComplaintsIsSilentOnAGoodConfig(t *testing.T) {
	c := UIConfig{
		ReportIntervalSeconds:  60,
		PollWaitSeconds:        30,
		MaxConcurrentSyncs:     4,
		TargetReplicationSlots: map[string]int{"dr01": 2},
		ShutdownTimeoutSec:     300,
		Schedule:               []ScheduleEntry{{VM: "web01", IntervalSeconds: 900, Enabled: true}},
	}
	if got := c.Complaints(); len(got) != 0 {
		t.Errorf("a valid configuration produced complaints: %v", got)
	}
}

// Complaints judged RAW entries, which was right before templates existed and
// became wrong the moment they did. Warnings that are false about a working
// system are worse than none: they send an operator hunting a fault that is
// not there, and they teach them to ignore the channel.
func TestComplaintsJudgesResolvedEntries(t *testing.T) {
	base := func(sched []ScheduleEntry, tpl map[string]ScheduleTemplate) UIConfig {
		return UIConfig{ReportIntervalSeconds: 60, PollWaitSeconds: 30,
			MaxConcurrentSyncs: 4, Schedule: sched, Templates: tpl}
	}

	t.Run("an inherited interval is not reported as never running", func(t *testing.T) {
		c := base(
			[]ScheduleEntry{{VM: "db01", Enabled: true, Template: "hourly"}},
			map[string]ScheduleTemplate{"hourly": {Name: "hourly", IntervalSeconds: 3600, Enabled: true}},
		)
		for _, g := range c.Complaints() {
			if strings.Contains(g, "interval_seconds 0") {
				t.Errorf("complained %q about an entry that inherits 3600s and runs fine", g)
			}
		}
	})

	t.Run("an inherited profile half is not reported as unusable", func(t *testing.T) {
		// compress_level without compress is invalid ALONE and valid once the
		// template's compress is resolved in. Judging the raw entry called
		// this broken on every adoption.
		c := base(
			[]ScheduleEntry{{VM: "web01", Enabled: true, Template: "z",
				Profile: SyncProfile{CompressLevel: "9"}}},
			map[string]ScheduleTemplate{"z": {Name: "z", IntervalSeconds: 900, Enabled: true,
				Profile: SyncProfile{Compress: "zstd", CompressLevel: "5"}}},
		)
		for _, g := range c.Complaints() {
			if strings.Contains(g, "unusable profile") {
				t.Errorf("complained %q about a profile that resolves cleanly", g)
			}
		}
	})

	t.Run("a genuinely broken entry is still reported", func(t *testing.T) {
		// The fix must not silence the complaints that were the point: an
		// entry with no cadence anywhere still never runs.
		c := base([]ScheduleEntry{{VM: "web01", Enabled: true}}, nil)
		if !containsSubstring(c.Complaints(), "interval_seconds") {
			t.Errorf("stopped reporting an entry that genuinely never runs: %v", c.Complaints())
		}
	})

	t.Run("the verify cadence is checked on this path at all", func(t *testing.T) {
		// It never was. Only the standalone loader ran validateVerifyCadence,
		// so a control plane could publish either of these and the agent
		// adopted it without a word, then quietly never verified that VM.
		for _, tc := range []struct {
			name string
			e    ScheduleEntry
			want string
		}{
			{
				"both cadence forms at once",
				ScheduleEntry{VM: "web01", IntervalSeconds: 900, Enabled: true,
					Profile:               SyncProfile{Verify: "fast"},
					VerifyIntervalSeconds: 86400,
					VerifyDays:            "Sun", VerifyWindow: "02:00-12:00"},
				"both",
			},
			{
				"a calendar that does not parse",
				ScheduleEntry{VM: "web01", IntervalSeconds: 900, Enabled: true,
					Profile:    SyncProfile{Verify: "fast"},
					VerifyDays: "Frunday"},
				"calendar",
			},
			{
				"a cadence with no verify mode",
				ScheduleEntry{VM: "web01", IntervalSeconds: 900, Enabled: true,
					VerifyDays: "Sun", VerifyWindow: "02:00-12:00"},
				"verify mode",
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				got := base([]ScheduleEntry{tc.e}, nil).Complaints()
				if !containsSubstring(got, tc.want) {
					t.Errorf("no complaint mentioning %q; got %v", tc.want, got)
				}
			})
		}
	})

	t.Run("a bad template is reported once, not once per entry", func(t *testing.T) {
		// On this path nothing refuses it -- the control plane's document is
		// input to survive -- so it has to be loud instead.
		c := base(
			[]ScheduleEntry{{VM: "a", Enabled: true, Template: "bad"}, {VM: "b", Enabled: true, Template: "bad"}},
			map[string]ScheduleTemplate{"bad": {Name: "bad", IntervalSeconds: 0}},
		)
		n := 0
		for _, g := range c.Complaints() {
			if strings.Contains(g, `template "bad"`) {
				n++
			}
		}
		if n != 1 {
			t.Errorf("the shared template was complained about %d times, want once: %v", n, c.Complaints())
		}
	})

	t.Run("a valid templated config is silent", func(t *testing.T) {
		c := base(
			[]ScheduleEntry{
				{VM: "db01", Enabled: true, Template: "nightly"},
				{VM: "web01", Enabled: true, IntervalSeconds: 300},
			},
			map[string]ScheduleTemplate{
				"nightly": {Name: "nightly", IntervalSeconds: 3600, Enabled: true,
					VerifyDays: "Sun *-*-01..07", VerifyWindow: "02:00-12:00",
					Profile: SyncProfile{Verify: "fast"}},
				DefaultTemplateName: {Name: DefaultTemplateName, IntervalSeconds: 900, Enabled: true},
			},
		)
		if got := c.Complaints(); len(got) != 0 {
			t.Errorf("a valid templated configuration produced complaints: %v", got)
		}
	})
}

func containsSubstring(all []string, want string) bool {
	for _, s := range all {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}
