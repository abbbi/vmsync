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
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"vmsync/pkg/disk"
	"vmsync/pkg/unsupportedqmp"
)

// bitmapCensus is where the owner's requirement lives: a QMP failure must warn
// and never fail a run. None of its branches can be reached from a test through
// a real libvirt connection, which is why the query is injected -- and why
// every failure mode below is a table row rather than a hardware run.
//
// The reply fixture is a REAL query-named-block-nodes answer, trimmed to the
// two format nodes that carry bitmaps. The paths are the capture host's,
// because the mapping under test is string equality against QcowDisk.Source.

const (
	cVda  = "/home/claude/vmsync-bench/probe/vmsync-fixture-probe-1.qcow2"
	cVdb  = "/home/claude/vmsync-bench/probe/vmsync-fixture-probe-2.qcow2"
	cName = "vmsync-cpt-2611040000-beef-000001"
)

func censusDisks() []disk.QcowDisk {
	return []disk.QcowDisk{
		{TargetDev: "vda", Source: cVda, RootSource: cVda},
		{TargetDev: "vdb", Source: cVdb, RootSource: cVdb},
	}
}

// A reply carrying one vmsync bitmap on each disk, shaped exactly as qemu
// sends it.
func censusReply(extra string) string {
	return `{"return":[` +
		`{"node-name":"libvirt-1-format","drv":"qcow2","file":"` + cVdb + `",` +
		`"dirty-bitmaps":[{"name":"` + cName + `","recording":true,"persistent":true,"busy":false}]},` +
		`{"node-name":"libvirt-1-storage","drv":"file","file":"` + cVdb + `"},` +
		`{"node-name":"libvirt-2-format","drv":"qcow2","file":"` + cVda + `",` +
		`"dirty-bitmaps":[{"name":"` + cName + `","recording":true,"persistent":true,"busy":false}]},` +
		`{"node-name":"libvirt-2-storage","drv":"file","file":"` + cVda + `"}` +
		extra + `],"id":"libvirt-15"}`
}

func baseInput(query func() (string, error)) censusInput {
	return censusInput{
		Domain:   "web01",
		Disks:    censusDisks(),
		FromFile: map[string][]string{},
		Active:   true,
		Query:    query,
	}
}

func okQuery(reply string) func() (string, error) {
	return func() (string, error) { return reply, nil }
}

// THE RESULT TYPE CARRIES NO ERROR, and that is the non-fatality guarantee.
//
// Not a style point. With no error field there is no value a future caller can
// propagate into a failed run and no `if err != nil` for somebody to attach one
// to. A test rather than a comment, because adding a field is one line and
// every other test here would still pass.
func TestTheCensusResultHasNoErrorToPropagate(t *testing.T) {
	rt := reflect.TypeOf(censusResult{})
	errIface := reflect.TypeOf((*error)(nil)).Elem()
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if f.Type == errIface || f.Type.Implements(errIface) {
			t.Errorf("censusResult.%s is an error. The owner's requirement is that this route warns and never fails a run; an error in the result is how that stops being true, because the next caller will return it", f.Name)
		}
	}
}

// A SHUT-DOWN SOURCE IS NOT ASKED, and not warned about either.
//
// Its image is closed, so the qcow2 directory is already a complete answer and
// there is nothing a census could add -- and nothing worth marking the domain
// tainted for. Asking anyway would taint a domain to learn what vmsync already
// knows.
func TestAShutDownSourceIsNotAsked(t *testing.T) {
	asked := false
	in := baseInput(func() (string, error) { asked = true; return censusReply(""), nil })
	in.Active = false
	in.FromFile = map[string][]string{cVda: {cName}}

	res := bitmapCensus(in)

	if asked {
		t.Error("asked qemu about a shut-down source. Its image is closed, so the file read already saw everything, and the ask costs a custom-monitor taint for nothing")
	}
	if !reflect.DeepEqual(res.Union, in.FromFile) {
		t.Errorf("Union = %v, want the image read unchanged %v", res.Union, in.FromFile)
	}
	if len(res.Notes) != 0 {
		t.Errorf("said something about a state that needs nothing said: %+v", res.Notes)
	}
	if res.Proven {
		t.Error("Proven is true though qemu was never asked. Proven means qemu answered; the caller uses it to stop saying its answer is incomplete")
	}
}

// AN UNREADABLE STATE IS SAID, and still not asked.
//
// The distinction from the shut-down case is the whole point: there vmsync
// knows the file read is complete, here it does not know anything. Sharing one
// silent branch would make "I checked" and "I could not check" the same
// outcome, which is this codebase's recurring defect.
func TestAnUnreadableStateIsSaidAndNotAsked(t *testing.T) {
	asked := false
	in := baseInput(func() (string, error) { asked = true; return censusReply(""), nil })
	in.Active, in.ActiveErr = false, errors.New("libvirt: domain lookup failed")

	res := bitmapCensus(in)

	if asked {
		t.Error("asked qemu without knowing whether the domain is running")
	}
	if len(res.Notes) != 1 || res.Notes[0].Level != censusWarn {
		t.Fatalf("want exactly one warning, got %+v", res.Notes)
	}
	if !strings.Contains(res.Notes[0].Text, "libvirt: domain lookup failed") {
		t.Errorf("the note drops the underlying error, so nobody can tell why the state was unreadable: %q", res.Notes[0].Text)
	}
	if res.Proven {
		t.Error("Proven is true with the state unreadable")
	}
}

// EVERY FAILURE LEAVES THE IMAGE READ EXACTLY AS IT WAS.
//
// The error strings are the real ones, captured from libvirt 12.0.0 — a
// read-only connection, a qemu too old for the query, an inactive domain and a
// reply that is not JSON. Each must produce one warning and a Union
// byte-identical to what the file read gave.
func TestEveryFailureLeavesTheImageReadUntouched(t *testing.T) {
	fromFile := map[string][]string{cVda: {"vmsync-cpt-2510010000-aaaa-000001"}}

	for _, tc := range []struct {
		name  string
		query func() (string, error)
	}{
		{"read-only connection", func() (string, error) {
			return "", errors.New("operation forbidden: read only access prevents virDomainQemuMonitorCommand")
		}},
		{"domain not running", func() (string, error) {
			return "", errors.New("Requested operation is not valid: domain is not running")
		}},
		{"query unsupported by this qemu", func() (string, error) {
			return `{"id":"libvirt-19","error":{"class":"CommandNotFound","desc":"The command query-named-block-nodes has not been found"}}`, nil
		}},
		{"reply is not JSON", func() (string, error) { return "unexpected monitor output", nil }},
		{"reply has no return key", func() (string, error) { return `{"id":"libvirt-3"}`, nil }},
		{"no block nodes at all", func() (string, error) { return `{"return":[],"id":"libvirt-4"}`, nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInput(tc.query)
			in.FromFile = fromFile

			res := bitmapCensus(in)

			if !reflect.DeepEqual(res.Union, fromFile) {
				t.Errorf("Union = %v, want the image read unchanged %v. A failed census must never change what the run acts on", res.Union, fromFile)
			}
			if res.Proven {
				t.Error("Proven is true after a failure, so the caller would stop warning that its answer is incomplete")
			}
			if len(res.Notes) == 0 {
				t.Error("failed silently. A census that did not happen must not read like one that found nothing")
			}
			for _, n := range res.Notes {
				if n.Level != censusWarn {
					t.Errorf("a failure was reported at Info: %q. WarningCount feeds the metrics textfile and the bench greps warning prose; Info is below where either looks", n.Text)
				}
			}
		})
	}
}

// THE CENSUS WIDENS, and the widening is the fix.
func TestTheCensusWidensTheImageRead(t *testing.T) {
	in := baseInput(okQuery(censusReply("")))
	in.FromFile = map[string][]string{cVda: {"vmsync-cpt-2510010000-aaaa-000001"}}

	res := bitmapCensus(in)

	if !res.Proven {
		t.Fatalf("a complete answer about both disks is not Proven; Notes=%+v", res.Notes)
	}
	if !slices.Contains(res.Union[cVda], "vmsync-cpt-2510010000-aaaa-000001") {
		t.Errorf("the image read's own bitmap is missing from the union: %v", res.Union)
	}
	if !slices.Contains(res.Union[cVda], cName) || !slices.Contains(res.Union[cVdb], cName) {
		t.Errorf("qemu's bitmaps did not reach the union: %v. This is the whole feature -- without it the orphan stays invisible on a running source", res.Union)
	}
	if !slices.Contains(res.FromQemu[cVda], cName) {
		t.Errorf("FromQemu does not carry qemu's answer alone: %v. The post-clear reader needs it, because the file would report a cleared orphan as a survivor", res.FromQemu)
	}
}

// THE UNION NEVER SHRINKS, across every branch. The one invariant that keeps
// the supported path independent of the unsupported one.
func TestTheUnionNeverShrinksWhateverHappens(t *testing.T) {
	fromFile := map[string][]string{
		cVda: {"vmsync-cpt-2510010000-aaaa-000001", "vmsync-cpt-2510010000-bbbb-000002"},
		cVdb: {"vmsync-cpt-2510010000-cccc-000003"},
	}
	queries := map[string]func() (string, error){
		"success":       okQuery(censusReply("")),
		"error":         func() (string, error) { return "", errors.New("refused") },
		"garbage":       func() (string, error) { return "{", nil },
		"empty reply":   func() (string, error) { return `{"return":[]}`, nil },
		"alien bitmaps": okQuery(`{"return":[{"node-name":"n","drv":"qcow2","file":"/elsewhere.qcow2","dirty-bitmaps":[{"name":"vmsync-cpt-x-1","persistent":true}]}]}`),
	}
	for name, q := range queries {
		t.Run(name, func(t *testing.T) {
			in := baseInput(q)
			in.FromFile = fromFile
			res := bitmapCensus(in)
			for p, names := range fromFile {
				for _, n := range names {
					if !slices.Contains(res.Union[p], n) {
						t.Errorf("%s/%s was dropped from the union. Deleting this whole route must never make vmsync act on less than the image read found", p, n)
					}
				}
			}
		})
	}
}

// A DIFFERENCE AGAINST A SET NOBODY READ IS NOT A DIFFERENCE.
func TestTheDisagreementNotesNeedBothReaders(t *testing.T) {
	t.Run("the image read failed", func(t *testing.T) {
		in := baseInput(okQuery(censusReply("")))
		in.FileErr = errors.New("inspect /x.qcow2: permission denied")
		res := bitmapCensus(in)
		for _, n := range res.Notes {
			if strings.Contains(n.Text, "do not carry yet") || strings.Contains(n.Text, "qemu does not hold") {
				t.Errorf("reported a disagreement against a set that was never read: %q. Every bitmap would read as qemu-only", n.Text)
			}
		}
	})
	t.Run("the census was partial", func(t *testing.T) {
		// A node whose file matches nothing makes the census unproven.
		in := baseInput(okQuery(censusReply(`,{"node-name":"libvirt-9-format","drv":"qcow2","file":"/stray.qcow2","dirty-bitmaps":[{"name":"` + cName + `","persistent":true}]}`)))
		in.FromFile = map[string][]string{cVda: {"vmsync-cpt-2510010000-aaaa-000001"}}
		res := bitmapCensus(in)
		if res.Proven {
			t.Fatal("a census with an unplaceable node is Proven; the rest of this case is vacuous")
		}
		for _, n := range res.Notes {
			if strings.Contains(n.Text, "qemu does not hold") {
				t.Errorf("called a bitmap file-only on the strength of a partial census: %q", n.Text)
			}
		}
	})
}

// THE EXPECTED STATE IS INFO AND CARRIES NO REMEDY; the one that needs
// downtime is a WARNING and carries the offline recipe.
func TestTheTwoDisagreementsReadDifferently(t *testing.T) {
	t.Run("qemu-only is an observation", func(t *testing.T) {
		note := qmpOnlyNote("web01", map[string][]string{cVda: {cName}})
		if note == "" {
			t.Fatal("said nothing about bitmaps only qemu holds, which is the ordinary state on a running source and the reason this route exists")
		}
		for _, forbidden := range []string{"virsh shutdown", "qemu-img bitmap --remove"} {
			if strings.Contains(note, forbidden) {
				t.Errorf("offers %q for a state this run is already fixing. A report that sends an operator to take downtime they do not need stops being read", forbidden)
			}
		}
	})
	t.Run("file-only needs the offline recipe", func(t *testing.T) {
		report := fileOnlyReport("web01", map[string][]string{cVda: {cName}})
		if report == "" {
			t.Fatal("said nothing about a bitmap the online route cannot remove")
		}
		for _, want := range []string{"qemu-img bitmap --remove -f qcow2", "virsh shutdown", cName, cVda} {
			if !strings.Contains(report, want) {
				t.Errorf("the report never mentions %q, so it names a problem without the one remedy that works:\n\n%s", want, report)
			}
		}
		if !strings.Contains(report, "qemu HOLDS") && !strings.Contains(report, "qemu does not hold") {
			t.Errorf("the report does not explain why adopt-and-delete cannot help here:\n\n%s", report)
		}
	})
	if qmpOnlyNote("web01", nil) != "" || fileOnlyReport("web01", nil) != "" {
		t.Error("said something when there was no disagreement")
	}
}

// A BITMAP QEMU HOLDS THAT VMSYNC WILL NOT TOUCH IS ALWAYS NAMED.
func TestEveryUnclaimedReasonIsExplained(t *testing.T) {
	for _, reason := range []string{
		unsupportedqmp.ReasonBacking,
		unsupportedqmp.ReasonNoTarget,
		unsupportedqmp.ReasonNoFile,
		unsupportedqmp.ReasonUnknown,
	} {
		t.Run(reason, func(t *testing.T) {
			r := unclaimedReport("web01", []unsupportedqmp.UnclaimedNode{
				{NodeName: "libvirt-7-format", File: "/some/file.qcow2", Reason: reason, Bitmaps: []string{cName}},
			})
			if r == "" {
				t.Fatalf("%s was dropped silently. A bitmap qemu holds that vmsync has decided not to remove is exactly what an operator needs told", reason)
			}
			for _, want := range []string{cName, "libvirt-7-format", "/some/file.qcow2"} {
				if !strings.Contains(r, want) {
					t.Errorf("the line does not name %q:\n\n%s", want, r)
				}
			}
			if len(r) < 120 {
				t.Errorf("%s is named but not explained, so the reader cannot tell whether to act:\n\n%s", reason, r)
			}
		})
	}
	if unclaimedReport("web01", nil) != "" {
		t.Error("said something with nothing unclaimed")
	}
}

// QEMU'S ANSWER IS KEPT SEPARATE, AND THE TWO DISAGREEMENTS ARE LEVELLED
// DIFFERENTLY.
//
// Three one-line mutations hide here that every other test in this file
// survives. Setting FromQemu to the union instead of qemu's own list reads the
// same in every assertion about the union -- and hands the post-clear reader
// the file's view back, reintroducing the false positive it exists to avoid.
// Swapping the two OnlyIn calls produces both notes with their contents
// exchanged, which no test of either report's wording in isolation can see.
// And logging the expected state as a Warning rather than Info puts a routine
// observation into WarningCount, where the metrics textfile and the bench both
// read it as a fault.
func TestQemusAnswerIsKeptSeparateAndTheNotesAreLevelled(t *testing.T) {
	const fileOnly = "vmsync-cpt-2510010000-aaaa-000001"
	in := baseInput(okQuery(censusReply("")))
	in.FromFile = map[string][]string{cVda: {fileOnly}}

	res := bitmapCensus(in)
	if !res.Proven {
		t.Fatalf("this case needs a proven census for the disagreement notes to fire; Notes=%+v", res.Notes)
	}

	if slices.Contains(res.FromQemu[cVda], fileOnly) {
		t.Errorf("FromQemu carries %q, which only the image read found: %v.\n\nThe post-clear reader takes FromQemu precisely because the file lags what qemu holds -- a bitmap qemu has let go of stays in the qcow2 until the image closes. Handing it the union puts that stale name back and reports a cleared orphan as a survivor.", fileOnly, res.FromQemu)
	}

	var info, warn []string
	for _, n := range res.Notes {
		switch n.Level {
		case censusInfo:
			info = append(info, n.Text)
		case censusWarn:
			warn = append(warn, n.Text)
		}
	}

	// The bitmap only qemu holds is the ordinary state: Info, and it must be
	// the name that appears there rather than the file-only one.
	if !anyContains(info, cName) {
		t.Errorf("the bitmap only qemu holds (%s) is not reported at Info level; Info=%v Warn=%v", cName, info, warn)
	}
	if anyContains(info, fileOnly) {
		t.Errorf("%q came only from the image read, so reporting it as something qemu holds has the two differences the wrong way round; Info=%v", fileOnly, info)
	}

	// The bitmap only the image carries needs downtime: Warning.
	if !anyContains(warn, fileOnly) {
		t.Errorf("the bitmap only the image carries (%s) is not reported at Warning level; the online route cannot remove it and nothing told the operator; Warn=%v", fileOnly, warn)
	}
	if anyContains(warn, cName) {
		t.Errorf("%q is in the set this run clears, so warning about it sends an operator to take downtime for a state the run is already fixing; Warn=%v", cName, warn)
	}
}

// ONLYQEMU IS A DECISION INPUT, NOT A MESSAGE, so its value is pinned
// separately from the note built out of it.
//
// clearFailureIsFatal forgives a clearing failure only over a bitmap this
// route made newly visible. Setting OnlyQemu to everything qemu reported
// leaves every note in this file reading correctly -- a shared name is still
// absent from the file-only report, and still present in the qemu-only one --
// while quietly moving a bitmap the image read ALSO found into the forgiven
// set. The result is a rebuild that stops refusing over a failure every
// previous version refused over, with no test and no log line changed.
func TestOnlyQemuExcludesWhatTheImageReadAlsoFound(t *testing.T) {
	in := baseInput(okQuery(censusReply("")))
	// censusReply puts cName on BOTH disks. Naming it in the image read for
	// vda only makes it shared there and qemu-only on vdb, so one call covers
	// both directions.
	in.FromFile = map[string][]string{cVda: {cName}}

	res := bitmapCensus(in)

	if slices.Contains(res.OnlyQemu[cVda], cName) {
		t.Errorf("OnlyQemu[vda] = %v and includes %q, which the image read found too.\n\nThat name is what clearFailureIsFatal consults: calling it newly visible makes a clearing failure over it stop refusing the rebuild, which every previous version of vmsync did. Nothing else in this file notices, because the notes read the same either way.",
			res.OnlyQemu[cVda], cName)
	}
	if !slices.Contains(res.OnlyQemu[cVdb], cName) {
		t.Errorf("OnlyQemu[vdb] = %v and is missing %q, which only qemu reported. Without it a failure over a bitmap this route just made visible would refuse a rebuild that used to succeed", res.OnlyQemu[cVdb], cName)
	}
}

// AND THE CENSUS MUST ACTUALLY SAY IT.
//
// EveryUnclaimedReasonIsExplained tests the report function; this tests that
// bitmapCensus calls it. Deleting the call site leaves that test green, the
// Unclaimed field still populated, and the warning gone -- a bitmap qemu holds
// that vmsync silently declines to touch, which is the exact shape of defect
// this feature exists to remove.
func TestTheCensusReportsWhatItWouldNotClaim(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply string
		disks []disk.QcowDisk
		names []string
	}{
		{
			name:  "a file this domain does not manage",
			reply: censusReply(`,{"node-name":"libvirt-9-format","drv":"qcow2","file":"/stray/disk.qcow2","dirty-bitmaps":[{"name":"vmsync-cpt-2509010000-dead-000007","persistent":true}]}`),
			disks: censusDisks(),
			names: []string{"vmsync-cpt-2509010000-dead-000007", "/stray/disk.qcow2", "libvirt-9-format"},
		},
		{
			name: "a disk the XML gives no target dev",
			reply: `{"return":[{"node-name":"libvirt-1-format","drv":"qcow2","file":"/notarget.qcow2",` +
				`"dirty-bitmaps":[{"name":"vmsync-cpt-2509010000-dead-000008","persistent":true}]}]}`,
			disks: []disk.QcowDisk{{TargetDev: "", Source: "/notarget.qcow2", RootSource: "/notarget.qcow2"}},
			names: []string{"vmsync-cpt-2509010000-dead-000008", "<target dev>"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInput(okQuery(tc.reply))
			in.Disks = tc.disks

			res := bitmapCensus(in)

			var warn []string
			for _, n := range res.Notes {
				if n.Level == censusWarn {
					warn = append(warn, n.Text)
				}
			}
			for _, want := range tc.names {
				if !anyContains(warn, want) {
					t.Errorf("no warning mentions %q, so qemu holds a bitmap vmsync has decided not to touch and nothing says so.\n\nNotes=%+v", want, res.Notes)
				}
			}
			if res.Proven {
				t.Error("Proven is true with something unclaimed")
			}
		})
	}
}

func anyContains(texts []string, needle string) bool {
	for _, t := range texts {
		if strings.Contains(t, needle) {
			return true
		}
	}
	return false
}

func TestABusyBitmapIsSetAsideAndSaid(t *testing.T) {
	in := baseInput(okQuery(`{"return":[{"node-name":"libvirt-1-format","drv":"qcow2","file":"` + cVdb + `",` +
		`"dirty-bitmaps":[{"name":"` + cName + `","recording":true,"persistent":true,"busy":true}]}]}`))

	res := bitmapCensus(in)

	if slices.Contains(res.Union[cVdb], cName) {
		t.Error("admitted a busy bitmap into the set the run clears. A backup job is using it, and removing it leaves that job describing something gone")
	}
	var said bool
	for _, n := range res.Notes {
		if strings.Contains(n.Text, "busy") {
			said = true
		}
	}
	if !said {
		t.Errorf("set a busy bitmap aside without saying so; the next run meets it again and nothing explains the skip: %+v", res.Notes)
	}
	if res.Proven {
		t.Error("Proven is true with a bitmap set aside")
	}
}

func TestAnUncoveredDiskIsSaid(t *testing.T) {
	in := baseInput(okQuery(`{"return":[{"node-name":"libvirt-1-format","drv":"qcow2","file":"` + cVdb + `",` +
		`"dirty-bitmaps":[{"name":"` + cName + `","persistent":true}]}]}`))

	res := bitmapCensus(in)

	var said bool
	for _, n := range res.Notes {
		if strings.Contains(n.Text, cVda) {
			said = true
		}
	}
	if !said {
		t.Errorf("qemu described no node for vda and the run said nothing about it, so a symlinked <source file> would read as a disk with no bitmaps: %+v", res.Notes)
	}
	if res.Proven {
		t.Error("Proven is true with a managed disk uncovered")
	}
}
