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

func TestCheckRestoreIdentityRefusesAnUnreadableDomainDefinition(t *testing.T) {
	plan := restoreTestPlan(t)
	plan.domXML = "<domain>truncated"
	if err := checkRestoreIdentity(restoreTestCfg(), plan); err == nil {
		t.Fatal("a domain whose definition could not be parsed was accepted -- the disk check could not have run")
	}
}
