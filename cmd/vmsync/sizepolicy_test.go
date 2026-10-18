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

package main

import (
	"errors"
	"fmt"
	"testing"

	"vmsync/pkg/disk"
	"vmsync/pkg/nbdsync"
)

// A SIZE MISMATCH MUST NOT READ AS A FINDING ABOUT THE DATA.
//
// isVerifyMismatch decides what counts toward failure_count AND what records
// verify_state=failed, which then refuses every subsequent sync until an
// operator runs the repair ladder -- a full re-copy of the replica. A size
// difference means nothing was compared at all, so it cannot be a finding
// about the bytes, and it must never latch that.
func TestASizeMismatchIsNotAVerifyMismatch(t *testing.T) {
	sized := fmt.Errorf("%w: source=20 target=10", nbdsync.ErrSizeMismatch)
	if isVerifyMismatch(sized) {
		t.Error("a size mismatch counts as a verify mismatch, so it would record verify_state=failed and refuse later syncs over a replica nothing has looked at")
	}

	// Not vacuous: the real findings still count, from both comparators.
	if !isVerifyMismatch(fmt.Errorf("%w: ranges differ", nbdsync.ErrImagesDiffer)) {
		t.Error("a genuine data difference from nbdsync no longer counts as a mismatch")
	}
	if !isVerifyMismatch(fmt.Errorf("%w: content mismatch", disk.ErrImagesDiffer)) {
		t.Error("a genuine data difference from qemu-img no longer counts as a mismatch")
	}
}

// Whether a size difference is excused depends on whether this run staged
// anything, and on nothing else.
func TestSizeMismatchIsExplainedOnlyWhenNothingWasStaged(t *testing.T) {
	sized := fmt.Errorf("%w: source=20 target=10", nbdsync.ErrSizeMismatch)
	differ := fmt.Errorf("%w: ranges differ", nbdsync.ErrImagesDiffer)
	other := errors.New("the verify export would not start")

	for _, c := range []struct {
		name        string
		err         error
		stagedDelta bool
		want        bool
	}{
		{
			// The case this exists for: an operator resized the source and
			// the next sync had nothing to copy, so nothing grew the base.
			"size mismatch, nothing staged", sized, false, true,
		},
		{
			// This run created an image at the source's current size and
			// committed it, so the base must match. A difference is a fault.
			"size mismatch, a delta was staged", sized, true, false,
		},
		{
			// A real data difference is never excused, staged or not.
			"data difference, nothing staged", differ, false, false,
		},
		{"data difference, a delta was staged", differ, true, false},
		{
			// Nor is a failure to look at all, which would otherwise be
			// silently swallowed on every quiet sync.
			"some other failure, nothing staged", other, false, false,
		},
		{"no error at all", nil, false, false},
		{"no error, delta staged", nil, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := sizeMismatchIsExplained(c.err, c.stagedDelta); got != c.want {
				t.Errorf("sizeMismatchIsExplained(%v, staged=%v) = %v, want %v", c.err, c.stagedDelta, got, c.want)
			}
		})
	}
}

// And the wrapping survives, so the predicate works on an error that has
// been annotated on its way up rather than only on the bare sentinel.
func TestSizeMismatchIsRecognisedThroughWrapping(t *testing.T) {
	deep := fmt.Errorf("verify disk vda: %w", fmt.Errorf("planning the comparison failed: %w",
		fmt.Errorf("%w: source=20 target=10", nbdsync.ErrSizeMismatch)))
	if !sizeMismatchIsExplained(deep, false) {
		t.Error("a size mismatch wrapped by the layers above it is no longer recognised, so the tolerance would not apply where it is reached in practice")
	}
	if isVerifyMismatch(deep) {
		t.Error("the same wrapped error reads as a data finding")
	}
}
