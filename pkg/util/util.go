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

func HostFromURIOrLocal(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "127.0.0.1"
	}
	return u.Hostname()
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

// SetTargetPath computes the on-disk path a disk is synced to on the target
// host. When targetDiskPath is set, the basename alone is not enough to keep
// two disks apart -- storage layouts that give every VM's disk the same
// filename inside a per-VM directory (e.g. OpenStack Nova's
// instances/<uuid>/disk) or qcow2 linked clones sharing a template's
// basename would otherwise collide on the identical target file. vmName and
// targetDev (the domain name and its <target dev='vda'/>) are prefixed onto
// the basename to keep every disk of every domain unique under a shared
// targetDiskPath.
func SetTargetPath(targetDiskPath, vmName, targetDev, diskPath string) string {
	if targetDiskPath == "" {
		return diskPath
	}
	base := fmt.Sprintf("%s-%s-%s", vmName, targetDev, filepath.Base(diskPath))
	return filepath.Join(targetDiskPath, base)
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
