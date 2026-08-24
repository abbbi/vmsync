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

// This file exercises vmsync-bridge-helper's two core pieces of logic:
//
//   - recoverRelayPanic, which turns a panic on one of handleConn's two
//     relay-direction goroutines into a returned error instead of letting it
//     crash the whole (now long-lived, multi-connection) helper process.
//
//   - handleConn itself, end to end, over real loopback TCP connections on
//     both sides -- deliberately not net.Pipe(), because handleConn only
//     takes its CloseWrite half-close path when the concrete connection type
//     is *net.TCPConn (see the type assertions in main.go), and net.Pipe()
//     connections are not *net.TCPConn. Using real listeners on both the
//     "client" side (conn) and the "real NBD export" side (real) exercises
//     that path for real, the same way it runs in production.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"vmsync/pkg/zstdrelay"
)

// runWithDeadline runs fn in its own goroutine and reports whether it
// returned within timeout. Mirrors the helper of the same name and purpose
// in tests/netbuffer_deadlock_test.go: a goroutine left blocked past the
// deadline (e.g. a hung handleConn) is simply abandoned for the rest of this
// test binary's process -- there is no way to forcibly stop it, which is
// normal and expected for this class of test.
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

// newLoopbackPair returns two ends of a genuine loopback TCP connection:
// accepted is the server-accepted end, playing the role of the local bridge
// relay's already-accepted outbound connection (handleConn's own conn
// parameter); dialed is the client end the test drives directly. Both are
// real *net.TCPConn values under the net.Conn interface, which matters here
// -- see the package doc comment above.
func newLoopbackPair(t *testing.T) (accepted, dialed net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	acceptedCh := make(chan net.Conn, 1)
	acceptErrCh := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			acceptErrCh <- err
			return
		}
		acceptedCh <- c
	}()

	dialed, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	var acceptErr error
	_, returned := runWithDeadline(t, 5*time.Second, func() error {
		select {
		case accepted = <-acceptedCh:
		case acceptErr = <-acceptErrCh:
		}
		return nil
	})
	if !returned {
		t.Fatal("timed out waiting to accept the loopback connection")
	}
	if acceptErr != nil {
		t.Fatalf("accept: %v", acceptErr)
	}
	return accepted, dialed
}

// startFakeRealExport starts a stand-in for the real, plaintext NBD endpoint
// handleConn dials via -connect. It accepts exactly one connection, reads
// exactly expectBytes from it, records what it received, writes back a fixed,
// known response distinct from the request, and half-closes so handleConn's
// outbound relay direction can in turn see EOF and finish.
//
// It reads a KNOWN LENGTH via io.ReadFull rather than everything-until-EOF
// via io.ReadAll, which is what it used to do. io.ReadAll cannot tell "the
// peer sent the whole request and then half-closed" apart from "the peer sent
// nothing at all and closed" -- both are a nil error and a slice, differing
// only in length. That made a genuine truncation surface far away from where
// it happened, as a puzzling byte-comparison mismatch in the caller, and it
// made the content assertion depend on FIN timing rather than on the bytes
// themselves.
//
// The distinction matters most for -compress=s2: s2's reader treats EOF at a
// chunk boundary as a clean end of stream rather than corruption (see
// readFull's own allowEOF handling in klauspost/compress/s2/reader.go), so a
// stream that ends early decodes to zero bytes with NO error anywhere in the
// pipeline -- RelayFromWire returns nil, handleConn half-closes, and the only
// evidence left is an empty read here. zstd reports the same event as an
// error instead, which is why this only ever showed up on the s2 case.
// io.ReadFull turns it back into an explicit "unexpected EOF" naming the
// moment it occurred.
//
// After the request, it confirms EOF actually follows: that half-close is a
// real behavior of handleConn's inbound direction worth asserting, it just
// shouldn't be load-bearing for the content check above.
//
// It returns the listener's address to hand to handleConn as -connect, plus
// two receive-only channels reporting what was received and any failure, so
// the caller can verify both directions of the relay independently.
// The three return channels report three different things, and keeping them
// separate is what makes this usable from a select.
//
// errCh carries ONLY a real failure -- it never receives a nil to mean
// success. An earlier version signalled completion by sending nil on the same
// channel, which made the caller's
//
//	select {
//	case got = <-receivedCh:
//	case err := <-exportErrCh:
//	}
//
// a coin flip whenever this goroutine ran to completion before the caller
// reached the select: both channels were ready, Go picks a ready case at
// random, and picking the nil error left got nil and failed the test with
// "received 0 bytes ... the relay corrupted the payload in transit" -- an
// accusation against the relay for something the test did to itself. It
// reproduced roughly half the time under -race -count 4, and every time with
// a 50ms sleep before the select.
//
// doneCh closes when the goroutine has finished, whatever the outcome, so
// waiting for it never consumes the error a later check wants to read.
func startFakeRealExport(t *testing.T, expectBytes int, response []byte) (addr string, receivedCh <-chan []byte, errCh <-chan error, doneCh <-chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen (fake real export): %v", err)
	}

	rc := make(chan []byte, 1)
	ec := make(chan error, 1)
	dc := make(chan struct{})
	go func() {
		defer close(dc)
		defer ln.Close()
		conn, err := ln.Accept()
		if err != nil {
			ec <- fmt.Errorf("accept: %w", err)
			return
		}
		defer conn.Close()

		data := make([]byte, expectBytes)
		if _, err := io.ReadFull(conn, data); err != nil {
			ec <- fmt.Errorf("read the %d-byte request the relay should have delivered: %w", expectBytes, err)
			return
		}
		// Nothing more may follow: handleConn's inbound direction
		// half-closes once it is done, so this must be a clean EOF and not
		// extra bytes.
		if n, err := conn.Read(make([]byte, 1)); n != 0 || err != io.EOF {
			ec <- fmt.Errorf("expected EOF after the %d-byte request (handleConn's inbound CloseWrite), got n=%d err=%v", expectBytes, n, err)
			return
		}
		rc <- data

		if _, err := conn.Write(response); err != nil {
			ec <- fmt.Errorf("write response: %w", err)
			return
		}
		if tcpConn, ok := conn.(*net.TCPConn); ok {
			tcpConn.CloseWrite()
		}
		// Deliberately no `ec <- nil` here: success is signalled by dc
		// closing, so that errCh staying empty is unambiguous.
	}()

	return ln.Addr().String(), rc, ec, dc
}

// TestRecoverRelayPanic checks that a panicking fn is converted into a
// non-nil error mentioning both the label and the panic value, for a few
// different panic value shapes -- rather than escaping and crashing the test
// process, which is the entire point of recoverRelayPanic existing (see its
// doc comment in main.go). The mere fact that these subtests run to
// completion and report a pass/fail, instead of the whole test binary
// aborting, is itself part of what's being checked.
func TestRecoverRelayPanic(t *testing.T) {
	cases := []struct {
		name       string
		panicValue interface{}
	}{
		{"panics with a string", "boom"},
		{"panics with an error value", errors.New("boom error")},
		{"panics with a non-string, non-error value", 42},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := recoverRelayPanic("test-label", func() error {
				panic(tc.panicValue)
			})
			if err == nil {
				t.Fatal("recoverRelayPanic returned a nil error after fn panicked")
			}
			if !strings.Contains(err.Error(), "test-label") {
				t.Fatalf("recoverRelayPanic's error %q does not mention the label %q", err.Error(), "test-label")
			}
			wantSubstr := fmt.Sprint(tc.panicValue)
			if !strings.Contains(err.Error(), wantSubstr) {
				t.Fatalf("recoverRelayPanic's error %q does not mention the panic value %q", err.Error(), wantSubstr)
			}
		})
	}
}

// TestRecoverRelayPanicNoPanic is a sanity control: when fn doesn't panic,
// recoverRelayPanic must be a plain passthrough -- it actually calls fn, and
// returns exactly what fn returned (nil here), not swallowing or replacing
// it.
func TestRecoverRelayPanicNoPanic(t *testing.T) {
	called := false
	err := recoverRelayPanic("test-label", func() error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("recoverRelayPanic returned an unexpected error for a non-panicking fn: %v", err)
	}
	if !called {
		t.Fatal("recoverRelayPanic did not call fn")
	}
}

// TestRecoverRelayPanicPropagatesOrdinaryError checks the other half of the
// passthrough contract: an ordinary (non-panic) error returned by fn must
// come back unchanged, not be re-wrapped or replaced the way an actual panic
// value is.
func TestRecoverRelayPanicPropagatesOrdinaryError(t *testing.T) {
	wantErr := errors.New("ordinary failure")
	err := recoverRelayPanic("test-label", func() error {
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("recoverRelayPanic returned %v, want the ordinary error %v to propagate unchanged", err, wantErr)
	}
}

// TestHandleConnRelaysBothDirections drives handleConn end to end over real
// loopback TCP on both sides: a fake "client" pair (conn, the accepted end
// handleConn is given, and dialed, the end this test drives directly) and a
// fake "real NBD export" listener (what -connect points at). It checks, for
// each compression mode, that data written into the client end arrives at
// the fake real export unchanged, that the fake real export's response
// arrives back at the client end unchanged, and that handleConn itself
// returns promptly once both directions have drained -- rather than hanging,
// which is exactly the class of bug the CloseWrite half-closes in handleConn
// exist to prevent (see main.go's comments on why they matter now that this
// is a persistent, multi-connection daemon rather than a one-shot,
// exec'd-per-connection process).
func TestHandleConnRelaysBothDirections(t *testing.T) {
	const timeout = 5 * time.Second

	cases := []struct {
		name     string
		compress bool
		algo     zstdrelay.Algo
		level    string
	}{
		{"uncompressed", false, zstdrelay.AlgoZstd, "3"},
		{"zstd compressed", true, zstdrelay.AlgoZstd, "3"},
		{"s2 compressed", true, zstdrelay.AlgoS2, "default"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := bytes.Repeat([]byte("request-payload-"), 400)
			response := bytes.Repeat([]byte("response-payload-"), 400)

			realAddr, receivedCh, exportErrCh, exportDoneCh := startFakeRealExport(t, len(request), response)

			accepted, dialed := newLoopbackPair(t)
			defer dialed.Close()

			handleDone := make(chan struct{})
			go func() {
				handleConn(accepted, helperConfig{
					ConnectAddr: realAddr,
					Compress:    tc.compress,
					Algo:        tc.algo,
					Level:       tc.level,
				})
				close(handleDone)
			}()

			// Drive the wire side: send the request (compressing it first if
			// this case has compression on), then half-close so the inbound
			// relay direction's reader sees EOF. This half-close is what
			// stands in for "the real local relay is done sending" -- and
			// for the uncompressed case specifically, it's the ONLY way
			// RelayFromWire's plain io.Copy can ever learn the request is
			// complete, since there's no compressed-frame end marker to
			// detect it from instead.
			sendErrCh := make(chan error, 1)
			go func() {
				var err error
				if tc.compress {
					err = zstdrelay.Relay(dialed, bytes.NewReader(request), true, tc.algo, tc.level, "", "", nil)
				} else {
					_, err = dialed.Write(request)
				}
				if err == nil {
					if tcpConn, ok := dialed.(*net.TCPConn); ok {
						err = tcpConn.CloseWrite()
					}
				}
				sendErrCh <- err
			}()

			err, returned := runWithDeadline(t, timeout, func() error { return <-sendErrCh })
			if !returned {
				t.Fatal("timed out sending the request into the relay")
			}
			if err != nil {
				t.Fatalf("sending request into the relay: %v", err)
			}

			var got []byte
			err, returned = runWithDeadline(t, timeout, func() error {
				select {
				case got = <-receivedCh:
					return nil
				case err := <-exportErrCh:
					return err
				}
			})
			if !returned {
				t.Fatal("timed out waiting for the fake real export to receive the relayed request -- neither the full request nor its trailing EOF ever arrived")
			}
			if err != nil {
				t.Fatalf("fake real export failed before responding: %v", err)
			}
			// Length is already guaranteed by the export's own io.ReadFull
			// (a short delivery fails above, naming itself); what's left to
			// check here is that the bytes came through unchanged.
			if !bytes.Equal(got, request) {
				t.Fatalf("fake real export received %d bytes that do not match the request -- the relay corrupted the payload in transit", len(got))
			}

			// Read back whatever the relay forwarded from the fake real
			// export, decompressing it ourselves if this case has
			// compression on.
			var respBuf bytes.Buffer
			recvErrCh := make(chan error, 1)
			go func() {
				var err error
				if tc.compress {
					err = zstdrelay.RelayFromWire(&respBuf, dialed, true, tc.algo, "", "", nil)
				} else {
					_, err = io.Copy(&respBuf, dialed)
				}
				recvErrCh <- err
			}()

			err, returned = runWithDeadline(t, timeout, func() error { return <-recvErrCh })
			if !returned {
				t.Fatal("timed out reading the response back out of the relay")
			}
			if err != nil {
				t.Fatalf("receiving the response back out of the relay: %v", err)
			}
			if !bytes.Equal(respBuf.Bytes(), response) {
				t.Fatalf("relay delivered a response of %d bytes, want the %d-byte response delivered unchanged", respBuf.Len(), len(response))
			}

			// Waiting on doneCh rather than on errCh: errCh may legitimately
			// be empty, and reading it to find out whether the goroutine
			// finished would block forever exactly when nothing went wrong.
			_, returned = runWithDeadline(t, timeout, func() error {
				<-exportDoneCh
				return nil
			})
			if !returned {
				t.Fatal("timed out waiting for the fake real export goroutine to finish")
			}
			select {
			case err := <-exportErrCh:
				t.Fatalf("fake real export goroutine finished with an error: %v", err)
			default:
			}

			// handleConn spawns two goroutines (inbound and outbound relay
			// directions) and only returns once sync.WaitGroup says both are
			// done. Both directions have now fully drained above, so this
			// must return promptly -- if it doesn't, the CloseWrite
			// propagation between the two directions has regressed.
			_, returned = runWithDeadline(t, timeout, func() error {
				<-handleDone
				return nil
			})
			if !returned {
				t.Fatal("handleConn did not return after both relay directions should have drained -- CloseWrite propagation regressed")
			}
		})
	}
}

// TestHandleConnReturnsOnRealSideInterruption simulates a WAN link drop on
// the real, plaintext side while a sync is mid-flight: it abruptly closes
// (not gracefully half-closes) the fake real export's accepted connection
// while the client side is actively still writing, and checks that
// handleConn still returns within a bounded deadline instead of hanging
// forever waiting on a peer that is never coming back.
//
// This deliberately uses the uncompressed path: the point of this test is
// the failure/return-promptly behavior, not compression correctness (already
// covered by TestHandleConnRelaysBothDirections), so there's no reason to
// pull compressed-stream framing into the mix here too.
func TestHandleConnReturnsOnRealSideInterruption(t *testing.T) {
	const timeout = 5 * time.Second

	realLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen (fake real export): %v", err)
	}
	defer realLn.Close()

	realAcceptedCh := make(chan net.Conn, 1)
	go func() {
		c, err := realLn.Accept()
		if err == nil {
			realAcceptedCh <- c
		}
	}()

	accepted, dialed := newLoopbackPair(t)
	defer dialed.Close()

	handleDone := make(chan struct{})
	go func() {
		handleConn(accepted, helperConfig{
			ConnectAddr: realLn.Addr().String(),
			Compress:    false,
			Algo:        zstdrelay.AlgoZstd,
			Level:       "3",
		})
		close(handleDone)
	}()

	var realConn net.Conn
	_, returned := runWithDeadline(t, timeout, func() error {
		realConn = <-realAcceptedCh
		return nil
	})
	if !returned {
		t.Fatal("timed out waiting for handleConn to dial the fake real export")
	}

	// Keep pumping data from the client side in the background so there is
	// continuous traffic in flight when the real side gets yanked out from
	// under the relay, and so there's independent proof the interruption
	// actually reached back to the client side, rather than the round trip
	// quietly completing as if nothing had happened.
	writeErrCh := make(chan error, 1)
	go func() {
		chunk := bytes.Repeat([]byte("W"), 32*1024)
		for {
			if _, err := dialed.Write(chunk); err != nil {
				writeErrCh <- err
				return
			}
		}
	}()

	// Make sure some of that data has actually reached the real side before
	// yanking it away, so this is a genuine mid-stream interruption rather
	// than a failure at startup. Bounded like everything else here: if the
	// relay ever regressed to not forwarding data at all, this Read must
	// time out rather than hang the whole test run forever.
	buf := make([]byte, 4096)
	readErr, returned := runWithDeadline(t, timeout, func() error {
		_, err := realConn.Read(buf)
		return err
	})
	if !returned {
		t.Fatal("timed out waiting for the fake real export to see any data before killing the connection")
	}
	if readErr != nil {
		t.Fatalf("fake real export: reading initial data: %v", readErr)
	}

	// Abruptly close it -- a full Close, not a graceful CloseWrite --
	// simulating a WAN link drop on the real, plaintext side mid-sync.
	realConn.Close()

	_, returned = runWithDeadline(t, timeout, func() error { return <-writeErrCh })
	if !returned {
		t.Fatal("client-side writes never failed after the real endpoint was killed -- the interruption never propagated back through the relay")
	}

	_, returned = runWithDeadline(t, timeout, func() error {
		<-handleDone
		return nil
	})
	if !returned {
		t.Fatal("handleConn did not return after the real endpoint was killed abruptly -- it hung instead of surfacing the failure")
	}
}
