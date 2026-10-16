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

package libvirtsync

import (
	"errors"
	"fmt"
	"testing"

	"libvirt.org/go/libvirt"
)

// The guarantee is narrow and worth stating: IsCheckpointInconsistent keys on
// the numeric code and on nothing else. Every case here that returns false
// returns false for a reason a prose-matching implementation would get wrong
// in at least one locale, which is the mistake its prose-matching sibling
// IsCheckpointBlockedBySnapshot is stuck with and this one deliberately is
// not.
func TestIsCheckpointInconsistent(t *testing.T) {
	const realMessage = "missing or broken bitmap 'vmsync-cpt-3' for disk 'vda'"

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{
			"the real thing, unwrapped",
			libvirt.Error{Code: libvirt.ERR_CHECKPOINT_INCONSISTENT, Message: realMessage},
			true,
		},
		{
			// StartPullBackupTCP wraps with %w before anything sees the
			// error, so this is the shape the production caller actually
			// classifies. A type assertion instead of errors.As would
			// report false here and the guard would never fire.
			"wrapped once, as StartPullBackupTCP wraps it",
			fmt.Errorf("start pull backup (tcp 127.0.0.1:10809): %w (xml=<domainbackup/>)",
				libvirt.Error{Code: libvirt.ERR_CHECKPOINT_INCONSISTENT, Message: realMessage}),
			true,
		},
		{
			"wrapped twice",
			fmt.Errorf("sync disk vda: %w", fmt.Errorf("start pull backup: %w",
				libvirt.Error{Code: libvirt.ERR_CHECKPOINT_INCONSISTENT, Message: realMessage})),
			true,
		},
		{
			// libvirtd formats server-side in its own locale. The code is
			// identical, so the verdict must be too.
			"same code, message in French",
			libvirt.Error{Code: libvirt.ERR_CHECKPOINT_INCONSISTENT, Message: "bitmap « vmsync-cpt-3 » manquant ou corrompu pour le disque « vda »"},
			true,
		},
		{
			// The inverse mistake: the prose is right but the condition is
			// something else entirely. Matching text would call this a
			// broken bitmap and tell the operator to -reinit for nothing.
			"different code, message still mentions a bitmap",
			libvirt.Error{Code: libvirt.ERR_OPERATION_FAILED, Message: realMessage},
			false,
		},
		{
			"a neighbouring checkpoint code is not this one",
			libvirt.Error{Code: libvirt.ERR_NO_DOMAIN_CHECKPOINT, Message: "no such checkpoint"},
			false,
		},
		{
			"not a libvirt.Error at all, however it reads",
			errors.New(realMessage),
			false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsCheckpointInconsistent(tc.err); got != tc.want {
				t.Errorf("IsCheckpointInconsistent(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// The constant must be the one libvirt actually raises. A binding that
// renumbered it, or a hand-written 109 that drifted, would make every test
// above pass against each other while the guard never fires in production.
func TestCheckpointInconsistentIsLibvirtCode109(t *testing.T) {
	if got := int(libvirt.ERR_CHECKPOINT_INCONSISTENT); got != 109 {
		t.Errorf("libvirt.ERR_CHECKPOINT_INCONSISTENT = %d, want 109 (VIR_ERR_CHECKPOINT_INCONSISTENT, virterror.h)", got)
	}
}
