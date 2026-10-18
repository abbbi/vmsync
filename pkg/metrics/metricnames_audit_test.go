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

package metrics

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Where the series are actually defined. Every other file in the module is
// held to naming only what these two emit.
var metricEmitters = map[string]bool{
	"pkg/metrics/metrics.go":      true,
	"cmd/vmsync-agent/metrics.go": true,
}

// Identifiers that look like a series and are not. Keep this list short and
// make each entry obviously not a metric; a real series added here would
// silently exempt the thing this test exists to catch.
var notMetricNames = map[string]bool{
	"vmsync_path": true, // a JSON field in the agent's on-disk state
}

var metricNameRe = regexp.MustCompile(`vmsync_[a-z0-9_]+`)

// A PROMETHEUS NAME IN A MESSAGE MUST BE A SERIES THAT EXISTS.
//
// PromQL names are exact, so an operator told to watch a series that is not
// emitted writes an alert that is silent forever and reads the silence as
// health. That is strictly worse than not mentioning one.
//
// It happened: a warning explaining why a prune had been skipped told the
// reader that "restore points will accumulate until this succeeds, which
// vmsync_restore_points_over reports". The series is
// vmsync_restore_points_over_count. The whole justification for skipping the
// prune -- that unbounded growth is visible -- rested on an alert that could
// never fire, and the doc written beside it repeated the same wrong name
// while the reference doc two files away had it right.
//
// String literals only, so prose in a comment may still abbreviate a name.
// Non-test files only, because fixtures legitimately invent labels.
func TestEveryMetricNameInAMessageIsASeriesThatExists(t *testing.T) {
	root := moduleRoot(t)
	defined := map[string]bool{}
	type site struct {
		file string
		line int
	}
	mentioned := map[string][]site{}

	fset := token.NewFileSet()
	err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		f, perr := parser.ParseFile(fset, p, nil, 0)
		if perr != nil {
			return nil // not this test's business
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			for _, m := range metricNameRe.FindAllString(lit.Value, -1) {
				if metricEmitters[rel] {
					defined[m] = true
				} else if !notMetricNames[m] {
					mentioned[m] = append(mentioned[m], site{rel, fset.Position(lit.Pos()).Line})
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	// Not vacuous: if the emitters stopped being found this test would pass
	// by knowing nothing.
	if len(defined) < 40 {
		t.Fatalf("only %d series found in %v; the emitters were not read, so this test proves nothing", len(defined), metricEmitters)
	}

	var bad []string
	for name := range mentioned {
		if !defined[name] {
			bad = append(bad, name)
		}
	}
	sort.Strings(bad)
	for _, name := range bad {
		var where []string
		for _, s := range mentioned[name] {
			where = append(where, s.file+":"+itoa(s.line))
		}
		t.Errorf("%q is named in a message at %s but no exporter emits it. An operator who builds an alert on that name gets an empty result forever and reads it as health. Check the exact series in %v -- the real one is usually this name with a suffix.",
			name, strings.Join(where, ", "), keysOf(metricEmitters))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// moduleRoot walks up from this package to the directory holding go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("no go.mod found above the metrics package")
	return ""
}
