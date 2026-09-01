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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"vmsync/pkg/trace"
)

// reloadPollInterval is how often the config file is re-read looking for a
// change nobody signalled.
//
// The poll is not a convenience on top of SIGHUP. Making "settings can be
// changed without a restart" true only for operators who remember to signal
// makes it a ritual rather than a property -- and the one time it is
// forgotten is during an incident, which is when it matters.
const reloadPollInterval = 10 * time.Second

// live holds the configuration every loop reads.
//
// atomic.Pointer rather than a mutex around a mutable struct, and not for
// speed: it makes a half-applied configuration STRUCTURALLY impossible. A
// mutex guarding field-by-field assignment would let a reader observe the new
// ssh_key beside the old ssh_user -- a combination that was never in any file
// and that nobody could reproduce afterwards.
//
// Never nil after startup.
type live struct{ cfg atomic.Pointer[agentConfig] }

func newLive(cfg agentConfig) *live {
	l := &live{}
	l.cfg.Store(&cfg)
	return l
}

// get returns the current generation.
//
// THE READER RULE: one get() per unit of work, and that pointer is used for
// the whole of it. It is easy to state and easy to break -- turning
// cfg.VmsyncPath into a second get() inside a launch would give generation
// N's argv and generation N+1's binary. A sync that started under one
// configuration must finish under it.
func (l *live) get() *agentConfig { return l.cfg.Load() }

// reloader re-reads the configuration file and publishes new generations.
//
// One instance, and every path into it is serialized by mu, so SIGHUP racing
// the poll cannot interleave two swaps.
type reloader struct {
	lv         *live
	configPath string
	// Startup-only inputs that a reload must carry forward unchanged. They
	// are not in the file, so re-resolving without them would silently clear
	// them on the first reload.
	once       bool
	forceDebug bool

	mu sync.Mutex
	// lastApplied is the digest of the file contents that produced the
	// current generation. Committed only AFTER a reload fully succeeds, so a
	// half-written file fails to parse, this is unchanged, and the next poll
	// retries by itself -- which is what makes a truncate-in-place editor
	// safe without a second signal.
	lastApplied string
	// lastErr suppresses repeating the same failure every 10 seconds while an
	// operator edits a file over several minutes.
	lastErr string
	gen     uint64
}

func newReloader(lv *live, configPath string, initialDigest string, once, forceDebug bool) *reloader {
	return &reloader{
		lv: lv, configPath: configPath,
		once: once, forceDebug: forceDebug,
		lastApplied: initialDigest,
	}
}

// Run watches for changes until ctx is cancelled.
//
// SIGHUP and a content poll both funnel through the same reload, so there is
// exactly one code path that can publish a generation.
func (r *reloader) Run(ctx context.Context, hup <-chan os.Signal) {
	t := time.NewTicker(reloadPollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
			r.reload("SIGHUP")
		case <-t.C:
			r.reload("file changed")
		}
	}
}

// reload re-reads, validates and publishes -- or changes nothing at all.
func (r *reloader) reload(cause string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	data, err := os.ReadFile(r.configPath)
	if err != nil {
		r.failed(cause, fmt.Errorf("read %s: %w", r.configPath, err))
		return
	}
	digest := configDigest(data)
	if digest == r.lastApplied {
		// Silent for the poll -- it is answering "no" every ten seconds
		// forever -- but never silent for an explicit signal, because an
		// operator who asked for a reload is owed an answer.
		if cause == "SIGHUP" {
			trace.Info("reload: the configuration file is unchanged, nothing to do", "path", r.configPath)
		}
		return
	}

	// Everything that can fail happens BEFORE anything is published.
	af, warnings, err := LoadAgentFile(r.configPath)
	if err != nil {
		r.failed(cause, err)
		return
	}
	next, err := resolveAgentConfig(af, r.configPath, r.once, r.forceDebug, "")
	if err != nil {
		r.failed(cause, err)
		return
	}

	cur := r.lv.get()
	if err := refuseColdChanges(*cur, next); err != nil {
		// Refuses the WHOLE reload, not just the offending field. Applying
		// the rest would publish a generation that is neither the old file
		// nor the new one, and no operator could reason about what is
		// actually running.
		r.failed(cause, err)
		cur.metrics.configRejected()
		return
	}

	changes := describeChanges(*cur, next)

	// Carry forward the live objects: they have process lifetime and are not
	// described by the file. Losing any of them here would reset counters,
	// orphan the run log's open file, or detach the metrics identity.
	next.metrics = cur.metrics
	next.runLog = cur.runLog
	r.gen++
	next.Gen = r.gen

	r.lv.cfg.Store(&next)
	next.metrics.setConfigGeneration(next.Gen)
	r.lastApplied = digest
	r.lastErr = ""

	trace.SetDebug(next.Debug)
	for _, w := range warnings {
		trace.Warning("configuration hygiene", "detail", w)
	}
	if len(changes) == 0 {
		// The bytes differed but nothing this agent acts on did: a comment,
		// reordering, whitespace. Worth one line so the operator knows their
		// edit was seen and had no effect, rather than wondering.
		trace.Info("reload: the file changed but no setting did", "cause", cause, "generation", next.Gen)
		return
	}
	trace.Warning("reload: configuration changed", "cause", cause, "generation", next.Gen, "changes", len(changes))
	for _, c := range changes {
		trace.Warning("reload: " + c)
	}
}

// failed reports a refused reload without repeating itself every ten seconds
// while somebody edits a file.
func (r *reloader) failed(cause string, err error) {
	msg := err.Error()
	if msg == r.lastErr {
		return
	}
	r.lastErr = msg
	trace.Error("reload REFUSED; the running configuration is unchanged", "cause", cause, "error", err)
}

// configDigest identifies file contents.
//
// A hash of the bytes, with NO stat pre-filter. A (size, mtime) gate would
// defeat the hash it gates: a same-second, equal-length in-place rewrite is
// then invisible outright, and the operator's edit silently never applies
// while the log says there was no change. Reading a few KB every ten seconds
// costs nothing next to that.
func configDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// coldFields are the settings a running agent cannot adopt.
//
// state_dir is the only one, and it is not a config change at all: it is a
// move of four files that other goroutines are actively writing -- the
// credential, the schedule cache, the operation ledger, the fence ledger and
// the run journal. The fence ledger is the sharp one. Both ledgers snapshot
// under their mutex and write OUTSIDE it, so a relocate interleaved with a
// fence's Begin can land that record in the directory nobody will read next;
// after a restart Acted() returns false and the agent performs a SECOND
// unattended shutdown of a production VM.
//
// Refused rather than half-applied, and refused loudly enough that nobody
// concludes the reload worked.
func refuseColdChanges(old, next agentConfig) error {
	if old.StateDir != next.StateDir {
		return fmt.Errorf(`"state_dir" cannot be changed while the agent is running (%s -> %s): it holds the credential, both ledgers and the run log, which other goroutines are writing to right now. Restart the agent to move it`,
			old.StateDir, next.StateDir)
	}
	// Mode is not a setting either. Flipping between control-plane and
	// standalone changes who is in charge of this host, and costing the
	// operator a deliberate restart for that is the feature.
	if (old.StandaloneFile == "") != (next.StandaloneFile == "") {
		return fmt.Errorf("this agent cannot change between control-plane and standalone mode while running; restart it")
	}
	return nil
}

// describeChanges lists what an operator would care about, one line each.
//
// Pure and exhaustive by construction: every field compared here is a field a
// reload can change, and anything missing from this list applies silently --
// which is the failure this whole mechanism exists to remove, wearing a
// different hat.
func describeChanges(old, next agentConfig) []string {
	var out []string
	add := func(name, was, now string) {
		if was != now {
			out = append(out, fmt.Sprintf("%s: %q -> %q", name, was, now))
		}
	}
	addInt := func(name string, was, now int) {
		if was != now {
			out = append(out, fmt.Sprintf("%s: %d -> %d", name, was, now))
		}
	}
	addBool := func(name string, was, now bool) {
		if was != now {
			out = append(out, fmt.Sprintf("%s: %v -> %v", name, was, now))
		}
	}

	add("hostname", old.Hostname, next.Hostname)
	add("libvirt_uri", old.LibvirtURI, next.LibvirtURI)
	add("vmsync_path", old.VmsyncPath, next.VmsyncPath)
	add("bridge_helper_path", old.BridgeHelperPath, next.BridgeHelperPath)
	add("target_uri_pattern", old.TargetURIPattern, next.TargetURIPattern)
	add("prometheus_dir", old.PrometheusDir, next.PrometheusDir)
	add("ssh.user", old.SSHUser, next.SSHUser)
	add("ssh.key", old.SSHKey, next.SSHKey)
	add("ssh.known_hosts", old.SSHKnownHosts, next.SSHKnownHosts)
	addInt("ssh.port", old.SSHPort, next.SSHPort)
	addInt("limits.max_concurrent_syncs", old.MaxConcurrentSyncs, next.MaxConcurrentSyncs)
	addBool("features.schedule", !old.NoSchedule, !next.NoSchedule)
	addBool("features.autofence", !old.NoAutoFence, !next.NoAutoFence)
	addBool("log.debug", old.Debug, next.Debug)
	add("control_plane.url", old.UIBase, next.UIBase)
	add("control_plane.ca_file", old.CAFile, next.CAFile)
	if old.HTTPTimeout != next.HTTPTimeout {
		out = append(out, fmt.Sprintf("control_plane.http_timeout_sec: %v -> %v", old.HTTPTimeout, next.HTTPTimeout))
	}
	return out
}
