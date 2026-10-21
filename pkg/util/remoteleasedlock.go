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

package util

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// DefaultRemoteLockLease is how long a leased target-side lock survives silence
// from the driver that took it.
//
// 90 seconds, with the driver beating every 30, so two beats can be lost before
// the lock goes. That margin is sized for the things that legitimately stop a
// healthy driver for tens of seconds -- a hypervisor stalling the whole guest
// while it snapshots or migrates it, a long stop-the-world pause, a link that
// retransmits for half a minute. Under all of those the lock must NOT move: the
// driver is still going to write to the replica. Set against that, a real
// disaster costs an extra minute and a half before the replica can be promoted,
// which is nothing next to the two hours it costs without a lease at all.
const DefaultRemoteLockLease = 90 * time.Second

// ErrRemoteLockLost reports that this process can no longer prove it holds the
// target-side run lock.
//
// Treated as "the lock is gone", never as "the lock is probably still fine". The
// far end may have released it on lease expiry and handed it to a promotion, so
// anything that would write to the replica has to stop.
var ErrRemoteLockLost = errors.New("the target-side run lock was lost")

// heldReasonSuffix renders what a holder said for appending to a loss, or ""
// when it said nothing.
//
// A suffix rather than a wrapped error, for the reason pkg/remotessh gives for
// its own: this is the remote command's words, not a Go error, and errors.Is on
// ErrRemoteLockLost has to keep working for every caller that refuses on it.
func heldReasonSuffix(said string) string {
	if said == "" {
		return ""
	}
	return ", saying: " + said
}

// RemoteRunLock is a held target-side run lock.
//
// Two kinds, and a caller has to be able to tell them apart because they make
// different promises. A LEASED lock is held by vmsync-bridge-helper and is
// released by the target itself when this process stops beating, including when
// this host loses power. An unleased one is held by a POSIX shell parked on a
// read that never times out: it is released when the SSH channel closes, and
// after a partition or a power loss that does not happen until TCP keepalive
// gives up -- roughly two hours, during which the replica cannot be promoted.
type RemoteRunLock struct {
	handle io.WriteCloser
	path   string
	lease  time.Duration
	leased bool

	// fellBackBecause records why the leased holder could not be used, so the
	// caller can say so once rather than every caller having to ask.
	fellBackBecause error

	stop     chan struct{}
	stopOnce sync.Once
	beating  sync.WaitGroup

	mu   sync.Mutex
	lost error
	// lostAuthoritative records whether lost came from the holder itself
	// rather than from an observation here. See setLostAuthoritatively.
	lostAuthoritative bool
}

// startHeartbeat begins telling the far end that this process is still here.
//
// Beats are sent on both kinds of lock, although only a leased one releases
// itself on silence. On an unleased one the beat still earns something: a failed
// write is the only signal this side gets that the channel -- and therefore the
// lock -- is gone, which is what Lost reports and what the gates before commit
// and before define refuse on.
func startHeartbeat(handle io.WriteCloser, path string, lease time.Duration, leased bool) *RemoteRunLock {
	l := &RemoteRunLock{
		handle: handle,
		path:   path,
		lease:  lease,
		leased: leased,
		stop:   make(chan struct{}),
	}
	interval := lease / 3
	if interval <= 0 {
		interval = lease
	}
	// A holder that can say when it exited is watched for it, which is the
	// prompt signal: a leased holder whose lease ran out EXITS, and that is the
	// instant the lock became somebody else's. Found by shape rather than by a
	// named type, because this package deliberately does not import the SSH one;
	// a holder without it still works, just discovering the loss on the next
	// beat instead.
	// What the holder said, when the handle can tell us. Optional for the same
	// reason Exited is: this package finds both by shape and a handle without
	// them still works, just less informatively.
	said := func() string { return "" }
	if s, ok := handle.(interface{ Said() string }); ok {
		said = s.Said
	}
	if ex, ok := handle.(interface{ Exited() <-chan error }); ok {
		l.beating.Add(1)
		go func() {
			defer l.beating.Done()
			select {
			case <-l.stop:
				return
			case err, open := <-ex.Exited():
				if !open {
					return
				}
				// The exit status says THAT it ended; stderr says why, and
				// the two do not agree about which cases matter. A lease
				// that expired exits 0, so without this the most ordinary
				// reason for losing a target lock arrives as a nil error
				// and no explanation at all.
				l.setLostAuthoritatively(fmt.Errorf("%w: the process holding %s on the target %s%s", ErrRemoteLockLost, path, heldExitPhrase(err), heldReasonSuffix(said())))
			}
		}()
	}

	l.beating.Add(1)
	go func() {
		defer l.beating.Done()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-l.stop:
				return
			case <-t.C:
				// One newline. The content carries nothing: the only fact being
				// transmitted is that this process is still running to send it.
				if _, err := l.handle.Write([]byte("\n")); err != nil {
					l.setLost(fmt.Errorf("%w: heartbeat to %s failed: %v", ErrRemoteLockLost, path, err))
					return
				}
			}
		}
	}()
	return l
}

// setLost records a loss this side OBSERVED, and keeps the first one.
func (l *RemoteRunLock) setLost(err error) { l.recordLoss(err, false) }

// setLostAuthoritatively records a loss whose cause came from the holder
// itself, and it outranks an observation.
//
// WHY THERE ARE TWO. Both goroutines above can discover the same loss, and they
// know different amounts about it. The exit watcher knows the CAUSE: the holder
// ended, its status is in hand, and its reason is on stderr. The heartbeat only
// knows that its write failed -- which, when the holder has exited, is a
// consequence of that exit rather than an independent cause.
//
// They are woken separately, so the heartbeat's one pending tick can arrive
// first. With first-wins that reported "heartbeat to <path> failed: EOF" and
// silently discarded "run lock lease expired" arriving microseconds behind it:
// the symptom kept, the cause thrown away, in exactly the scenario the stderr
// plumbing was added for. The operator then reads a network fault where the
// truth is that the target released the lock and may already have given it to a
// promotion.
//
// Only this direction. An observation never overwrites a cause, and a cause
// never overwrites another cause. A genuine partition is unaffected and must
// be: nothing exits there, no cause is ever offered, and the heartbeat failure
// is the only true thing anybody can say about it.
func (l *RemoteRunLock) setLostAuthoritatively(err error) { l.recordLoss(err, true) }

// recordLoss keeps the first loss, except that a cause outranks an observation.
//
// The `authoritative &&` conjunct is redundant today, and deleting it is an
// equivalent mutation no test can catch: at most one observation and one cause
// ever arrive, because the heartbeat goroutine returns after its first failed
// write and the exit fires once. Across all four reachable orderings -- neither,
// either alone, observation then cause, cause then observation -- both
// spellings behave identically.
//
// It stays because it is the half that will still be right if the heartbeat
// ever stops returning on failure. Without it, a second observation would
// replace the first, and after a cause had been recorded the ranking would then
// depend on nothing having beaten since -- which is the kind of coupling that
// is invisible until it is wrong.
func (l *RemoteRunLock) recordLoss(err error, authoritative bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lost == nil || (authoritative && !l.lostAuthoritative) {
		l.lost = err
		l.lostAuthoritative = authoritative
	}
}

// heldExitPhrase says how the holder ended.
//
// A nil error means it ended cleanly, which for a lock holder means it let go
// deliberately -- a lease that expired being the ordinary case. Rendering that
// through %v produced "exited (<nil>)", which is the least informative
// available spelling of the most common reason a target lock is lost.
func heldExitPhrase(err error) string {
	if err == nil {
		return "let go deliberately and exited cleanly"
	}
	return "exited (" + err.Error() + ")"
}

// Lost reports why this process can no longer prove it holds the lock, or nil
// while it still can.
//
// Checked at the points where losing the lock changes what is safe to do rather
// than continuously, because there is nothing useful to do about it in between:
// the run is either about to write something irreversible, in which case it must
// stop, or it is not.
func (l *RemoteRunLock) Lost() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lost
}

// Leased reports whether the target releases this lock on its own when this
// process stops beating.
//
// False means a stale lock has to be waited out or broken by hand, which is what
// BreakRemoteRunLock exists for and what callers warn about.
func (l *RemoteRunLock) Leased() bool { return l != nil && l.leased }

// FellBackBecause reports why the leased holder was not available, or nil when
// the lock is leased.
func (l *RemoteRunLock) FellBackBecause() error {
	if l == nil {
		return nil
	}
	return l.fellBackBecause
}

// Lease is how long the far end waits before releasing a leased lock.
func (l *RemoteRunLock) Lease() time.Duration {
	if l == nil {
		return 0
	}
	return l.lease
}

// Close stops beating and releases the lock.
//
// Safe on a nil receiver and safe to call more than once, so a caller can defer
// it next to an acquisition that may have failed.
func (l *RemoteRunLock) Close() error {
	if l == nil {
		return nil
	}
	l.stopOnce.Do(func() { close(l.stop) })
	// Waited for, so the handle is never closed underneath an in-flight write:
	// that races the SSH channel's own state and turns an ordinary release into
	// an error the caller cannot act on.
	l.beating.Wait()
	return l.handle.Close()
}

// MinRemoteLockLease is the shortest lease that can be asked for.
//
// 10 seconds, and the floor exists for one reason: a lease shorter than the
// stalls a HEALTHY driver legitimately has will hand the lock to somebody else
// while this run is still writing to the replica. A hypervisor pausing the guest
// to snapshot it, a long stop-the-world pause and a link retransmitting all cost
// seconds; two writers on one replica costs the replica.
//
// Low enough to be testable -- the bench cannot spend 90 seconds per assertion --
// and high enough that no realistic scheduling hiccup crosses it.
const MinRemoteLockLease = 10 * time.Second
