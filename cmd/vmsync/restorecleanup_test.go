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
	"strings"
	"testing"

	"vmsync/pkg/util"
)

// ctxRecordingRunner answers like a remote host that is perfectly healthy,
// and refuses -- exactly as remotessh does -- any command handed a context
// that is already cancelled.
//
// That refusal is the behaviour under test. remotessh's run path opens with
// `select { case <-ctx.Done(): return "", "", fmt.Errorf("open ssh session:
// %w", ctx.Err()) }`, so a cancelled context is not a slow command, it is
// no command at all.
type ctxRecordingRunner struct {
	ran     []string
	refused int
}

func (r *ctxRecordingRunner) Run(ctx context.Context, cmd string) (string, error) {
	if err := ctx.Err(); err != nil {
		r.refused++
		return "", err
	}
	r.ran = append(r.ran, cmd)
	return "", nil
}

// A RESTORE ABANDONED BY A SIGNAL MUST STILL BE ABLE TO PUT THE DISKS BACK.
//
// The mode dispatch builds the run's context with signal.NotifyContext, so
// a SIGTERM or SIGINT cancels it. undoRestore used to be handed that same
// context, which meant the rollback could not issue one command on the one
// occasion it exists for: a signal landing between the first and last disk
// swap left the replica assembled from two moments, the staged and aside
// copies on disk with nothing reaping them, and ownership unrestored.
//
// Nothing unsafe was reachable -- the armed replica_incomplete and the
// failure_count written through libvirt both refuse a promotion, and
// neither goes over ssh -- but the automatic repair was lost, leaving an
// operator to put disks back by hand from the aside stamp.
func TestARollbackRunsEvenAfterTheRunWasCancelled(t *testing.T) {
	plan := restoreTestPlan(t)
	plan.asides = []string{"/data/replicas/web01.qcow2.aside", "/data/replicas/web01-data.qcow2.aside"}
	plan.temps = []string{"/data/replicas/web01.qcow2.tmp", "/data/replicas/web01-data.qcow2.tmp"}

	runner := &ctxRecordingRunner{}
	owners := make([]util.DiskOwner, len(plan.disks))

	// The run's own context is already dead, which is the state every
	// signal-interrupted restore reaches this function in.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if ctx.Err() == nil {
		t.Fatal("the test's own context is not cancelled")
	}

	// undoRestore takes no context precisely so that it cannot be handed
	// this one. If that ever changes, the signature change is the warning.
	undoRestore(runner, nil, restoreTestCfg(), plan, len(plan.disks), owners)

	if runner.refused != 0 {
		t.Errorf("%d rollback command(s) were refused for a cancelled context; the disks stay half-swapped on exactly the path this function exists for", runner.refused)
	}
	if len(runner.ran) == 0 {
		t.Fatal("the rollback issued no commands at all")
	}
	// Every disk already replaced has to be put back, not just the first.
	for _, aside := range plan.asides {
		found := false
		for _, cmd := range runner.ran {
			if strings.Contains(cmd, aside) {
				found = true
			}
		}
		if !found {
			t.Errorf("no command put back %s, so that disk is left holding restored contents while others were reverted", aside)
		}
	}
}

// The same for the stand-down paths, which remove the copies a restore made
// before giving up. Left behind they are full-size files that share extents
// now and stop sharing them as the replica is written -- a cost that arrives
// later, on the target's filesystem, with nothing pointing at the run that
// caused it.
func TestTheStandDownDiscardsRunEvenAfterCancellation(t *testing.T) {
	cctx, cancel := restoreCleanupCtx()
	defer cancel()
	if err := cctx.Err(); err != nil {
		t.Fatalf("a freshly built cleanup context is already unusable: %v", err)
	}
	// It must be bounded: an unbounded one would need a second signal to
	// escape a target that has gone away.
	if _, hasDeadline := cctx.Deadline(); !hasDeadline {
		t.Error("the cleanup context has no deadline, so a target that stops answering hangs the stand-down")
	}
}
