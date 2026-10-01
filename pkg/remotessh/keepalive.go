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

package remotessh

import (
	"sync"
	"time"

	"vmsync/pkg/trace"
)

// How long an unreachable peer goes unnoticed.
//
// Nothing else bounds it. A write into a TCP socket whose peer has vanished
// succeeds into the local send buffer and keeps succeeding; the kernel only
// gives up after its own retransmission budget, which on Linux is around
// fifteen minutes. So a vmsync holding the target's run lock can go that long
// believing it still holds a lock the target gave away minutes ago.
//
// THE PAIRING RULE. This window must stay comfortably SHORTER than
// util.DefaultRemoteLockLease, because the lease is when the target hands the
// lock to somebody else. A driver that notices after the hand-over would commit
// into disks a promoted guest is already writing. 10 s between probes, 10 s for
// a reply and three strikes puts detection at ~40 s against a 90 s lease, which
// leaves room for two lost probes on a link that is merely slow.
//
// Not to be confused with sshd's ClientAliveInterval on the far side, whose
// pairing runs the other way: that one reaps the session holding the lock, so it
// must be LONGER than the lease, or the two would both decide the lock is free.
const (
	keepaliveInterval = 10 * time.Second
	keepaliveTimeout  = 10 * time.Second
	keepaliveStrikes  = 3
)

// keepaliveRequest is OpenSSH's own no-op request type. Any sshd answers it,
// and the answer is the only thing being asked for -- a reply proves the peer is
// still processing requests, which a TCP ACK from its kernel does not.
const keepaliveRequest = "keepalive@openssh.com"

// startKeepalive probes the peer until the client is closed, and closes the
// client itself once the peer has stopped answering.
//
// Closing is the point. Every channel on this connection fails as soon as it
// goes, which is what turns "the peer is unreachable" into an error the callers
// holding something over this connection can see -- a failed heartbeat write
// and a session that reports it exited. Without that, those callers wait on the
// kernel.
func (c *Client) startKeepalive() {
	if c == nil || c.client == nil {
		return
	}
	c.keepaliveStop = make(chan struct{})
	c.keepaliveDone.Add(1)
	t := time.NewTicker(keepaliveInterval)
	go func() {
		defer c.keepaliveDone.Done()
		defer t.Stop()
		if !keepaliveLoop(c.keepaliveStop, t.C, c.probe, keepaliveStrikes) {
			return
		}
		trace.Warning("the ssh connection to this host stopped answering keepalives; closing it so anything held over it fails now rather than when the kernel gives up",
			"host", c.cfg.Address, "probes", keepaliveStrikes,
			"after", (keepaliveInterval * keepaliveStrikes).String())
		_ = c.client.Close()
	}()
}

// keepaliveLoop probes on each tick and reports whether the peer should be
// given up on.
//
// Consecutive failures, reset by any success, which is the distinction that
// matters: a single lost probe on a busy link is not a dead peer, and treating
// it as one would close a healthy connection and fail a running sync. Only an
// unbroken run of them is evidence.
//
// Pure with respect to the clock and the connection -- both arrive as arguments
// -- so the strike logic is provable without a peer to disconnect.
func keepaliveLoop(stop <-chan struct{}, tick <-chan time.Time, probe func() bool, strikes int) bool {
	missed := 0
	for {
		select {
		case <-stop:
			return false
		case _, open := <-tick:
			if !open {
				return false
			}
		}
		if probe() {
			missed = 0
			continue
		}
		missed++
		if missed >= strikes {
			return true
		}
	}
}

// probe sends one keepalive and reports whether a reply came back in time.
func (c *Client) probe() bool {
	return probeWithin(func() error {
		_, _, err := c.client.SendRequest(keepaliveRequest, true, nil)
		return err
	}, keepaliveTimeout)
}

// probeWithin bounds a send that has no deadline of its own.
//
// SendRequest against a vanished peer blocks exactly as long as the kernel does,
// which is the thing being bounded here, so the reply is awaited in a goroutine
// and abandoned on timeout. That goroutine outlives the timeout until the
// connection closes -- which is what the caller does after enough of these fail.
//
// A server that does not recognise the request type still REPLIES, and the reply
// is all that was wanted: it proves the peer is still processing requests, which
// an ACK from its kernel does not. Only a transport error counts against it.
func probeWithin(send func() error, timeout time.Duration) bool {
	done := make(chan error, 1)
	go func() { done <- send() }()
	select {
	case err := <-done:
		return err == nil
	case <-time.After(timeout):
		return false
	}
}

// stopKeepalive ends the prober and waits for it, so a closed client leaves no
// goroutine probing a connection nobody is using.
func (c *Client) stopKeepalive() {
	if c == nil || c.keepaliveStop == nil {
		return
	}
	c.keepaliveStopOnce.Do(func() { close(c.keepaliveStop) })
	c.keepaliveDone.Wait()
}

// keepaliveState is embedded in Client; kept here beside the prober that uses it.
type keepaliveState struct {
	keepaliveStop     chan struct{}
	keepaliveStopOnce sync.Once
	keepaliveDone     sync.WaitGroup
}
