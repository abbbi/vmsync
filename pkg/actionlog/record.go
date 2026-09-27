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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// SchemaVersion is the "v" every record carries.
//
// Present from the first record ever written, not added the day the format
// first has to change: a journal is read years after it was written, by a
// build that did not exist when it was, and a record with no version is one a
// future reader has to guess about.
const SchemaVersion = 1

// The two kinds of record. Exactly two are ever EMITTED today.
const (
	// KindIntent is written BEFORE the action acts, and is the whole point:
	// an intent with no outcome beside it is a run that started here and never
	// came back, which is the only trace an os.Exit'ing signal handler, a lost
	// link or a power cut leaves behind.
	KindIntent = "intent"
	// KindOutcome is written when the action stops, however it stopped.
	KindOutcome = "outcome"
	// KindStep is reserved and deliberately NOT emitted at the "actions"
	// journal level, which is the only level there is today. The schema
	// carries "step" and "seq" so per-step records can be added later without
	// a format change -- readers already ignore a kind they do not know, and
	// the intent<->outcome join is by (aid, seq), which step records would
	// share rather than disturb.
	KindStep = "step"
)

// What an outcome record says happened. The caller states it rather than
// having it inferred from an error, because the three are genuinely different
// outcomes and two of them are not failures of anything.
const (
	// ResultOK: the action did what its intent said it would.
	ResultOK = "ok"
	// ResultFailed: it did not.
	ResultFailed = "failed"
	// ResultBusy: it stood down without touching anything because another
	// vmsync held the lock. Not a failure, and recording it as one would make
	// every scheduled overlap look like a broken replica in the history.
	ResultBusy = "busy"
)

// Bounds. Every one of them exists so that a single pathological record --
// a multi-megabyte error from a remote shell, a detail map built from a
// directory listing -- cannot fill the target's filesystem or make the file
// unreadable for everything written after it.
const (
	// MaxDetailBytes bounds the encoded "d" map.
	MaxDetailBytes = 1024
	// MaxErrBytes bounds "err". An error longer than this has already said
	// what it is going to say; the rest is a stack of wrappers.
	MaxErrBytes = 512
	// MaxRecordBytes bounds the whole line, newline included. Also the reason
	// the append can stay atomic: see AppendCommand.
	MaxRecordBytes = 2048

	// MaxActionIDBytes matches vmsync-agent's own bound on what it will pass
	// as -action-id, and libvirtsync.ReplicaIncompleteValue's bound on what it
	// will carry. Three places, one number, because the id has to survive all
	// three to be the join it is meant to be.
	MaxActionIDBytes = 64

	maxVerbBytes     = 32
	maxStepBytes     = 32
	maxDetailKey     = 32
	maxDetailValue   = 256
	degradedErrBytes = 128
)

// detailDroppedKey names how many detail keys were dropped to fit the bound.
// Reported rather than silent: a detail map that was trimmed is still useful,
// but a reader has to know it is not the whole of what the writer knew.
const detailDroppedKey = "_dropped"

// detailMarkerCost reserves room for detailDroppedKey and its value so that
// adding the marker cannot push a map that just fitted back over the bound.
const detailMarkerCost = 24

// degradedErr is what a record says about itself when it could not be written
// whole. Fixed text, so it is greppable and so the minimal record's size is
// knowable by construction.
const degradedErr = "vmsync-journal: record degraded to fit its size bound"

// Record is one line of the journal.
//
// FIELD ORDER IS THE WIRE ORDER, because encoding/json emits struct fields in
// declaration order. It is chosen for a human reading a raw line during an
// incident: what this is, which action it belongs to, when, what it was doing,
// then the context, then how it ended.
//
// WHICH KEYS ARE ALWAYS PRESENT is a deliberate split rather than an
// inconsistency. v/k/aid/seq/at/verb/res/exit are the frame every reader and
// every golden test is built on, and a key that sometimes vanishes is one a
// reader has to special-case for ever; the rest is context that is often
// genuinely absent and costs bytes against MaxRecordBytes when it is.
type Record struct {
	V      int    `json:"v"`
	Kind   string `json:"k"`
	Action string `json:"aid"`
	// Seq numbers the ACTION within the process that wrote it, from 1. An
	// intent and its outcome share it, and (aid, seq) is the join. Step
	// records, if they are ever emitted, would carry the same pair and be told
	// apart by "step" -- which is why the join must never be on aid alone.
	Seq int `json:"seq"`
	// At is when this record was written, in unix seconds. The intent's is
	// when the action STARTED, which is the number an operator compares
	// against the replica_incomplete marker on the domain.
	At   int64  `json:"at"`
	Verb string `json:"verb"`
	// Step is always empty at the "actions" journal level. See KindStep.
	Step   string `json:"step,omitempty"`
	Domain string `json:"domain,omitempty"`
	// Host is the machine that RAN the action, which for a sync is not the
	// machine this file is stored on: the journal lives beside the disks, on
	// the target, while the sync runs from the source. Recording it is the
	// only way a reader on the DR host learns where to go looking.
	Host   string            `json:"host,omitempty"`
	PID    int               `json:"pid,omitempty"`
	RunID  string            `json:"run_id,omitempty"`
	OpID   string            `json:"op_id,omitempty"`
	By     string            `json:"by,omitempty"`
	Detail map[string]string `json:"d,omitempty"`
	Result string            `json:"res"`
	Exit   int               `json:"exit"`
	Err    string            `json:"err,omitempty"`
}

// Encode renders one journal line, newline included.
//
// IT CANNOT FAIL, and that is a design decision rather than an accident of the
// types involved. A journal entry that is refused is an entry that is lost,
// and the entry most likely to be pathological -- a two-megabyte error from a
// remote shell, a detail map assembled from a directory listing -- is
// precisely the one describing the run worth reading about. So an oversized
// record DEGRADES: first its detail map is trimmed, then dropped, then its
// error text is cut, and in the last resort it falls back to a minimal record
// that still carries k/aid/seq/res. That minimum is what keeps the
// intent<->outcome join intact, which is the one property of this format that
// a reader cannot work around.
//
// The result is always exactly one line: encoding/json escapes newlines inside
// strings, so nothing a caller puts in a field can split a record in two.
func (r Record) Encode() []byte {
	r.V = SchemaVersion
	// Bounded before anything else, and unconditionally, so that the minimal
	// record below is small BY CONSTRUCTION rather than by hope: with aid and
	// verb capped, the fallback cannot itself overflow.
	r.Action = clip(r.Action, MaxActionIDBytes)
	r.Verb = clip(r.Verb, maxVerbBytes)
	r.Step = clip(r.Step, maxStepBytes)
	r.Err = clip(r.Err, MaxErrBytes)
	r.Detail = boundDetail(r.Detail)

	if line, err := marshalLine(r); err == nil && len(line) <= MaxRecordBytes {
		return line
	}

	// Ladder, cheapest loss first. The detail map is context; the error text
	// is the finding; the frame is the join.
	dropped := r
	dropped.Detail = nil
	if line, err := marshalLine(dropped); err == nil && len(line) <= MaxRecordBytes {
		return line
	}
	dropped.Err = clip(dropped.Err, degradedErrBytes)
	if line, err := marshalLine(dropped); err == nil && len(line) <= MaxRecordBytes {
		return line
	}

	minimal := Record{
		V:      SchemaVersion,
		Kind:   r.Kind,
		Action: r.Action,
		Seq:    r.Seq,
		At:     r.At,
		Verb:   r.Verb,
		Result: r.Result,
		Exit:   r.Exit,
		Err:    degradedErr,
	}
	line, err := marshalLine(minimal)
	if err != nil {
		// Unreachable: every field of minimal is a string or an int, and
		// encoding/json cannot fail on those. Answered anyway rather than
		// panicking, because a panic inside a diagnostic would take down the
		// action the diagnostic exists to describe.
		return []byte(`{"v":1,"k":"` + r.Kind + `","aid":"","seq":0,"at":0,"verb":"","res":"","exit":0,"err":"` + degradedErr + `"}` + "\n")
	}
	return line
}

// marshalLine encodes one record as a single line.
//
// json.Encoder with HTML escaping OFF, not json.Marshal: the default escapes
// '<', '>' and '&' into < and friends, which would turn an ordinary shell
// command or a path in an error message into something an operator has to
// decode by eye during an incident. Encode appends the newline itself, so the
// record IS the line.
func marshalLine(r Record) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(r); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// boundDetail trims a detail map to MaxDetailBytes, deterministically.
//
// Deterministic because golden tests and a human diffing two runs both depend
// on it: keys are trimmed in reverse sorted order, which is the order
// encoding/json emits them in reversed, so what survives is the head of the
// map a reader sees first.
func boundDetail(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in)+1)
	keys := make([]string, 0, len(in))
	for k, v := range in {
		k = clip(k, maxDetailKey)
		if k == detailDroppedKey {
			// Reserved for the marker below. A caller's own key of that name
			// would make the count it reports a lie.
			continue
		}
		if _, seen := out[k]; !seen {
			keys = append(keys, k)
		}
		out[k] = clip(v, maxDetailValue)
	}
	sort.Strings(keys)

	dropped := 0
	for {
		limit := MaxDetailBytes
		if dropped > 0 {
			limit -= detailMarkerCost
		}
		b, err := json.Marshal(out)
		if err == nil && len(b) <= limit {
			break
		}
		if len(keys) == 0 {
			// Nothing left to drop and it still does not fit, which can only
			// mean the marker alone is over the limit. Give up on the map
			// rather than loop; Encode's own ladder handles the rest.
			return map[string]string{detailDroppedKey: strconv.Itoa(dropped)}
		}
		last := keys[len(keys)-1]
		keys = keys[:len(keys)-1]
		delete(out, last)
		dropped++
	}
	if dropped > 0 {
		out[detailDroppedKey] = strconv.Itoa(dropped)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// clip cuts a string to max bytes at a rune boundary, marking that it was cut.
//
// At a boundary because a record is JSON: half a multi-byte rune would be
// escaped as U+FFFD by the encoder, which is merely ugly -- but cutting an
// operator's hostname or a path mid-character is how a truncated value stops
// matching the thing it names.
func clip(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	const ellipsis = "..."
	cut := max - len(ellipsis)
	if cut < 0 {
		cut = 0
	}
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ellipsis
}

// DecodeLine reads one journal line back.
//
// Tolerant on purpose, in one direction only: a record carrying a kind or a
// version this build has never heard of is returned AS FOUND rather than
// refused, so a journal written by a newer vmsync still reads here. What is
// refused is a line that is not a record at all -- the torn final line a power
// cut leaves behind, or anything else that found its way into the file -- and
// the reader counts those rather than stopping at them.
func DecodeLine(line []byte) (Record, error) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return Record{}, errors.New("empty line")
	}
	var r Record
	if err := json.Unmarshal(line, &r); err != nil {
		return Record{}, fmt.Errorf("not a journal record: %w", err)
	}
	if strings.TrimSpace(r.Kind) == "" {
		// Valid JSON that is not one of ours. Refused rather than returned as
		// an empty record, which would join against every other empty record
		// and invent an action nobody performed.
		return Record{}, errors.New("not a journal record: no \"k\"")
	}
	return r, nil
}
