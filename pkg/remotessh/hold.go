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

// maxHoldStderrTail bounds what is kept of a refused command's stderr.
//
// 4 KiB because the useful part is the FIRST thing said: a missing binary, a
// permission denial or a shell syntax error names itself immediately and then
// usually says nothing else. A command that fails by printing without end
// must not be able to grow this, and truncating the front to keep the tail
// would throw away exactly the sentence worth having.
const maxHoldStderrTail = 4 << 10

// holdStderr collects what a held command said on stderr, so a refusal can
// report the reason instead of reporting that there was none.
//
// Mutex-guarded, because x/crypto/ssh copies the channel's stderr into this
// writer from a goroutine of its own and the branches that read it do so
// while that goroutine may still be running. A bare bytes.Buffer here is a
// data race, and one on a path taken only when something has already gone
// wrong -- the worst place to put a rare crash.
type holdStderr struct {
	mu   sync.Mutex
	buf  []byte
	full bool
}

func (s *holdStderr) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Always reports the whole write as accepted: this is a diagnostic sink,
	// and a short write would make x/crypto/ssh treat a full buffer as a
	// broken stream and tear down a session that is working.
	if s.full {
		return len(p), nil
	}
	room := maxHoldStderrTail - len(s.buf)
	if room <= 0 {
		s.full = true
		return len(p), nil
	}
	if len(p) > room {
		s.buf = append(s.buf, p[:room]...)
		s.full = true
		return len(p), nil
	}
	s.buf = append(s.buf, p...)
	return len(p), nil
}

// String is what the remote command said, trimmed, or "" when it said
// nothing.
func (s *holdStderr) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.TrimSpace(string(s.buf))
}

// holdStderrSuffix renders what the command said for appending to an error,
// or "" when it said nothing.
//
// A suffix rather than a wrapped error: this is the remote command's words,
// not a Go error, and errors.Is on anything above must keep working.
func holdStderrSuffix(said string) string {
	if said == "" {
		return ""
	}
	return ": " + said
}

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

	// said is the stderr buffer x/crypto/ssh keeps filling for this session's
	// whole life. Held here because the command's own explanation of why it
	// ended is written there, and without a reference it is unreachable the
	// moment HoldCommand returns: the buffer stays alive, attached to the
	// session, with nothing able to read it.
	said *holdStderr

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

// Said is what the remote command wrote to stderr, or "" if it wrote nothing.
//
// WHY THIS IS NOT FOLDED INTO Exited's ERROR. The reason a holder let go is on
// stderr REGARDLESS of how it exited, and the two cases that matter most differ
// in exit status. vmsync-bridge-helper prints "run lock lease expired" and exits
// ZERO -- its own runLockExit calls that "the lock doing its job ... the outcome
// it exists to produce" -- while a dispossessed holder prints why and exits 1.
// Wrapping only the non-nil error would therefore deliver the dispossession and
// lose the lease expiry, which is the one an operator asks about most. Wrapping
// the nil one too would destroy the distinction Exited documents, where nil
// means the command let go deliberately rather than failed.
//
// COMPLETE ONCE Exited HAS FIRED, and that is not incidental. session.Wait
// drains the session's copy goroutines before it returns -- the stderr copier
// among them -- and the exit is published only after Wait returns, so a caller
// that reads this after receiving from Exited sees everything the command said.
// Reading it earlier is safe but may see less; holdStderr is mutex-guarded, so
// it is never a race.
func (h *heldCommand) Said() string {
	if h.said == nil {
		return ""
	}
	return h.said.String()
}

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
	// Attached BEFORE Start, which is the only time it can be: this is the
	// channel the remote shell uses to explain itself, and without a writer
	// here x/crypto/ssh discards it. A refusal then has to report that the
	// command said nothing, while the command in fact said exactly what was
	// wrong -- "No such file or directory" for a mis-pathed helper being the
	// one an operator meets first.
	var stderr holdStderr
	session.Stderr = &stderr

	if err := session.Start(command); err != nil {
		session.Close()
		return nil, fmt.Errorf("start remote command: %w", err)
	}

	h := &heldCommand{session: session, stdin: stdin, desc: readyLine, said: &stderr, exited: make(chan error, 1)}
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
			return nil, fmt.Errorf("remote command produced no output: %w%s", r.err, holdStderrSuffix(stderr.String()))
		case r.line == readyLine:
			return h, nil
		case r.line == "":
			_ = h.Close()
			// The reason, when there is one, INSTEAD of saying there was
			// none. "it exited without saying why" is only true when stderr
			// is empty too, and it was being printed over the top of a
			// perfectly good explanation.
			if said := stderr.String(); said != "" {
				return nil, fmt.Errorf("%w: %s", ErrHoldRefused, said)
			}
			return nil, fmt.Errorf("%w: it exited without saying why, on stdout or on stderr", ErrHoldRefused)
		default:
			_ = h.Close()
			// stderr appended rather than substituted here: this branch
			// already has the command's own first line, and one of the
			// VMSYNC-LOCK-* markers arriving where the ready line was
			// expected is the whole diagnosis. Where it is something else,
			// stderr is usually what explains it.
			return nil, fmt.Errorf("%w: %s%s", ErrHoldRefused, r.line, holdStderrSuffix(stderr.String()))
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

// The handle reports its command's exit and what the command said, as well as
// being a WriteCloser. util.AcquireRemoteRunLock looks for the first to learn
// promptly that a lock holder let go, and for the second to say WHY -- without
// it the driver can only report "Process exited with status 1" about a holder
// that explained itself perfectly well on stderr. Asserted here so renaming
// either breaks the build rather than quietly costing the diagnosis: pkg/util
// finds them by shape, because it deliberately does not import this package.
var _ interface {
	io.WriteCloser
	Exited() <-chan error
	Said() string
} = (*heldCommand)(nil)
