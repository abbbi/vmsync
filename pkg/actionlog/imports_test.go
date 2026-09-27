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

package actionlog

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// importPath is this package, as anything importing it would spell it.
const importPath = "vmsync/pkg/actionlog"

// THE RULE, MECHANICALLY. Nothing that DECIDES anything may import the
// journal.
//
// The journal is evidence, never an input to a decision. It is written
// best-effort onto a remote filesystem that may be full, read-only, or simply
// gone; a refusal that consulted it would be a refusal that silently stops
// refusing the day the target's disk fills up. The refusal that closes CI-02
// therefore rests on the domain's own replica_incomplete metadata field --
// readable on the DR host with the source host unreachable, which is the state
// a disaster actually leaves behind.
//
// pkg/failover holds every promotion and inversion decision; pkg/libvirtsync
// holds every metadata gate. Neither may reach for this package, and a comment
// saying so is not enforcement. This is.
//
// Deliberately a source-level check rather than a build-tag or a link-time
// one: pkg/libvirtsync cannot be compiled without libvirt headers, so a test
// that imported it to inspect it would not run on most developer machines --
// and a rule that only runs somewhere else is a rule that gets broken here.
func TestNothingThatDecidesImportsThisPackage(t *testing.T) {
	for _, pkg := range []string{"failover", "libvirtsync"} {
		dir := filepath.Join("..", pkg)
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("cannot find pkg/%s to check its imports: %v -- this test is the only thing enforcing that diagnosis never decides, so it must not be allowed to pass by skipping", pkg, err)
		}
		for _, file := range goFilesIn(t, dir) {
			for _, imp := range importsOf(t, file) {
				if imp == importPath {
					t.Errorf("%s imports %s. The journal is evidence, never an input to a decision: a refusal built on a best-effort record written to a remote filesystem stops refusing the day that filesystem fills up. Whatever this needs, read it from the domain's own metadata instead", file, importPath)
				}
			}
		}
	}
}

// The other half of the same rule: this package must not reach into anything
// that decides, either, or the dependency would simply run the other way and
// the two would still be welded together.
//
// It also must not import pkg/util or any libvirt binding, which is what keeps
// its tests runnable on a machine with no hypervisor toolchain -- the entire
// reason the journal's format and every command it sends to a production
// target can be tested at all.
func TestThisPackageStaysStdlibOnly(t *testing.T) {
	for _, file := range goFilesIn(t, ".") {
		for _, imp := range importsOf(t, file) {
			switch {
			case strings.HasPrefix(imp, "vmsync/"):
				t.Errorf("%s imports %s; this package is deliberately free of the rest of vmsync so it builds and tests anywhere", file, imp)
			case strings.HasPrefix(imp, "libvirt.org/"), strings.HasPrefix(imp, "libguestfs.org/"):
				t.Errorf("%s imports %s, which needs hypervisor headers to compile", file, imp)
			case strings.Contains(imp, "."):
				// A dot in the first path element means a hosted module.
				if strings.Contains(strings.SplitN(imp, "/", 2)[0], ".") {
					t.Errorf("%s imports the third-party module %s; this package is stdlib only", file, imp)
				}
			}
		}
	}
}

func goFilesIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	if len(out) == 0 {
		t.Fatalf("no go files in %s, so this check would pass by finding nothing", dir)
	}
	return out
}

func importsOf(t *testing.T, file string) []string {
	t.Helper()
	// ImportsOnly: parsing the whole of pkg/libvirtsync is unnecessary here,
	// and stopping at the import block means a file using a language feature
	// newer than this parser still gets checked.
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	var out []string
	for _, spec := range f.Imports {
		out = append(out, importPathOf(t, spec))
	}
	return out
}

func importPathOf(t *testing.T, spec *ast.ImportSpec) string {
	t.Helper()
	p, err := strconv.Unquote(spec.Path.Value)
	if err != nil {
		t.Fatalf("unquote import %s: %v", spec.Path.Value, err)
	}
	return p
}
