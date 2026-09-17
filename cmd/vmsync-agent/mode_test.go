package main

import (
	"strings"
	"testing"
)

func TestResolveModeAcceptsExactlyOne(t *testing.T) {
	cases := []struct {
		name                            string
		standalone, monitor, controlled bool
		want                            agentMode
	}{
		{"standalone", true, false, false, modeStandalone},
		{"monitor", false, true, false, modeMonitor},
		{"controlled", false, false, true, modeControlled},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolveMode(c.standalone, c.monitor, c.controlled)
			if err != nil {
				t.Fatalf("resolveMode: %v", err)
			}
			if got != c.want {
				t.Errorf("mode = %q, want %q", got, c.want)
			}
		})
	}
}

// No default. The only mode that could be one is --controlled, which is the
// mode where a network service can stop this host's VMs, and arriving there
// because nobody said otherwise is precisely the accident the flags exist to
// prevent.
func TestResolveModeRefusesNone(t *testing.T) {
	_, err := resolveMode(false, false, false)
	if err == nil {
		t.Fatal("no mode was given and resolveMode accepted it")
	}
	// The error has to be actionable on its own: an operator seeing this has
	// just been refused a startup and needs the list, not a lookup.
	for _, want := range []string{"--standalone", "--monitor", "--controlled"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %s: %v", want, err)
		}
	}
}

func TestResolveModeRefusesMoreThanOne(t *testing.T) {
	cases := []struct {
		name                            string
		standalone, monitor, controlled bool
		names                           []string
	}{
		{"standalone and monitor", true, true, false, []string{"--standalone", "--monitor"}},
		{"standalone and controlled", true, false, true, []string{"--standalone", "--controlled"}},
		{"monitor and controlled", false, true, true, []string{"--monitor", "--controlled"}},
		{"all three", true, true, true, []string{"--standalone", "--monitor", "--controlled"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := resolveMode(c.standalone, c.monitor, c.controlled)
			if err == nil {
				t.Fatal("two modes at once were accepted")
			}
			// Naming the ones actually given, not the whole list: the
			// operator mistyped a unit file and needs to see which two
			// words are fighting.
			for _, want := range c.names {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the error does not name %s: %v", want, err)
				}
			}
		})
	}
}

// Enrolment needs no mode: the token exchange is the same for --monitor and
// --controlled, so a setup invocation carrying --enrol-token-file may omit
// the flag and simply enrols (reporting once with --once) instead of running.
func TestResolveModeOrEnrolAllowsModelessEnrolment(t *testing.T) {
	for _, tokenFile := range []string{"/run/vmsync-enrol-token", "-"} {
		mode, enrolOnly, err := resolveModeOrEnrol(false, false, false, tokenFile)
		if err != nil {
			t.Fatalf("modeless enrolment with %q was refused: %v", tokenFile, err)
		}
		if !enrolOnly || mode != "" {
			t.Errorf("resolveModeOrEnrol = (%q, %v), want no mode and enrol-only", mode, enrolOnly)
		}
	}
}

func TestResolveModeOrEnrolKeepsTheModeWhenOneIsNamed(t *testing.T) {
	// A mode plus a token is the ordinary daemon enrolment: first start with
	// the token present, later starts reusing the stored credential.
	mode, enrolOnly, err := resolveModeOrEnrol(false, true, false, "/run/vmsync-enrol-token")
	if err != nil {
		t.Fatalf("resolveModeOrEnrol: %v", err)
	}
	if enrolOnly || mode != modeMonitor {
		t.Errorf("resolveModeOrEnrol = (%q, %v), want (%q, false)", mode, enrolOnly, modeMonitor)
	}
}

func TestResolveModeOrEnrolStillRefusesNoModeWithoutAToken(t *testing.T) {
	_, _, err := resolveModeOrEnrol(false, false, false, "")
	if err == nil {
		t.Fatal("no mode and no token was accepted; a daemon with no mode is the accident the flags exist to prevent")
	}
	for _, want := range []string{"--standalone", "--monitor", "--controlled"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %s: %v", want, err)
		}
	}
}

func TestResolveModeOrEnrolStillRefusesContradictoryModes(t *testing.T) {
	// Two modes at once is a mistyped unit file, even when a token is present:
	// the enrolment cannot know which agent it is setting up.
	_, _, err := resolveModeOrEnrol(false, true, true, "/run/vmsync-enrol-token")
	if err == nil {
		t.Fatal("two modes at once were accepted")
	}
	for _, want := range []string{"--monitor", "--controlled"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %s: %v", want, err)
		}
	}
}

func TestCheckFileMatchesModeToFile(t *testing.T) {
	cp := &ControlPlaneFile{URL: "https://ui.example:8443"}

	cases := []struct {
		name    string
		mode    agentMode
		file    AgentFile
		wantErr string // "" means it must be accepted
	}{
		{"standalone with a schedule file", modeStandalone,
			AgentFile{ScheduleFile: "/etc/vmsync/schedule.json"}, ""},
		{"standalone with nothing to run", modeStandalone,
			AgentFile{ControlPlane: cp}, "schedule_file"},

		{"controlled with a control plane", modeControlled,
			AgentFile{ControlPlane: cp}, ""},
		{"controlled with nothing to be controlled by", modeControlled,
			AgentFile{ScheduleFile: "/etc/vmsync/schedule.json"}, "control_plane"},

		{"monitor with a control plane", modeMonitor,
			AgentFile{ControlPlane: cp}, ""},
		{"monitor with nothing to report to", modeMonitor,
			AgentFile{ScheduleFile: "/etc/vmsync/schedule.json"}, "control_plane"},

		{"an unknown mode", agentMode("watcher"),
			AgentFile{ControlPlane: cp}, "unknown agent mode"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.mode.checkFile(c.file, "/etc/vmsync/agent.json")
			switch {
			case c.wantErr == "" && err != nil:
				t.Fatalf("a valid combination was refused: %v", err)
			case c.wantErr != "" && err == nil:
				t.Fatal("an invalid combination was accepted")
			case c.wantErr != "" && !strings.Contains(err.Error(), c.wantErr):
				t.Errorf("the error does not mention %q: %v", c.wantErr, err)
			}
		})
	}
}

// The trap this test exists for: applyDefaults turns every nil *bool into
// true before LoadAgentFile returns, so "features.schedule": true is what a
// file that never mentioned scheduling looks like by the time checkFile sees
// it. A rule refusing monitor mode on that value -- the obvious rule, and one
// worth trying to add -- would refuse every monitor configuration that exists.
//
// Monitor mode ignores features entirely and says so in the startup log.
func TestMonitorModeDoesNotJudgeFeatureFlags(t *testing.T) {
	cp := &ControlPlaneFile{URL: "https://ui.example:8443"}
	on, off := boolPtr(true), boolPtr(false)

	for _, c := range []struct {
		name     string
		features FeaturesFile
	}{
		{"as applyDefaults leaves an unmentioned file", FeaturesFile{Schedule: on, AutoFence: on}},
		{"explicitly switched off", FeaturesFile{Schedule: off, AutoFence: off}},
		{"not defaulted at all", FeaturesFile{}},
		{"mixed", FeaturesFile{Schedule: on, AutoFence: off}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := AgentFile{ControlPlane: cp, Features: c.features}
			if err := modeMonitor.checkFile(f, "/etc/vmsync/agent.json"); err != nil {
				t.Fatalf("monitor mode refused a file over its feature flags: %v", err)
			}
		})
	}
}

// actsOnItsOwn is what gates the scheduler, the operations loop and the fence
// loop. Asserting it directly keeps the three call sites honest: a mode added
// later gets an answer here rather than three independent comparisons that
// can disagree.
func TestOnlyMonitorRefrainsFromActing(t *testing.T) {
	if !modeStandalone.actsOnItsOwn() {
		t.Error("standalone must act: running the schedule is the whole mode")
	}
	if !modeControlled.actsOnItsOwn() {
		t.Error("controlled must act: it runs the schedule, executes operations and fences")
	}
	if modeMonitor.actsOnItsOwn() {
		t.Error("monitor must not act: it reports what something else did, and must never stop a VM")
	}
}
