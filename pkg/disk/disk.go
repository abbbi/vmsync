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

package disk

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"vmsync/pkg/trace"
	"vmsync/pkg/util"

	"libvirt.org/go/libvirtxml"
)

type QcowDisk struct {
	TargetDev   string
	Source      string
	Format      string
	DiscardMode string
	ClusterSize int64
	VirtualSize int64
}

type QemuImgInfo struct {
	Filename    string                 `json:"filename"`
	Format      string                 `json:"format"`
	VirtualSize int64                  `json:"virtual-size"`
	ClusterSize int64                  `json:"cluster-size"`
	FormatSpec  map[string]interface{} `json:"format-specific"`
}

type CommandRunner interface {
	Run(ctx context.Context, command string) (string, error)
}

func ParseQcowDisks(domainXML string) ([]QcowDisk, error) {
	domcfg := &libvirtxml.Domain{}
	err := domcfg.Unmarshal(domainXML)
	if err != nil {
		return nil, err
	}

	var out []QcowDisk
	for _, d := range domcfg.Devices.Disks {
		// Target/Source are pointers and can be nil for malformed or
		// unusual disk entries; don't dereference them blindly.
		targetDev := ""
		if d.Target != nil {
			targetDev = d.Target.Dev
		}

		if util.IgnoreDevice(d) == true {
			trace.Info("skipping incompatible", "device", targetDev)
			continue
		}

		source := ""
		if d.Source != nil && d.Source.File != nil {
			source = d.Source.File.File
		}

		out = append(out, QcowDisk{
			TargetDev:   targetDev,
			Source:      source,
			Format:      d.Driver.Type,
			DiscardMode: d.Driver.Discard,
		})
	}

	return out, nil
}

func QemuImgInfoJSON(path string) (QemuImgInfo, error) {
	cmd := exec.Command("qemu-img", "info", "--force-share", "--output=json", path)
	b, err := cmd.Output()
	if err != nil {
		return QemuImgInfo{}, fmt.Errorf("qemu-img info for %s: %w", path, err)
	}

	var info QemuImgInfo
	if err := json.Unmarshal(b, &info); err != nil {
		return QemuImgInfo{}, fmt.Errorf("parse qemu-img json for %s: %w", path, err)
	}
	return info, nil
}

func QemuImgInfoJSONRemote(ctx context.Context, runner CommandRunner, path string) (QemuImgInfo, error) {
	if runner == nil {
		return QemuImgInfo{}, fmt.Errorf("remote command runner is nil")
	}

	cmd := fmt.Sprintf("qemu-img info --force-share --output=json %s", shellEscape(path))
	out, err := runner.Run(ctx, cmd)
	if err != nil {
		return QemuImgInfo{}, fmt.Errorf("remote qemu-img info for %s: %w", path, err)
	}

	var info QemuImgInfo
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		return QemuImgInfo{}, fmt.Errorf("parse remote qemu-img json for %s: %w", path, err)
	}
	return info, nil
}

func shellEscape(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
