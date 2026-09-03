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
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The run log is the durable record of every vmsync process this agent
// starts.
//
// It is NOT a ledger. Nothing branches on it, nothing is looked up in it, and
// no decision anywhere consults it -- that is what separates it from
// operations.json (whose Seen() gates execution) and fences.json (whose
// Acted() does). It is evidence, and its whole job is to be able to answer
// "what did this host actually run, and when" after the fact.
//
// Append-only, one JSON object per line. A whole-file rewrite has a
// truncate-then-write window; O_APPEND plus one write(2) has none, tolerates a
// torn last line after power loss (readers skip what does not parse), and
// rotates by rename with no reader coordination.
const (
	runLogFile = "runs.jsonl"
	// runLogRotateAt is the size at which the current generation is renamed
	// aside. Size rather than age: this agent already knows its clock may be
	// wrong -- it measures skew against the UI for exactly that reason -- and
	// a clock that jumps backward makes an age-capped log immortal.
	runLogRotateAt = 32 << 20 // 32 MiB
	// runLogKept is how many rotated generations survive. One: the current
	// file plus one predecessor bounds the worst case at twice the cap.
	runLogKept = 1
)

// Event kinds. A run produces two records joined by RunID -- never one mutable
// record, because a mutable record needs a rewrite and a rewrite reintroduces
// the truncation window this format exists to avoid.
const (
	runEventSession = "session" // this agent process started
	runEventLaunch  = "launch"  // a vmsync process is about to be started
	runEventExit    = "exit"    // ...and how it ended
	runEventAdopt   = "adopt"   // a run started by a PREVIOUS agent, taken over
	runEventRotate  = "rotate"  // this file was rotated
)

// Where a launch came from. Recorded because the three have different
// recovery stories and different guarantees -- see OpenRuns, which must
// exclude fences.
const (
	runOriginScheduled = "scheduled"
	runOriginOperation = "operation"
	runOriginFence     = "fence"
	runOriginProbe     = "probe"
)

type runLogRecord struct {
	Event   string `json:"event"`
	AtUnix  int64  `json:"at_unix"`
	Session string `json:"session"` // identifies this agent PROCESS
	RunID   string `json:"run_id,omitempty"`

	// launch
	Origin      string   `json:"origin,omitempty"`
	VM          string   `json:"vm,omitempty"`
	TargetHost  string   `json:"target_host,omitempty"`
	Binary      string   `json:"binary,omitempty"` // the path AT LAUNCH
	Args        []string `json:"args,omitempty"`   // redacted; see redactArgs
	OperationID string   `json:"operation_id,omitempty"`
	FenceID     string   `json:"fence_id,omitempty"`

	// exit
	PID       int    `json:"pid,omitempty"`
	ExitCode  *int   `json:"exit_code,omitempty"` // POINTER: nil means never observed
	StartErr  string `json:"start_error,omitempty"`
	DurationS int64  `json:"duration_s,omitempty"`
	Outcome   string `json:"outcome,omitempty"`
	LogTail   string `json:"log_tail,omitempty"` // failures only

	// adopt
	PriorSession  string `json:"prior_session,omitempty"`
	StartedAtUnix int64  `json:"started_at_unix,omitempty"`
}

// runLog is the append-only writer. Safe for concurrent use: the scheduler,
// the operations loop and the fence loop all write to it.
type runLog struct {
	path    string
	session string
	metrics *agentMetrics // nil is safe; every method on it is nil-guarded

	mu   sync.Mutex
	f    *os.File
	size int64
	// rotateAt is runLogRotateAt in a running agent. A field only so a test
	// can use a cap it can actually reach without writing 32 MiB.
	rotateAt int64
	// writable is the last observed state, so callers can log on the
	// TRANSITION rather than every tick. At 10s ticks across fifty VMs,
	// per-attempt logging would fill the journal of a host whose disk is
	// already full -- which is the one thing that makes a full disk worse.
	writable bool
}

func newRunLog(stateDir, session string, m *agentMetrics) *runLog {
	return &runLog{
		path:     filepath.Join(stateDir, runLogFile),
		session:  session,
		metrics:  m,
		rotateAt: runLogRotateAt,
		writable: true, // until proven otherwise; Open() settles it
	}
}

// Open prepares the file and records that this agent process started.
//
// A failure here is returned rather than swallowed. Under the fail-closed
// contract (see Append) an agent that cannot write this file cannot launch
// syncs, and starting anyway would produce a host that looks healthy and
// silently replicates nothing -- the exact shape of failure this whole design
// is built to refuse.
func (l *runLog) Open() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.reopenLocked(); err != nil {
		l.writable = false
		l.metrics.setRunLogWritable(false)
		return err
	}
	l.writable = true
	err := l.appendLocked(runLogRecord{Event: runEventSession})
	l.writable = err == nil
	l.metrics.setRunLogWritable(l.writable)
	return err
}

func (l *runLog) reopenLocked() error {
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return fmt.Errorf("create state dir for %s: %w", l.path, err)
	}
	// 0600, unlike the 0644 state files beside it. Those hold no secret
	// individually and neither does this -- but this one AGGREGATES: every
	// argv ever run, every SSH key path, every target URI, every disk path.
	// Collectively that is a map of this host's DR topology, and it is also
	// the file most likely to leave the host attached to a ticket.
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open run log %s: %w", l.path, err)
	}
	size := int64(0)
	if fi, err := f.Stat(); err == nil {
		size = fi.Size()
	}
	l.f, l.size = f, size
	return nil
}

// Append durably records one line.
//
// A non-nil error means the record is NOT on disk, and the caller MUST NOT
// launch: an unrecorded vmsync is a process nothing can later account for.
// This is the deliberate opposite of a best-effort audit hook, and it is why
// the signature returns an error at all -- see the design note in
// docs/design/agent-config.md.
//
// The one exception is the fence path, which attempts this and proceeds
// regardless. Not because the record matters less there, but because the
// alternative -- one workload live in two places writing to two diverging
// copies -- is worse than an audit gap.
func (l *runLog) Append(rec runLogRecord) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	err := l.appendLocked(rec)
	l.writable = err == nil
	l.metrics.setRunLogWritable(l.writable)
	return err
}

func (l *runLog) appendLocked(rec runLogRecord) error {
	if rec.AtUnix == 0 {
		rec.AtUnix = time.Now().Unix()
	}
	rec.Session = l.session

	line, err := json.Marshal(rec)
	if err != nil {
		// Cannot happen for this struct, and is not worth pretending it can
		// be recovered from: returning it means the caller does not launch,
		// which is the correct response to "we cannot describe what we are
		// about to do".
		return fmt.Errorf("encode run log record: %w", err)
	}
	line = append(line, '\n')

	if l.f == nil {
		if err := l.reopenLocked(); err != nil {
			return err
		}
	}
	if l.size+int64(len(line)) > l.rotateAt {
		l.rotateLocked()
	}

	n, err := l.f.Write(line)
	l.size += int64(n)
	if err != nil {
		return fmt.Errorf("append to run log %s: %w", l.path, err)
	}
	// Durable before the caller acts on the answer. Without this, a launch
	// record can sit in the page cache while the process it describes is
	// already running, and a power loss loses exactly the record whose whole
	// purpose is surviving one.
	if err := l.f.Sync(); err != nil {
		return fmt.Errorf("flush run log %s: %w", l.path, err)
	}
	return nil
}

// rotateLocked renames the current generation aside and starts a new one.
//
// Best-effort by design: a rename that fails (a read-only filesystem, say)
// leaves the current file open and appending past the cap, which is strictly
// better than the alternative. A filesystem that cannot rename generally
// cannot append either, so it resolves itself -- and silently discarding the
// audit trail to honour a size limit would be the wrong trade in the one
// situation where the trail matters most.
func (l *runLog) rotateLocked() {
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
	rotated := l.path + fmt.Sprintf(".%d", runLogKept)
	if err := os.Rename(l.path, rotated); err != nil {
		// Reopen the original and keep going past the cap.
		if rerr := l.reopenLocked(); rerr != nil {
			return
		}
		return
	}
	if err := l.reopenLocked(); err != nil {
		return
	}
	// Recorded so a reader of the new generation knows why it starts where it
	// does, and can go looking for the predecessor.
	rec := runLogRecord{Event: runEventRotate, AtUnix: time.Now().Unix(), Session: l.session}
	if line, err := json.Marshal(rec); err == nil {
		line = append(line, '\n')
		if n, err := l.f.Write(line); err == nil {
			l.size += int64(n)
		}
	}
}

// Writable reports the last observed state, for the gauge.
//
// A gauge and not only a counter: a counter that stops incrementing and a host
// with nothing to run look identical, and the difference between them is
// "replication is stopped" versus "replication is idle".
func (l *runLog) Writable() bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.writable
}

func (l *runLog) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// openRun is a launch with no matching exit record, as seen by a LATER agent.
//
// Deliberately carries no Args and no Binary. This file is evidence, never an
// instruction source, and "here are the commands that were interrupted, with
// their argv" is one commit away from "so re-run them" -- the same structural
// refusal LoadCache makes by nilling Operations rather than trusting callers.
type openRun struct {
	RunID         string
	VM            string
	Origin        string
	PriorSession  string
	StartedAtUnix int64
}

// OpenRuns returns launches this file has no exit record for.
//
// NEVER infer liveness from this. "Launch with no exit" equally means "the
// child died and the exit record was lost"; only the run lock answers whether
// something is still running. This exists to give an adopted or abandoned run
// a name and a start time, not to decide anything.
//
// Fences are excluded: -shutdown-domain takes no run lock, so a fence launch
// whose exit record is missing can never be resolved either way and would
// accumulate here as a permanent phantom.
func (l *runLog) OpenRuns() ([]openRun, error) {
	l.mu.Lock()
	path := l.path
	l.mu.Unlock()

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	open := map[string]openRun{}
	sc := bufio.NewScanner(f)
	// Generous, because a launch record carries a whole argv. A line longer
	// than this is not a record we wrote.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var rec runLogRecord
		// A line that does not parse is skipped, not fatal: the last line of
		// an append-only file can be torn by a power loss, and one unreadable
		// record must not cost the rest of the file.
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			continue
		}
		switch rec.Event {
		case runEventLaunch:
			if rec.Origin == runOriginFence || rec.Origin == runOriginProbe {
				continue
			}
			open[rec.RunID] = openRun{
				RunID: rec.RunID, VM: rec.VM, Origin: rec.Origin,
				PriorSession: rec.Session, StartedAtUnix: rec.AtUnix,
			}
		case runEventExit:
			delete(open, rec.RunID)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	out := make([]openRun, 0, len(open))
	for _, r := range open {
		out = append(out, r)
	}
	return out, nil
}

// newRunID returns an id joining a launch record to its exit record.
//
// Same shape as the fence token id: 16 random bytes, hex. Not derived from the
// VM or the time, because two runs for the same VM in the same second must be
// distinguishable in the log.
func newRunID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand does not fail in practice, and a run id is not a
		// security boundary -- it joins two lines in a file. A degraded id
		// beats refusing to launch.
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// --- redaction ---------------------------------------------------------------

// argClass says what an argv element is, so the value after it is handled
// correctly without re-parsing the line.
type argClass int

const (
	argFlagOnly argClass = iota // no value follows
	argValue                    // the next element is its value, safe to log
	argURI                      // the next element is a URL; strip any userinfo
)

// agentFlagVocabulary is every flag the agent is allowed to emit.
//
// An ALLOWLIST, not a denylist, and that direction is the point: a denylist
// fails open, so the day somebody adds a flag carrying a secret, an unfiltered
// log writes it to disk with rotation as the only expiry. This fails closed --
// an unrecognised flag's value is redacted rather than logged.
//
// It is cheap to keep exhaustive precisely because of the property the agent
// already asserts: it owns the flag vocabulary entirely, and the UI never
// names a vmsync flag. Anything here comes from this repo, not the network.
//
// Knowing ARITY is what stops a value from being mistaken for a flag. A
// -promoted-by whose value is the literal string "-ssh-key" (it is free-form
// text from the UI's CreatedBy) would otherwise re-frame everything after it.
var agentFlagVocabulary = map[string]argClass{
	// transport and identity
	"-source-uri":      argURI,
	"-target-uri":      argURI,
	"-source-domain":   argValue,
	"-target-domain":   argValue,
	"-local-host-name": argValue,
	"-run-id":          argValue,
	"-result-json":     argValue,

	// ssh
	"-ssh-user":        argValue,
	"-ssh-key":         argValue, // a PATH; the file is the secret, not this
	"-ssh-port":        argValue,
	"-ssh-known-hosts": argValue,

	// tuning
	"-bridge-helper-path":      argValue,
	"-compress-level":          argValue,
	"-io-depth":                argValue,
	"-prometheus-textfile":     argValue,
	"-reinit-after-failures":   argValue,
	"-retention":               argValue,
	"-source-nbd-port":         argValue,
	"-target-nbd-port":         argValue,
	"-target-disk-path":        argValue,
	"-timestamp-tolerance-sec": argValue,
	"-no-checksum":             argFlagOnly,
	"-use-ssh":                 argFlagOnly,

	// operations
	"-promote":               argFlagOnly,
	"-promote-mode":          argValue,
	"-promoted-by":           argValue,
	"-force-promote":         argFlagOnly,
	"-fence-source":          argFlagOnly,
	"-invert":                argFlagOnly,
	"-reinit":                argFlagOnly,
	"-force-clean":           argFlagOnly,
	"-start":                 argFlagOnly,
	"-update-role":           argValue,
	"-restore-restore-point": argValue,
	"-restored-by":           argValue,
	"-force-restore":         argFlagOnly,

	// fencing
	"-shutdown-domain":      argFlagOnly,
	"-shutdown-timeout-sec": argValue,
	"-read-fence":           argFlagOnly,

	// the "=" forms. vmsync's -compress and -netbuffer implement IsBoolFlag,
	// so they MUST be written as one element; -verify is an ordinary string
	// flag written the same way. Listed here so the left-hand side resolves.
	"-compress":  argValue,
	"-netbuffer": argValue,
	"-verify":    argValue,
}

const argRedacted = "[redacted]"

// uriPasswordPlaceholder is what replaces a password inside a URL, and it is
// deliberately not argRedacted: userinfo is percent-encoded when the URL is
// reassembled, so brackets would come back as %5B...%5D and the log would say
// something no human recognises as "we removed this on purpose".
const uriPasswordPlaceholder = "redacted"

// redactArgs prepares an argv for durable storage.
//
// Two jobs. Strip userinfo from libvirt URIs, because a URL can carry
// user:pass@host and an operator putting a password on a command line is
// exactly the accident a permanent log must not immortalise. And refuse to
// write the value of any flag not in the vocabulary above.
//
// Note what this is NOT doing: the unredacted argv is already in
// /proc/<pid>/cmdline, world-readable on a default Linux. This does not close
// a new hole -- it declines to make a transient exposure durable, archivable,
// and attached to a support ticket.
func redactArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]

		// The "-flag=value" form carries its value inline.
		if name, inline, found := strings.Cut(a, "="); found {
			class, known := agentFlagVocabulary[name]
			switch {
			case !known:
				out = append(out, name+"="+argRedacted)
			case class == argURI:
				out = append(out, name+"="+redactURI(inline))
			default:
				out = append(out, a)
			}
			continue
		}

		class, known := agentFlagVocabulary[a]
		if !known {
			// The flag name itself is code, not data, so it stays -- knowing
			// WHICH unknown flag appeared is what makes this diagnosable. Its
			// value does not.
			out = append(out, a)
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				out = append(out, argRedacted)
				i++
			}
			continue
		}

		out = append(out, a)
		if class == argFlagOnly || i+1 >= len(args) {
			continue
		}
		// Arity is known, so the next element is this flag's value even if it
		// happens to look like a flag itself.
		i++
		if class == argURI {
			out = append(out, redactURI(args[i]))
		} else {
			out = append(out, args[i])
		}
	}
	return out
}

// redactURI removes any userinfo from a URL, and fails CLOSED.
//
// -target-uri is built by interpolating a host into an operator-supplied
// pattern, so it is not guaranteed to parse. A parse failure redacts the whole
// element rather than passing it through: an unparsable string is exactly the
// case where "there is probably no password in it" is a guess.
func redactURI(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return argRedacted
	}
	if u.User == nil {
		return raw
	}
	// The username is kept: it says WHO the connection was made as, which is
	// exactly the sort of thing an incident asks, and it is not the secret.
	u.User = url.UserPassword(u.User.Username(), uriPasswordPlaceholder)
	return u.String()
}
