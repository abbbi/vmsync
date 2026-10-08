/*
	Copyright (C) 2026  Orsiris de Jong <ozy@netpower.fr>

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

// vmsync-bridge-helper is vmsync's remote-side agent on the target host. It
// has two entirely separate modes.
//
// The bridge relay (the default) is a long-lived process, started once (see
// pkg/nbdbridge/command.go), listening on -listen; for each accepted
// connection it dials the real, plaintext NBD endpoint (-connect) and relays
// bytes between the two, optionally compressing and/or buffering the
// wire-facing side natively via pkg/streamrelay rather than through an
// external CLI shell pipe, which cannot flush data through a long-lived,
// synchronous, small-message connection like NBD's.
//
// The digest pass (-checksum) is a one-shot command, run over SSH and gone:
// it reads a digest request on stdin, hashes the ranges it names off a local
// qemu-nbd export, and writes a digest response on stdout. It never listens
// and shares no state with the relay. This is the target half of vmsync's
// pre-commit integrity check -- computing the digests HERE, on the host that
// already holds the bytes, is what keeps the check affordable: a few bytes
// per megabyte cross the network instead of the megabyte. See checksum.go.
//
// This binary must be deployed to any target host by the user themselves
// (e.g. via scp or configuration management) -- vmsync does not upload it.
// See -bridge-helper-path in `vmsync`'s own flags. It is required for
// --compress/--netbuffer, and because those are the only features that have
// ever required it, vmsync treats one of them being in use as its only
// evidence that this binary is present: without them the integrity check is
// skipped rather than attempted and failed.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"vmsync/pkg/netbuffer"
	"vmsync/pkg/streamrelay"
	"vmsync/pkg/util"
	"vmsync/pkg/version"
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
func validateCompressLevel(algo streamrelay.Algo, level string) error {
	if algo == streamrelay.AlgoS2 {
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

// helperConfig is the resolved, validated configuration one helper process
// runs with: built once in main() from the flags, then passed unchanged to
// serve and on to every handleConn.
//
// A struct rather than a positional parameter list for these two
// functions. As positional arguments these fields number seven, four of
// them plain strings and three of those adjacent (Level, NetBufferBlock,
// NetBufferSize), with ListenAddr/ConnectAddr an adjacent pair of their
// own. Transposing any same-typed pair compiles perfectly and fails at
// runtime instead: swapped netbuffer arguments configure the buffer
// backwards and merely relay badly, swapped addresses make the helper
// listen where it should dial, and a compression level landing in a
// netbuffer slot is parsed as a byte size. Named fields make each of those
// a visible mistake at the assignment rather than a silent one at the call.
type helperConfig struct {
	// ListenAddr is the local host:port to accept bridged connections on.
	ListenAddr string
	// ConnectAddr is the real endpoint dialed once per accepted connection.
	ConnectAddr string

	// Compress gates the compression stage entirely; Algo and Level are
	// only consulted when it is true.
	Compress bool
	Algo     streamrelay.Algo
	Level    string

	// NetBufferBlock/NetBufferSize are the two halves of
	// -netbuffer=<blocksize>,<buffersize>, already split and validated by
	// netbuffer.ParseSpec. Both empty means the buffering stage is off.
	NetBufferBlock string
	NetBufferSize  string
}

func main() {
	listenAddr := flag.String("listen", "", "local host:port to listen on for bridged connections (required)")
	connectAddr := flag.String("connect", "", "real endpoint host:port to dial and forward plaintext traffic to/from, once per accepted connection (required)")
	compressArg := optionalValueFlag{bareDefault: "s2"}
	netBufferArg := optionalValueFlag{bareDefault: "128k,1G"}
	flag.Var(&compressArg, "compress", "Same syntax as vmsync")
	// Empty default for the same reason vmsync's is empty: no single literal
	// is valid for both algorithms. Resolved per algorithm below.
	level := flag.String("compress-level", "", "Same syntax as vmsync: a number 1-19 for zstd (default 3), or default|better|best for s2 (default better). Resolves per algorithm when unset")
	flag.Var(&netBufferArg, "netbuffer", "Same syntax as vmsync")
	// Checksum mode. One-shot and unrelated to the relay: it reads a range
	// plan on stdin, hashes those ranges off a local qemu-nbd export, and
	// prints one digest line per block. See checksum.go.
	checksumMode := flag.Bool("checksum", false, "Run one digest pass instead of the bridge relay: read a digest request on stdin, hash the ranges it names off -nbd/-export, write a digest response on stdout. Requires -nbd and -export; -listen/-connect are not used. The digest algorithm and block size come from the request itself, so there is nothing to configure here and nothing that can disagree with vmsync")
	checksumNBD := flag.String("nbd", "", "With -checksum: where to reach the qemu-nbd export to hash -- either a Unix socket path (anything starting with \"/\") or a host:port. Always local to this host, since the point of computing digests here is that the bytes already are. vmsync uses a socket for the pre-commit check, which is why that check costs no TCP port at all, and 127.0.0.1:<port> for the -verify export, which it needs on TCP for its own reads")
	checksumExport := flag.String("export", "", "With -checksum: NBD export name to ask for. Required, and not merely for symmetry -- asking by name is what makes a stale export from an earlier run fail the handshake instead of being hashed as if it were this disk")
	checksumTimeout := flag.Duration("nbd-timeout", time.Minute, "With -checksum: deadline for each individual NBD socket operation. Not a deadline for the whole pass, which legitimately takes many round trips on a large disk")
	// The target-side run lock, held here under a lease instead of by a remote
	// shell parked on `cat`, so it is released when the driver holding it dies
	// rather than when TCP keepalive eventually gives up. See runlock.go.
	holdRunLockPath := flag.String("hold-run-lock", "", "Hold vmsync's target-side run lock on this path until stdin goes quiet, instead of running the bridge relay. Prints "+util.RemoteLockReady+" once held, then releases the lock if no heartbeat line arrives within -lock-lease. -listen/-connect are not used")
	lockLease := flag.Duration("lock-lease", 90*time.Second, "With -hold-run-lock: how long the lock survives silence from the driver. vmsync beats at a third of this, so two lost beats do not release a lock whose driver is healthy")
	lockStamp := flag.String("lock-stamp", "", "With -hold-run-lock: provenance to record in the lock file, as the JSON of a run lock identity. Read by whoever has to decide whether breaking the lock is safe; the pid and boot id in it are filled in by this process, which is the only one that knows them")
	showVersion := flag.Bool("v", false, "Show version and exit")
	showVersionLong := flag.Bool("version", false, "Show version and exit")
	flag.Parse()

	if *showVersion || *showVersionLong {
		fmt.Println(version.Version)
		os.Exit(0)
	}

	// See the identical check (and its comment) in cmd/vmsync/main.go: a
	// flag.Var flag whose Value implements IsBoolFlag (-compress and
	// -netbuffer, here) never consumes a following space-separated argument
	// as its value, so a mistaken "-compress zstd" leaves "zstd" as a
	// positional argument, which stops flag parsing right there and
	// silently drops every flag after it. This binary takes no positional
	// arguments at all, so any leftover ones are unambiguously a mistake.
	if flag.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "vmsync-bridge-helper: unexpected extra argument(s) %v -- if you meant to pass a value to -compress or -netbuffer, use -compress=value / -netbuffer=value (with an \"=\"), not a space\n", flag.Args())
		os.Exit(2)
	}

	if *holdRunLockPath != "" {
		// Refused rather than ignored, for the same reason -checksum refuses
		// the relay flags below: a caller that passed both has a wrong idea of
		// what this process will do, and doing the other thing silently is how
		// that stays undiscovered. Here it matters more than usual -- a caller
		// that thinks it holds a lock and does not would run concurrently with
		// whoever really does.
		if *listenAddr != "" || *connectAddr != "" || *checksumMode {
			fmt.Fprintln(os.Stderr, "vmsync-bridge-helper: -hold-run-lock holds a lock and does nothing else, so -listen/-connect/-checksum must not be given with it")
			os.Exit(2)
		}
		err := holdRunLock(runLockConfig{
			Path:  *holdRunLockPath,
			Lease: *lockLease,
			Stamp: *lockStamp,
		}, os.Stdin, os.Stdout)
		marker, code := runLockExit(err)
		if marker != "" {
			// On stdout, where the caller's ready-line reader is looking. The
			// detail goes to stderr so the marker stays the whole first line.
			fmt.Println(marker)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "vmsync-bridge-helper: %v\n", err)
		}
		os.Exit(code)
	}

	if *checksumMode {
		// Refuse the mixed invocation rather than quietly ignoring the
		// relay flags. A caller that passed -listen alongside -checksum has
		// a wrong idea of what this process will do, and silently doing the
		// other thing is how that stays undiscovered.
		if *listenAddr != "" || *connectAddr != "" {
			fmt.Fprintln(os.Stderr, "vmsync-bridge-helper: -checksum runs one checksum pass and never relays, so -listen/-connect must not be given with it")
			os.Exit(2)
		}
		if *checksumNBD == "" {
			fmt.Fprintln(os.Stderr, "vmsync-bridge-helper: -checksum requires -nbd")
			os.Exit(2)
		}
		if *checksumExport == "" {
			fmt.Fprintln(os.Stderr, "vmsync-bridge-helper: -checksum requires -export")
			os.Exit(2)
		}
		cfg := checksumConfig{
			NBDAddr: *checksumNBD,
			Export:  *checksumExport,
			Timeout: *checksumTimeout,
			// stderr, never stdout: stdout is the digest response, and a
			// progress line mixed into it would fail the far side's parse.
			Progress: os.Stderr,
		}
		if err := runChecksum(context.Background(), cfg, os.Stdin, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "vmsync-bridge-helper: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if *listenAddr == "" {
		fmt.Fprintln(os.Stderr, "vmsync-bridge-helper: -listen is required")
		os.Exit(2)
	}
	if *connectAddr == "" {
		fmt.Fprintln(os.Stderr, "vmsync-bridge-helper: -connect is required")
		os.Exit(2)
	}

	compress := compressArg.value != ""
	var algo streamrelay.Algo
	if compress {
		var err error
		algo, err = streamrelay.ParseAlgo(compressArg.value)
		if err != nil {
			fmt.Fprintf(os.Stderr, "vmsync-bridge-helper: %v\n", err)
			os.Exit(2)
		}
		// Same resolution as vmsync's own, through the same function: this
		// process is started BY vmsync (see nbdbridge.BuildStartCommand), so
		// the two disagreeing about an unset level would mean a relay
		// compressing at one setting while the operator was told another.
		*level = streamrelay.ResolveLevel(algo, *level)
		if err := validateCompressLevel(algo, *level); err != nil {
			fmt.Fprintf(os.Stderr, "vmsync-bridge-helper: %v\n", err)
			os.Exit(2)
		}
	}

	netbufferBlock, netbufferSize, err := netbuffer.ParseSpec(netBufferArg.value)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vmsync-bridge-helper: %v\n", err)
		os.Exit(2)
	}

	cfg := helperConfig{
		ListenAddr:     *listenAddr,
		ConnectAddr:    *connectAddr,
		Compress:       compress,
		Algo:           algo,
		Level:          *level,
		NetBufferBlock: netbufferBlock,
		NetBufferSize:  netbufferSize,
	}

	if err := serve(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "vmsync-bridge-helper: %v\n", err)
		os.Exit(1)
	}
}

// serve listens on listenAddr and hands each accepted connection to
// handleConn on its own goroutine, indefinitely -- the same "listen, fork
// per connection" role as socat's "TCP-LISTEN:...,fork", done natively so
// the remote host needs no socat installed at all.
func serve(cfg helperConfig) error {
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.ListenAddr, err)
	}
	defer ln.Close()

	for {
		conn, err := ln.Accept()
		if err != nil {
			return fmt.Errorf("accept on %s: %w", cfg.ListenAddr, err)
		}
		go handleConn(conn, cfg)
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
// lets a panic escape -- every connection shares this one long-lived
// process, with no per-connection crash isolation from the OS, so an
// unrecovered panic here would take down every other connection this helper
// is currently serving, not just this one.
func handleConn(conn net.Conn, cfg helperConfig) {
	defer conn.Close()
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "vmsync-bridge-helper: connection handler panic: %v\n", r)
		}
	}()

	real, err := net.Dial("tcp", cfg.ConnectAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vmsync-bridge-helper: dial %s: %v\n", cfg.ConnectAddr, err)
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
			err := streamrelay.RelayFromWire(real, conn, cfg.Compress, cfg.Algo, cfg.NetBufferBlock, cfg.NetBufferSize, nil)
			if tc, ok := real.(*net.TCPConn); ok {
				tc.CloseWrite() // half-close: tell the real server we're done sending
			}
			return err
		}))
	}()

	// real (plaintext, from the real NBD server) -> [compress+flush] -> [buffer] -> conn (wire, back to the client)
	//
	// The explicit CloseWrite here is what signals "done" to the peer: this
	// helper is a persistent daemon serving many connections, so nothing
	// else will ever half-close conn's write side -- without this, the local
	// relay on the other end of the SSH channel would block forever waiting
	// for an EOF that can never arrive (the same class of hang a SIGQUIT
	// goroutine dump exposes on the opposite direction).
	go func() {
		defer wg.Done()
		reportErr(recoverRelayPanic("outbound relay (real -> conn)", func() error {
			err := streamrelay.Relay(conn, real, cfg.Compress, cfg.Algo, cfg.Level, cfg.NetBufferBlock, cfg.NetBufferSize, nil)
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
