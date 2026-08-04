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
	"strings"
	"sync"

	"vmsync/pkg/zstdrelay"
)

func main() {
	listenAddr := flag.String("listen", "", "local host:port to listen on for bridged connections (required)")
	connectAddr := flag.String("connect", "", "real endpoint host:port to dial and forward plaintext traffic to/from, once per accepted connection (required)")
	compress := flag.Bool("compress", false, "compress the wire-facing traffic")
	algoFlag := flag.String("algo", "zstd", "compression format to use with -compress: \"zstd\" or \"s2\"")
	level := flag.Int("level", 3, "zstd compression level (1-19), only used with -compress and -algo=zstd")
	netbuffer := flag.String("netbuffer", "", "buffer wire-facing traffic through a bounded in-memory buffer, "+
		"formatted as <blocksize>,<buffersize> (e.g. 64k,256M); empty disables it")
	flag.Parse()

	if *listenAddr == "" {
		fmt.Fprintln(os.Stderr, "vmsync-bridge-helper: -listen is required")
		os.Exit(2)
	}
	if *connectAddr == "" {
		fmt.Fprintln(os.Stderr, "vmsync-bridge-helper: -connect is required")
		os.Exit(2)
	}
	algo, err := zstdrelay.ParseAlgo(*algoFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vmsync-bridge-helper: %v\n", err)
		os.Exit(2)
	}

	var netbufferBlock, netbufferSize string
	if *netbuffer != "" {
		parts := strings.SplitN(*netbuffer, ",", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			fmt.Fprintf(os.Stderr, "vmsync-bridge-helper: -netbuffer must be of the form <blocksize>,<buffersize>, got %q\n", *netbuffer)
			os.Exit(2)
		}
		netbufferBlock, netbufferSize = parts[0], parts[1]
	}

	if err := serve(*listenAddr, *connectAddr, *compress, algo, *level, netbufferBlock, netbufferSize); err != nil {
		fmt.Fprintf(os.Stderr, "vmsync-bridge-helper: %v\n", err)
		os.Exit(1)
	}
}

// serve listens on listenAddr and hands each accepted connection to
// handleConn on its own goroutine, indefinitely -- the same "listen, fork
// per connection" role socat's "TCP-LISTEN:...,fork" used to play, now done
// natively so the remote host no longer needs socat installed at all.
func serve(listenAddr, connectAddr string, compress bool, algo zstdrelay.Algo, level int, netbufferBlock, netbufferSize string) error {
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

// handleConn serves exactly one accepted connection: dial the real NBD
// endpoint and relay bidirectionally until either side is done. It never
// lets a panic escape -- unlike the old one-process-per-connection model
// (each connection was a separate socat-exec'd process, crash-isolated by
// the OS), all connections now share this one long-lived process, so an
// unrecovered panic here would take down every other connection this helper
// is currently serving, not just this one.
func handleConn(conn net.Conn, connectAddr string, compress bool, algo zstdrelay.Algo, level int, netbufferBlock, netbufferSize string) {
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
		reportErr(zstdrelay.RelayFromWire(real, conn, compress, algo, netbufferBlock, netbufferSize, nil))
		if tc, ok := real.(*net.TCPConn); ok {
			tc.CloseWrite() // half-close: tell the real server we're done sending
		}
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
		reportErr(zstdrelay.Relay(conn, real, compress, algo, level, netbufferBlock, netbufferSize, nil))
		if tc, ok := conn.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()

	wg.Wait()
	if firstErr != nil {
		fmt.Fprintf(os.Stderr, "vmsync-bridge-helper: connection %s: %v\n", conn.RemoteAddr(), firstErr)
	}
}
