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
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"vmsync/pkg/libvirtsync"
	"vmsync/pkg/trace"
)

// opTickInterval is how often the executor looks for work. Operations
// arrive by long poll, so this only bounds the gap between a config landing
// and being acted on.
const opTickInterval = 5 * time.Second

// operationsLoop executes one-shot operations from the config currently in
// force.
//
// Deliberately its own loop rather than part of the scheduler, and NOT
// disabled by -no-schedule. A DR-site target host is exactly the machine
// most likely to be installed reporting-only -- it has no schedule of its
// own, so nobody ever removes the flag -- and it is also the machine a
// failover must run on. Tying the two together would deliver a promotion to
// a visibly healthy agent that silently ignores it.
func operationsLoop(ctx context.Context, cfg agentConfig, state *sharedState, ledger *operationLedger) {
	ticker := time.NewTicker(opTickInterval)
	defer ticker.Stop()

	for {
		// Whatever config is in force. Operations reach it only from a poll
		// in this process lifetime -- Store.LoadCache strips them, so a
		// restart cannot replay one off disk. See the reasoning there.
		//
		// Executed one at a time, deliberately. These are failovers; two
		// running concurrently on one host is not a throughput problem to
		// solve but a situation nobody should be able to create by clicking
		// twice. A long one blocking the loop is the correct behaviour.
		cached := state.get()
		published := map[string]bool{}
		for _, op := range cached.Config.Operations {
			published[op.ID] = true
			runOperation(ctx, cfg, cached.Config.Schedule, op, ledger)
			if ctx.Err() != nil {
				return
			}
		}
		// Anything the UI has stopped publishing has been acknowledged, so
		// its result no longer needs carrying.
		ledger.Forget(published)

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// runOperation executes one operation, exactly once, ever.
func runOperation(ctx context.Context, cfg agentConfig, schedule []ScheduleEntry, op Operation, ledger *operationLedger) {
	if _, seen := ledger.Seen(op.ID); seen {
		// Already acted on, in some state. Its stored result is re-reported
		// until the UI stops publishing it; nothing else to do.
		return
	}
	now := time.Now()

	localPeers, err := localPeersFor(cfg, op.VM)
	if err != nil {
		finishOperation(ledger, op, OperationResult{State: OpStateRefused, Error: err.Error()}, now)
		return
	}
	if err := op.Validate(localPeers, now); err != nil {
		state := OpStateRefused
		if op.NotAfterUnix > 0 && now.Unix() > op.NotAfterUnix {
			state = OpStateExpired
		}
		// Refusals are RECORDED and reported, never silently dropped: an
		// operator watching a failover sit "pending" against a healthy agent
		// with nothing saying why is the worst outcome available here.
		trace.Error("refusing an operation", "id", op.ID, "kind", op.Kind, "vm", op.VM, "error", err)
		finishOperation(ledger, op, OperationResult{State: state, Error: err.Error()}, now)
		return
	}

	args, err := operationArgs(cfg, schedule, op)
	if err != nil {
		finishOperation(ledger, op, OperationResult{State: OpStateRefused, Error: err.Error()}, now)
		return
	}

	// Intent first, and durably, BEFORE anything is done to a domain. A
	// crash after this point leaves a `running` record that becomes
	// `unknown` on restart and is never retried -- which is the whole
	// contract. If even this write fails, nothing is executed: acting
	// without a durable record is what makes a failover replayable.
	if err := ledger.Begin(op, now); err != nil {
		trace.Error("not executing an operation because its intent could not be recorded", "id", op.ID, "error", err)
		return
	}

	trace.Info("executing operation", "id", op.ID, "kind", op.Kind, "vm", op.VM, "by", op.CreatedBy)
	trace.Debug("operation command", "id", op.ID, "binary", cfg.VmsyncPath, "args", args)

	cmd := exec.CommandContext(ctx, cfg.VmsyncPath, args...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 60 * time.Second
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out

	runErr := cmd.Run()
	res := OperationResult{
		State:         OpStateDone,
		StartedAtUnix: now.Unix(),
		ExitCode:      cmd.ProcessState.ExitCode(),
		LogTail:       tail(out.String(), logTailBytes),
	}
	if runErr != nil {
		res.State, res.Error = OpStateFailed, runErr.Error()
		trace.Error("operation failed", "id", op.ID, "kind", op.Kind, "vm", op.VM, "exit_code", res.ExitCode, "error", runErr)
		if t := tail(out.String(), logTailBytes); t != "" {
			trace.Error("operation output", "id", op.ID, "output", t)
		}
	} else {
		trace.Info("operation finished", "id", op.ID, "kind", op.Kind, "vm", op.VM)
	}
	finishOperation(ledger, op, res, time.Now())
}

func finishOperation(ledger *operationLedger, op Operation, res OperationResult, now time.Time) {
	if err := ledger.Finish(op, res, now); err != nil {
		// The operation happened; only the record of it failed. Loud,
		// because the UI will keep publishing it and the agent will keep
		// re-executing until this is fixed.
		trace.Error("operation outcome could not be recorded; it may be executed again", "id", op.ID, "error", err)
	}
}

// scheduleEntryFor finds the schedule entry a reinit needs, by VM name.
//
// Case-sensitive, matching libvirt: a domain name is what it is, and matching
// loosely here would let a reinit resolve to a different pair's transport
// settings than the operator's schedule shows.
func scheduleEntryFor(schedule []ScheduleEntry, vm string) (ScheduleEntry, bool) {
	for _, e := range schedule {
		if e.VM == vm {
			return e, true
		}
	}
	return ScheduleEntry{}, false
}

// operationArgs builds the vmsync argv for an operation.
//
// The local URI throughout: -promote, -shutdown-domain and -restore-restore-point
// all act on the host they run on, so that a failover needs no credentials to
// reach the site that just failed. Two exceptions, both of which genuinely span
// both ends and both of which run on the SOURCE, which already has the SSH path
// because that is the direction syncs run: inversion, and reinit.
//
// The schedule is here only for reinit, which is the one kind that is a SYNC
// and therefore needs a whole transport configuration rather than a domain name.
func operationArgs(cfg agentConfig, schedule []ScheduleEntry, op Operation) ([]string, error) {
	switch op.Kind {
	case OpPromote:
		args := []string{"-promote", "-target-uri", cfg.LibvirtURI, "-target-domain", op.VM}
		if op.Mode != "" {
			args = append(args, "-promote-mode", op.Mode)
		}
		if op.StartVM {
			args = append(args, "-start")
		}
		if op.Force {
			args = append(args, "-force-promote")
		}
		if op.ArmFence {
			// Bare, so vmsync resolves the source from the promoted
			// domain's own replica_source rather than from anything that
			// travelled over the network. The UI can ask for a fence; it
			// cannot choose who gets shut down.
			args = append(args, "-fence-source")
		}
		if op.CreatedBy != "" {
			args = append(args, "-promoted-by", op.CreatedBy)
		}
		return args, nil

	case OpShutdown:
		args := []string{"-shutdown-domain", "-target-uri", cfg.LibvirtURI, "-target-domain", op.VM}
		if op.ShutdownTimeoutSec > 0 {
			args = append(args, "-shutdown-timeout-sec", strconv.Itoa(op.ShutdownTimeoutSec))
		}
		return args, nil

	case OpInvert:
		if op.PeerHost == "" {
			return nil, fmt.Errorf("invert needs the promoted peer, and operation %s names none", op.ID)
		}
		peerVM := op.PeerVM
		if peerVM == "" {
			peerVM = op.VM
		}
		return []string{
			"-invert",
			"-source-uri", cfg.LibvirtURI, "-source-domain", op.VM,
			"-target-uri", fmt.Sprintf(cfg.TargetURIPattern, op.PeerHost), "-target-domain", peerVM,
		}, nil

	case OpSetRole:
		if op.Mode == "" {
			return nil, fmt.Errorf("set-role needs a role, and operation %s names none", op.ID)
		}
		return []string{"-update-role", op.Mode, "-target-uri", cfg.LibvirtURI, "-target-domain", op.VM}, nil

	case OpRestore:
		// The local URI, like promote and shutdown-domain: a restore acts on
		// the host it runs on, so it needs no credentials to reach anywhere.
		//
		// -target-disk-path is deliberately NOT passed. The engine derives
		// the directory from the target domain's own definition, which is
		// the same rule the sync used to place the restore points -- a
		// configured path agrees only when it happens to name the directory
		// the disks are really in. See restoreRootFor.
		//
		// -force-restore is mandatory here. Without it the run only prints
		// an assessment and changes nothing, which as an operation would be
		// a request that reports success having done nothing at all. The
		// assessment step is not lost: it moves to the UI, which must show
		// what the restore would do before anyone can issue this.
		return []string{
			"-restore-restore-point", op.Tag,
			"-force-restore",
			"-target-uri", cfg.LibvirtURI,
			"-target-domain", op.VM,
		}, nil

	case OpReinit:
		// A one-shot full resync, as its own operation rather than a flag on
		// the schedule. On the schedule it would be desired state with
		// nobody to clear it, and every run would delete the target's disks
		// again -- forever.
		//
		// Unlike every other kind here this is a SYNC, so it runs on the
		// source and needs that pair's whole transport configuration --
		// which lives in the schedule, not on the operation. Built from the
		// same SyncRequest a scheduled run uses, so a reinit differs from an
		// ordinary sync by exactly one flag and cannot drift from it.
		entry, ok := scheduleEntryFor(schedule, op.VM)
		if !ok {
			return nil, fmt.Errorf("reinit operation %s names %s, which this agent has no schedule entry for -- a reinit is a full resync and needs that pair's source, target and transport settings, which live on the schedule. Note a reinit runs on the SOURCE's agent, unlike promote and restore which run on the target's", op.ID, op.VM)
		}
		plan, err := buildSyncRequest(cfg, entry)
		if err != nil {
			return nil, fmt.Errorf("reinit operation %s: %w", op.ID, err)
		}
		// The scheduled sync's own argv plus one flag, so a reinit cannot
		// drift from the sync it is a one-off variant of.
		return append(plan.CommandArgs(), "-reinit"), nil
	}
	return nil, fmt.Errorf("operation %s has kind %q, which this agent does not understand", op.ID, op.Kind)
}

// localPeersFor reads the replication peers a domain records about ITSELF.
//
// This is the trust boundary. The UI's PeerHost/PeerVM are a claim to be
// checked against this, never an endpoint to use: everywhere else in the
// agent the far end comes from local libvirt metadata, on the reasoning
// that a name read from local state is not attacker-controlled while one
// arriving over the network is a parameter needing validation. Operations
// are the one channel that can stop a production VM, so they are the last
// place to relax it.
//
// A domain that does not exist is an error rather than an empty list: "no
// such VM here" and "a VM with no replication relationship" are different
// refusals, and an operator chasing a failover that did nothing needs to be
// told which one happened.
func localPeersFor(cfg agentConfig, vm string) ([]string, error) {
	mgr, err := libvirtsync.Connect(cfg.LibvirtURI)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", cfg.LibvirtURI, err)
	}
	defer mgr.Close()

	st, err := libvirtsync.ReadFailoverState(mgr, vm)
	if err != nil {
		return nil, err
	}
	if !st.Exists {
		return nil, fmt.Errorf("no domain named %s on this host", vm)
	}

	peers := make([]string, 0, len(st.ReplicaTargets)+1)
	peers = append(peers, st.ReplicaTargets...)
	if st.ReplicaSource != "" {
		peers = append(peers, st.ReplicaSource)
	}
	return peers, nil
}
