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
	"net"
	"net/url"
	"path/filepath"
	"strings"
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

func RemotePathExists(ctx context.Context, runner interface {
	Run(context.Context, string) (string, error)
}, p string) (bool, error) {
	_, err := runner.Run(ctx, "stat "+ShQuote(p))
	if err != nil {
		return false, nil
	}
	return true, nil
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
