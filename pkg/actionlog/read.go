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

import "bytes"

// Action is one action as the journal recorded it: its intent, and its outcome
// if it ever produced one.
type Action struct {
	// ActionID and Seq are the join. Both come from the records themselves,
	// never invented here.
	ActionID string
	Seq      int

	Intent     Record
	Outcome    Record
	HasIntent  bool
	HasOutcome bool
}

// Unfinished reports an intent with no outcome beside it.
//
// This is what the journal exists to be able to say. A run that was killed by
// a signal, lost its link or lost its power writes its intent and then nothing
// -- so this is not a hole in the record, it IS the record, and it is the only
// artefact naming the run that left a half-written replica behind.
func (a Action) Unfinished() bool { return a.HasIntent && !a.HasOutcome }

// Orphaned reports an outcome whose intent is no longer here.
//
// Ordinary rather than alarming: only one rotated generation is kept, so an
// action that straddled a rotation loses its older half. Named so a reader is
// not left wondering whether the intent was never written.
func (a Action) Orphaned() bool { return a.HasOutcome && !a.HasIntent }

// Verb names the action, preferring the intent -- which is the record written
// while the caller still knew what it set out to do.
func (a Action) Verb() string {
	if a.HasIntent && a.Intent.Verb != "" {
		return a.Intent.Verb
	}
	return a.Outcome.Verb
}

// StartedAt is when the action began, or when it ended if its intent is gone.
func (a Action) StartedAt() int64 {
	if a.HasIntent {
		return a.Intent.At
	}
	return a.Outcome.At
}

// TornLine is a line that could not be read back as a record.
type TornLine struct {
	// Generation indexes the slice passed to Read, so a reader can say which
	// file it was in. 0 is the first (oldest) one passed.
	Generation int
	// Line is 1-based within that generation.
	Line int
	// Text is the line itself, clipped: it is untrusted content from a file
	// somebody else may have written into, and it gets printed.
	Text string
	Err  string
}

// Reading is everything one pass over a domain's journal found.
type Reading struct {
	// Actions in the order their FIRST record appeared, oldest first, across
	// every generation in the order they were passed.
	Actions []Action
	// Records is how many lines decoded as records.
	Records int
	// Torn is every line that did not. Reported rather than swallowed: a
	// journal that is half unreadable is itself a finding about the filesystem
	// holding the replica.
	Torn []TornLine
}

// Unfinished returns the actions that recorded an intent and never an outcome.
func (r Reading) Unfinished() []Action {
	var out []Action
	for _, a := range r.Actions {
		if a.Unfinished() {
			out = append(out, a)
		}
	}
	return out
}

// tornTextMax bounds what a torn line contributes to a report. The line is by
// definition not a record, so it may be anything at all; printing megabytes of
// it during an incident helps nobody.
const tornTextMax = 200

// maxTornReported bounds how many torn lines are carried. A file that is
// entirely unreadable -- a journal directory somebody pointed at a binary, a
// filesystem returning garbage -- would otherwise produce one entry per line.
// The count that matters is "some", and the first few say what kind.
const maxTornReported = 32

// joinKey is the (aid, seq) pair the join is on.
//
// Seq is part of it and must be: one process performs several actions in a row
// under ONE action id -- a sync and the verify-failure repair that follows it,
// for instance -- and joining on the id alone would fold them into a single
// action whose intent and outcome describe different runs.
type joinKey struct {
	action string
	seq    int
}

// Read joins intents to outcomes across the generations it is given.
//
// Callers pass the OLDEST generation first (the rotated file, then the live
// one), so the resulting order is the order things happened. A generation that
// could not be read at all is passed as nil rather than omitted, which keeps
// the Generation index on a TornLine meaningful.
//
// Tolerant of a torn final line by construction: every line is decoded on its
// own, a failure costs exactly that line, and the last line of a file a power
// cut interrupted mid-append is simply one of those. Nothing here reads ahead
// or carries state between lines, which is the property JSON Lines was chosen
// for.
func Read(generations ...[]byte) Reading {
	var out Reading
	index := map[joinKey]int{}

	for gen, content := range generations {
		if len(content) == 0 {
			continue
		}
		for i, raw := range bytes.Split(content, []byte("\n")) {
			if len(bytes.TrimSpace(raw)) == 0 {
				// The trailing empty field after the final newline, or a blank
				// line somebody left. Neither is torn.
				continue
			}
			rec, err := DecodeLine(raw)
			if err != nil {
				if len(out.Torn) < maxTornReported {
					out.Torn = append(out.Torn, TornLine{
						Generation: gen,
						Line:       i + 1,
						Text:       clip(string(bytes.TrimSpace(raw)), tornTextMax),
						Err:        err.Error(),
					})
				}
				continue
			}
			out.Records++

			key := joinKey{action: rec.Action, seq: rec.Seq}
			at, seen := index[key]
			if !seen {
				out.Actions = append(out.Actions, Action{ActionID: rec.Action, Seq: rec.Seq})
				at = len(out.Actions) - 1
				index[key] = at
			}
			switch rec.Kind {
			case KindIntent:
				// LAST intent wins on a repeated key, which only happens when
				// two processes shared an action id and a seq. Neither record
				// is more right than the other; taking the later one at least
				// pairs it with the outcome that followed it in the file.
				out.Actions[at].Intent = rec
				out.Actions[at].HasIntent = true
			case KindOutcome:
				out.Actions[at].Outcome = rec
				out.Actions[at].HasOutcome = true
			default:
				// A kind this build does not emit -- a step record from a
				// newer vmsync, most likely. Counted as a record and otherwise
				// left alone: a reader that discarded it would under-report
				// what the file holds, and one that guessed at it would invent
				// an action.
			}
		}
	}
	return out
}
