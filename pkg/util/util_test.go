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

package util

import (
	"context"
	"errors"
	"testing"

	"libvirt.org/go/libvirtxml"
)

func TestUriUsesSSH(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"ssh scheme", "qemu+ssh://host/system", true},
		{"plain qemu scheme", "qemu:///system", false},
		{"mixed case ssh scheme", "QEMU+SSH://host/system", true},
		{"invalid uri", "://not a uri", false},
		{"empty string", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := UriUsesSSH(tc.raw); got != tc.want {
				t.Errorf("UriUsesSSH(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestHostFromURIOrLocal(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"host present", "qemu+ssh://myhost/system", "myhost"},
		{"host with user and port", "qemu+ssh://user@myhost:2222/system", "myhost"},
		{"no host at all", "qemu:///system", "127.0.0.1"},
		{"invalid uri", "://bad", "127.0.0.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HostFromURIOrLocal(tc.raw); got != tc.want {
				t.Errorf("HostFromURIOrLocal(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestConnectHostFromBindOrURI(t *testing.T) {
	cases := []struct {
		name   string
		bind   string
		rawURI string
		want   string
	}{
		{"specific bind ip wins", "192.168.1.5", "qemu+ssh://myhost/system", "192.168.1.5"},
		{"unspecified ipv4 bind falls back to uri host", "0.0.0.0", "qemu+ssh://myhost/system", "myhost"},
		{"unspecified ipv6 bind falls back to uri host", "::", "qemu+ssh://myhost/system", "myhost"},
		{"non-ip bind falls back to uri host", "somehostname", "qemu+ssh://myhost/system", "myhost"},
		{"empty bind falls back to uri host", "", "qemu+ssh://myhost/system", "myhost"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ConnectHostFromBindOrURI(tc.bind, tc.rawURI); got != tc.want {
				t.Errorf("ConnectHostFromBindOrURI(%q, %q) = %q, want %q", tc.bind, tc.rawURI, got, tc.want)
			}
		})
	}
}

func TestShQuote(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain word", "hello", "'hello'"},
		{"with spaces", "hello world", "'hello world'"},
		{"single quote", "it's", `'it'\''s'`},
		{"empty string", "", "''"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ShQuote(tc.in); got != tc.want {
				t.Errorf("ShQuote(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSetTargetPath(t *testing.T) {
	cases := []struct {
		name           string
		targetDiskPath string
		diskPath       string
		want           string
	}{
		{"empty target path returns disk path unchanged", "", "/var/lib/libvirt/images/disk.qcow2", "/var/lib/libvirt/images/disk.qcow2"},
		{"target path joins with disk basename", "/mnt/target", "/var/lib/libvirt/images/disk.qcow2", "/mnt/target/disk.qcow2"},
		{"target path with trailing slash still joins cleanly", "/mnt/target/", "/some/dir/disk.qcow2", "/mnt/target/disk.qcow2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SetTargetPath(tc.targetDiskPath, tc.diskPath); got != tc.want {
				t.Errorf("SetTargetPath(%q, %q) = %q, want %q", tc.targetDiskPath, tc.diskPath, got, tc.want)
			}
		})
	}
}

func TestIgnoreDevice(t *testing.T) {
	cases := []struct {
		name string
		d    libvirtxml.DomainDisk
		want bool
	}{
		{
			name: "cdrom ignored even with qcow2 driver",
			d: libvirtxml.DomainDisk{
				Device: "cdrom",
				Driver: &libvirtxml.DomainDiskDriver{Type: "qcow2"},
			},
			want: true,
		},
		{
			name: "nil driver ignored (no <driver> element)",
			d: libvirtxml.DomainDisk{
				Device: "disk",
				Driver: nil,
			},
			want: true,
		},
		{
			name: "non-qcow2 driver ignored",
			d: libvirtxml.DomainDisk{
				Device: "disk",
				Driver: &libvirtxml.DomainDiskDriver{Type: "raw"},
			},
			want: true,
		},
		{
			name: "qcow2 disk not ignored",
			d: libvirtxml.DomainDisk{
				Device: "disk",
				Driver: &libvirtxml.DomainDiskDriver{Type: "qcow2"},
			},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IgnoreDevice(tc.d); got != tc.want {
				t.Errorf("IgnoreDevice(%+v) = %v, want %v", tc.d, got, tc.want)
			}
		})
	}
}

type fakeRunner struct {
	out string
	err error
}

func (f fakeRunner) Run(_ context.Context, _ string) (string, error) {
	return f.out, f.err
}

func TestRemotePathExists(t *testing.T) {
	t.Run("path exists", func(t *testing.T) {
		r := fakeRunner{out: "VMSYNC_PATH_EXISTS"}
		exists, err := RemotePathExists(context.Background(), r, "/some/path")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !exists {
			t.Fatal("expected exists=true")
		}
	})

	t.Run("path missing", func(t *testing.T) {
		r := fakeRunner{out: "VMSYNC_PATH_MISSING"}
		exists, err := RemotePathExists(context.Background(), r, "/some/path")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if exists {
			t.Fatal("expected exists=false")
		}
	})

	t.Run("runner error propagates, is not silently treated as missing", func(t *testing.T) {
		r := fakeRunner{err: errors.New("ssh connection lost")}
		exists, err := RemotePathExists(context.Background(), r, "/some/path")
		if err == nil {
			t.Fatal("expected a non-nil error when the runner itself fails")
		}
		if exists {
			t.Error("exists should be false alongside the error")
		}
	})

	t.Run("unexpected output is an error, not silently missing", func(t *testing.T) {
		r := fakeRunner{out: "garbage unexpected output"}
		exists, err := RemotePathExists(context.Background(), r, "/some/path")
		if err == nil {
			t.Fatal("expected an error for unrecognized output")
		}
		if exists {
			t.Error("exists should be false alongside the error")
		}
	})

	t.Run("marker is trimmed of surrounding whitespace", func(t *testing.T) {
		r := fakeRunner{out: "  VMSYNC_PATH_EXISTS\n"}
		exists, err := RemotePathExists(context.Background(), r, "/some/path")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !exists {
			t.Fatal("expected exists=true after trimming whitespace")
		}
	})
}
