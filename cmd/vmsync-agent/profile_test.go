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
	"slices"
	"strings"
	"testing"
)

// Validate is the gate between a networked UI and a binary that can delete
// a replica's disks, so it is worth testing exhaustively in both
// directions: rejecting something legitimate breaks replication, accepting
// something unexpected is the failure this whole design exists to prevent.

func TestValidateAcceptsEveryBuiltInPreset(t *testing.T) {
	for name, p := range Presets {
		if err := p.Validate(); err != nil {
			t.Errorf("built-in preset %q does not pass validation: %v", name, err)
		}
	}
	// The presets are what most installs will run, so their intent is worth
	// pinning rather than leaving to whoever edits the map next.
	if wan := Presets[PresetWAN]; wan.Compress != "zstd" || wan.CompressLevel != "5" {
		t.Errorf("wan preset = %+v, want zstd level 5 -- the link is the bottleneck there", wan)
	}
	if lan := Presets[PresetLAN]; lan.Compress != "zstd" || lan.CompressLevel != "1" {
		t.Errorf("lan preset = %+v, want zstd level 1 -- the CPU is the bottleneck there", lan)
	}
	if direct := Presets[PresetDirect]; direct.Compress != "" || direct.NetBuffer != "" {
		t.Errorf("direct preset = %+v, want no bridge at all", direct)
	}
}

func TestValidateAcceptsReasonableProfiles(t *testing.T) {
	good := []SyncProfile{
		{},
		{Compress: "zstd", CompressLevel: "19"},
		{Compress: "s2", CompressLevel: "best"},
		{Compress: "s2"},
		{NetBuffer: "64k,512M"},
		{Compress: "zstd", CompressLevel: "3", NetBuffer: "128k,1G", UseSSH: true},
		{IODepth: 1},
		{IODepth: maxIODepth},
		{Verify: "full"},
		{ReinitAfterFailures: 3},
		{TargetDiskPath: "/data/replicas"},
		{SourcePortRange: "auto", TargetPortRange: "20000-20100"},
		{TargetPortRange: "20809"},
	}
	for _, p := range good {
		if err := p.Validate(); err != nil {
			t.Errorf("Validate(%+v) = %v, want nil", p, err)
		}
	}
}

func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name     string
		profile  SyncProfile
		mentions string
	}{
		{"an unknown compression algorithm", SyncProfile{Compress: "lz4"}, "lz4"},
		{"a zstd level out of range", SyncProfile{Compress: "zstd", CompressLevel: "20"}, "20"},
		{"a zstd level that is not a number", SyncProfile{Compress: "zstd", CompressLevel: "best"}, "best"},
		{"an s2 level that is a number", SyncProfile{Compress: "s2", CompressLevel: "5"}, "5"},
		{"a level with no algorithm", SyncProfile{CompressLevel: "5"}, "compress_level"},
		{"a malformed netbuffer spec", SyncProfile{NetBuffer: "notaspec"}, "netbuffer"},
		{"a zero-sized netbuffer, which would deadlock", SyncProfile{NetBuffer: "64k,0M"}, "netbuffer"},
		{"use_ssh with no bridge to tunnel", SyncProfile{UseSSH: true}, "use_ssh"},
		{"a negative io depth", SyncProfile{IODepth: -1}, "io_depth"},
		{"an absurd io depth", SyncProfile{IODepth: maxIODepth + 1}, "io_depth"},
		{"an unknown verify mode", SyncProfile{Verify: "quick"}, "quick"},
		{"a negative reinit threshold", SyncProfile{ReinitAfterFailures: -1}, "reinit_after_failures"},
		{"a relative target disk path", SyncProfile{TargetDiskPath: "replicas"}, "absolute"},
		{"a target disk path climbing out with ..", SyncProfile{TargetDiskPath: "/data/../../etc"}, "clean"},
		{"an inverted port range", SyncProfile{TargetPortRange: "20100-20000"}, "target_port_range"},
		{"a privileged port range", SyncProfile{SourcePortRange: "80-100"}, "source_port_range"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.profile.Validate()
			if err == nil {
				t.Fatalf("Validate(%+v) = nil, want an error", tc.profile)
			}
			// The message has to name the offending field or value: this
			// fires against settings typed into a UI on another machine, and
			// a bare "invalid profile" would be untraceable from a
			// hypervisor's journal.
			if !strings.Contains(err.Error(), tc.mentions) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.mentions)
			}
		})
	}
}

// TestCommandArgsUsesEqualsFormForBoolLikeFlags pins a detail that would
// otherwise fail only at runtime, on a real host: vmsync's -compress and
// -netbuffer implement IsBoolFlag, so "-compress zstd" does NOT consume
// "zstd" as the value -- it is left as a positional argument, which vmsync
// rejects outright. The "=" form is mandatory.
func TestCommandArgsUsesEqualsFormForBoolLikeFlags(t *testing.T) {
	req := SyncRequest{
		SourceURI: "qemu:///system", SourceDomain: "web01",
		TargetURI: "qemu+ssh://root@hyper02p/system", TargetDomain: "web01",
		Profile: SyncProfile{
			Compress: "zstd", CompressLevel: "5",
			NetBuffer: "128k,1G", UseSSH: true, Verify: "full",
		},
	}
	args := req.CommandArgs()

	for _, want := range []string{"-compress=zstd", "-netbuffer=128k,1G", "-verify=full", "-use-ssh"} {
		if !slices.Contains(args, want) {
			t.Errorf("args %v do not contain %q", args, want)
		}
	}
	for _, bad := range []string{"-compress", "-netbuffer", "-verify"} {
		if slices.Contains(args, bad) {
			t.Errorf("args %v contain the bare %q, which would leave its value as a positional argument vmsync rejects", args, bad)
		}
	}
}

func TestCommandArgsCarriesEndpointsAndCredentials(t *testing.T) {
	req := SyncRequest{
		SourceURI: "qemu:///system", SourceDomain: "web01",
		TargetURI: "qemu+ssh://root@hyper02p/system", TargetDomain: "web01",
		SSHUser: "root", SSHKey: "/root/.ssh/id_vmsync", SSHPort: 2222,
		SSHKnownHosts:      "/etc/vmsync/known_hosts",
		PrometheusTextfile: "/var/lib/node_exporter/textfile_collector/vmsync_web01.prom",
		BridgeHelperPath:   "/usr/local/bin/vmsync-bridge-helper",
		Profile:            SyncProfile{TargetDiskPath: "/data/replicas", IODepth: 16},
	}
	args := req.CommandArgs()

	pairs := map[string]string{
		"-source-uri":          "qemu:///system",
		"-source-domain":       "web01",
		"-target-uri":          "qemu+ssh://root@hyper02p/system",
		"-target-domain":       "web01",
		"-target-disk-path":    "/data/replicas",
		"-ssh-user":            "root",
		"-ssh-key":             "/root/.ssh/id_vmsync",
		"-ssh-port":            "2222",
		"-ssh-known-hosts":     "/etc/vmsync/known_hosts",
		"-bridge-helper-path":  "/usr/local/bin/vmsync-bridge-helper",
		"-io-depth":            "16",
		"-prometheus-textfile": "/var/lib/node_exporter/textfile_collector/vmsync_web01.prom",
	}
	for flag, want := range pairs {
		i := slices.Index(args, flag)
		if i < 0 {
			t.Errorf("args %v are missing %s", args, flag)
			continue
		}
		if i+1 >= len(args) || args[i+1] != want {
			t.Errorf("%s = %q, want %q", flag, args[i+1], want)
		}
	}
}

// TestCommandArgsOmitsWhatWasNotAskedFor guards against the opposite
// mistake: emitting a flag with an empty value, which vmsync would either
// reject or -- worse -- interpret as an explicit choice of the empty value.
func TestCommandArgsOmitsWhatWasNotAskedFor(t *testing.T) {
	req := SyncRequest{
		SourceURI: "qemu:///system", SourceDomain: "web01",
		TargetURI: "qemu+ssh://root@h/system", TargetDomain: "web01",
	}
	args := req.CommandArgs()

	for _, absent := range []string{
		"-compress", "-compress=", "-compress-level", "-netbuffer", "-netbuffer=",
		"-no-checksum",
		"-use-ssh", "-io-depth", "-verify", "-verify=", "-reinit-after-failures",
		"-target-disk-path", "-ssh-user", "-ssh-key", "-ssh-port", "-ssh-known-hosts",
		"-source-nbd-port", "-target-nbd-port", "-prometheus-textfile",
		"-bridge-helper-path",
	} {
		if slices.Contains(args, absent) {
			t.Errorf("args %v contain %q although the profile did not ask for it", args, absent)
		}
	}
	// An empty profile still has to produce a runnable command.
	if len(args) != 8 {
		t.Errorf("args = %v, want exactly the four endpoint flags and their values", args)
	}
}

// TestCommandArgsNeverEmitsAShell is the property the whole design rests
// on: the result is an argv for exec.Command, so no element is ever
// interpreted by a shell. A value containing shell metacharacters must
// therefore travel through completely untouched -- neither escaped nor
// stripped -- because nothing will ever try to parse it.
func TestCommandArgsPassesAwkwardValuesThroughVerbatim(t *testing.T) {
	nasty := "/data/replicas; rm -rf /"
	req := SyncRequest{
		SourceURI: "qemu:///system", SourceDomain: "web01",
		TargetURI: "qemu+ssh://root@h/system", TargetDomain: "web01",
		Profile: SyncProfile{TargetDiskPath: nasty},
	}
	args := req.CommandArgs()
	i := slices.Index(args, "-target-disk-path")
	if i < 0 || args[i+1] != nasty {
		t.Fatalf("args %v did not carry the path through verbatim", args)
	}
	// And it would never have got this far in practice: Validate rejects it
	// long before a command is built.
	if err := (SyncProfile{TargetDiskPath: nasty}).Validate(); err == nil {
		t.Error("Validate accepted a target_disk_path containing shell metacharacters")
	}
}

// The pre-commit integrity check is ON by vmsync's own default, so the agent
// must emit -no-checksum only when a profile explicitly asks for it.
//
// The direction that matters is the silent one: a profile written before this
// field existed, or one that simply does not mention it, must leave the check
// running. If the field were positive ("checksum": false) an absent value
// would be indistinguishable from a deliberate disable, and every existing
// profile would quietly turn a safety feature off.
func TestNoChecksumIsEmittedOnlyWhenAsked(t *testing.T) {
	base := SyncRequest{
		SourceURI: "qemu:///system", SourceDomain: "web01",
		TargetURI: "qemu+ssh://root@h/system", TargetDomain: "web01",
	}

	t.Run("absent from the profile leaves the check on", func(t *testing.T) {
		if args := base.CommandArgs(); slices.Contains(args, "-no-checksum") {
			t.Errorf("args %v contain -no-checksum although the profile never mentioned it", args)
		}
	})

	t.Run("explicitly disabled emits the flag", func(t *testing.T) {
		req := base
		req.Profile = SyncProfile{NoChecksum: true}
		args := req.CommandArgs()
		if !slices.Contains(args, "-no-checksum") {
			t.Errorf("args %v are missing -no-checksum for a profile that set it", args)
		}
		// A bare flag: it must not acquire a value argument, which would be
		// parsed by vmsync as a positional and stop flag parsing dead.
		for i, a := range args {
			if a == "-no-checksum" && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				t.Errorf("-no-checksum was followed by %q; it takes no value", args[i+1])
			}
		}
	})

	t.Run("a profile that disables it is still valid", func(t *testing.T) {
		p := SyncProfile{NoChecksum: true}
		if err := p.Validate(); err != nil {
			t.Errorf("Validate rejected a profile that only disables the checksum: %v", err)
		}
	})
}

// -verify-failure-reinit must be emitted only when asked for, and a profile
// asking for it without a verify mode must be REFUSED rather than quietly
// stripped.
//
// The refusal is the half worth pinning. This flag is what clears a recorded
// verification failure -- vmsync lets it past the refusal that a failed
// replica otherwise imposes -- so a profile that enabled it with no
// verification would hold the key to the interlock while being unable to
// prove anything. Silently dropping it instead would be worse than refusing:
// a schedule would claim to self-repair for months and only be found out on
// the night it mattered.
func TestVerifyFailureReinitIsEmittedOnlyWhenAsked(t *testing.T) {
	base := SyncRequest{
		SourceURI: "qemu:///system", SourceDomain: "web01",
		TargetURI: "qemu+ssh://root@h/system", TargetDomain: "web01",
	}

	t.Run("absent from the profile emits nothing", func(t *testing.T) {
		if args := base.CommandArgs(); slices.Contains(args, "-verify-failure-reinit") {
			t.Errorf("args %v contain -verify-failure-reinit although the profile never mentioned it", args)
		}
	})

	t.Run("asked for, it is emitted as a bare flag", func(t *testing.T) {
		req := base
		req.Profile = SyncProfile{Verify: "full", VerifyFailureReinit: true}
		args := req.CommandArgs()
		if !slices.Contains(args, "-verify-failure-reinit") {
			t.Fatalf("args %v are missing -verify-failure-reinit for a profile that set it", args)
		}
		// Same trap as -no-checksum: a value argument here would be read by
		// vmsync as a positional and stop flag parsing dead.
		for i, a := range args {
			if a == "-verify-failure-reinit" && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				t.Errorf("-verify-failure-reinit was followed by %q; it takes no value", args[i+1])
			}
		}
	})

	t.Run("without a verify mode the profile is refused", func(t *testing.T) {
		p := SyncProfile{VerifyFailureReinit: true}
		if err := p.Validate(); err == nil {
			t.Error("Validate accepted verify_failure_reinit with no verify mode; it would hold the key to the verification interlock while being unable to prove anything")
		}
	})

	t.Run("with a verify mode the profile is valid", func(t *testing.T) {
		for _, mode := range []string{"fast", "full", "qemu-img"} {
			p := SyncProfile{Verify: mode, VerifyFailureReinit: true}
			if err := p.Validate(); err != nil {
				t.Errorf("Validate rejected verify=%s with verify_failure_reinit: %v", mode, err)
			}
		}
	})
}
