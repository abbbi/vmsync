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

package zstdrelay

import (
	"fmt"
	"io"
	"strconv"

	"github.com/klauspost/compress/zstd"
)

// ParseByteSize parses a size string like "64k", "512M", or a bare number of
// bytes, returning the value in bytes. Suffixes are case-insensitive and
// binary (k=1024, m=1024*1024, ...). Deliberately independent of
// pkg/nbdbridge's ParseNetBufferSpec (which only validates the CLI flag's
// string format) to avoid an import cycle -- nbdbridge imports zstdrelay,
// not the other way around.
func ParseByteSize(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	multiplier := 1
	numPart := s
	switch s[len(s)-1] {
	case 'b', 'B':
		numPart = s[:len(s)-1]
	case 'k', 'K':
		multiplier = 1024
		numPart = s[:len(s)-1]
	case 'm', 'M':
		multiplier = 1024 * 1024
		numPart = s[:len(s)-1]
	case 'g', 'G':
		multiplier = 1024 * 1024 * 1024
		numPart = s[:len(s)-1]
	case 't', 'T':
		multiplier = 1024 * 1024 * 1024 * 1024
		numPart = s[:len(s)-1]
	}
	n, err := strconv.Atoi(numPart)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", s, err)
	}
	return n * multiplier, nil
}

// Relay moves bytes from src to dst for one direction of a bridged
// connection, optionally compressing (with per-chunk Flush, so nothing sits
// buffered indefinitely) and/or passing through a bounded buffer for
// throughput smoothing -- in that order: compress nearest src, buffer
// nearest dst. This mirrors the positioning used throughout vmsync's bridge
// design (compress nearest the real data endpoint, buffer nearest the
// network hop) and is shared verbatim by both the local relay
// (pkg/nbdbridge/local.go) and the remote vmsync-bridge-helper binary, so
// the two ends can never drift apart in behavior.
//
// netbufferSize (the "buffersize" half of --netbuffer=blocksize,buffersize)
// sets the bounded buffer's capacity; netbufferBlock is accepted for
// CLI-format symmetry but not otherwise used -- BoundedBuffer has no fixed
// block-size concept of its own. wireCounter, if non-nil, is atomically
// incremented with the bytes actually written to dst (the wire), for the
// caller's own compression-savings reporting.
//
// Relay runs until src reaches EOF (returning nil) or a real error occurs.
// On EOF it properly finalizes each active stage (closing the zstd encoder
// so the peer's decoder sees a clean end-of-frame rather than an abrupt
// disconnect, and draining/closing the bounded buffer) before returning.
func Relay(dst io.Writer, src io.Reader, compress bool, level int, netbufferBlock, netbufferSize string, wireCounter *uint64) error {
	var effectiveDst io.Writer = dst
	if wireCounter != nil {
		effectiveDst = &CountingWriter{W: dst, Counter: wireCounter}
	}

	var bufStage *BoundedBuffer
	var drainDone chan error
	if netbufferBlock != "" || netbufferSize != "" {
		maxBytes, err := ParseByteSize(netbufferSize)
		if err != nil {
			return fmt.Errorf("parse netbuffer size %q: %w", netbufferSize, err)
		}
		bufStage = NewBoundedBuffer(maxBytes)
		// Snapshot the pre-buffer destination now: effectiveDst is
		// reassigned to bufStage right below, and since io.Writer is an
		// interface value, capturing effectiveDst itself in the closure
		// (rather than this snapshot) would race against that reassignment
		// -- the goroutine could end up copying bufStage into itself.
		preBufferDst := effectiveDst
		drainDone = make(chan error, 1)
		go func() {
			_, err := io.Copy(preBufferDst, bufStage)
			drainDone <- err
		}()
		effectiveDst = bufStage
	}

	var relayErr error
	if compress {
		enc, err := NewEncoder(effectiveDst, level)
		if err != nil {
			return fmt.Errorf("create zstd encoder: %w", err)
		}
		_, relayErr = CopyFlushing(enc, src)
		if closeErr := enc.Close(); relayErr == nil {
			relayErr = closeErr
		}
	} else {
		_, relayErr = io.Copy(effectiveDst, src)
	}

	if bufStage != nil {
		bufStage.Close()
		if drainErr := <-drainDone; relayErr == nil {
			relayErr = drainErr
		}
	}
	return relayErr
}

// RelayFromWire moves bytes from src (the wire) to dst (plaintext) -- the
// mirror image of Relay. A *reader* chain wraps the SOURCE (each stage
// decorates what the next stage reads from), so the stage order is reversed
// from Relay's write side: buffering (if enabled) sits nearest src, and
// decompression (if enabled) sits nearest dst.
//
// RelayFromWire runs until src reaches EOF (returning nil) or a real error
// occurs. On EOF it closes the decoder (if any) and, if a buffer stage is
// active, waits for its fill goroutine to finish -- the fill goroutine
// itself closes the buffer once src is drained, so the decoder (or the
// final io.Copy, if compression is off) sees a clean end rather than
// hanging.
func RelayFromWire(dst io.Writer, src io.Reader, compress bool, netbufferBlock, netbufferSize string, wireCounter *uint64) error {
	var effectiveSrc io.Reader = src
	if wireCounter != nil {
		effectiveSrc = &CountingReader{R: src, Counter: wireCounter}
	}

	var bufStage *BoundedBuffer
	var fillDone chan error
	if netbufferBlock != "" || netbufferSize != "" {
		maxBytes, err := ParseByteSize(netbufferSize)
		if err != nil {
			return fmt.Errorf("parse netbuffer size %q: %w", netbufferSize, err)
		}
		bufStage = NewBoundedBuffer(maxBytes)
		// Same snapshot-before-reassignment reasoning as Relay above:
		// effectiveSrc is about to be reassigned to bufStage, so the
		// goroutine must capture the pre-buffer source now, not the
		// variable itself.
		preBufferSrc := effectiveSrc
		fillDone = make(chan error, 1)
		go func() {
			_, err := io.Copy(bufStage, preBufferSrc)
			bufStage.Close() // signals EOF to whatever reads from bufStage below
			fillDone <- err
		}()
		effectiveSrc = bufStage
	}

	var decodedSrc io.Reader = effectiveSrc
	var dec *zstd.Decoder
	if compress {
		var err error
		dec, err = NewDecoder(effectiveSrc)
		if err != nil {
			return fmt.Errorf("create zstd decoder: %w", err)
		}
		decodedSrc = dec
	}

	_, relayErr := io.Copy(dst, decodedSrc)
	if dec != nil {
		dec.Close()
	}

	if bufStage != nil {
		if fillErr := <-fillDone; relayErr == nil {
			relayErr = fillErr
		}
	}
	return relayErr
}
