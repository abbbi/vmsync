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
// The lifetime coupling is the entire point. The remote command blocks
// reading its stdin, so it exits when this handle is closed -- and also
// when the SSH connection drops, when the local process is killed, and when
// the network partitions, because every one of those closes the channel and
// delivers EOF. Anything the remote command holds is therefore released by
// the same event that ends the local run, with no timeout to tune, no
// heartbeat to miss, and no stale state to reason about afterwards.
type heldCommand struct {
	session *ssh.Session
	stdin   io.WriteCloser
	desc    string

	once     sync.Once
	closeErr error
}

// ErrHoldRefused reports that the remote command started but signalled a
// refusal rather than readiness.
var ErrHoldRefused = errors.New("remote command refused")

// HoldCommand starts command on the remote host and waits for it to print
// readyLine on stdout, then returns a handle whose Close ends it.
//
// Any other line the command prints before readyLine is treated as a
// refusal and returned in the error, so a caller can distinguish "could not
// do this" from "did not answer". A command that neither answers nor exits
// is bounded by ctx like everything else here.
func (c *Client) HoldCommand(ctx context.Context, command, readyLine string) (io.Closer, error) {
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

	h := &heldCommand{session: session, stdin: stdin, desc: readyLine}

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
