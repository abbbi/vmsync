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
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"
)

// THE TWO DOORS, AND WHY HANDING A DIRECTORY TO THE WRONG ONE IS SILENT.
//
// newRecorder takes a DISK PATH and strips the last element, because the
// journal lives beside a domain's disks. newRecorderInDir takes the
// DIRECTORY. Give the directory to newRecorder and the journal lands one
// level too high -- beside another machine's disks, and nowhere any reader
// looks -- with no error, because a path is a path.
//
// This is asserted by writing a record and finding the file, rather than by
// comparing strings, so it covers the sink as well as the path arithmetic.
func TestTheJournalLandsBesideTheDisksNotAboveThem(t *testing.T) {
	ctx := context.Background()
	cfg := syncConfig{Journal: "", TargetDomain: "web01"}

	t.Run("the directory-taking door puts it inside", func(t *testing.T) {
		dir := t.TempDir()
		rec := newRecorderInDir(cfg, nil, dir, "web01")
		if rec == nil {
			t.Fatal("no recorder was built")
		}
		rec.Intent(ctx, journalVerbBreakTargetLock, nil)

		want := filepath.Join(dir, ".vmsync-journal", "web01.jsonl")
		if _, err := os.Stat(want); err != nil {
			t.Errorf("no journal at %s: %v -- a record written anywhere else is one -explain-domain cannot find", want, err)
		}
	})

	t.Run("the disk-path door strips a level, which is why it is the wrong one for a directory", func(t *testing.T) {
		parent := t.TempDir()
		dir := filepath.Join(parent, "replicas")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		rec := newRecorder(cfg, nil, dir, "web01")
		if rec == nil {
			t.Fatal("no recorder was built")
		}
		rec.Intent(ctx, journalVerbBreakTargetLock, nil)

		// It lands beside the replica DIRECTORY rather than inside it.
		// Pinned so the hazard stays demonstrated rather than asserted.
		above := filepath.Join(parent, ".vmsync-journal", "web01.jsonl")
		inside := filepath.Join(dir, ".vmsync-journal", "web01.jsonl")
		if _, err := os.Stat(above); err != nil {
			t.Errorf("expected the disk-path door to write one level up (at %s), which is the mistake this pair of functions exists to keep apart: %v", above, err)
		}
		if _, err := os.Stat(inside); err == nil {
			t.Errorf("newRecorder wrote inside %s, so the two doors no longer differ and nothing stops a directory being handed to either", dir)
		}
	})
}

// -break-target-lock must not reach for the disk-path door again.
//
// It did, with cfg.TargetDiskPath -- a directory -- so its record landed
// one level above the replica. And because the documented invocation
// (docs/RUNBOOK.md) passes no -target-disk-path at all, the ordinary case
// wrote nothing: the verb whose own comment calls its record "the first
// thing anybody investigating a two-writer incident will look for" left
// none.
//
// Checked as a property of the source because the fixed path needs a
// libvirt connection to find the domain's disks, which no unit test has.
func TestBreakTargetLockUsesItsOwnRecorder(t *testing.T) {
	const file = "breaklock.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "runBreakTargetLock" {
			fn = fd
			break
		}
	}
	if fn == nil {
		t.Fatal("no func runBreakTargetLock in " + file)
	}

	used := map[string]int{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok {
			used[id.Name]++
		}
		return true
	})

	if used["newRecorder"] > 0 {
		t.Errorf("runBreakTargetLock calls newRecorder, which takes a DISK PATH and strips the last element. Handed -target-disk-path -- a directory -- it writes the journal one level above the replica, where no other verb writes and -explain-domain does not read. Use newBreakLockRecorder, which derives the directory from the domain itself.")
	}
	if used["newBreakLockRecorder"] != 1 {
		t.Errorf("runBreakTargetLock calls newBreakLockRecorder %d times, want exactly 1 -- without it this verb leaves no record of a lock break, which is the one action whose record an investigation starts from", used["newBreakLockRecorder"])
	}
}
