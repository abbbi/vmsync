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
	"strings"
	"testing"
)

// TestOperationArgsCarriesTheActionID pins the correlation id on every kind
// of operation.
//
// The id is the operation's own, which is what the control plane's
// operations.json is keyed by and what this agent already writes into its
// run log as operation_id -- so the journal record the engine leaves beside
// the disks joins back to both without anyone having to line up two hosts'
// clocks. Reinit and force-clean matter most, because those are the runs
// that arm replica_incomplete: the marker carries this same id, so a
// promotion refused long afterwards, on a host whose peer is gone, still
// names the action that armed it.
//
// Reinit and force-clean take the second loop rather than the table: their
// argv is finished by buildSyncRequest, which needs a live libvirt holding
// the domain, so only the libvirt-free half -- reinitArgs, where the id is
// actually stamped -- can be reached from a test. They are checked against
// the same contract as every kind above, and they are the two that must not
// be left out of it.
func TestOperationArgsCarriesTheActionID(t *testing.T) {
	cfg := agentConfig{LibvirtURI: "qemu:///system", TargetURIPattern: "qemu+ssh://%s/system"}
	const id = "0123456789abcdef0123456789abcdef"

	for _, tc := range []struct {
		name string
		op   Operation
	}{
		{"promote", Operation{ID: id, Kind: OpPromote, VM: "web01", Mode: "planned"}},
		{"shutdown", Operation{ID: id, Kind: OpShutdown, VM: "web01", ShutdownTimeoutSec: 300}},
		{"invert", Operation{ID: id, Kind: OpInvert, VM: "web01", PeerHost: "prod01"}},
		{"set-role", Operation{ID: id, Kind: OpSetRole, VM: "web01", Mode: "target"}},
		{"restore", Operation{ID: id, Kind: OpRestore, VM: "web01", Tag: "1756041600-vmsync-cpt-000040"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args, err := operationArgs(cfg, nil, tc.op)
			if err != nil {
				t.Fatalf("operationArgs: %v", err)
			}
			if got := valueOf(args, "-action-id"); got != id {
				t.Errorf("-action-id = %q, want the operation id %q -- without it the journal record for this action cannot be joined to operations.json", got, id)
			}
			if n := countArg(args, "-action-id"); n != 1 {
				t.Errorf("-action-id appears %d times in %v", n, args)
			}
		})
	}

	// The two kinds that arm replica_incomplete. Their id is the one that
	// ends up in the marker's action= field, so a promotion refused on a
	// host whose peer is long gone still names the operation that armed it;
	// an id lost here leaves that marker pointing at nothing.
	for _, tc := range []struct {
		name       string
		forceClean bool
		flag       string
	}{
		{"reinit", false, "-reinit"},
		{"force-clean", true, "-force-clean"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := reinitArgs(reinitTestPlan(), id, tc.forceClean)
			if got := valueOf(args, "-action-id"); got != id {
				t.Errorf("-action-id = %q, want the operation id %q -- this is the id the replica_incomplete marker carries, and without it a refused promotion leads nowhere", got, id)
			}
			if n := countArg(args, "-action-id"); n != 1 {
				t.Errorf("-action-id appears %d times in %v", n, args)
			}
			// The kind has to survive too: a force-clean that reaches vmsync
			// as a plain reinit leaves the target domain defined, which is
			// the whole reason the two are separate operations.
			if !hasArg(args, tc.flag) {
				t.Errorf("args = %v carry no %s, so this is no longer the full resync that was asked for", args, tc.flag)
			}
		})
	}
}

// TestReinitArgsStampsTheOperationIDOverAnyRunID pins why the id is set on
// the plan instead of being appended to the finished argv.
//
// CommandArgs falls back to the plan's RunID when no ActionID is set, so an
// id appended alongside would leave TWO -action-id flags disagreeing about
// the value, and whichever the engine took last would go into the marker. A
// run id the control plane has never heard of breaks exactly the join a
// refused promotion is followed back along. buildSyncRequest mints no run id
// today; this keeps that from mattering if one is ever minted there.
func TestReinitArgsStampsTheOperationIDOverAnyRunID(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"
	plan := reinitTestPlan()
	plan.RunID = "9f3c1a2b4d5e6f70"

	args := reinitArgs(plan, id, false)
	if got := valueOf(args, "-action-id"); got != id {
		t.Errorf("-action-id = %q, want the operation id %q rather than the run id", got, id)
	}
	if n := countArg(args, "-action-id"); n != 1 {
		t.Errorf("-action-id appears %d times in %v, and the engine would read only one of them", n, args)
	}
	// By value, on purpose: the caller holds a plan built for this pair, and
	// one operation's id must not ride along on it into anything else.
	//
	// What this assertion actually pins is the SIGNATURE, not a behaviour it
	// could catch at run time: syncPlan carries no pointer, map or slice
	// through which ActionID could be reached, so with a value parameter the
	// caller's copy is untouchable and this can never fire. Change the
	// parameter to *syncPlan and the failure is a compile error on this line
	// instead. Kept because that is the property worth defending, and worth
	// saying so rather than leaving a reader to believe it is guarding
	// against a mutation it would notice.
	if plan.ActionID != "" {
		t.Errorf("reinitArgs left ActionID = %q on the caller's plan", plan.ActionID)
	}
}

// reinitTestPlan is the plan buildSyncRequest would hand back for one pair,
// with only the endpoints CommandArgs insists on. Built here rather than by
// that function because it needs a live libvirt holding the domain.
func reinitTestPlan() syncPlan {
	return syncPlan{SyncRequest: SyncRequest{
		SourceURI:    "qemu:///system",
		SourceDomain: "web01",
		TargetURI:    "qemu+ssh://dr01/system",
		TargetDomain: "web01",
	}}
}

// countArg is how many times flag appears, which is the question a duplicated
// -action-id would answer with 2.
func countArg(args []string, flag string) int {
	n := 0
	for _, a := range args {
		if a == flag {
			n++
		}
	}
	return n
}

// An operation id that would not survive the replica_incomplete grammar is
// dropped rather than escaped, and the operation still runs.
//
// Both halves matter. Passing a comma through would corrupt the one field a
// promotion refuses on -- it would still refuse, but with no verb, no host
// and no aside stamp naming the complete copy that was set aside. And
// REFUSING THE OPERATION over it would be worse still: this is a failover
// path, and an odd id from a control plane is not a reason to decline to
// promote during a disaster. vmsync mints its own id, and the run proceeds
// with a journal record that simply cannot be joined back.
func TestOperationArgsDropsAnUnusableActionIDWithoutRefusingTheOperation(t *testing.T) {
	cfg := agentConfig{LibvirtURI: "qemu:///system", TargetURIPattern: "qemu+ssh://%s/system"}

	args, err := operationArgs(cfg, nil, Operation{
		ID: "id,with=separators", Kind: OpPromote, VM: "web01", Mode: "planned",
	})
	if err != nil {
		t.Fatalf("operationArgs refused an operation over its id: %v", err)
	}
	for _, a := range args {
		if a == "-action-id" {
			t.Errorf("args = %v pass an id that would corrupt the replica_incomplete value", args)
		}
	}
	// The operation itself is unchanged: still a promotion of this VM.
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-promote") || !strings.Contains(joined, "-target-domain web01") {
		t.Errorf("args = %v are no longer the promotion that was asked for", args)
	}
}
