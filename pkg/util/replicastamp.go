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

package util

import (
	"sort"
	"strconv"
	"strings"
)

// Helpers for the replica_written_at metadata field: the record of when
// vmsync itself last wrote each replica disk, measured on the TARGET host's
// own clock.
//
// It exists because the out-of-band-modification check compares a replica
// disk's mtime against the last recorded sync, and that record is only
// written when a whole run succeeds. A run that copied the disks and then
// failed -- a failed -verify being the common case -- left the mtime
// advanced and the record stale, so every later run refused, blaming a
// writer that did not exist. These render and parse the value; where it is
// written and read is cmd/vmsync's business.

// statMTimeFieldSep separates the name from the mtime in the stat output
// these helpers agree on. A space, because a path may contain one and an
// mtime may not -- which is what lets ParseStatMTimes split on the LAST one
// and round-trip a path with spaces in it.
const statMTimeFieldSep = " "

// StatMTimesCommand builds the shell command that reads several files'
// modification times in ONE round trip.
//
// Tolerant by construction: a path that does not exist is simply absent from
// the output rather than failing the command. That matters on the path this
// is used for -- a multi-disk run where one disk died before its target file
// was created still wrote the others, and losing every stamp because one
// file is missing would leave exactly the pairs this is meant to unwedge.
// Same shape as StatOwnerCommand above, for the same reason.
//
// Deliberately NOT -L. The check this feeds stats the link itself, so this
// has to as well, or a symlinked replica would compare two different files'
// times and refuse forever.
//
// Returns "" for no paths, so a caller can skip the round trip entirely.
func StatMTimesCommand(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	quoted := make([]string, 0, len(paths))
	for _, p := range paths {
		quoted = append(quoted, ShQuote(p))
	}
	// `--` so a path beginning with a dash is an operand, not a flag.
	// `exit 0` rather than `|| true` because stat exits non-zero when ANY
	// operand is missing, even though it still printed the ones that were
	// not -- `|| true` would keep that output too, but this says plainly
	// that a partial result is the intended outcome.
	return "stat -c '%n" + statMTimeFieldSep + "%Y' -- " + strings.Join(quoted, " ") + " 2>/dev/null; exit 0"
}

// ParseStatMTimes reads StatMTimesCommand's output into path -> unix seconds.
//
// Splits each line on its LAST space: everything before is the path, which
// may itself contain spaces, and what follows is the mtime, which cannot.
//
// Lenient throughout -- an unreadable line is skipped, never an error. The
// caller's fallback for a missing entry is well defined (the older, less
// precise timestamp), so a garbled line costs precision, not correctness,
// and failing the whole parse would cost every disk's stamp for one bad one.
func ParseStatMTimes(out string) map[string]int64 {
	result := map[string]int64{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		i := strings.LastIndex(line, statMTimeFieldSep)
		if i <= 0 {
			continue
		}
		path, raw := line[:i], line[i+1:]
		mtime, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil {
			continue
		}
		result[path] = mtime
	}
	return result
}

// validStampDev reports whether a disk's target dev can be used as a key.
//
// The rendered value is a flat "dev=unix,dev=unix" string in domain XML, so
// a dev containing "," or "=" would produce something its own parser could
// not read back. Every real libvirt target dev (vda, sda, hdc, ...) passes;
// this is a guard against a nonsense one, not a filter anybody should hit.
func validStampDev(dev string) bool {
	if dev == "" {
		return false
	}
	for _, r := range dev {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_', r == '.', r == '-':
		default:
			return false
		}
	}
	return true
}

// FormatReplicaWrittenAt renders per-disk write times for storage in domain
// metadata: "vda=1756000000,vdb=1756000005".
//
// Keys are sorted so the same set of disks always renders identically --
// this lands in a domain's XML, which people read and diff, and Go's map
// iteration order would otherwise churn it on every run for no reason.
//
// A dev that cannot be a key is DROPPED rather than emitted: a value its own
// parser chokes on would take the other disks' stamps down with it.
// Returns "" for nothing worth writing, which callers treat as "no stamp".
func FormatReplicaWrittenAt(byDev map[string]int64) string {
	devs := make([]string, 0, len(byDev))
	for dev := range byDev {
		if validStampDev(dev) {
			devs = append(devs, dev)
		}
	}
	if len(devs) == 0 {
		return ""
	}
	sort.Strings(devs)
	parts := make([]string, 0, len(devs))
	for _, dev := range devs {
		parts = append(parts, dev+"="+strconv.FormatInt(byDev[dev], 10))
	}
	return strings.Join(parts, ",")
}

// ParseReplicaWrittenAt is FormatReplicaWrittenAt's lenient inverse.
//
// Never returns an error. A malformed entry means "no stamp for that disk",
// and the caller already has a defined fallback for that; refusing the whole
// value would turn one bad entry into a refused sync, which is the failure
// this whole field exists to remove.
func ParseReplicaWrittenAt(v string) map[string]int64 {
	result := map[string]int64{}
	for _, entry := range strings.Split(v, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		dev, raw, ok := strings.Cut(entry, "=")
		if !ok || dev == "" {
			continue
		}
		mtime, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil {
			continue
		}
		result[dev] = mtime
	}
	return result
}
