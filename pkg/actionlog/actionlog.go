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

// Package actionlog is vmsync's action journal: an append-only JSONL record of
// what each invocation SET OUT to do, written before it acts, and of what came
// of it, written when it stops.
//
// WHY IT EXISTS. A run that dies mid-flight -- SIGTERM (whose handler calls
// os.Exit, so nothing deferred runs), a dropped link, a lost power feed --
// leaves no trace of itself anywhere. The domain metadata describes the replica
// the run was part-way through REPLACING, the log is on whichever machine
// launched it, and the disks say nothing. The intent record is the one thing
// that survives beside the data: an intent with no outcome IS the evidence that
// a run started here and never came back, and it is the only artefact that
// names which run, at what time, under which correlation id.
//
// NOTHING EVER BRANCHES ON THIS JOURNAL, and that rule is the whole reason it
// can be trusted. It is evidence, never an input to a decision. No refusal, no
// gate and no plan may read it -- a diagnostic that decides is a diagnostic
// that has to be correct, and this one is written best-effort on a remote
// filesystem that may be full, read-only or gone. The refusal that closes
// CI-02 rests on the domain's own replica_incomplete metadata field, which is
// readable on the DR host with the source host unreachable; this only explains
// it afterwards. TestNothingThatDecidesImportsThisPackage is the mechanical
// expression of that rule.
//
// Deliberately free of any libvirt import, and of pkg/util -- the first would
// need libvirt headers to compile, the second needs syscall.Flock, and either
// would mean the journal's format, its bounds and every command it sends to a
// production target could only be tested on a Linux box with a hypervisor
// toolchain. pkg/restorepoint and pkg/failover are the same shape for the same
// reason. The libvirt- and SSH-facing code is a thin shell in cmd/vmsync whose
// only job is to run what this package builds.
package actionlog

import (
	"path"
	"strconv"
	"strings"
)

// DirName is the directory, beside the disks it describes, that holds a host's
// journals.
//
// Beside the disks and not in /var/log, because the disks are what the journal
// is about and they are what an operator has in front of them during an
// incident. A replica moved to another host, or examined from a rescue system,
// carries its own history with it; a log on the machine that launched the sync
// does not survive that machine being the thing that died.
//
// A dotted subdirectory, mirroring restorepoint.DirName, for one hard reason
// beyond tidiness: cmd/vmsync's promotion check globs <disk>_* beside the
// replica to find an uncommitted overlay, and a match BLOCKS promotion. A file
// named next to a disk risks making the replica unpromotable.
const DirName = ".vmsync-journal"

// Ext is the journal file extension. JSON Lines: one self-contained record per
// line, so a reader needs no state, a torn final line costs exactly one record,
// and appending needs no rewrite of what is already there.
const Ext = ".jsonl"

// Rotation bounds. One live file, ONE rotated generation, nothing else.
//
// A journal that grows without limit on a DR target is a journal that
// eventually fills the filesystem holding the replicas -- which would turn a
// diagnostic aid into an outage. Two generations is the smallest thing that
// still survives a rotation happening five minutes before somebody comes
// looking.
const (
	// RotateAtBytes is the size the live file is moved aside at.
	RotateAtBytes = 4 << 20
	// Generations is how many rotated files are kept. Exactly one.
	Generations = 1
	// MaxPerDomainBytes is the consequence of the two above, stated so it can
	// be reasoned about (and tested) rather than recomputed by each reader.
	MaxPerDomainBytes = RotateAtBytes * (Generations + 1)
)

// Root is the journal directory for a set of disks, named by any one of them.
//
// Mirrors restorepoint.Root exactly, including taking a DISK path rather than a
// directory: every caller already has a disk path in hand, and the two
// functions being the same shape is what stops the journal and the restore
// points ending up in different places for the same replica.
func Root(diskPath string) string {
	return path.Join(path.Dir(diskPath), DirName)
}

// RootOfDir is Root for a caller that has the directory rather than a disk.
//
// Kept separate rather than letting callers guess: Root("/data/replicas")
// would silently answer "/data/.vmsync-journal", one directory too high, and
// the journal would be written beside the wrong machine's disks.
func RootOfDir(dir string) string {
	return path.Join(dir, DirName)
}

// File is the live journal for one domain inside root.
//
// One file per domain, not one per host: a domain's history has to travel with
// its disks, and a shared file would mean a replica moved elsewhere either
// loses its history or drags every other domain's along.
func File(root, domain string) string {
	return path.Join(root, SafeKey(domain)+Ext)
}

// RotatedFile is where File is moved when it reaches RotateAtBytes.
//
// "<name>.1.jsonl" rather than "<name>.jsonl.1" so the rotated generation is
// still recognisably JSON Lines to everything that sorts by extension --
// including the operator reaching for it with a glob during an incident.
func RotatedFile(file string) string {
	return strings.TrimSuffix(file, Ext) + ".1" + Ext
}

// RotateDecision reports whether a file of this size must be moved aside
// before the next record is appended.
//
// sizeBytes is what the file would become -- its current size plus the record
// about to be written -- so the live file never exceeds RotateAtBytes at all,
// rather than exceeding it by one record. Both writers (the local os path and
// the remote shell command) compute it from this one rule; two rules would
// eventually disagree and leave a file nothing ever rotates.
func RotateDecision(sizeBytes int64) bool {
	return sizeBytes >= RotateAtBytes
}

// safeKeyReplacer is util.SafeKey's encoding, duplicated rather than imported.
//
// pkg/util pulls in syscall.Flock and libvirtxml, which would make this package
// build only on a Linux hypervisor host and take its tests down with it --
// being testable anywhere is the entire reason this package exists.
// pkg/restorepoint duplicates util.ShQuote for the same reason and says so in
// the same words.
//
// The duplication is only safe if it behaves identically, so TestSafeKey pins
// it against the same cases pkg/util's own test uses. If util.SafeKey ever
// changes, a domain's journal would move to a different filename and its
// history would appear to have vanished -- which is why the two are pinned
// rather than merely "kept in step".
var safeKeyReplacer = strings.NewReplacer("%", "%%", "/", "%2f", " ", "%20")

// SafeKey makes a domain name safe as ONE filename component, reversibly.
//
// Reversible percent-style encoding rather than lossy substitution, because
// mapping both "/" and " " to "_" would make "web server" and "web/server"
// share a journal -- two different machines' histories interleaved in one file,
// which is worse than either having none.
//
// What this does NOT do is make a name safe for a SHELL. A domain may legally
// contain a quote, a newline or a "$(", and this leaves all three alone; every
// path built from it is shell-quoted at the point it enters a command (see
// AppendCommand). Confusing the two jobs is how a domain name becomes a command
// on a production target.
func SafeKey(key string) string { return safeKeyReplacer.Replace(key) }

// ShQuote is util.ShQuote, duplicated for the reason given on safeKeyReplacer.
// Pinned by TestShQuote against the same cases pkg/restorepoint pins its own
// copy with.
func ShQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// AppendCommand builds the shell command that appends one record, read from
// STDIN, to a domain's journal on the host that owns its disks.
//
// STDIN AND NEVER ARGV. A record can be two kilobytes of arbitrary text --
// error messages, paths, a control plane's operation id -- and putting it in
// the command line would mean quoting it correctly for every shell that might
// run it, and living inside ARG_MAX. The payload crossing as data means the
// command is a fixed shape, independent of what the record says, and a record
// containing a quote or a newline cannot become a command. Everything that IS
// interpolated here is a path, and every one of them is ShQuote'd.
//
// payloadLen is the length of that record, used only to decide rotation: the
// file is moved aside when this record would take it past RotateAtBytes, which
// is the same rule RotateDecision applies locally.
//
// It reports through its exit status rather than a marker on stdout, following
// pkg/restorepoint's convention for a command that DOES something: for an
// action there is no useful difference between "it failed" and "we could not
// tell". The caller counts the failure and carries on regardless -- a journal
// write must never fail the action it describes.
//
// `cat >> file` rather than a here-document or a shell append loop: cat issues
// ONE write() for an input this small, and a single write of at most 2 KiB to
// an O_APPEND descriptor is atomic on Linux, so two vmsync processes journaling
// the same domain at once interleave whole records and never half-lines. A
// record split down the middle would cost the reader the intent<->outcome join
// for both of them.
func AppendCommand(root, file string, payloadLen int) string {
	qRoot := ShQuote(root)
	qFile := ShQuote(file)
	qRotated := ShQuote(RotatedFile(file))

	// The size at which this record tips the file over the limit. Clamped at
	// zero so a pathological payload cannot produce a negative threshold, which
	// would compare as "always rotate" on some shells and "never" on others.
	threshold := int64(RotateAtBytes) - int64(payloadLen)
	if threshold < 0 {
		threshold = 0
	}

	// wc's answer goes through an unquoted ${sz:-0} deliberately: some wc
	// implementations pad the count with leading spaces, and word splitting is
	// what strips them. The :- default covers a wc that printed nothing at all
	// (no such file yet), which is the ordinary case on a first run.
	return "mkdir -p " + qRoot + " || exit 1; " +
		"sz=$(wc -c < " + qFile + " 2>/dev/null) || sz=0; " +
		"if [ ${sz:-0} -ge " + strconv.FormatInt(threshold, 10) + " ]; then mv -f " + qFile + " " + qRotated + " 2>/dev/null; fi; " +
		"cat >> " + qFile
}
