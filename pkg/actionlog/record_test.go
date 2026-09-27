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
	"strings"
	"testing"
)

func sampleIntent() Record {
	return Record{
		Kind:   KindIntent,
		Action: "9f3c1a2b4d5e6f70",
		Seq:    1,
		At:     1758441600,
		Verb:   "sync",
		Domain: "web01",
		Host:   "hv-a",
		PID:    4242,
		RunID:  "run-7",
		Detail: map[string]string{"mode": "reinit"},
	}
}

// The key set is the contract bench and the docs describe, and the one every
// golden test is built on. A key that silently changed name or vanished would
// be found by whoever has to read a journal during an incident, which is the
// worst possible moment.
func TestEncodeEmitsTheAgreedKeys(t *testing.T) {
	line := sampleIntent().Encode()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(line), &m); err != nil {
		t.Fatalf("the record is not JSON: %v (%s)", err, line)
	}
	for _, k := range []string{"v", "k", "aid", "seq", "at", "verb", "res", "exit"} {
		if _, ok := m[k]; !ok {
			t.Errorf("key %q is missing; it is part of the frame every reader depends on: %s", k, line)
		}
	}
	for _, k := range []string{"domain", "host", "pid", "run_id", "d"} {
		if _, ok := m[k]; !ok {
			t.Errorf("key %q is missing although the record carries a value for it: %s", k, line)
		}
	}
	// Context keys the record has nothing for stay out, because they cost
	// bytes against MaxRecordBytes.
	for _, k := range []string{"step", "op_id", "by", "err"} {
		if _, ok := m[k]; ok {
			t.Errorf("key %q was emitted empty: %s", k, line)
		}
	}
	if got := string(m["v"]); got != "1" {
		t.Errorf("v = %s, want 1", got)
	}
}

// One record is one line. Nothing a caller puts in a field may split it,
// because a split record costs the reader the join for BOTH halves.
func TestEncodeIsAlwaysExactlyOneLine(t *testing.T) {
	r := sampleIntent()
	r.Err = "line one\nline two\r\nline three"
	r.Detail = map[string]string{"path": "/data/a\nb/disk.qcow2"}
	line := r.Encode()
	if n := bytes.Count(line, []byte("\n")); n != 1 {
		t.Fatalf("record spans %d newlines, want exactly the trailing one: %q", n, line)
	}
	if !bytes.HasSuffix(line, []byte("\n")) {
		t.Fatalf("record does not end in a newline: %q", line)
	}
	back, err := DecodeLine(line)
	if err != nil {
		t.Fatalf("DecodeLine: %v", err)
	}
	if back.Err != r.Err {
		t.Errorf("newlines did not survive the round trip: %q", back.Err)
	}
}

// HTML escaping off. encoding/json escapes the angle brackets and the
// ampersand by default, which would turn an ordinary shell command or a path
// in an error message into a run of backslash-u sequences an operator has to
// decode by eye in the middle of an incident.
func TestEncodeDoesNotHTMLEscape(t *testing.T) {
	r := sampleIntent()
	r.Err = `cp a b && echo <done> 2>&1`
	line := string(r.Encode())
	// The escape written without writing the escape: anything encoded would
	// appear as a backslash followed by "u00".
	if strings.Contains(line, "\\u00") {
		t.Errorf("the record is HTML-escaped, so an operator has to decode it by eye: %s", line)
	}
	for _, verbatim := range []string{"<done>", "&&", "2>&1"} {
		if !strings.Contains(line, verbatim) {
			t.Errorf("expected %q verbatim in the record: %s", verbatim, line)
		}
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	r := sampleIntent()
	r.OpID = "op-123"
	r.By = "alice"
	back, err := DecodeLine(r.Encode())
	if err != nil {
		t.Fatalf("DecodeLine: %v", err)
	}
	r.V = SchemaVersion
	if back.Kind != r.Kind || back.Action != r.Action || back.Seq != r.Seq || back.At != r.At ||
		back.Verb != r.Verb || back.Domain != r.Domain || back.Host != r.Host || back.PID != r.PID ||
		back.RunID != r.RunID || back.OpID != r.OpID || back.By != r.By || back.V != r.V {
		t.Errorf("round trip lost a field:\n got %+v\nwant %+v", back, r)
	}
	if back.Detail["mode"] != "reinit" {
		t.Errorf("detail lost: %v", back.Detail)
	}
}

func TestEncodeBoundsTheErrorText(t *testing.T) {
	r := sampleIntent()
	r.Kind = KindOutcome
	r.Result = ResultFailed
	r.Err = strings.Repeat("x", 5000)
	back, err := DecodeLine(r.Encode())
	if err != nil {
		t.Fatalf("DecodeLine: %v", err)
	}
	if len(back.Err) > MaxErrBytes {
		t.Errorf("err is %d bytes, over the %d bound", len(back.Err), MaxErrBytes)
	}
	if !strings.HasSuffix(back.Err, "...") {
		t.Errorf("a clipped value must say it was clipped: %q", back.Err)
	}
}

func TestEncodeBoundsTheDetailMap(t *testing.T) {
	d := map[string]string{}
	for _, k := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		d[k] = strings.Repeat(k, 400)
	}
	r := sampleIntent()
	r.Detail = d
	line := r.Encode()
	back, err := DecodeLine(line)
	if err != nil {
		t.Fatalf("DecodeLine: %v", err)
	}
	if len(line) > MaxRecordBytes {
		t.Errorf("record is %d bytes, over the %d bound", len(line), MaxRecordBytes)
	}
	encoded, _ := json.Marshal(back.Detail)
	if len(encoded) > MaxDetailBytes {
		t.Errorf("detail is %d bytes, over the %d bound", len(encoded), MaxDetailBytes)
	}
	// Trimmed rather than silently shortened: a reader has to know the map is
	// not everything the writer knew.
	if back.Detail[detailDroppedKey] == "" {
		t.Errorf("keys were dropped without saying so: %v", back.Detail)
	}
	// Values that survived were clipped rather than left whole.
	if v := back.Detail["a"]; v != "" && len(v) > maxDetailValue {
		t.Errorf("a surviving value was not clipped: %d bytes", len(v))
	}
}

// Deterministic, because two runs of -explain-domain over the same journal
// must produce the same bytes and because a human diffs these.
func TestEncodeIsDeterministic(t *testing.T) {
	d := map[string]string{}
	for _, k := range []string{"zz", "aa", "mm", "bb", "yy", "cc"} {
		d[k] = strings.Repeat(k, 300)
	}
	r := sampleIntent()
	r.Detail = d
	first := string(r.Encode())
	for i := 0; i < 20; i++ {
		if got := string(r.Encode()); got != first {
			t.Fatalf("encoding is not deterministic:\n%s\n%s", first, got)
		}
	}
}

// The last resort. A record that cannot be made to fit still has to carry the
// join, or the intent it belongs to becomes unmatchable and the reader reports
// a run that finished as one that vanished.
func TestEncodeDegradesToAJoinableRecord(t *testing.T) {
	r := Record{
		Kind:   KindOutcome,
		Action: "9f3c1a2b4d5e6f70",
		Seq:    3,
		At:     1758441600,
		Verb:   "sync",
		Domain: strings.Repeat("d", 4000),
		Host:   strings.Repeat("h", 4000),
		RunID:  strings.Repeat("r", 4000),
		OpID:   strings.Repeat("o", 4000),
		By:     strings.Repeat("b", 4000),
		Detail: map[string]string{"x": strings.Repeat("x", 4000)},
		Result: ResultFailed,
		Exit:   1,
		Err:    strings.Repeat("e", 9000),
	}
	line := r.Encode()
	if len(line) > MaxRecordBytes {
		t.Fatalf("degraded record is still %d bytes, over the %d bound: %s", len(line), MaxRecordBytes, line)
	}
	back, err := DecodeLine(line)
	if err != nil {
		t.Fatalf("the degraded record does not decode: %v (%s)", err, line)
	}
	if back.Kind != KindOutcome || back.Action != r.Action || back.Seq != 3 || back.Result != ResultFailed {
		t.Errorf("the join was lost in the degradation: %+v", back)
	}
}

// An action id long enough to crowd out the frame is clipped rather than
// allowed to make the record unreadable.
func TestEncodeClipsTheActionIDAndVerb(t *testing.T) {
	r := sampleIntent()
	r.Action = strings.Repeat("a", 500)
	r.Verb = strings.Repeat("v", 500)
	back, err := DecodeLine(r.Encode())
	if err != nil {
		t.Fatalf("DecodeLine: %v", err)
	}
	if len(back.Action) > MaxActionIDBytes {
		t.Errorf("aid is %d bytes, over the %d bound", len(back.Action), MaxActionIDBytes)
	}
	if len(back.Verb) > maxVerbBytes {
		t.Errorf("verb is %d bytes, over the %d bound", len(back.Verb), maxVerbBytes)
	}
}

// Clipping at a rune boundary: half a rune would become U+FFFD and stop
// matching the hostname or path it names.
func TestClipCutsAtARuneBoundary(t *testing.T) {
	s := strings.Repeat("é", 100) // two bytes each
	got := clip(s, 21)
	if len(got) > 21 {
		t.Fatalf("clip returned %d bytes for a bound of 21", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("no clip marker: %q", got)
	}
	body := strings.TrimSuffix(got, "...")
	if len(body)%2 != 0 {
		t.Errorf("clip cut a two-byte rune in half: %q", got)
	}
}

func TestClipLeavesShortValuesAlone(t *testing.T) {
	if got := clip("web01", 64); got != "web01" {
		t.Errorf("clip mangled a value that fitted: %q", got)
	}
	if got := clip("", 64); got != "" {
		t.Errorf("clip mangled an empty value: %q", got)
	}
}

// The torn final line a power cut leaves behind, and everything else that is
// not a record, must be refused rather than decoded into an empty record --
// an empty record would join against every other one and invent an action
// nobody performed.
func TestDecodeLineRefusesWhatIsNotARecord(t *testing.T) {
	for name, line := range map[string]string{
		"empty":             "",
		"whitespace":        "   \t ",
		"torn mid-record":   `{"v":1,"k":"intent","aid":"9f3c","se`,
		"torn mid-string":   `{"v":1,"k":"inte`,
		"not json at all":   "this is not json",
		"json but not ours": `{"hello":"world"}`,
		"json array":        `[1,2,3]`,
		"kind is blank":     `{"v":1,"k":"  ","aid":"x"}`,
	} {
		if _, err := DecodeLine([]byte(line)); err == nil {
			t.Errorf("%s: decoded %q as a record", name, line)
		}
	}
}

// Forward compatible in one direction only: a record from a newer vmsync --
// a step record, a version this build has never seen -- reads back as found.
// Refusing it would make an upgrade look like a corrupt journal.
func TestDecodeLineAcceptsARecordFromANewerVmsync(t *testing.T) {
	line := `{"v":7,"k":"step","aid":"9f3c","seq":2,"at":1758441600,"verb":"sync","step":"copy-disk","res":"","exit":0,"future":{"x":1}}`
	r, err := DecodeLine([]byte(line))
	if err != nil {
		t.Fatalf("a newer record was refused: %v", err)
	}
	if r.V != 7 || r.Kind != KindStep || r.Step != "copy-disk" || r.Action != "9f3c" || r.Seq != 2 {
		t.Errorf("a newer record was not read as found: %+v", r)
	}
}
