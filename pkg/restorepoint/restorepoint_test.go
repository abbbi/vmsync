package restorepoint

import (
	"strings"
	"testing"
	"time"
)

func mustTag(t *testing.T, secs int64, checkpoint string) Tag {
	t.Helper()
	tag, err := NewTag(time.Unix(secs, 0), checkpoint)
	if err != nil {
		t.Fatalf("NewTag(%d, %q): %v", secs, checkpoint, err)
	}
	return tag
}

func TestParsePolicy(t *testing.T) {
	for _, tc := range []struct {
		in       string
		want     Policy
		wantErr  bool
		whyValid string
	}{
		{in: "24,3h", want: Policy{Count: 24, Interval: 3 * time.Hour}},
		{in: " 24 , 3h ", want: Policy{Count: 24, Interval: 3 * time.Hour}, whyValid: "surrounding space is an operator typo, not a syntax error"},
		{in: "24,3H", want: Policy{Count: 24, Interval: 3 * time.Hour}, whyValid: "Go's duration parser is lowercase-only; the documented example must not fail"},
		{in: "24,90m", want: Policy{Count: 24, Interval: 90 * time.Minute}},
		{in: "1,0", want: Policy{Count: 1, Interval: 0}, whyValid: "a zero interval means every sync"},
		{in: "0,3h", want: Policy{Count: 0, Interval: 3 * time.Hour}, whyValid: "count zero disables the feature"},
		{in: "", want: Policy{}, whyValid: "flag not passed"},

		{in: "24", wantErr: true},
		{in: "24,", wantErr: true},
		{in: ",3h", wantErr: true},
		{in: "twenty,3h", wantErr: true},
		{in: "24,3 hours", wantErr: true},
		{in: "-1,3h", wantErr: true},
		{in: "24,-3h", wantErr: true},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParsePolicy(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParsePolicy(%q) = %v, want an error", tc.in, got)
				}
				if !strings.Contains(err.Error(), "-retention") {
					t.Errorf("the error does not name the flag it is about: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParsePolicy(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParsePolicy(%q) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestPolicyEnabled(t *testing.T) {
	if (Policy{Count: 0, Interval: time.Hour}).Enabled() {
		t.Error("a count of zero must disable the feature no matter what the interval says")
	}
	if !(Policy{Count: 1}).Enabled() {
		t.Error("a count of one is enabled")
	}
}

// The instant has to lead, or restore points collide: -reinit restarts the
// source chain at vmsync-cpt-000001, so the same checkpoint name recurs.
func TestTagRoundTrip(t *testing.T) {
	for _, name := range []string{"vmsync-cpt-000042", "vmsync-cpt-000001", "a", "a.b_c-d"} {
		tag := mustTag(t, 1756041600, name)
		back, err := ParseTag(tag.String())
		if err != nil {
			t.Fatalf("ParseTag(%q): %v", tag.String(), err)
		}
		if !back.At.Equal(tag.At) || back.Checkpoint != tag.Checkpoint {
			t.Errorf("round trip of %q gave %+v", tag.String(), back)
		}
	}
}

func TestTagLeadsWithTheInstant(t *testing.T) {
	tag := mustTag(t, 1756041600, "vmsync-cpt-000042")
	if got, want := tag.String(), "1756041600-vmsync-cpt-000042"; got != want {
		t.Errorf("Tag.String() = %q, want %q", got, want)
	}
	// Two reinit generations reuse the checkpoint name; only the instant
	// separates them.
	older := mustTag(t, 1756041600, "vmsync-cpt-000001")
	newer := mustTag(t, 1756052400, "vmsync-cpt-000001")
	if older.String() == newer.String() {
		t.Error("two generations of vmsync-cpt-000001 produced the same directory name")
	}
}

// A tag becomes a directory name that is interpolated into rm -rf. Nothing
// that could escape it may become one.
func TestTagRefusesAnythingThatCouldEscapeAPath(t *testing.T) {
	for _, bad := range []string{
		"", "..", ".", "../../etc", "a/b", "a b", "a;rm -rf /", "a'b", "a$b", "a`b`",
		"a\nb", "a*", ".hidden", "-leading-dash",
	} {
		t.Run(bad, func(t *testing.T) {
			if _, err := NewTag(time.Unix(1756041600, 0), bad); err == nil {
				t.Errorf("NewTag accepted %q as a checkpoint name", bad)
			}
		})
	}
}

func TestParseTagRejectsJunk(t *testing.T) {
	for _, bad := range []string{
		"", "nounderscore", "notanumber-vmsync-cpt-1", "1756041600", "1756041600-",
		"1756041600-../escape", "-1-x",
	} {
		t.Run(bad, func(t *testing.T) {
			if got, err := ParseTag(bad); err == nil {
				t.Errorf("ParseTag(%q) = %+v, want an error", bad, got)
			}
		})
	}
}

func TestNewTagRefusesAZeroInstant(t *testing.T) {
	if _, err := NewTag(time.Time{}, "vmsync-cpt-000001"); err == nil {
		t.Error("accepted a tag with no checkpoint time; it would sort before every real one forever")
	}
}

// The layout has to stay clear of <disk>_* beside the replica, because
// cmd/vmsync/failover.go globs exactly that to detect an uncommitted overlay
// and a match blocks promotion.
func TestLayoutStaysOutOfThePromotionGlob(t *testing.T) {
	disk := "/data/replicas/web01-disk0.qcow2"
	root := Root(disk)
	if got, want := root, "/data/replicas/.vmsync-rp"; got != want {
		t.Fatalf("Root(%q) = %q, want %q", disk, got, want)
	}

	tag := mustTag(t, 1756041600, "vmsync-cpt-000042")
	for _, p := range []string{
		Dir(root, tag),
		StagingDir(root, tag),
		DiskPath(Dir(root, tag), disk),
	} {
		if !strings.HasPrefix(p, root+"/") {
			t.Errorf("%q escapes the restore point directory", p)
		}
		// The glob is <disk>_* in the disk's OWN directory. Anything one
		// level down cannot match it, which is the property being pinned.
		if strings.HasPrefix(p, disk+"_") {
			t.Errorf("%q would be read as an uncommitted overlay and block promotion", p)
		}
	}
}

func TestDiskPathKeepsTheBasename(t *testing.T) {
	tag := mustTag(t, 1756041600, "vmsync-cpt-000042")
	root := Root("/data/replicas/web01-disk0.qcow2")
	got := DiskPath(Dir(root, tag), "/data/replicas/web01-disk0.qcow2")
	want := "/data/replicas/.vmsync-rp/1756041600-vmsync-cpt-000042/web01-disk0.qcow2"
	if got != want {
		t.Errorf("DiskPath = %q, want %q", got, want)
	}
}

// The interval is a floor, not a schedule: vmsync does not choose when it
// runs, so this can only answer "has enough time passed".
func TestDue(t *testing.T) {
	base := time.Unix(1756041600, 0)
	p := Policy{Count: 24, Interval: 3 * time.Hour}

	for name, tc := range map[string]struct {
		latest time.Time
		now    time.Time
		policy Policy
		want   bool
	}{
		"none yet":                     {time.Time{}, base, p, true},
		"an hour after the last":       {base, base.Add(time.Hour), p, false},
		"exactly the interval":         {base, base.Add(3 * time.Hour), p, true},
		"a moment short of it":         {base, base.Add(3*time.Hour - time.Second), p, false},
		"long overdue after a pause":   {base, base.Add(30 * time.Hour), p, true},
		"zero interval means always":   {base, base, Policy{Count: 4}, true},
		"disabled is never due":        {time.Time{}, base, Policy{}, false},
		"disabled even with a history": {base, base.Add(99 * time.Hour), Policy{Interval: time.Hour}, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := Due(tc.latest, tc.now, tc.policy); got != tc.want {
				t.Errorf("Due = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPruneKeepsTheNewest(t *testing.T) {
	var tags []Tag
	for i := 0; i < 6; i++ {
		tags = append(tags, mustTag(t, 1756041600+int64(i)*3600, "vmsync-cpt-00000"+string(rune('1'+i))))
	}
	// Deliberately unsorted going in.
	shuffled := []Tag{tags[3], tags[0], tags[5], tags[1], tags[4], tags[2]}

	plan := Prune(shuffled, Policy{Count: 3, Interval: time.Hour})
	if len(plan.Keep) != 3 || len(plan.Remove) != 3 {
		t.Fatalf("kept %d, removed %d, want 3 and 3", len(plan.Keep), len(plan.Remove))
	}
	if !plan.Keep[0].At.Equal(tags[5].At) {
		t.Errorf("Keep is not newest-first: %v", plan.Keep[0])
	}
	if !plan.Remove[0].At.Equal(tags[0].At) {
		t.Errorf("Remove is not oldest-first: an interrupted prune should drop the least useful history first, got %v", plan.Remove[0])
	}
	for _, k := range plan.Keep {
		for _, r := range plan.Remove {
			if k.String() == r.String() {
				t.Fatalf("%q is in both Keep and Remove", k)
			}
		}
	}
}

func TestPruneUnderCount(t *testing.T) {
	tags := []Tag{
		mustTag(t, 1756041600, "vmsync-cpt-000001"),
		mustTag(t, 1756045200, "vmsync-cpt-000002"),
	}
	plan := Prune(tags, Policy{Count: 24, Interval: time.Hour})
	if len(plan.Remove) != 0 {
		t.Errorf("removed %v while under the retention count", plan.Remove)
	}
	if len(plan.Keep) != 2 {
		t.Errorf("kept %d of 2", len(plan.Keep))
	}
}

func TestPruneWithRetentionOffReportsEverythingAsLeftovers(t *testing.T) {
	tags := []Tag{mustTag(t, 1756041600, "vmsync-cpt-000001")}
	plan := Prune(tags, Policy{})
	if len(plan.Keep) != 0 || len(plan.Remove) != 1 {
		t.Errorf("with retention off, kept %v removed %v; want nothing kept", plan.Keep, plan.Remove)
	}
}

func TestPruneEmpty(t *testing.T) {
	plan := Prune(nil, Policy{Count: 24, Interval: time.Hour})
	if len(plan.Keep) != 0 || len(plan.Remove) != 0 {
		t.Errorf("Prune(nil) = %+v, want empty", plan)
	}
}

func TestLatest(t *testing.T) {
	if got := Latest(nil); !got.IsZero() {
		t.Errorf("Latest(nil) = %v, want the zero time so Due treats it as none yet", got)
	}
	tags := []Tag{
		mustTag(t, 1756045200, "vmsync-cpt-000002"),
		mustTag(t, 1756041600, "vmsync-cpt-000001"),
	}
	if got := Latest(tags); got.Unix() != 1756045200 {
		t.Errorf("Latest = %v, want the newest", got)
	}
}

func TestStatusRoundTrip(t *testing.T) {
	s := Status{
		Checkpoint:   "vmsync-cpt-000042",
		CheckpointAt: 1756041600,
		TakenAt:      1756041605,
		Source:       "hyper01:web01",
		Verify:       VerifyNotRun,
		Disks:        []string{"web01-disk0.qcow2", "web01-disk1.qcow2"},
	}
	b, err := s.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !strings.HasSuffix(string(b), "\n") {
		t.Error("the sidecar does not end in a newline; an operator will be cat-ing this")
	}
	back, err := DecodeStatus(b)
	if err != nil {
		t.Fatalf("DecodeStatus: %v", err)
	}
	if back.Checkpoint != s.Checkpoint || back.Verify != s.Verify || len(back.Disks) != 2 {
		t.Errorf("round trip gave %+v", back)
	}
}

// "not run" is the common case and has to be distinguishable from "passed" --
// a restore point is taken before -verify, so most carry no verdict at all.
func TestVerifyStatesAreDistinct(t *testing.T) {
	seen := map[string]bool{}
	for _, v := range []string{VerifyNotRun, VerifyPassed, VerifyFailed} {
		if v == "" {
			t.Error("a verify state is empty, which would be indistinguishable from an unset field")
		}
		if seen[v] {
			t.Errorf("duplicate verify state %q", v)
		}
		seen[v] = true
	}
}
