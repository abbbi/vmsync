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
	"hash"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"vmsync/pkg/checksum"
	"vmsync/pkg/netbuffer"
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

// helperConfig is the resolved, validated configuration one helper process
// runs with: built once in main() from the flags, then passed unchanged to
// serve and on to every handleConn.
//
// A struct rather than the positional parameter list these two functions
// used to take. That list had grown to seven arguments, four of them plain
// strings and three of those adjacent (Level, NetBufferBlock,
// NetBufferSize), with ListenAddr/ConnectAddr an adjacent pair of their
// own. Transposing any same-typed pair compiled perfectly and failed at
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
	Algo     zstdrelay.Algo
	Level    string

	// NetBufferBlock/NetBufferSize are the two halves of
	// -netbuffer=<blocksize>,<buffersize>, already split and validated by
	// netbuffer.ParseSpec. Both empty means the buffering stage is off.
	NetBufferBlock string
	NetBufferSize  string
	// Checksum enables rolling hash verification of plaintext. "auto" is
	// resolved on the helper's own host to crc32c if hw else xxhash; the
	// caller (BuildStartCommand) already passes the resolved concrete algo
	// so both sides use the same one.
	Checksum string
}

func main() {
	listenAddr := flag.String("listen", "", "local host:port to listen on for bridged connections (required)")
	connectAddr := flag.String("connect", "", "real endpoint host:port to dial and forward plaintext traffic to/from, once per accepted connection (required)")
	compressArg := optionalValueFlag{bareDefault: "s2"}
	netBufferArg := optionalValueFlag{bareDefault: "128k,1G"}
	checksumArg := optionalValueFlag{bareDefault: "crc32c"}
	flag.Var(&compressArg, "compress", "Same syntax as vmsync")
	level := flag.String("compress-level", "3", "Same syntax as vmsync")
	flag.Var(&netBufferArg, "netbuffer", "Same syntax as vmsync")
	flag.Var(&checksumArg, "checksum", "rolling checksum algo auto|crc32c|xxhash|off (default off); when set both sides hash plaintext and helper writes final hash to /run/vmsync-bridge/checksum-<port>.hash")
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

	netbufferBlock, netbufferSize, err := netbuffer.ParseSpec(netBufferArg.value)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vmsync-bridge-helper: %v\n", err)
		os.Exit(2)
	}
	// Validate checksum if provided — helper accepts the resolved concrete
	// algo from BuildStartCommand (not auto), so we just check it parses.
	if checksumArg.value != "" {
		if _, err := checksum.Parse(checksumArg.value); err != nil {
			fmt.Fprintf(os.Stderr, "vmsync-bridge-helper: %v\n", err)
			os.Exit(2)
		}
	}

	cfg := helperConfig{
		ListenAddr:     *listenAddr,
		ConnectAddr:    *connectAddr,
		Compress:       compress,
		Algo:           algo,
		Level:          *level,
		NetBufferBlock: netbufferBlock,
		NetBufferSize:  netbufferSize,
		Checksum:       checksumArg.value,
	}

	if err := serve(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "vmsync-bridge-helper: %v\n", err)
		os.Exit(1)
	}
}

// serve listens on listenAddr and hands each accepted connection to
// handleConn on its own goroutine, indefinitely -- the same "listen, fork
// per connection" role socat's "TCP-LISTEN:...,fork" used to play, now done
// natively so the remote host no longer needs socat installed at all.
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

// hashingReader wraps an io.Reader and feeds every byte read into h.
type hashingReader struct {
	R io.Reader
	H hash.Hash
	M *sync.Mutex
}

func (h *hashingReader) Read(p []byte) (int, error) {
	n, err := h.R.Read(p)
	if n > 0 && h.H != nil {
		h.M.Lock()
		_, _ = h.H.Write(p[:n])
		h.M.Unlock()
	}
	return n, err
}

// hashingWriter wraps an io.Writer and feeds every byte written into h.
type hashingWriter struct {
	W io.Writer
	H hash.Hash
	M *sync.Mutex
}

func (h *hashingWriter) Write(p []byte) (int, error) {
	if h.H != nil && len(p) > 0 {
		h.M.Lock()
		_, _ = h.H.Write(p)
		h.M.Unlock()
	}
	return h.W.Write(p)
}

// handleConn serves exactly one accepted connection: dial the real NBD
// endpoint and relay bidirectionally until either side is done. It never
// lets a panic escape -- unlike the old one-process-per-connection model
// (each connection was a separate socat-exec'd process, crash-isolated by
// the OS), all connections now share this one long-lived process, so an
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

	// Rolling hash of plaintext, separate per direction to avoid
	// non-deterministic interleaving, combined deterministically at the end.
	var outHash, inHash hash.Hash64
	var hMu sync.Mutex
	var hashAlgo checksum.Algo
	if cfg.Checksum != "" {
		if a, err := checksum.Parse(cfg.Checksum); err == nil && a != checksum.AlgoNone {
			a = checksum.Resolve(a)
			if h1, err := checksum.New(a); err == nil {
				if h2, err := checksum.New(a); err == nil {
					outHash = h1
					inHash = h2
					hashAlgo = a
				}
			}
		}
	}
	// Wrap plaintext side so hash sees bytes before compress / after decompress.
	realReaderForRelay := io.Reader(real)
	realWriterForRelay := io.Writer(real)
	if outHash != nil {
		realReaderForRelay = &hashingReader{R: real, H: outHash, M: &hMu}
	}
	if inHash != nil {
		realWriterForRelay = &hashingWriter{W: real, H: inHash, M: &hMu}
	}

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
			err := zstdrelay.RelayFromWire(realWriterForRelay, conn, cfg.Compress, cfg.Algo, cfg.NetBufferBlock, cfg.NetBufferSize, nil)
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
			err := zstdrelay.Relay(conn, realReaderForRelay, cfg.Compress, cfg.Algo, cfg.Level, cfg.NetBufferBlock, cfg.NetBufferSize, nil)
			if tc, ok := conn.(*net.TCPConn); ok {
				tc.CloseWrite()
			}
			return err
		}))
	}()

	wg.Wait()
	// Persist rolling hash for the source to verify single final hash
	// before merging. Written per-port so parallel disks (different ports)
	// do not interfere. Best-effort: failure to write just means the
	// source's re-read fallback will be used. Combined deterministically
	// from both directions.
	if outHash != nil && inHash != nil {
		outVal := outHash.Sum64()
		inVal := inHash.Sum64()
		final := outVal ^ (inVal<<1 | inVal>>63) ^ 0x9e3779b97f4a7c15
		if _, portStr, err := net.SplitHostPort(cfg.ListenAddr); err == nil {
			if port, err := strconv.Atoi(portStr); err == nil {
				dir := "/run/vmsync-bridge"
				_ = os.MkdirAll(dir, 0755)
				p := filepath.Join(dir, fmt.Sprintf("checksum-%d.hash", port))
				content := fmt.Sprintf("%s:%016x\n", hashAlgo, final)
				_ = os.WriteFile(p, []byte(content), 0644)
			}
		}
	}
	if firstErr != nil {
		fmt.Fprintf(os.Stderr, "vmsync-bridge-helper: connection %s: %v\n", conn.RemoteAddr(), firstErr)
	}
}
