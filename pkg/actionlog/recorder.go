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

package actionlog

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"
)

// Sink appends one already-encoded record where the journal lives.
//
// A function rather than an interface because the two implementations have
// nothing in common beyond this: one is an SSH command reading the record from
// stdin on the target host, the other is an O_APPEND write on this one. Both
// live in cmd/vmsync, which is where libvirt and SSH are allowed; keeping the
// seam this narrow is what lets the whole of the journal's behaviour be tested
// on a machine with neither.
type Sink func(ctx context.Context, line []byte) error

// Identity is everything a record says about WHO is writing it, fixed once for
// the life of a process. Nothing here changes between one action and the next.
type Identity struct {
	// ActionID is the correlation id. It is what ties a record to the
	// replica_incomplete marker on the domain and to the control plane's
	// operation, and it is the first half of the (aid, seq) join. Empty is
	// allowed and means "no correlation available" -- a record with no id is
	// far better than no record.
	ActionID string
	// Domain is the domain being acted on: the TARGET of a sync, not its
	// source, because the journal describes the replica it sits beside.
	Domain string
	// Host is the machine RUNNING the action. See Record.Host for why it is
	// not the same as the machine the file is written to.
	Host  string
	PID   int
	RunID string
	// OpID is a control plane operation id, when one launched this.
	OpID string
	// By is whoever asked for it, for the verbs that carry an actor.
	By string
}

// Recorder writes one intent and one outcome per action.
//
// THE LEVEL IS "actions" AND ONLY "actions": one record when a verb starts and
// one when it stops. Per-step records were considered and deliberately left
// out -- a journal that writes a line per disk turns a 40-disk reinit into
// hundreds of records on the very filesystem the replica lives on, and the
// question it would answer ("which disk was it on") is already answered by the
// disks themselves. The schema keeps "step" and "seq" so the decision can be
// revisited without a format change.
//
// A NIL *Recorder IS VALID and every method on it does nothing. That is what
// -journal=off is, and it is also what a verb with nowhere to write gets, so
// no call site needs a conditional around journaling. A conditional is exactly
// how a caller ends up journaling in one branch and not another.
type Recorder struct {
	sink  Sink
	id    Identity
	onErr func(error)
	now   func() time.Time

	mu       sync.Mutex
	seq      int
	verb     string
	failures int
}

// New builds a recorder.
//
// onErr is how a write failure is reported, and it is a callback rather than a
// log call so this package needs no logging policy of its own and so a test
// can see exactly what a caller would have been told. cmd/vmsync passes a
// trace.Warning closure. Nil is allowed and means the count is the only
// report.
//
// A NIL SINK RETURNS A NIL RECORDER, so "there is nowhere to write" and
// "-journal=off" are the same object downstream and cannot drift apart.
func New(sink Sink, id Identity, onErr func(error)) *Recorder {
	if sink == nil {
		return nil
	}
	if id.PID == 0 {
		id.PID = os.Getpid()
	}
	return &Recorder{sink: sink, id: id, onErr: onErr, now: time.Now}
}

// Intent records what this action is ABOUT to do, and returns only once the
// record has actually been handed to the sink.
//
// SYNCHRONOUS, never buffered, and that is the entire mechanism. The record's
// value is that it is on the target's filesystem BEFORE the first step that
// can displace anything -- a buffered write flushed at exit is a record that a
// power cut, a SIGTERM or a dropped link removes at precisely the moment it
// would have been worth having. Callers must place this before the first
// destructive step and rely on it having landed when it returns.
//
// It cannot fail: a write failure is counted and reported through onErr, and
// the action carries on. Refusing to sync because a diagnostic could not be
// written would make the journal a thing that causes outages.
func (r *Recorder) Intent(ctx context.Context, verb string, detail map[string]string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.seq++
	r.verb = verb
	seq := r.seq
	r.mu.Unlock()

	r.write(ctx, Record{
		Kind:   KindIntent,
		Seq:    seq,
		At:     r.now().Unix(),
		Verb:   verb,
		Detail: detail,
	})
}

// Outcome records how the action ended, whatever way it ended.
//
// result is stated by the caller rather than inferred from err, because
// standing down on a held lock is not a failure and recording it as one would
// make every scheduled overlap read as a broken replica. exit is the process
// exit code the caller is about to produce, so a reader can line the journal up
// against whatever launched vmsync.
//
// An action that never reaches this leaves an intent with no outcome, and the
// reader reports it as exactly that. THAT IS NOT A GAP, it is the finding: a
// run killed by a signal (whose handler calls os.Exit, so nothing deferred
// runs), by a lost link or by a power cut is precisely the run CI-02 is about,
// and its silence here is the evidence that it happened.
func (r *Recorder) Outcome(ctx context.Context, result string, exit int, err error, detail map[string]string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.seq == 0 {
		// An outcome with no intent before it. Should not happen, and is
		// recorded rather than dropped: a caller that got the order wrong is
		// itself worth seeing in the journal.
		r.seq++
	}
	seq := r.seq
	verb := r.verb
	r.mu.Unlock()

	rec := Record{
		Kind:   KindOutcome,
		Seq:    seq,
		At:     r.now().Unix(),
		Verb:   verb,
		Detail: detail,
		Result: result,
		Exit:   exit,
	}
	if err != nil {
		rec.Err = err.Error()
	}
	r.write(ctx, rec)
}

// Failures is how many records could not be written.
//
// Exposed so a run can say so once at the end rather than only per failure: a
// journal that is silently failing every write looks exactly like a journal
// nothing is writing to, and the difference matters to whoever comes looking
// for an intent that is not there.
func (r *Recorder) Failures() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failures
}

// write stamps the identity onto a record and hands it to the sink.
func (r *Recorder) write(ctx context.Context, rec Record) {
	rec.Action = r.id.ActionID
	rec.Domain = r.id.Domain
	rec.Host = r.id.Host
	rec.PID = r.id.PID
	rec.RunID = r.id.RunID
	rec.OpID = r.id.OpID
	rec.By = r.id.By

	if err := r.sink(ctx, rec.Encode()); err != nil {
		r.mu.Lock()
		r.failures++
		r.mu.Unlock()
		if r.onErr != nil {
			r.onErr(fmt.Errorf("could not write the %s record for %s: %w", rec.Kind, rec.Verb, err))
		}
	}
}
