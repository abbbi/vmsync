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

// Which binaries the agent execs when its config does not say, and why those
// defaults answer to the packager rather than to taste.
//
// vmsync_path is the binary EVERY sync, operation and fence runs. An agent
// whose default points somewhere the package does not install fails every one
// of them, on a host that was installed the ordinary way, with an exec error
// naming a path the operator never chose.
//
// The example config in contrib/agent/ deliberately omits both binary-path
// keys so the defaults are what a fresh install uses, which puts the whole
// weight of "a packaged agent can exec anything at all" on the default below
// and the unit beside it.
//
// bridge_helper_path has no agent-side default on purpose: left unset, the
// agent omits the flag (profile.go) and vmsync's own default answers, which is
// asserted in cmd/vmsync/installpath_test.go. Adding one here would create the
// second independent literal these tests exist to avoid.
package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// packagedDestination returns where contrib/packaging installs the named
// binary, read from that binary's own nfpm manifest.
//
// Found through the manifest ENTRY rather than by searching for a path that
// looks right: the entry whose src is the built binary is the one whose dst
// says where it lands.
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

func TestTheVmsyncPathDefaultIsWhereThePackageInstallsIt(t *testing.T) {
	got := AgentFile{}.withDefaults().VmsyncPath
	want := packagedDestination(t, "vmsync")
	if got != want {
		t.Fatalf("vmsync_path defaults to %q but the package installs vmsync at %q.\n"+
			"An agent installed from that package, with the example config this repo ships, would fail every sync, operation and fence on an exec error. Move one of them, not neither.",
			got, want)
	}
}

// The example config this repo ships as /etc/vmsync/agent.json.example must
// keep leaving both binary paths unset, so that what an operator copies uses
// the defaults the tests above pin rather than a literal that has to be moved
// in lockstep with them.
//
// Here as well as in contrib/packaging/build.sh, and that is the point: the
// build-time guard runs only when somebody builds packages, so a key added to
// the example would pass CI, merge, and surface as a refused nightly package
// build days later. Parsing it here also covers the file's validity on hosts
// with no python3, which build.sh's own check skips.
func TestTheExampleConfigPinsNoBinaryPaths(t *testing.T) {
	path := filepath.Join("..", "..", "contrib", "agent", "agent-controlled.json.example")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read the example config the package ships: %v", err)
	}
	var example AgentFile
	if err := json.Unmarshal(raw, &example); err != nil {
		t.Fatalf("%s does not parse: %v -- it is installed as /etc/vmsync/agent.json.example and copied verbatim", path, err)
	}
	if example.VmsyncPath != "" {
		t.Errorf("%s pins vmsync_path to %q. Leave it unset: the default is asserted against the packager above, and a pinned path has to be moved in lockstep with it",
			path, example.VmsyncPath)
	}
	if example.BridgeHelperPath != "" {
		t.Errorf("%s pins bridge_helper_path to %q. Leave it unset: unset means the agent omits the flag and vmsync's own default answers, which cmd/vmsync/installpath_test.go asserts against the packager",
			path, example.BridgeHelperPath)
	}
}

// And the unit has to exec the agent where the package puts IT, or the service
// a package install leaves behind cannot start at all. Asserted here as well
// as in contrib/packaging/build.sh, for the same reason: `go test ./...` runs
// on every change, a package build does not.
func TestTheShippedUnitExecsThePackagedAgent(t *testing.T) {
	unit, err := os.ReadFile(filepath.Join("..", "..", "contrib", "systemd", "vmsync-agent.service"))
	if err != nil {
		t.Fatalf("cannot read the shipped systemd unit: %v", err)
	}
	want := "ExecStart=" + packagedDestination(t, "vmsync-agent")
	for _, line := range strings.Split(string(unit), "\n") {
		if strings.HasPrefix(line, "ExecStart=") {
			if !strings.HasPrefix(line, want+" ") && line != want {
				t.Fatalf("the unit execs %q but the package installs the agent at %q", line, want)
			}
			return
		}
	}
	t.Fatal("the shipped unit has no ExecStart line")
}
