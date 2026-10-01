/*
	Copyright (C) 2026  Michael Ablassmeier <abi@grinser.de>

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
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
)

// heldCommand is a remote command kept running for as long as the handle is
// open, rather than run to completion like Run does.
//
// The remote command blocks reading its stdin, so it exits when this handle is
// closed, when the SSH connection drops, and when the local process is killed:
// each of those closes the channel and delivers EOF, and anything the remote
// command holds is released with it.
//
// A PARTITION OR A POWER LOSS IS DIFFERENT, and the difference is the whole of
// CI-10. Nothing is delivered to the far end at all -- no FIN, no reset -- so the
// remote command blocks on a stdin that will never close, and sshd notices only
// when TCP keepalive gives up, roughly two hours later. Callers that hold
// something whose release matters cannot rely on EOF for it. Write to this
// handle periodically instead, and have the remote side give up on silence:
// util.AcquireRemoteRunLock does exactly that, and the heartbeat is why Write
// exists here.
type heldCommand struct {
	session *ssh.Session
	stdin   io.WriteCloser
	desc    string

	// exited carries the remote command's own exit, once. It is what makes a
	// caller's loss of whatever the command held PROMPT rather than discovered
	// on the next write: a lock holder that releases on its own exits, and that
	// exit arrives here.
	exited chan error

	once     sync.Once
	closeErr error
}

// Exited delivers the remote command's exit and is closed afterwards.
//
// A nil error means it ended cleanly, which for a command holding something on
// the caller's behalf means it let go deliberately -- a lease that expired, for
// instance. Either way, receiving anything here means the caller no longer holds
// what the command was holding.
func (h *heldCommand) Exited() <-chan error { return h.exited }

// ErrHoldRefused reports that the remote command started but signalled a
// refusal rather than readiness.
var ErrHoldRefused = errors.New("remote command refused")

// HoldCommand starts command on the remote host and waits for it to print
// readyLine on stdout, then returns a handle whose Close ends it and whose Write
// reaches the command's stdin.
//
// Any other line the command prints before readyLine is treated as a
// refusal and returned in the error, so a caller can distinguish "could not
// do this" from "did not answer". A command that neither answers nor exits
// is bounded by ctx like everything else here.
//
// Nothing drains the command's stdout after the ready line. A command a caller
// writes to must therefore not echo what it reads, or the channel's window fills
// and both ends block -- see the remote script in util.AcquireRemoteRunLock,
// which sends its reader's output to /dev/null for that reason.
func (c *Client) HoldCommand(ctx context.Context, command, readyLine string) (io.WriteCloser, error) {
	if c == nil || c.client == nil {
		return nil, errors.New("ssh client is not connected")
	}

	session, err := c.client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("open ssh session: %w", err)
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		session.Close()
		return nil, fmt.Errorf("open stdin to remote command: %w", err)
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		session.Close()
		return nil, fmt.Errorf("open stdout from remote command: %w", err)
	}
	if err := session.Start(command); err != nil {
		session.Close()
		return nil, fmt.Errorf("start remote command: %w", err)
	}

	h := &heldCommand{session: session, stdin: stdin, desc: readyLine, exited: make(chan error, 1)}
	// Reaped here rather than in Close, so the exit is observable while the
	// handle is still open. Wait returns for every reason the command can end,
	// including the connection being closed under it, which is what the
	// keepalive prober does to an unreachable peer.
	go func() {
		err := session.Wait()
		h.exited <- err
		close(h.exited)
	}()

	type firstLine struct {
		line string
		err  error
	}
	lineCh := make(chan firstLine, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		if sc.Scan() {
			lineCh <- firstLine{line: strings.TrimSpace(sc.Text())}
			return
		}
		// No line at all: the command exited (or the channel closed) without
		// saying anything, which is itself the answer.
		lineCh <- firstLine{err: sc.Err()}
	}()

	select {
	case <-ctx.Done():
		_ = h.Close()
		return nil, fmt.Errorf("waiting for remote command to become ready: %w", ctx.Err())
	case r := <-lineCh:
		switch {
		case r.err != nil:
			_ = h.Close()
			return nil, fmt.Errorf("remote command produced no output: %w", r.err)
		case r.line == readyLine:
			return h, nil
		case r.line == "":
			_ = h.Close()
			return nil, fmt.Errorf("%w: it exited without saying why", ErrHoldRefused)
		default:
			_ = h.Close()
			return nil, fmt.Errorf("%w: %s", ErrHoldRefused, r.line)
		}
	}
}

// Write sends bytes to the remote command's stdin.
//
// A failure means the channel is gone, which is the one signal this side gets
// that the remote command -- and anything it was holding on our behalf -- is no
// longer reachable. Callers treat that as having lost whatever they held.
func (h *heldCommand) Write(p []byte) (int, error) {
	if h.stdin == nil {
		return 0, errors.New("remote command has no stdin")
	}
	return h.stdin.Write(p)
}

// Close ends the remote command by closing its stdin, then tears the
// session down. Safe to call more than once.
func (h *heldCommand) Close() error {
	h.once.Do(func() {
		// Closing stdin is what the remote command is waiting on, so this is
		// the graceful path: it sees EOF and exits, releasing whatever it
		// held. Closing the session as well covers a command that ignores
		// its stdin, and is harmless when it did not.
		if h.stdin != nil {
			_ = h.stdin.Close()
		}
		if h.session != nil {
			h.closeErr = h.session.Close()
			// An already-finished session reports io.EOF from Close. That is
			// the expected outcome here, not a failure.
			if errors.Is(h.closeErr, io.EOF) {
				h.closeErr = nil
			}
		}
	})
	return h.closeErr
}

// The handle reports its command's exit as well as being a WriteCloser, which
// util.AcquireRemoteRunLock looks for to learn promptly that a lock holder let
// go. Asserted here so renaming the method breaks the build rather than quietly
// costing that promptness: pkg/util finds it by shape, because it deliberately
// does not import this package.
var _ interface {
	io.WriteCloser
	Exited() <-chan error
} = (*heldCommand)(nil)
