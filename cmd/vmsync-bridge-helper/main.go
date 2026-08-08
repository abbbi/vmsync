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

// vmsync-bridge-helper is vmsync's remote-side NBD bridge process. It is
// started once (see pkg/nbdbridge/command.go), listens on -listen, and for
// each accepted connection dials the real, plaintext NBD endpoint (-connect)
// and relays bytes between the two, optionally compressing and/or buffering
// the wire-facing side natively via pkg/zstdrelay -- replacing what used to
// be an external CLI shell pipe, which was proven unable to flush data
// through a long-lived, synchronous, small-message connection like NBD's.
//
// This binary must be deployed to any target host --compress/--netbuffer
// will run against by the user themselves (e.g. via scp or configuration
// management) -- vmsync does not upload it. See -bridge-helper-path in
// `vmsync`'s own flags.
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"

	"vmsync/pkg/version"
	"vmsync/pkg/zstdrelay"
)

// optionalValueFlag implements flag.Value (plus the IsBoolFlag optimization)
// for a string flag that also works bare -- "-name" alone resolves to
// bareDefault, "-name=x" takes x literally, and "-name=false" (or simply
// omitting the flag) disables it. Duplicated from cmd/vmsync/main.go rather
// than shared -- for the same reason recoverRelayPanic below has its own
// copy instead of importing pkg/nbdbridge's: cmd/vmsync is package main (not
// importable at all), and pkg/nbdbridge would pull in its pkg/remotessh (SSH
// client) dependency, which this otherwise minimal, dependency-light binary
// deliberately avoids.
type optionalValueFlag struct {
	value       string
	bareDefault string
}

func (f *optionalValueFlag) String() string   { return f.value }
func (f *optionalValueFlag) IsBoolFlag() bool { return true }
func (f *optionalValueFlag) Set(s string) error {
	switch s {
	case "true":
		f.value = f.bareDefault
	case "false":
		f.value = ""
	default:
		f.value = s
	}
	return nil
}

// validateCompressLevel checks level is valid for algo. Duplicated from
// pkg/nbdbridge.ValidateCompressLevel rather than imported, for the same
// dependency-avoidance reason optionalValueFlag above is duplicated.
func validateCompressLevel(algo zstdrelay.Algo, level string) error {
	if algo == zstdrelay.AlgoS2 {
		switch level {
		case "default", "better", "best":
			return nil
		default:
			return fmt.Errorf("-compress-level must be \"default\", \"better\", or \"best\" when -compress=s2, got %q", level)
		}
	}
	n, err := strconv.Atoi(level)
	if err != nil {
		return fmt.Errorf("-compress-level must be a number between 1 and 19 for -compress=zstd, got %q", level)
	}
	if n < 1 || n > 19 {
		return fmt.Errorf("-compress-level must be between 1 and 19, got %d", n)
	}
	return nil
}

func main() {
	listenAddr := flag.String("listen", "", "local host:port to listen on for bridged connections (required)")
	connectAddr := flag.String("connect", "", "real endpoint host:port to dial and forward plaintext traffic to/from, once per accepted connection (required)")
	compressArg := optionalValueFlag{bareDefault: "s2"}
	netBufferArg := optionalValueFlag{bareDefault: "128k,1G"}
	flag.Var(&compressArg, "compress", "Same syntax as vmsync")
	level := flag.String("compress-level", "3", "Same syntax as vmsync")
	flag.Var(&netBufferArg, "netbuffer", "Same syntax as vmsync")
	showVersion := flag.Bool("v", false, "Show version and exit")
	showVersionLong := flag.Bool("version", false, "Show version and exit")
	flag.Parse()

	if *showVersion || *showVersionLong {
		fmt.Println(version.Version)
		os.Exit(0)
	}

	if *listenAddr == "" {
		fmt.Fprintln(os.Stderr, "vmsync-bridge-helper: -listen is required")
		os.Exit(2)
	}
	if *connectAddr == "" {
		fmt.Fprintln(os.Stderr, "vmsync-bridge-helper: -connect is required")
		os.Exit(2)
	}

	compressLevelExplicit := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "compress-level" {
			compressLevelExplicit = true
		}
	})

	compress := compressArg.value != ""
	var algo zstdrelay.Algo
	if compress {
		var err error
		algo, err = zstdrelay.ParseAlgo(compressArg.value)
		if err != nil {
			fmt.Fprintf(os.Stderr, "vmsync-bridge-helper: %v\n", err)
			os.Exit(2)
		}
		if !compressLevelExplicit && algo == zstdrelay.AlgoS2 {
			*level = "better"
		}
		if err := validateCompressLevel(algo, *level); err != nil {
			fmt.Fprintf(os.Stderr, "vmsync-bridge-helper: %v\n", err)
			os.Exit(2)
		}
	}

	var netbufferBlock, netbufferSize string
	if netBufferArg.value != "" {
		parts := strings.SplitN(netBufferArg.value, ",", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			fmt.Fprintf(os.Stderr, "vmsync-bridge-helper: -netbuffer must be of the form <blocksize>,<buffersize>, got %q\n", netBufferArg.value)
			os.Exit(2)
		}
		netbufferBlock, netbufferSize = parts[0], parts[1]
	}

	if err := serve(*listenAddr, *connectAddr, compress, algo, *level, netbufferBlock, netbufferSize); err != nil {
		fmt.Fprintf(os.Stderr, "vmsync-bridge-helper: %v\n", err)
		os.Exit(1)
	}
}

// serve listens on listenAddr and hands each accepted connection to
// handleConn on its own goroutine, indefinitely -- the same "listen, fork
// per connection" role socat's "TCP-LISTEN:...,fork" used to play, now done
// natively so the remote host no longer needs socat installed at all.
func serve(listenAddr, connectAddr string, compress bool, algo zstdrelay.Algo, level string, netbufferBlock, netbufferSize string) error {
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", listenAddr, err)
	}
	defer ln.Close()

	for {
		conn, err := ln.Accept()
		if err != nil {
			return fmt.Errorf("accept on %s: %w", listenAddr, err)
		}
		go handleConn(conn, connectAddr, compress, algo, level, netbufferBlock, netbufferSize)
	}
}

// recoverRelayPanic runs fn, converting any panic into a returned error
// instead of letting it escape the calling goroutine. handleConn's own
// recover() (below) only protects its own goroutine stack; it spawns two
// more goroutines to do the actual bidirectional relay, and a panic on
// either of those stacks is not caught by that outer recover -- an
// unrecovered panic there would still crash this whole process and every
// other connection it's currently serving, exactly the failure handleConn's
// own recover was meant to prevent in the first place. label identifies
// which direction panicked in the logged message, for diagnosability.
func recoverRelayPanic(label string, fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "vmsync-bridge-helper: recovered from panic in %s: %v\n", label, r)
			err = fmt.Errorf("panic in %s: %v", label, r)
		}
	}()
	return fn()
}

// handleConn serves exactly one accepted connection: dial the real NBD
// endpoint and relay bidirectionally until either side is done. It never
// lets a panic escape -- unlike the old one-process-per-connection model
// (each connection was a separate socat-exec'd process, crash-isolated by
// the OS), all connections now share this one long-lived process, so an
// unrecovered panic here would take down every other connection this helper
// is currently serving, not just this one.
func handleConn(conn net.Conn, connectAddr string, compress bool, algo zstdrelay.Algo, level string, netbufferBlock, netbufferSize string) {
	defer conn.Close()
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "vmsync-bridge-helper: connection handler panic: %v\n", r)
		}
	}()

	real, err := net.Dial("tcp", connectAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vmsync-bridge-helper: dial %s: %v\n", connectAddr, err)
		return
	}
	defer real.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	var firstErr error
	var errOnce sync.Once
	reportErr := func(e error) {
		if e == nil {
			return
		}
		errOnce.Do(func() { firstErr = e })
	}

	// conn (wire, compressed/buffered client traffic) -> [buffer] -> [decompress] -> real (plaintext, to the real NBD server)
	go func() {
		defer wg.Done()
		reportErr(recoverRelayPanic("inbound relay (conn -> real)", func() error {
			err := zstdrelay.RelayFromWire(real, conn, compress, algo, netbufferBlock, netbufferSize, nil)
			if tc, ok := real.(*net.TCPConn); ok {
				tc.CloseWrite() // half-close: tell the real server we're done sending
			}
			return err
		}))
	}()

	// real (plaintext, from the real NBD server) -> [compress+flush] -> [buffer] -> conn (wire, back to the client)
	//
	// The explicit CloseWrite here matters now in a way it didn't when this
	// was a one-shot, exec'd-per-connection process: process exit used to
	// implicitly close stdout, signaling "done" to the peer for free. This
	// helper is now a persistent daemon serving many connections, so nothing
	// else will ever half-close conn's write side -- without this, the local
	// relay on the other end of the SSH channel would block forever waiting
	// for an EOF that can now never arrive (the same class of hang
	// diagnosed via a SIGQUIT goroutine dump on the opposite direction).
	go func() {
		defer wg.Done()
		reportErr(recoverRelayPanic("outbound relay (real -> conn)", func() error {
			err := zstdrelay.Relay(conn, real, compress, algo, level, netbufferBlock, netbufferSize, nil)
			if tc, ok := conn.(*net.TCPConn); ok {
				tc.CloseWrite()
			}
			return err
		}))
	}()

	wg.Wait()
	if firstErr != nil {
		fmt.Fprintf(os.Stderr, "vmsync-bridge-helper: connection %s: %v\n", conn.RemoteAddr(), firstErr)
	}
}
