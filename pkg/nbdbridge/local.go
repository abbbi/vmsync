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
	"fmt"
	"net"
	"strconv"
	"sync"
	"sync/atomic"

	"vmsync/pkg/remotessh"
	"vmsync/pkg/trace"
	"vmsync/pkg/zstdrelay"
)

// ByteCounters tracks bytes actually sent/received over the SSH-tunneled
// (compressed) leg of a bridged connection, updated concurrently from the
// relay goroutines via pkg/zstdrelay's CountingWriter/CountingReader, which
// operate directly on these exported fields.
type ByteCounters struct {
	Sent     uint64
	Received uint64
}

// SentSnapshot atomically reads the total bytes sent over the wire so far.
func (b *ByteCounters) SentSnapshot() uint64 {
	return atomic.LoadUint64(&b.Sent)
}

// ReceivedSnapshot atomically reads the total bytes received over the wire
// so far.
func (b *ByteCounters) ReceivedSnapshot() uint64 {
	return atomic.LoadUint64(&b.Received)
}

// StartLocal opens a local TCP listener that transparently compresses/buffers
// traffic to/from remoteBridgeAddr (reached through sshClient's SSH tunnel),
// symmetric with the remote vmsync-bridge-helper process on the other end.
// Callers should redirect their real NBD dial target from the real host:port
// to 127.0.0.1:<returned port> for the bridged leg.
func StartLocal(ctx context.Context, sshClient *remotessh.Client, remoteBridgeAddr string, cfg Config) (localPort int, counters *ByteCounters, stop func() error, err error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, nil, nil, fmt.Errorf("listen for local nbd bridge: %w", err)
	}
	_, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		ln.Close()
		return 0, nil, nil, fmt.Errorf("parse local nbd bridge listen address: %w", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		ln.Close()
		return 0, nil, nil, fmt.Errorf("parse local nbd bridge listen port: %w", err)
	}

	counters = &ByteCounters{}
	var wg sync.WaitGroup
	var closeOnce sync.Once
	stopFn := func() error {
		var stopErr error
		closeOnce.Do(func() {
			stopErr = ln.Close()
			wg.Wait()
		})
		return stopErr
	}

	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				if relayErr := relayConnection(conn, sshClient, remoteBridgeAddr, cfg, counters); relayErr != nil {
					trace.Warning("nbd bridge connection ended with error", "remote", remoteBridgeAddr, "error", relayErr)
				}
			}()
		}
	}()

	return port, counters, stopFn, nil
}

// relayConnection bridges one accepted plaintext connection (from the local
// NBD client) to remoteBridgeAddr over the SSH tunnel, compressing/buffering
// the outbound direction and reversing that for the inbound direction via
// pkg/zstdrelay -- the same logic cmd/vmsync-bridge-helper uses on the
// remote end, so both sides of the bridge can never drift apart in
// behavior. There's no subprocess left to bind to a context for
// cancellation (unlike the old CLI-piped version); closing conn/remote from
// the caller's own teardown is what unblocks a relay in progress.
func relayConnection(conn net.Conn, sshClient *remotessh.Client, remoteBridgeAddr string, cfg Config, counters *ByteCounters) error {
	defer conn.Close()

	remote, err := sshClient.DialTCP(remoteBridgeAddr)
	if err != nil {
		return fmt.Errorf("dial remote nbd bridge %s over ssh: %w", remoteBridgeAddr, err)
	}
	defer remote.Close()

	var relayWg sync.WaitGroup
	relayWg.Add(2)
	var firstErr error
	var errOnce sync.Once
	reportErr := func(err error) {
		if err == nil {
			return
		}
		errOnce.Do(func() { firstErr = err })
	}

	// conn (plaintext, from the local NBD client) -> [compress+flush] -> [buffer] -> remote (wire, over SSH)
	go func() {
		defer relayWg.Done()
		err := zstdrelay.Relay(remote, conn, cfg.Compress, cfg.CompressLevel, cfg.NetBufferBlock, cfg.NetBufferSize, &counters.Sent)
		// Half-close the SSH channel once we're done sending, mirroring what
		// cmd/vmsync-bridge-helper does on its own outbound direction
		// (tc.CloseWrite() after its stdin hits EOF). Without this, nothing
		// ever signals "no more data" on remote: it's a long-lived SSH
		// direct-tcpip channel, not a pipe that closes on process exit, so
		// the remote helper's stdin never sees EOF, its matching relay
		// goroutine blocks forever, its process never exits, and the whole
		// bridge connection hangs even after the real NBD client has
		// finished and closed -- observed directly via a SIGQUIT goroutine
		// dump: the decoder on this same connection's inbound direction was
		// blocked waiting on data that could now never arrive.
		if wc, ok := remote.(interface{ CloseWrite() error }); ok {
			wc.CloseWrite()
		}
		reportErr(err)
	}()
	// remote (wire, over SSH) -> [buffer] -> [decompress] -> conn (plaintext, to the local NBD client)
	go func() {
		defer relayWg.Done()
		reportErr(zstdrelay.RelayFromWire(conn, remote, cfg.Compress, cfg.NetBufferBlock, cfg.NetBufferSize, &counters.Received))
	}()
	relayWg.Wait()

	return firstErr
}
