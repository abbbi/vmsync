/*
	Copyright (C) 2026  Orsiris de Jong <ozy@netpower.fr>

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

You should have received a copy of the GNU General Public License
along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

package libvirtsync

import (
	"errors"
	"testing"

	libvirt "libvirt.org/go/libvirt"
)

// Reconnect has a three-step ordering -- dial, swap, close the old one --
// and each step is here for a different failure it prevents. Only the swap
// is visible on the Manager afterwards, which is why reconnectWith takes
// the close as a parameter: a test that could not see the close would pin
// one third of the contract while reading as though it pinned all of it.
//
// &libvirt.Connect{} is used as a pointer-identity sentinel throughout.
// Nothing dereferences it -- the injected close records the pointer instead
// of calling Close on it. (For the record, calling Close on a zero value is
// harmless: libvirt's virCheckConnectReturn is NULL-safe and it returns
// (-1, "invalid connection pointer in virConnectClose"). An earlier version
// of this file asserted the opposite and built its design on it.)

// recorder captures what reconnectWith did to the outside world.
type recorder struct {
	dialled []string
	closed  []*libvirt.Connect
}

// A FAILED RECONNECT MUST NOT LEAVE Conn NIL.
//
// Every method on Manager dereferences Conn without a guard, and so does
// libvirt-go beneath them -- its methods read c.ptr off the receiver with no
// nil check. Clearing Conn before dialling therefore turned the ordinary
// case of "the target hypervisor went away late in a run" into a
// nil-pointer panic in the final metadata phase, on a run that had already
// copied and committed every disk.
//
// The old connection is kept instead. It is broken by assumption, but a
// broken connection fails with libvirt's own error, which callers handle;
// nil panics.
func TestAFailedReconnectKeepsTheOldConnection(t *testing.T) {
	sentinel := &libvirt.Connect{}
	m := &Manager{Conn: sentinel, URI: "qemu+tcp://nonexistent.invalid/system"}
	var r recorder

	err := m.reconnectWith(
		func(uri string) (*libvirt.Connect, error) {
			r.dialled = append(r.dialled, uri)
			return nil, errors.New("unable to connect to server: connection refused")
		},
		func(c *libvirt.Connect) { r.closed = append(r.closed, c) },
	)

	if err == nil {
		t.Fatal("a failed dial reported success")
	}
	if len(r.dialled) != 1 || r.dialled[0] != m.URI {
		t.Errorf("dialled %v, want exactly one dial of %q", r.dialled, m.URI)
	}
	if m.Conn == nil {
		t.Fatal("Conn is nil after a failed reconnect; the next call through this Manager panics instead of returning libvirt's error, in the final metadata phase of a run that has already committed every disk")
	}
	if m.Conn != sentinel {
		t.Error("Conn was replaced despite the dial failing")
	}
	// THE OTHER HALF. Closing it here would hand the caller a Manager whose
	// Conn is a closed handle -- no better than nil, and worse to diagnose,
	// because the failure surfaces later and somewhere else.
	if len(r.closed) != 0 {
		t.Errorf("the old connection was closed on the failure path (%d time(s)); the Manager is left holding a closed handle", len(r.closed))
	}
}

// The success path: new connection installed, old one closed exactly once.
//
// Starting from a NON-nil Conn is the whole point -- it is the only way the
// close branch is entered at all.
func TestASuccessfulReconnectInstallsTheNewOneAndClosesTheOld(t *testing.T) {
	old := &libvirt.Connect{}
	fresh := &libvirt.Connect{}
	m := &Manager{Conn: old, URI: "qemu:///system"}
	var r recorder

	if err := m.reconnectWith(
		func(uri string) (*libvirt.Connect, error) {
			r.dialled = append(r.dialled, uri)
			return fresh, nil
		},
		func(c *libvirt.Connect) { r.closed = append(r.closed, c) },
	); err != nil {
		t.Fatalf("reconnectWith: %v", err)
	}

	if m.Conn != fresh {
		t.Error("the new connection was not installed, so every later call would keep using the broken one")
	}
	if len(r.closed) != 1 {
		t.Fatalf("the old connection was closed %d time(s), want exactly 1 -- a dropped close leaks one libvirt connection per reconnect, in exactly the hour-long runs this function exists for", len(r.closed))
	}
	if r.closed[0] != old {
		t.Error("something other than the old connection was closed")
	}
	// Order matters as much as the fact: closing before the swap is what the
	// old code did, and it is what left Conn pointing at a dead handle for
	// the width of a dial.
	if m.Conn == old {
		t.Error("Conn still points at the connection that was just closed")
	}
}

// A Manager that held nothing has nothing to close.
func TestReconnectFromNoConnectionClosesNothing(t *testing.T) {
	fresh := &libvirt.Connect{}
	m := &Manager{Conn: nil, URI: "qemu:///system"}
	var r recorder

	if err := m.reconnectWith(
		func(string) (*libvirt.Connect, error) { return fresh, nil },
		func(c *libvirt.Connect) { r.closed = append(r.closed, c) },
	); err != nil {
		t.Fatalf("reconnectWith: %v", err)
	}
	if m.Conn != fresh {
		t.Error("the new connection was not installed")
	}
	if len(r.closed) != 0 {
		t.Errorf("closed %d connection(s) when there was none to close", len(r.closed))
	}
}

// A nil Manager is a programming error, not a panic. Reconnect is called
// from error paths, which is exactly where a nil would otherwise turn a
// handled failure into a crash.
func TestReconnectOnANilManagerReports(t *testing.T) {
	var m *Manager
	err := m.reconnectWith(
		func(string) (*libvirt.Connect, error) {
			t.Error("a nil Manager dialled")
			return nil, nil
		},
		func(*libvirt.Connect) { t.Error("a nil Manager closed something") },
	)
	if err == nil {
		t.Error("a nil Manager reported a successful reconnect")
	}
}
