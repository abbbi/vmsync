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

func (l *RemoteRunLock) setLost(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lost == nil {
		l.lost = err
	}
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
