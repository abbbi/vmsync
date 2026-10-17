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
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// EVERY CALL TO UriUsesSSH IN THE MODULE IS LISTED HERE, WITH A REASON AND
// A COUNT.
//
// The function is correct, and several callers genuinely need it: it
// answers "would reaching this URI go over ssh", which is what you ask
// before building an ssh runner. The problem is that its NAME reads like a
// locality test, and for most of its life it carried no doc comment at all.
// Two guards were built on it that meant to ask "is this machine", and both
// then admitted every non-ssh remote transport -- qemu+tcp://, qemu+tls://,
// a bare qemu://host/ and qemu+ext://. One was the gate on the DR verbs,
// the other the action journal's.
//
// So this is an allowlist, not a ban. A new call surfaces here the moment
// somebody adds one without deciding which question they are asking. If the
// answer is "is this machine", the function is cmd/vmsync's
// uriRunsCommandsLocally, and this list should not grow.
//
// THREE THINGS THIS DOES THE EXPENSIVE WAY, because the cheap version of
// each was written first and proved worthless under mutation:
//
//   - It COUNTS call sites per function rather than only naming the
//     function. Keying on the name alone blesses cmd/vmsync's run()
//     wholesale -- 5,800 lines, already holding three of these calls, and
//     far and away the likeliest place a fourth lands. A count fails that
//     fourth.
//   - It RESOLVES THE IMPORT ALIAS per file instead of matching the literal
//     identifier "util", so `import u "vmsync/pkg/util"` with u.UriUsesSSH
//     cannot slip past, and a dot-import is caught too.
//   - It walks the WHOLE MODULE, not one package. UriUsesSSH is exported;
//     the next misuse has no reason to land in the package the last two
//     did.
func TestEveryUriUsesSSHCallIsDeliberate(t *testing.T) {
	type site struct {
		calls int
		why   string
	}
	// "package/path.function" -> how many calls, and why ssh specifically is
	// the right question there.
	allowed := map[string]site{
		"cmd/vmsync.run": {3,
			"builds the source and target ssh configs, and validates --source-nbd-host against the bridge; all three ask whether an ssh runner is needed, and the target one refuses anything that is not ssh outright"},
		"cmd/vmsync.uriRunsCommandsLocally": {1,
			"the locality predicate itself: ssh-to-localhost is deliberately NOT local, because the operator named a user and a key, and running as whoever invoked vmsync would change which account owns the files"},
		"cmd/vmsync.targetRunnerForRestorePoints": {1,
			"reached only after uriRunsCommandsLocally said no, to tell 'remote but reachable by ssh' from 'remote with no way to run a command there at all'"},
	}

	const modulePath = "vmsync/pkg/util"
	root := filepath.Join("..", "..") // the module root, from pkg/util
	fset := token.NewFileSet()
	found := map[string][]string{}
	scanned := 0

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "testdata", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			// Not this test's job to police syntax; the build already fails.
			return nil
		}
		scanned++

		// What does THIS file call pkg/util? Usually "util", but an alias or
		// a dot-import is exactly how a new call would hide from a check
		// that matched the literal name.
		local := ""
		dotImported := false
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, "\"") != modulePath {
				continue
			}
			switch {
			case imp.Name == nil:
				local = "util"
			case imp.Name.Name == ".":
				dotImported = true
			case imp.Name.Name == "_":
				// Imported for side effects only; nothing can be called.
			default:
				local = imp.Name.Name
			}
		}
		// Calls from inside pkg/util itself are unqualified.
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		inOwnPackage := strings.HasPrefix(rel, "pkg/util/")
		if local == "" && !dotImported && !inOwnPackage {
			return nil
		}
		pkgKey := filepath.ToSlash(filepath.Dir(rel))

		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				hit := false
				switch fun := call.Fun.(type) {
				case *ast.SelectorExpr:
					if fun.Sel.Name == "UriUsesSSH" && local != "" {
						if id, idOK := fun.X.(*ast.Ident); idOK && id.Name == local {
							hit = true
						}
					}
				case *ast.Ident:
					if fun.Name == "UriUsesSSH" && (dotImported || inOwnPackage) {
						hit = true
					}
				}
				if hit {
					key := pkgKey + "." + fn.Name.Name
					found[key] = append(found[key], rel+":"+strconv.Itoa(fset.Position(call.Pos()).Line))
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	// A walk that scanned nothing, or matched nothing, would pass in
	// silence -- which is how a source-reading test rots into a no-op. Both
	// floors sit well below the real figures.
	if scanned < 40 {
		t.Fatalf("scanned only %d non-test source files from %s; this audit is no longer module-wide", scanned, root)
	}
	if len(found) == 0 {
		t.Fatal("found no UriUsesSSH calls anywhere -- either the import path changed or this walk stopped working, and either way the guards it protects are now unchecked")
	}

	for key, sites := range found {
		want, ok := allowed[key]
		if !ok {
			t.Errorf("UriUsesSSH is called in %s (%s) and is not on the deliberate list.\n"+
				"  If it asks whether an ssh RUNNER is needed, add it here with the reason and the count.\n"+
				"  If it asks whether the URI names THIS MACHINE, it is the wrong function: qemu+tcp://, qemu+tls://, qemu://host/ and qemu+ext:// are all remote and all report false. Use cmd/vmsync's uriRunsCommandsLocally.",
				key, strings.Join(sites, ", "))
			continue
		}
		if len(sites) != want.calls {
			t.Errorf("%s now calls UriUsesSSH %d time(s); the list says %d (%s).\n"+
				"  If the new one asks whether the URI names THIS MACHINE, it is the wrong function -- use uriRunsCommandsLocally.\n"+
				"  If it genuinely needs the ssh question, raise the count here and say why.",
				key, len(sites), want.calls, strings.Join(sites, ", "))
		}
	}
	for key := range allowed {
		if _, ok := found[key]; !ok {
			t.Errorf("%s is listed as a deliberate UriUsesSSH caller but no longer calls it; drop it so this list stays an audit rather than folklore", key)
		}
	}
}
