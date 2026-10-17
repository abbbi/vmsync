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
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"vmsync/pkg/trace"
)

// The ledger is what makes an operation single-use. These tests cover the
// one window where it can be lost: an agent restart, before the control
// plane has told this process anything.
//
// Both halves have to hold, and they pull in opposite directions:
//
//   - Nothing may be forgotten on the strength of a published set this
//     process has not actually been told (TestOperationsPass...BeforeTheFirstPoll).
//   - ...but once it HAS been told, forgetting must still happen, or the
//     ledger grows without bound and the UI never sees an acknowledgement
//     (TestOperationsPassStillPrunesOnceTheUIHasAnswered).

// A restart USED TO wipe the ledger on the first pass: the cached config
// carries no operations by construction, and an empty published set read as
// "the UI has acknowledged everything".
//
// The consequence was not merely a lost audit record. A one-shot operation
// the UI is still publishing would no longer be known to have run, so the
// next poll that re-delivered it executed it a SECOND time -- a forced
// promote, run twice, with no operator involved.
func TestOperationsPassDoesNotForgetBeforeTheFirstPoll(t *testing.T) {
	ledger := newOperationLedger(t.TempDir())
	op := promoteOp()
	if err := ledger.Begin(op, opNow); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Finish(op, OperationResult{State: OpStateDone}, opNow); err != nil {
		t.Fatal(err)
	}

	// Exactly what Store.LoadCache hands a freshly started process: a
	// schedule, and no operations -- not because none are published, but
	// because the cache cannot hold one.
	cached := CachedConfig{Config: UIConfig{
		Schedule: []ScheduleEntry{{VM: "web01", IntervalSeconds: 900, Enabled: true}},
	}}

	operationsPass(context.Background(), agentConfig{}, cached, false, ledger)

	if _, seen := ledger.Seen(op.ID); !seen {
		t.Error("the ledger forgot a completed operation before this process had heard from the UI; the next poll to re-deliver it would execute it again")
	}
	if got := ledger.Results(); len(got) != 1 {
		t.Errorf("results = %d, want the stored result still attached to reports -- the console shows the operation pending forever without it", len(got))
	}
}

// The other direction. Forget exists to bound the ledger, and the guard
// above must not switch that off: once a full document has arrived, an
// operation absent from it has been acknowledged and must go.
func TestOperationsPassStillPrunesOnceTheUIHasAnswered(t *testing.T) {
	ledger := newOperationLedger(t.TempDir())
	op := promoteOp()
	if err := ledger.Begin(op, opNow); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Finish(op, OperationResult{State: OpStateDone}, opNow); err != nil {
		t.Fatal(err)
	}

	// A real document from the UI that no longer publishes the operation.
	operationsPass(context.Background(), agentConfig{}, CachedConfig{Config: UIConfig{}}, true, ledger)

	if _, seen := ledger.Seen(op.ID); seen {
		t.Error("the record outlived its acknowledgement; the ledger would grow without bound")
	}
}

// Still published, and already acted on: kept, and never re-run.
func TestOperationsPassKeepsWhatIsStillPublished(t *testing.T) {
	ledger := newOperationLedger(t.TempDir())
	op := promoteOp()
	if err := ledger.Begin(op, opNow); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Finish(op, OperationResult{State: OpStateDone}, opNow); err != nil {
		t.Fatal(err)
	}

	cached := CachedConfig{Config: UIConfig{Operations: []Operation{op}}}
	operationsPass(context.Background(), agentConfig{}, cached, true, ledger)

	if _, seen := ledger.Seen(op.ID); !seen {
		t.Error("a still-published operation was forgotten; its result would stop being reported and the console would wait forever")
	}
}

// THE FIRST POLL OF A PROCESS MUST BE UNCONDITIONAL.
//
// Store.LoadCache deliberately drops Operations, but the envelope keeps the
// ETag of the document they came in. Presenting that ETag asks the UI
// "anything newer than the document I hold?" about a document this agent
// does NOT hold -- so a UI still publishing a failover answers 304, the
// agent keeps its operation-free cached copy, and the failover is never
// delivered. It is published, pending, and silently inert until something
// unrelated changes the document.
func TestFirstPollAfterARestartIsUnconditional(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type seenReq struct {
		ifNoneMatch string
		had         bool
	}
	var mu sync.Mutex
	var reqs []seenReq
	const etag = `"doc-with-a-pending-operation"`

	client, _ := stubUI(t, func(w http.ResponseWriter, r *http.Request) {
		v := r.Header.Get("If-None-Match")
		mu.Lock()
		reqs = append(reqs, seenReq{ifNoneMatch: v, had: v != ""})
		n := len(reqs)
		mu.Unlock()

		if v == etag {
			w.Header().Set("ETag", etag)
			w.WriteHeader(http.StatusNotModified)
		} else {
			w.Header().Set("ETag", etag)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(UIConfig{
				ReportIntervalSeconds: 60,
				PollWaitSeconds:       1,
				Operations:            []Operation{promoteOp()},
			})
		}
		// One full exchange is all this test needs; the second request
		// proves the ETag is used from then on. Stop after it, whichever
		// way it was answered.
		if n >= 2 {
			cancel()
		}
	})

	// A process that has just restarted: the cache knows the ETag but,
	// by construction, holds none of the operations it attests to.
	state := &sharedState{cached: CachedConfig{ETag: etag, Config: DefaultUIConfig()}}
	store := Store{Dir: t.TempDir()}

	done := make(chan struct{})
	go func() {
		defer close(done)
		pollLoop(ctx, client, newLive(agentConfig{}), store, state, newAgentMetrics("test", "npftest", modeControlled))
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		cancel()
		<-done
		t.Fatal("pollLoop did not finish; the stub never saw the requests it expected")
	}

	mu.Lock()
	seen := append([]seenReq(nil), reqs...)
	mu.Unlock()

	if len(seen) == 0 {
		t.Fatal("pollLoop made no request")
	}
	if seen[0].had {
		t.Errorf("the first poll of this process sent If-None-Match: %s -- a UI still publishing a failover answers 304 and the operation is never delivered", seen[0].ifNoneMatch)
	}
	if got := state.get().Config.Operations; len(got) != 1 {
		t.Errorf("after the first poll the agent holds %d operation(s), want the pending one delivered", len(got))
	}
	// A full document arrived, so the published set is now known and
	// operationsPass may prune against it. Without this, the guard that
	// stops the restart wipe would never lift and the ledger would grow
	// for the life of the process.
	if _, known := state.snapshot(); !known {
		t.Error("a full configuration document was adopted without recording its provenance; the operation ledger would never be pruned again")
	}
	// ...and the ETag is still used afterwards, so a stable estate keeps
	// long-polling instead of refetching the document every time.
	if len(seen) > 1 && !seen[1].had {
		t.Error("the second poll also went unconditional; long polling would refetch the whole document on every cycle")
	}
}

// pollOnceAgainst runs pollLoop against a stub UI that answers every
// unconditional request with cfg under etag, and 304 to a matching one. It
// returns after the exchange that follows the first, so the first 200 has
// been fully processed.
func pollOnceAgainst(t *testing.T, cachedETag, etag string, cfg UIConfig) *sharedState {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	n := 0
	client, _ := stubUI(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		seen := n
		mu.Unlock()

		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
		} else {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(cfg)
		}
		if seen >= 2 {
			cancel()
		}
	})

	state := &sharedState{cached: CachedConfig{ETag: cachedETag, Config: DefaultUIConfig()}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		// HTTPTimeout 0 keeps the poll-wait warning quiet, so the only
		// warnings this test can see are the ones it is counting.
		pollLoop(ctx, client, newLive(agentConfig{}), Store{Dir: t.TempDir()}, state,
			newAgentMetrics("test", "npftest", modeControlled))
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		cancel()
		<-done
		t.Fatal("pollLoop did not finish")
	}
	return state
}

// The unconditional first poll means a 200 no longer implies the document
// changed, and two things downstream were reading it as though it did.
//
// complainAbout's placement is justified by "once per actual change rather
// than every thirty seconds forever" -- a host crash-looping under
// Restart=always would otherwise reproduce that flood at the restart rate.
// And "configuration updated from the UI" is the line an operator greps to
// answer "when did the control plane last change this host?", which is worth
// nothing if it also appears on every start.
func TestAnUnchangedFirstPollNeitherComplainsNorClaimsAnUpdate(t *testing.T) {
	// MaxConcurrentSyncs survives Normalize (store.go only normalizes the
	// two interval fields), so a negative value really does reach
	// Complaints() through the poll path.
	bad := DefaultUIConfig()
	bad.MaxConcurrentSyncs = -1

	const etag = `"unchanged"`
	before := trace.WarningCount()
	state := pollOnceAgainst(t, etag, etag, bad)
	after := trace.WarningCount()

	if after != before {
		t.Errorf("an unchanged document logged %d warning(s) on the first poll of a process; a crash-looping host would repeat the whole complaint block at its restart rate", after-before)
	}
	// ...while still being adopted: the point of the unconditional poll is
	// that the agent ends up holding the real document.
	if _, known := state.snapshot(); !known {
		t.Error("the document was not adopted; the unconditional first poll exists precisely so that it is")
	}
}

// The control. A document that genuinely differs must still be complained
// about -- the gate above must not silence the warning it was protecting.
func TestAChangedDocumentStillComplains(t *testing.T) {
	bad := DefaultUIConfig()
	bad.MaxConcurrentSyncs = -1

	before := trace.WarningCount()
	pollOnceAgainst(t, `"what-we-had"`, `"something-new"`, bad)
	after := trace.WarningCount()

	if after <= before {
		t.Error("a changed document with an out-of-range setting logged no warning; the operator is never told their configuration was corrected")
	}
}
