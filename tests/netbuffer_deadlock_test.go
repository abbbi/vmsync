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

// This file is a standalone reproduction of a real, unfixed bug found during a
// code review of the --netbuffer feature: Relay's drain goroutine and
// RelayFromWire's main copy loop don't close the shared BoundedBuffer when the
// *other* side of that direction fails while data is still flowing, so the
// side still writing into the buffer blocks forever once it fills. It exists
// to let this be checked empirically -- by actually running it and watching
// it hang -- rather than trusted on code-reading alone.
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

// TestRelayDeadlockOnDrainFailure reproduces the bug in Relay (the write
// direction): when netbuffer is enabled, a background goroutine drains the
// bounded buffer out to the real destination. If that destination write
// fails, the drain goroutine exits WITHOUT closing the buffer. The producer
// side (copying from src into the buffer, in Relay's own goroutine) has no
// way to know the drain side is gone, and blocks forever in BoundedBuffer's
// Write() the moment the now-undrained buffer fills -- so Relay() itself
// never reaches the point where it would otherwise close the buffer on the
// normal EOF path, and never returns.
//
// A tiny 4096-byte buffer against an infinite source guarantees it fills
// almost immediately, so if the bug is present this test times out in
// seconds, not by chance -- not because the buffer merely hasn't filled yet.
func TestRelayDeadlockOnDrainFailure(t *testing.T) {
	_, returned := runWithDeadline(t, 5*time.Second, func() error {
		return zstdrelay.Relay(alwaysFailWriter{}, infiniteReader{}, false, zstdrelay.AlgoZstd, "", "64k", "4096", nil)
	})
	if returned {
		t.Fatal("Relay returned promptly despite the destination failing -- the deadlock was NOT reproduced; either it's already fixed, or this repro needs adjusting to still match the current code")
	}
	t.Log("confirmed: Relay did not return within 5s -- the drain goroutine's write failure never closed the buffer, so the producer is stuck writing into a full, undrained buffer forever (this goroutine remains leaked for the rest of this test binary's run, which is expected)")
}

// TestRelayFromWireDeadlockOnConsumerFailure reproduces the mirror-image bug
// in RelayFromWire (the read direction): a background goroutine fills the
// bounded buffer from the wire. RelayFromWire's own main loop drains that
// buffer out to the real (local) destination. If THAT write fails,
// RelayFromWire's main io.Copy returns immediately -- but nothing tells the
// fill goroutine to stop, so it keeps reading from the (here, infinite) wire
// and writing into the now-unconsumed buffer until it fills and blocks.
// RelayFromWire then blocks forever itself, waiting on that fill goroutine's
// completion channel, which will now never fire.
func TestRelayFromWireDeadlockOnConsumerFailure(t *testing.T) {
	_, returned := runWithDeadline(t, 5*time.Second, func() error {
		return zstdrelay.RelayFromWire(alwaysFailWriter{}, infiniteReader{}, false, zstdrelay.AlgoZstd, "64k", "4096", nil)
	})
	if returned {
		t.Fatal("RelayFromWire returned promptly despite the destination failing -- the deadlock was NOT reproduced; either it's already fixed, or this repro needs adjusting to still match the current code")
	}
	t.Log("confirmed: RelayFromWire did not return within 5s -- the consumer's write failure never closed the buffer, so the fill goroutine (and RelayFromWire itself, waiting on it) are stuck forever (this goroutine remains leaked for the rest of this test binary's run, which is expected)")
}

var _ io.Writer = alwaysFailWriter{}
var _ io.Reader = infiniteReader{}
