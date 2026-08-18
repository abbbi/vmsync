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

package util

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// fakeHolder stands in for an SSH client, recording the script it was asked
// to run and replaying a canned outcome.
type fakeHolder struct {
	script string
	ready  string
	// failWith is the marker the remote script would have printed.
	failWith string
	closed   *bool
}

type fakeCloser struct{ closed *bool }

func (f fakeCloser) Close() error {
	if f.closed != nil {
		*f.closed = true
	}
	return nil
}

func (h *fakeHolder) HoldCommand(_ context.Context, command, readyLine string) (io.Closer, error) {
	h.script, h.ready = command, readyLine
	if h.failWith != "" {
		// Mirrors how remotessh reports a command that answered with
		// something other than the ready line.
		return nil, fmt.Errorf("remote command refused: %s", h.failWith)
	}
	return fakeCloser{closed: h.closed}, nil
}

func TestAcquireRemoteRunLockScript(t *testing.T) {
	h := &fakeHolder{}
	if _, err := AcquireRemoteRunLock(context.Background(), h, "/run/vmsync-locks", "target-web01"); err != nil {
		t.Fatalf("AcquireRemoteRunLock: %v", err)
	}

	// The lock must be held by the SHELL, not by the flock child: an
	// `exec 9>` in the shell outlives the flock invocation, and `cat` then
	// keeps that shell alive. `flock -n 9 -c ...` instead would release the
	// moment the child exited, making the whole thing a no-op that looks
	// like it works.
	for _, want := range []string{"exec 9>", "flock -n 9", "\ncat"} {
		if !strings.Contains(h.script, want) {
			t.Errorf("script does not contain %q:\n%s", want, h.script)
		}
	}
	// Non-blocking: a contended lock has to answer now, not queue behind a
	// multi-hour sync.
	if strings.Contains(h.script, "flock 9") {
		t.Error("script blocks on the lock instead of failing fast")
	}
	// The path has to match the one the LOCAL lock would use, or a sync and
	// a promotion of the same domain would lock different files and both
	// proceed.
	if !strings.Contains(h.script, RunLockPath("/run/vmsync-locks", "target-web01")) {
		t.Errorf("script does not lock %s:\n%s", RunLockPath("/run/vmsync-locks", "target-web01"), h.script)
	}
	if h.ready != remoteLockReady {
		t.Errorf("ready line = %q, want %q", h.ready, remoteLockReady)
	}
}

func TestAcquireRemoteRunLockQuotesItsPaths(t *testing.T) {
	// The key reaches this from a domain name, and a domain name is not
	// guaranteed to be shell-safe.
	h := &fakeHolder{}
	if _, err := AcquireRemoteRunLock(context.Background(), h, "/run/vmsync-locks", "target-web01; rm -rf /"); err != nil {
		t.Fatalf("AcquireRemoteRunLock: %v", err)
	}
	if strings.Contains(h.script, "; rm -rf /'") == false && strings.Contains(h.script, "rm -rf") {
		// Present but not inside quotes: that is the failure worth catching.
		idx := strings.Index(h.script, "rm -rf")
		before := h.script[:idx]
		if strings.Count(before, "'")%2 == 0 {
			t.Errorf("the key is interpolated unquoted:\n%s", h.script)
		}
	}
}

// TestAcquireRemoteRunLockContentionIsNotAnError distinguishes the one
// outcome callers must treat as a clean skip from every genuine failure.
// Confusing them either turns an ordinary overlap into a paging failure, or
// hides a broken host as routine contention.
func TestAcquireRemoteRunLockContentionIsNotAnError(t *testing.T) {
	for _, tc := range []struct {
		marker      string
		wantHeld    bool
		wantMention string
	}{
		{remoteLockBusy, true, "already working on"},
		{remoteLockNoFlock, false, "no flock"},
		{remoteLockNoDir, false, "lock directory"},
		{remoteLockNoCreate, false, "lock file"},
	} {
		t.Run(tc.marker, func(t *testing.T) {
			h := &fakeHolder{failWith: tc.marker}
			_, err := AcquireRemoteRunLock(context.Background(), h, "/run/vmsync-locks", "target-web01")
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := errors.Is(err, ErrLockHeld); got != tc.wantHeld {
				t.Errorf("errors.Is(err, ErrLockHeld) = %v, want %v (err = %v)", got, tc.wantHeld, err)
			}
			if !strings.Contains(err.Error(), tc.wantMention) {
				t.Errorf("error %q does not mention %q", err, tc.wantMention)
			}
		})
	}
}

func TestAcquireRemoteRunLockReturnsAWorkingCloser(t *testing.T) {
	closed := false
	h := &fakeHolder{closed: &closed}
	lock, err := AcquireRemoteRunLock(context.Background(), h, "/run/vmsync-locks", "target-web01")
	if err != nil {
		t.Fatalf("AcquireRemoteRunLock: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if !closed {
		t.Error("closing the lock did not end the remote command holding it")
	}
}
