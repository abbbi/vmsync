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
	"io"
	"net"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"

	"vmsync/pkg/remotessh"
	"vmsync/pkg/trace"
)

// ByteCounters tracks bytes actually sent/received over the SSH-tunneled
// (compressed) leg of a bridged connection, updated concurrently from the
// relay goroutines.
type ByteCounters struct {
	Sent     uint64
	Received uint64
}

func (b *ByteCounters) addSent(n int64) {
	if n > 0 {
		atomic.AddUint64(&b.Sent, uint64(n))
	}
}

func (b *ByteCounters) addReceived(n int64) {
	if n > 0 {
		atomic.AddUint64(&b.Received, uint64(n))
	}
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

// StartLocal opens a local TCP listener that transparently compresses
// traffic to/from remoteBridgeAddr (reached through sshClient's SSH tunnel)
// using zstd subprocesses run locally, symmetric with the remote bridge.
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
				if relayErr := relayConnection(ctx, conn, sshClient, remoteBridgeAddr, cfg, counters); relayErr != nil {
					trace.Debug("nbd bridge connection ended", "error", relayErr)
				}
			}()
		}
	}()

	return port, counters, stopFn, nil
}

// relayConnection bridges one accepted plaintext connection (from the local
// NBD client) to remoteBridgeAddr over the SSH tunnel, compressing the
// outbound direction and decompressing the inbound direction with local zstd
// subprocesses.
func relayConnection(ctx context.Context, conn net.Conn, sshClient *remotessh.Client, remoteBridgeAddr string, cfg Config, counters *ByteCounters) error {
	defer conn.Close()

	remote, err := sshClient.DialTCP(remoteBridgeAddr)
	if err != nil {
		return fmt.Errorf("dial remote nbd bridge %s over ssh: %w", remoteBridgeAddr, err)
	}
	defer remote.Close()

	compress := exec.CommandContext(ctx, "zstd", "-q", fmt.Sprintf("-%d", cfg.CompressLevel))
	compressIn, err := compress.StdinPipe()
	if err != nil {
		return fmt.Errorf("compress stdin pipe: %w", err)
	}
	compressOut, err := compress.StdoutPipe()
	if err != nil {
		return fmt.Errorf("compress stdout pipe: %w", err)
	}
	if err := compress.Start(); err != nil {
		return fmt.Errorf("start local zstd compressor: %w", err)
	}
	defer compress.Wait()

	decompress := exec.CommandContext(ctx, "zstd", "-dq")
	decompressIn, err := decompress.StdinPipe()
	if err != nil {
		return fmt.Errorf("decompress stdin pipe: %w", err)
	}
	decompressOut, err := decompress.StdoutPipe()
	if err != nil {
		return fmt.Errorf("decompress stdout pipe: %w", err)
	}
	if err := decompress.Start(); err != nil {
		return fmt.Errorf("start local zstd decompressor: %w", err)
	}
	defer decompress.Wait()

	var relayWg sync.WaitGroup
	relayWg.Add(4)
	var firstErr error
	var errOnce sync.Once
	reportErr := func(err error) {
		if err == nil || err == io.EOF {
			return
		}
		errOnce.Do(func() { firstErr = err })
	}

	go func() {
		defer relayWg.Done()
		_, err := io.Copy(compressIn, conn)
		compressIn.Close()
		reportErr(err)
	}()
	go func() {
		defer relayWg.Done()
		n, err := io.Copy(remote, compressOut)
		counters.addSent(n)
		reportErr(err)
	}()
	go func() {
		defer relayWg.Done()
		n, err := io.Copy(decompressIn, remote)
		counters.addReceived(n)
		decompressIn.Close()
		reportErr(err)
	}()
	go func() {
		defer relayWg.Done()
		_, err := io.Copy(conn, decompressOut)
		reportErr(err)
	}()

	relayWg.Wait()
	return firstErr
}
