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
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// THE ONE THING ABOUT BreakRunLock THAT ONLY STRUCTURE CAN SEE: when the lock
// is released.
//
// The interlock is that the flock is held ACROSS the unlink. `defer f.Close()`
// achieves that, and turning it into a plain `f.Close()` placed just before
// `os.Remove` undoes it completely while every behavioural test still passes --
// measured on 2026-10-19, with gofmt, go vet and `go test ./...` all clean
// afterwards. It is also the single most natural edit anyone touching this
// function will make: close the file before you delete it.
//
// No behavioural test can catch that. Observing it would mean watching the lock
// state from inside BreakRunLock between two adjacent statements.
//
// SCOPE, because an earlier version of this test claimed more than it proved:
// a structural test sees names and positions, nothing else. It cannot tell a
// guard from a call whose result is thrown away, and three ways of neutering
// the inode check -- a bare call, a call behind an always-false condition, and
// a call moved into a defer -- all satisfied the order assertions this file
// used to make. That check is now pinned behaviourally instead, by
// TestBreakRunLockRefusesWhenWhatItLockedIsNotWhatIsAtThePath, which forces the
// comparison through the lockStillAtPath seam. What is left here is the release
// ordering, which is genuinely structural, plus the presence and order of the
// two steps that bracket it.
func TestBreakRunLockHoldsTheLockAcrossTheUnlink(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "breaklock.go", nil, 0)
	if err != nil {
		t.Fatalf("parse breaklock.go: %v", err)
	}

	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv == nil && fn.Name != nil && fn.Name.Name == "BreakRunLock" {
			body = fn.Body
			break
		}
	}
	if body == nil {
		t.Fatal("BreakRunLock is not in breaklock.go any more; this test guards its internal order and has to move with it")
	}

	// calls returns the names called anywhere inside a statement, including
	// inside its Init and its nested blocks.
	calls := func(n ast.Node) map[string]bool {
		found := map[string]bool{}
		ast.Inspect(n, func(x ast.Node) bool {
			call, ok := x.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fun := call.Fun.(type) {
			case *ast.Ident:
				found[fun.Name] = true
			case *ast.SelectorExpr:
				if fun.Sel == nil {
					return true
				}
				if pkg, ok := fun.X.(*ast.Ident); ok {
					found[pkg.Name+"."+fun.Sel.Name] = true
				}
				found["."+fun.Sel.Name] = true
			}
			return true
		})
		return found
	}

	const missing = -1
	flockAt, removeAt, bareCloseAt := missing, missing, missing
	for i, stmt := range body.List {
		in := calls(stmt)
		if flockAt == missing && in["FlockExclusiveNB"] {
			flockAt = i
		}
		if removeAt == missing && in["os.Remove"] {
			removeAt = i
		}
		// A Close that is NOT deferred, as its own statement. `defer
		// f.Close()` is a DeferStmt and is the correct form; a bare
		// `f.Close()` releases the lock at that point in the flow.
		if _, deferred := stmt.(*ast.DeferStmt); !deferred && bareCloseAt == missing {
			if expr, ok := stmt.(*ast.ExprStmt); ok && calls(expr)["."+"Close"] {
				bareCloseAt = i
			}
		}
	}

	if flockAt == missing {
		t.Error("BreakRunLock does not take the lock before unlinking: without that it removes the lock file of whatever currently holds it, which is defect 4.11, measured with both inodes on 2026-10-18")
	}
	if removeAt == missing {
		t.Error("BreakRunLock no longer removes anything, so breaking a lock does nothing while every caller is told it succeeded")
	}
	if flockAt == missing || removeAt == missing {
		return
	}
	if flockAt > removeAt {
		t.Error("BreakRunLock unlinks before it takes the lock, which is the defect this order exists to fix")
	}
	if bareCloseAt != missing && bareCloseAt < removeAt {
		t.Errorf("BreakRunLock closes the lock file at statement %d, before the unlink at statement %d: the descriptor is what holds the flock, so releasing it first puts the unlink back outside the lock and a run acquiring in that window is dispossessed exactly as before. Keep it deferred", bareCloseAt, removeAt)
	}
}
