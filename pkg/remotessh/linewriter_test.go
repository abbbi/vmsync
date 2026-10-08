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

package remotessh

import (
	"strings"
	"testing"
)

// TestLineWriterEmitsEachLineAsItArrives is the property the whole point rests
// on: a line must reach the caller when it is WRITTEN, not when the stream
// ends. A remote pass that reports progress for forty minutes and then dies
// has to have reported it, and an implementation that waited for EOF would
// have delivered nothing at all -- which is the failure this writer exists to
// fix.
func TestLineWriterEmitsEachLineAsItArrives(t *testing.T) {
	var got []string
	w := &lineWriter{onLine: func(s string) { got = append(got, s) }}

	// Arrives in pieces that do not respect line boundaries, because that is
	// what a network stream does.
	for _, chunk := range []string{"first li", "ne\nsecond line\nthi", "rd line\n"} {
		n, err := w.Write([]byte(chunk))
		if err != nil {
			t.Fatalf("Write(%q): %v", chunk, err)
		}
		if n != len(chunk) {
			t.Fatalf("Write(%q) wrote %d bytes, want %d", chunk, n, len(chunk))
		}
	}

	want := []string{"first line", "second line", "third line"}
	if len(got) != len(want) {
		t.Fatalf("got %d lines %q, want %d %q", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestLineWriterDropsPartialAndEmptyLines pins two deliberate omissions.
//
// A trailing fragment is never emitted: the only way to see one is for the
// stream to end mid-line, which means the session died, and half a progress
// line is not worth logging as though it were news. The caller still gets the
// complete text from the buffer alongside it.
//
// Blank lines are dropped because a remote command that ends its output with a
// newline would otherwise log an empty line after every real one.
func TestLineWriterDropsPartialAndEmptyLines(t *testing.T) {
	var got []string
	w := &lineWriter{onLine: func(s string) { got = append(got, s) }}

	if _, err := w.Write([]byte("done\n\n\nhalf a line, no newline")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(got) != 1 || got[0] != "done" {
		t.Fatalf("got %q, want exactly [\"done\"]", got)
	}
}

// TestLineWriterTrimsCarriageReturns keeps a CRLF-emitting remote from putting
// a stray \r into the middle of a log line.
func TestLineWriterTrimsCarriageReturns(t *testing.T) {
	var got []string
	w := &lineWriter{onLine: func(s string) { got = append(got, s) }}
	if _, err := w.Write([]byte("progress 50%\r\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(got) != 1 || got[0] != "progress 50%" {
		t.Fatalf("got %q, want [\"progress 50%%\"]", got)
	}
}

// TestLineWriterBoundsItsBuffer is the safety property: the far side is a
// process on another host, and one that writes without ever sending a newline
// must not be able to grow this buffer until vmsync runs out of memory.
func TestLineWriterBoundsItsBuffer(t *testing.T) {
	var got []string
	w := &lineWriter{onLine: func(s string) { got = append(got, s) }}

	blob := strings.Repeat("x", 8*1024)
	for i := 0; i < 64; i++ { // 512 KiB, no newline anywhere
		if _, err := w.Write([]byte(blob)); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}
	if len(got) != 0 {
		t.Errorf("emitted %d lines from input containing no newline: %q", len(got), got)
	}
	if len(w.buf) > 64*1024 {
		t.Errorf("buffer grew to %d bytes with no newline in sight; it must stay bounded", len(w.buf))
	}

	// The newline that finally arrives ends the over-long line, so it is
	// consumed and nothing is emitted for it. That is the point: the bytes
	// after an overflow belong to the line being thrown away, and logging
	// them would mean logging a real-looking line with a kilobyte of someone
	// else's output glued to its front.
	if _, err := w.Write([]byte("the rest of the oversized line\n")); err != nil {
		t.Fatalf("Write ending the oversized line: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("emitted %q for the tail of a line that was already discarded", got)
	}

	// The NEXT line, though, must come through intact -- an overflow must
	// cost one line, not the rest of the stream.
	if _, err := w.Write([]byte("back to normal\n")); err != nil {
		t.Fatalf("Write after overflow: %v", err)
	}
	if len(got) != 1 || got[0] != "back to normal" {
		t.Errorf("after an oversized line, got %q, want exactly [\"back to normal\"]", got)
	}
}
