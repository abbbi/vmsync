package restorepoint

import (
	"path"
	"strings"
	"testing"
	"time"
)

// testReplicaDir is the directory a replica's disks live in. The store is one
// level plus a domain below it -- which is the whole subject of this file.
const testReplicaDir = "/data/replicas"

func testStore(t *testing.T) Store {
	t.Helper()
	s, err := StoreForDir(testReplicaDir, "web01")
	if err != nil {
		t.Fatalf("StoreForDir(%q, web01): %v", testReplicaDir, err)
	}
	return s
}

func testTag(t *testing.T) Tag {
	t.Helper()
	return mustTag(t, 1756041600, "vmsync-cpt-000042")
}

func testPoint(t *testing.T) Point {
	t.Helper()
	return testStore(t).Point(testTag(t))
}

// must unwraps a builder that now returns an error. Every one of them does,
// because an unset store would otherwise become a RELATIVE path inside an
// rm -rf; see the convention note at the top of remote.go.
//
// It hands back a closure rather than taking the two values directly so that
// must(t)(Builder(...)) works: Go only allows a multi-value call as an argument
// list when it IS the whole argument list.
func must(t *testing.T) func(string, error) string {
	return func(cmd string, err error) string {
		t.Helper()
		if err != nil {
			t.Fatalf("building a restore point command: %v", err)
		}
		return cmd
	}
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

// SafeKey is duplicated from util.SafeKey for the same reason shQuote is, and
// pinned against the same cases pkg/util's and pkg/actionlog's own tests use.
//
// The stake is higher here than it is for the journal. If this encoding ever
// drifted from util.SafeKey's, a domain's restore points would be looked for in
// a directory that is not the one they were written to -- so its entire history
// would read as empty, on a healthy host, with no error anywhere.
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

// Two domains must never encode to one segment. They would share a store, which
// is the defect the per-domain layout exists to fix, rebuilt one level down and
// this time invisible: the two would agree about the directory, so neither
// would report anything wrong.
func TestSafeKeyNeverCollapsesTwoDifferentDomains(t *testing.T) {
	seen := map[string]string{}
	for _, name := range []string{"web server", "web/server", "web_server", "web%20server", "web%2fserver", "100%", "100%%"} {
		key := SafeKey(name)
		if other, dup := seen[key]; dup {
			t.Fatalf("domains %q and %q both encode to %q, so they would share one restore point store", other, name, key)
		}
		seen[key] = name
	}
}

// SafeKey is a filename encoder and not a shell escape, and the commands in
// this package depend on that division staying visible: they quote every path
// at the point it enters a command. If SafeKey started stripping shell
// metacharacters, shQuote would look redundant and somebody would remove it.
func TestSafeKeyDoesNotPretendToBeShellSafe(t *testing.T) {
	if got := SafeKey("$(rm -rf /)"); !strings.Contains(got, "$(") {
		t.Fatalf("SafeKey stripped shell metacharacters (%q); every path built from a domain name is shQuoted instead, and making this sanitise would invite that quoting's removal", got)
	}
	if strings.Contains(SafeKey("a/b"), "/") {
		t.Error("SafeKey left a path separator in a filename component")
	}
}

// DomainSegment refuses what SafeKey deliberately leaves alone.
//
// SafeKey encodes "/" and " " and nothing else, because it is reversible on
// purpose. "." and ".." are therefore untouched by it -- and they are perfectly
// good filenames that happen to name directories. A domain called ".." would
// give a store of <dir>/.vmsync-rp/.. , which is the replica's own directory,
// and that path is the argument a -reinit hands to rm -rf.
func TestDomainSegmentRefusesNamesThatWouldLeaveTheStore(t *testing.T) {
	for _, bad := range []string{
		"",
		".",
		"..",
		"a\x00b",
		strings.Repeat("x", MaxSegmentBytes), // + DomainPrefix, so over
	} {
		t.Run(bad, func(t *testing.T) {
			if seg, err := DomainSegment(bad); err == nil {
				t.Errorf("DomainSegment(%q) = %q, want a refusal: every path built from it reaches rm -rf", bad, seg)
			}
		})
	}

	// A name whose ENCODED form is over the limit though the name is not. This
	// is the case a length check on the domain name would miss, and its only
	// symptom would be a failed mkdir on a production target mid-sync.
	if _, err := DomainSegment(strings.Repeat("/", MaxSegmentBytes/2)); err == nil {
		t.Error("a domain name that encodes past the filesystem's name limit was accepted; the first sign of it would be an opaque mkdir failure on the target")
	}

	seg, err := DomainSegment("web01")
	if err != nil {
		t.Fatalf("DomainSegment(web01): %v", err)
	}
	if seg != DomainPrefix+"web01" {
		t.Errorf("DomainSegment(web01) = %q, want %q", seg, DomainPrefix+"web01")
	}
}

// The prefix exists for exactly one reason: a domain directory must never be
// readable as a tag. Without it a domain legally named like a tag would be
// indistinguishable from a restore point left flat in the shared root by a
// pre-change run -- and the classifier would then offer another domain's whole
// store as a single restorable point.
func TestADomainSegmentCanNeverBeReadAsATag(t *testing.T) {
	for _, domain := range []string{"web01", "1756041600-vmsync-cpt-000042", "1756041600", "0"} {
		seg, err := DomainSegment(domain)
		if err != nil {
			t.Fatalf("DomainSegment(%q): %v", domain, err)
		}
		if _, err := ParseTag(seg); err == nil {
			t.Errorf("ParseTag(%q) succeeded, so domain %q's store would be read as a restore point", seg, domain)
		}
	}
	// And the converse: an aside can never be read as a live store, because
	// DomainSegment cannot produce a leading dot.
	if strings.HasPrefix(AsidePrefix, DomainPrefix) {
		t.Error("an aside directory would be classified as a live store, so a second reinit would believe it owns it")
	}
}

// StoreForDir is the constructor the two read-only verbs use, and it holds a
// DIRECTORY rather than a disk. Everything it refuses is something that would
// otherwise become a path in a command.
func TestStoreForDirRefusesWhatItCannotSafelyAddress(t *testing.T) {
	for name, dir := range map[string]string{
		"relative":              "data/replicas",
		"unclean":               "/data/replicas/",
		"traversal":             "/data/../data/replicas",
		"root":                  "/",
		"empty":                 "",
		"already the store dir": "/data/replicas/" + DirName,
		"already a domain dir":  "/data/replicas/" + DirName + "/" + DomainPrefix + "web01",
		"an aside":              "/data/replicas/" + DirName + "/" + AsidePrefix + "vm-web01-1756041600",
	} {
		t.Run(name, func(t *testing.T) {
			if s, err := StoreForDir(dir, "web01"); err == nil {
				t.Errorf("StoreForDir(%q, web01) = %s, want a refusal", dir, s)
			}
		})
	}

	// A relative directory is not a hypothetical: -target-disk-path is an
	// operator string, and before this constructor existed it was path.Joined
	// with no check at all. A relative store would resolve against whatever
	// directory the SSH login lands in.
	if _, err := StoreForDir("replicas", "web01"); err == nil {
		t.Error("a relative -target-disk-path was accepted, so rm -rf would run against the SSH login directory")
	}
}

// StoreFor takes a DISK, StoreForDir takes its DIRECTORY, and handing one to
// the other is silent: it answers a directory one level too high, beside
// whatever else lives there, and every command built from it works.
func TestStoreForAndStoreForDirDifferByExactlyOnePathDir(t *testing.T) {
	fromDisk, err := StoreFor("/data/replicas/web01-disk0.qcow2", "web01")
	if err != nil {
		t.Fatalf("StoreFor: %v", err)
	}
	fromDir, err := StoreForDir("/data/replicas", "web01")
	if err != nil {
		t.Fatalf("StoreForDir: %v", err)
	}
	a, _ := fromDisk.Path()
	b, _ := fromDir.Path()
	if a != b {
		t.Errorf("StoreFor(disk) = %q but StoreForDir(its dir) = %q; the two must name one directory or the sync path and the read verbs disagree about where a pair's history is", a, b)
	}
	if want := "/data/replicas/" + DirName + "/" + DomainPrefix + "web01"; a != want {
		t.Errorf("store = %q, want %q", a, want)
	}
}

// The one mistake that would be silent and expensive: =auto falls back to a
// full copy on a filesystem that cannot share extents.
func TestNothingInTheRetentionPathUsesReflinkAuto(t *testing.T) {
	p := testPoint(t)
	status := must(t)(StatusCommand(p, Status{Verify: VerifyNotRun}))
	for name, cmd := range map[string]string{
		"probe":  ProbeCommand(testReplicaDir),
		"copy":   must(t)(CopyCommand(p, "/data/replicas/web01-disk0.qcow2")),
		"stage":  must(t)(StageCommand(p)),
		"status": status,
		"commit": must(t)(CommitCommand(p)),
	} {
		if strings.Contains(cmd, "--reflink=auto") {
			t.Errorf("%s uses --reflink=auto, which silently falls back to a full copy: %s", name, cmd)
		}
	}
	if !strings.Contains(ProbeCommand(testReplicaDir), "--reflink=always") {
		t.Error("the probe does not test --reflink=always, so it is not testing what the copies will do")
	}
	if !strings.Contains(must(t)(CopyCommand(p, "/d/x.qcow2")), "--reflink=always") {
		t.Error("the copy is not --reflink=always")
	}
}

// The clone is the deliberate exception: the operator named the destination
// and it may be on another filesystem entirely.
func TestCloneUsesReflinkAutoOnPurpose(t *testing.T) {
	cmd := must(t)(CloneCommand(testPoint(t), "/data/replicas/web01-disk0.qcow2", "/scratch/look.qcow2"))
	if !strings.Contains(cmd, "--reflink=auto") {
		t.Errorf("the clone must tolerate a destination on another filesystem: %s", cmd)
	}
}

func TestProbeAlwaysExitsZeroAndCleansUp(t *testing.T) {
	cmd := ProbeCommand(testReplicaDir)
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
	store, err := StoreFor(nasty, "web01")
	if err != nil {
		t.Fatalf("StoreFor(%q): %v", nasty, err)
	}
	p := store.Point(testTag(t))

	status := must(t)(StatusCommand(p, Status{Disks: []string{nasty}, Verify: VerifyNotRun}))
	staging := must(t)(RemoveStagingCommand(store, StagingPrefix+testTag(t).String()))

	for name, cmd := range map[string]string{
		"probe":  ProbeCommand("/data/it's a; path"),
		"stage":  must(t)(StageCommand(p)),
		"copy":   must(t)(CopyCommand(p, nasty)),
		"status": status,
		"commit": must(t)(CommitCommand(p)),
		"list":   must(t)(ListCommand(store)),

		"remove":        must(t)(RemoveCommand(p)),
		"removeStaging": staging,
		"removeStore":   must(t)(RemoveStoreCommand(store)),
		"readStatus":    must(t)(ReadStatusCommand(p)),
		"clone":         must(t)(CloneCommand(p, nasty, "/scratch/out's.qcow2")),
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

	// The rename reports the path it moves to, so it is checked the same way.
	mv, _, err := RenameStoreCommand(store, time.Unix(1756041600, 0))
	if err != nil {
		t.Fatalf("RenameStoreCommand: %v", err)
	}
	if strings.Contains(mv, "it's a") || !strings.Contains(mv, `it'\''s`) {
		t.Errorf("the rename does not quote the directory it moves: %s", mv)
	}
}

// The DOMAIN is now a path component too, and it is the one SafeKey
// deliberately does not sanitise: a libvirt domain may legally be named with a
// quote, a newline or a "$(", and SafeKey leaves all three alone by design.
// Only shQuote stands between such a name and a shell on the target, so every
// builder that gained a Store is checked against a hostile domain and not only
// against a hostile path.
func TestCommandsQuoteAHostileDomainName(t *testing.T) {
	for _, domain := range []string{`it's a $(id)`, "web01\nrm -rf /", "web01;id", "`whoami`"} {
		t.Run(domain, func(t *testing.T) {
			store, err := StoreForDir(testReplicaDir, domain)
			if err != nil {
				t.Fatalf("StoreForDir(%q): %v", domain, err)
			}
			p := store.Point(testTag(t))
			status := must(t)(StatusCommand(p, Status{Verify: VerifyNotRun}))
			mv, _, err := RenameStoreCommand(store, time.Unix(1756041600, 0))
			if err != nil {
				t.Fatalf("RenameStoreCommand: %v", err)
			}
			cmds := map[string]string{
				"stage":       must(t)(StageCommand(p)),
				"copy":        must(t)(CopyCommand(p, "/data/replicas/web01-disk0.qcow2")),
				"status":      status,
				"commit":      must(t)(CommitCommand(p)),
				"list":        must(t)(ListCommand(store)),
				"remove":      must(t)(RemoveCommand(p)),
				"removeStore": must(t)(RemoveStoreCommand(store)),
				"renameStore": mv,
				"readStatus":  must(t)(ReadStatusCommand(p)),
			}
			for name, cmd := range cmds {
				// Nothing from the domain name may reach the shell as syntax.
				// Asserted by stripping every single-quoted span and every
				// backslash-escape -- what is left is the command's own
				// grammar, and no part of the domain may appear in it.
				// The segment is the whole of the domain's contribution to
				// every path here, and shQuote quotes a path entire -- so if
				// any of it had escaped the quoting, it would show up here.
				// Checking for bare metacharacters instead would be wrong:
				// these commands legitimately contain ';' and '[' of their own.
				bare := outsideQuotes(cmd)
				if seg := DomainPrefix + SafeKey(domain); strings.Contains(bare, seg) {
					t.Errorf("%s: the DOMAIN's directory name %q reached the shell as syntax rather than as data.\ncommand: %s\noutside the quoting: %s", name, seg, cmd, bare)
				}
				if strings.Contains(domain, "'") {
					if strings.Contains(cmd, "it's a") {
						t.Errorf("%s: an apostrophe from the DOMAIN survived unescaped, so the shell would run the rest: %s", name, cmd)
					}
					if !strings.Contains(cmd, `it'\''s`) {
						t.Errorf("%s: the domain does not appear in its escaped form: %s", name, cmd)
					}
				}
			}
		})
	}
}

func TestStatusCommandPassesThePayloadAsAnArgument(t *testing.T) {
	// A '%' in a disk path must not be read as a printf verb.
	s := Status{Disks: []string{"/data/100%-full/web01.qcow2"}, Verify: VerifyNotRun}
	cmd := must(t)(StatusCommand(testPoint(t), s))
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
	cmd := must(t)(CommitCommand(testPoint(t)))
	if !strings.HasPrefix(cmd, "mv ") {
		t.Errorf("commit is not a rename, so a half-built set could be published: %s", cmd)
	}
	if !strings.Contains(cmd, StagingPrefix+tag.String()) || !strings.Contains(cmd, "/"+tag.String()) {
		t.Errorf("commit does not move staging into place: %s", cmd)
	}
	// Both ends inside this domain's store: a commit that crossed stores would
	// publish one domain's staged copy as another's history.
	root, _ := testStore(t).Path()
	if strings.Count(cmd, root) != 2 {
		t.Errorf("commit does not stay inside %s: %s", root, cmd)
	}
}

// Every command a domain's run issues must address that domain's store and no
// other. This is the property the whole layout exists for, so it is asserted
// directly rather than inferred from the paths in the tests above.
func TestEveryCommandStaysInsideOneDomainsStore(t *testing.T) {
	mine := testStore(t)
	theirs, err := StoreForDir(testReplicaDir, "db01")
	if err != nil {
		t.Fatalf("StoreForDir(db01): %v", err)
	}
	mineRoot, _ := mine.Path()
	theirsRoot, _ := theirs.Path()
	p := mine.Point(testTag(t))

	status := must(t)(StatusCommand(p, Status{Verify: VerifyNotRun}))
	mv, aside, err := RenameStoreCommand(mine, time.Unix(1756041600, 0))
	if err != nil {
		t.Fatalf("RenameStoreCommand: %v", err)
	}
	for name, cmd := range map[string]string{
		"stage":         must(t)(StageCommand(p)),
		"copy":          must(t)(CopyCommand(p, "/data/replicas/web01-disk0.qcow2")),
		"status":        status,
		"commit":        must(t)(CommitCommand(p)),
		"list":          must(t)(ListCommand(mine)),
		"remove":        must(t)(RemoveCommand(p)),
		"removeStaging": must(t)(RemoveStagingCommand(mine, StagingPrefix+testTag(t).String())),
		"removeStore":   must(t)(RemoveStoreCommand(mine)),
		"renameStore":   mv,
		"readStatus":    must(t)(ReadStatusCommand(p)),
	} {
		t.Run(name, func(t *testing.T) {
			if strings.Contains(cmd, theirsRoot) {
				t.Errorf("names another domain's store %q: %s", theirsRoot, cmd)
			}
			if !strings.Contains(cmd, mineRoot) {
				t.Errorf("does not name this domain's store %q at all: %s", mineRoot, cmd)
			}
		})
	}

	// The aside is the one path that is deliberately OUTSIDE the store, because
	// it is where the store goes. It must still be outside the other domain's.
	if strings.HasPrefix(aside, theirsRoot) {
		t.Errorf("the reinit aside %q lands inside db01's store", aside)
	}
	if !strings.HasPrefix(aside, testReplicaDir+"/"+DirName+"/") {
		t.Errorf("the reinit aside %q is not inside the shared root, so it may be on another filesystem and the rename would copy a full image", aside)
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
			"1756041600-vmsync-cpt-000042/",
			"1756052400-vmsync-cpt-000043/",
			StagingPrefix + "1756063200-vmsync-cpt-000044/",
			"somebody-elses-junk/",
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

	// ls -1Ap marks directories and nothing else. An entry without the mark is
	// not a restore point whatever it is called: reported, never staged into,
	// never handed to rm -rf. pkg/inventory's local reader has always checked
	// IsDir, and this is what stops the two disagreeing.
	t.Run("a file named like a restore point is not one", func(t *testing.T) {
		out := strings.Join([]string{
			markerListing,
			"1756041600-vmsync-cpt-000042", // a FILE: no trailing slash
			StagingPrefix + "1756063200-vmsync-cpt-000044",
			"1756052400-vmsync-cpt-000043/", // a real one, for contrast
			"",
		}, "\n")
		l, err := ParseListing(out)
		if err != nil {
			t.Fatalf("ParseListing: %v", err)
		}
		if len(l.Points) != 1 || l.Points[0].Checkpoint != "vmsync-cpt-000043" {
			t.Errorf("points = %+v, want only the actual directory; a regular file reported as a restore point would be staged into and then rm -rf'd", l.Points)
		}
		if len(l.Staging) != 0 {
			t.Errorf("staging = %+v, want none: the entry is a file, so removing it as an abandoned staging directory would delete somebody's file", l.Staging)
		}
		if len(l.Unknown) != 2 {
			t.Errorf("unknown = %+v, want both non-directories reported so nobody is left unaware of them", l.Unknown)
		}
	})

	t.Run("a garbled answer is an error, not an empty directory", func(t *testing.T) {
		if _, err := ParseListing("ls: command not found\n"); err == nil {
			t.Error("a failed listing was read as 'there are no restore points', which would let a prune think everything was already gone")
		}
	})
}

// RemoveCommand emits rm -rf. The only way to reach it is through a validated
// Tag inside a checked Store, so there is no signature that deletes an
// arbitrary path.
func TestRemoveStagingRefusesAnythingItCannotIdentify(t *testing.T) {
	store := testStore(t)
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
			if cmd, err := RemoveStagingCommand(store, bad); err == nil {
				t.Errorf("RemoveStagingCommand accepted %q and would run: %s", bad, cmd)
			}
		})
	}

	good := StagingPrefix + "1756041600-vmsync-cpt-000042"
	cmd, err := RemoveStagingCommand(store, good)
	if err != nil {
		t.Fatalf("RemoveStagingCommand(%q): %v", good, err)
	}
	if !strings.Contains(cmd, good) {
		t.Errorf("the command does not name the directory it removes: %s", cmd)
	}
}

func TestRemoveStaysInsideTheRestorePointDirectory(t *testing.T) {
	p := testPoint(t)
	cmd := must(t)(RemoveCommand(p))
	dir, err := p.Dir()
	if err != nil {
		t.Fatalf("Point.Dir: %v", err)
	}
	if !strings.Contains(cmd, shQuote(dir)) {
		t.Errorf("remove does not target exactly one restore point: %s", cmd)
	}
	if strings.Contains(cmd, "*") {
		t.Errorf("remove contains a glob: %s", cmd)
	}
}

func TestListCommandAlwaysExitsZero(t *testing.T) {
	for name, cmd := range map[string]string{
		"list": must(t)(ListCommand(testStore(t))),
	} {
		if !strings.HasSuffix(cmd, "exit 0") {
			t.Errorf("%s must exit 0 either way, so a non-zero exit means only that the question could not be put", name)
		}
		if !strings.Contains(cmd, "ls -1Ap") {
			t.Errorf("%s does not mark directories, so a regular file would be classified by its name alone: %s", name, cmd)
		}
	}
}

// A whole cycle, as the caller will drive it: probe, take one, prune.
func TestOneRetentionCycle(t *testing.T) {
	p, err := ParsePolicy("3,1h")
	if err != nil {
		t.Fatalf("ParsePolicy: %v", err)
	}
	store, err := StoreFor("/data/replicas/web01-disk0.qcow2", "web01")
	if err != nil {
		t.Fatalf("StoreFor: %v", err)
	}

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
	rm := must(t)(RemoveCommand(store.Point(plan.Remove[0])))
	if !strings.Contains(rm, "1756041600-vmsync-cpt-000040") {
		t.Error("the removal command does not name the tag the plan chose")
	}
}

// -reinit acts on a whole store, one level above the tag validation that
// protects RemoveCommand -- so the store itself is the guard. Store.check is
// that guard, and these are the ways past the one it replaced.
func TestStoreCommandsRefuseAStoreThatIsNotOurs(t *testing.T) {
	at := time.Unix(1756041600, 0)

	// A zero Store is the only invalid one a caller outside this package can
	// hold, and it is the dangerous one: its path is RELATIVE, so an rm -rf
	// built from it would run against the SSH login directory.
	t.Run("zero store", func(t *testing.T) {
		var s Store
		if cmd, err := RemoveStoreCommand(s); err == nil {
			t.Errorf("RemoveStoreCommand accepted a zero store and would run: %s", cmd)
		}
		if cmd, _, err := RenameStoreCommand(s, at); err == nil {
			t.Errorf("RenameStoreCommand accepted a zero store and would run: %s", cmd)
		}
		if cmd, err := ListCommand(s); err == nil {
			t.Errorf("ListCommand accepted a zero store: %s", cmd)
		}
		if s.String() != "<unset restore point store>" {
			t.Errorf("a zero store prints as %q; a log line must not show a path that would be relative", s.String())
		}
	})

	// Fields that disagree. Only reachable from inside this package, which is
	// exactly why check recomputes the segment rather than trusting it: a
	// future edit here is the threat, not an external caller.
	t.Run("forged segment", func(t *testing.T) {
		s := testStore(t)
		s.segment = DomainPrefix + "db01"
		if cmd, err := RemoveStoreCommand(s); err == nil {
			t.Errorf("RemoveStoreCommand accepted a store whose directory is not the one its domain encodes to, and would delete db01's history while reporting web01's: %s", cmd)
		}
	})

	t.Run("bare prefix", func(t *testing.T) {
		s := testStore(t)
		s.domain, s.segment = "", DomainPrefix
		if cmd, err := RemoveStoreCommand(s); err == nil {
			t.Errorf("RemoveStoreCommand accepted the empty-domain store %q: %s", DomainPrefix, cmd)
		}
	})

	// The old guard's cases, carried forward. Each of these was a bad ROOT
	// then; each is now refused by the constructor instead, one step earlier.
	for _, bad := range []string{
		"/data/replicas/" + DirName, // the shared root: the defect itself
		"/",                         // the obvious catastrophe
		"/data/replicas/.vmsync",    // close, but not the name
		"",                          //
		"/data/replicas/" + DirName + "/1756041600-vmsync-cpt-000042", // a point, not a store
	} {
		t.Run("constructor refuses "+bad, func(t *testing.T) {
			if s, err := StoreForDir(bad, "web01"); err == nil {
				// It is only safe for the constructor to accept a path if the
				// store it builds is not the path itself.
				root, _ := s.Path()
				if root == bad {
					t.Errorf("StoreForDir(%q) produced a store AT that path: %s", bad, root)
				}
			}
		})
	}
}

func TestStoreCommandsOnARealStore(t *testing.T) {
	store, err := StoreFor("/data/replicas/web01-disk0.qcow2", "web01")
	if err != nil {
		t.Fatalf("StoreFor: %v", err)
	}
	root, err := store.Path()
	if err != nil {
		t.Fatalf("Store.Path: %v", err)
	}
	at := time.Unix(1756041600, 0)

	rm, err := RemoveStoreCommand(store)
	if err != nil {
		t.Fatalf("RemoveStoreCommand: %v", err)
	}
	if !strings.Contains(rm, shQuote(root)) {
		t.Errorf("remove does not name the directory: %s", rm)
	}

	mv, aside, err := RenameStoreCommand(store, at)
	if err != nil {
		t.Fatalf("RenameStoreCommand: %v", err)
	}
	if aside == root {
		t.Fatal("the aside path is the same as the store, so the rename would be a no-op or fail")
	}
	if !strings.Contains(mv, shQuote(aside)) {
		t.Errorf("the command does not move to the path it reported: %s -> %s", mv, aside)
	}
	// Same instant, same name: the caller warns with the path rather than
	// assuming uniqueness, so the name has to be a function of the instant
	// alone.
	if _, again, _ := RenameStoreCommand(store, at); again != aside {
		t.Errorf("the same instant produced two different aside paths, %q and %q", aside, again)
	}
	// ...and because it can collide, the command refuses rather than letting mv
	// put the second set INSIDE the first, where nothing would ever look for
	// it.
	if !strings.Contains(mv, "if [ -e ") || !strings.Contains(mv, "exit 1") {
		t.Errorf("the rename does not refuse an existing destination, so two reinits in one second would bury one history inside the other: %s", mv)
	}
	if !strings.Contains(mv, "mv -- ") {
		t.Errorf("the rename does not use `mv --`, so a path beginning with a dash would be read as an option: %s", mv)
	}
}

// The aside must be a sibling of the stores, not a child of one: inside, a
// later reinit of the same domain would rm -rf it along with the live set, and
// the whole point of renaming instead of deleting would be lost.
func TestTheReinitAsideIsNotInsideAnyStore(t *testing.T) {
	store := testStore(t)
	root, _ := store.Path()
	_, aside, err := RenameStoreCommand(store, time.Unix(1756041600, 0))
	if err != nil {
		t.Fatalf("RenameStoreCommand: %v", err)
	}
	if strings.HasPrefix(aside, root+"/") || aside == root {
		t.Errorf("aside %q is inside the store %q it is moving", aside, root)
	}
	if !strings.HasPrefix(path.Base(aside), AsidePrefix) {
		t.Errorf("aside %q is not named with %q, so it could be mistaken for a live store", aside, AsidePrefix)
	}
	if _, err := ParseTag(path.Base(aside)); err == nil {
		t.Errorf("aside %q parses as a tag, so it would be offered as a restore point", aside)
	}
}

// outsideQuotes returns cmd with every single-quoted span and every
// backslash-escape removed, leaving only what the shell would read as syntax.
//
// It models the shell's actual rules rather than toggling on every quote:
// inside single quotes nothing is special, and OUTSIDE them a backslash escapes
// the next character -- which is exactly the shape shQuote emits for an
// apostrophe ('\”), so a naive toggler would lose track of the state after the
// first quoted apostrophe and then report safely-quoted text as exposed.
func outsideQuotes(cmd string) string {
	// Spelled as its code point so the literal cannot be mangled by whatever
	// writes this file; a backslash in a rune literal is escaped twice over by
	// the time it has been through a shell heredoc.
	const backslash = byte(0x5c)
	var b strings.Builder
	in := false
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		if in {
			if c == '\'' {
				in = false
			}
			continue
		}
		switch {
		case c == backslash && i+1 < len(cmd):
			i++ // an escaped character is data, not syntax
		case c == '\'':
			in = true
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// The helper above is what several assertions rest on, so it is pinned itself.
func TestOutsideQuotesModelsTheShellsRules(t *testing.T) {
	for _, tc := range []struct{ cmd, want string }{
		{`rm -rf '/data/x'`, "rm -rf "},
		{`rm -rf '/data/it'\''s'`, "rm -rf "},
		{`cat '/a' '/b'`, "cat  "},
		{"echo `id`", "echo `id`"},              // NOT quoted: must show through
		{`echo '` + "`id`" + `'`, "echo "},      // quoted: must not
		{`mv -- '/a' '/b;rm -rf /'`, "mv --  "}, // the metacharacter is data
	} {
		if got := outsideQuotes(tc.cmd); got != tc.want {
			t.Errorf("outsideQuotes(%q) = %q, want %q", tc.cmd, got, tc.want)
		}
	}
}
