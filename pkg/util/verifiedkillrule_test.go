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
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Where a kill may be spelled out by hand, how many there are, and why.
// Everything else goes through VerifiedKillCommand.
//
// This is the only test in this package that reads outside it, because what it
// forbids is an ABSENCE -- a kill with no proof in front of it -- and an
// absence cannot be tested at the call site that lacks it. VerifiedKillCommand's
// own behaviour is asserted against real processes next door; this says nobody
// can quietly go back to not calling it.
//
// IT COUNTS KILLS PER FUNCTION rather than naming files, and that is not
// fussiness. The first version of this rule keyed an allowlist on the FILE and
// recorded only that a kill existed somewhere in it. cmd/vmsync/main.go was on
// that list -- eleven thousand lines, the home of two of the three kills this
// rule exists because of, and far and away the likeliest place a fourth lands.
// Inside an allowlisted file nothing else fired, so a new teardown spelled
// `p=$(cat pidfile); kill -9 "$p"` passed the whole rule: the operand is a
// quoted variable, the file is blessed, and the evidence string it was blessed
// for is still somewhere else in those eleven thousand lines. That is 4.20's
// defect, re-added, with the guard against it green.
//
// pkg/util/uriusesssh_audit_test.go in this same package had already reached
// that conclusion and written down why: "It COUNTS call sites per function
// rather than only naming the function. Keying on the name alone blesses
// cmd/vmsync's run() wholesale -- 5,800 lines, already holding three of these
// calls, and far and away the likeliest place a fourth lands. A count fails
// that fourth." This is that design, applied to kills.
//
// The count is of OPERANDS, not of command strings: one concatenation chain can
// hold several kills, as the checksum export's stop does (a SIGKILL and the
// liveness poll of its wait loop), and counting chains would let a fourth kill
// be folded into an existing one.
var handWrittenKills = map[string]struct {
	kills int
	why   string
}{
	"pkg/util.VerifiedKill.Command": {3,
		"the builder itself: the single-process kill, the group kill, and the liveness poll that waits for the target to go"},
	"cmd/vmsync.run": {3,
		"the checksum export's own stop -- its SIGKILL and its wait loop's liveness poll -- which matches on its SOCKET path rather than its pidfile and so was left as it was, plus the inline /proc-scan reclaim whose pid IS the directory the argv match was found in. This function is 6,000 lines and holds three of them, which is exactly why this list counts"},
	"cmd/vmsync.reclaimAbandonedExportCmd": {1,
		"the /proc-scan reclaim builder: the pid is the directory the argv match was found in, so it is verified by construction rather than by a check"},
}

// repoRoot returns the module root, and fails rather than returning something
// plausible: a wrong root would make every rule below pass over no files at
// all, which is the shape of a test that proves nothing.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve the module root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("no go.mod at %s, so this test is not where it thinks it is: %v", root, err)
	}
	return root
}

// shellFragments returns every shell-command string a node builds: each
// top-level string concatenation flattened into one fragment, plus every
// standalone literal. Flattening matters -- the command this rule exists to
// forbid is spread over four concatenated literals, and no single one of them
// holds enough to recognise it.
//
// Comments are not literals, so the many places that DISCUSS kill -9 (including
// the ones explaining why this rule exists) are invisible here. That is the
// reason this works on the AST rather than on the file's text.
func shellFragments(n ast.Node) []string {
	join := func(n ast.Node) string {
		var b strings.Builder
		ast.Inspect(n, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if s, err := strconv.Unquote(lit.Value); err == nil {
					b.WriteString(s)
				}
			}
			return true
		})
		return b.String()
	}

	var out []string
	ast.Inspect(n, func(n ast.Node) bool {
		switch e := n.(type) {
		case *ast.BinaryExpr:
			if e.Op == token.ADD {
				out = append(out, join(e))
				return false // outermost chain only
			}
		case *ast.BasicLit:
			if e.Kind == token.STRING {
				if s, err := strconv.Unquote(e.Value); err == nil {
					out = append(out, s)
				}
				return false
			}
		}
		return true
	})
	return out
}

// isWordBoundary reports whether a byte cannot be part of a command name, so
// "kill" is found in `; kill -9 x` and `|kill` but not in "killall" or
// "superviseKilled".
func isWordBoundary(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return false
	case b == '_' || b == '-' || b == '.':
		return false
	}
	return true
}

// wordAt reports whether word appears in s at index i as a whole shell word.
func wordAt(s, word string, i int) bool {
	if i > 0 && !isWordBoundary(s[i-1]) {
		return false
	}
	j := i + len(word)
	return j >= len(s) || isWordBoundary(s[j])
}

// killOperands returns the first operand of every `kill` COMMAND in frag, in
// order.
//
// Two mistakes were made in writing this gate, in opposite directions, and the
// shape it ended up with is the narrow path between them.
//
// Looking for the string "kill -" is fail-OPEN: it cannot see `kill 1234`,
// `kill  -9 x` with two spaces, or any default-signal kill, and it sat in front
// of the rule's strongest check.
//
// Looking for the word "kill" is fail-CLOSED but far too eager, because this
// tree talks to operators about killing things. It flagged a flag's help text
// ("so it survives a kill aimed at the group it started in") and a refusal
// message ("kill it or its ssh session on this host") -- English, in strings
// that never reach a shell.
//
// So: the word "kill", followed by something only a command would have there --
// an option, a quote, an expansion, or a digit. Prose puts a letter there.
func killOperands(frag string) []string {
	looksLikeACommand := func(after string) bool {
		s := strings.TrimLeft(after, " \t")
		if s == "" {
			return true // `"kill -9 " + variable`: the operand is not a literal
		}
		switch s[0] {
		case '-', '$', '"', '\'', '`', '%':
			return true
		}
		return s[0] >= '0' && s[0] <= '9'
	}

	var out []string
	for i := 0; i+4 <= len(frag); i++ {
		if frag[i:i+4] != "kill" || !wordAt(frag, "kill", i) {
			continue
		}
		if !looksLikeACommand(frag[i+4:]) {
			continue
		}
		rest := strings.Fields(frag[i+4:])
	skipOptions:
		for len(rest) > 0 {
			w := rest[0]
			switch {
			case w == "--":
				rest = rest[1:]
			case w == "-s" || w == "-n":
				// Takes a signal or number of its own, which is not the
				// operand: `kill -s KILL "$p"` would otherwise be read as a
				// kill aimed at KILL.
				rest = rest[1:]
				if len(rest) > 0 {
					rest = rest[1:]
				}
			case strings.HasPrefix(w, "-") && w != "-" && !strings.HasPrefix(w, "-$"):
				rest = rest[1:] // -9, -KILL, -SIGKILL
			default:
				break skipOptions
			}
		}
		if len(rest) == 0 {
			out = append(out, "") // a kill with no operand in the literal
			continue
		}
		out = append(out, rest[0])
	}
	return out
}

// nameMatchedSignallers are forbidden outright. pkill and killall select their
// targets BY NAME, which is the unverified identity this whole rule is about,
// and no amount of quoting makes that a proof.
var nameMatchedSignallers = []string{"pkill", "killall"}

// THE RULE, and it is deliberately the other way round from the obvious one.
//
// The obvious rule forbids the dangerous spelling: an operand that is a command
// substitution, `kill -9 $(cat pidfile)`. That version was written first and had
// two faults. It flagged the checksum export's CORRECT stop, because that reads
// its pid with `p=$(cat pidfile)` -- reading the file is not the defect, letting
// the result BE the kill's target is. And it could be walked straight past:
// `fmt.Sprintf("kill -9 %s", target)` leaves the literal operand `%s`, and
// `"kill -9 " + target` leaves none at all, so a kill assembled through a
// variable sailed through a rule only ever looking for the four characters `$(`.
//
// A denylist of spellings is the wrong shape. So instead: every kill in this
// tree must aim at a DOUBLE-QUOTED SHELL EXPANSION -- "$p", "${d#/proc/}", or
// "-$p" for a group. That is what all the hand-written kills do, and it is the
// shape that means a variable stands between the source of the number and the
// signal, which is the only place a check can live. It proves a check is
// POSSIBLE, not that one is present; the counts above are what keep the second
// half honest.
//
// Fails closed: an operand it cannot recognise is a failure, not a pass. A
// format verb ("%s"), a backtick substitution ("`cat f`") and a literal pid
// ("1234") are all rejected, because none of them is a variable something had
// the chance to validate.
func aimsAtAValidatedVariable(operand string) bool {
	if len(operand) < 3 || operand[0] != '"' || operand[len(operand)-1] != '"' {
		return false
	}
	inner := operand[1 : len(operand)-1]
	inner = strings.TrimPrefix(inner, "-") // a group's negated pid
	// Must be an expansion, and not one that runs a command or formats a
	// value: "$(...)" and "`...`" are substitutions, "%s" is a format verb
	// standing in for something this rule cannot see.
	if !strings.HasPrefix(inner, "$") {
		return false
	}
	return !strings.Contains(inner, "$(") && !strings.ContainsAny(inner, "`%")
}

// funcKey names the enclosing function the way uriusesssh_audit_test.go does:
// the package directory relative to the module root, then the function, with a
// receiver type for a method.
func funcKey(pkgDir string, fn *ast.FuncDecl) string {
	name := fn.Name.Name
	if fn.Recv != nil && len(fn.Recv.List) > 0 {
		t := fn.Recv.List[0].Type
		if star, ok := t.(*ast.StarExpr); ok {
			t = star.X
		}
		if id, ok := t.(*ast.Ident); ok {
			name = id.Name + "." + name
		}
	}
	return pkgDir + "." + name
}

func TestNoKillIsAimedByAPidfileAlone(t *testing.T) {
	root := repoRoot(t)

	var scanned, operandsSeen int
	found := map[string][]string{}    // funcKey -> the operands found there
	badShape := map[string][]string{} // funcKey -> operands that prove nothing
	var nameMatched []string

	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "testdata", "node_modules":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		scanned++
		rel, _ := filepath.Rel(root, p)
		pkgDir := filepath.ToSlash(filepath.Dir(rel))

		f, perr := parser.ParseFile(token.NewFileSet(), p, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", rel, perr)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			key := funcKey(pkgDir, fn)
			for _, frag := range shellFragments(fn.Body) {
				for _, bad := range nameMatchedSignallers {
					if i := strings.Index(frag, bad); i >= 0 && wordAt(frag, bad, i) {
						nameMatched = append(nameMatched,
							key+": "+bad+" in "+strings.TrimSpace(frag))
					}
				}
				for _, operand := range killOperands(frag) {
					operandsSeen++
					found[key] = append(found[key], operand)
					if !aimsAtAValidatedVariable(operand) {
						badShape[key] = append(badShape[key],
							"kill ... "+operand+"\n    in: "+strings.TrimSpace(frag))
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	// ANTI-VACUITY, and the assertion that fails if this test ever stops
	// working: a broken walk, a parse that silently returns nothing, or a
	// flattener that loses literals would all leave every map empty and report
	// success. Finding the kills it knows about is the only way this rule can
	// tell "clean" from "blind".
	if scanned < 20 {
		t.Fatalf("only %d non-test Go files scanned under %s -- the walk is not reaching the tree", scanned, root)
	}
	if operandsSeen == 0 {
		t.Fatal("found no kill operands anywhere in the module -- either every kill was removed, in which case empty handWrittenKills, or this walk has stopped seeing shell commands and is passing blind")
	}

	for _, o := range nameMatched {
		t.Errorf("a signaller that selects its target BY NAME:\n  %s\npkill and killall match on a name, which is the unverified identity this rule exists to forbid. Use util.VerifiedKillCommand, which proves the pid still carries the pidfile path in its argv.", o)
	}

	keys := make([]string, 0, len(found))
	for k := range found {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// What the scan actually saw, so a count that needs changing can be
	// changed from evidence rather than from guesswork, and so a reader can
	// tell at once whether a surprising verdict comes from the rule or from
	// the walk.
	for _, key := range keys {
		t.Logf("%d hand-written kill(s) in %s: %s",
			len(found[key]), key, strings.Join(found[key], " "))
	}

	for _, key := range keys {
		for _, o := range badShape[key] {
			t.Errorf("a kill that does not aim at a double-quoted shell variable, in %s:\n  %s\nEvery kill here must be `kill -<sig> \"$var\"` (or `\"-$var\"` for a group), because the variable is where a check can live. kill -9 succeeds against any valid pid, recycled or not, and `|| true` hides it. Use util.VerifiedKillCommand.", key, o)
		}

		want, ok := handWrittenKills[key]
		if !ok {
			t.Errorf("%s spells out %d kill(s) by hand and is not on the deliberate list (%s).\n"+
				"  Use util.VerifiedKillCommand, which proves the pid still carries the pidfile path in its argv before it signals.\n"+
				"  If it genuinely cannot -- the pid is already verified by construction, say, because it came out of a /proc argv scan -- add it here with the count and the reason.",
				key, len(found[key]), strings.Join(found[key], " "))
			continue
		}
		if len(found[key]) != want.kills {
			t.Errorf("%s now spells out %d kill(s) by hand; the list says %d (%s).\n"+
				"  Operands found: %s\n"+
				"  A new one belongs in util.VerifiedKillCommand. If it really is another verified-by-construction kill, raise the count here and say why.",
				key, len(found[key]), want.kills, want.why, strings.Join(found[key], " "))
		}
	}

	for key, want := range handWrittenKills {
		if _, ok := found[key]; !ok {
			t.Errorf("%s is listed as spelling out %d kill(s) by hand (%s) but no longer holds any; drop it so this list stays an audit rather than folklore", key, want.kills, want.why)
		}
	}
}

// The hand-written kills have to keep the thing that makes them safe.
//
// WHAT IS PINNED MATTERS. The first version of this pinned the string
// "/proc/$p/cmdline", which survives the one weakening mutation testing called
// plausible: dropping the -x from `grep -qxF` turns a whole-argv-word match
// into a substring match, and the stop command of a sibling run -- `sh -c
// "p=$(cat /run/....pid); ..."` -- carries that path inside one long argv word.
// The /proc path is still right there while the proof is gone. So the match is
// what gets pinned, not the path.
func TestHandWrittenKillsKeepTheirProof(t *testing.T) {
	root := repoRoot(t)
	// file -> the text that is the proof there, and how many there must be.
	//
	// COUNTED, for the same reason the kills are. main.go holds two argv
	// matches, so a mere substring check is satisfied by either and weakening
	// one leaves the other to vouch for it.
	//
	// Counted inside string literals only, through the same AST pass: main.go
	// also DISCUSSES `grep -qxF <sockpath>` in a comment, and a rule that
	// counted prose would move every time somebody edited an explanation.
	proofs := map[string]struct {
		text  string
		count int
	}{
		"pkg/util/verifiedkill.go": {"grep -qxF", 1},
		"cmd/vmsync/main.go":       {"grep -qxF", 2},
		// The reclaim's pid IS the /proc directory the match was found in, so
		// the proof is that derivation rather than a comparison.
		// Twice: once to match the argv, once to pull the image path out
		// of it for the message the reclaim prints.
		"cmd/vmsync/supervisedcmd.go": {"$d/cmdline", 2},
	}
	for rel, want := range proofs {
		p := filepath.Join(root, filepath.FromSlash(rel))
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, 0)
		if err != nil {
			t.Errorf("%s holds a hand-written kill but cannot be parsed: %v", rel, err)
			continue
		}
		got := 0
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			for _, frag := range shellFragments(fn.Body) {
				got += strings.Count(frag, want.text)
			}
		}
		if got != want.count {
			t.Errorf("%s builds %d command(s) containing %q; there should be %d.\n"+
				"  That match is what makes aiming at a pid a proof rather than a guess. If a kill was added or removed here, change the count; if a match was WEAKENED -- the -x dropped, say, which turns a whole-argv-word match into a substring one -- put it back.",
				rel, got, want.text, want.count)
		}
	}
}
