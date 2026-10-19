package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// THE WIRING, not the decision.
//
// incompleteReplicaRefusal is pure and tested as such. What no test here can
// reach is whether run() acts on it: that function needs two libvirt
// connections and an SSH client to enter. Mutation testing on 2026-10-20 proved
// the gap rather than assumed it -- deleting the call does not compile (the
// metadata variable becomes unused, which is a small mercy), but DOWNGRADING it
// to a warning compiles, keeps the pure test green, and lands the delta on the
// half-written image anyway. That is the most plausible future edit: somebody
// decides the refusal is too strict for a pair that is stuck.
//
// So the shape is asserted, the way sizewiring_test.go and
// reclaimwiring_test.go do for decisions their callers cannot be entered to
// test. A weaker kind of test, and the one available.
func TestRunActsOnTheIncompleteReplicaRefusal(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	var runFn *ast.FuncDecl
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if ok && fn.Recv == nil && fn.Name != nil && fn.Name.Name == "run" {
			runFn = fn
			break
		}
	}
	if runFn == nil {
		t.Fatal("run() is not in main.go any more; this test guards what it does with the refusal and has to move with it")
	}

	calls := func(n ast.Node, name string) bool {
		found := false
		ast.Inspect(n, func(x ast.Node) bool {
			c, ok := x.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := c.Fun.(*ast.Ident); ok && id.Name == name {
				found = true
			}
			return true
		})
		return found
	}

	var guard *ast.IfStmt
	ast.Inspect(runFn.Body, func(n ast.Node) bool {
		is, ok := n.(*ast.IfStmt)
		if !ok || guard != nil || is.Init == nil {
			return true
		}
		if calls(is.Init, "incompleteReplicaRefusal") {
			guard = is
		}
		return true
	})
	if guard == nil {
		t.Fatal("run() does not consult incompleteReplicaRefusal in an if-statement guard, so a replica marked incomplete receives a delta: the image is part old and part new, the delta is consistent with neither, and the run reports success and clears the marker")
	}

	// The error must be RETURNED. Logging it and carrying on is the whole
	// defect with a line of reassurance on top.
	returns := false
	ast.Inspect(guard.Body, func(n ast.Node) bool {
		if _, ok := n.(*ast.ReturnStmt); ok {
			returns = true
		}
		return true
	})
	if !returns {
		at := fset.Position(guard.Pos())
		t.Errorf("%s: run() consults the refusal but does not return it, so the sync proceeds onto a half-written replica with the reason merely recorded", at)
	}
}
