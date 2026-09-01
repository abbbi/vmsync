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

// Package runresult is how one vmsync run tells its parent what happened,
// beyond what an exit code can carry.
//
// The parent is vmsync-agent, and until now it had exactly two channels: the
// exit status, and the last 4000 bytes of output. Neither can carry a
// DEGRADATION -- an outcome that is not the run's success or failure but a
// fact about the guest or the copy that outlives the run:
//
//   - The exit code is one value and it is already spoken for. A degraded
//     run succeeded; making it exit non-zero would drive
//     -reinit-after-failures toward discarding a healthy replica.
//   - The log tail is bounded and a warning logged early in a long sync has
//     scrolled out of it by the end. Freeze happens in the first seconds of
//     a run that can take hours, so the tail is exactly the wrong place to
//     look for it.
//
// So vmsync writes this file instead, at the same points it writes the
// Prometheus textfile -- including the os.Exit paths, which matters because
// the run that both failed to quiesce AND then failed is the one where the
// degradation matters most.
//
// Distinct from the Prometheus textfile on purpose. That file is optional
// (an operator who runs no node_exporter sets no path), it is world-readable
// monitoring output, and its schema is a public contract with dashboards.
// This one is private between the two binaries, always written, and read
// once and unlinked.
package runresult

import (
	"encoding/json"
	"fmt"
	"os"

	"vmsync/pkg/atomicjson"
)

// Version is the schema version, written into every file.
//
// Read leniently rather than strictly: this is vmsync's own output being
// read by its own agent, and the pair can be at different versions during a
// rolling upgrade. An older agent reading a newer vmsync's file must ignore
// what it does not know rather than refuse the whole file, because refusing
// it loses the fields it DOES understand -- and losing a thaw failure to a
// schema quibble is the worst possible trade.
const Version = 1

// Result is what one run reports about itself.
//
// Every field is a fact the parent cannot get any other way. Nothing that is
// already in the exit code or in the metrics textfile belongs here.
type Result struct {
	Version int `json:"version"`
	// VM is the source domain, so a file found on its own is identifiable.
	VM string `json:"vm,omitempty"`
	// RunID echoes -run-id, letting the agent prove the file it just read
	// came from the child it just waited for rather than from a stale one a
	// previous crash left behind.
	RunID string `json:"run_id,omitempty"`

	// FSFreezeFailed says the guest filesystems could not be quiesced, so
	// whatever this run copied is crash-consistent only.
	FSFreezeFailed bool `json:"fsfreeze_failed,omitempty"`
	// FSThawFailed says the source guest was left FROZEN. It is still frozen
	// now, after this process exited, and will block on every write until
	// somebody thaws it by hand.
	FSThawFailed bool `json:"fsthaw_failed,omitempty"`
}

// Degraded reports whether anything here needs a person.
func (r Result) Degraded() bool { return r.FSFreezeFailed || r.FSThawFailed }

// Reason is the degradation in an operator's words, empty when there is
// none.
//
// Written here rather than in the UI so both ends say the same thing, and so
// the sentence lives next to the flag that means it.
func (r Result) Reason() string {
	switch {
	case r.FSThawFailed && r.FSFreezeFailed:
		// Possible: a freeze that fails partway can leave some filesystems
		// frozen, and the thaw that should undo that can fail too.
		return "the guest filesystems are still FROZEN and the copy is crash-consistent only — " +
			"run `virsh domfsthaw " + r.VM + "` on the source host now"
	case r.FSThawFailed:
		// First, because it is the only one that is still happening. The
		// copy is finished and cannot get worse; the guest is blocked right
		// now and stays blocked until somebody acts.
		return "the guest filesystems are still FROZEN: the source VM blocks on every write until " +
			"somebody runs `virsh domfsthaw " + r.VM + "` on its host"
	case r.FSFreezeFailed:
		return "the guest filesystems could not be quiesced, so this copy is crash-consistent only — " +
			"a database restored from it recovers as if the host had lost power"
	}
	return ""
}

// Write puts r at path, atomically.
//
// Atomically because the reader is a separate process that may look at any
// moment, and half a result read as a whole one reports a degradation that
// did not happen or misses one that did.
func Write(path string, r Result) error {
	r.Version = Version
	return atomicjson.Write(path, r, 0o600)
}

// Read loads a result, or reports that there is none.
//
// A missing file is NOT an error: the run may have died before writing one,
// or be an older vmsync that does not write them at all. Callers get the
// zero Result, which reports no degradation -- fail-open, and deliberately
// so. The alternative is refusing to record an otherwise fine run because a
// file the run itself may never have created is not there.
//
// A file that IS there but will not parse is a different matter, and is
// returned as an error: something wrote it, so something is wrong.
func Read(path string) (Result, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Result{}, nil
	}
	if err != nil {
		return Result{}, err
	}
	var r Result
	// Deliberately NOT DisallowUnknownFields; see Version.
	if err := json.Unmarshal(b, &r); err != nil {
		return Result{}, fmt.Errorf("parse run result %s: %w", path, err)
	}
	return r, nil
}
