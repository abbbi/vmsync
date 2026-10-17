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
	"fmt"
	"strings"
	"testing"

	"vmsync/pkg/restorepoint"
)

// checkRestoreIdentity is the only thing standing between a mistyped flag and
// a live domain's disks, so it is asserted exhaustively rather than by
// example. It is reachable without libvirt -- it takes a plan and a config,
// and its only dependency is parsing a domain XML string -- which is why it
// was written as a separate function from checkRestoreTargetState in the first
// place.

const restoreTestDomXML = `<domain type='kvm'>
  <name>web01</name>
  <uuid>12345678-1234-1234-1234-123456789abc</uuid>
  <metadata>
    <vmsync:vmsync xmlns:vmsync="http://vmsync.org/xmlns/libvirt/domain/1.0">
      <replica_source id="srchost:web01"/>
    </vmsync:vmsync>
  </metadata>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='/data/replicas/web01.qcow2'/>
      <target dev='vda' bus='virtio'/>
    </disk>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='/data/replicas/web01-data.qcow2'/>
      <target dev='vdb' bus='virtio'/>
    </disk>
  </devices>
</domain>`

func restoreTestCfg() syncConfig {
	return syncConfig{
		TargetURI:      "qemu+ssh://tgthost/system",
		TargetDomain:   "web01",
		TargetDiskPath: "/data/replicas",
	}
}

func restoreTestPlan(t *testing.T) restorePlan {
	t.Helper()
	tag, err := restorepoint.ParseTag("1756041600-vmsync-cpt-000042")
	if err != nil {
		t.Fatalf("ParseTag: %v", err)
	}
	return restorePlan{
		tag:           tag,
		domXML:        restoreTestDomXML,
		replicaSource: "srchost:web01",
		disks: []string{
			"/data/replicas/web01.qcow2",
			"/data/replicas/web01-data.qcow2",
		},
		status: restorepoint.Status{
			Checkpoint:   "vmsync-cpt-000042",
			CheckpointAt: 1756041000,
			TakenAt:      1756041600,
			Source:       "srchost:web01",
			Verify:       restorepoint.VerifyPassed,
			Disks:        []string{"web01.qcow2", "web01-data.qcow2"},
		},
	}
}

func TestCheckRestoreIdentityAcceptsAGenuinePair(t *testing.T) {
	if err := checkRestoreIdentity(restoreTestCfg(), restoreTestPlan(t)); err != nil {
		t.Fatalf("a matching domain, disk set and provenance was refused: %v", err)
	}
}

// The crossed-flags case. -target-domain and -target-disk-path are independent
// and nothing else binds them: point them at two different replicas and one
// machine's disks are rolled back while the OTHER's metadata is rewritten and
// paused -- leaving the first with contents older than its metadata claims,
// which is precisely the state the next incremental sync cannot detect.
func TestCheckRestoreIdentityRefusesDisksTheDomainDoesNotOwn(t *testing.T) {
	plan := restoreTestPlan(t)
	cfg := restoreTestCfg()
	cfg.TargetDiskPath = "/data/other-replicas"
	plan.disks = []string{"/data/other-replicas/web01.qcow2", "/data/other-replicas/web01-data.qcow2"}

	err := checkRestoreIdentity(cfg, plan)
	if err == nil {
		t.Fatal("a restore was allowed to replace files the target domain does not refer to")
	}
	// The message has to name both sides: an operator who crossed two flags
	// cannot fix it from "paths do not match".
	for _, want := range []string{"/data/other-replicas/web01.qcow2", "/data/replicas/web01.qcow2", "web01"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err.Error(), want)
		}
	}
}

func TestCheckRestoreIdentityRefusesOneStrayDiskAmongSeveral(t *testing.T) {
	// The partial case matters more than the wholly-wrong one: a restore that
	// replaced two of three correctly and one from somewhere else would
	// assemble a machine out of two hosts.
	plan := restoreTestPlan(t)
	plan.disks[1] = "/data/replicas/somebody-elses.qcow2"
	if err := checkRestoreIdentity(restoreTestCfg(), plan); err == nil {
		t.Fatal("a restore was allowed with one disk the domain does not refer to")
	}
}

func TestCheckRestoreIdentityRefusesAnotherPairsRestorePoint(t *testing.T) {
	plan := restoreTestPlan(t)
	plan.status.Source = "otherhost:web01"

	err := checkRestoreIdentity(restoreTestCfg(), plan)
	if err == nil {
		t.Fatal("a restore point taken while replicating from a different source was accepted")
	}
	for _, want := range []string{"otherhost:web01", "srchost:web01"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name both sources", err.Error())
		}
	}
}

func TestCheckRestoreIdentityToleratesMissingProvenance(t *testing.T) {
	// Refusing on an absent value would lock out every replica written by a
	// vmsync that predates the sidecar's source field, and every domain in a
	// deployment that has never recorded replica_source. -promote takes the
	// same position: it wants corroboration, not a guess, and an absent value
	// is not a contradiction.
	for name, mutate := range map[string]func(*restorePlan){
		"no sidecar source":  func(p *restorePlan) { p.status.Source = "" },
		"no replica_source":  func(p *restorePlan) { p.replicaSource = "" },
		"neither is present": func(p *restorePlan) { p.status.Source, p.replicaSource = "", "" },
	} {
		t.Run(name, func(t *testing.T) {
			plan := restoreTestPlan(t)
			mutate(&plan)
			if err := checkRestoreIdentity(restoreTestCfg(), plan); err != nil {
				t.Fatalf("refused on an absent value rather than a contradicting one: %v", err)
			}
		})
	}
}

// This is what tells the two meanings of replication_role=paused apart.
// TargetRoleAllowsRestore allows paused deliberately, but -shutdown-domain
// also writes paused -- on a domain that was serving live and has just been
// stopped by a planned failover or a fence. Its disks then hold everything
// written since the last sync in the other direction, and last_replicated_at
// is the field that says so.
func TestCheckRestoreIdentityRefusesADomainThatServedAsASourceSince(t *testing.T) {
	plan := restoreTestPlan(t)
	plan.lastReplicatedAt = fmt.Sprintf("%d", plan.status.TakenAt+3600)

	err := checkRestoreIdentity(restoreTestCfg(), plan)
	if err == nil {
		t.Fatal("a restore was allowed on a domain that replicated OUT after the restore point was taken -- its disks hold writes no replica of it contains")
	}
	if !strings.Contains(err.Error(), "-update-role") {
		t.Errorf("refusal %q does not tell the operator the way out", err.Error())
	}
}

func TestCheckRestoreIdentityAllowsADomainWhoseOutwardSyncsPredateThePoint(t *testing.T) {
	// A domain that was a source long ago and has since been a replica. The
	// restore point is newer than anything it ever sent, so it cannot be
	// hiding writes the point does not contain.
	plan := restoreTestPlan(t)
	plan.lastReplicatedAt = fmt.Sprintf("%d", plan.status.TakenAt-3600)
	if err := checkRestoreIdentity(restoreTestCfg(), plan); err != nil {
		t.Fatalf("refused a domain whose outward replication predates the restore point: %v", err)
	}
}

func TestCheckRestoreIdentityIgnoresAnUnparsableLastReplicatedAt(t *testing.T) {
	// Written by something else, or truncated. It cannot be compared, so it
	// cannot be evidence either way -- and refusing on it would make an
	// unrelated corrupt field block recovery during an incident.
	plan := restoreTestPlan(t)
	plan.lastReplicatedAt = "not-a-timestamp"
	if err := checkRestoreIdentity(restoreTestCfg(), plan); err != nil {
		t.Fatalf("an unparsable last_replicated_at blocked a restore: %v", err)
	}
}

// restoreRootFor derives -target-disk-path from the domain when the flag is
// absent. A restore refuses outright without the domain, so by the time this
// runs the domain is known to exist and has already said where its disks are.

func TestRestoreRootForDerivesFromTheDomainWhenTheFlagIsAbsent(t *testing.T) {
	cfg := restoreTestCfg()
	cfg.TargetDiskPath = ""

	replicaDir, store, err := restoreRootFor(cfg, restoreTestPlan(t))
	if err != nil {
		t.Fatalf("restoreRootFor: %v", err)
	}
	if replicaDir != "/data/replicas" {
		t.Errorf("replicaDir = %q, want /data/replicas", replicaDir)
	}
	// The SAME rule the sync path uses, StoreFor(actual disk path, target
	// domain) -- not StoreForDir(flag, …). Those agree only when the flag names
	// the directory the disks are really in, so a sync run without the flag
	// takes restore points the verbs cannot find.
	want, err := restorepoint.StoreFor("/data/replicas/web01.qcow2", cfg.TargetDomain)
	if err != nil {
		t.Fatalf("StoreFor: %v", err)
	}
	got, err := store.Path()
	if err != nil {
		t.Fatalf("Store.Path: %v", err)
	}
	wantPath, err := want.Path()
	if err != nil {
		t.Fatalf("Store.Path: %v", err)
	}
	if got != wantPath {
		t.Errorf("store = %q, want %q (the same expression the sync path uses)", got, wantPath)
	}
	// And it is keyed by the DOMAIN, not just the directory. Two replicas
	// sharing this directory must not resolve to one store -- that is the whole
	// of CI-06, and a restore is the path where addressing another pair's
	// history would be worst.
	if want := "/data/replicas/" + restorepoint.DirName + "/" + restorepoint.DomainPrefix + cfg.TargetDomain; got != want {
		t.Errorf("store = %q, want %q", got, want)
	}
}

func TestRestoreRootForPrefersAnExplicitFlag(t *testing.T) {
	cfg := restoreTestCfg()
	cfg.TargetDiskPath = "/elsewhere"

	replicaDir, store, err := restoreRootFor(cfg, restoreTestPlan(t))
	if err != nil {
		t.Fatalf("restoreRootFor: %v", err)
	}
	got, err := store.Path()
	if err != nil {
		t.Fatalf("Store.Path: %v", err)
	}
	want := "/elsewhere/" + restorepoint.DirName + "/" + restorepoint.DomainPrefix + cfg.TargetDomain
	if replicaDir != "/elsewhere" || got != want {
		t.Fatalf("an explicit -target-disk-path was ignored: dir=%q store=%q, want dir=/elsewhere store=%q", replicaDir, got, want)
	}
	// Ignored, not trusted: checkRestoreIdentity refuses it against this same
	// domain, so a wrong flag cannot silently aim the restore elsewhere.
	plan := restoreTestPlan(t)
	plan.replicaDir = replicaDir
	plan.disks = []string{"/elsewhere/web01.qcow2", "/elsewhere/web01-data.qcow2"}
	if err := checkRestoreIdentity(cfg, plan); err == nil {
		t.Fatal("an explicit -target-disk-path naming files the domain does not own was accepted")
	}
}

func TestRestoreRootForRefusesScatteredDisks(t *testing.T) {
	// -retention refuses such a domain outright ("every target disk in one
	// directory so a restore point is a single consistent set"), so there is
	// no set to find. Picking one of the directories would look in an empty
	// one and report "no restore point" for entirely the wrong reason.
	plan := restoreTestPlan(t)
	plan.domXML = strings.Replace(plan.domXML,
		"/data/replicas/web01-data.qcow2", "/data/other/web01-data.qcow2", 1)
	cfg := restoreTestCfg()
	cfg.TargetDiskPath = ""

	_, _, err := restoreRootFor(cfg, plan)
	if err == nil {
		t.Fatal("a domain whose disks span two directories was accepted")
	}
	for _, want := range []string{"/data/replicas", "/data/other", "-target-disk-path"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err.Error(), want)
		}
	}
}

func TestRestoreRootForRefusesADomainWithNoQcowDisks(t *testing.T) {
	plan := restoreTestPlan(t)
	plan.domXML = `<domain type='kvm'><name>web01</name><devices></devices></domain>`
	cfg := restoreTestCfg()
	cfg.TargetDiskPath = ""

	if _, _, err := restoreRootFor(cfg, plan); err == nil {
		t.Fatal("a domain with no qcow2 disks was accepted; there is nowhere for restore points to be")
	}
}

// uriRunsCommandsLocally decides whether cp, mv and rm run here or over SSH.
// Getting it wrong in the permissive direction runs them against the wrong
// machine's filesystem -- silently, because the paths very likely exist there
// too.
func TestUriRunsCommandsLocally(t *testing.T) {
	local := []string{
		"qemu:///system",
		"qemu:///session",
		"qemu+unix:///system",
		"qemu://localhost/system",
		"qemu://127.0.0.1/system",
	}
	for _, uri := range local {
		if !uriRunsCommandsLocally(uri) {
			t.Errorf("uriRunsCommandsLocally(%q) = false, want true -- vmsync is on the host holding the disks", uri)
		}
	}

	remote := []struct{ uri, why string }{
		{"qemu+ssh://tgthost/system", "ssh: the operator named a user and a key, and running as whoever invoked vmsync would change who owns the files it creates"},
		{"qemu+ssh://localhost/system", "ssh even to localhost stays on the ssh path, for the same reason"},
		{"qemu+tcp://tgthost/system", "THE trap: neither ssh nor local. Running commands here would touch this machine's filesystem while libvirt acts on another's"},
		{"qemu+tls://tgthost/system", "same as tcp"},
		{"qemu://tgthost/system", "names a remote host"},
	}
	for _, tc := range remote {
		if uriRunsCommandsLocally(tc.uri) {
			t.Errorf("uriRunsCommandsLocally(%q) = true, want false -- %s", tc.uri, tc.why)
		}
	}
}

func TestTargetRunnerRefusesAURIItCannotRunCommandsOn(t *testing.T) {
	// qemu+tcp:// reaches libvirt but offers no way to run a shell command on
	// the host. Falling back to local would be silently wrong, so it refuses --
	// and the message has to name the two ways out, since the fix is a
	// different URI scheme rather than a missing flag.
	cfg := syncConfig{TargetURI: "qemu+tcp://tgthost/system", TargetDiskPath: "/data/replicas"}
	_, closeRunner, err := targetRunnerForRestorePoints(cfg)
	closeRunner()
	if err == nil {
		t.Fatal("a qemu+tcp:// target URI was accepted; commands would have run on the wrong host")
	}
	for _, want := range []string{"qemu+ssh://", "qemu:///system"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not offer %s as a way out", err.Error(), want)
		}
	}
}

func TestTargetRunnerCleanupIsAlwaysSafeToCall(t *testing.T) {
	// Callers defer it unconditionally, including on the error path, so a nil
	// func would turn every bad URI into a panic.
	for _, uri := range []string{"qemu:///system", "qemu+tcp://tgthost/system", "::not a uri::"} {
		_, closeRunner, _ := targetRunnerForRestorePoints(syncConfig{TargetURI: uri})
		if closeRunner == nil {
			t.Fatalf("targetRunnerForRestorePoints(%q) returned a nil cleanup", uri)
		}
		closeRunner()
	}
}

// localRunner has to behave exactly as remotessh.Client.Run does, because
// every command builder in pkg/restorepoint is written against those
// semantics. A difference here would show up as a restore command that works
// over SSH and misbehaves on the host itself.
func TestLocalRunnerMatchesTheSSHRunContract(t *testing.T) {
	ctx := context.Background()
	r := localRunner{}

	out, err := r.Run(ctx, "echo hello")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "hello" {
		t.Errorf("Run trimmed output = %q, want %q", out, "hello")
	}

	// stderr is merged, not dropped: the command builders write their refusal
	// messages to stderr and the callers put them in the error.
	out, err = r.Run(ctx, "echo oops >&2; exit 1")
	if err == nil {
		t.Fatal("a non-zero exit was reported as success")
	}
	if !strings.Contains(out, "oops") {
		t.Errorf("stderr was dropped: output = %q", out)
	}

	// Shell, not a parsed argv: the builders emit if/fi, pipes and redirection.
	if out, err = r.Run(ctx, "if [ -n \"x\" ]; then echo yes; fi"); err != nil || out != "yes" {
		t.Errorf("shell constructs are not honoured: out=%q err=%v", out, err)
	}
}

func TestCheckRestoreIdentityRefusesAnUnreadableDomainDefinition(t *testing.T) {
	plan := restoreTestPlan(t)
	plan.domXML = "<domain>truncated"
	if err := checkRestoreIdentity(restoreTestCfg(), plan); err == nil {
		t.Fatal("a domain whose definition could not be parsed was accepted -- the disk check could not have run")
	}
}

// THE DIRECTION THE IDENTITY CHECKS DID NOT LOOK.
//
// Check 1 proves every disk the POINT holds is one the domain owns, and the
// replica-presence check in loadRestorePlan proves the same thing again
// against the files. Neither proves the point is not SHORTER than the
// machine. A disk added to the VM after the point was taken appears in
// neither list, so it is rolled back by nothing: every other volume goes
// back in time and this one stays where it is.
//
// A database with data and log on separate volumes is the ordinary shape of
// this, and the replica's metadata afterwards asserts one coherent
// checkpoint -- so evidenceProblems finds nothing and -promote accepts it.
func TestCheckRestoreIdentityRefusesAPointShorterThanTheMachine(t *testing.T) {
	// THREE disks, of which the point holds two. Two would prove the
	// refusal fires, but not that it says anything useful: with one disk in
	// the point and one out, "names the extra" and "dumps every disk the
	// domain owns" produce the same substring, and a count of 1 is
	// indistinguishable from a count of everything. On a real VM with forty
	// volumes that is the whole difference between a refusal an operator can
	// act on and a wall of paths.
	const threeDisks = `<domain type='kvm'>
  <name>web01</name>
  <metadata>
    <vmsync:vmsync xmlns:vmsync="http://vmsync.org/xmlns/libvirt/domain/1.0">
      <replica_source id="srchost:web01"/>
    </vmsync:vmsync>
  </metadata>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='/data/replicas/web01.qcow2'/>
      <target dev='vda' bus='virtio'/>
    </disk>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='/data/replicas/web01-data.qcow2'/>
      <target dev='vdb' bus='virtio'/>
    </disk>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='/data/replicas/web01-log.qcow2'/>
      <target dev='vdc' bus='virtio'/>
    </disk>
  </devices>
</domain>`

	plan := restoreTestPlan(t)
	plan.domXML = threeDisks
	// The point predates web01-log.qcow2 being added to the domain.
	plan.disks = []string{"/data/replicas/web01.qcow2", "/data/replicas/web01-data.qcow2"}
	plan.status.Disks = []string{"web01.qcow2", "web01-data.qcow2"}

	err := checkRestoreIdentity(restoreTestCfg(), plan)
	if err == nil {
		t.Fatal("a restore point holding fewer disks than the domain was accepted; the disk it does not hold would keep its current contents while the rest rolled back, producing a machine assembled from two different moments")
	}
	msg := err.Error()

	// It names the disk that is unaccounted for, the domain, the tag, and
	// the remedy -- the same remedy the opposite direction offers.
	for _, want := range []string{"/data/replicas/web01-log.qcow2", "web01", "1756041600-vmsync-cpt-000042", "-clone-restore-point"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q does not mention %q", msg, want)
		}
	}
	// ...and names ONLY that one. Listing the disks the point does hold
	// turns a precise refusal into a dump of the domain, which is the
	// failure the assertion above cannot see on its own.
	for _, unwanted := range []string{"/data/replicas/web01.qcow2", "/data/replicas/web01-data.qcow2"} {
		if strings.Contains(msg, unwanted) {
			t.Errorf("refusal names %q, which the restore point DOES hold; an operator reading this cannot tell which disk is the problem: %s", unwanted, msg)
		}
	}
	// The count is the other half of being precise, and nothing else
	// asserts it: "3 disk(s)" for one missing disk is as misleading as
	// listing all three.
	if !strings.Contains(msg, "1 disk(s)") {
		t.Errorf("refusal does not report exactly one unaccounted disk: %s", msg)
	}
	// ...and it must not be mistaken for the crossed-flags refusal above,
	// whose remedy is a different one entirely.
	if strings.Contains(msg, "-target-disk-path name the same replica") {
		t.Error("the short-point refusal reports itself as the crossed-flags case, sending the operator to check flags that are correct")
	}
}

// THE ASSUMPTION THE WHOLE CHECK RESTS ON. ParseQcowDisks drops cdroms and
// non-qcow2 disks via util.IgnoreDevice, so the domain's qcow2 set is
// exactly what a sync would have copied.
//
// If that ever stops being true, the new check above refuses every restore
// of a perfectly ordinary domain -- a cdrom is on nearly every VM -- and it
// would do so at the worst possible moment, during an incident. Pinned
// here rather than trusted.
func TestCheckRestoreIdentityIgnoresDevicesVmsyncNeverReplicates(t *testing.T) {
	const withExtras = `<domain type='kvm'>
  <name>web01</name>
  <metadata>
    <vmsync:vmsync xmlns:vmsync="http://vmsync.org/xmlns/libvirt/domain/1.0">
      <replica_source id="srchost:web01"/>
    </vmsync:vmsync>
  </metadata>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='/data/replicas/web01.qcow2'/>
      <target dev='vda' bus='virtio'/>
    </disk>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='/data/replicas/web01-data.qcow2'/>
      <target dev='vdb' bus='virtio'/>
    </disk>
    <disk type='file' device='cdrom'>
      <driver name='qemu' type='raw'/>
      <source file='/iso/debian.iso'/>
      <target dev='sda' bus='sata'/>
      <readonly/>
    </disk>
    <disk type='file' device='disk'>
      <driver name='qemu' type='raw'/>
      <source file='/data/replicas/scratch.raw'/>
      <target dev='vdc' bus='virtio'/>
    </disk>
  </devices>
</domain>`

	plan := restoreTestPlan(t)
	plan.domXML = withExtras

	if err := checkRestoreIdentity(restoreTestCfg(), plan); err != nil {
		t.Fatalf("a domain carrying a cdrom and a raw disk was refused: %v -- vmsync replicates neither, so neither can be in a restore point, and refusing on them would block every restore of an ordinary VM", err)
	}
}

// Equality, not merely inclusion: the two halves of check 1 together have to
// mean "the same set", so a point and a domain that disagree in EITHER
// direction are refused.
func TestCheckRestoreIdentityRequiresTheDiskSetsToMatchBothWays(t *testing.T) {
	for _, tc := range []struct {
		name      string
		planDisks []string
		pointDisk []string
	}{
		{"the point holds a disk the domain does not own",
			[]string{"/data/replicas/web01.qcow2", "/data/replicas/web01-data.qcow2", "/data/replicas/gone.qcow2"},
			[]string{"web01.qcow2", "web01-data.qcow2", "gone.qcow2"}},
		{"the domain owns a disk the point does not hold",
			[]string{"/data/replicas/web01.qcow2"},
			[]string{"web01.qcow2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := restoreTestPlan(t)
			plan.disks = tc.planDisks
			plan.status.Disks = tc.pointDisk
			if err := checkRestoreIdentity(restoreTestCfg(), plan); err == nil {
				t.Error("accepted; the restore would produce a machine whose volumes come from two different moments")
			}
		})
	}
}

// A qcow2 disk that is not FILE-backed -- type='block', 'network', 'volume',
// 'nvme', or one with no <source> at all.
//
// IgnoreDevice filters on device type and driver format, never on where the
// bytes live, so these reach checkRestoreIdentity with an empty Source.
// Refusing is correct: nothing ever copied such a disk, so no restore point
// can hold it, and rolling the rest back would leave it at its current
// contents. But an empty path renders the whole list as "[]", which tells an
// operator nothing about which device to deal with.
func TestCheckRestoreIdentityNamesADiskThatHasNoFilePath(t *testing.T) {
	const blockBacked = `<domain type='kvm'>
  <name>web01</name>
  <metadata>
    <vmsync:vmsync xmlns:vmsync="http://vmsync.org/xmlns/libvirt/domain/1.0">
      <replica_source id="srchost:web01"/>
    </vmsync:vmsync>
  </metadata>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='/data/replicas/web01.qcow2'/>
      <target dev='vda' bus='virtio'/>
    </disk>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='/data/replicas/web01-data.qcow2'/>
      <target dev='vdb' bus='virtio'/>
    </disk>
    <disk type='block' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source dev='/dev/vg0/scratch'/>
      <target dev='vdz' bus='virtio'/>
    </disk>
  </devices>
</domain>`

	plan := restoreTestPlan(t)
	plan.domXML = blockBacked

	err := checkRestoreIdentity(restoreTestCfg(), plan)
	if err == nil {
		t.Fatal("a domain carrying a qcow2 disk no sync could have copied was accepted for restore")
	}
	msg := err.Error()
	if !strings.Contains(msg, "vdz") {
		t.Errorf("the refusal does not name the device, so an operator cannot tell which disk it means: %s", msg)
	}
	// The empty-string rendering this exists to prevent.
	if strings.Contains(msg, "hold: []") || strings.Contains(msg, "[]") {
		t.Errorf("the refusal renders the unaccounted disk as an empty list: %s", msg)
	}
}
