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

package nbdsync

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// WHY THIS IS READ RATHER THAN EXECUTED.
//
// The functions it guards open two live NBD connections through libnbd's cgo
// bindings and negotiate a handshake against them. There is no machine on
// which a unit test can call them -- not here, not in CI -- so the ordinary
// way of pinning behaviour, running it and watching, is unavailable for the
// one step that has to happen before anything observable does.
//
// What it guards is a handshake-ordering rule with no runtime complaint when
// it is broken. NBD settles metadata contexts during the handshake: a context
// not requested before connecting can never be queried afterwards. Miss it and
// nbd_block_status reports nothing, allocationExtents returns an empty list,
// planCompareChunks skips nothing, and the comparison hashes the whole disk --
// correctly, silently, and for a terabyte replica about thirty-two times
// slower than it needed to be. Only a Debug line marked the difference.
//
// It happened to exactly one of the two planners. compareTCP negotiated the
// context; CompareChunkPlanTCP, the digest path that runs whenever
// vmsync-bridge-helper is deployed, did not. Both call allocationExtents, so
// "every caller of allocationExtents negotiates base:allocation on every
// handle it reads" is the invariant, and this asserts it over the source.
const allocationSourceFile = "nbd.go"

// TestEveryAllocationQueryNegotiatesItsContext walks the file and, for each
// function that calls allocationExtents, requires an
// AddMetaContext("base:allocation") for every handle it passes there.
//
// Handles are matched by the identifier the function uses, so a new planner
// reading a third export is covered without touching this test.
func TestEveryAllocationQueryNegotiatesItsContext(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, allocationSourceFile, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", allocationSourceFile, err)
	}

	checked := 0
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil || fn.Name.Name == "allocationExtents" {
			continue
		}

		queried := map[string]token.Pos{} // handle ident -> where it was queried
		negotiated := map[string]bool{}   // handle ident -> context requested
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			// allocationExtents(ctx, <handle>, role)
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "allocationExtents" && len(call.Args) >= 2 {
				if h, ok := call.Args[1].(*ast.Ident); ok {
					if _, seen := queried[h.Name]; !seen {
						queried[h.Name] = call.Pos()
					}
				}
				return true
			}
			// <handle>.AddMetaContext("base:allocation")
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "AddMetaContext" || len(call.Args) != 1 {
				return true
			}
			h, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Value == `"base:allocation"` {
				negotiated[h.Name] = true
			}
			return true
		})

		for handle, at := range queried {
			checked++
			if !negotiated[handle] {
				t.Errorf("%s: %s reads an allocation map from %q but never calls %s.AddMetaContext(\"base:allocation\") -- NBD negotiates meta contexts during the handshake, so the query will report nothing and the whole disk will be hashed",
					fset.Position(at), fn.Name.Name, handle, handle)
			}
		}
	}

	// Both planners must still be found. If this drops to zero the test has
	// stopped testing anything, which is worse than the bug: it would pass
	// over a tree where nobody negotiates anything.
	if checked < 4 {
		t.Errorf("found %d handle(s) reading an allocation map in %s, want at least 4 (two planners x two sides)", checked, allocationSourceFile)
	}
}

// TestAllocationContextIsNegotiatedBeforeConnecting pins the ORDER, which is
// the half of the rule a reader is most likely to get wrong: the call compiles
// and returns nil error wherever it is put, and only the handshake cares. A
// request made after ConnectTcp is a request that never reaches the server.
func TestAllocationContextIsNegotiatedBeforeConnecting(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, allocationSourceFile, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", allocationSourceFile, err)
	}

	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		negotiatedAt := map[string]token.Pos{}
		connectedAt := map[string]token.Pos{}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			h, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "AddMetaContext":
				if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Value == `"base:allocation"` {
					if _, seen := negotiatedAt[h.Name]; !seen {
						negotiatedAt[h.Name] = call.Pos()
					}
				}
			case "ConnectTcp", "ConnectUri", "ConnectUnix":
				if _, seen := connectedAt[h.Name]; !seen {
					connectedAt[h.Name] = call.Pos()
				}
			}
			return true
		})
		for handle, negotiated := range negotiatedAt {
			connected, ok := connectedAt[handle]
			if !ok {
				continue
			}
			if negotiated > connected {
				t.Errorf("%s: %s negotiates base:allocation on %q AFTER connecting it (connect at %s) -- the handshake is over by then and the context is never granted",
					fset.Position(negotiated), fn.Name.Name, handle, fset.Position(connected))
			}
		}
	}
}
