//go:build unix

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

package disk

import (
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func requireQemuImg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not available")
	}
	if _, err := exec.LookPath("qemu-io"); err != nil {
		t.Skip("qemu-io not available")
	}
}

func mkImage(t *testing.T, path, size string, writes ...string) {
	t.Helper()
	if out, err := exec.Command("qemu-img", "create", "-f", "qcow2", path, size).CombinedOutput(); err != nil {
		t.Fatalf("create %s: %v: %s", path, err, out)
	}
	for _, w := range writes {
		if out, err := exec.Command("qemu-io", "-c", w, path).CombinedOutput(); err != nil {
			t.Fatalf("qemu-io %q on %s: %v: %s", w, path, err, out)
		}
	}
}

// THE DEFAULT MODE'S SIZE BEHAVIOUR IS THE ONE WE WANT, in both directions.
//
// This is the test that stops someone "fixing" CompareImages by adding -s.
// It pins the two outcomes the doc comment claims, by running the real
// qemu-img rather than citing its manual:
//
//   - a larger source whose extra region holds DATA -> differ, because data
//     is missing from the replica
//   - a larger source whose extra region is ZEROS -> identical, because
//     nothing is missing; the size difference is reported through the output
//     instead
func TestCompareImagesSizeBehaviourIsTheOneWeWant(t *testing.T) {
	requireQemuImg(t)
	dir := t.TempDir()

	t.Run("extra region holds data: differ", func(t *testing.T) {
		src := filepath.Join(dir, "d-src.qcow2")
		dst := filepath.Join(dir, "d-dst.qcow2")
		mkImage(t, src, "2G", "write -P 0xaa 1500M 1M")
		mkImage(t, dst, "1G")

		said, err := CompareImages(src, dst)
		if !errors.Is(err, ErrImagesDiffer) {
			t.Errorf("err = %v, want ErrImagesDiffer: the source holds data the replica cannot (%s)", err, said)
		}
	})

	t.Run("extra region is zeros: identical, and the size is still reported", func(t *testing.T) {
		src := filepath.Join(dir, "z-src.qcow2")
		dst := filepath.Join(dir, "z-dst.qcow2")
		mkImage(t, src, "2G", "write -P 0xbb 0 1M")
		mkImage(t, dst, "1G", "write -P 0xbb 0 1M")

		said, err := CompareImages(src, dst)
		if err != nil {
			t.Fatalf("err = %v (%s): no data is missing, so this is not a finding about the data", err, said)
		}
		// THE OUTPUT IS RETURNED ON SUCCESS. This used to be discarded, and
		// it is the only notice an operator gets that a resize has not
		// reached the replica.
		if !strings.Contains(said, "size mismatch") {
			t.Errorf("output on a size-differing pass was %q, with no mention of the size. That warning is the signal a resize is mid-flight", said)
		}
	})
}

// -s WOULD REFUSE HEALTHY REPLICAS, which is why the doc says not to add it.
//
// Two images of the SAME size holding IDENTICAL data, differing only in
// allocation -- which is the ordinary state of a vmsync replica, because it
// writes only the extents the dirty bitmap names and skips extents that read
// as zeros. The default mode passes; strict mode fails on block status.
//
// If this test ever fails because strict mode stopped comparing allocation,
// then -s becomes usable and the doc comment on CompareImages should be
// revisited. Until then it is a trap.
func TestStrictModeWouldRefuseAHealthyReplica(t *testing.T) {
	requireQemuImg(t)
	dir := t.TempDir()
	a := filepath.Join(dir, "a.qcow2")
	b := filepath.Join(dir, "b.qcow2")

	// a has a region of explicitly written zeros (allocated); b leaves the
	// same region unallocated. Same data, same size.
	mkImage(t, a, "1G", "write -P 0x00 0 64M", "write -P 0xcd 128M 1M")
	mkImage(t, b, "1G", "write -P 0xcd 128M 1M")

	if _, err := CompareImages(a, b); err != nil {
		t.Fatalf("the default mode calls two identical-data images different: %v", err)
	}

	strict := exec.Command("qemu-img", "compare", "-U", "-s", a, b)
	out, err := strict.CombinedOutput()
	if err == nil {
		t.Errorf("strict mode accepted images differing in allocation; -s may now be usable, and CompareImages' doc comment should be revisited. Output: %s", out)
	}
}
