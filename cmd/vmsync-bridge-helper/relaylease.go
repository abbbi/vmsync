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
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// defaultRelayLease is how long a relay keeps listening with nothing using it.
//
// THE RELAY HAS TO BE ABLE TO END ITSELF, because the only process that would
// otherwise end it is on another host and may be gone. vmsync starts this
// helper over SSH under setsid, so it survives the SSH channel by design --
// and therefore also survives its driver being killed, losing power, or
// falling off the network. Nothing on this side then knows the sync is over:
// the accept loop waits forever on a port nobody will connect to, dialling a
// qemu-nbd that exited hours ago.
//
// Observed, not theorised: a bench run whose driver was SIGKILLed left four
// helpers listening on a production target for over two hours, three of them
// still named by a pid file and one of them not named anywhere at all.
//
// The same answer the target-side run lock already uses, for the same reason
// -- see runlock.go, which puts it better: "A lease answers that with a clock
// on THIS side of the link." The difference is only what counts as a sign of
// life. The run lock wants a heartbeat because a holder can legitimately hold
// it while doing nothing visible. A relay cannot: it exists to carry a
// connection, so a connection IS the heartbeat, and the absence of one for
// long enough means nobody is coming.
//
// WHAT THIS DOES NOT COVER, and it is worth knowing: a connection counts as
// alive until handleConn returns, and handleConn waits for BOTH relay
// directions. If the far client vanishes but the local server holds its half
// of the connection open forever, the lease stays suspended. In practice the
// local server is qemu-nbd, which closes when its client goes away, and the
// leak actually observed never reached a connection at all -- the helpers had
// zero-byte logs, meaning nothing ever dialled them. Covering the wedged-peer
// case too would mean measuring bytes rather than connections, which is what
// supervise.go's -stall-after already does for target commands; that is the
// escalation if one is ever seen.
//
// Fifteen minutes is far longer than any legitimate gap. vmsync starts a
// bridge immediately before the export it fronts and connects within seconds;
// while a copy runs the connection is open, which suspends the lease
// entirely. The value is generous because the cost of being wrong is
// asymmetric: expiring early kills a sync, expiring late just leaves a
// listener a little longer.
//
// DELIBERATELY A DEFAULT RATHER THAN SOMETHING vmsync PASSES. A helper has to
// be safe when started by something that has never heard of this flag --
// which includes every vmsync built before it existed. Passing it from the
// driver would put the value in `ps` where it would have helped the
// diagnosis, so the startup log line below names it instead.
const defaultRelayLease = 15 * time.Minute

// relayLease tracks whether anything is using the relay.
//
// Live connections suspend it: a sync that takes six hours holds a connection
// for six hours, and a lease that counted elapsed time rather than idle time
// would kill it. Only the gaps count.
type relayLease struct {
	lease time.Duration
	now   func() time.Time

	mu sync.Mutex
	// idle is broadcast when live reaches zero, for drain.
	idle *sync.Cond
	// live is how many connections are open right now. While this is above
	// zero the relay is in use and the lease does not apply at all, which is
	// what lets a copy of any length run under it.
	live int
	// idleSince is when live last reached zero -- set by the constructor so
	// that a relay nothing ever connects to is on the same clock as one
	// abandoned halfway through, and only ever read while live is zero.
	idleSince time.Time
}

func newRelayLease(lease time.Duration, now func() time.Time) *relayLease {
	// Starts idle, so a helper whose driver dies before it ever connects is
	// reaped on the same clock as one abandoned halfway through. That is the
	// case the pid file cannot help with -- see writeOwnPIDFile.
	l := &relayLease{lease: lease, now: now, idleSince: now()}
	l.idle = sync.NewCond(&l.mu)
	return l
}

// drain blocks until no connection is open.
//
// For the instant between the lease deciding it has expired and the listener
// actually closing: a connection accepted in that window has already been
// handed to handleConn, and returning from serve would end the process
// underneath it. Waiting means such a connection is relayed to completion and
// the relay exits after it, which is the only reading under which a sync that
// started at the wrong moment is not broken by its own bridge.
func (l *relayLease) drain() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for l.live > 0 {
		l.idle.Wait()
	}
}

// begin records a connection being accepted.
func (l *relayLease) begin() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.live++
	// idleSince is deliberately left alone: live being above zero is what
	// suspends the lease, and end() restarts the clock when it returns to
	// zero. Zeroing it here as well was a second mechanism for one fact, and
	// could be deleted without a single test noticing.
}

// end records a connection finishing.
func (l *relayLease) end() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.live > 0 {
		l.live--
	}
	if l.live == 0 {
		l.idleSince = l.now()
		l.idle.Broadcast()
	}
}

// expired reports whether nothing has used the relay for the whole lease.
func (l *relayLease) expired() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.live > 0 {
		return false
	}
	return l.now().Sub(l.idleSince) >= l.lease
}

// relayLeaseTick is how often the lease is checked.
//
// A third of the lease, matching -lock-lease's own "vmsync beats at a third
// of this": close enough that an expired relay goes away promptly, coarse
// enough that the check costs nothing. Floored at a second so a tiny lease
// set by a test does not spin.
func relayLeaseTick(lease time.Duration) time.Duration {
	tick := lease / 3
	if tick < time.Second {
		return time.Second
	}
	return tick
}

// writeOwnPIDFile records this process's pid where vmsync can find it.
//
// WRITTEN BY THE HELPER, not by the shell that started it, and that is the
// whole point of the function.
//
// BuildStartCommand used to do it: `setsid sh -c '<helper>' & echo $! > pid`.
// The redirection runs AFTER the fork, in a shell that belongs to the SSH
// session -- so a driver that dies in that window takes the recording shell
// with it while setsid has already carried the helper out of reach. What is
// left is a listener that no pid file names, which no cleanup path in vmsync
// can see: killOrphanedRemoteBridge, BuildStopCommand and any future sweep
// all start by reading a pid file that was never written. One such helper was
// found on a production target with no record of it anywhere.
//
// The helper knows its own pid and cannot be interrupted between learning it
// and writing it down, so the record exists whatever happens to the far end.
//
// Written AFTER the listener is bound, so a helper that lost a port race
// leaves nothing behind to be mistaken for the winner, and removed on a clean
// exit. A kill -9 leaves it, which is why BuildStopCommand removes the file
// itself rather than trusting the process to.
func writeOwnPIDFile(path string) error {
	// Same directory, so the rename cannot cross a filesystem, and a unique
	// name so two helpers racing for one path cannot write each other's
	// halves. Renamed into place because a reader that catches a half-written
	// file reads a truncated pid -- and a truncated pid is a real pid
	// belonging to something else, which `kill -9 -$(cat pidfile)` would then
	// signal the process group of.
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op once the rename below succeeds

	if _, err := tmp.WriteString(strconv.Itoa(os.Getpid()) + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return nil
}

// relayStartupLine is what a relay says for itself the moment it is up.
//
// Its stdout and stderr go to the log file beside its pid file, and on every
// leaked helper found so far that file was ZERO BYTES: the process had said
// nothing at all, so there was no way to tell a helper that had never been
// connected to from one whose driver had died mid-sync, nor to see what it
// had been asked to do. One line at startup costs nothing and is the only
// record that survives the driver.
func relayStartupLine(cfg helperConfig, lease time.Duration) string {
	leaseDesc := lease.String()
	if lease <= 0 {
		leaseDesc = "none (this relay will listen until something stops it)"
	}
	return fmt.Sprintf("vmsync-bridge-helper: relaying %s -> %s, pid %d, idle lease %s",
		cfg.ListenAddr, cfg.ConnectAddr, os.Getpid(), leaseDesc)
}
