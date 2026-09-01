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
	"crypto/x509"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strings"
	"time"
)

// agentFileVersion is the only config_version this build accepts.
//
// Checked in a raw-map pre-pass rather than after decoding, because
// DisallowUnknownFields returns on the FIRST unknown field: a version-2 file
// would otherwise be reported as `json: unknown field "…"` naming whatever key
// happened to be new, instead of "this agent is too old for this file".
const agentFileVersion = 1

// AgentFile is the local half of the agent's configuration: everything a
// control plane must never be able to choose.
//
// Nothing here is ever written by the agent, and nothing here ever arrives
// over the network. That is the whole point of the split -- ssh.key,
// vmsync_path and libvirt_uri together decide which binary runs as root with
// which credentials against which host, so a UI that could set them would not
// need any other vulnerability.
//
// The MODE is not a field. It is the presence of ControlPlane. A "mode"
// string could contradict a populated control_plane block; an object cannot
// contradict itself. And because mode then lives in the file the control
// plane cannot write, it is not something a compromised UI can grant itself.
type AgentFile struct {
	ConfigVersion int `json:"config_version"`

	// Hostname is "" to mean the system hostname. Resolved by
	// ResolveHostname, NOT here and not by withDefaults -- see there.
	Hostname     string `json:"hostname,omitempty"`
	LibvirtURI   string `json:"libvirt_uri,omitempty"`
	StateDir     string `json:"state_dir,omitempty"`
	ScheduleFile string `json:"schedule_file,omitempty"` // STANDALONE ONLY

	VmsyncPath       string `json:"vmsync_path,omitempty"`
	BridgeHelperPath string `json:"bridge_helper_path,omitempty"`
	TargetURIPattern string `json:"target_uri_pattern,omitempty"`
	PrometheusDir    string `json:"prometheus_dir,omitempty"`

	SSH      SSHFile      `json:"ssh"`
	Limits   LimitsFile   `json:"limits"`
	Features FeaturesFile `json:"features"`
	Log      LogFile      `json:"log"`

	// A POINTER, so "absent" and "present but empty" are different states:
	// the first is standalone, the second is a mistake worth refusing. An
	// explicit `"control_plane": null` decodes to nil indistinguishably from
	// absent, so it is caught before this struct decodes -- see LoadAgentFile.
	ControlPlane *ControlPlaneFile `json:"control_plane,omitempty"`
}

type SSHFile struct {
	User string `json:"user,omitempty"`
	// Key is a PATH, never key material. That is what keeps this file
	// secret-free, and therefore safe to manage from a repository.
	Key        string `json:"key,omitempty"`
	Port       int    `json:"port,omitempty"` // 0 leaves vmsync's own default
	KnownHosts string `json:"known_hosts,omitempty"`
}

type LimitsFile struct {
	// MaxConcurrentSyncs is a CEILING, never a floor: it can only lower what
	// the schedule asks for. How much concurrent I/O a hypervisor absorbs is
	// a property of that machine, and the machine is the only party that
	// knows it.
	MaxConcurrentSyncs int `json:"max_concurrent_syncs,omitempty"`
}

// FeaturesFile states what IS, not what is refused. A file full of
// "no_schedule": false is a double negative nobody reads correctly.
//
// *bool, and that is load-bearing rather than stylistic: both defaults are
// TRUE, and an absent JSON key decodes into a bool's zero value -- which
// would silently disable scheduling and autofencing on every host whose file
// omits them. A pointer distinguishes "not mentioned" from "turned off".
type FeaturesFile struct {
	Schedule  *bool `json:"schedule,omitempty"`  // false == the old -no-schedule
	AutoFence *bool `json:"autofence,omitempty"` // false == the old -no-autofence
}

type LogFile struct {
	Debug bool `json:"debug,omitempty"`
	// RunLogProbes records the fence path's peer queries in the run log too.
	// Off by default: one line per running replicated VM per minute, forever.
	RunLogProbes bool `json:"run_log_probes,omitempty"`
}

type ControlPlaneFile struct {
	URL            string `json:"url"`
	CAFile         string `json:"ca_file,omitempty"`
	HTTPTimeoutSec int    `json:"http_timeout_sec,omitempty"`
}

// Standalone reports whether this agent runs with no control plane.
func (a AgentFile) Standalone() bool { return a.ControlPlane == nil }

// resolveAgentConfig turns the file into the runtime configuration the rest
// of the agent already understands.
//
// A deliberate seam. The file is the operator's document -- nested, optional,
// stated positively -- while agentConfig is what the loops read, and keeping
// them separate means the file's shape can change without touching every
// consumer, and the loops keep the flat field names their call sites already
// use. It is also the one place the two vocabularies are reconciled, so the
// inversions (features.schedule -> NoSchedule) exist exactly once.
func resolveAgentConfig(a AgentFile, configPath string, once, forceDebug bool, enrolTokenFile string) (agentConfig, error) {
	host, err := a.ResolveHostname()
	if err != nil {
		return agentConfig{}, err
	}

	cfg := agentConfig{
		ConfigPath: configPath,
		StateDir:   a.StateDir,
		LibvirtURI: a.LibvirtURI,
		Hostname:   host,
		Once:       once,
		// The flag can only turn debug ON. It is a startup-only override that
		// survives every reload, so a --debug passed deliberately during an
		// incident is never silently switched off by a config file that says
		// false -- and the operator is told it is stuck that way.
		Debug: forceDebug || a.Log.Debug,

		VmsyncPath:       a.VmsyncPath,
		BridgeHelperPath: a.BridgeHelperPath,
		TargetURIPattern: a.TargetURIPattern,
		PrometheusDir:    a.PrometheusDir,

		SSHUser:       a.SSH.User,
		SSHKey:        a.SSH.Key,
		SSHPort:       a.SSH.Port,
		SSHKnownHosts: a.SSH.KnownHosts,

		MaxConcurrentSyncs: a.Limits.MaxConcurrentSyncs,

		// Stated positively in the file, negatively here. The file says what
		// IS, because "no_schedule": false is a double negative nobody reads
		// correctly; the code kept its existing names so every call site did
		// not have to be re-read at the same time as everything else changed.
		NoSchedule:  !boolValue(a.Features.Schedule, true),
		NoAutoFence: !boolValue(a.Features.AutoFence, true),

		// Doubles as the standalone marker, exactly as it did when it was a
		// flag: validation guarantees schedule_file is set if and only if
		// there is no control plane, so "this string is non-empty" and "this
		// agent has no control plane" remain the same question.
		StandaloneFile: a.ScheduleFile,
	}

	if cp := a.ControlPlane; cp != nil {
		cfg.UIBase = cp.URL
		cfg.CAFile = cp.CAFile
		cfg.HTTPTimeout = time.Duration(cp.HTTPTimeoutSec) * time.Second

		tok, err := readEnrolToken(enrolTokenFile)
		if err != nil {
			return agentConfig{}, err
		}
		cfg.EnrolToken = tok
	} else if enrolTokenFile != "" {
		// An enrolment token is meaningless with nothing to enrol with, and
		// accepting it silently would leave an operator believing this host
		// reports somewhere. That belief is what gets a host forgotten.
		return agentConfig{}, fmt.Errorf(`--enrol-token-file was given but %s has no "control_plane": there is nothing to enrol with`, configPath)
	}
	return cfg, nil
}

// readEnrolToken reads a single-use token and REMOVES the file.
//
// Deleting it is the point. The token is spent by the enrolment call, so a
// copy left on disk is a credential-shaped thing that is no longer a
// credential -- confusing at best, and at worst something an operator
// re-deploys forever because it looks like configuration. This also keeps it
// off the command line, where /proc/<pid>/cmdline is world-readable and shell
// history keeps it indefinitely.
func readEnrolToken(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Already spent on a previous start, which is the ordinary state
			// of an enrolled host whose unit still names the file.
			return "", nil
		}
		return "", fmt.Errorf("read enrolment token %s: %w", path, err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", fmt.Errorf("enrolment token file %s is empty", path)
	}
	if err := os.Remove(path); err != nil {
		// Not fatal: enrolment can still succeed, and refusing to start over
		// a leftover file would be worse than the leftover.
		return tok, nil
	}
	return tok, nil
}

// boolValue dereferences an optional flag with its default.
func boolValue(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// ResolveHostname returns the name this agent reports under and is identified
// by in replication metadata, falling back to the system hostname.
//
// Deliberately NOT done in withDefaults, for two reasons. os.Hostname can
// fail, and withDefaults has nowhere to put an error; and a loader that asks
// the operating system what machine it is on stops being a pure function of
// its input, so every test of it would depend on the host it runs on.
//
// The fallback is validated too, which main.go never did: nothing stops a
// machine being named with a comma, and a comma splits one entry into two in
// the replica list -- so the entry never matches on the next sync, a fresh one
// is appended every time, and the VM ends up looking like it replicates to a
// dozen targets until picking one is refused and replication stops. Better to
// refuse at startup, naming the problem, than to corrupt metadata for weeks.
func (a AgentFile) ResolveHostname() (string, error) {
	if a.Hostname != "" {
		return a.Hostname, nil
	}
	h, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf(`no "hostname" is set and the system hostname could not be read: %w`, err)
	}
	if err := validateHostname(h); err != nil {
		return "", fmt.Errorf(`no "hostname" is set and the system hostname %q cannot be used: %w`, h, err)
	}
	return h, nil
}

// validateHostname applies the one rule that matters, to both the configured
// name and the system fallback.
func validateHostname(h string) error {
	if strings.TrimSpace(h) == "" {
		return fmt.Errorf(`"hostname" is blank; omit it to use the system hostname`)
	}
	// A colon is deliberately allowed: splitReplicaRef takes the LAST one,
	// which is exactly what makes host:port:domain split correctly.
	if strings.ContainsAny(h, ",/") || strings.ContainsAny(h, " \t\n") {
		return fmt.Errorf(`"hostname" %q contains a comma, slash or whitespace; replica metadata is a comma-separated list of host:domain entries and would be corrupted by it`, h)
	}
	return nil
}

// LoadAgentFile reads, defaults and validates the local configuration.
//
// Returns warnings alongside the config: hygiene problems that are worth
// saying out loud but must not stop an agent starting. They are returned
// rather than logged so this function stays pure and directly testable.
func LoadAgentFile(configPath string) (AgentFile, []string, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return AgentFile{}, nil, fmt.Errorf("read agent config %s: %w", configPath, err)
	}

	// Pre-pass, before the strict decode, for the two things the strict
	// decode cannot report usefully.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return AgentFile{}, nil, fmt.Errorf("parse agent config %s: %w", configPath, err)
	}
	if err := checkConfigVersion(raw, configPath); err != nil {
		return AgentFile{}, nil, err
	}
	// An explicit null would decode to a nil pointer, indistinguishable from
	// the key being absent -- i.e. a silent flip into standalone mode out of a
	// JSON literal, on a host that is meant to have a control plane.
	if v, ok := raw["control_plane"]; ok && string(bytes.TrimSpace(v)) == "null" {
		return AgentFile{}, nil, fmt.Errorf(`%s: "control_plane" is null. Remove the key entirely to run standalone, or give it a url -- an explicit null is indistinguishable from a typo that would silently take this host off its control plane`, configPath)
	}

	var a AgentFile
	dec := json.NewDecoder(bytes.NewReader(data))
	// Strict, unlike the wire document. A person wrote this file, and a
	// misspelled key that silently keeps its default is the failure mode a
	// config file has and a flag does not: an unknown FLAG is already an
	// error, and this must not be a regression on that.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return AgentFile{}, nil, fmt.Errorf("parse agent config %s: %w", configPath, err)
	}

	a = a.withDefaults()
	if err := a.Validate(); err != nil {
		return AgentFile{}, nil, fmt.Errorf("%s: %w", configPath, err)
	}
	return a, a.warnings(configPath), nil
}

func checkConfigVersion(raw map[string]json.RawMessage, configPath string) error {
	v, ok := raw["config_version"]
	if !ok {
		return fmt.Errorf(`%s: no "config_version". Add "config_version": %d`, configPath, agentFileVersion)
	}
	var n int
	if err := json.Unmarshal(v, &n); err != nil {
		return fmt.Errorf(`%s: "config_version" is not a number: %w`, configPath, err)
	}
	if n != agentFileVersion {
		return fmt.Errorf(`%s: "config_version" is %d, but this vmsync-agent understands %d -- upgrade the agent, or write a version %d file`, configPath, n, agentFileVersion, agentFileVersion)
	}
	return nil
}

// withDefaults fills in everything that has one, so Validate and every reader
// afterwards see a fully-resolved document.
func (a AgentFile) withDefaults() AgentFile {
	if a.LibvirtURI == "" {
		a.LibvirtURI = "qemu:///system"
	}
	if a.StateDir == "" {
		a.StateDir = "/var/lib/vmsync-agent"
	}
	if a.VmsyncPath == "" {
		a.VmsyncPath = "/usr/local/bin/vmsync"
	}
	if a.TargetURIPattern == "" {
		a.TargetURIPattern = "qemu+ssh://%s/system"
	}
	if a.Features.Schedule == nil {
		a.Features.Schedule = boolPtr(true)
	}
	if a.Features.AutoFence == nil {
		a.Features.AutoFence = boolPtr(true)
	}
	if a.ControlPlane != nil && a.ControlPlane.HTTPTimeoutSec == 0 {
		a.ControlPlane.HTTPTimeoutSec = 120
	}
	return a
}

func boolPtr(b bool) *bool { return &b }

// Validate refuses a file that cannot be run, naming the JSON key rather than
// a flag that no longer exists.
func (a AgentFile) Validate() error {
	// A hostname ends up in "host:domain" replica entries, and the stored
	// list is split on commas. A comma splits one entry into two, so the
	// entry never matches on the next sync, a fresh one is appended every
	// time, and the VM eventually looks like it replicates to a dozen
	// targets -- at which point picking one is refused and replication stops.
	// A colon is deliberately fine: splitReplicaRef takes the LAST one.
	if a.Hostname != "" {
		if err := validateHostname(a.Hostname); err != nil {
			return err
		}
	}

	for _, p := range []struct{ key, val string }{
		{"state_dir", a.StateDir},
		{"schedule_file", a.ScheduleFile},
		{"vmsync_path", a.VmsyncPath},
		{"bridge_helper_path", a.BridgeHelperPath},
		{"prometheus_dir", a.PrometheusDir},
		{"ssh.key", a.SSH.Key},
		{"ssh.known_hosts", a.SSH.KnownHosts},
	} {
		if err := validateAbsCleanPath(p.key, p.val); err != nil {
			return err
		}
	}

	// Trial-render rather than count verbs. A trial render catches zero
	// verbs, two verbs and a stray %d alike, and it accepts the valid
	// "%s%%s" that substring-counting would refuse.
	if strings.Contains(fmt.Sprintf(a.TargetURIPattern, "probe"), "%!") {
		return fmt.Errorf(`"target_uri_pattern" %q does not take exactly one %%s; it renders as %q`, a.TargetURIPattern, fmt.Sprintf(a.TargetURIPattern, "probe"))
	}

	if a.SSH.Port < 0 || a.SSH.Port > 65535 {
		return fmt.Errorf(`"ssh.port" %d is out of range; 0 leaves vmsync's own default`, a.SSH.Port)
	}

	// Refused above the ceiling, not clamped. The UI's answer is clamped
	// because the UI is another program of another vintage; this file was
	// typed by a person, and quietly running 128 when they asked for 5000 is
	// not what they meant either way.
	if a.Limits.MaxConcurrentSyncs < 0 || a.Limits.MaxConcurrentSyncs > hardMaxConcurrent {
		return fmt.Errorf(`"limits.max_concurrent_syncs" %d is out of range (0 to leave it to the schedule, otherwise up to %d)`, a.Limits.MaxConcurrentSyncs, hardMaxConcurrent)
	}

	if a.ControlPlane != nil {
		if err := a.ControlPlane.validate(); err != nil {
			return err
		}
		// In control-plane mode the schedule file is agent-written state
		// under state_dir. Letting the operator point it at /etc means the
		// agent silently overwrites a config-managed file.
		if a.ScheduleFile != "" {
			return fmt.Errorf(`"schedule_file" cannot be set together with "control_plane": in control-plane mode the schedule is written by the agent under "state_dir", and pointing it elsewhere would have the agent overwrite that file`)
		}
	} else if a.ScheduleFile == "" {
		return fmt.Errorf(`no "control_plane" and no "schedule_file": with no control plane this file is the only thing that says what to sync, so one of the two is required`)
	}
	return nil
}

func (c ControlPlaneFile) validate() error {
	if strings.TrimSpace(c.URL) == "" {
		return fmt.Errorf(`"control_plane.url" is required when "control_plane" is present`)
	}
	// https only, and no option to skip verification -- the same refusal the
	// client has always made, hoisted here so a bad address fails at load
	// rather than at first contact.
	if !strings.HasPrefix(c.URL, "https://") {
		return fmt.Errorf(`"control_plane.url" %q must be https://`, c.URL)
	}
	if c.HTTPTimeoutSec <= 0 {
		return fmt.Errorf(`"control_plane.http_timeout_sec" must be positive`)
	}
	if c.CAFile != "" {
		if err := validateAbsCleanPath("control_plane.ca_file", c.CAFile); err != nil {
			return err
		}
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return fmt.Errorf(`"control_plane.ca_file": %w`, err)
		}
		if !x509.NewCertPool().AppendCertsFromPEM(pem) {
			return fmt.Errorf(`"control_plane.ca_file" %s contains no usable PEM certificate`, c.CAFile)
		}
	}
	return nil
}

// validateAbsCleanPath refuses a relative or non-canonical path.
//
// Relative because it would resolve against whatever working directory
// systemd handed the unit, which is not something the file's author chose.
// Non-canonical because "/var/lib/../lib/vmsync-agent" and
// "/var/lib/vmsync-agent" are the same directory but not the same string, and
// several things here compare paths.
//
// POSIX semantics via "path", NOT "path/filepath", and deliberately so. Every
// path in this file names a location on the Linux host the agent runs on:
// libvirt, /run, /proc and systemd are all in that sentence already. filepath
// would make the RULE depend on the machine the code was compiled for --
// filepath.IsAbs("/var/lib/vmsync-agent") is false on Windows -- so a
// perfectly good config would validate differently per build host, and the
// validator could not be exercised anywhere but Linux.
func validateAbsCleanPath(key, val string) error {
	if val == "" {
		return nil
	}
	if !path.IsAbs(val) {
		return fmt.Errorf("%q %s must be an absolute path", key, val)
	}
	if c := path.Clean(val); c != val {
		return fmt.Errorf("%q %s is not canonical; write it as %s", key, val, c)
	}
	return nil
}

// warnings are hygiene problems: worth telling the operator, never worth
// refusing to start over.
//
// Deliberately not refusals. A permissions check tests the wrong thing at the
// wrong time -- os.Stat follows symlinks and the exec happens minutes to weeks
// later -- and turning any of these into an error would newly break hosts that
// work today, on a reload, across an estate at once.
func (a AgentFile) warnings(configPath string) []string {
	var w []string
	if m, ok := looseMode(configPath); ok {
		w = append(w, fmt.Sprintf("%s is mode %#o: it is group- or world-writable, and it decides which binary this agent runs as root", configPath, m))
	}
	if m, ok := looseMode(a.VmsyncPath); ok {
		w = append(w, fmt.Sprintf("%s is mode %#o: anyone who can write it chooses what this agent executes as root", a.VmsyncPath, m))
	}
	// Not a refusal, and not even really about this file: vmsync reads the
	// key with os.ReadFile and never inspects its mode, so a 0644 key works
	// today. Refusing one would break working hosts to enforce a rule nothing
	// else enforces.
	if a.SSH.Key != "" {
		if fi, err := os.Stat(a.SSH.Key); err == nil && fi.Mode().Perm()&0o077 != 0 {
			w = append(w, fmt.Sprintf("%s is mode %#o: an SSH private key readable by anyone but its owner", a.SSH.Key, fi.Mode().Perm()))
		}
	}
	return w
}

// looseMode reports whether p is group- or world-writable.
func looseMode(p string) (os.FileMode, bool) {
	fi, err := os.Stat(p)
	if err != nil {
		return 0, false
	}
	perm := fi.Mode().Perm()
	return perm, perm&0o022 != 0
}
