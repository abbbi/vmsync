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

// writeAgentFile drops a config on disk and returns its path.
func writeAgentFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(p, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	return p
}

const standaloneBody = `{
  "config_version": 1,
  "schedule_file": "/etc/vmsync/schedule.json"
}`

const controlPlaneBody = `{
  "config_version": 1,
  "control_plane": { "url": "https://ui.dr.example.org" }
}`

// Everything a file omits must come back as the documented default, because a
// short file is the common case and every omitted key is a decision nobody
// made.
func TestLoadAgentFileAppliesDefaults(t *testing.T) {
	a, _, err := LoadAgentFile(writeAgentFile(t, standaloneBody))
	if err != nil {
		t.Fatalf("LoadAgentFile: %v", err)
	}
	for _, tc := range []struct{ name, got, want string }{
		{"libvirt_uri", a.LibvirtURI, "qemu:///system"},
		{"state_dir", a.StateDir, "/var/lib/vmsync-agent"},
		{"vmsync_path", a.VmsyncPath, "/usr/local/bin/vmsync"},
		{"target_uri_pattern", a.TargetURIPattern, "qemu+ssh://%s/system"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
	// The two that must default to TRUE. A plain bool would decode an absent
	// key as false and silently switch scheduling and fencing off on every
	// host whose file does not mention them.
	if a.Features.Schedule == nil || !*a.Features.Schedule {
		t.Error("features.schedule did not default to true -- an omitted key would stop this host scheduling")
	}
	if a.Features.AutoFence == nil || !*a.Features.AutoFence {
		t.Error("features.autofence did not default to true -- an omitted key would turn off split-brain protection")
	}
}

// Mode is the presence of control_plane, nothing else.
func TestLoadAgentFileDetectsMode(t *testing.T) {
	sa, _, err := LoadAgentFile(writeAgentFile(t, standaloneBody))
	if err != nil {
		t.Fatalf("standalone: %v", err)
	}
	if !sa.Standalone() {
		t.Error("a file with no control_plane is not standalone")
	}

	cp, _, err := LoadAgentFile(writeAgentFile(t, controlPlaneBody))
	if err != nil {
		t.Fatalf("control plane: %v", err)
	}
	if cp.Standalone() {
		t.Error("a file with a control_plane read as standalone")
	}
	if cp.ControlPlane.HTTPTimeoutSec != 120 {
		t.Errorf("http_timeout_sec = %d, want the 120s default", cp.ControlPlane.HTTPTimeoutSec)
	}
}

// An explicit null decodes to a nil pointer indistinguishably from the key
// being absent -- i.e. a host silently leaving its control plane because of a
// JSON literal. It has to be caught before the struct decode.
func TestLoadAgentFileRefusesAnExplicitNullControlPlane(t *testing.T) {
	_, _, err := LoadAgentFile(writeAgentFile(t, `{"config_version":1,"control_plane":null}`))
	if err == nil {
		t.Fatal("an explicit null control_plane was accepted; this host would silently run standalone")
	}
	if !strings.Contains(err.Error(), "null") {
		t.Errorf("error %q does not explain the null", err)
	}
}

// The version check must run BEFORE the strict decode, or a future file is
// reported as an unknown field naming whatever key happens to be new.
func TestLoadAgentFileChecksVersionBeforeUnknownFields(t *testing.T) {
	_, _, err := LoadAgentFile(writeAgentFile(t, `{"config_version":2,"a_key_from_the_future":true}`))
	if err == nil {
		t.Fatal("a version 2 file was accepted")
	}
	if !strings.Contains(err.Error(), "config_version") {
		t.Errorf("error %q blames the wrong thing; it should name config_version, not the unknown key", err)
	}
	if strings.Contains(err.Error(), "unknown field") {
		t.Errorf("error %q is the strict-decode message; the version pre-pass did not run first", err)
	}
}

// A misspelled key that silently keeps its default is the failure a config
// file has and a flag does not -- an unknown FLAG is already an error today,
// and this must not be a regression on that.
func TestLoadAgentFileRefusesAnUnknownKey(t *testing.T) {
	_, _, err := LoadAgentFile(writeAgentFile(t, `{"config_version":1,"schedule_file":"/etc/x.json","libvirt_url":"qemu:///system"}`))
	if err == nil {
		t.Fatal("a misspelled key was accepted and would have silently kept its default")
	}
	if !strings.Contains(err.Error(), "libvirt_url") {
		t.Errorf("error %q does not name the offending key", err)
	}
}

func TestAgentFileValidation(t *testing.T) {
	for _, tc := range []struct {
		name, body, wantIn string
	}{
		{
			// A comma splits one replica entry into two, so the entry never
			// matches on the next sync and a fresh one is appended every time.
			"a hostname containing a comma",
			`{"config_version":1,"schedule_file":"/etc/x.json","hostname":"a,b"}`,
			"hostname",
		},
		{
			"a relative path",
			`{"config_version":1,"schedule_file":"etc/x.json"}`,
			"absolute",
		},
		{
			"a non-canonical path",
			`{"config_version":1,"schedule_file":"/etc/../etc/x.json"}`,
			"canonical",
		},
		{
			"a target_uri_pattern with no verb",
			`{"config_version":1,"schedule_file":"/etc/x.json","target_uri_pattern":"qemu+ssh:///system"}`,
			"target_uri_pattern",
		},
		{
			"a target_uri_pattern with two verbs",
			`{"config_version":1,"schedule_file":"/etc/x.json","target_uri_pattern":"qemu+ssh://%s%s/system"}`,
			"target_uri_pattern",
		},
		{
			"an out-of-range ssh port",
			`{"config_version":1,"schedule_file":"/etc/x.json","ssh":{"port":70000}}`,
			"ssh.port",
		},
		{
			// Refused, not clamped: a person typed this, and quietly running
			// 128 is not what they meant either.
			"a nonsense concurrency limit",
			`{"config_version":1,"schedule_file":"/etc/x.json","limits":{"max_concurrent_syncs":5000}}`,
			"max_concurrent_syncs",
		},
		{
			"a control plane url that is not https",
			`{"config_version":1,"control_plane":{"url":"http://ui.example.org"}}`,
			"https",
		},
		{
			"a control plane with no url",
			`{"config_version":1,"control_plane":{"ca_file":"/etc/ca.pem"}}`,
			"url",
		},
		{
			// In control-plane mode the schedule file is agent-written state
			// under state_dir; pointing it at /etc means the agent overwrites
			// a config-managed file.
			"schedule_file together with control_plane",
			`{"config_version":1,"schedule_file":"/etc/x.json","control_plane":{"url":"https://ui"}}`,
			"schedule_file",
		},
		{
			"standalone with nothing to run",
			`{"config_version":1}`,
			"schedule_file",
		},
		{
			"no config_version at all",
			`{"schedule_file":"/etc/x.json"}`,
			"config_version",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := LoadAgentFile(writeAgentFile(t, tc.body))
			if err == nil {
				t.Fatalf("accepted: %s", tc.body)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error %q does not mention %q", err, tc.wantIn)
			}
		})
	}
}

// "%s%%s" is a legitimate pattern that renders one verb and a literal %s.
// Counting occurrences of "%s" refuses it; a trial render accepts it.
func TestTargetURIPatternAcceptsAnEscapedPercent(t *testing.T) {
	body := `{"config_version":1,"schedule_file":"/etc/x.json","target_uri_pattern":"qemu+ssh://%s/system%%s"}`
	if _, _, err := LoadAgentFile(writeAgentFile(t, body)); err != nil {
		t.Errorf("a pattern with an escaped percent was refused: %v", err)
	}
}

// An omitted hostname must actually resolve to the system one. The struct
// says so; nothing enforced it until this test existed, and a config that
// omits the key is the common case.
func TestResolveHostnameFallsBackToTheSystemName(t *testing.T) {
	a, _, err := LoadAgentFile(writeAgentFile(t, standaloneBody))
	if err != nil {
		t.Fatal(err)
	}
	if a.Hostname != "" {
		t.Fatalf("the loader filled in hostname = %q; resolution belongs to ResolveHostname, where the error has somewhere to go", a.Hostname)
	}
	got, err := a.ResolveHostname()
	if err != nil {
		t.Fatalf("ResolveHostname: %v", err)
	}
	want, _ := os.Hostname()
	if got != want {
		t.Errorf("ResolveHostname = %q, want the system hostname %q", got, want)
	}
}

// An explicit hostname wins over the system one, and is returned unchanged.
func TestResolveHostnamePrefersTheConfiguredName(t *testing.T) {
	a, _, err := LoadAgentFile(writeAgentFile(t, `{"config_version":1,"schedule_file":"/etc/x.json","hostname":"prod01"}`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.ResolveHostname()
	if err != nil || got != "prod01" {
		t.Errorf("ResolveHostname = %q, %v; want prod01", got, err)
	}
}

// The same rule applies to the fallback as to the configured value. main.go
// never checked the system hostname at all, so a machine named with a comma
// would quietly corrupt every replica list it appeared in.
func TestValidateHostnameAppliesToTheFallbackToo(t *testing.T) {
	for _, bad := range []string{"a,b", "a/b", "a b", "  "} {
		if err := validateHostname(bad); err == nil {
			t.Errorf("validateHostname(%q) accepted it", bad)
		}
	}
	if err := validateHostname("prod01:2222"); err != nil {
		t.Errorf("validateHostname rejected a colon, which splitReplicaRef handles: %v", err)
	}
}

// A colon in a hostname is fine and must stay fine: splitReplicaRef takes the
// LAST colon, which is exactly what makes host:port:domain split correctly.
func TestHostnameMayContainAColon(t *testing.T) {
	body := `{"config_version":1,"schedule_file":"/etc/x.json","hostname":"prod01:2222"}`
	if _, _, err := LoadAgentFile(writeAgentFile(t, body)); err != nil {
		t.Errorf("a hostname with a colon was refused: %v", err)
	}
}

// Hygiene problems are reported, never fatal. Turning any of these into an
// error would break hosts that work today, on a reload, across an estate.
func TestLooseSSHKeyPermissionsWarnRatherThanRefuse(t *testing.T) {
	dir := t.TempDir()
	// The config validator requires POSIX-absolute paths because it describes
	// a Linux host; a Windows temp dir cannot satisfy that, so there is nothing
	// meaningful to assert here off Unix.
	if !strings.HasPrefix(filepath.ToSlash(dir), "/") {
		t.Skip("needs a POSIX-absolute temp dir")
	}
	key := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(key, []byte("not really a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Chmod, NOT the mode argument to WriteFile. That one is masked by the
	// process umask, so on a host running umask 077 -- which is exactly the
	// kind of host that hardens SSH key permissions -- the file lands at 0600
	// and there is correctly nothing to warn about. chmod(2) is not masked, so
	// this asks for the permissions the test is actually about.
	if err := os.Chmod(key, 0o644); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "agent.json")
	if err := os.WriteFile(p, []byte(`{"config_version":1,"schedule_file":"/etc/x.json","ssh":{"key":"`+filepath.ToSlash(key)+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, warns, err := LoadAgentFile(p)
	if err != nil {
		t.Fatalf("a group-readable ssh key was refused, which would break hosts that work today: %v", err)
	}
	if !runtimeSupportsUnixModes() {
		t.Skip("this filesystem does not carry Unix permission bits")
	}
	// Named specifically: "some warning was produced" would pass on a warning
	// about an entirely different file.
	var found bool
	for _, w := range warns {
		if strings.Contains(w, key) {
			found = true
		}
	}
	if !found {
		t.Errorf("no warning mentions the group-readable ssh key %s; got %v", key, warns)
	}
}

// Reports whether this filesystem carries Unix permission bits at all, so the
// skip is a statement about the platform rather than about the rule. Uses
// Chmod rather than a create mode for the same reason the test above does:
// the create mode is masked by umask and would make this answer "no" on a
// perfectly capable host.
func runtimeSupportsUnixModes() bool {
	f, err := os.CreateTemp("", "modeprobe")
	if err != nil {
		return false
	}
	defer os.Remove(f.Name())
	f.Close()
	if err := os.Chmod(f.Name(), 0o646); err != nil {
		return false
	}
	fi, err := os.Stat(f.Name())
	return err == nil && fi.Mode().Perm()&0o022 != 0
}
