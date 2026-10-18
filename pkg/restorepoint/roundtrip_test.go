//go:build unix

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

package restorepoint

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// THE COMMAND AND THE PARSER, CHECKED AGAINST EACH OTHER.
//
// Everything else in this package asserts on the command as a STRING and
// feeds the parser a hand-written stream, so the two halves were never
// compared. Deleting the line that emits the opening marker left the entire
// 23-package suite green -- and on a real target ParseAllStatus would then
// return its "did not begin with" error on every cycle, publishInstants would
// always fail, and no restore point would ever be deleted again, on any
// target, for any domain. The suite could not see it because no test ran one
// side's output through the other.
//
// Guarded by build tag rather than by a runtime skip: this package is
// deliberately buildable and testable on any OS -- it is why shQuote is
// duplicated here instead of imported from pkg/util -- and only this one file
// needs a POSIX shell.
func runSidecarRoundTrip(t *testing.T, base string, specs []struct {
	name    string
	takenAt int64
}) (map[string]time.Time, error, []Tag) {
	t.Helper()
	store, err := StoreForDir(base, "web01")
	if err != nil {
		t.Fatalf("StoreForDir(%q): %v", base, err)
	}
	root, err := store.Path()
	if err != nil {
		t.Fatalf("store path: %v", err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", root, err)
	}

	var tags []Tag
	for _, sp := range specs {
		dir := filepath.Join(root, sp.name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		tag, err := ParseTag(sp.name)
		if err != nil {
			t.Fatalf("ParseTag(%q): %v", sp.name, err)
		}
		tags = append(tags, tag)
		if sp.takenAt == 0 {
			continue // a point with no sidecar at all
		}
		b, err := Status{Checkpoint: tag.Checkpoint, CheckpointAt: tag.At.Unix(), TakenAt: sp.takenAt}.Encode()
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, StatusName), b, 0o644); err != nil {
			t.Fatalf("write sidecar: %v", err)
		}
	}

	cmd, err := ReadAllStatusCommand(store, tags)
	if err != nil {
		t.Fatalf("ReadAllStatusCommand: %v", err)
	}
	out, runErr := exec.Command("sh", "-c", cmd).CombinedOutput()
	if runErr != nil {
		t.Fatalf("sh refused the command (%v): %s\noutput: %s", runErr, cmd, out)
	}
	got, perr := ParseAllStatus(string(out), tags)
	return got, perr, tags
}

func TestTheCommandAndTheParserAgreeAgainstARealShell(t *testing.T) {
	specs := []struct {
		name    string
		takenAt int64
	}{
		{"1700000000-vmsync-cpt-000001", 1700000120},
		{"1700003600-vmsync-cpt-000002", 0}, // no sidecar: a record with an empty body
		{"1700007200-vmsync-cpt-000003", 1700007300},
	}

	got, err, _ := runSidecarRoundTrip(t, t.TempDir(), specs)
	if err != nil {
		t.Fatalf("ParseAllStatus could not read what ReadAllStatusCommand produced: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("parsed %v, want the two points that have a sidecar", got)
	}
	if at := got["1700000000-vmsync-cpt-000001"]; !at.Equal(time.Unix(1700000120, 0).UTC()) {
		t.Errorf("first point published at %v", at)
	}
	if at := got["1700007200-vmsync-cpt-000003"]; !at.Equal(time.Unix(1700007300, 0).UTC()) {
		t.Errorf("third point published at %v", at)
	}
	// The point with no sidecar is ABSENT, not zero: a zero would read as
	// 1970 and make it the first thing the next prune deletes.
	if _, ok := got["1700003600-vmsync-cpt-000002"]; ok {
		t.Errorf("a point with no sidecar was given an instant: %v", got)
	}
}

// A STORE PATH CANNOT BREAK THE FRAMING, proven through the shell rather than
// by reading the command.
//
// The framing is newline-delimited, and an earlier version put the filesystem
// path on the record header via a shell glob. Nothing rejects a newline in
// -target-disk-path or -target-domain, so one newline split every header,
// every record was dropped, and ParseAllStatus returned an empty map and NO
// error -- which the caller read as "no point has a publish instant" and
// retention silently fell back to the checkpoint instant.
func TestARealShellSurvivesAHostileStorePath(t *testing.T) {
	specs := []struct {
		name    string
		takenAt int64
	}{
		{"1700000000-vmsync-cpt-000001", 1700000120},
		{"1700003600-vmsync-cpt-000002", 1700003700},
	}

	for _, suffix := range []string{
		"we\nird",
		"we\n" + markerStatusPoint + " 1700009999-vmsync-cpt-000099",
		"it's quoted",
		"percent %s here",
		"back\\slash",
		"dollar $HOME",
	} {
		base := filepath.Join(t.TempDir(), suffix)
		if err := os.MkdirAll(base, 0o755); err != nil {
			t.Fatalf("mkdir %q: %v", base, err)
		}
		got, err, _ := runSidecarRoundTrip(t, base, specs)
		if err != nil {
			t.Errorf("store path %q: ParseAllStatus failed: %v", suffix, err)
			continue
		}
		if len(got) != 2 {
			t.Errorf("store path %q: parsed %v, want both points. A path must not be able to decide where one record ends and the next begins", suffix, got)
		}
	}
}

// A sidecar whose BODY carries the framing markers must not be able to speak
// for another point. Through the shell, because the body reaches the stream
// byte for byte -- nothing escapes it on the way.
func TestARealShellCannotBeTalkedIntoAForgedRecord(t *testing.T) {
	base := t.TempDir()
	store, err := StoreForDir(base, "web01")
	if err != nil {
		t.Fatalf("StoreForDir: %v", err)
	}
	root, _ := store.Path()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	names := []string{"1700000000-vmsync-cpt-000001", "1700003600-vmsync-cpt-000002"}
	var tags []Tag
	for _, n := range names {
		if err := os.MkdirAll(filepath.Join(root, n), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		tag, err := ParseTag(n)
		if err != nil {
			t.Fatalf("ParseTag: %v", err)
		}
		tags = append(tags, tag)
	}

	// The OLDEST point is read first, so its body is the one that can speak
	// for the points after it. It claims its neighbour was published in 2096.
	forged := strings.Join([]string{
		`{"taken_at":1700000120}`,
		markerStatusEnd,
		markerStatusPoint + " 1700003600-vmsync-cpt-000002",
		`{"taken_at":4000000000}`,
	}, "\n")
	if err := os.WriteFile(filepath.Join(root, names[0], StatusName), []byte(forged), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	b, _ := Status{TakenAt: 1700003700}.Encode()
	if err := os.WriteFile(filepath.Join(root, names[1], StatusName), b, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cmd, err := ReadAllStatusCommand(store, tags)
	if err != nil {
		t.Fatalf("ReadAllStatusCommand: %v", err)
	}
	out, _ := exec.Command("sh", "-c", cmd).CombinedOutput()
	got, perr := ParseAllStatus(string(out), tags)

	if perr == nil {
		if at := got["1700003600-vmsync-cpt-000002"]; at.Equal(time.Unix(4000000000, 0).UTC()) {
			t.Fatalf("a sidecar forged its neighbour's publish instant: %v", got)
		}
		t.Errorf("the forged stream was accepted and returned %v; an injected record shifts every record after it, so the only honest answer is to refuse the read", got)
	}
}
