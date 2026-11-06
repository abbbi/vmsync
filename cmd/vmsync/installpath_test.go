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

// Where -bridge-helper-path points when nobody says, and why that is not a
// free choice.
//
// The default has to name the path the vmsync-bridge-helper package installs
// to. A default pointing anywhere else makes every bridged sync fail on a host
// where the helper was installed the ordinary way -- and it fails as a refusal
// naming a path the operator never chose, which reads as a deployment mistake
// rather than as a wrong default.
//
// So the test reads the packager's own manifest instead of restating the path:
// two literals that have to agree, with nothing checking them, are two
// literals that eventually disagree.
package main

import (
	"bufio"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// packagedDestination returns where contrib/packaging installs the named
// binary, read from that binary's nfpm manifest.
//
// Found through the manifest ENTRY rather than by searching for a path: the
// entry whose src is the built binary is the one whose dst is where it lands,
// and matching on "the dst that looks like a binary path" would be the test
// asserting the answer it was given.
func packagedDestination(t *testing.T, binary string) string {
	t.Helper()
	manifest := filepath.Join("..", "..", "contrib", "packaging", "nfpm", binary+".yaml")
	f, err := os.Open(manifest)
	if err != nil {
		t.Fatalf("cannot read the packager's manifest for %s: %v", binary, err)
	}
	defer f.Close()

	wantSrc := "src: ${BIN_DIR}/" + binary
	scanner := bufio.NewScanner(f)
	seenSrc := false
	for scanner.Scan() {
		line := strings.TrimLeft(strings.TrimPrefix(strings.TrimSpace(scanner.Text()), "- "), " ")
		switch {
		case line == wantSrc:
			seenSrc = true
		case seenSrc && strings.HasPrefix(line, "dst:"):
			return strings.TrimSpace(strings.TrimPrefix(line, "dst:"))
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("reading %s: %v", manifest, err)
	}
	t.Fatalf("%s has no entry installing %s -- it is the only statement of where that binary goes, so this test cannot check the default against anything", manifest, wantSrc)
	return ""
}

func TestTheBridgeHelperDefaultIsWhereThePackageInstallsIt(t *testing.T) {
	var cfg syncConfig
	fs := flag.NewFlagSet("vmsync", flag.ContinueOnError)
	registerFlags(fs, &cfg)

	want := packagedDestination(t, "vmsync-bridge-helper")
	if cfg.BridgeHelperPath != want {
		t.Fatalf("-bridge-helper-path defaults to %q but the package installs the helper at %q.\n"+
			"A bridged sync on a host that installed the package the ordinary way would be refused for a path nobody chose. Move one of them, not neither.",
			cfg.BridgeHelperPath, want)
	}
}

// The default also has to be absolute. The path is handed to a shell on the
// REMOTE host, where the working directory is whatever sshd left it at and a
// relative name would resolve against something this end cannot see -- or
// worse, find a different binary of the same name.
func TestTheBridgeHelperDefaultIsAbsolute(t *testing.T) {
	var cfg syncConfig
	fs := flag.NewFlagSet("vmsync", flag.ContinueOnError)
	registerFlags(fs, &cfg)

	if !strings.HasPrefix(cfg.BridgeHelperPath, "/") {
		t.Fatalf("-bridge-helper-path defaults to %q, which is relative: it is resolved by a shell on the target, against a directory this end does not control", cfg.BridgeHelperPath)
	}
	if filepath.Base(cfg.BridgeHelperPath) != "vmsync-bridge-helper" {
		t.Fatalf("-bridge-helper-path defaults to %q, whose basename is not vmsync-bridge-helper: the supervision and kill paths match the helper by its comm, which is its basename truncated", cfg.BridgeHelperPath)
	}
}
