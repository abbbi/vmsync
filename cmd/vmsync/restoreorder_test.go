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
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// WHY THIS IS READ RATHER THAN EXECUTED: the same reason runorder_test.go
// and stamporder_test.go give for run(). runRestoreRestorePoint opens a
// libvirt connection, an SSH connection and a run lock before it does
// anything a test could observe, so no unit test can call it -- and the
// property at stake is an ORDER, which is a property of the source.
//
// THE PROPERTY. The state checks that can refuse a restore outright --
// the domain is running, its role forbids it, it has served live -- are
// only worth anything if nothing can change the answer afterwards. The
// thing that stops that is the target run lock, and runPromote takes the
// same key (targetLockKey), so once it is held a failover cannot start.
//
// The checks used to run only BEFORE that lock. A promote that began while
// the lock was being acquired therefore completed unseen, and the restore
// went on to replace the disks of a domain that had just become the live
// copy -- on facts that were true a moment earlier. The window is small,
// and what fits in it is an agent executing a failover operation, which is
// exactly what is happening during the incident that makes somebody reach
// for a restore.
func TestTheRestoreRechecksTheTargetAfterTakingItsLock(t *testing.T) {
	const file = "restore.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "runRestoreRestorePoint" {
			fn = fd
			break
		}
	}
	if fn == nil {
		t.Fatal("no func runRestoreRestorePoint in " + file)
	}

	// Every call position, by callee name.
	calls := map[string][]token.Pos{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok {
			calls[id.Name] = append(calls[id.Name], call.Pos())
		}
		return true
	})

	lockPos := calls["acquireTargetRunLock"]
	checks := calls["checkRestoreTargetState"]
	applies := calls["applyRestore"]

	// Each has to be found, or the walk is asserting about nothing.
	switch {
	case len(lockPos) != 1:
		t.Fatalf("found %d calls to acquireTargetRunLock, want exactly 1 -- this test locates the window by that call", len(lockPos))
	case len(checks) == 0:
		t.Fatal("runRestoreRestorePoint does not call checkRestoreTargetState at all")
	case len(applies) != 1:
		t.Fatalf("found %d calls to applyRestore, want exactly 1", len(applies))
	}

	// THE ASSERTION: a state check must happen after the lock is held and
	// before anything is staged.
	after := false
	for _, c := range checks {
		if c > lockPos[0] && c < applies[0] {
			after = true
		}
	}
	if !after {
		lines := make([]int, 0, len(checks))
		for _, c := range checks {
			lines = append(lines, fset.Position(c).Line)
		}
		t.Errorf("every checkRestoreTargetState call (line(s) %v) is outside the window between acquireTargetRunLock (line %d) and applyRestore (line %d).\n"+
			"  A check taken before the lock can stop being true before a disk is touched: runPromote takes the same lock key, so a failover that starts while the lock is being acquired completes unseen, and the restore then replaces the disks of the live copy.\n"+
			"  Keep the cheap pass before the lock if you like, but there must be one after it.",
			lines, fset.Position(lockPos[0]).Line, fset.Position(applies[0]).Line)
	}
}
