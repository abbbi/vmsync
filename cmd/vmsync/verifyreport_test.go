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

// TestTheRunResultReportsWhetherVerificationRan guards this end of a contract
// whose other end is in another binary.
//
// vmsync-agent decides whether a calendar verify window was spent by reading
// VerificationRan out of the file this run writes. A missing field reads as
// false, which is deliberately the safe direction for an OLD engine -- but it
// is silent, so an engine that simply stopped filling the field in would look
// identical to one too old to have it. The agent would go back to re-arming
// the window after every failure, a replica with a real finding would be
// verified again every few minutes for hours, and nothing anywhere would fail.
//
// Hence a test on this side rather than trusting the agent's. The agent's own
// tests prove it reads the field; they cannot prove anybody writes it.
//
// WHY READ RATHER THAN EXECUTED: writeRunResult is a closure inside run(),
// which opens libvirt connections to two hypervisors.
//
// MUTATION CAUGHT: dropping the VerificationRan line from the literal, or
// replacing it with a constant.
func TestTheRunResultReportsWhetherVerificationRan(t *testing.T) {
	const file = "main.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	literals, populated := 0, 0
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Result" {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "runresult" {
			return true
		}
		literals++
		for _, el := range lit.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok || key.Name != "VerificationRan" {
				continue
			}
			call, ok := kv.Value.(*ast.CallExpr)
			if !ok {
				t.Errorf("%s: VerificationRan is set to something that is not a call; it must come from verificationRan(), the same predicate the metric uses, or \"asked for a verify\" and \"a verify happened\" drift apart again",
					fset.Position(kv.Pos()))
				continue
			}
			fn, ok := call.Fun.(*ast.Ident)
			if !ok || fn.Name != "verificationRan" {
				t.Errorf("%s: VerificationRan does not come from verificationRan()", fset.Position(kv.Pos()))
				continue
			}
			populated++
		}
		return true
	})

	if literals == 0 {
		t.Fatal("no runresult.Result literal in main.go: this run no longer reports anything to a supervising agent")
	}
	if populated != literals {
		t.Errorf("%d of %d runresult.Result literals never populate VerificationRan; vmsync-agent would re-arm the verify window after every failed verification, re-verifying a failing replica every few minutes for the length of its window", literals-populated, literals)
	}
}
