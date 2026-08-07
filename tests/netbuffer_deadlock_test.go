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

// This file is a regression test for a bug found during a code review of the
// --netbuffer feature (now fixed in pkg/zstdrelay/relay.go): Relay's drain
// goroutine and RelayFromWire's main copy loop didn't close the shared
// BoundedBuffer when the *other* side of that direction failed while data
// was still flowing, so the side still writing into the buffer blocked
// forever once it filled. It exists to check this empirically -- by
// actually running it -- rather than trusting code-reading alone, both for
// the original bug and for the fix.
//
// This lives under tests/ rather than alongside pkg/zstdrelay as an
// in-package _test.go file because it only exercises zstdrelay's exported
// API (Relay, RelayFromWire, AlgoZstd) -- nothing here needs
// package-internal access.
package tests

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"vmsync/pkg/zstdrelay"
)

// alwaysFailWriter fails every write, simulating a destination connection
// that's already broken (a network blip on the real side of a bridged
// connection, mid-sync).
type alwaysFailWriter struct{}

func (alwaysFailWriter) Write(p []byte) (int, error) {
	return 0, errors.New("simulated write failure")
}

// infiniteReader never returns EOF -- it keeps producing data forever,
// simulating a long-lived connection (like NBD's own) that never closes
// mid-sync on its own. This is what keeps the producer side of each bug
// pushing data into the buffer well after the other side has already failed.
type infiniteReader struct{}

func (infiniteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// runWithDeadline runs fn in its own goroutine and reports whether it
// returned within timeout. A leaked, still-blocked goroutine from a
// reproduced deadlock is expected to live for the rest of this test
// binary's process -- there is no way to forcibly stop it, which is normal
// and expected for this class of test.
func runWithDeadline(t *testing.T, timeout time.Duration, fn func() error) (err error, returned bool) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err, true
	case <-time.After(timeout):
		return nil, false
	}
}

// TestRelayHappyPath is a sanity control: with netbuffer enabled and nothing
// failing, Relay must still complete promptly and deliver the data
// correctly. This is here so a hang in the two tests below can't be
// dismissed as "the harness itself always hangs" -- this one proves the
// harness and the buffering path work fine when nothing is broken.
func TestRelayHappyPath(t *testing.T) {
	src := bytes.NewReader([]byte("hello, netbuffer"))
	var dst bytes.Buffer

	err, returned := runWithDeadline(t, 5*time.Second, func() error {
		return zstdrelay.Relay(&dst, src, false, zstdrelay.AlgoZstd, "", "64k", "4096", nil)
	})
	if !returned {
		t.Fatal("Relay (happy path, nothing failing) did not return within 5s -- the test harness itself is broken, unrelated to the bug under test")
	}
	if err != nil {
		t.Fatalf("Relay (happy path) returned an unexpected error: %v", err)
	}
	if dst.String() != "hello, netbuffer" {
		t.Fatalf("Relay (happy path) delivered %q, want %q", dst.String(), "hello, netbuffer")
	}
}

// TestRelayReturnsOnDrainFailure guards against the fixed bug in Relay (the
// write direction): when netbuffer is enabled, a background goroutine drains
// the bounded buffer out to the real destination. Before the fix, if that
// destination write failed, the drain goroutine exited WITHOUT closing the
// buffer -- the producer side (copying from src into the buffer, in Relay's
// own goroutine) had no way to know the drain side was gone, and blocked
// forever in BoundedBuffer's Write() the moment the now-undrained buffer
// filled, so Relay() itself never returned. The fix closes the buffer from
// the drain goroutine as soon as it stops draining, for any reason.
//
// A tiny 4096-byte buffer against an infinite source guarantees it fills
// almost immediately, so a regression here shows up as a prompt timeout,
// not by chance -- not because the buffer merely hasn't filled yet.
func TestRelayReturnsOnDrainFailure(t *testing.T) {
	err, returned := runWithDeadline(t, 5*time.Second, func() error {
		return zstdrelay.Relay(alwaysFailWriter{}, infiniteReader{}, false, zstdrelay.AlgoZstd, "", "64k", "4096", nil)
	})
	if !returned {
		t.Fatal("Relay did not return within 5s -- the drain-failure deadlock has regressed: the drain goroutine's write failure isn't closing the buffer, so the producer is stuck writing into a full, undrained buffer forever")
	}
	if err == nil {
		t.Fatal("Relay returned a nil error despite the destination failing every write")
	}
	if !strings.Contains(err.Error(), "simulated write failure") {
		t.Fatalf("Relay returned %q, want it to surface the destination's real failure (\"simulated write failure\"), not a closed-pipe artifact left over from force-closing the buffer to unblock the producer", err)
	}
}

// TestRelayFromWireReturnsOnConsumerFailure guards against the fixed
// mirror-image bug in RelayFromWire (the read direction): a background
// goroutine fills the bounded buffer from the wire. RelayFromWire's own main
// loop drains that buffer out to the real (local) destination. Before the
// fix, if THAT write failed, RelayFromWire's main io.Copy returned
// immediately, but nothing told the fill goroutine to stop -- it kept
// reading from the (here, infinite) wire and writing into the now-unconsumed
// buffer until it filled and blocked, and RelayFromWire then blocked forever
// itself waiting on that fill goroutine's completion channel. The fix closes
// the buffer from the main loop as soon as it stops consuming, for any
// reason.
func TestRelayFromWireReturnsOnConsumerFailure(t *testing.T) {
	err, returned := runWithDeadline(t, 5*time.Second, func() error {
		return zstdrelay.RelayFromWire(alwaysFailWriter{}, infiniteReader{}, false, zstdrelay.AlgoZstd, "64k", "4096", nil)
	})
	if !returned {
		t.Fatal("RelayFromWire did not return within 5s -- the consumer-failure deadlock has regressed: the fill goroutine is left pumping the wire into a full, unconsumed buffer forever")
	}
	if err == nil {
		t.Fatal("RelayFromWire returned a nil error despite the destination failing every write")
	}
	if !strings.Contains(err.Error(), "simulated write failure") {
		t.Fatalf("RelayFromWire returned %q, want it to surface the destination's real failure (\"simulated write failure\")", err)
	}
}

var _ io.Writer = alwaysFailWriter{}
var _ io.Reader = infiniteReader{}
