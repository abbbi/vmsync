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
	"os"
	"strings"
	"testing"
)

// WHAT THESE TWO TESTS ARE FOR, and why they read source rather than run it.
//
// Taking the target-side run lock without recording WHO took it is worse than
// it sounds, and three of the five acquisition sites in this package had that
// shape. Acquisition never truncates the lock file and release never unlinks
// it, so a previous holder's stamp survives underneath a new holder: a caller
// blocked by a live promote or restore was not told "unattributable", it was
// shown a DEAD process's identity and told the lock was stale, with
// -break-target-lock offered as the remedy. Breaking it would unlink the lock
// from under a running restore and let a sync overwrite the disks it was
// rolling back.
//
// pkg/util already proves the mechanism -- the round trip, the truncate, the
// missing and empty cases, the path both sides agree on. What nothing proved
// was that the CALL SITES use it, and they are unreachable from a test: one
// needs two libvirt connections, one needs an SSH peer, one needs a restore in
// progress. The design also deliberately refuses to lean on the stamp for
// correctness (it is best-effort provenance, with a kernel liveness probe
// doing the load-bearing work), so no behavioural assertion anywhere else
// fails when a stamp goes missing.
//
// WHAT THEY CANNOT SEE, stated so the coverage is not mistaken for more than
// it is: that the identity describes the right operation, that it is written
// before anything that could block, and that the warning on failure is
// reached. Only the presence and the target of the write.

// packageFiles are this package's own sources, excluding tests. Read from the
// directory rather than listed, so a new file carrying a new acquisition site
// is covered the day it lands rather than the day somebody remembers.
func packageFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		out = append(out, name)
	}
	if len(out) == 0 {
		t.Fatal("no non-test Go files found; this test is looking in the wrong directory")
	}
	return out
}

// isUtilCall reports whether e is a call to util.<name>.
func isUtilCall(e ast.Expr, name string) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "util"
}

// TestEveryLocalRunLockIsStamped pins the LOCAL acquirers: whatever
// util.AcquireRunLock hands back must then be handed to
// util.WriteRunLockIdentity.
//
// Matched by VARIABLE, not merely by both calls appearing in the same
// function, so a site that stamps some other file -- or stamps nothing while
// a sibling branch stamps its own -- is still a failure. That second shape is
// exactly what was wrong in acquireTargetRunLock, whose remote branch passed
// an Identity while its local branch dropped the one built eight lines above.
//
// MUTATION CAUGHT: deleting a WriteRunLockIdentity call, or pointing it at a
// different handle. Both compile, and every other test in the tree passes.
func TestEveryLocalRunLockIsStamped(t *testing.T) {
	for _, file := range packageFiles(t) {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}

		// Handles the local lock hands out, by variable name, and where.
		held := map[string]token.Position{}
		stamped := map[string]bool{}

		ast.Inspect(f, func(n ast.Node) bool {
			if assign, ok := n.(*ast.AssignStmt); ok && len(assign.Rhs) == 1 && isUtilCall(assign.Rhs[0], "AcquireRunLock") {
				if len(assign.Lhs) == 0 {
					t.Errorf("%s: the run lock's handle is discarded, so nothing can stamp it", fset.Position(assign.Pos()))
					return true
				}
				if id, ok := assign.Lhs[0].(*ast.Ident); ok {
					if id.Name == "_" {
						t.Errorf("%s: the run lock's handle is assigned to _, so nothing can stamp it", fset.Position(assign.Pos()))
						return true
					}
					held[id.Name] = fset.Position(assign.Pos())
				}
			}
			if call, ok := n.(*ast.CallExpr); ok && isUtilCall(call, "WriteRunLockIdentity") && len(call.Args) > 0 {
				if id, ok := call.Args[0].(*ast.Ident); ok {
					stamped[id.Name] = true
				}
			}
			return true
		})

		for name, pos := range held {
			if !stamped[name] {
				t.Errorf("%s: %s holds the run lock and is never passed to util.WriteRunLockIdentity, so whoever this lock blocks is told the holder is unattributable -- or, because acquisition never truncates the file, is shown a dead predecessor's identity and told the lock is stale while this process holds it",
					pos, name)
			}
		}
	}
}

// TestEveryRemoteRunLockCarriesAnIdentity pins the other two sites.
//
// The remote lock takes its stamp as a field rather than a second call, and
// RemoteLockOptions has other fields -- so a literal that sets HelperPath and
// nothing else compiles, runs, holds the lock correctly, and records nobody.
// The helper fills in the liveness fields itself because it is the actual
// holder, but only if it was given an identity to fill them into.
//
// MUTATION CAUGHT: dropping the Identity field from either literal.
func TestEveryRemoteRunLockCarriesAnIdentity(t *testing.T) {
	seen := 0
	for _, file := range packageFiles(t) {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !isUtilCall(call, "AcquireRemoteRunLock") {
				return true
			}
			seen++
			for _, arg := range call.Args {
				lit, ok := arg.(*ast.CompositeLit)
				if !ok {
					continue
				}
				sel, ok := lit.Type.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "RemoteLockOptions" {
					continue
				}
				for _, el := range lit.Elts {
					if kv, ok := el.(*ast.KeyValueExpr); ok {
						if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Identity" {
							return true
						}
					}
				}
				t.Errorf("%s: this remote run lock is taken with no Identity in its RemoteLockOptions, so the helper holding it records nobody and anything blocked by it is told the holder is unattributable",
					fset.Position(lit.Pos()))
			}
			return true
		})
	}
	// The count is asserted because the walk above proves nothing about a
	// site it never visited: a refactor that moves an acquisition behind a
	// wrapper in another package would leave this test passing on zero.
	if seen != 2 {
		t.Errorf("found %d util.AcquireRemoteRunLock call sites, want 2 (the sync's target lock and the restore's) -- if a third has landed, decide what stamps it and add it here", seen)
	}
}
