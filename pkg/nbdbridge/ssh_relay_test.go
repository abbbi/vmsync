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
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"vmsync/pkg/remotessh"
)

// This file covers the cfg.UseSSH == true leg of relayConnection, which
// every other test in this package deliberately avoids (they pass
// UseSSH: false and a nil *remotessh.Client, so `remote` is always a plain
// loopback *net.TCPConn). That leg is what production actually uses with
// vmsync's own -use-ssh, and until now nothing exercised it end to end:
// remotessh.Client.DialTCP had no coverage at all, and neither did the
// half-close in local.go that only exists on this path.
//
// No sshd is involved. golang.org/x/crypto/ssh -- already a dependency, via
// pkg/remotessh -- can run a full server in-process, and remotessh.Dial is
// pure Go against it (ConfigFromLibvirtURI is the function that shells out
// to `ssh -G`, and nothing here goes near it). remotessh.Config's own
// InsecureIgnoreHostKey and Password fields mean no known_hosts fixture and
// no private key file are needed either, so this builds a genuine
// *remotessh.Client with zero production-code changes or test hooks.

const (
	testSSHUser     = "vmsync-test"
	testSSHPassword = "vmsync-test-password"
)

// directTCPIPPayload is the wire format of a "direct-tcpip" channel-open
// request (RFC 4254 section 7.2) -- what golang.org/x/crypto/ssh's
// Client.Dial, and therefore remotessh.Client.DialTCP, sends to ask the
// server to open a forwarded connection on its behalf.
type directTCPIPPayload struct {
	HostToConnect     string
	PortToConnect     uint32
	OriginatorAddress string
	OriginatorPort    uint32
}

// startFakeSSHServer runs an in-process SSH server on loopback that serves
// exactly one thing: "direct-tcpip" channel opens, which it satisfies by
// dialing the requested address for real and relaying bytes both ways. That
// is precisely the sshd behavior remotessh.Client.DialTCP depends on, and
// all this package needs from a "remote host".
//
// The host key is generated per-call as ed25519 rather than RSA: it's
// effectively instant, where an RSA keygen would add a noticeable delay to
// every test that calls this. Authentication is password-only, so the
// client side needs no key file on disk.
//
// Returns the port to point remotessh.Dial at. All goroutines and listeners
// are torn down via t.Cleanup.
func startFakeSSHServer(t *testing.T) int {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 host key: %v", err)
	}
	hostKey, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("build host key signer: %v", err)
	}

	srvCfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if c.User() != testSSHUser || string(pass) != testSSHPassword {
				return nil, fmt.Errorf("bad credentials")
			}
			return nil, nil
		},
	}
	srvCfg.AddHostKey(hostKey)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for fake ssh server: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			tcpConn, err := ln.Accept()
			if err != nil {
				return // listener closed by cleanup
			}
			go serveFakeSSHConn(tcpConn, srvCfg)
		}
	}()

	return ln.Addr().(*net.TCPAddr).Port
}

// serveFakeSSHConn completes the SSH handshake on one accepted connection
// and services its channel-open requests. Errors are deliberately swallowed
// rather than routed to t: this runs on a background goroutine that can
// still be mid-handshake when a test finishes and t.Cleanup closes the
// listener, and calling t.Error/t.Fatal after a test completes panics the
// test binary. Anything that actually matters shows up as the test's own
// round trip failing or timing out instead.
func serveFakeSSHConn(tcpConn net.Conn, srvCfg *ssh.ServerConfig) {
	defer tcpConn.Close()

	sshConn, chans, reqs, err := ssh.NewServerConn(tcpConn, srvCfg)
	if err != nil {
		return
	}
	defer sshConn.Close()
	go ssh.DiscardRequests(reqs)

	for newChan := range chans {
		if newChan.ChannelType() != "direct-tcpip" {
			newChan.Reject(ssh.UnknownChannelType, "only direct-tcpip is supported")
			continue
		}
		var payload directTCPIPPayload
		if err := ssh.Unmarshal(newChan.ExtraData(), &payload); err != nil {
			newChan.Reject(ssh.ConnectionFailed, "malformed direct-tcpip payload")
			continue
		}
		target := net.JoinHostPort(payload.HostToConnect, fmt.Sprintf("%d", payload.PortToConnect))
		upstream, err := net.Dial("tcp", target)
		if err != nil {
			newChan.Reject(ssh.ConnectionFailed, "dial "+target+" failed")
			continue
		}
		ch, chReqs, err := newChan.Accept()
		if err != nil {
			upstream.Close()
			continue
		}
		go ssh.DiscardRequests(chReqs)
		go relayChannelToUpstream(ch, upstream)
	}
}

// relayChannelToUpstream copies bytes both ways between an accepted
// direct-tcpip channel and the real connection it was opened for, and --
// critically for this package -- propagates each side's EOF onward as a
// half-close rather than tearing the whole pair down.
//
// A real sshd does exactly this, and local.go's outbound relay depends on
// it: it calls CloseWrite() on the SSH channel once the local NBD client is
// done sending, expecting that to surface as EOF at the far end. Closing
// both directions here instead would mask a regression in that half-close
// by making the connection collapse anyway.
func relayChannelToUpstream(ch ssh.Channel, upstream net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		io.Copy(upstream, ch)
		// Channel hit EOF (the client half-closed): tell upstream, but
		// leave the other direction running.
		if tc, ok := upstream.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		io.Copy(ch, upstream)
		ch.CloseWrite()
	}()

	wg.Wait()
	ch.Close()
	upstream.Close()
}

// dialFakeSSH builds a real *remotessh.Client against the in-process server,
// through remotessh.Dial itself rather than by reaching into the package --
// so the handshake, auth-method selection and host-key handling under test
// are the production ones.
func dialFakeSSH(t *testing.T, port int) *remotessh.Client {
	t.Helper()

	// A real ssh-agent on the developer's or CI machine would otherwise be
	// offered as an additional auth method ahead of the password (see
	// buildAuthMethods): harmless, since the server accepts only passwords
	// and x/crypto/ssh falls through to the next method, but it makes the
	// handshake depend on ambient environment state. Neutralize it so this
	// test behaves identically everywhere.
	t.Setenv("SSH_AUTH_SOCK", "")

	client, err := remotessh.Dial(remotessh.Config{
		Address:               "127.0.0.1",
		Port:                  port,
		User:                  testSSHUser,
		Password:              testSSHPassword,
		InsecureIgnoreHostKey: true,
		Timeout:               10 * time.Second,
	})
	if err != nil {
		t.Fatalf("remotessh.Dial against fake ssh server on port %d: %v", port, err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

// TestStartLocalRelaysBytesRoundTripOverSSH is
// TestStartLocalRelaysBytesRoundTrip's UseSSH counterpart: same config
// matrix, same round trip, but `remote` inside relayConnection is a real SSH
// direct-tcpip channel obtained through remotessh.Client.DialTCP instead of
// a plain net.Dial.
//
// That difference is not cosmetic. x/crypto/ssh's forwarded connection is a
// *chanConn wrapping an ssh.Channel -- a different concrete type from
// *net.TCPConn, with a different method set (notably no io.WriterTo or
// io.ReaderFrom, so io.Copy takes neither of its fast paths, and a
// SetDeadline that deliberately returns an error). None of that was covered
// by any test before this one.
func TestStartLocalRelaysBytesRoundTripOverSSH(t *testing.T) {
	sshPort := startFakeSSHServer(t)

	for _, tt := range bridgeConfigCombos("1M") {
		t.Run(tt.name, func(t *testing.T) {
			ln, accepted := newFakeRemoteListener(t)
			defer ln.Close()
			go func() {
				for conn := range accepted {
					go echoLoop(conn)
				}
			}()

			cfg := tt.cfg
			cfg.UseSSH = true
			client := dialFakeSSH(t, sshPort)

			localPort, counters, stop, err := StartLocal(context.Background(), client, ln.Addr().String(), cfg)
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
				t.Fatal("round trip through the ssh-tunneled bridge did not complete within 15s")
			}
			if relayErr != nil {
				t.Fatal(relayErr)
			}

			// The byte counters are wired identically on both legs, but
			// until now only the plain-TCP one was ever asserted. An SSH
			// channel conn reaching CountingWriter through a different
			// io.Copy path is exactly the sort of difference that could
			// silently zero these out.
			// Bounded wait rather than an immediate read, for the reason
			// waitForCounter's own comment sets out: the counter moves
			// after the write it counts has already returned, so the
			// payload arriving back proves nothing about it yet.
			if !waitForCounter(counters.SentSnapshot) {
				t.Error("ByteCounters.Sent stayed 0 despite relaying real traffic over ssh")
			}
			if !waitForCounter(counters.ReceivedSnapshot) {
				t.Error("ByteCounters.Received stayed 0 despite relaying real traffic over ssh")
			}

			if tt.cfg.Compress {
				if got := counters.SentSnapshot(); got >= uint64(len(payload)) {
					t.Errorf("ByteCounters.Sent = %d, want less than the uncompressed payload size %d -- compression does not appear to have reduced anything over ssh", got, len(payload))
				}
				if got := counters.ReceivedSnapshot(); got >= uint64(len(payload)) {
					t.Errorf("ByteCounters.Received = %d, want less than the uncompressed payload size %d -- compression does not appear to have reduced anything over ssh", got, len(payload))
				}
			}
		})
	}
}

// TestStartLocalOverSSHHalfClosePropagatesEOFToRemote pins the half-close in
// local.go's outbound relay: after the local NBD client stops sending, the
// SSH channel's write side must be closed so the far end sees EOF.
//
// This is the one behavior that exists ONLY on the SSH leg, and its failure
// mode is the worst kind -- not a wrong value but a permanent hang. local.go's
// own comment records it happening in production and being diagnosed from a
// SIGQUIT goroutine dump: a direct-tcpip channel is long-lived and does not
// close when a process exits the way a pipe does, so without CloseWrite the
// remote helper's stdin never reaches EOF, its relay goroutine blocks
// forever, and the connection hangs even after the real NBD client is done.
//
// The fake remote here does not echo. It reads to EOF and reports how many
// bytes it saw, so the test can only pass if EOF genuinely arrived: a
// regression that drops the CloseWrite leaves io.ReadAll blocked and the
// deadline below expires.
func TestStartLocalOverSSHHalfClosePropagatesEOFToRemote(t *testing.T) {
	sshPort := startFakeSSHServer(t)

	ln, accepted := newFakeRemoteListener(t)
	defer ln.Close()

	drained := make(chan int, 1)
	go func() {
		for conn := range accepted {
			go func(c net.Conn) {
				defer c.Close()
				got, _ := io.ReadAll(c) // returns only once EOF actually arrives
				drained <- len(got)
			}(conn)
		}
	}()

	client := dialFakeSSH(t, sshPort)
	localPort, _, stop, err := StartLocal(context.Background(), client, ln.Addr().String(), Config{UseSSH: true})
	if err != nil {
		t.Fatalf("StartLocal: %v", err)
	}
	defer stop()

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", localPort))
	if err != nil {
		t.Fatalf("dial local bridge port %d: %v", localPort, err)
	}
	defer conn.Close()

	payload := []byte(strings.Repeat("half close probe payload ", 100))
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	// Half-close the local client side. That EOF has to travel: local conn
	// -> relayConnection's outbound Relay returning -> CloseWrite on the SSH
	// channel -> the fake sshd's own CloseWrite on its upstream dial -> the
	// fake remote's io.ReadAll returning.
	if tc, ok := conn.(*net.TCPConn); ok {
		if err := tc.CloseWrite(); err != nil {
			t.Fatalf("half-close local client connection: %v", err)
		}
	} else {
		t.Fatal("expected the local bridge connection to be a *net.TCPConn")
	}

	select {
	case n := <-drained:
		if n != len(payload) {
			t.Errorf("fake remote saw %d bytes before EOF, want %d", n, len(payload))
		}
	case <-time.After(15 * time.Second):
		t.Fatal("fake remote never saw EOF after the local client half-closed -- the outbound relay's CloseWrite on the ssh channel did not propagate, which is the hang local.go's own comment documents")
	}
}

// Compile-time guard: the half-close in local.go's outbound relay is a
// behavioral type assertion (`remote.(interface{ CloseWrite() error })`), so
// nothing would fail to build if x/crypto/ssh's forwarded-connection type
// stopped offering CloseWrite -- the assertion would just silently stop
// matching and the hang above would come back. ssh.Channel is what
// *chanConn embeds to provide it, so pinning the method here turns that
// silent regression into a build failure at dependency-upgrade time.
var _ interface{ CloseWrite() error } = (ssh.Channel)(nil)
