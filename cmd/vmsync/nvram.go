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
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"path"
	"strings"

	"vmsync/pkg/libvirtsync"
	"vmsync/pkg/trace"
	"vmsync/pkg/util"
)

// Replicating a UEFI guest's varstore -- the <os><nvram> file, OVMF's
// per-domain VARS -- so the replica is a complete copy of the VM rather than
// its disks alone.
//
// WHAT IS COPIED, AND WHAT MUST NEVER BE. Two files hide behind "UEFI
// firmware" and only one of them belongs to the VM:
//
//   - <loader>, e.g. /usr/share/OVMF/OVMF_CODE_4M.fd, is read-only firmware
//     shipped by the target's own OVMF package. It belongs to the MACHINE.
//     Copying it would put one host's firmware build under another host's
//     qemu, and it is never touched here.
//   - <nvram>, e.g. /var/lib/libvirt/qemu/web01_VARS.fd, is mutable per-domain
//     state: the UEFI boot entries and the enrolled Secure Boot keys. It
//     belongs to the VM, and until now it was the one piece of a replicated
//     domain that was never replicated.
//
// What a replica lost without it: Windows registers a boot entry pointing at
// \EFI\Microsoft\Boot\bootmgfw.efi, and a freshly created VARS file has no
// such entry -- so the replica booted only if the fallback
// \EFI\BOOT\BOOTX64.EFI happened to exist. A guest with enrolled Secure Boot
// keys lost them outright. Both are discovered at failover, which is the worst
// possible moment to discover anything.

// nvramCopyAttempts is how many times a source read may be retried when the
// file changed underneath it. See syncNvram for why a retry converges.
const nvramCopyAttempts = 3

// commandRunner is the shared shape of remotessh.Client.Run and localRunner.
type commandRunner interface {
	Run(ctx context.Context, command string) (string, error)
}

// nvramMarker prefixes the one line of output that carries a payload, so it can
// be found among whatever else the far end decided to say.
//
// Both runners return stdout and stderr MERGED -- remotessh.Client.Run uses
// session.CombinedOutput, and localRunner.Run documents that it matches those
// semantics deliberately. That is fine for a command whose output is read by a
// human and fatal for one carrying base64: a single line of MOTD, an /etc/bashrc
// warning, or a login banner lands in the middle of the payload. Most such
// noise makes the decode fail loudly rather than corrupt silently, which is the
// right failure -- but failing every sync on a host that prints a banner is not
// acceptable either, and these hosts do print one.
//
// So the payload announces itself on its own line and everything else is
// ignored. `base64 -w0` emits exactly one line, which is what makes this work.
const nvramMarker = "VMSYNC-PAYLOAD:"

// markedOutput returns the payload from the one output line carrying the
// marker, or an error naming what came back instead.
func markedOutput(out, what string) (string, error) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, nvramMarker); ok {
			return after, nil
		}
	}
	// The whole output, trimmed, because on failure it is the only evidence of
	// what the far end actually did.
	trimmed := strings.TrimSpace(out)
	if len(trimmed) > 200 {
		trimmed = trimmed[:200] + "..."
	}
	return "", fmt.Errorf("%s produced no marked output; the host said: %q", what, trimmed)
}

// fileSHA256 returns the hex sha256 of a file on whichever host runner reaches,
// and ok=false when the file does not exist.
//
// Missing is not an error: on a first sync the target has no varstore yet, and
// that is the ordinary case rather than a failure.
func fileSHA256(ctx context.Context, runner commandRunner, p string) (sum string, ok bool, err error) {
	// The marker line is printed unconditionally so its ABSENCE means the
	// command did not run at all, while an empty payload after it means the
	// file is not there. Without that distinction a host whose sha256sum is
	// missing would look exactly like a host with no varstore.
	out, runErr := runner.Run(ctx,
		"printf '%s' "+util.ShQuote(nvramMarker)+"; sha256sum "+util.ShQuote(p)+" 2>/dev/null | cut -d' ' -f1; echo")
	if runErr != nil {
		return "", false, runErr
	}
	payload, err := markedOutput(out, "hashing "+p)
	if err != nil {
		return "", false, err
	}
	if payload == "" {
		return "", false, nil
	}
	if len(payload) != hex.EncodedLen(sha256.Size) {
		return "", false, fmt.Errorf("hashing %s returned %q, which is not a hex digest", p, payload)
	}
	return payload, true, nil
}

// readFileVerified reads a file over a command runner and proves it did not
// change while being read.
//
// This is the answer to "can we copy a pflash file while ensuring it is not
// written?" -- and the honest answer is that we cannot PREVENT a write without
// pausing the VM, which is far too heavy a price for a 128 KiB file on every
// sync. What we can do is make a torn copy impossible to COMMIT.
//
// qemu holds the varstore open as a pflash device for the whole life of a
// running guest, so nothing outside qemu can lock it. But UEFI variable writes
// happen essentially only when firmware is executing (boot) or when the guest
// deliberately writes one (efibootmgr, a Secure Boot key update, Windows
// servicing bootmgr). On a running guest in steady state the file is quiescent
// for weeks. So: read it, hash what was read, then ask the source for the
// file's hash NOW. Equal means nothing wrote to it across the read, and the
// bytes in hand are a faithful point-in-time copy. Unequal means a write
// landed mid-read, and the retry converges immediately because the thing that
// made it change does not happen twice in a row.
//
// Deliberately NOT done inside the freeze window vmsync already has around
// checkpoint creation. A frozen guest cannot run efibootmgr, which would make
// this even safer -- but the window stalls every guest I/O, and putting an SSH
// round trip inside it to protect a file that changes a few times a year is
// the wrong trade.
func readFileVerified(ctx context.Context, runner commandRunner, p string) (data []byte, sum string, err error) {
	for attempt := 1; attempt <= nvramCopyAttempts; attempt++ {
		// base64 rather than raw: Run trims the string it returns, which
		// would silently mangle binary content. Marked, because that same Run
		// merges stderr in -- see nvramMarker.
		out, err := runner.Run(ctx, "printf '%s' "+util.ShQuote(nvramMarker)+"; base64 -w0 "+util.ShQuote(p)+"; echo")
		if err != nil {
			return nil, "", fmt.Errorf("read %s: %w", p, err)
		}
		b64, err := markedOutput(out, "reading "+p)
		if err != nil {
			return nil, "", err
		}
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, "", fmt.Errorf("decode %s: %w", p, err)
		}
		got := sha256.Sum256(raw)
		read := hex.EncodeToString(got[:])

		now, ok, err := fileSHA256(ctx, runner, p)
		if err != nil {
			return nil, "", err
		}
		if !ok {
			return nil, "", fmt.Errorf("%s disappeared while it was being read", p)
		}
		if now == read {
			return raw, read, nil
		}
		trace.Warning("the varstore changed while it was being read; re-reading it",
			"path", p, "attempt", attempt, "of", nvramCopyAttempts)
	}
	return nil, "", fmt.Errorf("%s kept changing while being read, after %d attempts -- something is writing UEFI variables continuously, which is not a state this can copy consistently", p, nvramCopyAttempts)
}

// syncNvram copies the source domain's varstore to the target, when it has one
// and it differs from what the target already holds.
//
// Advisory throughout: every failure returns an error the CALLER logs as a
// warning rather than failing the run. The replica's disks are already
// committed and correct by the time this runs, and refusing a sync that
// produced a good replica because a boot-variable file could not be copied
// would be a much worse trade than a replica whose boot entries are one
// interval stale. What it must never do is fail SILENTLY, so every path here
// either copies or says why not.
func syncNvram(ctx context.Context, srcRunner commandRunner, tgt *remoteFileWriter, srcXML, sourceDomain, targetDomain string) error {
	nvram, err := libvirtsync.DetectNvram(srcXML)
	if err != nil {
		return err
	}
	if nvram == "" {
		// Not a UEFI guest, or a firmware setup with no per-domain varstore.
		// Nothing to do, and nothing worth logging on every sync.
		return nil
	}

	// Where it goes on the TARGET, which is not necessarily where it sits on
	// the source. DefineDomain has just repathed this domain's <os><nvram> for
	// the target -- libvirt names a varstore after the domain, so a replica
	// under a different name gets its own file rather than one named for the
	// source. The same function decides it here, so the file lands exactly
	// where the definition says it will. Computing it twice would be two
	// chances to disagree; using one function is what makes them the same
	// answer by construction.
	//
	// Identical to nvram whenever the names match, which is the default.
	dst := libvirtsync.TargetNvramPath(nvram, sourceDomain, targetDomain)
	if dst != nvram {
		trace.Info("the replica's UEFI varstore is repathed for its own domain name, so it cannot collide with the source's",
			"source_domain", sourceDomain, "target_domain", targetDomain, "source_path", nvram, "target_path", dst)
	} else if sourceDomain != targetDomain && !strings.Contains(path.Base(nvram), sourceDomain) {
		// A varstore whose filename is not domain-derived: nothing to correct,
		// and guessing would override a deliberate choice. Worth one line,
		// because it is the one shape where two differently-named domains can
		// still end up sharing a file.
		trace.Warning("this domain's UEFI varstore filename is not derived from its domain name, so it is used as-is for the replica. If the target host runs another domain pointing at the same file, they share one varstore and overwrite each other's boot entries and Secure Boot keys",
			"source_domain", sourceDomain, "target_domain", targetDomain, "path", nvram)
	}

	srcSum, ok, err := fileSHA256(ctx, srcRunner, nvram)
	if err != nil {
		return fmt.Errorf("hash the source varstore %s: %w", nvram, err)
	}
	if !ok {
		// The domain XML names a varstore the source host does not have.
		// libvirt would create it on next boot from the template, so this is
		// a real state rather than an impossible one -- and there is nothing
		// to copy.
		return fmt.Errorf("the source domain's XML names a varstore at %s but no such file exists on the source host", nvram)
	}

	// The cheapest possible skip, and the reason no metadata field is needed
	// to track what was copied last time: ask the target what it holds. A
	// varstore changes a few times in a VM's life, so on almost every sync
	// these match and the whole operation costs two sha256sums of a 128 KiB
	// file. Comparing content rather than recording state also self-corrects:
	// if somebody replaces the target's varstore by hand, the next sync puts
	// it back rather than believing a stale record.
	tgtSum, tgtExists, err := fileSHA256(ctx, tgt.runner, dst)
	if err != nil {
		return fmt.Errorf("hash the target varstore %s: %w", dst, err)
	}
	if tgtExists && tgtSum == srcSum {
		trace.Debug("varstore already matches the source; nothing to copy", "path", dst)
		return nil
	}

	data, sum, err := readFileVerified(ctx, srcRunner, nvram)
	if err != nil {
		return err
	}

	if err := tgt.write(ctx, dst, data, sum); err != nil {
		return err
	}

	what := "updated the replica's UEFI varstore"
	if !tgtExists {
		what = "copied the source's UEFI varstore to the replica for the first time"
	}
	trace.Info(what+"; its boot entries and any enrolled Secure Boot keys now match the source",
		"vm", targetDomain, "path", dst, "bytes", len(data), "sha256", sum)
	return nil
}

// remoteFileWriter installs a file on the target host, atomically.
type remoteFileWriter struct {
	runner commandRunner
	// input writes bytes to a command's stdin. Separate from runner because
	// only the ssh client can do it; a local target uses the same shape.
	input func(ctx context.Context, command string, data []byte) error
	// owner is what the installed file is chowned to, e.g. "qemu:qemu". Empty
	// leaves ownership alone.
	owner string
}

// write installs data at dst, verifying the bytes that landed.
//
// Temp-then-rename, never a direct write: the target's varstore is a file
// libvirt hands to qemu at boot, and a half-written one is worse than a stale
// one. A stale varstore boots the VM with last week's boot entries; a truncated
// one can hang the firmware. rename(2) within a directory is atomic, so the
// file at dst is only ever a complete varstore -- the old one or the new one.
//
// Verified before the rename rather than after, so a transfer that arrived
// corrupt is discarded while it is still a temp file nothing refers to.
func (w *remoteFileWriter) write(ctx context.Context, dst string, data []byte, wantSum string) error {
	tmp := dst + ".vmsync-tmp"

	if err := w.input(ctx, "cat > "+util.ShQuote(tmp), data); err != nil {
		return fmt.Errorf("write %s on the target: %w", tmp, err)
	}

	got, ok, err := fileSHA256(ctx, w.runner, tmp)
	if err != nil {
		return fmt.Errorf("hash the varstore written to the target: %w", err)
	}
	if !ok {
		return fmt.Errorf("wrote %s on the target but it is not there", tmp)
	}
	if got != wantSum {
		// Left in place deliberately is the wrong instinct here: a corrupt
		// temp file next to a live varstore is a trap for the next person to
		// look at that directory, and it has nothing to tell us that the
		// hashes have not already said.
		_, _ = w.runner.Run(ctx, "rm -f "+util.ShQuote(tmp))
		return fmt.Errorf("the varstore arrived on the target corrupted: sent sha256 %s, target holds %s", wantSum, got)
	}

	// Ownership BEFORE the rename, so the file is never in place with the
	// wrong owner -- qemu runs as an unprivileged user and a varstore it
	// cannot open stops the domain from starting at all. Same reasoning, and
	// the same owner, as the replica's disks.
	if w.owner != "" {
		if _, err := w.runner.Run(ctx, "chown "+util.ShQuote(w.owner)+" "+util.ShQuote(tmp)); err != nil {
			_, _ = w.runner.Run(ctx, "rm -f "+util.ShQuote(tmp))
			return fmt.Errorf("set owner %s on the target varstore: %w", w.owner, err)
		}
	}
	// 0600: it holds Secure Boot keys and boot configuration, and libvirt
	// creates it this way itself.
	if _, err := w.runner.Run(ctx, "chmod 0600 "+util.ShQuote(tmp)); err != nil {
		_, _ = w.runner.Run(ctx, "rm -f "+util.ShQuote(tmp))
		return fmt.Errorf("set mode on the target varstore: %w", err)
	}

	if _, err := w.runner.Run(ctx, "mv -f "+util.ShQuote(tmp)+" "+util.ShQuote(dst)); err != nil {
		_, _ = w.runner.Run(ctx, "rm -f "+util.ShQuote(tmp))
		return fmt.Errorf("install the varstore at %s: %w", dst, err)
	}
	return nil
}
