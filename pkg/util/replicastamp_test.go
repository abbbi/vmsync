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
	"reflect"
	"strings"
	"testing"
)

func TestStatMTimesCommand(t *testing.T) {
	t.Run("no paths means no round trip", func(t *testing.T) {
		if got := StatMTimesCommand(nil); got != "" {
			t.Errorf("StatMTimesCommand(nil) = %q, want empty so the caller can skip the ssh call", got)
		}
	})

	t.Run("tolerates a missing file", func(t *testing.T) {
		cmd := StatMTimesCommand([]string{"/a.qcow2", "/b.qcow2"})
		// The whole point: stat exits non-zero when ANY operand is missing,
		// while still printing the ones that were not. Without this the
		// stamps of every disk that WAS written are lost because one was not
		// -- which is the multi-disk failure this field exists to handle.
		if !strings.Contains(cmd, "exit 0") {
			t.Errorf("command does not tolerate a missing path: %q", cmd)
		}
		if !strings.Contains(cmd, "2>/dev/null") {
			t.Errorf("command does not silence stat's per-file errors: %q", cmd)
		}
	})

	t.Run("quotes and terminates options", func(t *testing.T) {
		cmd := StatMTimesCommand([]string{"/data/a b.qcow2", "-weird.qcow2"})
		if !strings.Contains(cmd, "--") {
			t.Errorf("no -- terminator, so a leading-dash path is read as a flag: %q", cmd)
		}
		if !strings.Contains(cmd, ShQuote("/data/a b.qcow2")) {
			t.Errorf("path with a space is not quoted: %q", cmd)
		}
	})

	t.Run("stats the link, not its target", func(t *testing.T) {
		// -L would follow a symlink, and the preflight this feeds stats the
		// link itself. Two different files' times compared against each
		// other would refuse forever.
		if cmd := StatMTimesCommand([]string{"/a.qcow2"}); strings.Contains(cmd, " -L") {
			t.Errorf("command dereferences symlinks; the check it feeds does not: %q", cmd)
		}
	})
}

func TestParseStatMTimes(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		want map[string]int64
	}{
		{"empty", "", map[string]int64{}},
		{
			"one line per file",
			"/data/a.qcow2 1756000000\n/data/b.qcow2 1756000005\n",
			map[string]int64{"/data/a.qcow2": 1756000000, "/data/b.qcow2": 1756000005},
		},
		{
			// The reason for splitting on the LAST space rather than the
			// first: a replica path may legitimately contain spaces, an
			// mtime never can.
			"a path containing spaces round-trips",
			"/data/my disk.qcow2 1756000000\n",
			map[string]int64{"/data/my disk.qcow2": 1756000000},
		},
		{
			// A missing file is simply absent, which is what the tolerant
			// command produces. The present ones must still be read.
			"a missing file just is not there",
			"/data/a.qcow2 1756000000\n",
			map[string]int64{"/data/a.qcow2": 1756000000},
		},
		{
			"unreadable lines are skipped, not fatal",
			"/data/a.qcow2 1756000000\ngarbage\n/data/b.qcow2 notanumber\n\n/data/c.qcow2 1756000009\n",
			map[string]int64{"/data/a.qcow2": 1756000000, "/data/c.qcow2": 1756000009},
		},
		{"CRLF is tolerated", "/data/a.qcow2 1756000000\r\n", map[string]int64{"/data/a.qcow2": 1756000000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseStatMTimes(tc.out); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParseStatMTimes(%q)\n = %v\nwant %v", tc.out, got, tc.want)
			}
		})
	}
}

func TestFormatReplicaWrittenAt(t *testing.T) {
	t.Run("stable ordering", func(t *testing.T) {
		in := map[string]int64{"vdc": 3, "vda": 1, "vdb": 2}
		want := "vda=1,vdb=2,vdc=3"
		// Run repeatedly: Go randomises map iteration, so an unsorted
		// implementation passes intermittently. This value lands in a
		// domain's XML, which people read and diff.
		for i := 0; i < 20; i++ {
			if got := FormatReplicaWrittenAt(in); got != want {
				t.Fatalf("FormatReplicaWrittenAt = %q, want %q (iteration %d)", got, want, i)
			}
		}
	})

	t.Run("nothing worth writing", func(t *testing.T) {
		if got := FormatReplicaWrittenAt(nil); got != "" {
			t.Errorf("FormatReplicaWrittenAt(nil) = %q, want empty", got)
		}
	})

	t.Run("a dev that would break the format is dropped, not emitted", func(t *testing.T) {
		// One unrepresentable key must not take the other disks' stamps with
		// it: an entry containing the separators would render a value this
		// package's own parser could not read back.
		got := FormatReplicaWrittenAt(map[string]int64{"vda": 1, "bad,dev": 2, "bad=dev": 3})
		if got != "vda=1" {
			t.Errorf("FormatReplicaWrittenAt = %q, want just the representable disk", got)
		}
	})
}

func TestReplicaWrittenAtRoundTrip(t *testing.T) {
	in := map[string]int64{"vda": 1756000000, "vdb": 1756000005, "sdc": 1756000010}
	got := ParseReplicaWrittenAt(FormatReplicaWrittenAt(in))
	if !reflect.DeepEqual(got, in) {
		t.Errorf("round trip lost data:\n got %v\nwant %v", got, in)
	}
}

func TestParseReplicaWrittenAt(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want map[string]int64
	}{
		{"empty", "", map[string]int64{}},
		{"single", "vda=1756000000", map[string]int64{"vda": 1756000000}},
		{
			// Lenient on purpose. The caller's fallback for a missing entry
			// is well defined, so a garbled entry costs precision. Refusing
			// the whole value would turn one bad entry into a refused sync,
			// which is the failure this field exists to remove.
			"garbled entries are skipped, the rest survive",
			"vda=1756000000,nonsense,vdb=notanumber,=5,vdc=1756000009,",
			map[string]int64{"vda": 1756000000, "vdc": 1756000009},
		},
		{"whitespace is tolerated", " vda = 1756000000 ", map[string]int64{"vda ": 1756000000}},
		{"a value alone is not an entry", "1756000000", map[string]int64{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseReplicaWrittenAt(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParseReplicaWrittenAt(%q)\n = %v\nwant %v", tc.in, got, tc.want)
			}
		})
	}
}

// The two parsers must agree on what an absent stamp looks like, because the
// preflight's fallback branch is chosen by "is this dev in the map".
func TestAbsentStampIsAMissingKeyNotAZero(t *testing.T) {
	m := ParseReplicaWrittenAt("vda=1756000000")
	if _, ok := m["vdb"]; ok {
		t.Error("a dev with no stamp is present in the map; the caller cannot tell it from a real 0")
	}
	if _, ok := ParseStatMTimes("/data/a.qcow2 1756000000\n")["/data/b.qcow2"]; ok {
		t.Error("a path stat could not read is present in the map")
	}
}
