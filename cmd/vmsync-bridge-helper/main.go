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
// spawned once per accepted connection by socat's "TCP-LISTEN:...,fork
// EXEC:vmsync-bridge-helper ..." (see pkg/nbdbridge/command.go), with the
// accepted connection wired to its stdin/stdout, and relays bytes between
// that connection and a real, plaintext NBD endpoint (-connect), optionally
// compressing and/or buffering the wire-facing side natively via
// pkg/zstdrelay -- replacing what used to be an external CLI shell pipe,
// which was proven unable to flush data through a long-lived, synchronous,
// small-message connection like NBD's.
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
	connectAddr := flag.String("connect", "", "real endpoint host:port to forward plaintext traffic to/from (required)")
	compress := flag.Bool("compress", false, "compress the wire-facing (stdin/stdout) traffic with zstd")
	level := flag.Int("level", 3, "zstd compression level (1-19), only used with -compress")
	netbuffer := flag.String("netbuffer", "", "buffer wire-facing traffic through a bounded in-memory buffer, "+
		"formatted as <blocksize>,<buffersize> (e.g. 64k,256M); empty disables it")
	flag.Parse()

	if *connectAddr == "" {
		fmt.Fprintln(os.Stderr, "vmsync-bridge-helper: -connect is required")
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

	if err := run(*connectAddr, *compress, *level, netbufferBlock, netbufferSize); err != nil {
		fmt.Fprintf(os.Stderr, "vmsync-bridge-helper: %v\n", err)
		os.Exit(1)
	}
}

func run(connectAddr string, compress bool, level int, netbufferBlock, netbufferSize string) error {
	conn, err := net.Dial("tcp", connectAddr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", connectAddr, err)
	}
	defer conn.Close()

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

	// stdin (wire, compressed/buffered client traffic) -> [buffer] -> [decompress] -> conn (plaintext, to the real NBD server)
	go func() {
		defer wg.Done()
		reportErr(zstdrelay.RelayFromWire(conn, os.Stdin, compress, netbufferBlock, netbufferSize, nil))
		if tc, ok := conn.(*net.TCPConn); ok {
			tc.CloseWrite() // half-close: tell the real server we're done sending
		}
	}()

	// conn (plaintext, from the real NBD server) -> [compress+flush] -> [buffer] -> stdout (wire, back to the client)
	go func() {
		defer wg.Done()
		reportErr(zstdrelay.Relay(os.Stdout, conn, compress, level, netbufferBlock, netbufferSize, nil))
	}()

	wg.Wait()
	return firstErr
}
