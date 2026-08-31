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
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Field 2 of /proc/<pid>/stat is the executable name in parentheses, it is
// NOT escaped, and a process can rename itself to anything -- including
// something containing spaces and close-parens. Splitting the line on
// whitespace is the obvious implementation and it is wrong.
func TestParseProcStatStartTicks(t *testing.T) {
	// The 20 fields after comm, with starttime (field 22) at index 19.
	afterComm := "S 1 1 1 0 -1 4194304 100 0 0 0 5 6 0 0 20 0 1 0 987654321 " +
		"12345678 900 18446744073709551615 1 2 3"

	for _, tc := range []struct {
		name string
		comm string
	}{
		{"an ordinary name", "(vmsync)"},
		{"a name containing a space", "(vm sync)"},
		{"a name containing a close paren", "(vmsync))"},
		{"a name that is entirely parens", "(((())))"},
		{"a name containing digits that look like fields", "(9 9 9)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseProcStatStartTicks("4242 " + tc.comm + " " + afterComm)
			if err != nil {
				t.Fatalf("ParseProcStatStartTicks: %v", err)
			}
			if got != 987654321 {
				t.Errorf("starttime = %d, want 987654321 -- the comm field was mis-parsed", got)
			}
		})
	}
}

func TestParseProcStatStartTicksRejectsRubbish(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		{"no comm field at all", "4242 vmsync S 1 2 3"},
		{"too few fields after comm", "4242 (vmsync) S 1 2 3"},
		{"starttime is not a number", "4242 (vmsync) S 1 1 1 0 -1 4194304 100 0 0 0 5 6 0 0 20 0 1 0 notanumber 1 2 3"},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseProcStatStartTicks(tc.in); err == nil {
				t.Errorf("ParseProcStatStartTicks(%q) returned no error", tc.in)
			}
		})
	}
}

// The identity round-trips through a real file, because that file is written
// by one program and read by another.
func TestRunLockIdentityRoundTrip(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(RunLockPath(dir, "web01"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	want := RunLockIdentity{
		PID: 4242, BootID: "boot-abc", StartTicks: 987654321,
		StartedAtUnix: 1_800_000_000, Kind: "sync",
		SourceDomain: "web01", TargetRef: "dr01:web01", RunID: "run-1",
	}
	if err := WriteRunLockIdentity(f, want); err != nil {
		t.Fatalf("WriteRunLockIdentity: %v", err)
	}

	got, ok, err := ReadRunLockIdentity(dir, "web01")
	if err != nil || !ok {
		t.Fatalf("ReadRunLockIdentity: ok=%v err=%v", ok, err)
	}
	if got != want {
		t.Errorf("round trip\n got %+v\nwant %+v", got, want)
	}
}

// Rewriting must not leave a tail of the previous, longer record behind --
// which is what an in-place write without a truncate does, and it produces
// trailing bytes that make the next parse fail.
func TestWriteRunLockIdentityTruncatesTheOldRecord(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(RunLockPath(dir, "web01"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	long := RunLockIdentity{PID: 1, Kind: "sync", SourceDomain: "a-very-long-domain-name-indeed", TargetRef: "some-distant-host:a-very-long-domain-name-indeed"}
	if err := WriteRunLockIdentity(f, long); err != nil {
		t.Fatal(err)
	}
	short := RunLockIdentity{PID: 2, Kind: "s"}
	if err := WriteRunLockIdentity(f, short); err != nil {
		t.Fatal(err)
	}

	got, ok, err := ReadRunLockIdentity(dir, "web01")
	if err != nil {
		t.Fatalf("the second record did not parse, so the first one's tail survived: %v", err)
	}
	if !ok || got.PID != 2 {
		t.Errorf("got %+v, want the second record", got)
	}
}

// Every vmsync before this feature left an EMPTY lock file behind, and a
// holder that has not written its identity yet looks identical. Neither is an
// error; both simply carry no information.
func TestReadRunLockIdentityHandlesEmptyAndMissing(t *testing.T) {
	dir := t.TempDir()

	if _, ok, err := ReadRunLockIdentity(dir, "never-locked"); ok || err != nil {
		t.Errorf("a missing lock file gave ok=%v err=%v, want false/nil", ok, err)
	}

	if err := os.WriteFile(RunLockPath(dir, "empty"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := ReadRunLockIdentity(dir, "empty"); ok || err != nil {
		t.Errorf("an empty lock file gave ok=%v err=%v, want false/nil -- this is what every older vmsync leaves behind", ok, err)
	}
}

// Fail-open is the contract: every "cannot tell" answer must be "not held", so
// the caller launches and the engine's own lock decides. Failing closed here
// would defer every VM forever on one permanent error.
func TestRunLockHeldFailsOpen(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   RunLockIdentity
	}{
		{"no pid", RunLockIdentity{PID: 0}},
		{"negative pid", RunLockIdentity{PID: -1}},
		{"a boot id from a previous boot", RunLockIdentity{PID: os.Getpid(), BootID: "definitely-not-this-boot"}},
		{"a pid that cannot exist", RunLockIdentity{PID: 0x7FFFFFF0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			held, reason := RunLockHeld(tc.id, "")
			if held {
				t.Errorf("RunLockHeld = true (%s), want false so the caller launches and lets the engine decide", reason)
			}
			if reason == "" {
				t.Error("no reason given; the log would say nothing useful")
			}
		})
	}
}

// The PID-reuse guard, against this very process: same pid, wrong start time.
func TestRunLockHeldRejectsAReusedPID(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs /proc")
	}
	self := os.Getpid()
	ticks, err := ProcStartTicks(self)
	if err != nil {
		t.Fatalf("ProcStartTicks(self): %v", err)
	}

	// Honest identity: held.
	boot, _ := CurrentBootID()
	if held, reason := RunLockHeld(RunLockIdentity{PID: self, BootID: boot, StartTicks: ticks}, ""); !held {
		t.Errorf("this process reports as not held (%s)", reason)
	}
	// Same pid, a start time it never had: a different process wearing a
	// recycled pid.
	if held, _ := RunLockHeld(RunLockIdentity{PID: self, BootID: boot, StartTicks: ticks + 1}, ""); held {
		t.Error("a pid whose start time does not match is reported as held -- pid reuse would be mistaken for a live run")
	}
}

func TestSameBinaryComparesByBaseName(t *testing.T) {
	// The agent knows its configured -vmsync-path; /proc/<pid>/exe reports the
	// resolved target of every symlink along the way. Both are correct.
	if !sameBinary("/usr/local/bin/vmsync", "/opt/vmsync-1.2.3/bin/vmsync") {
		t.Error("two paths to the same program compared unequal")
	}
	if sameBinary("/usr/local/bin/vmsync", "/usr/bin/qemu-img") {
		t.Error("two different programs compared equal")
	}
}

func TestRunLockPathIsWhereIdentityIsRead(t *testing.T) {
	// ReadRunLockIdentity must look at exactly the file AcquireRunLock
	// creates, or it silently reads nothing forever.
	dir := t.TempDir()
	want := filepath.Join(dir, "web01.lock")
	if got := RunLockPath(dir, "web01"); got != want {
		t.Errorf("RunLockPath = %q, want %q", got, want)
	}
}
