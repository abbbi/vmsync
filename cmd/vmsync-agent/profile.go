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
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"vmsync/pkg/netbuffer"
	"vmsync/pkg/portalloc"
	"vmsync/pkg/restorepoint"
)

// SyncProfile is the transport configuration for one scheduled sync, as
// sent by the UI.
//
// This type is the security boundary of the whole control plane. The UI is
// a separate program reachable over the network, and everything here
// eventually becomes arguments to a binary that can delete a replica's
// disks. So:
//
//   - Every field is a typed, bounded value. There is no field that carries
//     a flag name, a command line, or free-form arguments.
//   - Validate rejects anything outside a known set BEFORE a command is
//     built, and CommandArgs is only ever called on a validated profile.
//   - The agent owns the flag vocabulary entirely. The UI never names a
//     vmsync flag; it describes intent, and this file decides how that is
//     spelled on a command line.
//   - The result is an argv slice passed to exec.Command with no shell
//     anywhere, so quoting and metacharacters are not a concern at all.
//
// Credentials are deliberately absent. The SSH user, key and known-hosts
// used to reach the target come from the agent's own local configuration,
// never from the UI: how to reach another hypervisor is the host's own
// business, and a UI compromise must not be able to redirect it.
type SyncProfile struct {
	// Compress is "", "zstd" or "s2". Empty disables compression.
	Compress string `json:"compress,omitempty"`
	// CompressLevel is a number 1-19 for zstd, or default|better|best for s2.
	CompressLevel string `json:"compress_level,omitempty"`
	// NetBuffer is "<blocksize>,<buffersize>", or empty to disable.
	NetBuffer string `json:"netbuffer,omitempty"`
	// UseSSH routes bridged traffic through the existing SSH connection.
	UseSSH bool `json:"use_ssh,omitempty"`
	// IODepth is how many NBD read/write pairs stay in flight.
	IODepth int `json:"io_depth,omitempty"`
	// Verify is "", "compare", "fast" or "online". Note that compare and
	// fast SUSPEND the source domain; online does not.
	Verify string `json:"verify,omitempty"`
	// ReinitAfterFailures forces a full resync after N consecutive failures.
	ReinitAfterFailures int `json:"reinit_after_failures,omitempty"`
	// TargetDiskPath is where the replica's disks live on the target host.
	TargetDiskPath string `json:"target_disk_path,omitempty"`
	// TimestampToleranceSec is how far a replica disk's mtime may be ahead of
	// the last sync timestamp before vmsync refuses, in seconds.
	//
	// Here rather than only on the command line because the outage it exists
	// to end is one an agent-managed estate hits hardest: those two
	// timestamps come from different hosts' clocks, so a target running a
	// second fast fails EVERY scheduled sync for that pair, forever, with an
	// error blaming out-of-band modification. A knob only reachable by
	// hand-running vmsync would not fix a schedule.
	//
	// Zero is vmsync's own default and the behaviour that predates the flag.
	TimestampToleranceSec int `json:"timestamp_tolerance_sec,omitempty"`
	// Retention is "<count>,<interval>" (e.g. "24,3h"), or empty to keep no
	// restore points.
	//
	// Without this a scheduled sync never passes -retention, so an estate run
	// entirely through the control plane takes no restore points at all --
	// and a restore operation would have nothing to restore. The feature was
	// reachable only from a hand-run or cron vmsync until this existed.
	//
	// Note the target's filesystem must support reflink copies (XFS with
	// reflink=1, or btrfs) or vmsync REFUSES the whole run rather than
	// silently making each restore point a full copy. That refusal is
	// deliberate, and it means a bad value here stops replication for that
	// pair -- so it is validated below rather than discovered at 3am.
	Retention string `json:"retention,omitempty"`
	// SourcePortRange/TargetPortRange are passed to vmsync's own port
	// selection: a fixed port, a range, or "auto".
	SourcePortRange string `json:"source_port_range,omitempty"`
	TargetPortRange string `json:"target_port_range,omitempty"`
}

// Preset names a built-in profile. The UI offers these so nobody has to
// rediscover sensible settings per link type, and can then adjust fields
// individually.
type Preset string

const (
	// PresetWAN is for a bandwidth-constrained, high-latency link: strong
	// compression is worth the CPU because the link is the bottleneck, and
	// buffering smooths out the bursty read pattern a delta sync produces.
	PresetWAN Preset = "wan"
	// PresetLAN is for a fast local link: the CPU is the bottleneck, not the
	// wire, so compression is set as light as it goes while still removing
	// the easy redundancy in qcow2 data.
	PresetLAN Preset = "lan"
	// PresetDirect turns the bridge off entirely -- no compression, no
	// buffering, no helper binary needed on the target. The floor to
	// compare the others against, and the right choice on a link fast
	// enough that any compression is a net loss.
	PresetDirect Preset = "direct"
)

// Presets are the built-in profiles, by name.
var Presets = map[Preset]SyncProfile{
	PresetWAN: {
		Compress:      "zstd",
		CompressLevel: "5",
		NetBuffer:     "128k,1G",
		IODepth:       16,
	},
	PresetLAN: {
		Compress:      "zstd",
		CompressLevel: "1",
		IODepth:       8,
	},
	PresetDirect: {
		IODepth: 8,
	},
}

// PresetProfile returns a copy of a built-in profile.
func PresetProfile(name Preset) (SyncProfile, bool) {
	p, ok := Presets[name]
	return p, ok
}

const (
	maxIODepth             = 64
	maxReinitAfterFailures = 100
	// An hour. Enough for any clock disagreement worth calling drift --
	// NTP-managed hosts differ by milliseconds and a badly broken one by
	// seconds or minutes -- and past it the out-of-band-modification check
	// would tolerate a whole working morning's worth of stray writes, which
	// is switching the check off rather than allowing for skew.
	maxTimestampToleranceSec = 3600
)

// Validate rejects anything this agent will not run. Every error names the
// offending field and its value: this fires against configuration a person
// typed into a UI on another machine, and "invalid profile" alone would be
// untraceable from a hypervisor's journal.
func (p SyncProfile) Validate() error {
	switch p.Compress {
	case "", "zstd", "s2":
	default:
		return fmt.Errorf("compress %q is not one of \"\", \"zstd\", \"s2\"", p.Compress)
	}

	if p.Compress != "" && p.CompressLevel != "" {
		if err := validateCompressLevel(p.Compress, p.CompressLevel); err != nil {
			return err
		}
	}
	if p.Compress == "" && p.CompressLevel != "" {
		return fmt.Errorf("compress_level %q was given without a compress algorithm", p.CompressLevel)
	}

	if p.NetBuffer != "" {
		// The engine's own parser, so the agent cannot drift from what
		// vmsync will actually accept.
		if _, _, err := netbuffer.ParseSpec(p.NetBuffer); err != nil {
			return fmt.Errorf("netbuffer %q: %w", p.NetBuffer, err)
		}
	}
	if p.UseSSH && p.Compress == "" && p.NetBuffer == "" {
		// vmsync documents -use-ssh as a no-op without bridging. Refusing
		// is friendlier than silently ignoring a setting someone chose.
		return fmt.Errorf("use_ssh has no effect without compress or netbuffer, which route traffic through the bridge it tunnels")
	}

	if p.IODepth < 0 || p.IODepth > maxIODepth {
		return fmt.Errorf("io_depth %d is out of range (0 to use the default, otherwise 1-%d)", p.IODepth, maxIODepth)
	}

	switch p.Verify {
	case "", "compare", "fast", "online":
	default:
		return fmt.Errorf("verify %q is not one of \"\", \"compare\", \"fast\", \"online\"", p.Verify)
	}

	if p.ReinitAfterFailures < 0 || p.ReinitAfterFailures > maxReinitAfterFailures {
		return fmt.Errorf("reinit_after_failures %d is out of range (0 to disable, otherwise up to %d)", p.ReinitAfterFailures, maxReinitAfterFailures)
	}

	if p.TargetDiskPath != "" {
		// Absolute only. A relative path would resolve against whatever
		// working directory the agent happens to have, which is not
		// something a person configuring this from a browser can reason
		// about.
		if !strings.HasPrefix(p.TargetDiskPath, "/") {
			return fmt.Errorf("target_disk_path %q must be absolute", p.TargetDiskPath)
		}
		if p.TargetDiskPath != filepath.Clean(p.TargetDiskPath) {
			return fmt.Errorf("target_disk_path %q is not a clean path (contains \"..\", a trailing slash, or a doubled separator)", p.TargetDiskPath)
		}
	}

	// Bounded on both sides. Negative is a typo; and past an hour this stops
	// being a tolerance for clock disagreement and becomes a way to switch
	// the out-of-band-modification check off, which is a different decision
	// and should not be reachable by fat-fingering a zero.
	if p.TimestampToleranceSec < 0 || p.TimestampToleranceSec > maxTimestampToleranceSec {
		return fmt.Errorf("timestamp_tolerance_sec %d is out of range (0 to compare exactly, otherwise up to %d -- beyond that the check stops catching anything)",
			p.TimestampToleranceSec, maxTimestampToleranceSec)
	}

	if p.Retention != "" {
		// The engine's own parser, for the same reason netbuffer and the
		// port ranges use theirs: a value this agent accepts and vmsync then
		// rejects is a schedule that looks configured and never runs.
		if _, err := restorepoint.ParsePolicy(p.Retention); err != nil {
			return fmt.Errorf("retention: %w", err)
		}
	}

	for label, spec := range map[string]string{
		"source_port_range": p.SourcePortRange,
		"target_port_range": p.TargetPortRange,
	} {
		if spec == "" {
			continue
		}
		// Again the engine's own parser, for the same reason.
		if _, err := portalloc.ParseSpec(spec, portalloc.DefaultTargetAutoLow, portalloc.DefaultTargetAutoHigh); err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
	}
	return nil
}

func validateCompressLevel(algo, level string) error {
	if algo == "s2" {
		switch level {
		case "default", "better", "best":
			return nil
		default:
			return fmt.Errorf("compress_level %q is not valid for s2 (want default, better or best)", level)
		}
	}
	n, err := strconv.Atoi(level)
	if err != nil {
		return fmt.Errorf("compress_level %q is not a number, which zstd requires", level)
	}
	if n < 1 || n > 19 {
		return fmt.Errorf("compress_level %d is out of zstd's range 1-19", n)
	}
	return nil
}

// SyncRequest is everything needed to run one sync: the profile plus the
// endpoints, which come from local state rather than from the UI.
type SyncRequest struct {
	SourceURI    string
	SourceDomain string
	TargetURI    string
	TargetDomain string
	Profile      SyncProfile
	// SSH* come from the agent's own configuration.
	SSHUser       string
	SSHKey        string
	SSHPort       int
	SSHKnownHosts string
	// PrometheusTextfile keeps the existing metrics pipeline working for
	// agent-scheduled runs exactly as it does for cron-driven ones.
	PrometheusTextfile string
	BridgeHelperPath   string
	// RunID joins the run lock vmsync writes to this agent's own run-log
	// entry for having launched it, so an operator holding one can find the
	// other. Set per launch, not per schedule entry.
	RunID string

	// LocalHostName is the name this agent reports under, passed so the
	// references vmsync writes into metadata match what the control plane
	// correlates pairs by. Without it a local -source-uri records the host
	// half as a loopback literal, which names every machine and therefore
	// none.
	LocalHostName string
}

// CommandArgs builds the argv for one vmsync invocation.
//
// Returns a slice for exec.Command, never a string for a shell: with no
// shell in the path there is nothing to quote and no metacharacter to
// escape. Callers must have called Validate first; this function assumes
// every value is already known-good and does no checking of its own, which
// is why it is unexported-by-convention through SyncRequest rather than
// taking loose parameters.
func (r SyncRequest) CommandArgs() []string {
	p := r.Profile
	args := []string{
		"-source-uri", r.SourceURI,
		"-source-domain", r.SourceDomain,
		"-target-uri", r.TargetURI,
		"-target-domain", r.TargetDomain,
	}

	if p.TargetDiskPath != "" {
		args = append(args, "-target-disk-path", p.TargetDiskPath)
	}
	if p.Retention != "" {
		args = append(args, "-retention", p.Retention)
	}
	if p.TimestampToleranceSec > 0 {
		args = append(args, "-timestamp-tolerance-sec", strconv.Itoa(p.TimestampToleranceSec))
	}
	if r.SSHUser != "" {
		args = append(args, "-ssh-user", r.SSHUser)
	}
	if r.SSHKey != "" {
		args = append(args, "-ssh-key", r.SSHKey)
	}
	if r.SSHPort > 0 {
		args = append(args, "-ssh-port", strconv.Itoa(r.SSHPort))
	}
	if r.SSHKnownHosts != "" {
		args = append(args, "-ssh-known-hosts", r.SSHKnownHosts)
	}
	if r.BridgeHelperPath != "" {
		args = append(args, "-bridge-helper-path", r.BridgeHelperPath)
	}

	// "-compress=zstd" rather than "-compress zstd": vmsync's -compress and
	// -netbuffer implement IsBoolFlag, so a space-separated value is NOT
	// consumed as the flag's value -- it would be left as a positional
	// argument, which vmsync rejects outright. The "=" form is required.
	if p.Compress != "" {
		args = append(args, "-compress="+p.Compress)
		if p.CompressLevel != "" {
			args = append(args, "-compress-level", p.CompressLevel)
		}
	}
	if p.NetBuffer != "" {
		args = append(args, "-netbuffer="+p.NetBuffer)
	}
	if p.UseSSH {
		args = append(args, "-use-ssh")
	}
	if p.IODepth > 0 {
		args = append(args, "-io-depth", strconv.Itoa(p.IODepth))
	}
	if p.Verify != "" {
		args = append(args, "-verify="+p.Verify)
	}
	if p.ReinitAfterFailures > 0 {
		args = append(args, "-reinit-after-failures", strconv.Itoa(p.ReinitAfterFailures))
	}
	if p.SourcePortRange != "" {
		args = append(args, "-source-nbd-port", p.SourcePortRange)
	}
	if p.TargetPortRange != "" {
		args = append(args, "-target-nbd-port", p.TargetPortRange)
	}
	if r.PrometheusTextfile != "" {
		args = append(args, "-prometheus-textfile", r.PrometheusTextfile)
	}
	if r.LocalHostName != "" {
		args = append(args, "-local-host-name", r.LocalHostName)
	}
	// Passed so vmsync stamps it into the run lock it holds. That is what
	// lets a LATER agent -- one that restarted while this sync was still
	// going -- match the running process back to the launch record in its own
	// run log, instead of knowing only that something holds the lock.
	if r.RunID != "" {
		args = append(args, "-run-id", r.RunID)
	}
	return args
}
