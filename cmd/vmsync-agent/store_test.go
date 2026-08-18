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
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestCredentialsRoundTrip(t *testing.T) {
	s := Store{Dir: t.TempDir()}

	if _, ok, err := s.LoadCredentials(); err != nil || ok {
		t.Fatalf("LoadCredentials() on a fresh agent = ok:%v err:%v, want ok:false with no error -- first boot is not a failure", ok, err)
	}

	want := Credentials{AgentID: "agent-7", Token: "secret", UIBase: "https://ui.example.org"}
	if err := s.SaveCredentials(want); err != nil {
		t.Fatalf("SaveCredentials() error = %v", err)
	}
	got, ok, err := s.LoadCredentials()
	if err != nil || !ok {
		t.Fatalf("LoadCredentials() = ok:%v err:%v, want the saved credentials back", ok, err)
	}
	if got != want {
		t.Errorf("LoadCredentials() = %+v, want %+v", got, want)
	}
}

func TestCredentialsAreNotWorldReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix file modes")
	}
	s := Store{Dir: t.TempDir()}
	if err := s.SaveCredentials(Credentials{AgentID: "a", Token: "secret"}); err != nil {
		t.Fatalf("SaveCredentials() error = %v", err)
	}
	info, err := os.Stat(s.credentialsPath())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("credentials file mode = %04o, want 0600 -- this is the agent's bearer token", perm)
	}
}

func TestCacheFallsBackToDefaultsBeforeTheUIIsEverReached(t *testing.T) {
	// The distinction matters: "never reached the UI" and "the UI said
	// this" produce the same config here, and the agent needs to be able to
	// tell them apart when reporting how stale its instructions are.
	s := Store{Dir: t.TempDir()}
	got, ok, err := s.LoadCache()
	if err != nil {
		t.Fatalf("LoadCache() error = %v", err)
	}
	if ok {
		t.Error("LoadCache() reported ok on a fresh agent that has never polled")
	}
	if !reflect.DeepEqual(got.Config, DefaultUIConfig()) {
		t.Errorf("LoadCache() = %+v, want the defaults so a never-enrolled agent still runs", got.Config)
	}
}

func TestCacheRoundTripAndNormalizesOnRead(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	// A cache file written by an older or buggier build could hold values
	// that would turn the poll loop into a busy loop; reading normalizes.
	if err := s.SaveCache(CachedConfig{
		ETag:          `"v9"`,
		FetchedAtUnix: 1_800_000_000,
		Config:        UIConfig{ReportIntervalSeconds: 0, PollWaitSeconds: 0},
	}); err != nil {
		t.Fatalf("SaveCache() error = %v", err)
	}

	got, ok, err := s.LoadCache()
	if err != nil || !ok {
		t.Fatalf("LoadCache() = ok:%v err:%v, want the saved cache back", ok, err)
	}
	if got.ETag != `"v9"` || got.FetchedAtUnix != 1_800_000_000 {
		t.Errorf("LoadCache() = %+v, want the etag and timestamp preserved", got)
	}
	if !reflect.DeepEqual(got.Config, DefaultUIConfig()) {
		t.Errorf("LoadCache() config = %+v, want zero values normalized to the defaults", got.Config)
	}
}

// TestWriteIsAtomic pins the property the config cache depends on: it is
// what the agent falls back on when the UI is unreachable, so a crash or a
// full disk partway through a write must not be able to replace a working
// fallback with a truncated file at exactly the moment it is needed.
func TestWriteIsAtomic(t *testing.T) {
	dir := t.TempDir()
	s := Store{Dir: dir}

	if err := s.SaveCache(CachedConfig{ETag: `"first"`, Config: DefaultUIConfig()}); err != nil {
		t.Fatalf("SaveCache() error = %v", err)
	}
	if err := s.SaveCache(CachedConfig{ETag: `"second"`, Config: DefaultUIConfig()}); err != nil {
		t.Fatalf("SaveCache() error = %v", err)
	}

	got, _, err := s.LoadCache()
	if err != nil {
		t.Fatalf("LoadCache() error = %v", err)
	}
	if got.ETag != `"second"` {
		t.Errorf("etag = %q, want the second write to have replaced the first", got.ETag)
	}

	// No temporary files left behind. A rename-based write that leaked its
	// temp files would fill the state directory over months of polling.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read state dir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" {
			t.Errorf("state dir holds leftover %q, want only the .json files", e.Name())
		}
	}
}

func TestCorruptStateFileIsReportedNotIgnored(t *testing.T) {
	// Silently treating unparsable state as "absent" would make an agent
	// quietly re-enrol, or quietly discard its fallback config, with no
	// indication anything was wrong.
	s := Store{Dir: t.TempDir()}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(s.credentialsPath(), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, err := s.LoadCredentials(); err == nil {
		t.Fatal("LoadCredentials() silently accepted a corrupt file")
	}
}

func TestSaveCreatesTheStateDirectory(t *testing.T) {
	// systemd's StateDirectory= would normally create this, but the agent
	// must also work when run by hand from a shell for debugging.
	s := Store{Dir: filepath.Join(t.TempDir(), "nested", "state")}
	if err := s.SaveCredentials(Credentials{AgentID: "a", Token: "t"}); err != nil {
		t.Fatalf("SaveCredentials() into a missing directory failed: %v", err)
	}
	if _, ok, err := s.LoadCredentials(); err != nil || !ok {
		t.Fatalf("LoadCredentials() = ok:%v err:%v after creating the directory", ok, err)
	}
}
