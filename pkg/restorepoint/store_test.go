package restorepoint

import (
	"strings"
	"testing"
	"time"
)

// A replica directory whose name merely begins with "vm-" is an ordinary
// directory, and refusing it breaks retention on that pair forever.
//
// The guard exists to catch an operator pasting a restore point path where the
// replica's own directory belongs. Its first form tested the last component
// against DomainPrefix, which also refused /data/vm-images, /srv/vm-disks and
// every other real directory named that way -- on every run, for a
// -target-disk-path that worked before the store was keyed per domain. What
// actually means "this is a restore point path" is a component named DirName.
func TestStoreForDirAcceptsAReplicaDirectoryNamedLikeAStore(t *testing.T) {
	for _, dir := range []string{
		"/data/vm-images",
		"/srv/vm-disks",
		"/mnt/pool/vm-storage",
		"/data/vm-web01",
		"/data/.replaced-by-hand",
		"/vm-images",
	} {
		t.Run(dir, func(t *testing.T) {
			s, err := StoreForDir(dir, "web01")
			if err != nil {
				t.Fatalf("StoreForDir(%q) was refused, so -retention on that pair fails on every run: %v", dir, err)
			}
			got, err := s.Path()
			if err != nil {
				t.Fatalf("Store.Path: %v", err)
			}
			if want := dir + "/" + DirName + "/" + DomainPrefix + "web01"; got != want {
				t.Errorf("store = %q, want %q", got, want)
			}
		})
	}
}

// ...and a path that really is inside a restore point store is still refused,
// at any depth, because a store nested inside a store is one a later -reinit
// would rm -rf while taking the outer one's history with it.
func TestStoreForDirRefusesAPathInsideAStore(t *testing.T) {
	for _, dir := range []string{
		"/data/replicas/" + DirName,
		"/data/replicas/" + DirName + "/" + DomainPrefix + "web01",
		"/data/replicas/" + DirName + "/" + DomainPrefix + "web01/1756041600-vmsync-cpt-000042",
		"/data/replicas/" + DirName + "/" + AsidePrefix + "vm-web01-1756041600",
		"/" + DirName,
	} {
		t.Run(dir, func(t *testing.T) {
			if s, err := StoreForDir(dir, "web01"); err == nil {
				t.Errorf("StoreForDir(%q) = %s, want a refusal: a store nested inside a store is deleted by the outer one's reinit", dir, s)
			}
		})
	}
}

// Tag is an exported struct with exported fields, so a caller can build one
// without a constructor -- and that value reaches rm -rf.
//
// RemoveCommand's contract is that the only way to it is through a validated
// tag. That was a statement about how callers behave, not a property of the
// code: Tag{At: t, Checkpoint: "../../etc"} needs no constructor.
//
// What it produced was `rm -rf <store>/etc`, not an escape from the store --
// String() prefixes the instant, so the first path component is never itself
// ".." and path.Join cleans the rest inside. The point is still worth pinning:
// a command built from a tag nothing validated is one nobody can reason about,
// and it is the next change to String() or DiskPath that would turn it into a
// traversal. Every path builder now revalidates, so the contract is enforced
// rather than asserted.
func TestAForgedTagCannotEscapeTheStore(t *testing.T) {
	s, err := StoreForDir("/data/replicas", "web01")
	if err != nil {
		t.Fatalf("StoreForDir: %v", err)
	}
	at := time.Unix(1756041600, 0)

	for _, checkpoint := range []string{
		"../../etc",
		"..",
		".",
		"../vm-db01",
		"a/b",
		".hidden",
		"with space",
		"semi;colon",
		"$(id)",
		"",
	} {
		t.Run(checkpoint, func(t *testing.T) {
			p := s.Point(Tag{At: at, Checkpoint: checkpoint})

			if cmd, err := RemoveCommand(p); err == nil {
				t.Errorf("RemoveCommand accepted a forged tag and would run: %s", cmd)
			}
			if dir, err := p.Dir(); err == nil {
				t.Errorf("Point.Dir accepted a forged tag: %q", dir)
			}
			if cmd, err := StageCommand(p); err == nil {
				t.Errorf("StageCommand accepted a forged tag: %s", cmd)
			}
			if cmd, err := CommitCommand(p); err == nil {
				t.Errorf("CommitCommand accepted a forged tag: %s", cmd)
			}
			if cmd, err := ReadStatusCommand(p); err == nil {
				t.Errorf("ReadStatusCommand accepted a forged tag: %s", cmd)
			}
			if cmd, err := CopyCommand(p, "/data/replicas/web01.qcow2"); err == nil {
				t.Errorf("CopyCommand accepted a forged tag: %s", cmd)
			}
		})
	}

	// A legitimate tag still works, so the check refuses rather than refusing
	// everything.
	good, err := NewTag(at, "vmsync-cpt-000042")
	if err != nil {
		t.Fatalf("NewTag: %v", err)
	}
	cmd, err := RemoveCommand(s.Point(good))
	if err != nil {
		t.Fatalf("RemoveCommand on a real tag: %v", err)
	}
	if !strings.Contains(cmd, "/data/replicas/"+DirName+"/"+DomainPrefix+"web01/1756041600-vmsync-cpt-000042") {
		t.Errorf("remove does not name the point: %s", cmd)
	}
}

// A newest point dated in the FUTURE must not stop the store advancing.
//
// The instant comes from the SOURCE's checkpoint, so a source clock two days
// ahead dates every point two days ahead. Read literally, "has enough time
// passed since the newest point" answers no for two days plus the interval: the
// replica takes NO restore point for that whole window, the run reports not_due,
// and nothing else about it looks wrong. That is the same silent starvation the
// per-domain layout was introduced to end, arriving by way of NTP instead.
func TestAFutureDatedPointDoesNotStallTheStore(t *testing.T) {
	p := Policy{Count: 24, Interval: 3 * time.Hour}
	now := time.Unix(1756041600, 0)
	future := now.Add(48 * time.Hour)

	if !Due(future, now, p) {
		t.Error("a newest point dated 48h in the future made the next one not due; the replica would take none until real time caught up, reporting not_due the whole way")
	}
	// Nothing is OWED in that state -- a point is about to be taken -- so the
	// starvation series stays quiet rather than firing alongside.
	if got := OverdueBy(future, now, p); got != 0 {
		t.Errorf("OverdueBy = %v, want 0: Due answers true here, so the point is produced and nothing is outstanding", got)
	}

	// The ordinary cases are unchanged.
	if Due(now, now.Add(time.Hour), p) {
		t.Error("an hour into a three-hour floor is not due")
	}
	if !Due(now, now.Add(3*time.Hour), p) {
		t.Error("exactly at the floor is due")
	}
}

// Overdue is zero on every healthy run and positive only when the floor had
// passed and nothing appeared. This is the series an alert fires on directly, so
// a false positive here is a page in the middle of the night.
func TestOverdueIsZeroOnEveryHealthyShape(t *testing.T) {
	p := Policy{Count: 24, Interval: 3 * time.Hour}
	base := time.Unix(1756041600, 0)

	if got := OverdueBy(base, base.Add(time.Hour), p); got != 0 {
		t.Errorf("an hour into a three-hour floor is not overdue, got %v", got)
	}
	if got := OverdueBy(base, base.Add(3*time.Hour), p); got != 0 {
		t.Errorf("exactly at the floor is due, not overdue, got %v", got)
	}
	if got := OverdueBy(time.Time{}, base, p); got != 0 {
		t.Errorf("a store with no points is reported by its count, not as infinitely overdue, got %v", got)
	}
	if got := OverdueBy(base, base.Add(4*time.Hour), Policy{}); got != 0 {
		t.Errorf("no policy means nothing is owed, got %v", got)
	}
	if got := OverdueBy(base, base.Add(4*time.Hour), p); got != time.Hour {
		t.Errorf("an hour past the floor with no new point is one hour overdue, got %v", got)
	}
	// A pair whose syncs are far apart is the case a threshold derived from the
	// interval gets wrong. Here the floor passed 9 hours ago and no point
	// appeared, which IS overdue -- but only because none appeared: the moment
	// one does, the caller zeroes it.
	if got := OverdueBy(base, base.Add(12*time.Hour), p); got != 9*time.Hour {
		t.Errorf("got %v, want 9h", got)
	}
}
