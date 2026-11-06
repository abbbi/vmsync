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

// The replica file of a disk that stops being replicated.
//
// A pair that copied vdb for months and now excludes it leaves a full-size
// file on the DR host that no later run asks about: every per-disk step
// iterates the disks being copied. Renaming it aside makes it an ordinary
// displaced set, which the sweep reports and -reclaim-leftovers-after removes.
//
// What the cases below are really about is the PATH. Taking it from the
// replica's own previous definition is what makes it exact; the guard that it
// must lie inside the replica's own disk directory is what stops an ignored
// disk's second run acting on a path that belongs to the source host.
package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"testing"
)

// asideRunner answers the existence probe and records every command.
type asideRunner struct {
	cmds []string
	// exists is consulted for the -e probe; missing paths answer "not there".
	exists map[string]bool
	// failProbe and failMove make the two error paths reachable.
	failProbe bool
	failMove  bool
}

func (r *asideRunner) Run(_ context.Context, cmd string) (string, error) {
	r.cmds = append(r.cmds, cmd)
	switch {
	case strings.HasPrefix(cmd, "if [ -e "):
		if r.failProbe {
			return "", errors.New("ssh went away")
		}
		for p, ok := range r.exists {
			if ok && strings.Contains(cmd, "'"+p+"'") {
				return "VMSYNC_PATH_EXISTS\n", nil
			}
		}
		return "VMSYNC_PATH_MISSING\n", nil
	case strings.HasPrefix(cmd, "mv -n "):
		if r.failMove {
			return "mv: cannot move", errors.New("exit status 1")
		}
		return "", nil
	}
	return "", nil
}

func (r *asideRunner) moves() []string {
	var out []string
	for _, c := range r.cmds {
		if strings.HasPrefix(c, "mv -n ") {
			out = append(out, c)
		}
	}
	return out
}

// The replica as the previous sync defined it: both disks under the replica's
// own disk directory.
const replicaXMLBothDisks = `<domain type='kvm'>
  <name>web01</name>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='/data/replicas/web01-vda.qcow2'/>
      <target dev='vda'/>
    </disk>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='/data/replicas/web01-vdb.qcow2'/>
      <target dev='vdb'/>
    </disk>
  </devices>
</domain>`

func TestTheReplicaFileOfANewlyExcludedDiskIsRenamedAside(t *testing.T) {
	r := &asideRunner{exists: map[string]bool{"/data/replicas/web01-vdb.qcow2": true}}
	displaced, err := asideExcludedReplicas(context.Background(), r, replicaXMLBothDisks,
		[]string{"vdb"}, "/data/replicas", "1700000000")
	if err != nil {
		t.Fatalf("asideExcludedReplicas: %v", err)
	}
	if strings.Join(displaced, ",") != "/data/replicas/web01-vdb.qcow2" {
		t.Errorf("displaced = %v, want the ordinary path -- the sweep attributes a set by the base name it sits beside, so without it the file is listed and unattributable", displaced)
	}
	moves := r.moves()
	if len(moves) != 1 {
		t.Fatalf("got %d moves, want 1: %v", len(moves), r.cmds)
	}
	for _, want := range []string{
		"'/data/replicas/web01-vdb.qcow2'",
		"'/data/replicas/web01-vdb.qcow2" + replacedDiskSuffix + "1700000000'",
	} {
		if !strings.Contains(moves[0], want) {
			t.Errorf("the move does not mention %s: %s", want, moves[0])
		}
	}
	// vda is still replicated and must not be touched.
	if strings.Contains(moves[0], "vda") {
		t.Errorf("a disk this run still copies was renamed aside: %s", moves[0])
	}
}

// NEVER DELETED, whatever -replaced-disk-action says. That flag is about the
// disk a -reinit replaces; this is the last replica of a disk somebody has
// just said they manage another way, and the whole point of renaming it is
// that it leaves by -reclaim-leftovers-after rather than now.
func TestANewlyExcludedReplicaIsNeverDeleted(t *testing.T) {
	r := &asideRunner{exists: map[string]bool{"/data/replicas/web01-vdb.qcow2": true}}
	if _, err := asideExcludedReplicas(context.Background(), r, replicaXMLBothDisks,
		[]string{"vdb"}, "/data/replicas", "1700000000"); err != nil {
		t.Fatalf("asideExcludedReplicas: %v", err)
	}
	for _, c := range r.cmds {
		if strings.HasPrefix(c, "rm ") || strings.Contains(c, " rm ") {
			t.Errorf("a remove reached the target: %s", c)
		}
	}
}

// THE LOAD-BEARING GUARD. After one run with -ignore-disk the replica's
// definition names the SOURCE host's path for that disk -- that is the flag's
// own semantics. Without the directory check, a second run would rename a file
// on the TARGET that merely happens to sit at the same path as something on
// the source.
func TestAPathOutsideTheReplicasDiskDirectoryIsLeftAlone(t *testing.T) {
	const afterOneIgnoredRun = `<domain type='kvm'>
  <name>web01</name>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='/data/replicas/web01-vda.qcow2'/>
      <target dev='vda'/>
    </disk>
    <disk type='file' device='disk'>
      <driver name='qemu' type='raw'/>
      <source file='/vm/web01-vdb.raw'/>
      <target dev='vdb'/>
    </disk>
  </devices>
</domain>`
	// The file DOES exist at that path on the target, which is exactly the
	// case the guard is for: without it, this would be renamed.
	r := &asideRunner{exists: map[string]bool{"/vm/web01-vdb.raw": true}}
	displaced, err := asideExcludedReplicas(context.Background(), r, afterOneIgnoredRun,
		[]string{"vdb"}, "/data/replicas", "1700000000")
	if err != nil {
		t.Fatalf("asideExcludedReplicas: %v", err)
	}
	if len(displaced) != 0 {
		t.Errorf("displaced = %v for a path outside the replica's own disk directory", displaced)
	}
	if moves := r.moves(); len(moves) != 0 {
		t.Errorf("a file outside the replica's disk directory was renamed: %v", moves)
	}
}

// After one run with -drop-disk the device is gone from the definition
// entirely, so there is nothing to look up and nothing to do. The second run
// must be a no-op rather than an error.
func TestADeviceAbsentFromTheDefinitionIsANoOp(t *testing.T) {
	const afterOneDroppedRun = `<domain type='kvm'>
  <name>web01</name>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='/data/replicas/web01-vda.qcow2'/>
      <target dev='vda'/>
    </disk>
  </devices>
</domain>`
	r := &asideRunner{}
	displaced, err := asideExcludedReplicas(context.Background(), r, afterOneDroppedRun,
		[]string{"vdb"}, "/data/replicas", "1700000000")
	if err != nil {
		t.Fatalf("a second dropped run errored: %v", err)
	}
	if len(displaced) != 0 || len(r.cmds) != 0 {
		t.Errorf("displaced=%v cmds=%v; a device the definition does not name needs no probe at all", displaced, r.cmds)
	}
}

// A disk excluded from the very first sync has no replica file, so the probe
// answers "not there" and nothing is renamed.
func TestADiskThatWasNeverReplicatedIsANoOp(t *testing.T) {
	r := &asideRunner{exists: map[string]bool{}}
	displaced, err := asideExcludedReplicas(context.Background(), r, replicaXMLBothDisks,
		[]string{"vdb"}, "/data/replicas", "1700000000")
	if err != nil {
		t.Fatalf("asideExcludedReplicas: %v", err)
	}
	if len(displaced) != 0 {
		t.Errorf("displaced = %v with no file on the target", displaced)
	}
	if moves := r.moves(); len(moves) != 0 {
		t.Errorf("a file that does not exist was renamed: %v", moves)
	}
}

// Idempotent across runs: once the file has been renamed aside, the ordinary
// name is gone, so the next run finds nothing and does nothing. Without that,
// every run would make another aside of the same disk.
func TestASecondRunMakesNoSecondAside(t *testing.T) {
	r := &asideRunner{exists: map[string]bool{"/data/replicas/web01-vdb.qcow2": true}}
	if _, err := asideExcludedReplicas(context.Background(), r, replicaXMLBothDisks,
		[]string{"vdb"}, "/data/replicas", "1700000000"); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// The rename happened, so the ordinary name no longer exists.
	r.exists["/data/replicas/web01-vdb.qcow2"] = false
	r.cmds = nil
	displaced, err := asideExcludedReplicas(context.Background(), r, replicaXMLBothDisks,
		[]string{"vdb"}, "/data/replicas", "1700000001")
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(displaced) != 0 {
		t.Errorf("the second run displaced %v; the file was already moved", displaced)
	}
	if moves := r.moves(); len(moves) != 0 {
		t.Errorf("the second run made another aside: %v", moves)
	}
}

// FAIL CLOSED on an inconclusive probe, for the reason RemotePathExists itself
// gives: a transient SSH failure read as "the path is not there" would leave
// the orphan unreported for ever, and the run has already proved it can reach
// the target.
func TestAnInconclusiveProbeFailsTheRun(t *testing.T) {
	r := &asideRunner{failProbe: true, exists: map[string]bool{"/data/replicas/web01-vdb.qcow2": true}}
	if _, err := asideExcludedReplicas(context.Background(), r, replicaXMLBothDisks,
		[]string{"vdb"}, "/data/replicas", "1700000000"); err == nil {
		t.Error("a failed existence probe was treated as 'no file there'")
	}
}

func TestAFailedRenameFailsTheRun(t *testing.T) {
	r := &asideRunner{failMove: true, exists: map[string]bool{"/data/replicas/web01-vdb.qcow2": true}}
	_, err := asideExcludedReplicas(context.Background(), r, replicaXMLBothDisks,
		[]string{"vdb"}, "/data/replicas", "1700000000")
	if err == nil {
		t.Fatal("a failed rename was ignored; the file would stay unreported and the run would claim success")
	}
	// -n means an existing aside is never clobbered, so this is also the path
	// a deliberately-kept set takes. The message has to name both ends.
	for _, want := range []string{"vdb", "/data/replicas/web01-vdb.qcow2", replacedDiskSuffix} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

// Every ordinary pair takes this path, so it must not touch the target at all
// -- and must not SAY anything either.
//
// The quiet half is what the early return is really for, and a mutation
// proved it: without the guard, a first-ever sync of a pair that excludes a
// disk hands an empty document to the parser, which fails, and every such run
// warns that it could not read a definition that does not exist yet. Nothing
// is renamed either way, so a test that only counted commands passed on the
// mutant.
func TestNoExclusionsMeansNoTargetCommandsAndNoWarning(t *testing.T) {
	r := &asideRunner{}
	for _, c := range []struct {
		name     string
		excluded []string
		xml      string
	}{
		{"no exclusions at all", nil, replicaXMLBothDisks},
		{"a first-ever sync, so no replica to read", []string{"vdb"}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := captureAsideTrace(t)
			r.cmds = nil
			displaced, err := asideExcludedReplicas(context.Background(), r, c.xml, c.excluded, "/data/replicas", "1700000000")
			if err != nil {
				t.Fatalf("asideExcludedReplicas: %v", err)
			}
			if len(displaced) != 0 || len(r.cmds) != 0 {
				t.Errorf("displaced=%v cmds=%v", displaced, r.cmds)
			}
			if strings.Contains(out.String(), "WARNING") {
				t.Errorf("an ordinary run warned about the replica's definition; a warning on every first sync is one operators learn to ignore:\n%s", out.String())
			}
		})
	}
}

// captureAsideTrace diverts the trace package's output into a buffer for one
// test, with colour off so assertions read plain level names.
func captureAsideTrace(t *testing.T) *bytes.Buffer {
	t.Helper()
	t.Setenv("NO_COLOR", "1")
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

// THE BUG A LIVE RUN FOUND. The aside was reported on the run that made it and
// never again: the sweep attributes a displaced set by a base name this
// replica owns, and after the rename that base is in neither the definition
// nor the disk list. Reported once and then lost is one invisible orphan
// turned into another with a different name -- and one -reclaim-leftovers-after
// could never reap, because it only removes what the sweep attributed.
func TestTheExcludedDisksCandidatePathsAreAvailableOnEveryRun(t *testing.T) {
	s := diskSelection{
		domain: "web01",
		all:    []string{"vda", "vdb", "sdc"},
		sourceByDev: map[string]string{
			"vda": "/vm/web01-vda.qcow2",
			"vdb": "/vm/web01-vdb.qcow2",
			"sdc": "/vm/web01-sdc.raw",
		},
	}
	s.ignore, s.drop = sets("vdb", "sdc")

	got := s.candidateReplicaPaths("/data/replicas")
	want := "/data/replicas/web01-sdc.raw,/data/replicas/web01-vdb.qcow2"
	if strings.Join(got, ",") != want {
		t.Errorf("candidateReplicaPaths = %v, want %s -- these are the base names the sweep needs to attribute the aside to this replica", got, want)
	}
	// A disk this run still copies must not be in it: it is already in
	// targetDiskPaths, and a duplicate would be harmless but misleading.
	for _, p := range got {
		if strings.Contains(p, "vda") {
			t.Errorf("a replicated disk appeared among the candidates: %s", p)
		}
	}
	// With no -target-disk-path the replica keeps the source's own directory,
	// which is what SetTargetPath does and what the paths must agree with.
	if got := s.candidateReplicaPaths(""); strings.Join(got, ",") != "/vm/web01-sdc.raw,/vm/web01-vdb.qcow2" {
		t.Errorf("candidateReplicaPaths(\"\") = %v, want the source's own directory", got)
	}
}

// A dev the source XML has no path for -- a block or network disk -- yields no
// candidate rather than an empty-stringed one, which would make the sweep
// treat "" as a base name.
func TestADeviceWithNoSourcePathYieldsNoCandidate(t *testing.T) {
	s := diskSelection{domain: "web01", all: []string{"vda", "sdc"}, sourceByDev: map[string]string{"vda": "/vm/a.qcow2"}}
	s.ignore, s.drop = sets("sdc", "")
	if got := s.candidateReplicaPaths("/data/replicas"); len(got) != 0 {
		t.Errorf("candidateReplicaPaths = %v for a device with no <source file=>", got)
	}
}

// Several at once, each under the same stamp, so one suffix identifies what
// one run displaced -- the property the reinit aside relies on too.
// And the wiring, because every case above would pass with run() still only
// handing the candidates to the sweep on the run that happened to rename
// something -- which is exactly the bug. Source-level, since the alternative
// is a live two-host run.
func TestRunGivesTheSweepTheCandidatePathsUnconditionally(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	body := string(src)
	if !strings.Contains(body, "sweepDiskPaths = append(append([]string{}, targetDiskPaths...), selection.candidateReplicaPaths(cfg.TargetDiskPath)...)") {
		t.Error("run() no longer builds the sweep's paths from candidateReplicaPaths; a displaced set made by an earlier run would be listed and attributed to nobody, and never reaped")
	}
	if !strings.Contains(body, "sweepDisplacedSets(ctx, cfg, targetSSHClient, replicaIncompleteAtStart, incompleteErr, sweepDiskPaths, time.Now())") {
		t.Error("the displaced-set sweep is no longer given sweepDiskPaths, so the excluded disks' base names never reach it")
	}
	// The restore-point store must NOT get them: a disk this replica does not
	// have would start acquiring restore points.
	if strings.Contains(body, "newRestorePoints(ctx, cfg.RetentionPolicy, targetSSHClient, sweepDiskPaths") {
		t.Error("the restore-point store was given the sweep's path list; it must only ever see the disks this replica actually has")
	}
}

func TestSeveralExcludedDisksShareOneStamp(t *testing.T) {
	const three = `<domain type='kvm'>
  <name>web01</name>
  <devices>
    <disk type='file' device='disk'><driver name='qemu' type='qcow2'/><source file='/data/replicas/a.qcow2'/><target dev='vda'/></disk>
    <disk type='file' device='disk'><driver name='qemu' type='qcow2'/><source file='/data/replicas/b.qcow2'/><target dev='vdb'/></disk>
    <disk type='file' device='disk'><driver name='qemu' type='qcow2'/><source file='/data/replicas/c.qcow2'/><target dev='vdc'/></disk>
  </devices>
</domain>`
	r := &asideRunner{exists: map[string]bool{
		"/data/replicas/b.qcow2": true,
		"/data/replicas/c.qcow2": true,
	}}
	displaced, err := asideExcludedReplicas(context.Background(), r, three,
		[]string{"vdb", "vdc"}, "/data/replicas", "1700000000")
	if err != nil {
		t.Fatalf("asideExcludedReplicas: %v", err)
	}
	if len(displaced) != 2 {
		t.Fatalf("displaced = %v, want both", displaced)
	}
	for _, m := range r.moves() {
		if !strings.Contains(m, replacedDiskSuffix+"1700000000'") {
			t.Errorf("a move used a different stamp, so one suffix no longer identifies this run's set: %s", m)
		}
	}
}
