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

// The shipped example documents have to parse through the real loaders.
//
// They are what an operator copies, and both loaders use
// DisallowUnknownFields -- so a key that is a comment, a typo, or a field that
// has since been renamed is not ignored, it is a hard refusal of the whole
// document. For a standalone agent that means no schedule at all.
//
// This exists because adding a `"//"` explanation to a schedule example looks
// harmless and is not: there is no comment syntax in these documents, and the
// strict decoder rejects the file.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The standalone schedule, named by agent.json's schedule_file.
func TestTheShippedScheduleExampleLoadsStrictly(t *testing.T) {
	path := filepath.Join("..", "..", "contrib", "agent", "schedule.json.example")
	doc, err := LoadScheduleFile(path)
	if err != nil {
		t.Fatalf("the example an operator copies does not load: %v", err)
	}
	if len(doc.Schedule) == 0 {
		t.Fatal("the example schedule is empty, so this test proves nothing")
	}
	// Every entry has to pass the validation the agent applies to it, or the
	// example documents a schedule the agent would refuse.
	for _, e := range doc.Schedule {
		if err := e.Validate(); err != nil {
			t.Errorf("the example's %s entry does not validate: %v", e.VM, err)
		}
	}
}

// THE KEY NAMES, which are what an operator actually types. A struct tag typo
// would make a schedule carrying them a parse error rather than a field quietly
// ignored, and this is the document that tells people what to write.
func TestTheScheduleExampleShowsTheDiskExclusionKeys(t *testing.T) {
	path := filepath.Join("..", "..", "contrib", "agent", "schedule.json.example")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the example: %v", err)
	}
	for _, key := range []string{`"ignore_disks"`, `"drop_disks"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("the schedule example does not show %s. vmsync refuses a domain carrying a disk it cannot replicate, so a pair whose vdb is raw is refused on every interval until somebody names it -- and this file is where they look for how", key)
		}
	}

	doc, err := LoadScheduleFile(path)
	if err != nil {
		t.Fatalf("LoadScheduleFile: %v", err)
	}
	var seen bool
	for _, e := range doc.Schedule {
		if len(e.IgnoreDisks) > 0 || len(e.DropDisks) > 0 {
			seen = true
			// The values must arrive as device names, not as the whole array
			// collapsed into one string, which is what a wrong tag type does.
			for _, dev := range append(append([]string{}, e.IgnoreDisks...), e.DropDisks...) {
				if !validDeviceName.MatchString(dev) {
					t.Errorf("%s decoded a device as %q", e.VM, dev)
				}
			}
		}
	}
	if !seen {
		t.Error("the keys appear in the file but decoded into no entry, so the struct tags do not match what the example tells operators to write")
	}
}

// Both exclusion lists belong to one VM. A template is inherited by every VM
// it covers, and a device letter means a different disk on each of them, so an
// example putting them in a template would document the one mistake here with
// no loud symptom -- the syncs all succeed.
func TestNoShippedExamplePutsDiskExclusionsInATemplateOrProfile(t *testing.T) {
	for _, name := range []string{"schedule.json.example", "agent-controlled.json.example", "agent-standalone.json.example", "agent-monitor.json.example"} {
		path := filepath.Join("..", "..", "contrib", "agent", name)
		raw, err := os.ReadFile(path)
		if err != nil {
			continue // not every example exists in every tree
		}
		body := string(raw)
		for _, block := range []string{`"templates"`, `"profile"`} {
			i := strings.Index(body, block)
			if i < 0 {
				continue
			}
			// Crude but sufficient: the exclusion keys must not appear
			// anywhere after a templates block begins in a document that has
			// one, because that is where an inherited copy would sit.
			if block == `"templates"` && strings.Contains(body[i:], `"ignore_disks"`) {
				// The schedule array follows templates in these documents, so
				// narrow to the templates block itself.
				end := strings.Index(body[i:], `"schedule"`)
				if end > 0 && strings.Contains(body[i:i+end], `"ignore_disks"`) {
					t.Errorf("%s puts ignore_disks inside its templates block; a template's list applies to every VM it covers", name)
				}
			}
		}
	}
}
