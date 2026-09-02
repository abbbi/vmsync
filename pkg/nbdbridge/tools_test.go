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

package nbdbridge

import (
	"context"
	"errors"
	"strings"
	"testing"

	"vmsync/pkg/version"
)

func TestCheckLocalAlwaysReturnsNil(t *testing.T) {
	// CheckLocal is documented as always succeeding now that compression and
	// buffering are native Go, regardless of what cfg asks for.
	tests := []struct {
		name string
		cfg  Config
	}{
		{"zero value config", Config{}},
		{"compress enabled", Config{Compress: true, CompressAlgo: "zstd", CompressLevel: "3"}},
		{"netbuffer enabled", Config{NetBufferBlock: "64k", NetBufferSize: "512M"}},
		{"compress and netbuffer with ssh", Config{
			Compress: true, CompressAlgo: "s2", CompressLevel: "best",
			NetBufferBlock: "64k", NetBufferSize: "512M",
			UseSSH: true, HelperPath: "/opt/vmsync/vmsync-bridge-helper",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := CheckLocal(tt.cfg); err != nil {
				t.Errorf("CheckLocal(%+v) = %v, want nil", tt.cfg, err)
			}
		})
	}
}

// TestCheckRemoteDisabledReturnsNilWithoutTouchingClient exercises the one
// SSH-free path through CheckRemote: cfg.Enabled() == false. Reading the
// source confirms this is the function's very first check -- it returns nil
// before client is ever touched -- so this must hold even with a nil
// *remotessh.Client and a bare context.Background() (no deadline, no
// values), which would make any real use of client observable (a panic, or
// a hang) if the short-circuit were ever removed or reordered.
func TestCheckRemoteDisabledReturnsNilWithoutTouchingClient(t *testing.T) {
	err := CheckRemote(context.Background(), nil, Config{}, "somehost")
	if err != nil {
		t.Fatalf("CheckRemote with a disabled config = %v, want nil", err)
	}
}

// TestCheckRemoteEnabledWithNilClientErrorsWithoutPanicking checks the other
// side of that same nil-safety: once bridging IS enabled, CheckRemote does
// call into client (client.Run). *remotessh.Client's own methods are
// nil-receiver-safe (they check "c == nil || c.client == nil" before
// touching anything and return a plain error instead), so this must surface
// as a normal, wrapped error -- not a panic -- even though no real SSH
// connection exists here at all.
func TestCheckRemoteEnabledWithNilClientErrorsWithoutPanicking(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("CheckRemote panicked with a nil client instead of returning an error: %v", r)
		}
	}()

	cfg := Config{Compress: true, CompressAlgo: "zstd", CompressLevel: "3", HelperPath: "/opt/vmsync/vmsync-bridge-helper"}
	err := CheckRemote(context.Background(), nil, cfg, "somehost")
	if err == nil {
		t.Fatal("CheckRemote with a nil client and bridging enabled = nil error, want a non-nil error")
	}
	if !strings.Contains(err.Error(), "somehost") {
		t.Errorf("CheckRemote error %q does not mention the host it was checking", err.Error())
	}
}

// fakeRunner is a CommandRunner that answers a scripted reply per command
// substring, which is all ProbeHelper needs -- it issues exactly two
// commands and both are recognisable by a fragment.
//
// This is why ProbeHelper takes the narrow CommandRunner interface rather
// than *remotessh.Client: the two questions it asks are pure logic over two
// command results, and before it was extracted they were inline in
// CheckRemote, reachable only with a real SSH server and therefore never
// tested at all.
type fakeRunner struct {
	testX      string // reply to "test -x ..."
	testXErr   error
	version    string // reply to "... -version"
	versionErr error
	commands   []string
}

func (f *fakeRunner) Run(ctx context.Context, command string) (string, error) {
	f.commands = append(f.commands, command)
	if strings.Contains(command, "test -x") {
		return f.testX, f.testXErr
	}
	return f.version, f.versionErr
}

func TestProbeHelper(t *testing.T) {
	cfg := Config{HelperPath: "/usr/local/bin/vmsync-bridge-helper"}

	t.Run("present and matching is usable", func(t *testing.T) {
		f := &fakeRunner{version: version.Version}
		st := ProbeHelper(context.Background(), f, cfg, "target01")
		if !st.Usable {
			t.Fatalf("Usable = false, reason %q", st.Reason)
		}
		if !st.Present {
			t.Error("Present = false for a usable helper")
		}
		if st.Version != version.Version {
			t.Errorf("Version = %q, want %q", st.Version, version.Version)
		}
		if st.Reason != "" {
			t.Errorf("Reason = %q, want empty when usable", st.Reason)
		}
	})

	t.Run("trailing whitespace on the version is tolerated", func(t *testing.T) {
		// The helper prints with fmt.Println and the reply arrives through
		// SSH; a stray newline must not read as a version mismatch.
		f := &fakeRunner{version: "  " + version.Version + "\n"}
		if st := ProbeHelper(context.Background(), f, cfg, "target01"); !st.Usable {
			t.Errorf("Usable = false for a whitespace-padded version: %q", st.Reason)
		}
	})

	t.Run("missing binary is not present and names the path and host", func(t *testing.T) {
		f := &fakeRunner{testXErr: errors.New("exit status 1")}
		st := ProbeHelper(context.Background(), f, cfg, "target01")
		if st.Present || st.Usable {
			t.Fatalf("Present=%v Usable=%v for a missing binary", st.Present, st.Usable)
		}
		if !strings.Contains(st.Reason, cfg.HelperPath) || !strings.Contains(st.Reason, "target01") {
			t.Errorf("Reason = %q, want it to name the path and the host", st.Reason)
		}
		// Must not have bothered asking for a version.
		if len(f.commands) != 1 {
			t.Errorf("issued %d commands, want 1 -- no version query after test -x failed: %v", len(f.commands), f.commands)
		}
	})

	t.Run("version mismatch is present but not usable", func(t *testing.T) {
		f := &fakeRunner{version: "0.1-ancient"}
		st := ProbeHelper(context.Background(), f, cfg, "target01")
		if !st.Present {
			t.Error("Present = false for a binary that exists but is the wrong version")
		}
		if st.Usable {
			t.Fatal("Usable = true for a version mismatch")
		}
		// Both versions belong in the message: the operator has to know
		// which of the two deployments to move.
		if !strings.Contains(st.Reason, "0.1-ancient") || !strings.Contains(st.Reason, version.Version) {
			t.Errorf("Reason = %q, want both versions named", st.Reason)
		}
	})

	t.Run("unaskable version is present but not usable", func(t *testing.T) {
		f := &fakeRunner{versionErr: errors.New("exit status 2")}
		st := ProbeHelper(context.Background(), f, cfg, "target01")
		if !st.Present {
			t.Error("Present = false although test -x succeeded")
		}
		if st.Usable {
			t.Fatal("Usable = true although the version could not be determined")
		}
		if st.Reason == "" {
			t.Error("Reason is empty for an unusable helper")
		}
	})

	t.Run("probes the configured path, quoted", func(t *testing.T) {
		spaced := Config{HelperPath: "/opt/vm sync/vmsync-bridge-helper"}
		f := &fakeRunner{version: version.Version}
		ProbeHelper(context.Background(), f, spaced, "target01")
		if len(f.commands) != 2 {
			t.Fatalf("issued %d commands, want 2: %v", len(f.commands), f.commands)
		}
		for _, c := range f.commands {
			// Unquoted, a path with a space would split into two arguments
			// and the probe would test the wrong thing.
			if !strings.Contains(c, "'/opt/vm sync/vmsync-bridge-helper'") {
				t.Errorf("command %q does not contain the shell-quoted helper path", c)
			}
		}
	})
}

// CheckRemote must stay a no-op when bridging is off, even now that the
// integrity check probes the same helper independently: an unbridged run
// that cannot use the helper is not a failed run.
func TestCheckRemoteStillNoOpWhenBridgingDisabled(t *testing.T) {
	if err := CheckRemote(context.Background(), nil, Config{HelperPath: "/nonexistent"}, "target01"); err != nil {
		t.Errorf("CheckRemote with bridging disabled = %v, want nil (and it must not dereference the nil client)", err)
	}
}
