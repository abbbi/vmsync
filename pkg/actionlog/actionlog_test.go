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

package actionlog

import (
	"strings"
	"testing"
)

// SafeKey is duplicated from pkg/util; that is only safe if it behaves the
// same. These are the cases pkg/util's own reasoning is written about: two
// different keys must never collide on one filename, because a collision means
// two machines' histories interleaved in one file.
func TestSafeKey(t *testing.T) {
	for in, want := range map[string]string{
		"web01":            "web01",
		"web server":       "web%20server",
		"web/server":       "web%2fserver",
		"web_server":       "web_server",
		"100%":             "100%%",
		"a%20b":            "a%%20b",
		"../../etc/passwd": "..%2f..%2fetc%2fpasswd",
		"":                 "",
	} {
		if got := SafeKey(in); got != want {
			t.Errorf("SafeKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// The collision this encoding exists to prevent, stated as a test rather than
// left to the reader of the comment.
func TestSafeKeyNeverCollapsesTwoDifferentDomains(t *testing.T) {
	seen := map[string]string{}
	for _, name := range []string{"web server", "web/server", "web_server", "web%20server", "web%2fserver", "100%", "100%%"} {
		key := SafeKey(name)
		if other, dup := seen[key]; dup {
			t.Fatalf("domains %q and %q both encode to %q, so they would share one journal file", other, name, key)
		}
		seen[key] = name
	}
}

// SafeKey makes a name one filesystem COMPONENT. It emphatically does not make
// it safe for a shell, and a reader who confuses the two turns a domain name
// into a command on a production target.
func TestSafeKeyDoesNotPretendToBeShellSafe(t *testing.T) {
	if got := SafeKey("$(rm -rf /)"); !strings.Contains(got, "$(") {
		t.Fatalf("SafeKey stripped shell metacharacters (%q); the journal's quoting assumes it does NOT, so making it do so here would leave AppendCommand's quoting looking redundant and invite its removal", got)
	}
	if strings.Contains(SafeKey("a/b"), "/") {
		t.Error("SafeKey left a path separator in a filename component")
	}
}

// ShQuote is duplicated for the same reason as SafeKey, and pinned against the
// same cases pkg/restorepoint pins its own copy with.
func TestShQuote(t *testing.T) {
	for in, want := range map[string]string{
		"/data/x.jsonl":   `'/data/x.jsonl'`,
		"":                `''`,
		"with space":      `'with space'`,
		"it's":            `'it'\''s'`,
		"$(rm -rf /)":     `'$(rm -rf /)'`,
		"`whoami`":        "'`whoami`'",
		"a\nb":            "'a\nb'",
		"semi;colon":      `'semi;colon'`,
		"100%":            `'100%'`,
		"'":               `''\'''`,
		`back\slash`:      `'back\slash'`,
		"quote'and'quote": `'quote'\''and'\''quote'`,
	} {
		if got := ShQuote(in); got != want {
			t.Errorf("ShQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestRootMirrorsRestorePointRoot(t *testing.T) {
	if got, want := Root("/data/replicas/web01-disk0.qcow2"), "/data/replicas/"+DirName; got != want {
		t.Errorf("Root = %q, want %q", got, want)
	}
	// The mistake RootOfDir exists to prevent: Root takes a DISK, and handing
	// it a directory silently writes the journal one level too high, beside
	// somebody else's disks.
	if got := Root("/data/replicas"); got == "/data/replicas/"+DirName {
		t.Fatal("Root treated a directory as a disk path; RootOfDir is the function for that and the two must stay distinguishable")
	}
	if got, want := RootOfDir("/data/replicas"), "/data/replicas/"+DirName; got != want {
		t.Errorf("RootOfDir = %q, want %q", got, want)
	}
}

func TestFileAndRotatedFile(t *testing.T) {
	root := "/data/replicas/" + DirName
	if got, want := File(root, "web01"), root+"/web01.jsonl"; got != want {
		t.Errorf("File = %q, want %q", got, want)
	}
	if got, want := File(root, "web server"), root+"/web%20server.jsonl"; got != want {
		t.Errorf("File of a spaced domain = %q, want %q", got, want)
	}
	// A domain name full of separators must not escape the journal directory.
	if got := File(root, "../../etc/passwd"); strings.Count(got, "/") != strings.Count(root, "/")+1 {
		t.Errorf("File(%q) = %q escaped the journal directory", "../../etc/passwd", got)
	}
	if got, want := RotatedFile(File(root, "web01")), root+"/web01.1.jsonl"; got != want {
		t.Errorf("RotatedFile = %q, want %q", got, want)
	}
	// Still recognisably JSON Lines, which is what a glob during an incident
	// depends on.
	if !strings.HasSuffix(RotatedFile(File(root, "web01")), Ext) {
		t.Error("the rotated generation lost its .jsonl extension")
	}
}

func TestRotateDecisionAndTheCapItImplies(t *testing.T) {
	if RotateDecision(RotateAtBytes - 1) {
		t.Error("rotated below the threshold")
	}
	if !RotateDecision(RotateAtBytes) {
		t.Error("did not rotate at the threshold")
	}
	if !RotateDecision(RotateAtBytes + 1) {
		t.Error("did not rotate above the threshold")
	}
	if RotateDecision(0) {
		t.Error("rotated an empty file")
	}
	// One live generation plus one rotated. A third would double what a
	// journal costs the filesystem the replicas live on.
	if Generations != 1 {
		t.Fatalf("Generations = %d; the per-domain cap below is written for exactly one", Generations)
	}
	if MaxPerDomainBytes != 2*RotateAtBytes {
		t.Errorf("MaxPerDomainBytes = %d, want %d", MaxPerDomainBytes, 2*RotateAtBytes)
	}
	if MaxPerDomainBytes != 8<<20 {
		t.Errorf("the per-domain cap is %d bytes, and the design fixes it at 8 MiB", MaxPerDomainBytes)
	}
}

func TestAppendCommandShape(t *testing.T) {
	root := "/data/replicas/" + DirName
	file := File(root, "web01")
	cmd := AppendCommand(root, file, 512)

	if !strings.HasPrefix(cmd, "mkdir -p "+ShQuote(root)) {
		t.Errorf("the journal directory must be created before anything else is attempted: %s", cmd)
	}
	if !strings.HasSuffix(cmd, "cat >> "+ShQuote(file)) {
		t.Errorf("the record must be appended from STDIN as the last thing the command does: %s", cmd)
	}
	if !strings.Contains(cmd, "mv -f "+ShQuote(file)+" "+ShQuote(RotatedFile(file))) {
		t.Errorf("rotation is missing, so the live file would grow without limit on the filesystem holding the replicas: %s", cmd)
	}
	// One line: this crosses as a single ssh command, and an embedded newline
	// in the SKELETON would split it into two the remote shell runs
	// independently.
	if strings.Contains(cmd, "\n") {
		t.Errorf("the command skeleton spans more than one line: %q", cmd)
	}
}

// The payload never appears in the command. That is the whole reason it is
// read from stdin: a record is arbitrary text up to 2 KiB, and putting it in
// the command line would mean quoting it for every shell that might run it and
// living inside ARG_MAX.
func TestAppendCommandNeverCarriesThePayload(t *testing.T) {
	root := "/data/replicas/" + DirName
	file := File(root, "web01")
	payload := `{"v":1,"k":"intent","aid":"$(rm -rf /)","err":"it's broken\nreally"}`
	cmd := AppendCommand(root, file, len(payload))
	for _, fragment := range []string{"intent", "$(rm -rf /)", "it's broken", `{"v"`} {
		if strings.Contains(cmd, fragment) {
			t.Fatalf("the command carries part of the record (%q); it must arrive on stdin: %s", fragment, cmd)
		}
	}
}

// printf would read a '%' in a path as a format verb. Nothing here may use it.
func TestAppendCommandUsesNoPrintf(t *testing.T) {
	cmd := AppendCommand("/data/100%/"+DirName, "/data/100%/"+DirName+"/web01.jsonl", 64)
	if strings.Contains(cmd, "printf") {
		t.Errorf("printf would read the %% in this path as a format verb: %s", cmd)
	}
}

// The property that matters: a hostile domain name or path can only ever land
// inside a quoted slot. Proven by shaping the command with the dangerous paths
// and with harmless ones and requiring the two skeletons to be identical --
// anything that leaked out of a slot would change the shape.
func TestAppendCommandConfinesAdversarialPathsToQuotedSlots(t *testing.T) {
	shape := func(root, file string) string {
		c := AppendCommand(root, file, 100)
		c = strings.ReplaceAll(c, ShQuote(RotatedFile(file)), "<ROTATED>")
		c = strings.ReplaceAll(c, ShQuote(file), "<FILE>")
		c = strings.ReplaceAll(c, ShQuote(root), "<ROOT>")
		return c
	}
	benign := shape("/data/replicas/"+DirName, "/data/replicas/"+DirName+"/web01.jsonl")

	for _, nasty := range []string{
		"it's",
		"$(rm -rf /)",
		"`whoami`",
		"a\nb",
		"100%",
		`x";rm -rf /;"`,
		"semi;colon",
		"with space",
		"tab\there",
		`back\slash`,
		"$HOME",
		"&& reboot",
	} {
		root := "/data/" + nasty + "/" + DirName
		file := File(root, nasty)
		got := shape(root, file)
		if got != benign {
			t.Errorf("a path containing %q changed the command's shape, so part of it escaped its quoted slot:\n got %q\nwant %q", nasty, got, benign)
		}
		if strings.Contains(got, nasty) {
			t.Errorf("part of %q survived outside its quoted slot: %s", nasty, got)
		}
	}
}

// Rotation must be decided by the same rule on both sides. The remote command
// compares the file's CURRENT size against the threshold, so the threshold has
// to be the limit minus the record about to be written -- otherwise the file
// ends up one record past the bound every time, and the two writers disagree
// about when a generation ends.
func TestAppendCommandRotationThresholdMatchesRotateDecision(t *testing.T) {
	const payload = 700
	cmd := AppendCommand("/r", "/r/d.jsonl", payload)
	want := RotateAtBytes - payload
	if !strings.Contains(cmd, "-ge "+itoa(want)+" ") {
		t.Errorf("the remote threshold is not %d, so it disagrees with RotateDecision: %s", want, cmd)
	}
	// Equivalence, stated directly: size >= limit-payload is exactly
	// RotateDecision(size+payload).
	for _, size := range []int64{0, 1, int64(want) - 1, int64(want), int64(want) + 1, RotateAtBytes} {
		remote := size >= int64(want)
		if local := RotateDecision(size + payload); local != remote {
			t.Errorf("at size %d the local writer says rotate=%v and the remote one says %v", size, local, remote)
		}
	}
}

// A record larger than the whole rotation limit must not produce a negative
// threshold, which some shells read as "always" and others refuse outright.
func TestAppendCommandClampsAnAbsurdPayload(t *testing.T) {
	cmd := AppendCommand("/r", "/r/d.jsonl", RotateAtBytes*2)
	if strings.Contains(cmd, "-ge -") {
		t.Errorf("negative rotation threshold: %s", cmd)
	}
	if !strings.Contains(cmd, "-ge 0 ") {
		t.Errorf("an oversized payload should clamp the threshold to 0: %s", cmd)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}
