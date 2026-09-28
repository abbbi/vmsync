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
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Explanation is everything -explain-domain prints, gathered by cmd/vmsync and
// rendered here.
//
// Split this way for one reason: the gathering needs libvirt and a filesystem,
// and the rendering is the part that has to be right during an incident. Kept
// here it is a pure function of plain values, so the exact bytes an operator
// will read are pinned by golden tests on any machine -- which is not true of
// anything inside cmd/vmsync.
type Explanation struct {
	Domain string
	URI    string

	// JournalPath is the live file. JournalNote says why it could not be read,
	// when it could not -- empty otherwise.
	JournalPath string
	JournalNote string

	// DomainNote is said about the DOMAIN rather than about the journal, and
	// exists for one case: there is no domain. An interrupted -force-clean
	// undefines the target and then dies part-way through rewriting its disks,
	// so the disks and this journal are all that is left -- which is precisely
	// when somebody needs to read it. Empty in every ordinary case.
	DomainNote string

	// ReplicaIncomplete is the domain's own marker, raw and unparsed, or empty
	// when there is none. ReplicaIncompleteNote is what pkg/failover makes of
	// it, rendered by the caller: this package must not depend on the decision
	// logic, and the decision logic must not depend on this package.
	ReplicaIncomplete     string
	ReplicaIncompleteNote string

	// ServedLiveNote is what the caller makes of the domain's durable
	// promotion trace, rendered rather than decided here -- same split as
	// ReplicaIncompleteNote, and for the same reason.
	//
	// Reported by this verb specifically because it is the verb somebody runs
	// when a command has just been refused and the reason is not on screen any
	// more. The trace is the one refusal that survives a role change, so it is
	// also the one an operator is most likely to be looking at without knowing
	// it: the domain reads `paused`, everything else about it reads healthy,
	// and three separate commands say no.
	ServedLiveNote string

	Reading Reading

	// Limit caps how many actions are listed, newest kept. Zero lists them
	// all.
	Limit int
}

// Render writes the report.
//
// ORDER IS THE ARGUMENT. The marker on the domain comes first because it is
// the thing that refuses a promotion and therefore the thing somebody is
// standing in front of; the action list comes second because it says which run
// put it there; the unfinished actions come last, spelled out, because that is
// the answer to the question the other two raise.
func (e Explanation) Render(w io.Writer) {
	fmt.Fprintf(w, "\nvmsync journal for %s\n", e.Domain)
	fmt.Fprintf(w, "  libvirt uri     %s\n", orUnknown(e.URI))
	fmt.Fprintf(w, "  journal         %s\n", orUnknown(e.JournalPath))
	if e.JournalNote != "" {
		fmt.Fprintf(w, "                  %s\n", e.JournalNote)
	}
	fmt.Fprintf(w, "  records read    %d in %d action(s)\n", e.Reading.Records, len(e.Reading.Actions))

	// --- the marker on the domain -------------------------------------------
	fmt.Fprintf(w, "\nreplica_incomplete\n")
	switch {
	case e.DomainNote != "":
		// No domain at all, so there is no metadata to carry a marker. Said
		// first and in its own branch, because "(none)" here would read as a
		// clean bill of health for a replica that may be the worst case there
		// is.
		for _, line := range wrapNote(e.DomainNote) {
			fmt.Fprintf(w, "  %s\n", line)
		}
	case e.ReplicaIncomplete == "":
		fmt.Fprintf(w, "  (none) -- this domain carries no record of an interrupted full copy\n")
	default:
		fmt.Fprintf(w, "  %s\n", e.ReplicaIncomplete)
		if e.ReplicaIncompleteNote != "" {
			for _, line := range wrapNote(e.ReplicaIncompleteNote) {
				fmt.Fprintf(w, "  %s\n", line)
			}
		}
	}

	// --- has this copy ever served live? -------------------------------------
	//
	// Its own section directly under the marker, because the two are the pair
	// of findings that refuse things while everything else about the domain
	// reads clean -- and unlike the marker, this one is not cleared by
	// re-running anything. Printed only when there is something to say: a
	// "(none)" line here on every ordinary replica would push the action list
	// down the terminal for no information.
	if e.ServedLiveNote != "" {
		fmt.Fprintf(w, "\nlast_promoted_at\n")
		for _, line := range wrapNote(e.ServedLiveNote) {
			fmt.Fprintf(w, "  %s\n", line)
		}
	}

	// --- what this journal recorded -----------------------------------------
	actions := e.Reading.Actions
	shown := actions
	if e.Limit > 0 && len(shown) > e.Limit {
		shown = shown[len(shown)-e.Limit:]
	}
	fmt.Fprintf(w, "\nrecent actions, oldest first (%d of %d)\n", len(shown), len(actions))
	if len(shown) == 0 {
		fmt.Fprintf(w, "  (nothing recorded)\n")
	} else {
		fmt.Fprintf(w, "  %-19s  %-21s  %-10s  %4s  %s\n", "STARTED", "VERB", "OUTCOME", "EXIT", "ACTION")
		for _, a := range shown {
			fmt.Fprintf(w, "  %-19s  %-21s  %-10s  %4s  %s\n",
				stamp(a.StartedAt()), orNone(a.Verb()), outcomeWord(a), exitWord(a), orNone(a.ActionID))
		}
	}

	// --- the finding ---------------------------------------------------------
	unfinished := e.Reading.Unfinished()
	if len(unfinished) > 0 {
		fmt.Fprintf(w, "\n%d action(s) recorded an intent and NEVER an outcome.\n", len(unfinished))
		fmt.Fprintf(w, "  Each is a run that started against this replica and stopped without saying so:\n")
		fmt.Fprintf(w, "  a signal (vmsync's handler exits the process, so nothing deferred runs), a dropped\n")
		fmt.Fprintf(w, "  link, or a lost power feed. If one of them was a full copy, the disks beside this\n")
		if e.DomainNote != "" {
			// There is no marker to point at, and saying there is would
			// contradict the note two paragraphs up. What refuses a promotion
			// here is the absence of the domain itself.
			fmt.Fprintf(w, "  journal may be the half-written set it left behind. Nothing above them records\n")
			fmt.Fprintf(w, "  that, because there is no domain above them: -promote reports only that there is\n")
			fmt.Fprintf(w, "  nothing here to promote, which is true and is not the whole story.\n")
		} else {
			fmt.Fprintf(w, "  journal may be the half-written set it left behind -- replica_incomplete above is\n")
			fmt.Fprintf(w, "  what refuses to promote them, and it names the aside files holding the good copy.\n")
		}
		for _, a := range unfinished {
			fmt.Fprintf(w, "    %s  %s  action=%s seq=%d host=%s pid=%d%s\n",
				stamp(a.Intent.At), orNone(a.Intent.Verb), orNone(a.ActionID), a.Seq,
				orNone(a.Intent.Host), a.Intent.PID, detailSuffix(a.Intent.Detail))
		}
	}

	// --- outcomes worth reading ---------------------------------------------
	var failed []Action
	for _, a := range shown {
		if a.HasOutcome && a.Outcome.Result != ResultOK && a.Outcome.Result != "" {
			failed = append(failed, a)
		}
	}
	if len(failed) > 0 {
		fmt.Fprintf(w, "\n%d action(s) ended other than \"ok\":\n", len(failed))
		for _, a := range failed {
			fmt.Fprintf(w, "    %s  %s  %s exit=%d  %s\n",
				stamp(a.Outcome.At), orNone(a.Verb()), a.Outcome.Result, a.Outcome.Exit, orNone(a.Outcome.Err))
		}
	}

	// --- what could not be read ---------------------------------------------
	if len(e.Reading.Torn) > 0 {
		fmt.Fprintf(w, "\n%d line(s) of this journal could not be read back as a record.\n", len(e.Reading.Torn))
		fmt.Fprintf(w, "  The final line of a file whose append was interrupted looks exactly like this and\n")
		fmt.Fprintf(w, "  costs exactly that one record; several of them mean something else is wrong here.\n")
		for _, t := range e.Reading.Torn {
			fmt.Fprintf(w, "    generation %d line %d: %s\n", t.Generation, t.Line, t.Err)
			fmt.Fprintf(w, "      %s\n", t.Text)
		}
	}
	fmt.Fprintf(w, "\nThis journal is evidence, never an input to a decision: no refusal anywhere in\n")
	fmt.Fprintf(w, "vmsync reads it. Nothing was changed by printing it.\n\n")
}

// outcomeWord is the OUTCOME column: what happened, or that nothing said.
func outcomeWord(a Action) string {
	switch {
	case a.Unfinished():
		// Upper case, deliberately: it is the one row in this table that is a
		// finding rather than a fact, and it must not read as just another
		// state word in a column of lower-case ones.
		return "UNFINISHED"
	case !a.HasOutcome:
		return "(none)"
	case a.Outcome.Result == "":
		return "(unstated)"
	default:
		return a.Outcome.Result
	}
}

func exitWord(a Action) string {
	if !a.HasOutcome {
		return "-"
	}
	return strconv.Itoa(a.Outcome.Exit)
}

// detailSuffix renders a detail map deterministically, sorted, so two runs of
// this command over the same journal produce the same bytes.
func detailSuffix(d map[string]string) string {
	if len(d) == 0 {
		return ""
	}
	keys := make([]string, 0, len(d))
	for k := range d {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+d[k])
	}
	return " {" + strings.Join(parts, " ") + "}"
}

// stamp renders a unix second in UTC.
//
// UTC always, never local: this is read next to timestamps written by the
// OTHER host, and a report that silently shifted one of them by the reader's
// timezone would make two clocks look like they disagree when they do not.
func stamp(unix int64) string {
	if unix <= 0 {
		return "(no time)"
	}
	return time.Unix(unix, 0).UTC().Format("2006-01-02 15:04:05")
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func orUnknown(s string) string {
	if s == "" {
		return "(unknown)"
	}
	return s
}

// wrapNote breaks the caller's explanation into lines at a width a terminal
// during an incident actually has, without reflowing anything it already
// broke itself.
func wrapNote(note string) []string {
	const width = 76
	var out []string
	for _, para := range strings.Split(note, "\n") {
		words := strings.Fields(para)
		if len(words) == 0 {
			continue
		}
		line := words[0]
		for _, wd := range words[1:] {
			if len(line)+1+len(wd) > width {
				out = append(out, line)
				line = wd
				continue
			}
			line += " " + wd
		}
		out = append(out, line)
	}
	return out
}
