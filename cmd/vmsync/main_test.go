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

package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"vmsync/pkg/nbdsync"
)

// TestOverlapsAnyExtent exercises the half-open-interval overlap check used
// by -verify=online to distinguish a genuine mismatch from one that merely
// falls inside a region the guest wrote during the compare window.
func TestOverlapsAnyExtent(t *testing.T) {
	cases := []struct {
		name    string
		m       nbdsync.MismatchRange
		touched []nbdsync.Extent
		want    bool
	}{
		{
			name: "fully inside a dirty extent",
			m:    nbdsync.MismatchRange{Offset: 120, Length: 20}, // [120,140)
			touched: []nbdsync.Extent{
				{Offset: 100, Length: 100, Dirty: true}, // [100,200)
			},
			want: true,
		},
		{
			name: "fully outside all extents",
			m:    nbdsync.MismatchRange{Offset: 300, Length: 10}, // [300,310)
			touched: []nbdsync.Extent{
				{Offset: 100, Length: 100, Dirty: true}, // [100,200)
			},
			want: false,
		},
		{
			name: "partially overlapping at an edge",
			m:    nbdsync.MismatchRange{Offset: 190, Length: 20}, // [190,210)
			touched: []nbdsync.Extent{
				{Offset: 100, Length: 100, Dirty: true}, // [100,200), overlap [190,200)
			},
			want: true,
		},
		{
			name: "touching but not overlapping -- mismatch ends exactly where the extent begins",
			m:    nbdsync.MismatchRange{Offset: 150, Length: 50}, // [150,200)
			touched: []nbdsync.Extent{
				{Offset: 200, Length: 50, Dirty: true}, // [200,250)
			},
			want: false,
		},
		{
			name: "non-dirty extent covering the same range is skipped",
			m:    nbdsync.MismatchRange{Offset: 120, Length: 20}, // [120,140)
			touched: []nbdsync.Extent{
				{Offset: 100, Length: 100, Dirty: false}, // [100,200), but not dirty
			},
			want: false,
		},
		{
			name:    "empty touched slice",
			m:       nbdsync.MismatchRange{Offset: 0, Length: 10},
			touched: []nbdsync.Extent{},
			want:    false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := overlapsAnyExtent(tc.m, tc.touched)
			if got != tc.want {
				t.Fatalf("overlapsAnyExtent(%+v, %+v) = %v, want %v", tc.m, tc.touched, got, tc.want)
			}
		})
	}
}

// TestOptionalValueFlag verifies the bare/"=value"/"=false" tri-state
// behavior optionalValueFlag hijacks from flag.Value's IsBoolFlag mechanism
// (see the type's doc comment in main.go).
func TestOptionalValueFlag(t *testing.T) {
	t.Run("bare -name resolves to bareDefault", func(t *testing.T) {
		f := optionalValueFlag{bareDefault: "s2"}
		if err := f.Set("true"); err != nil {
			t.Fatalf("Set(true) returned unexpected error: %v", err)
		}
		if f.value != "s2" {
			t.Fatalf("value = %q, want %q", f.value, "s2")
		}
	})

	t.Run("-name=false disables it", func(t *testing.T) {
		f := optionalValueFlag{bareDefault: "s2"}
		if err := f.Set("false"); err != nil {
			t.Fatalf("Set(false) returned unexpected error: %v", err)
		}
		if f.value != "" {
			t.Fatalf("value = %q, want empty string", f.value)
		}
	})

	t.Run("-name=x is a literal passthrough", func(t *testing.T) {
		f := optionalValueFlag{bareDefault: "s2"}
		if err := f.Set("zstd"); err != nil {
			t.Fatalf("Set(zstd) returned unexpected error: %v", err)
		}
		if f.value != "zstd" {
			t.Fatalf("value = %q, want %q", f.value, "zstd")
		}
	})

	t.Run("IsBoolFlag is always true", func(t *testing.T) {
		f := optionalValueFlag{bareDefault: "s2"}
		if !f.IsBoolFlag() {
			t.Fatal("IsBoolFlag() = false, want true")
		}
		_ = f.Set("false")
		if !f.IsBoolFlag() {
			t.Fatal("IsBoolFlag() = false after Set(false), want true")
		}
	})

	t.Run("String reflects the current value", func(t *testing.T) {
		f := optionalValueFlag{bareDefault: "s2"}
		if err := f.Set("zstd"); err != nil {
			t.Fatalf("Set(zstd) returned unexpected error: %v", err)
		}
		if f.String() != "zstd" {
			t.Fatalf("String() = %q, want %q", f.String(), "zstd")
		}
	})
}

// TestCallWithTimeout checks the three behaviors callWithTimeout's doc
// comment promises: a fast success passes through untouched, a fast failure
// passes through untouched, and a call that outlives its timeout is given up
// on -- without callWithTimeout itself blocking on the still-running fn.
func TestCallWithTimeout(t *testing.T) {
	t.Run("fast success returns nil well before the timeout", func(t *testing.T) {
		timeout := 200 * time.Millisecond
		start := time.Now()
		err := callWithTimeout("op", timeout, func() error {
			return nil
		})
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("callWithTimeout returned %v, want nil", err)
		}
		if elapsed >= timeout {
			t.Fatalf("callWithTimeout took %s, want well under the %s timeout", elapsed, timeout)
		}
	})

	t.Run("fast failure propagates the exact error", func(t *testing.T) {
		timeout := 200 * time.Millisecond
		wantErr := errors.New("boom")
		err := callWithTimeout("op", timeout, func() error {
			return wantErr
		})
		if err != wantErr {
			t.Fatalf("callWithTimeout returned %v, want the exact sentinel error %v", err, wantErr)
		}
	})

	t.Run("a call that outlives the timeout is given up on promptly", func(t *testing.T) {
		timeout := 50 * time.Millisecond
		sleep := 300 * time.Millisecond
		start := time.Now()
		err := callWithTimeout("stuck-op", timeout, func() error {
			time.Sleep(sleep)
			return nil
		})
		elapsed := time.Since(start)

		if err == nil {
			t.Fatal("callWithTimeout returned nil, want a timeout error")
		}
		if !strings.Contains(err.Error(), "timed out after") {
			t.Fatalf("error = %q, want it to contain %q", err.Error(), "timed out after")
		}
		// callWithTimeout must not wait for the orphaned goroutine to finish
		// its full sleep -- it should give up right around timeout, well
		// short of the fn's full sleep duration.
		if elapsed >= sleep {
			t.Fatalf("callWithTimeout took %s, want it to return well before the fn's %s sleep completes", elapsed, sleep)
		}
	})
}

// TestVerificationRan pins down the exact gating rule that keeps
// vmsync_verification_state/vmsync_verification_timestamp_seconds out of a
// plain sync or -reinit run's metrics: verification must have both been
// requested (verify != "") AND actually reached the compare block
// (attempted), not merely requested.
func TestVerificationRan(t *testing.T) {
	cases := []struct {
		verify    string
		attempted bool
		want      bool
	}{
		{verify: "", attempted: false, want: false},  // plain sync / -reinit -- the critical case
		{verify: "", attempted: true, want: false},   // defensive: shouldn't happen in practice
		{verify: "compare", attempted: false, want: false},
		{verify: "fast", attempted: false, want: false},
		{verify: "online", attempted: false, want: false},
		{verify: "compare", attempted: true, want: true},
		{verify: "fast", attempted: true, want: true},
		{verify: "online", attempted: true, want: true},
	}

	for _, tc := range cases {
		got := verificationRan(tc.verify, tc.attempted)
		if got != tc.want {
			t.Errorf("verificationRan(%q, %v) = %v, want %v", tc.verify, tc.attempted, got, tc.want)
		}
	}
}
