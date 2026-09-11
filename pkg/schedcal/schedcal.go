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

// Package schedcal answers one question: does this local time fall inside a
// recurring window, described the way systemd describes calendar days?
//
// It exists so a verification can be scheduled as "the first Sunday of the
// month, between 02:00 and 12:00" rather than as an interval. An interval
// cannot express a maintenance window and it DRIFTS -- a VM verified at 02:00
// today is verified mid-morning two months later, during exactly the hours
// that were being avoided.
//
// # What it deliberately does not do
//
// There is no "when does this next fire". That is where cron implementations
// get hairy, and a window makes it unnecessary: the only question is whether
// NOW matches, plus a caller-held timestamp of the last occurrence acted on.
// So there is no next-fire arithmetic, no leap handling, and no "this
// expression matches nothing" case to detect.
//
// # Why systemd's syntax
//
// vmsync-agent ships as systemd units, so anyone writing this configuration
// is already writing systemd -- but familiarity is not the decisive reason.
// `systemd-analyze calendar "Sun *-*-01..07"` lets an operator TEST an
// expression before pasting it in. A bespoke mini-language with no way to
// check it is how a monthly verification silently never fires and nobody
// notices for a year.
//
// Only the DAY part is accepted: the window carries the time. A full systemd
// expression with a time component is rejected with a message saying where
// the time belongs, rather than being silently half-understood.
//
// Everything here is pure and works on a time.Time in whatever location it
// carries. The agent passes its own local time, from the system, because that
// is the machine in the datacentre whose quiet hours these are.
package schedcal

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Days is a parsed day expression: which dates a window may open on.
//
// The zero value matches every day, which is what an empty expression means.
type Days struct {
	// weekdays is nil for "any". Otherwise indexed by time.Weekday.
	weekdays map[time.Weekday]bool
	// months and mdays are nil for "any" (the "*" in a date field).
	months map[time.Month]bool
	mdays  map[int]bool
	// years is nil for "any". Rarely useful, accepted for completeness with
	// the syntax rather than because anyone wants a one-year schedule.
	years map[int]bool
	// text is the expression as written, for error messages and display.
	text string
}

// String returns the expression as written, or "every day" for the zero value.
func (d Days) String() string {
	if d.text == "" {
		return "every day"
	}
	return d.text
}

// Matches reports whether t's date satisfies every stated field.
//
// Fields are ANDed, which is what makes "the first Sunday" expressible at all:
// `Sun *-*-01..07` is day-of-week Sunday AND day-of-month 1-7, and only one
// date a month can be both.
func (d Days) Matches(t time.Time) bool {
	if d.weekdays != nil && !d.weekdays[t.Weekday()] {
		return false
	}
	if d.years != nil && !d.years[t.Year()] {
		return false
	}
	if d.months != nil && !d.months[t.Month()] {
		return false
	}
	if d.mdays != nil && !d.mdays[t.Day()] {
		return false
	}
	return true
}

var weekdayNames = map[string]time.Weekday{
	"sun": time.Sunday, "sunday": time.Sunday,
	"mon": time.Monday, "monday": time.Monday,
	"tue": time.Tuesday, "tues": time.Tuesday, "tuesday": time.Tuesday,
	"wed": time.Wednesday, "weds": time.Wednesday, "wednesday": time.Wednesday,
	"thu": time.Thursday, "thurs": time.Thursday, "thursday": time.Thursday,
	"fri": time.Friday, "friday": time.Friday,
	"sat": time.Saturday, "saturday": time.Saturday,
}

// weekdayOrder is the cycle Mon..Fri style ranges walk, in systemd's order.
var weekdayOrder = []time.Weekday{
	time.Monday, time.Tuesday, time.Wednesday, time.Thursday,
	time.Friday, time.Saturday, time.Sunday,
}

// ParseDays parses the day part of a systemd calendar expression.
//
// Accepted:
//
//	""                     every day
//	"Sun"                  every Sunday
//	"Mon..Fri"             weekdays
//	"Sat,Sun"              either
//	"*-*-01"               the first of every month
//	"*-*-01..07"           the first week of every month
//	"Sun *-*-01..07"       the first Sunday of every month
//	"*-01,07-*"            any day in January or July
//
// Rejected, each by name rather than as a generic parse failure:
// a time component (it belongs in the window), systemd's `~` last-day
// syntax, `/` repetition, timezone suffixes, and the shorthand keywords
// (daily, weekly, monthly) -- all of which are real systemd and would
// otherwise be silently half-understood.
func ParseDays(expr string) (Days, error) {
	text := strings.TrimSpace(expr)
	if text == "" {
		return Days{}, nil
	}
	d := Days{text: text}

	if strings.Contains(text, ":") {
		// The suggestion keeps everything EXCEPT the time. Taking only the
		// first field would throw the date part away, so "Sun *-*-01..07
		// 02:00:00" would be answered with "try Sun" -- recommending a weekly
		// schedule to somebody who wrote a monthly one, in the voice of a
		// helpful correction.
		return Days{}, fmt.Errorf("%q contains a time; this field takes only the DAY part of a systemd calendar expression, and the time of day belongs in the window (for example days %q with window \"02:00-12:00\")",
			text, dropTimeFields(text))
	}
	// Before the "/" rule below, so "Mon Europe/Paris" is named as the
	// timezone it is rather than as repetition syntax it is not.
	if tz := timezoneSuffix(text); tz != "" {
		return Days{}, fmt.Errorf("%q: the timezone suffix %q is not supported: the window is evaluated in the agent host's own local time",
			text, tz)
	}
	for _, bad := range []struct{ tok, why string }{
		{"~", "systemd's ~ (counting back from the end of the month) is not supported"},
		{"/", "systemd's / repetition is not supported"},
	} {
		if strings.Contains(text, bad.tok) {
			return Days{}, fmt.Errorf("%q: %s", text, bad.why)
		}
	}
	switch strings.ToLower(text) {
	case "daily", "weekly", "monthly", "yearly", "annually", "hourly", "minutely", "quarterly", "semiannually":
		return Days{}, fmt.Errorf("%q: systemd's shorthand keywords are not supported here; write the days out (for example %q for the first Sunday of the month), or leave this empty and use the plain interval instead", text, "Sun *-*-01..07")
	}

	fields := strings.Fields(text)
	if len(fields) > 2 {
		return Days{}, fmt.Errorf("%q has %d parts; expected at most two: an optional weekday and an optional YEAR-MONTH-DAY", text, len(fields))
	}

	datePart := ""
	for i, f := range fields {
		if strings.Contains(f, "-") && !isWeekdayList(f) {
			if datePart != "" {
				return Days{}, fmt.Errorf("%q has two date parts", text)
			}
			datePart = f
			continue
		}
		if i > 0 && datePart == "" {
			return Days{}, fmt.Errorf("%q: expected a YEAR-MONTH-DAY after the weekday", text)
		}
		set, err := parseWeekdays(f)
		if err != nil {
			return Days{}, fmt.Errorf("%q: %w", text, err)
		}
		d.weekdays = set
	}

	if datePart != "" {
		parts := strings.Split(datePart, "-")
		if len(parts) != 3 {
			return Days{}, fmt.Errorf("%q: the date part must be YEAR-MONTH-DAY, three fields separated by \"-\" (use * for any), got %q", text, datePart)
		}
		var err error
		if d.years, err = parseNumSet(parts[0], 1970, 2999, "year"); err != nil {
			return Days{}, fmt.Errorf("%q: %w", text, err)
		}
		var months map[int]bool
		if months, err = parseNumSet(parts[1], 1, 12, "month"); err != nil {
			return Days{}, fmt.Errorf("%q: %w", text, err)
		}
		if months != nil {
			d.months = map[time.Month]bool{}
			for m := range months {
				d.months[time.Month(m)] = true
			}
		}
		if d.mdays, err = parseNumSet(parts[2], 1, 31, "day"); err != nil {
			return Days{}, fmt.Errorf("%q: %w", text, err)
		}
		if !d.feasible() {
			return Days{}, fmt.Errorf("%q matches no date that exists: check the month and day together (for example there is no 30 February)", text)
		}
	}
	return d, nil
}

// isWeekdayList reports whether a field containing "-" is really a weekday
// range written with a hyphen rather than a date. Guards "Mon-Fri", which
// people write from cron habit.
func isWeekdayList(f string) bool {
	for _, part := range strings.FieldsFunc(f, func(r rune) bool { return r == ',' || r == '-' }) {
		if _, ok := weekdayNames[strings.ToLower(strings.TrimSpace(part))]; !ok {
			return false
		}
	}
	return f != ""
}

func parseWeekdays(f string) (map[time.Weekday]bool, error) {
	out := map[time.Weekday]bool{}
	// "Mon-Fri" is cron's spelling; systemd wants "Mon..Fri". Accepted
	// because the intent is unmistakable and refusing it teaches nothing.
	f = strings.ReplaceAll(f, "-", "..")
	for _, item := range strings.Split(f, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		lo, hi, isRange := strings.Cut(item, "..")
		from, ok := weekdayNames[strings.ToLower(strings.TrimSpace(lo))]
		if !ok {
			return nil, fmt.Errorf("%q is not a weekday (use Mon, Tue, Wed, Thu, Fri, Sat or Sun)", lo)
		}
		if !isRange {
			out[from] = true
			continue
		}
		to, ok := weekdayNames[strings.ToLower(strings.TrimSpace(hi))]
		if !ok {
			return nil, fmt.Errorf("%q is not a weekday (use Mon, Tue, Wed, Thu, Fri, Sat or Sun)", hi)
		}
		// Walks systemd's Mon-first cycle and wraps, so "Fri..Mon" is the
		// weekend plus its edges rather than an error.
		start := indexOfWeekday(from)
		for i := 0; i < len(weekdayOrder); i++ {
			day := weekdayOrder[(start+i)%len(weekdayOrder)]
			out[day] = true
			if day == to {
				break
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no weekdays given")
	}
	return out, nil
}

func indexOfWeekday(w time.Weekday) int {
	for i, d := range weekdayOrder {
		if d == w {
			return i
		}
	}
	return 0
}

// dropTimeFields removes the time-of-day fields from an expression, keeping
// everything else, so the "write it like this instead" suggestion preserves
// the schedule the operator actually wrote.
func dropTimeFields(text string) string {
	var keep []string
	for _, f := range strings.Fields(text) {
		if strings.Contains(f, ":") {
			continue
		}
		keep = append(keep, f)
	}
	return strings.Join(keep, " ")
}

// timezoneSuffix returns the trailing timezone in an expression, or "".
//
// Real systemd accepts one -- "Sun *-*-01 UTC", "Mon Europe/Paris",
// "Sat +0200" -- and this package deliberately does not, because the window
// is evaluated in the agent host's own local time. Matching only the literal
// "UTC", which this did, meant every other spelling fell through to an
// unrelated error: the by-name refusal the surrounding code promises worked
// for exactly one timezone out of several hundred.
func timezoneSuffix(text string) string {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return ""
	}
	last := fields[len(fields)-1]

	// A region-qualified zone name: Europe/Paris, America/New_York.
	if strings.Contains(last, "/") && !strings.ContainsAny(last, "0123456789*") {
		return last
	}
	// A numeric UTC offset: +0200, -0500.
	if len(last) == 5 && (last[0] == '+' || last[0] == '-') && isAllDigits(last[1:]) {
		return last
	}
	// An abbreviation: UTC, GMT, CET, CEST. Only considered when another
	// field already carries the day spec, so a single mistyped weekday is
	// still reported as a mistyped weekday rather than as a timezone.
	if len(fields) >= 2 && len(last) >= 2 && len(last) <= 5 && isAllLetters(last) {
		if _, err := parseWeekdays(last); err != nil {
			return last
		}
	}
	return ""
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(s) > 0
}

func isAllLetters(s string) bool {
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return false
		}
	}
	return len(s) > 0
}

// feasible reports whether any calendar date can satisfy the date part.
//
// "*-02-30" parses cleanly -- 2 is a valid month and 30 a valid day -- and
// then matches nothing, ever. A verify schedule that silently never fires is
// indistinguishable from a working one until somebody asks why a replica has
// not been checked in a year, which is the exact failure this package exists
// to prevent.
//
// Only the month/day pair is judged. A weekday is not: over unbounded years
// every weekday eventually coincides with any date that exists at all, and
// checking the pinned-year case exactly would mean walking every day of every
// listed year on a path that runs per sync launch.
func (d Days) feasible() bool {
	for m := time.January; m <= time.December; m++ {
		if d.months != nil && !d.months[m] {
			continue
		}
		for day := 1; day <= 31; day++ {
			if d.mdays != nil && !d.mdays[day] {
				continue
			}
			if dayExistsIn(m, day, d.years) {
				return true
			}
		}
	}
	return false
}

func dayExistsIn(m time.Month, day int, years map[int]bool) bool {
	switch m {
	case time.February:
		if day <= 28 {
			return true
		}
		if day > 29 {
			return false
		}
		// The 29th needs a leap year. With no year pinned, one always comes.
		if years == nil {
			return true
		}
		for y := range years {
			if y%4 == 0 && (y%100 != 0 || y%400 == 0) {
				return true
			}
		}
		return false
	case time.April, time.June, time.September, time.November:
		return day <= 30
	default:
		return day <= 31
	}
}

// parseNumSet parses "*", "5", "1,3", "01..07" or a mix, returning nil for
// "*" so a caller can tell "any" from "an explicit set that happens to be
// everything".
func parseNumSet(f string, min, max int, what string) (map[int]bool, error) {
	f = strings.TrimSpace(f)
	if f == "*" {
		return nil, nil
	}
	// An EMPTY component is a typo, never an intention. It is only reachable
	// from a malformed date part -- a leading, trailing or doubled "-", which
	// still splits into three fields and so slips past the length guard.
	//
	// Treating it as "any" (which this did) is the worst available reading:
	// "*-*-" is a trailing-hyphen slip for "*-*-01" and would silently match
	// 365 days a year instead of 12, turning a monthly full-image verify into
	// a daily one with nothing logged. Widening a schedule by 30x must not be
	// something a stray keystroke can do quietly.
	if f == "" {
		return nil, fmt.Errorf("the %s field is empty; write a number or * for any", what)
	}
	out := map[int]bool{}
	for _, item := range strings.Split(f, ",") {
		item = strings.TrimSpace(item)
		lo, hi, isRange := strings.Cut(item, "..")
		from, err := strconv.Atoi(strings.TrimSpace(lo))
		if err != nil {
			return nil, fmt.Errorf("%q is not a %s number", lo, what)
		}
		to := from
		if isRange {
			if to, err = strconv.Atoi(strings.TrimSpace(hi)); err != nil {
				return nil, fmt.Errorf("%q is not a %s number", hi, what)
			}
		}
		if from > to {
			return nil, fmt.Errorf("%s range %q runs backwards", what, item)
		}
		if from < min || to > max {
			return nil, fmt.Errorf("%s %q is outside %d-%d", what, item, min, max)
		}
		for n := from; n <= to; n++ {
			out[n] = true
		}
	}
	return out, nil
}

// Window is a wall-clock time of day range, closed at the start and open at
// the end: 02:00-12:00 includes 02:00 and excludes 12:00.
//
// Wall clock, and never a duration. That distinction is the whole DST story:
// in Europe the transition IS the last Sunday of March and October, so any
// Sunday rule lands on one twice a year -- and on those days 02:00-12:00 is
// nine or eleven hours long, while in the spring-forward gap 02:00 does not
// exist at all. Asking "is the local clock between 02:00 and 12:00" is
// correct through all of that. Asking "is now within ten hours of the start"
// is not.
type Window struct {
	startMin int
	endMin   int
	text     string
}

// String returns the window as written.
func (w Window) String() string {
	if w.text == "" {
		return "all day"
	}
	return w.text
}

// IsZero reports whether no window was given, which means "all day".
func (w Window) IsZero() bool { return w.text == "" }

// crossesMidnight reports whether this window runs into the next day.
//
// 22:00-04:00 is an ordinary maintenance window, so it is supported rather
// than refused -- and the rule that makes it unambiguous is that a window
// OPENS on a matching day. "Sun 22:00-04:00" is Sunday night into Monday
// morning, not Sunday's small hours.
func (w Window) crossesMidnight() bool { return w.endMin <= w.startMin }

// ParseWindow parses "HH:MM-HH:MM". An empty string is the zero Window,
// meaning the whole of a matching day.
func ParseWindow(s string) (Window, error) {
	text := strings.TrimSpace(s)
	if text == "" {
		return Window{}, nil
	}
	from, to, ok := strings.Cut(text, "-")
	if !ok {
		return Window{}, fmt.Errorf("window %q must be written START-END, for example \"02:00-12:00\"", text)
	}
	start, err := parseHHMM(from)
	if err != nil {
		return Window{}, fmt.Errorf("window %q: start: %w", text, err)
	}
	end, err := parseHHMM(to)
	if err != nil {
		return Window{}, fmt.Errorf("window %q: end: %w", text, err)
	}
	if start == end {
		return Window{}, fmt.Errorf("window %q starts and ends at the same time, so it never opens; leave it empty to mean the whole day", text)
	}
	return Window{startMin: start, endMin: end, text: text}, nil
}

func parseHHMM(s string) (int, error) {
	s = strings.TrimSpace(s)
	hh, mm, ok := strings.Cut(s, ":")
	if !ok {
		return 0, fmt.Errorf("%q is not HH:MM", s)
	}
	h, err := strconv.Atoi(strings.TrimSpace(hh))
	if err != nil || h < 0 || h > 23 {
		return 0, fmt.Errorf("%q has no valid hour (00-23)", s)
	}
	m, err := strconv.Atoi(strings.TrimSpace(mm))
	if err != nil || m < 0 || m > 59 {
		return 0, fmt.Errorf("%q has no valid minute (00-59)", s)
	}
	return h*60 + m, nil
}

// Schedule is a day expression and a window together: the recurring
// occurrences a caller acts on at most once each.
type Schedule struct {
	Days   Days
	Window Window
}

// Parse builds a Schedule from the two fields as written.
func Parse(days, window string) (Schedule, error) {
	d, err := ParseDays(days)
	if err != nil {
		return Schedule{}, err
	}
	w, err := ParseWindow(window)
	if err != nil {
		return Schedule{}, err
	}
	return Schedule{Days: d, Window: w}, nil
}

// IsZero reports whether nothing was configured, in which case a caller
// should fall back to a plain interval.
func (s Schedule) IsZero() bool { return s.Days.text == "" && s.Window.IsZero() }

// OccurrenceStart returns when the occurrence containing t opened, and
// whether t is inside one at all.
//
// This is the whole interface a caller needs. Comparing the returned time
// against "when did I last act" answers both questions at once: am I in a
// window, and have I already done this one? A ten-hour window and a
// ten-second tick would otherwise fire dozens of times.
//
// The returned time is in t's own location, and is built with time.Date
// rather than by subtracting a duration -- so on a spring-forward day a start
// time that does not exist normalises forward to the next real instant
// instead of landing an hour out.
func (s Schedule) OccurrenceStart(t time.Time) (time.Time, bool) {
	if s.Window.IsZero() {
		if !s.Days.Matches(t) {
			return time.Time{}, false
		}
		return startOfDay(t), true
	}

	mins := t.Hour()*60 + t.Minute()
	if !s.Window.crossesMidnight() {
		if !s.Days.Matches(t) || mins < s.Window.startMin || mins >= s.Window.endMin {
			return time.Time{}, false
		}
		return atMinute(t, s.Window.startMin), true
	}

	// Crossing midnight: either the tail of today's opening, or the tail of
	// yesterday's. Checked in that order because a window may legitimately be
	// open on two consecutive days.
	if mins >= s.Window.startMin && s.Days.Matches(t) {
		return atMinute(t, s.Window.startMin), true
	}
	if mins < s.Window.endMin {
		// AddDate, not a 24h subtraction: on a DST boundary the previous
		// calendar day is not 24 hours ago.
		y := t.AddDate(0, 0, -1)
		if s.Days.Matches(y) {
			return atMinute(y, s.Window.startMin), true
		}
	}
	return time.Time{}, false
}

// Due reports whether t is inside an occurrence that has not been acted on,
// given when the caller last acted. A zero lastActed means never.
//
// The caller stores the returned start and passes it back next time. Nothing
// here is stateful, so a restart loses at most the memory of the current
// occurrence -- and the caller's own persistence decides whether that matters.
func (s Schedule) Due(t time.Time, lastActed time.Time) (start time.Time, due bool) {
	start, in := s.OccurrenceStart(t)
	if !in {
		return time.Time{}, false
	}
	if !lastActed.Before(start) {
		return start, false
	}
	return start, true
}

func startOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

func atMinute(t time.Time, min int) time.Time {
	// Deliberately unconditional, ambiguity included.
	//
	// On an autumn-back day the start hour happens TWICE, and time.Date
	// resolves that to one of the pair. Whichever it picks, it picks the SAME
	// one on both passes -- and that stability is the whole requirement,
	// because the returned value is the dedupe key: Due compares it against
	// lastVerified to decide whether this occurrence has already been acted
	// on. One key, one firing.
	//
	// The visible cost is that during the first pass through the repeated hour
	// the reported start can read as up to one DST shift in the future, which
	// looks odd in the log line naming it. Correcting that was tried and is a
	// trap: shifting only when the start looks future makes the two passes
	// resolve differently, which turns one occurrence into two and verifies
	// twice that night. A cosmetic timestamp is worth less than firing once,
	// so this stays as it is -- see TestDueFiresOncePerOccurrenceInEveryZone,
	// which fails at 53 firings if anyone tries the same correction again.
	return time.Date(t.Year(), t.Month(), t.Day(), min/60, min%60, 0, 0, t.Location())
}
