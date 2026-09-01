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
	"strings"
	"testing"
)

// reloadFixture gives a config file, a live config built from it, and a
// reloader primed with that file's digest -- the state an agent is in
// immediately after startup.
type reloadFixture struct {
	path string
	lv   *live
	r    *reloader
}

func newReloadFixture(t *testing.T, body string) *reloadFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.json")
	writeConfig(t, path, body)

	af, _, err := LoadAgentFile(path)
	if err != nil {
		t.Fatalf("the fixture config does not load: %v", err)
	}
	cfg, err := resolveAgentConfig(af, path, false, false, "")
	if err != nil {
		t.Fatalf("the fixture config does not resolve: %v", err)
	}
	lv := newLive(cfg)
	data, _ := os.ReadFile(path)
	return &reloadFixture{path: path, lv: lv, r: newReloader(lv, path, configDigest(data), false, false)}
}

func writeConfig(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A standalone config, because it needs no reachable control plane to
// validate. state_dir is pinned so the cold-change tests can move it.
func standaloneConfig(stateDir, vmsyncPath string) string {
	return `{
  "config_version": 1,
  "state_dir": "` + stateDir + `",
  "vmsync_path": "` + vmsyncPath + `",
  "schedule_file": "/etc/vmsync/schedule.json"
}`
}

func TestReloadAppliesAChange(t *testing.T) {
	f := newReloadFixture(t, standaloneConfig("/var/lib/vmsync-agent", "/usr/local/bin/vmsync"))
	if got := f.lv.get().VmsyncPath; got != "/usr/local/bin/vmsync" {
		t.Fatalf("starting vmsync_path = %q", got)
	}

	writeConfig(t, f.path, standaloneConfig("/var/lib/vmsync-agent", "/opt/vmsync/bin/vmsync"))
	f.r.reload("test")

	cur := f.lv.get()
	if cur.VmsyncPath != "/opt/vmsync/bin/vmsync" {
		t.Errorf("vmsync_path = %q, want the edited value", cur.VmsyncPath)
	}
	if cur.Gen != 1 {
		t.Errorf("generation = %d, want 1 after one accepted reload", cur.Gen)
	}
}

// An unchanged file must not publish a generation. Otherwise the generation
// gauge climbs every ten seconds forever and stops meaning anything.
func TestReloadIgnoresAnUnchangedFile(t *testing.T) {
	f := newReloadFixture(t, standaloneConfig("/var/lib/vmsync-agent", "/usr/local/bin/vmsync"))
	before := f.lv.get()

	f.r.reload("test")
	f.r.reload("SIGHUP")

	if after := f.lv.get(); after.Gen != before.Gen {
		t.Errorf("generation moved from %d to %d with no change to the file", before.Gen, after.Gen)
	}
}

// THE property that makes an editor safe without a second signal: a refused
// reload must not record the digest, so the next poll retries by itself.
//
// A half-written file is a parse error. If the digest were committed before
// validation, that broken content would be remembered as "applied" and the
// operator's finished edit -- saved a second later -- would be seen as
// unchanged and silently never applied.
func TestARefusedReloadIsRetriedOnceTheFileIsFixed(t *testing.T) {
	f := newReloadFixture(t, standaloneConfig("/var/lib/vmsync-agent", "/usr/local/bin/vmsync"))

	// Mid-save: truncated JSON.
	writeConfig(t, f.path, `{"config_version": 1, "vmsync_pa`)
	f.r.reload("test")
	if cur := f.lv.get(); cur.Gen != 0 || cur.VmsyncPath != "/usr/local/bin/vmsync" {
		t.Fatalf("a broken file changed the running configuration: gen=%d path=%q", cur.Gen, cur.VmsyncPath)
	}

	// The editor finishes writing.
	writeConfig(t, f.path, standaloneConfig("/var/lib/vmsync-agent", "/opt/vmsync/bin/vmsync"))
	f.r.reload("test")

	cur := f.lv.get()
	if cur.VmsyncPath != "/opt/vmsync/bin/vmsync" {
		t.Errorf("vmsync_path = %q; the completed edit was never applied, which is what committing the digest too early would cause", cur.VmsyncPath)
	}
	if cur.Gen != 1 {
		t.Errorf("generation = %d, want 1", cur.Gen)
	}
}

// A file that parses but fails validation is refused just as completely --
// nothing is half-applied.
func TestAnInvalidConfigChangesNothing(t *testing.T) {
	f := newReloadFixture(t, standaloneConfig("/var/lib/vmsync-agent", "/usr/local/bin/vmsync"))

	// A relative path: parses fine, fails rule 3.
	writeConfig(t, f.path, `{"config_version":1,"schedule_file":"etc/schedule.json"}`)
	f.r.reload("test")

	cur := f.lv.get()
	if cur.Gen != 0 {
		t.Errorf("generation = %d; an invalid file published a generation", cur.Gen)
	}
	if cur.StandaloneFile != "/etc/vmsync/schedule.json" {
		t.Errorf("schedule_file = %q, want the original", cur.StandaloneFile)
	}
}

// state_dir refuses the WHOLE reload, not just that field. Applying the rest
// would publish a generation that is neither the old file nor the new one,
// and no operator could reason about what is actually running.
func TestMovingStateDirRefusesTheEntireReload(t *testing.T) {
	f := newReloadFixture(t, standaloneConfig("/var/lib/vmsync-agent", "/usr/local/bin/vmsync"))

	// Two changes at once: one allowed, one not.
	writeConfig(t, f.path, standaloneConfig("/srv/vmsync", "/opt/vmsync/bin/vmsync"))
	f.r.reload("test")

	cur := f.lv.get()
	if cur.StateDir != "/var/lib/vmsync-agent" {
		t.Errorf("state_dir = %q; it must not move under a running agent", cur.StateDir)
	}
	if cur.VmsyncPath != "/usr/local/bin/vmsync" {
		t.Errorf("vmsync_path = %q; the allowed half of a refused reload was applied anyway, leaving a configuration that is neither file", cur.VmsyncPath)
	}
	if cur.Gen != 0 {
		t.Errorf("generation = %d, want 0", cur.Gen)
	}
}

func TestRefuseColdChanges(t *testing.T) {
	base := agentConfig{StateDir: "/var/lib/vmsync-agent", StandaloneFile: "/etc/vmsync/schedule.json"}
	for _, tc := range []struct {
		name    string
		next    agentConfig
		wantErr string
	}{
		{"moving state_dir", agentConfig{StateDir: "/srv/x", StandaloneFile: base.StandaloneFile}, "state_dir"},
		{"standalone to control plane", agentConfig{StateDir: base.StateDir, StandaloneFile: ""}, "mode"},
		{"no cold change", agentConfig{StateDir: base.StateDir, StandaloneFile: base.StandaloneFile}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := refuseColdChanges(base, tc.next)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("refused a permitted change: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Error("permitted a change a running agent cannot make")
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

// The live objects have process lifetime and are not described by the file.
// Losing them across a reload would reset every counter and orphan the run
// log's open file handle.
func TestReloadCarriesTheLiveObjectsForward(t *testing.T) {
	f := newReloadFixture(t, standaloneConfig("/var/lib/vmsync-agent", "/usr/local/bin/vmsync"))
	m := newAgentMetrics("test", "host01", true)
	rl := newRunLog(t.TempDir(), "session-1", m)

	cur := *f.lv.get()
	cur.metrics, cur.runLog = m, rl
	f.lv.cfg.Store(&cur)

	writeConfig(t, f.path, standaloneConfig("/var/lib/vmsync-agent", "/opt/vmsync/bin/vmsync"))
	f.r.reload("test")

	next := f.lv.get()
	if next.metrics != m {
		t.Error("the metrics object was replaced by a reload; every counter would reset")
	}
	if next.runLog != rl {
		t.Error("the run log was replaced by a reload; its open file handle would be orphaned")
	}
}

func TestDescribeChanges(t *testing.T) {
	old := agentConfig{
		Hostname: "prod01", VmsyncPath: "/usr/local/bin/vmsync",
		SSHUser: "root", MaxConcurrentSyncs: 4, NoSchedule: false,
	}
	if got := describeChanges(old, old); len(got) != 0 {
		t.Errorf("identical configurations produced %v", got)
	}

	next := old
	next.SSHUser = "vmsync"
	next.MaxConcurrentSyncs = 2
	next.NoSchedule = true

	joined := strings.Join(describeChanges(old, next), "\n")
	for _, want := range []string{"ssh.user", "limits.max_concurrent_syncs", "features.schedule"} {
		if !strings.Contains(joined, want) {
			t.Errorf("changes do not mention %q:\n%s", want, joined)
		}
	}
	// The old value has to be there too: "features.schedule: false" alone
	// does not tell an operator whether their edit is what changed it.
	if !strings.Contains(joined, `"root"`) || !strings.Contains(joined, `"vmsync"`) {
		t.Errorf("a change does not show both the old and new value:\n%s", joined)
	}
}

func TestConfigDigest(t *testing.T) {
	a := configDigest([]byte(`{"config_version":1}`))
	if a == "" {
		t.Fatal("empty digest")
	}
	if a != configDigest([]byte(`{"config_version":1}`)) {
		t.Error("the same bytes produced different digests")
	}
	// Whitespace counts. It has to: the digest's job is "are these the same
	// bytes", and anything cleverer risks calling a real edit unchanged.
	if a == configDigest([]byte(`{"config_version": 1}`)) {
		t.Error("a byte difference produced the same digest")
	}
}

// --debug is a startup-only override that survives every reload. A file
// saying log.debug=false must not be able to switch it off mid-incident.
func TestForcedDebugSurvivesAReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.json")
	writeConfig(t, path, standaloneConfig("/var/lib/vmsync-agent", "/usr/local/bin/vmsync"))
	af, _, err := LoadAgentFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// forceDebug true, as --debug does.
	cfg, err := resolveAgentConfig(af, path, false, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Debug {
		t.Fatal("--debug did not turn debug on at startup")
	}
	lv := newLive(cfg)
	data, _ := os.ReadFile(path)
	r := newReloader(lv, path, configDigest(data), false, true)

	// The file still says nothing about debug, i.e. false.
	writeConfig(t, path, standaloneConfig("/var/lib/vmsync-agent", "/opt/vmsync/bin/vmsync"))
	r.reload("test")

	if !lv.get().Debug {
		t.Error("a reload switched off a debug that was forced on from the command line")
	}
}
