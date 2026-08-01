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
					trace.Warning("nbd bridge connection ended with error", "remote", remoteBridgeAddr, "error", relayErr)
				}
			}()
		}
	}()

	return port, counters, stopFn, nil
}

// outboundStages returns the local filter chain for the accepted-conn ->
// SSH-channel direction: compress first (nearest the real, plaintext
// endpoint), then buffer (nearest the SSH channel) -- symmetric with the
// remote side's own stage ordering around its network hop.
func outboundStages(cfg Config) [][]string {
	var stages [][]string
	if cfg.Compress {
		stages = append(stages, []string{"zstd", "-q", fmt.Sprintf("-%d", cfg.CompressLevel)})
	}
	if cfg.MbufferEnabled() {
		stages = append(stages, []string{"mbuffer", "-q", "-s", cfg.MbufferBlock, "-m", cfg.MbufferSize})
	}
	return stages
}

// inboundStages returns the local filter chain for the SSH-channel ->
// accepted-conn direction: buffer first (nearest the SSH channel), then
// decompress (nearest the real, plaintext endpoint).
func inboundStages(cfg Config) [][]string {
	var stages [][]string
	if cfg.MbufferEnabled() {
		stages = append(stages, []string{"mbuffer", "-q", "-s", cfg.MbufferBlock, "-m", cfg.MbufferSize})
	}
	if cfg.Compress {
		stages = append(stages, []string{"zstd", "-dq"})
	}
	return stages
}

// startPipelineStages runs each stage in order, wiring stage i's stdout to
// stage i+1's stdin (Go's exec package copies a non-*os.File Stdin reader in
// a background goroutine, so no manual piping is needed between stages). It
// returns the final stage's stdout for the caller to drain, and the started
// commands for later cleanup via waitStages/killStages. If stages is empty,
// in is returned unchanged and no commands are started.
func startPipelineStages(ctx context.Context, in io.Reader, stages [][]string) (out io.Reader, cmds []*exec.Cmd, err error) {
	cur := in
	for _, args := range stages {
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		cmd.Stdin = cur
		stdout, perr := cmd.StdoutPipe()
		if perr != nil {
			killStages(cmds)
			return nil, nil, fmt.Errorf("stdout pipe for %s: %w", args[0], perr)
		}
		if serr := cmd.Start(); serr != nil {
			killStages(cmds)
			return nil, nil, fmt.Errorf("start %s: %w", args[0], serr)
		}
		cmds = append(cmds, cmd)
		cur = stdout
	}
	return cur, cmds, nil
}

// waitStages reaps started commands in reverse order. Each stage's stdout is
// only guaranteed fully drained once the next stage's Wait (which internally
// waits for the copy-into-stdin goroutine it started) has returned, so
// reaping must go from the last stage backwards.
func waitStages(cmds []*exec.Cmd) error {
	var firstErr error
	for i := len(cmds) - 1; i >= 0; i-- {
		if err := cmds[i].Wait(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// killStages forcibly terminates and reaps commands that were started but
// must be abandoned because a later stage in the same chain failed to start.
func killStages(cmds []*exec.Cmd) {
	for _, cmd := range cmds {
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
	}
	for _, cmd := range cmds {
		cmd.Wait()
	}
}

// relayConnection bridges one accepted plaintext connection (from the local
// NBD client) to remoteBridgeAddr over the SSH tunnel, running it through the
// enabled zstd/mbuffer filters -- compressing/buffering the outbound
// direction and reversing that for the inbound direction.
func relayConnection(ctx context.Context, conn net.Conn, sshClient *remotessh.Client, remoteBridgeAddr string, cfg Config, counters *ByteCounters) error {
	defer conn.Close()

	remote, err := sshClient.DialTCP(remoteBridgeAddr)
	if err != nil {
		return fmt.Errorf("dial remote nbd bridge %s over ssh: %w", remoteBridgeAddr, err)
	}
	defer remote.Close()

	outboundOut, outCmds, err := startPipelineStages(ctx, conn, outboundStages(cfg))
	if err != nil {
		return fmt.Errorf("start outbound filter chain: %w", err)
	}
	inboundOut, inCmds, err := startPipelineStages(ctx, remote, inboundStages(cfg))
	if err != nil {
		killStages(outCmds)
		return fmt.Errorf("start inbound filter chain: %w", err)
	}

	var relayWg sync.WaitGroup
	relayWg.Add(2)
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
		n, err := io.Copy(remote, outboundOut)
		counters.addSent(n)
		reportErr(err)
	}()
	go func() {
		defer relayWg.Done()
		n, err := io.Copy(conn, inboundOut)
		counters.addReceived(n)
		reportErr(err)
	}()
	relayWg.Wait()

	reportErr(waitStages(outCmds))
	reportErr(waitStages(inCmds))

	return firstErr
}
