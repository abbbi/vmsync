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
	"strings"
	"testing"
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
