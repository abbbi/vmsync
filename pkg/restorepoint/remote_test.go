package restorepoint

import (
	"strings"
	"testing"
	"time"
)

const testRoot = "/data/replicas/.vmsync-rp"

func testTag(t *testing.T) Tag {
	t.Helper()
	return mustTag(t, 1756041600, "vmsync-cpt-000042")
}

// shQuote is duplicated from util.ShQuote because importing pkg/util would
// make this package Linux-only. That duplication is only safe if it actually
// behaves the same, so it is pinned here.
func TestShQuote(t *testing.T) {
	for in, want := range map[string]string{
		"/data/x.qcow2":    `'/data/x.qcow2'`,
		"":                 `''`,
		"with space":       `'with space'`,
		"it's":             `'it'\''s'`,
		"$(rm -rf /)":      `'$(rm -rf /)'`,
		"`whoami`":         "'`whoami`'",
		"a\nb":             "'a\nb'",
		`back\slash`:       `'back\slash'`,
		"semi;colon":       `'semi;colon'`,
		"'":                `''\'''`,
		"quote'and'quote":  `'quote'\''and'\''quote'`,
		"already 'quoted'": `'already '\''quoted'\'''`,
	} {
		if got := shQuote(in); got != want {
			t.Errorf("shQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

// The one mistake that would be silent and expensive: =auto falls back to a
// full copy on a filesystem that cannot share extents.
func TestNothingInTheRetentionPathUsesReflinkAuto(t *testing.T) {
	tag := testTag(t)
	status, err := StatusCommand(testRoot, tag, Status{Verify: VerifyNotRun})
	if err != nil {
		t.Fatalf("StatusCommand: %v", err)
	}
	for name, cmd := range map[string]string{
		"probe":  ProbeCommand("/data/replicas"),
		"copy":   CopyCommand(testRoot, tag, "/data/replicas/web01-disk0.qcow2"),
		"stage":  StageCommand(testRoot, tag),
		"status": status,
		"commit": CommitCommand(testRoot, tag),
	} {
		if strings.Contains(cmd, "--reflink=auto") {
			t.Errorf("%s uses --reflink=auto, which silently falls back to a full copy: %s", name, cmd)
		}
	}
	if !strings.Contains(ProbeCommand("/data/replicas"), "--reflink=always") {
		t.Error("the probe does not test --reflink=always, so it is not testing what the copies will do")
	}
	if !strings.Contains(CopyCommand(testRoot, tag, "/d/x.qcow2"), "--reflink=always") {
		t.Error("the copy is not --reflink=always")
	}
}

// The clone is the deliberate exception: the operator named the destination
// and it may be on another filesystem entirely.
func TestCloneUsesReflinkAutoOnPurpose(t *testing.T) {
	cmd := CloneCommand(testRoot, testTag(t), "/data/replicas/web01-disk0.qcow2", "/scratch/look.qcow2")
	if !strings.Contains(cmd, "--reflink=auto") {
		t.Errorf("the clone must tolerate a destination on another filesystem: %s", cmd)
	}
}

func TestProbeAlwaysExitsZeroAndCleansUp(t *testing.T) {
	cmd := ProbeCommand("/data/replicas")
	if !strings.HasSuffix(cmd, "exit 0") {
		t.Error("the probe must exit 0 either way, so a non-zero exit means only that the question could not be put")
	}
	if !strings.Contains(cmd, "rm -f") {
		t.Error("the probe leaves its scratch files behind")
	}
	if strings.Contains(cmd, "mkdir") {
		t.Error("asking a question must not create a directory as a side effect")
	}
}

func TestParseProbe(t *testing.T) {
	yes, err := ParseProbe("some noise\n" + markerReflinkOK + "\n")
	if err != nil || !yes {
		t.Errorf("ParseProbe(ok) = %v, %v", yes, err)
	}
	no, err := ParseProbe(markerReflinkNo)
	if err != nil || no {
		t.Errorf("ParseProbe(no) = %v, %v", no, err)
	}
	// Neither marker means the command did not run as written. Reading that
	// as "no" would silently disable retention the operator asked for.
	if _, err := ParseProbe("cp: unrecognized option\n"); err == nil {
		t.Error("a garbled probe answer was read as a definite no")
	}
}

func TestCommandsQuoteHostilePaths(t *testing.T) {
	nasty := "/data/it's a; path/web01 $(id).qcow2"
	tag := testTag(t)
	root := Root(nasty)

	status, err := StatusCommand(root, tag, Status{Disks: []string{nasty}, Verify: VerifyNotRun})
	if err != nil {
		t.Fatalf("StatusCommand: %v", err)
	}
	staging, err := RemoveStagingCommand(root, StagingPrefix+tag.String())
	if err != nil {
		t.Fatalf("RemoveStagingCommand: %v", err)
	}

	for name, cmd := range map[string]string{
		"probe":         ProbeCommand("/data/it's a; path"),
		"stage":         StageCommand(root, tag),
		"copy":          CopyCommand(root, tag, nasty),
		"status":        status,
		"commit":        CommitCommand(root, tag),
		"list":          ListCommand(root),
		"remove":        RemoveCommand(root, tag),
		"removeStaging": staging,
		"readStatus":    ReadStatusCommand(root, tag),
		"clone":         CloneCommand(root, tag, nasty, "/scratch/out's.qcow2"),
	} {
		t.Run(name, func(t *testing.T) {
			// Every apostrophe from a path must arrive escaped. An unescaped
			// one would end the quoting and hand the rest to the shell.
			if strings.Contains(cmd, "it's a") {
				t.Errorf("an apostrophe survived unescaped, so the shell would see %q as code: %s", "s a; path...", cmd)
			}
			if !strings.Contains(cmd, `it'\''s`) {
				t.Errorf("the path does not appear in its escaped form at all: %s", cmd)
			}
		})
	}
}

func TestStatusCommandPassesThePayloadAsAnArgument(t *testing.T) {
	// A '%' in a disk path must not be read as a printf verb.
	s := Status{Disks: []string{"/data/100%-full/web01.qcow2"}, Verify: VerifyNotRun}
	cmd, err := StatusCommand(testRoot, testTag(t), s)
	if err != nil {
		t.Fatalf("StatusCommand: %v", err)
	}
	if !strings.HasPrefix(cmd, "printf '%s' ") {
		t.Errorf("the payload is not passed as an argument to a fixed format string: %s", cmd)
	}
	if !strings.Contains(cmd, StatusName) {
		t.Errorf("the sidecar is not written to %s: %s", StatusName, cmd)
	}
	if !strings.Contains(cmd, StagingPrefix) {
		t.Error("the sidecar is written outside the staging directory, so it would land in a published restore point before the set is complete")
	}
}

func TestCommitIsARenameWithinTheDirectory(t *testing.T) {
	tag := testTag(t)
	cmd := CommitCommand(testRoot, tag)
	if !strings.HasPrefix(cmd, "mv ") {
		t.Errorf("commit is not a rename, so a half-built set could be published: %s", cmd)
	}
	if !strings.Contains(cmd, StagingPrefix+tag.String()) || !strings.Contains(cmd, "/"+tag.String()) {
		t.Errorf("commit does not move staging into place: %s", cmd)
	}
}

func TestParseListing(t *testing.T) {
	t.Run("no directory yet", func(t *testing.T) {
		l, err := ParseListing(markerListingNone + "\n")
		if err != nil {
			t.Fatalf("ParseListing: %v", err)
		}
		if len(l.Points) != 0 || len(l.Staging) != 0 || len(l.Unknown) != 0 {
			t.Errorf("expected an empty listing, got %+v", l)
		}
	})

	t.Run("empty directory", func(t *testing.T) {
		l, err := ParseListing(markerListing + "\n")
		if err != nil {
			t.Fatalf("ParseListing: %v", err)
		}
		if len(l.Points) != 0 {
			t.Errorf("expected no points, got %+v", l.Points)
		}
	})

	t.Run("a real directory", func(t *testing.T) {
		out := strings.Join([]string{
			markerListing,
			"1756041600-vmsync-cpt-000042",
			"1756052400-vmsync-cpt-000043",
			StagingPrefix + "1756063200-vmsync-cpt-000044",
			"somebody-elses-junk",
			"",
		}, "\n")
		l, err := ParseListing(out)
		if err != nil {
			t.Fatalf("ParseListing: %v", err)
		}
		if len(l.Points) != 2 {
			t.Errorf("points = %+v, want 2", l.Points)
		}
		if len(l.Staging) != 1 {
			t.Errorf("staging = %+v, want 1", l.Staging)
		}
		if len(l.Unknown) != 1 || l.Unknown[0] != "somebody-elses-junk" {
			t.Errorf("unknown = %+v, want the one unrecognised entry reported rather than swallowed", l.Unknown)
		}
	})

	t.Run("a garbled answer is an error, not an empty directory", func(t *testing.T) {
		if _, err := ParseListing("ls: command not found\n"); err == nil {
			t.Error("a failed listing was read as 'there are no restore points', which would let a prune think everything was already gone")
		}
	})
}

// RemoveCommand emits rm -rf. The only way to reach it is through a validated
// Tag, so there is no signature that deletes an arbitrary path.
func TestRemoveStagingRefusesAnythingItCannotIdentify(t *testing.T) {
	for _, bad := range []string{
		"1756041600-vmsync-cpt-000042", // a real restore point, not staging
		StagingPrefix,                  // prefix alone
		StagingPrefix + "..",
		StagingPrefix + "../../etc",
		StagingPrefix + "not-a-tag",
		"..",
		"",
	} {
		t.Run(bad, func(t *testing.T) {
			if cmd, err := RemoveStagingCommand(testRoot, bad); err == nil {
				t.Errorf("RemoveStagingCommand accepted %q and would run: %s", bad, cmd)
			}
		})
	}

	good := StagingPrefix + "1756041600-vmsync-cpt-000042"
	cmd, err := RemoveStagingCommand(testRoot, good)
	if err != nil {
		t.Fatalf("RemoveStagingCommand(%q): %v", good, err)
	}
	if !strings.Contains(cmd, good) {
		t.Errorf("the command does not name the directory it removes: %s", cmd)
	}
}

func TestRemoveStaysInsideTheRestorePointDirectory(t *testing.T) {
	tag := testTag(t)
	cmd := RemoveCommand(testRoot, tag)
	if !strings.Contains(cmd, shQuote(testRoot+"/"+tag.String())) {
		t.Errorf("remove does not target exactly one restore point: %s", cmd)
	}
	if strings.Contains(cmd, "*") {
		t.Errorf("remove contains a glob: %s", cmd)
	}
}

func TestListCommandAlwaysExitsZero(t *testing.T) {
	if !strings.HasSuffix(ListCommand(testRoot), "exit 0") {
		t.Error("the listing must exit 0 either way, so a non-zero exit means only that the question could not be put")
	}
}

// A whole cycle, as the caller will drive it: probe, take one, prune.
func TestOneRetentionCycle(t *testing.T) {
	p, err := ParsePolicy("3,1h")
	if err != nil {
		t.Fatalf("ParsePolicy: %v", err)
	}
	disk := "/data/replicas/web01-disk0.qcow2"
	root := Root(disk)

	existing := []Tag{
		mustTag(t, 1756041600, "vmsync-cpt-000040"),
		mustTag(t, 1756045200, "vmsync-cpt-000041"),
		mustTag(t, 1756048800, "vmsync-cpt-000042"),
	}
	now := time.Unix(1756052400, 0)

	if !Due(Latest(existing), now, p) {
		t.Fatal("an hour after the last restore point should be due")
	}

	fresh := mustTag(t, now.Unix(), "vmsync-cpt-000043")
	// Prune counts the new one, and runs after it is in place.
	plan := Prune(append(append([]Tag{}, existing...), fresh), p)
	if len(plan.Keep) != 3 || len(plan.Remove) != 1 {
		t.Fatalf("plan = %+v, want 3 kept and 1 removed", plan)
	}
	if plan.Remove[0].Checkpoint != "vmsync-cpt-000040" {
		t.Errorf("removed %q, want the oldest", plan.Remove[0].Checkpoint)
	}
	if !strings.Contains(RemoveCommand(root, plan.Remove[0]), "1756041600-vmsync-cpt-000040") {
		t.Error("the removal command does not name the tag the plan chose")
	}
}
