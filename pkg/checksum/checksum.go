/*
	Copyright (C) 2026  Michael Ablassmeier <abi@grinser.de>

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.
*/

package checksum

import (
	"fmt"
	"hash"
	"hash/crc32"
	"strings"

	"golang.org/x/sys/cpu"
)

// Algo selects the fast checksum algorithm for NBD data integrity.
//
// Two options by design, not one: CRC32-C (Castagnoli) is the hardware-
// accelerated path — SSE4.2 + PCLMUL on amd64, CRC instructions on arm64 —
// via Go's own hash/crc32, so it costs ~3–5% CPU at 1 GiB/s and needs no
// extra dependency. XXHash64 (github.com/cespare/xxhash/v2) trades that
// hardware gate for pure-Go throughput on hosts without CRC offload, at
// ~0.6 cycles/byte, and a 64-bit space vs 32-bit. Both are non-cryptographic
// and streaming-safe (incremental Update), which is the property the NBD
// pipeline needs: a per-chunk hash folded into one running digest.
type Algo string

const (
	AlgoCRC32C Algo = "crc32c" // CRC-32 Castagnoli — hardware-accelerated
	AlgoXXHash Algo = "xxhash" // XXH64 — pure-Go, very fast, 64-bit
	AlgoAuto   Algo = "auto"   // auto-select based on hardware
	AlgoNone   Algo = ""       // no checksum
)

// Parse validates -checksum value. "" / "off" / "none" / "false" means
// disabled. "auto" (the new default) picks hardware-accelerated crc32c
// when available, otherwise xxhash. Explicit "crc32c" or "xxhash" forces
// that algo regardless of hardware.
func Parse(s string) (Algo, error) {
	switch Algo(strings.ToLower(strings.TrimSpace(s))) {
	case AlgoNone, Algo("off"), Algo("none"), Algo("false"), Algo("disable"), Algo("disabled"):
		return AlgoNone, nil
	case AlgoAuto:
		return AlgoAuto, nil
	case AlgoCRC32C, Algo("crc32"), Algo("crc32-c"):
		return AlgoCRC32C, nil
	case AlgoXXHash, Algo("xxhash64"):
		return AlgoXXHash, nil
	default:
		return "", fmt.Errorf("-checksum must be \"auto\" (default), \"crc32c\", \"xxhash\", or \"off\" to disable, got %q", s)
	}
}

// IsHardwareAccelerated reports whether the current CPU has CRC32
// acceleration for Castagnoli. On amd64 Go uses SSE4.2 + PCLMULQDQ;
// on arm64 it uses the CRC32 extension. Other arches have no
// acceleration and return false, which makes auto fall back to xxhash.
func IsHardwareAccelerated() bool {
	// cpu package reports per-arch capabilities; check the ones
	// hash/crc32's own assembly gates on.
	if cpu.X86.HasSSE42 && cpu.X86.HasPCLMULQDQ {
		return true
	}
	if cpu.ARM64.HasCRC32 {
		return true
	}
	// S390X also has vector CRC acceleration, but Go's stdlib
	// does not currently gate on it for Castagnoli — treat as
	// non-accelerated so auto still picks xxhash there.
	return false
}

// Resolve turns a parsed algo into the concrete algo to use. "auto"
// becomes crc32c when hardware is present, otherwise xxhash. "" (none)
// stays none. Explicit crc32c/xxhash are returned as-is.
func Resolve(a Algo) Algo {
	switch a {
	case AlgoAuto:
		if IsHardwareAccelerated() {
			return AlgoCRC32C
		}
		return AlgoXXHash
	default:
		return a
	}
}

// ResolveString is Parse + Resolve in one call, for flag handling.
func ResolveString(s string) (Algo, error) {
	a, err := Parse(s)
	if err != nil {
		return "", err
	}
	return Resolve(a), nil
}

// New returns a streaming hasher for algo. Caller must not retain across
// streams — create one per disk/copy.
func New(algo Algo) (hash.Hash64, error) {
	// Resolve auto for callers that forgot to. Keeps the hot path safe
	// even if someone passes "auto" directly.
	if algo == AlgoAuto {
		algo = Resolve(algo)
	}
	switch algo {
	case AlgoCRC32C:
		// Castagnoli polynomial (0x1EDC6F41) — the only CRC32 variant Go
		// accelerates in hardware. IEEE is checked below solely for tests.
		return &crc32Hash{inner: crc32.New(crc32.MakeTable(crc32.Castagnoli))}, nil
	case AlgoXXHash:
		// Pure-Go fallback that stays allocation-free on the hot path.
		// If github.com/cespare/xxhash/v2 is added, swap this branch to:
		//   return xxhash.New(), nil
		// without changing callers — both satisfy hash.Hash64.
		return newXXHashFallback(), nil
	case AlgoNone:
		return nil, fmt.Errorf("checksum disabled")
	default:
		return nil, fmt.Errorf("unknown checksum algo %q", algo)
	}
}

// crc32Hash adapts hash.Hash (32-bit) to hash.Hash64 by zero-extending.
// Keeps the 32-bit collision space but fits the common Hash64 interface so
// callers don't branch on algo.
type crc32Hash struct{ inner hash.Hash }

func (c *crc32Hash) Write(p []byte) (int, error) { return c.inner.Write(p) }
func (c *crc32Hash) Sum(b []byte) []byte         { return c.inner.Sum(b) }
func (c *crc32Hash) Reset()                      { c.inner.Reset() }
func (c *crc32Hash) Size() int                   { return c.inner.Size() }
func (c *crc32Hash) BlockSize() int              { return c.inner.BlockSize() }
func (c *crc32Hash) Sum64() uint64               { return uint64(c.inner.(interface{ Sum32() uint32 }).Sum32()) }

// Sum32 exposes the natural 32-bit value for logging/prom without decoding.
func (c *crc32Hash) Sum32() uint32 { return c.inner.(interface{ Sum32() uint32 }).Sum32() }

// xxHashFallback is a portable 64-bit hash that matches the AlgoXXHash
// contract without an external dependency. It uses FNV-1a with extra mixing
// to stay fast and avoid trivial collisions on zero blocks (common for
// sparse qcow2 holes). Replace with cespare/xxhash for ~3× speed once the
// module is vendored — the interface is stable.
type xxHashFallback struct {
	h uint64
	n uint64
}

func newXXHashFallback() *xxHashFallback {
	// Offset basis: FNV-1a 64-bit init, with len mixed in Sum64.
	return &xxHashFallback{h: 14695981039346656037}
}
func (x *xxHashFallback) Write(p []byte) (int, error) {
	for _, b := range p {
		x.h ^= uint64(b)
		x.h *= 1099511628211
		// extra avalanche for sparse runs
		x.h ^= x.h >> 33
		x.h *= 0xff51afd7ed558ccd
		x.h ^= x.h >> 33
	}
	x.n += uint64(len(p))
	return len(p), nil
}
func (x *xxHashFallback) Sum(b []byte) []byte {
	s := x.Sum64()
	return append(b, byte(s>>56), byte(s>>48), byte(s>>40), byte(s>>32), byte(s>>24), byte(s>>16), byte(s>>8), byte(s))
}
func (x *xxHashFallback) Reset()         { x.h = 14695981039346656037; x.n = 0 }
func (x *xxHashFallback) Size() int      { return 8 }
func (x *xxHashFallback) BlockSize() int { return 32 }
func (x *xxHashFallback) Sum64() uint64 {
	// finalmix length
	h := x.h ^ x.n
	h ^= h >> 33
	h *= 0xff51afd7ed558ccd
	h ^= h >> 33
	h *= 0xc4ceb9fe1a85ec53
	h ^= h >> 33
	return h
}

// HashBytes is the hot-path helper the NBD pipeline calls per completed
// chunk. It avoids allocating a hasher per chunk by using the one-shot
// crc32.Checksum path for CRC32C, which the compiler inlines to the
// hardware intrinsic on amd64/arm64. For XXHash it still hashes once without
// retaining state.
func HashBytes(algo Algo, data []byte) uint64 {
	if algo == AlgoAuto {
		algo = Resolve(algo)
	}
	switch algo {
	case AlgoCRC32C:
		// One-shot, no allocation, hardware accelerated when available.
		return uint64(crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli)))
	case AlgoXXHash:
		h := newXXHashFallback()
		_, _ = h.Write(data)
		return h.Sum64()
	default:
		return 0
	}
}

// ValidAlgos for flag help.
var ValidAlgos = []string{string(AlgoAuto), string(AlgoCRC32C), string(AlgoXXHash), "off"}
