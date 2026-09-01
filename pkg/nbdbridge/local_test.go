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

package nbdbridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// runWithDeadline runs fn in its own goroutine and reports whether it
// returned within timeout. Adapted from tests/netbuffer_deadlock_test.go's
// helper of the same name: a leaked, still-blocked goroutine from a
// reproduced deadlock/hang is expected to live for the rest of this test
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

// newFakeRemoteListener starts a plain TCP listener standing in for the
// remote vmsync-bridge-helper's own listening socket. It only accepts
// connections and hands each one to the returned channel -- callers decide
// what to do with each accepted connection (echo it, read from it and then
// drop it, etc), which the tests below need individual control over.
func newFakeRemoteListener(t *testing.T) (net.Listener, <-chan net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for fake remote: %v", err)
	}
	accepted := make(chan net.Conn, 8)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				close(accepted)
				return
			}
			accepted <- conn
		}
	}()
	return ln, accepted
}

// echoLoop reads whatever bytes arrive on conn and writes them straight
// back, looping until conn errors out or is closed. Used as the fake
// remote's behavior in the round-trip tests below.
//
// This is correct as a stand-in even when compression is enabled:
// relayConnection's outbound direction compresses local-client bytes before
// writing them to remote, so whatever arrives here already IS the
// compressed representation of what the local NBD client wrote. Echoing
// those exact bytes back verbatim hands the local relay's inbound direction
// (which decompresses remote -> local-client with the very same algo/level)
// back the identical compressed stream it just produced, which decodes to
// exactly the original plaintext. So content integrity holds regardless of
// cfg.Compress -- there's no need for this fake to know or care about
// compression at all.
func echoLoop(conn net.Conn) {
	defer conn.Close()
	buf := make([]byte, 32*1024)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			if _, werr := conn.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func TestRecoverRelayPanicRecoversAndReportsLabelAndPanicValue(t *testing.T) {
	err := recoverRelayPanic("outbound relay (conn -> remote)", func() error {
		panic("simulated relay panic")
	})
	if err == nil {
		t.Fatal("recoverRelayPanic returned a nil error for a panicking fn")
	}
	if !strings.Contains(err.Error(), "outbound relay (conn -> remote)") {
		t.Errorf("error %q does not contain the label", err.Error())
	}
	if !strings.Contains(err.Error(), "simulated relay panic") {
		t.Errorf("error %q does not contain the panic value", err.Error())
	}
	// Reaching this line at all is the other half of the assertion: a
	// panic escaping recoverRelayPanic would have crashed this test
	// process instead of merely failing the test.
}

func TestRecoverRelayPanicRecoversNonStringPanicValue(t *testing.T) {
	err := recoverRelayPanic("inbound relay (remote -> conn)", func() error {
		panic(errors.New("boom"))
	})
	if err == nil {
		t.Fatal("recoverRelayPanic returned a nil error for a panicking fn")
	}
	if !strings.Contains(err.Error(), "inbound relay (remote -> conn)") {
		t.Errorf("error %q does not contain the label", err.Error())
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error %q does not contain the panic value", err.Error())
	}
}

func TestRecoverRelayPanicPassesThroughNormalReturns(t *testing.T) {
	wantErr := errors.New("a normal, non-panic error")
	if err := recoverRelayPanic("label", func() error { return wantErr }); !errors.Is(err, wantErr) {
		t.Errorf("recoverRelayPanic(fn returning an error) = %v, want %v", err, wantErr)
	}
	if err := recoverRelayPanic("label", func() error { return nil }); err != nil {
		t.Errorf("recoverRelayPanic(fn returning nil) = %v, want nil", err)
	}
}

// bridgeConfigCombos returns the config matrix every StartLocal-driven test
// in this file drives its scenario across: no compress/no netbuffer, each
// supported compress algo alone, netbuffer alone, and both together --
// relayConnection wires cfg.Compress and cfg.NetBufferEnabled into the exact
// same Relay/RelayFromWire calls for both directions of every connection, so
// a test using only the plain "neither enabled" config never exercises the
// compression or buffering stages at all. netBufferSize is left to the
// caller: a happy-path test can use a comfortably large capacity, but a test
// that needs the buffer to reliably fill within a bounded time (see the two
// dropped-remote tests below) needs a deliberately small one instead.
func bridgeConfigCombos(netBufferSize string) []struct {
	name string
	cfg  Config
} {
	return []struct {
		name string
		cfg  Config
	}{
		{"no compress, no netbuffer", Config{}},
		{"compress zstd", Config{Compress: true, CompressAlgo: "zstd", CompressLevel: "3"}},
		{"compress s2 better", Config{Compress: true, CompressAlgo: "s2", CompressLevel: "better"}},
		{"netbuffer only", Config{NetBufferBlock: "64k", NetBufferSize: netBufferSize}},
		{"compress and netbuffer together", Config{
			Compress: true, CompressAlgo: "zstd", CompressLevel: "3",
			NetBufferBlock: "64k", NetBufferSize: netBufferSize,
		}},
	}
}

// TestStartLocalRelaysBytesRoundTrip dials the local bridge port StartLocal
// opens, as if it were the local NBD client, and checks that bytes written
// there make it all the way to a fake "remote" and back unchanged -- across
// every combination of compress/netbuffer settings StartLocal supports. cfg
// always has UseSSH: false, with a nil *remotessh.Client passed to
// StartLocal: relayConnection only ever dereferences sshClient when
// cfg.UseSSH is true, so this is a safe, SSH-free way to exercise the real
// TCP relay end to end over loopback.
// waitForCounter waits, briefly, for a byte counter to become non-zero.
//
// The payload arriving back at the test does NOT imply the counter has been
// updated yet, and assuming it does is what made this test flaky on CI.
// CountingWriter increments AFTER the underlying write returns:
//
//	n, err := c.W.Write(p)   // bytes are in the socket buffer from here
//	atomic.AddUint64(...)    // ...but the counter only moves here
//
// Everything in between -- the fake remote echoing, the inbound relay
// decoding and writing back, this test's ReadFull returning -- can complete
// inside that gap, all on loopback, if the outbound goroutine happens to be
// descheduled right after the write syscall. A loaded CI runner with few
// cores makes that ordinary rather than exotic.
//
// It shows up only in the uncompressed, unbuffered combination because that
// is the one where the whole payload moves in a single counted Write, so
// there is exactly one increment and it is the one being raced. Compression
// and buffering both produce several writes, and an earlier one has already
// made the counter non-zero.
//
// Counting after the write is the correct production semantic -- counting
// before would report bytes that failed to send -- so the fix belongs here:
// wait for the value instead of assuming a happens-before edge that does
// not exist. Bounded so a genuinely broken counter still fails the test
// rather than hanging it.
func waitForCounter(snapshot func() uint64) bool {
	deadline := time.Now().Add(2 * time.Second)
	for {
		if snapshot() > 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
}

func TestStartLocalRelaysBytesRoundTrip(t *testing.T) {
	for _, tt := range bridgeConfigCombos("1M") {
		t.Run(tt.name, func(t *testing.T) {
			ln, accepted := newFakeRemoteListener(t)
			defer ln.Close()
			go func() {
				for conn := range accepted {
					go echoLoop(conn)
				}
			}()

			localPort, counters, stop, err := StartLocal(context.Background(), nil, ln.Addr().String(), tt.cfg)
			if err != nil {
				t.Fatalf("StartLocal: %v", err)
			}
			defer stop()

			conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", localPort))
			if err != nil {
				t.Fatalf("dial local bridge port %d: %v", localPort, err)
			}
			defer conn.Close()

			payload := []byte(strings.Repeat("nbd bridge round trip payload data ", 200))

			relayErr, returned := runWithDeadline(t, 15*time.Second, func() error {
				if _, werr := conn.Write(payload); werr != nil {
					return fmt.Errorf("write payload: %w", werr)
				}
				got := make([]byte, len(payload))
				if _, rerr := io.ReadFull(conn, got); rerr != nil {
					return fmt.Errorf("read back payload: %w", rerr)
				}
				if string(got) != string(payload) {
					return fmt.Errorf("round-tripped payload does not match: got %d bytes, want %d bytes", len(got), len(payload))
				}
				return nil
			})
			if !returned {
				t.Fatal("round trip through the bridge did not complete within 15s")
			}
			if relayErr != nil {
				t.Fatal(relayErr)
			}

			if !waitForCounter(counters.SentSnapshot) {
				t.Error("ByteCounters.Sent stayed 0 despite relaying real traffic")
			}
			if !waitForCounter(counters.ReceivedSnapshot) {
				t.Error("ByteCounters.Received stayed 0 despite relaying real traffic")
			}

			// A non-zero counter alone can't tell a working compressor
			// apart from one that silently passes bytes through unchanged
			// (e.g. a broken algo/level wiring that falls back to a no-op
			// writer) -- both would still report non-zero counts. payload
			// is a 200x repeated string, about as compressible as real
			// data gets, so an actual encoder must bring the wire byte
			// count well below its plaintext size; echoLoop reflects the
			// outbound direction's own compressed bytes straight back (see
			// its own doc comment), so the inbound direction's count is
			// held to the same bar.
			if tt.cfg.Compress {
				if got := counters.SentSnapshot(); got >= uint64(len(payload)) {
					t.Errorf("ByteCounters.Sent = %d, want less than the uncompressed payload size %d -- compression does not appear to have reduced anything", got, len(payload))
				}
				if got := counters.ReceivedSnapshot(); got >= uint64(len(payload)) {
					t.Errorf("ByteCounters.Received = %d, want less than the uncompressed payload size %d -- compression does not appear to have reduced anything", got, len(payload))
				}
			}
		})
	}
}

// TestStartLocalStopClosesTheLocalListener checks that the stop function
// StartLocal returns actually tears the bridge down: it must return
// promptly, and afterward the local port it opened must no longer accept
// connections.
func TestStartLocalStopClosesTheLocalListener(t *testing.T) {
	ln, _ := newFakeRemoteListener(t)
	defer ln.Close()

	localPort, _, stop, err := StartLocal(context.Background(), nil, ln.Addr().String(), Config{})
	if err != nil {
		t.Fatalf("StartLocal: %v", err)
	}

	stopErr, returned := runWithDeadline(t, 5*time.Second, stop)
	if !returned {
		t.Fatal("stop() did not return within 5s")
	}
	if stopErr != nil {
		t.Fatalf("stop() returned an unexpected error: %v", stopErr)
	}

	if conn, dialErr := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", localPort)); dialErr == nil {
		conn.Close()
		t.Fatalf("dial to the local bridge port %d succeeded after stop(), want it refused", localPort)
	}
}

// TestStartLocalDroppedRemoteDoesNotWedgeBridge simulates a WAN interruption:
// after a relayed connection is already flowing, the fake remote's side of
// it vanishes abruptly (no clean shutdown of its own -- just gone, the way a
// severed network link looks from this end). The bridge must notice and
// unwind that one connection within a bounded deadline rather than hanging
// forever with the local NBD client's side of it wedged open.
//
// The relay's outbound goroutine (conn -> remote) is blocked reading from
// the local, client-facing conn until there's something new to relay, so it
// can only discover (and act on) the dead remote leg once fed more data --
// mirroring how a real NBD client would only notice a stuck connection the
// next time it tries to use it. The test loop below keeps feeding the local
// side until an operation on it fails, which is what "the relay noticed and
// tore the connection down" looks like from a client's vantage point.
//
// Driven across every compress/netbuffer combo (see bridgeConfigCombos), not
// just the plain pass-through config: two real deadlocks
// (tests/netbuffer_deadlock_test.go's TestRelayReturnsOnDrainFailure and
// TestRelayFromWireReturnsOnConsumerFailure) shipped specifically because a
// netbuffer drain/fill goroutine failed to close its BoundedBuffer when the
// OTHER side of the same relay direction errored -- a bug the plain
// "neither enabled" config can never exercise, since that path is a bare
// io.Copy with no buffer to leak, and the streamrelay-level regression tests
// for those two bugs never go through this package's own relayConnection
// wiring at all. netBufferSize is deliberately small (4096, matching those
// same streamrelay-level tests' own reasoning) so the buffer reliably fills
// within this test's own write loop instead of a regression only showing up
// once the buffer happens to fill by chance.
func TestStartLocalDroppedRemoteDoesNotWedgeBridge(t *testing.T) {
	for _, tt := range bridgeConfigCombos("4096") {
		t.Run(tt.name, func(t *testing.T) {
			ln, accepted := newFakeRemoteListener(t)
			defer ln.Close()

			localPort, _, stop, err := StartLocal(context.Background(), nil, ln.Addr().String(), tt.cfg)
			if err != nil {
				t.Fatalf("StartLocal: %v", err)
			}
			defer stop()

			conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", localPort))
			if err != nil {
				t.Fatalf("dial local bridge port %d: %v", localPort, err)
			}
			defer conn.Close()

			if _, err := conn.Write([]byte("hello before the drop")); err != nil {
				t.Fatalf("initial write: %v", err)
			}

			var remoteConn net.Conn
			select {
			case remoteConn = <-accepted:
			case <-time.After(5 * time.Second):
				t.Fatal("fake remote never saw a connection from the relay")
			}

			buf := make([]byte, 64)
			remoteConn.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, err := remoteConn.Read(buf); err != nil {
				t.Fatalf("fake remote did not see the relayed bytes before the drop: %v", err)
			}

			// Simulate an abrupt WAN interruption: the remote side vanishes with no
			// clean shutdown of its own.
			remoteConn.Close()

			checkErr, returned := runWithDeadline(t, 8*time.Second, func() error {
				deadline := time.Now().Add(3 * time.Second)
				for time.Now().Before(deadline) {
					if _, werr := conn.Write([]byte("keep going after the drop\n")); werr != nil {
						return nil // the write itself failing already proves the bridge noticed
					}
					readBuf := make([]byte, 64)
					conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
					if _, rerr := conn.Read(readBuf); rerr != nil {
						if ne, ok := rerr.(net.Error); ok && ne.Timeout() {
							continue // no data yet -- keep pushing until the relay reacts
						}
						return nil // any non-timeout read error also proves it noticed
					}
				}
				return errors.New("the local bridge connection never errored out after its remote leg was dropped")
			})
			if !returned {
				t.Fatal("checking for the dropped-remote error did not complete within its own deadline")
			}
			if checkErr != nil {
				t.Fatal(checkErr)
			}
		})
	}
}

// TestStartLocalDroppedRemoteUnblocksIdleClient reproduces the more
// realistic failure mode TestStartLocalDroppedRemoteDoesNotWedgeBridge above
// does not cover: a local NBD client that goes idle -- blocked waiting on a
// reply to a command it already sent, not actively sending anything new --
// at the moment the remote leg dies. Before relayConnection's inbound
// goroutine (remote -> conn) half-closed conn once its own source (remote)
// errored, nothing ever told a client in this state that the transport was
// gone, so its blocking read would never return. Unlike the test above, this
// one never writes to conn again after the drop -- only reads -- so it can
// only pass if the bridge itself notices and propagates the failure, not the
// client prompting it by sending more data.
//
// Driven across every compress/netbuffer combo for the same reason as the
// test above: with netbuffer enabled, the inbound direction has its own
// fill goroutine (reading remote's now-dead wire) and a separate main loop
// draining into conn, and the half-close-on-failure behavior this test
// pins down must still fire promptly with that extra buffering stage in the
// middle, not just on the bare pass-through path the plain config exercises.
func TestStartLocalDroppedRemoteUnblocksIdleClient(t *testing.T) {
	for _, tt := range bridgeConfigCombos("4096") {
		t.Run(tt.name, func(t *testing.T) {
			ln, accepted := newFakeRemoteListener(t)
			defer ln.Close()

			localPort, _, stop, err := StartLocal(context.Background(), nil, ln.Addr().String(), tt.cfg)
			if err != nil {
				t.Fatalf("StartLocal: %v", err)
			}
			defer stop()

			conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", localPort))
			if err != nil {
				t.Fatalf("dial local bridge port %d: %v", localPort, err)
			}
			defer conn.Close()

			if _, err := conn.Write([]byte("request before the drop")); err != nil {
				t.Fatalf("initial write: %v", err)
			}

			var remoteConn net.Conn
			select {
			case remoteConn = <-accepted:
			case <-time.After(5 * time.Second):
				t.Fatal("fake remote never saw a connection from the relay")
			}

			buf := make([]byte, 64)
			remoteConn.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, err := remoteConn.Read(buf); err != nil {
				t.Fatalf("fake remote did not see the relayed bytes before the drop: %v", err)
			}

			// Simulate an abrupt WAN interruption, exactly like the test above --
			// but the local side never writes again afterward: it just sits there
			// reading, the way a real NBD client would while waiting on a reply to
			// a command it already sent.
			remoteConn.Close()

			readErr, returned := runWithDeadline(t, 8*time.Second, func() error {
				readBuf := make([]byte, 64)
				_, rerr := conn.Read(readBuf)
				return rerr
			})
			if !returned {
				t.Fatal("an idle local client's blocked read was never unblocked after its remote leg was dropped -- the inbound relay goroutine must half-close conn once its own source (remote) errors")
			}
			if readErr == nil {
				t.Fatal("expected the idle client's read to fail once the remote leg was dropped, got a nil error")
			}
		})
	}
}
