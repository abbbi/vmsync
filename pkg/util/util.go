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
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"libvirt.org/go/libvirtxml"
)

func UriUsesSSH(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(u.Scheme), "ssh")
}

// HostFromURIOrLocal answers "what address do I CONNECT to for this URI",
// falling back to loopback when the URI names no host because a URI with no
// authority means this machine.
//
// For connectivity only. It is the wrong function for recording who a
// replica belongs to -- see ReplicaHost, and the warning there about what
// happens when the two are confused.
func HostFromURIOrLocal(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "127.0.0.1"
	}
	return u.Hostname()
}

// ReplicaHost answers a different question from HostFromURIOrLocal: "what
// do I call this host when writing it down for someone else to read".
//
// The distinction is not academic. replica_source and replica_targets are
// read by other hosts and by the control plane, which correlates a pair by
// matching "<host>:<domain>" against the hostname an agent reports. A
// connectivity answer of "127.0.0.1" is correct for opening a socket and
// useless as an identity: written into metadata it names every machine and
// therefore none, so every agent-driven pair fails to correlate, and
// anything reasoning about which source a promotion displaced gets a
// constant instead of a name.
//
// localName lets a caller supply the name the rest of the system knows this
// host by -- an agent reports under a configurable hostname, and metadata
// that disagreed with it would break the same correlation a different way.
// Empty falls back to the system hostname.
//
// The loopback literal survives only as a last resort, so a host whose name
// cannot be determined at all still writes something rather than an empty
// reference.
func ReplicaHost(rawURI, localName string) string {
	if u, err := url.Parse(rawURI); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	if localName != "" {
		return localName
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "127.0.0.1"
}

func ConnectHostFromBindOrURI(bind, rawURI string) string {
	if ip := net.ParseIP(bind); ip != nil && !ip.IsUnspecified() {
		return bind
	}
	return HostFromURIOrLocal(rawURI)
}

func ShQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// RemotePathExists reports whether p exists on the remote host. The check
// runs as a POSIX shell if/else (`[ -e ]`) rather than relying on `stat`'s
// own exit code, specifically so a successful SSH round-trip always exits 0
// regardless of which branch it took -- that makes a non-nil error from
// Run mean exclusively "the check itself couldn't be performed" (a
// transient SSH failure, a permission problem, a wedged connection,
// whatever), which must never be silently treated as "the path doesn't
// exist": doing so previously undermined every safety check built on this
// function, most importantly full sync's own guard against overwriting an
// already-existing target disk.
func RemotePathExists(ctx context.Context, runner interface {
	Run(context.Context, string) (string, error)
}, p string) (bool, error) {
	const existsMarker = "VMSYNC_PATH_EXISTS"
	const missingMarker = "VMSYNC_PATH_MISSING"
	cmd := fmt.Sprintf("if [ -e %s ]; then echo %s; else echo %s; fi", ShQuote(p), existsMarker, missingMarker)
	out, err := runner.Run(ctx, cmd)
	if err != nil {
		return false, fmt.Errorf("check remote path %s: %w", p, err)
	}
	switch strings.TrimSpace(out) {
	case existsMarker:
		return true, nil
	case missingMarker:
		return false, nil
	default:
		return false, fmt.Errorf("check remote path %s: unexpected output %q", p, out)
	}
}

func SetTargetPath(targetDiskPath string, diskPath string) string {
	var targetPath string
	if targetDiskPath != "" {
		targetPath = filepath.Join(targetDiskPath, filepath.Base(diskPath))
	} else {
		targetPath = diskPath
	}

	return targetPath
}

func IgnoreDevice(d libvirtxml.DomainDisk) bool {
	if d.Device == "cdrom" {
		return true
	}
	// Driver is a pointer and nil whenever <disk> has no <driver> element
	// (allowed by the schema); a disk with no declared driver can't be
	// confirmed qcow2, so treat it the same as any other non-qcow2 disk.
	if d.Driver == nil || d.Driver.Type != "qcow2" {
		return true
	}

	return false
}
