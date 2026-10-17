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
	"strings"
	"testing"
)

// NOTHING MAY BUILD A DIAL ADDRESS OUT OF A LOOPBACK LITERAL.
//
// vmsync starts two things on the TARGET host that connect to the target's
// own qemu-nbd: vmsync-bridge-helper, and the digest helper during -verify.
// Both were given "127.0.0.1:<port>" as a literal. That is correct for the
// default --target-nbd-bind of 0.0.0.0 and wrong for every narrowed value:
// qemu-nbd is then not listening on loopback at all, the connection is
// refused, and the feature fails on every run.
//
// The verify case was the damaging one, because its OTHER leg already
// derived its address from the bind -- so a narrowed bind left vmsync able
// to read the export and the helper unable, and a digest verify needs both.
//
// This checks the shape rather than the behaviour, because the behaviour
// needs two hosts and a live qemu-nbd. The rule: a loopback address with a
// port may be built only for an endpoint that is genuinely on THIS machine,
// and those are listed below. Everywhere else the address has to be derived
// from the bind.
//
// An allowlist rather than a ban, because three of these are correct:
// vmsync dialling its own local bridge relay really is dialling loopback.
// A new one surfaces here and has to be classified.
// isLoopbackSprintf reports whether a call is fmt.Sprintf with a format
// string containing the loopback literal and a verb -- i.e. an address
// being assembled rather than a message mentioning one.
func isLoopbackSprintf(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Sprintf" {
		return false
	}
	if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "fmt" {
		return false
	}
	if len(call.Args) == 0 {
		return false
	}
	lit, ok := call.Args[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	if !strings.Contains(lit.Value, "127.0.0.1") {
		return false
	}
	return strings.Contains(lit.Value, "%d") || strings.Contains(lit.Value, "%s")
}

func TestNoLoopbackLiteralIsFormattedIntoADialAddress(t *testing.T) {
	const file = "main.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	// Assignments to these are vmsync's OWN local endpoints: it starts a
	// relay on this machine and dials it over loopback, which is not a
	// guess about where anything is bound.
	localEndpoints := map[string]string{
		"sourceBridgeDialAddr": "vmsync's local end of the source bridge relay",
		"targetBridgeDialAddr": "vmsync's local end of the target bridge relay",
		"verifyBridgeDialAddr": "vmsync's local end of the verify bridge relay",
	}
	// Collected first, so the walk below can tell an allowlisted
	// assignment from a bare one by position.
	allowed := map[token.Pos]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		id, ok := as.Lhs[0].(*ast.Ident)
		if !ok {
			return true
		}
		if _, isLocal := localEndpoints[id.Name]; !isLocal {
			return true
		}
		// Only the loopback Sprintf itself, not any other call that
		// happens to feed the same variable: the allowlist has to name the
		// exact expression it is excusing.
		call, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok || !isLoopbackSprintf(call) {
			return true
		}
		allowed[call.Pos()] = true
		return true
	})
	if len(allowed) != len(localEndpoints) {
		t.Fatalf("found %d of the %d known local-endpoint assignments; one was renamed or removed, so this audit is now classifying it as a defect or missing it entirely", len(allowed), len(localEndpoints))
	}

	checked := 0
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if allowed[call.Pos()] {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Sprintf" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "fmt" {
			return true
		}
		if len(call.Args) == 0 {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		checked++
		formatStr := lit.Value
		if !strings.Contains(formatStr, "127.0.0.1") {
			return true
		}
		// A loopback literal with a port verb in it is an address being
		// built for something to dial.
		if strings.Contains(formatStr, "%d") || strings.Contains(formatStr, "%s") {
			t.Errorf("%s:%d: a dial address is built from the literal 127.0.0.1 (%s).\n"+
				"  If something on the TARGET host dials this, it must use util.ConnectHostFromBindSameHost(cfg.TargetNBDBind) -- a narrowed -target-nbd-bind leaves nothing listening on loopback.\n"+
				"  If it is one of vmsync's own local relay endpoints, that is genuinely loopback: add the variable it is assigned to, with its reason, to localEndpoints above.",
				file, fset.Position(call.Pos()).Line, formatStr)
		}
		return true
	})

	// The walk has to be finding Sprintf calls at all, or it passes by
	// looking at nothing.
	if checked < 20 {
		t.Fatalf("inspected only %d fmt.Sprintf format strings in %s; this audit is no longer covering the file", checked, file)
	}
}
