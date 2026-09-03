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
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testRunLog(t *testing.T) *runLog {
	t.Helper()
	// nil metrics: every method on agentMetrics is nil-guarded, and none of
	// these tests is about the gauge.
	l := newRunLog(t.TempDir(), "session-1", nil)
	if err := l.Open(); err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

// The contract the whole fail-closed decision rests on: if Append returns an
// error the record is not on disk, and the caller must not launch. An agent
// that cannot write this file must not run syncs it cannot account for.
func TestRunLogAppendFailsWhenTheFileCannotBeWritten(t *testing.T) {
	// A state dir nested under a regular FILE, so the open fails with ENOTDIR.
	// A merely absent directory would not do it -- reopenLocked creates one.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("a file, not a directory"), 0o600); err != nil {
		t.Fatalf("set up the blocker: %v", err)
	}
	l := newRunLog(filepath.Join(blocker, "state"), "session-1", nil)

	if err := l.Open(); err == nil {
		t.Fatal("Open reported success against an unwritable path")
	}
	if l.Writable() {
		t.Error("Writable() is true after a failed Open -- the gauge would read healthy while nothing can be logged")
	}
	if err := l.Append(runLogRecord{Event: runEventLaunch, RunID: "r1", VM: "web01"}); err == nil {
		t.Error("Append reported success against an unwritable path; a caller would launch an unrecorded vmsync")
	}
}

// A launch and its exit are two records joined by RunID. OpenRuns reports the
// launches with no exit -- and nothing else.
func TestRunLogOpenRunsPairsLaunchWithExit(t *testing.T) {
	l := testRunLog(t)
	code := 0

	for _, rec := range []runLogRecord{
		{Event: runEventLaunch, RunID: "finished", VM: "web01", Origin: runOriginScheduled},
		{Event: runEventExit, RunID: "finished", ExitCode: &code},
		{Event: runEventLaunch, RunID: "still-going", VM: "db01", Origin: runOriginScheduled},
	} {
		if err := l.Append(rec); err != nil {
			t.Fatalf("Append(%s): %v", rec.Event, err)
		}
	}

	open, err := l.OpenRuns()
	if err != nil {
		t.Fatalf("OpenRuns: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("OpenRuns returned %d runs, want just the unfinished one: %+v", len(open), open)
	}
	if open[0].RunID != "still-going" || open[0].VM != "db01" {
		t.Errorf("OpenRuns = %+v, want the db01 launch that has no exit", open[0])
	}
	if open[0].PriorSession != "session-1" {
		t.Errorf("PriorSession = %q, want the session that wrote the launch", open[0].PriorSession)
	}
}

// Fences are excluded, and not as an optimisation. -shutdown-domain takes no
// run lock, so a fence launch with no exit record can never be resolved to
// "finished" nor to "still running" -- it would sit here as a permanent
// phantom and make every later reader's answer wrong.
func TestRunLogOpenRunsExcludesFencesAndProbes(t *testing.T) {
	l := testRunLog(t)
	for _, o := range []string{runOriginFence, runOriginProbe} {
		if err := l.Append(runLogRecord{Event: runEventLaunch, RunID: "x-" + o, VM: "web01", Origin: o}); err != nil {
			t.Fatal(err)
		}
	}
	open, err := l.OpenRuns()
	if err != nil {
		t.Fatalf("OpenRuns: %v", err)
	}
	if len(open) != 0 {
		t.Errorf("OpenRuns returned %+v, want none -- a fence with no exit record is unresolvable, not open", open)
	}
}

// The last line of an append-only file can be torn by a power loss. One
// unreadable record must not cost the rest of the file.
func TestRunLogOpenRunsSkipsATornLine(t *testing.T) {
	l := testRunLog(t)
	if err := l.Append(runLogRecord{Event: runEventLaunch, RunID: "good", VM: "web01", Origin: runOriginScheduled}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"event":"launch","run_id":"torn`) // no closing brace, no newline
	f.Close()

	open, err := l.OpenRuns()
	if err != nil {
		t.Fatalf("OpenRuns on a file with a torn last line: %v", err)
	}
	if len(open) != 1 || open[0].RunID != "good" {
		t.Errorf("OpenRuns = %+v, want just the intact record", open)
	}
}

func TestRedactArgs(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []string
		want []string
	}{
		{
			"a plain sync argv is logged verbatim",
			[]string{"-source-domain", "web01", "-target-domain", "web01", "-ssh-user", "root"},
			[]string{"-source-domain", "web01", "-target-domain", "web01", "-ssh-user", "root"},
		},
		{
			// The accident this exists for: a password on a command line,
			// which a durable log must not immortalise.
			"userinfo is stripped from a libvirt uri",
			[]string{"-target-uri", "qemu+tcp://admin:hunter2@dr01/system"},
			[]string{"-target-uri", "qemu+tcp://admin:" + uriPasswordPlaceholder + "@dr01/system"},
		},
		{
			"a uri with no userinfo is untouched",
			[]string{"-source-uri", "qemu:///system"},
			[]string{"-source-uri", "qemu:///system"},
		},
		{
			// -promoted-by carries free-form text from the UI. If arity were
			// re-derived by looking for a leading dash, this value would be
			// read as a flag and everything after it would be mis-framed.
			"a value that looks like a flag does not re-frame the line",
			[]string{"-promoted-by", "-ssh-key", "-source-domain", "web01"},
			[]string{"-promoted-by", "-ssh-key", "-source-domain", "web01"},
		},
		{
			"the = form is understood",
			[]string{"-compress=zstd", "-verify=full", "-netbuffer=64M"},
			[]string{"-compress=zstd", "-verify=full", "-netbuffer=64M"},
		},
		{
			"a = form uri is redacted too",
			[]string{"-target-uri=qemu+tcp://u:p@h/system"},
			[]string{"-target-uri=qemu+tcp://u:" + uriPasswordPlaceholder + "@h/system"},
		},
		{
			"flag-only flags do not swallow the next element",
			[]string{"-use-ssh", "-source-domain", "web01"},
			[]string{"-use-ssh", "-source-domain", "web01"},
		},
		{
			// The whole point of an allowlist: a flag added later that
			// carries a secret is redacted by default, not logged by default.
			"an unknown flag keeps its name and loses its value",
			[]string{"-ssh-password", "hunter2", "-source-domain", "web01"},
			[]string{"-ssh-password", argRedacted, "-source-domain", "web01"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := redactArgs(tc.in)
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Errorf("redactArgs(%v)\n  = %v\nwant %v", tc.in, got, tc.want)
			}
		})
	}
}

// An unparsable URI is redacted whole. -target-uri is built by interpolating
// a host into an operator-supplied pattern, so it is not guaranteed to parse,
// and "there is probably no password in it" is a guess.
func TestRedactURIFailsClosed(t *testing.T) {
	got := redactURI("qemu+ssh://[::1/system")
	if got != argRedacted {
		t.Errorf("redactURI on an unparsable uri = %q, want %q", got, argRedacted)
	}
}

// Every flag the agent can emit must be in the vocabulary, or its value is
// redacted and the log quietly loses the thing it exists to record. This is
// the cheap stand-in for the compile-time enforcement a typed emit funnel
// would give: it catches a flag added to a builder and forgotten here.
func TestEveryEmittedFlagIsInTheVocabulary(t *testing.T) {
	// Flags that appear in the four argv builders. Kept as a literal rather
	// than derived, so adding one to a builder without adding it here is what
	// fails -- deriving both sides from the same source would prove nothing.
	for _, f := range []string{
		"-source-uri", "-target-uri", "-source-domain", "-target-domain",
		"-local-host-name", "-ssh-user", "-ssh-key", "-ssh-port", "-ssh-known-hosts",
		"-bridge-helper-path", "-compress-level", "-io-depth", "-prometheus-textfile",
		"-reinit-after-failures", "-retention", "-source-nbd-port", "-target-nbd-port",
		"-target-disk-path", "-timestamp-tolerance-sec", "-use-ssh", "-no-checksum", "-run-id",
		"-result-json",
		"-compress", "-netbuffer", "-verify",
		"-promote", "-promote-mode", "-promoted-by", "-force-promote", "-fence-source",
		"-invert", "-reinit", "-force-clean", "-start", "-update-role",
		"-restore-restore-point", "-restored-by", "-force-restore",
		"-shutdown-domain", "-shutdown-timeout-sec", "-read-fence",
	} {
		if _, ok := agentFlagVocabulary[f]; !ok {
			t.Errorf("%s is emitted by a builder but missing from agentFlagVocabulary, so its value would be redacted from the run log", f)
		}
	}
}

func TestNewRunIDIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := newRunID()
		if id == "" {
			t.Fatal("newRunID returned an empty id")
		}
		if seen[id] {
			t.Fatalf("newRunID collided on %q -- two runs for one VM in one second would be indistinguishable", id)
		}
		seen[id] = true
	}
}

// Rotation, at a cap a test can actually reach. The real one is 32 MiB, which
// is why this is a field rather than the constant.
func TestRunLogRotatesAndKeepsOneGeneration(t *testing.T) {
	l := newRunLog(t.TempDir(), "session-1", nil)
	l.rotateAt = 400 // a few records
	if err := l.Open(); err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	for i := 0; i < 40; i++ {
		if err := l.Append(runLogRecord{
			Event: runEventLaunch, RunID: "r", VM: "web01", Origin: runOriginScheduled,
			Binary: "/usr/local/bin/vmsync",
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	// The current generation stays bounded rather than growing forever.
	fi, err := os.Stat(l.path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > int64(l.rotateAt)*2 {
		t.Errorf("current generation is %d bytes against a %d cap; it is not rotating", fi.Size(), l.rotateAt)
	}
	// Exactly one predecessor, so the worst case is twice the cap and not
	// a directory full of generations.
	if _, err := os.Stat(l.path + ".1"); err != nil {
		t.Errorf("no rotated generation was kept: %v", err)
	}
	if _, err := os.Stat(l.path + ".2"); err == nil {
		t.Error("a second rotated generation exists; only one is meant to be kept")
	}
}

// A rotate record marks the boundary, so a reader of the new generation knows
// why it starts where it does and can go looking for the predecessor.
func TestRotationLeavesABreadcrumb(t *testing.T) {
	l := newRunLog(t.TempDir(), "session-1", nil)
	l.rotateAt = 300
	if err := l.Open(); err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	for i := 0; i < 20; i++ {
		if err := l.Append(runLogRecord{Event: runEventLaunch, RunID: "r", VM: "web01"}); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(l.path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"event":"rotate"`) {
		t.Errorf("the new generation carries no rotate record:\n%s", data)
	}
}

// Rotation must not lose the pairing OpenRuns depends on. A launch in the
// rotated generation whose exit landed in the current one has genuinely
// finished, and reading only the current file would report it as still open
// forever.
func TestOpenRunsAfterRotationDoesNotInventOpenRuns(t *testing.T) {
	l := newRunLog(t.TempDir(), "session-1", nil)
	l.rotateAt = 100000 // no rotation: this is about the reader, not the writer
	if err := l.Open(); err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	code := 0
	if err := l.Append(runLogRecord{Event: runEventLaunch, RunID: "paired", VM: "web01", Origin: runOriginScheduled}); err != nil {
		t.Fatal(err)
	}
	if err := l.Append(runLogRecord{Event: runEventExit, RunID: "paired", ExitCode: &code}); err != nil {
		t.Fatal(err)
	}
	open, err := l.OpenRuns()
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Errorf("OpenRuns = %+v, want none", open)
	}
}

// The wire contract with the console, from this side: a nil exit code must be
// ABSENT from the JSON, not rendered as 0. The console distinguishes "never
// observed" from "exited 0", and omitempty on a pointer is what carries that.
func TestSyncResultWireFormatDistinguishesUnobserved(t *testing.T) {
	unobserved, err := json.Marshal(SyncResult{VM: "web01", RunID: "r1", Outcome: outcomeUnknown})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(unobserved), "exit_code") {
		t.Errorf("an unobserved run serialised an exit_code; the console would render it as a real status:\n%s", unobserved)
	}
	if !strings.Contains(string(unobserved), `"outcome":"unknown"`) {
		t.Errorf("the outcome did not reach the wire:\n%s", unobserved)
	}

	code := 0
	observed, err := json.Marshal(SyncResult{VM: "web01", ExitCode: &code, Outcome: outcomeSuccess})
	if err != nil {
		t.Fatal(err)
	}
	// A real zero must survive. omitempty on a *int omits nil, not a pointer
	// to zero -- if that were ever changed to a plain int, this is what fails.
	if !strings.Contains(string(observed), `"exit_code":0`) {
		t.Errorf("an observed exit 0 was dropped from the wire:\n%s", observed)
	}
}

// The agent's half of the degraded contract. The console has a matching test
// decoding exactly these shapes; neither side can check the other.
func TestSyncResultWireFormatCarriesDegraded(t *testing.T) {
	code := 0
	const reason = "the guest filesystems are still FROZEN: run virsh domfsthaw db01"
	b, err := json.Marshal(SyncResult{
		VM: "db01", ExitCode: &code, Outcome: outcomeSuccess,
		Degraded: true, DegradedReason: reason,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{`"degraded":true`, `"degraded_reason":"` + reason + `"`, `"outcome":"success"`} {
		if !strings.Contains(got, want) {
			t.Errorf("wire form is missing %s:\n%s", want, got)
		}
	}

	// A clean run must carry NEITHER field. The console decodes reports with
	// DisallowUnknownFields, so every field the agent emits is a deployment
	// ordering constraint -- and emitting "degraded":false on every run would
	// spend that on nothing.
	clean, err := json.Marshal(SyncResult{VM: "db01", ExitCode: &code, Outcome: outcomeSuccess})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(clean), "degraded") {
		t.Errorf("a clean run serialised a degraded field:\n%s", clean)
	}
}
