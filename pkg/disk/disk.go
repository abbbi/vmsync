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
	"errors"
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
	// RootSource is Source's qcow2 backing chain resolved down to its base
	// file (no further backing file), or equal to Source itself when there
	// is no backing chain. A multi-element chain has two, unrelated possible
	// causes that this resolution deliberately treats the same way: an
	// external snapshot (e.g. virsh snapshot-create --disk-only redirects
	// the domain to a new overlay named after the snapshot, on top of the
	// disk's original file) or a permanent qcow2 linked clone (the domain's
	// disk has always had a backing file, e.g. a template shared by many
	// VMs, unrelated to snapshots). Either way, Source reflects the
	// domain's *currently active* file, and Target-side path naming must
	// use RootSource, not Source, or every target-path lookup breaks
	// against the real target file -- which was created under this same
	// resolved name during an earlier sync, and stays stable across
	// further snapshots since the chain's base file itself doesn't change
	// as more overlays are stacked on top of it.
	RootSource string
}

// Path is where this disk's file actually is: the backing-chain root when it
// has been resolved, and the domain's own <source file> otherwise.
//
// It exists because RootSource is a TRAP. ParseQcowDisks does not populate it
// -- resolving a backing chain means running qemu-img per disk, which the
// inventory scan explicitly refuses to pay on every report -- so it is empty
// for everyone except the sync path, which fills it in itself after doing that
// work. A caller that parses an XML and reads RootSource gets "", and nothing
// about the field says so.
//
// That has already cost one silent bug: cmd/vmsync's inversion warning about
// reversed -target-disk-path values compared path.Dir("") against path.Dir("")
// -- "." on both ends, always equal -- so the warning could never fire, on any
// pair, and the hazard it exists to flag went unmentioned.
//
// Use this instead of reading either field directly, unless you specifically
// need the currently-active overlay (Source) or specifically know the chain
// has been resolved.
func (d QcowDisk) Path() string {
	if d.RootSource != "" {
		return d.RootSource
	}
	return d.Source
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
			// The domain name, because this is not only called per-sync:
			// inventory.Scan runs it for EVERY domain on the host on every
			// report cycle, so without it an agent emits a stream of
			// "skipping vda" with nothing saying which of forty VMs it means.
			//
			// The reason too. "incompatible" covers two quite different
			// things -- a cdrom, which is expected and uninteresting, and a
			// disk whose driver is not qcow2, which is a VM somebody may
			// believe is being replicated and is not. Reading the same line
			// for both is what makes the second one easy to miss.
			format := "none"
			if d.Driver != nil && d.Driver.Type != "" {
				format = d.Driver.Type
			}
			trace.Info("skipping incompatible device, it will not be replicated",
				"vm", domcfg.Name, "device", targetDev, "device_type", d.Device, "format", format)
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

func shellEscape(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// QemuImgInfoChainJSON resolves path's full qcow2 backing chain in one call
// via --backing-chain, returned top (path itself) to base, in that order. A
// path with no backing file returns a single element. Used both for a
// disk's regular size/format info (chain[0]) and to find its stable base
// file (chain's last element) -- either because an external snapshot has
// redirected the domain to a new overlay, or because the disk is a
// permanent qcow2 linked clone of a shared base image -- see
// QcowDisk.RootSource.
func QemuImgInfoChainJSON(path string) ([]QemuImgInfo, error) {
	cmd := exec.Command("qemu-img", "info", "--force-share", "--backing-chain", "--output=json", path)
	b, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("qemu-img info --backing-chain for %s: %w", path, err)
	}

	var chain []QemuImgInfo
	if err := json.Unmarshal(b, &chain); err != nil {
		return nil, fmt.Errorf("parse qemu-img backing-chain json for %s: %w", path, err)
	}
	if len(chain) == 0 {
		return nil, fmt.Errorf("qemu-img info --backing-chain for %s returned no images", path)
	}
	return chain, nil
}

// QemuImgInfoChainJSONRemote is QemuImgInfoChainJSON run over runner instead
// of the local host -- see QemuImgInfoChainJSON.
func QemuImgInfoChainJSONRemote(ctx context.Context, runner CommandRunner, path string) ([]QemuImgInfo, error) {
	if runner == nil {
		return nil, fmt.Errorf("remote command runner is nil")
	}

	cmd := fmt.Sprintf("qemu-img info --force-share --backing-chain --output=json %s", shellEscape(path))
	out, err := runner.Run(ctx, cmd)
	if err != nil {
		return nil, fmt.Errorf("remote qemu-img info --backing-chain for %s: %w", path, err)
	}

	var chain []QemuImgInfo
	if err := json.Unmarshal([]byte(out), &chain); err != nil {
		return nil, fmt.Errorf("parse remote qemu-img backing-chain json for %s: %w", path, err)
	}
	if len(chain) == 0 {
		return nil, fmt.Errorf("qemu-img info --backing-chain for %s returned no images", path)
	}
	return chain, nil
}

// ResolveRootSource walks chain -- as returned by QemuImgInfoChainJSON or
// QemuImgInfoChainJSONRemote, ordered top (the disk's current, active file)
// to base -- down to its last element, the stable base file a backing chain
// eventually bottoms out at, and returns its Filename. This is exactly
// QcowDisk.RootSource's own value: see that field's doc comment for why
// target-side paths must be named after this base rather than source (the
// disk's current, active file), and why a chain longer than one element
// doesn't by itself distinguish an external snapshot from a permanent qcow2
// linked clone -- both look identical from here.
//
// Falls back to source, unchanged, if chain is empty -- defensive only: the
// real callers above always get a non-empty chain or a non-nil error from
// qemu-img itself (see QemuImgInfoChainJSON's own doc comment), so this
// should never actually trigger, but avoids an index-out-of-range panic
// rather than assume that invariant holds for every future caller too.
func ResolveRootSource(chain []QemuImgInfo, source string) string {
	if len(chain) == 0 {
		return source
	}
	return chain[len(chain)-1].Filename
}

// CompareImages runs qemu-img compare between refA and refB -- each any
// qemu-img-recognized image reference (a local file path, or a network URL
// such as "nbd://host:port/export"). It runs as a local subprocess wherever
// this process itself executes; neither ref needs to be a local file on
// this host, since qemu-img's own block layer dials network references
// itself. Returns nil when the two are byte-for-byte identical; otherwise a
// descriptive error, with a mismatch (qemu-img's own documented exit code
// 1, "images differ") distinguished from a genuine failure to even perform
// the comparison (any other non-zero exit).
//
// -U opens the image in shared mode: the caller's own use case includes
// comparing a source disk that may still be open by a suspended (not
// destroyed) qemu process, which would otherwise refuse a second,
// exclusive-by-default open. Harmless (a no-op) for a network reference.
func CompareImages(refA, refB string) error {
	cmd := exec.Command("qemu-img", "compare", "-U", refA, refB)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return fmt.Errorf("images differ: %s", strings.TrimSpace(string(out)))
	}
	return fmt.Errorf("qemu-img compare %s vs %s: %w: %s", refA, refB, err, strings.TrimSpace(string(out)))
}
