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

package nbdbridge

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// WHAT THE OPERATOR IS TOLD WHEN A REMOTE BRIDGE WILL NOT START.
//
// The helper's stdout and stderr go to a log file on the far host
// (BuildStartCommand), and the cleanup after a failed start rm's that file
// along with the pid file (BuildStopCommand). So the only record of WHY it
// failed was deleted, and what reached the operator was a symptom observed
// from outside: "the port never reached LISTEN".
func TestTheFailedStartReportsWhatTheHelperSaid(t *testing.T) {
	t.Run("the helper's own words are quoted", func(t *testing.T) {
		got := bridgeLogSentence("connect 10.0.0.5:10800: connection refused\n", nil, "/usr/local/bin/vmsync-bridge-helper")
		if !strings.Contains(got, "connection refused") {
			t.Errorf("the helper's reason is not in the sentence: %q", got)
		}
	})

	// An empty log is not nothing: it means the helper produced no output
	// at all, which on a failed start usually means it never ran. vmsync
	// never uploads the binary, so "it is not there" is the commonest cause
	// and the one the old message could not name.
	t.Run("an empty log names the likeliest cause and the path", func(t *testing.T) {
		got := bridgeLogSentence("", nil, "/opt/vmsync/vmsync-bridge-helper")
		if !strings.Contains(got, "/opt/vmsync/vmsync-bridge-helper") {
			t.Errorf("the sentence does not name the helper path the operator should check: %q", got)
		}
		if !strings.Contains(got, "executable") {
			t.Errorf("the sentence does not say what to check about it: %q", got)
		}
	})

	t.Run("an unconfigured path falls back to the binary name", func(t *testing.T) {
		got := bridgeLogSentence("   \n ", nil, "")
		if !strings.Contains(got, "vmsync-bridge-helper") {
			t.Errorf("the sentence names no binary at all: %q", got)
		}
	})

	// A log that could not be read must add nothing. The caller already has
	// a real error, and replacing it with one about reading a log would lose
	// the operator's actual problem for the second time on this path.
	t.Run("a failed read is silent", func(t *testing.T) {
		if got := bridgeLogSentence("whatever", errors.New("ssh client is not connected"), "/x"); got != "" {
			t.Errorf("a failed read produced %q, which would displace the caller's own error", got)
		}
	})

	// Whitespace-only output is an empty log, not a reason.
	t.Run("whitespace is not a reason", func(t *testing.T) {
		got := bridgeLogSentence("\n\t  \n", nil, "/x")
		if strings.Contains(got, "said:") {
			t.Errorf("blank output was reported as something the helper said: %q", got)
		}
	})
}

// READ BEFORE REMOVED. The ordering is the entire fix: BuildStopCommand
// deletes the log, so a read issued after it finds nothing.
//
// Checked in the source because both calls need a live ssh client, and the
// property is a sequence rather than a value.
func TestTheLogIsReadBeforeTheCleanupDeletesIt(t *testing.T) {
	const file = "remote.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "killOrphanedRemoteBridge" {
			fn = fd
			break
		}
	}
	if fn == nil {
		t.Fatal("no func killOrphanedRemoteBridge in " + file)
	}

	var readPos, stopPos token.Pos
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok {
			return true
		}
		switch id.Name {
		case "remoteBridgeLogTail":
			if readPos == 0 {
				readPos = call.Pos()
			}
		case "BuildStopCommand":
			if stopPos == 0 {
				stopPos = call.Pos()
			}
		}
		return true
	})

	if readPos == 0 {
		t.Fatal("killOrphanedRemoteBridge does not read the helper's log, so a failed start still deletes the only record of why it failed")
	}
	if stopPos == 0 {
		t.Fatal("killOrphanedRemoteBridge no longer builds a stop command; this test locates the deletion by that call")
	}
	if readPos > stopPos {
		t.Errorf("the log is read at line %d, after the cleanup at line %d removes it. BuildStopCommand rm's the log file, so the read finds an empty file and the error reports 'the helper wrote nothing' for every failure",
			fset.Position(readPos).Line, fset.Position(stopPos).Line)
	}
}
