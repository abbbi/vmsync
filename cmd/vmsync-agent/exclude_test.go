/*
	Copyright (C) 2026  Orsiris de Jong <ozy@netpower.fr>

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

You should have received a copy of the GNU General Public License
along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"vmsync/pkg/libvirtsync"
)

func TestSkipExcludedDomain(t *testing.T) {
	cases := []struct {
		name    string
		exclude string
		skip    bool
	}{
		{"not excluded", "", false},
		{"report-only", libvirtsync.ExcludeReportOnly, true},
		// Never reaches a scan loop -- Scan drops it -- but the helper
		// must not be the thing that decides otherwise if it ever does.
		{"fully-ignored", libvirtsync.ExcludeFullyIgnored, true},
		// A value nobody recognises still excludes, as everywhere else.
		{"a typo", "ful", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := skipExcludedDomain("web01", tc.exclude, "fence check"); got != tc.skip {
				t.Errorf("skipExcludedDomain(%q) = %v, want %v", tc.exclude, got, tc.skip)
			}
		})
	}
}

// EVERY PATH THAT ACTS ON A DOMAIN MUST CONSULT THE EXCLUSION, and there
// is no single chokepoint that can enforce it: a scheduled sync, a fence
// sweep and a control-plane operation reach a domain by three different
// routes. inventory.Scan covers fully-ignored for the two that read a
// scan, but report-only is a per-action decision and an operation names a
// VM directly, bypassing the scan entirely.
//
// So this names the four entry points and insists each one asks. A gate
// deleted in a refactor is otherwise invisible: the feature keeps working
// for every VM nobody excluded, which is almost all of them, and fails
// only on the domains somebody specifically asked vmsync to leave alone.
func TestEveryAgentActionPathConsultsTheExclusion(t *testing.T) {
	// function name -> the file it lives in
	// checkOneFence, NOT sweepFences: the exclusion deliberately sits at
	// the point of ACTION rather than at the top of the sweep. Placed in
	// the sweep it also suppressed the split-brain REPORT and cleared that
	// VM's gauge, so an excluded domain serving the same workload as its
	// peer read as healthy for ever -- report-only means reported.
	gates := map[string]string{
		"checkOneFence":    "fence.go",
		"runOperation":     "opexec.go",
		"buildSyncRequest": "scheduler.go",
		// scanSyncableVMs, not scanCache or syncableVMs: the scan produces
		// both the template-coverable set and the excluded set in one
		// pass, and the other two are wrappers over it -- scanCache adds
		// the scheduler's TTL cache, syncableVMs is a thin accessor. The
		// gate belongs where the domains are actually read, which is also
		// the function the OPERATION path calls: it has no Scheduler to
		// borrow a cache from, and an excluded VM must not be picked up by
		// a default template there either.
		"scanSyncableVMs": "syncable.go",
		// The effective schedule the console renders, which is built from
		// the control plane's list rather than from a scan and so needs
		// its own filter -- an explicit entry for an excluded VM would
		// otherwise be shown as a sync that will run.
		"EffectiveSchedule": "scheduler.go",
	}
	// Any of these counts as asking.
	askers := []string{"skipExcludedDomain", "excludedFromAction", "excludedVMs", "Excluded"}

	for fn, file := range gates {
		t.Run(fn, func(t *testing.T) {
			src, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("read %s: %v", file, err)
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, file, src, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", file, err)
			}
			var body string
			ast.Inspect(f, func(n ast.Node) bool {
				fd, ok := n.(*ast.FuncDecl)
				if !ok || fd.Name.Name != fn || fd.Body == nil {
					return true
				}
				start := fset.Position(fd.Body.Pos()).Offset
				end := fset.Position(fd.Body.End()).Offset
				if start >= 0 && end <= len(src) && start < end {
					body = string(src[start:end])
				}
				return false
			})
			if body == "" {
				t.Fatalf("could not find %s in %s -- it was renamed or moved, and this test is now watching nothing", fn, file)
			}
			for _, a := range askers {
				if strings.Contains(body, a) {
					return
				}
			}
			t.Errorf("%s in %s does not consult the exclusion. Every path that acts on a domain must, because an operator who set exclude expects nothing to happen to it -- and this is the kind of gate whose absence is invisible until it matters. Call one of %v.",
				fn, file, askers)
		})
	}
}
