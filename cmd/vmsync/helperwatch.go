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
	"sync"
	"time"

	"vmsync/pkg/trace"
)

// helperWatch bounds a wait on vmsync-bridge-helper by how long it has been
// SILENT, and is the one implementation of that rule.
//
// Both things vmsync waits on the helper for -- a digest pass and a
// supervised target command -- beat once per trace.ProgressInterval while
// they work. That beat is what makes a bound possible at all: how LONG the
// far side takes is not knowable from here (a terabyte of digests is most of
// an hour, a qemu-img commit as long as the storage needs), but whether it is
// still there is, and silence is the only honest evidence either way.
//
// One type rather than the same twenty lines at both call sites, because the
// two would drift and the drift would be invisible: a bound that is quietly
// never armed looks exactly like a peer that never fails.
type helperWatch struct {
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	start  time.Time

	mu        sync.Mutex
	lastHeard time.Time
	lastLine  string
	heardAny  bool
	gaveUp    bool
}

// watchHelperSilence derives a context for one helper exchange and abandons
// it once the helper has been quiet for targetDigestStallBound.
//
// The context is derived and scoped to the single call, NEVER the run's own:
// what happens immediately after a trip is the target-side export being torn
// down and the run lock released, both over the same SSH connection, and
// Client.Run answers a cancelled context before it even opens a session.
// Cancelling the run's context here would trade an audible hang for a silent
// leak.
//
// remark is called on every tick past targetDigestQuietGrace but short of the
// bound, which is what makes a long wait audible before it becomes a failure.
func watchHelperSilence(parent context.Context, remark func(quiet, elapsed time.Duration)) *helperWatch {
	ctx, cancel := context.WithCancel(parent)
	now := time.Now()
	w := &helperWatch{
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
		start:  now,
		// Seeded with the start, not with the first line, so the bound covers
		// a helper that never says anything at all -- one that never started,
		// or hung before its first beat.
		lastHeard: now,
	}
	go func() {
		t := time.NewTicker(trace.ProgressInterval)
		defer t.Stop()
		quietFor := func(at time.Time) time.Duration {
			w.mu.Lock()
			defer w.mu.Unlock()
			return at.Sub(w.lastHeard)
		}
		say := func(quiet time.Duration) {
			if remark != nil {
				remark(quiet, time.Since(w.start))
			}
		}
		if trace.QuietWatch(w.done, t.C, quietFor, targetDigestQuietGrace, targetDigestStallBound, say) {
			w.mu.Lock()
			w.gaveUp = true
			w.mu.Unlock()
			cancel()
		}
	}()
	return w
}

// heard records a line from the helper. Called from the goroutine that copies
// the command's stderr, so it must stay cheap.
func (w *helperWatch) heard(line string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lastHeard = time.Now()
	w.lastLine = line
	w.heardAny = true
}

// stop ends the watch. Always called, including on the error path: a watch
// left running would hold its goroutine and its ticker for the rest of the
// run.
func (w *helperWatch) stop() {
	close(w.done)
	w.cancel()
}

// verdict is what to say about a wait that ended badly.
//
// stalled is the only field that decides anything; the rest are for the
// message. They are worth carrying because this is vmsync's ONLY witness:
// RunWithInputProgress discards both its stdout and stderr buffers on the
// cancellation path, so by the time the caller formats an error the helper's
// own words exist nowhere else.
func (w *helperWatch) verdict() (stalled bool, lastLine string, heardAny bool, quiet, elapsed time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.gaveUp, w.lastLine, w.heardAny, time.Since(w.lastHeard), time.Since(w.start)
}
