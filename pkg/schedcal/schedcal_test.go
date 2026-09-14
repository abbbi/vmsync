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

package schedcal

import (
	"strings"
	"testing"
	"time"

	// Embeds the IANA timezone database in the test binary so the DST cases
	// below cannot silently skip on a host without tzdata. See zone().
	_ "time/tzdata"
)

// paris is the zone the DST cases use, chosen because Europe's transition IS
// the last Sunday of March and October -- so any "Sunday" rule lands on one
// twice a year, by construction rather than by bad luck.
func paris(t *testing.T) *time.Location { return zone(t, "Europe/Paris") }

// zone is a Fatal, never a Skip.
//
// It used to skip when the host had no tzdata, which turned the entire DST
// suite into something that could vanish into a green build -- the exact
// failure this package exists to prevent, applied to its own tests. A slim CI
// container is enough to trigger it, and nothing would have said so.
//
// Importing time/tzdata above embeds the IANA database in the test binary, so
// a load failure is now a real bug rather than a property of the machine, and
// the DST coverage is the same wherever it runs.
func zone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("loading %s failed even with tzdata embedded: %v", name, err)
	}
	return loc
}

func at(t *testing.T, loc *time.Location, s string) time.Time {
	t.Helper()
	ts, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
	if err != nil {
		t.Fatalf("bad test time %q: %v", s, err)
	}
	return ts
}

// The expression this whole package exists for.
func TestFirstSundayOfTheMonth(t *testing.T) {
	d, err := ParseDays("Sun *-*-01..07")
	if err != nil {
		t.Fatalf("ParseDays: %v", err)
	}
	utc := time.UTC

	for _, tc := range []struct {
		date string
		want bool
		why  string
	}{
		{"2026-03-01 09:00", true, "1 March 2026 is a Sunday and in 1..7"},
		{"2026-04-05 09:00", true, "5 April 2026 is the first Sunday"},
		{"2026-11-01 09:00", true, "1 November 2026 is a Sunday"},
		{"2026-03-08 09:00", false, "the SECOND Sunday: day 8 is outside 1..7"},
		{"2026-03-02 09:00", false, "a Monday inside 1..7"},
		{"2026-03-29 09:00", false, "the last Sunday of the month"},
	} {
		t.Run(tc.date, func(t *testing.T) {
			if got := d.Matches(at(t, utc, tc.date)); got != tc.want {
				t.Errorf("Matches = %v, want %v -- %s", got, tc.want, tc.why)
			}
		})
	}

	// Exactly one match a month is the property that makes the AND of
	// weekday and day-of-month mean "the first Sunday" rather than
	// approximately that.
	t.Run("matches exactly once in every month of a year", func(t *testing.T) {
		for m := time.January; m <= time.December; m++ {
			hits := 0
			for day := 1; day <= 31; day++ {
				ts := time.Date(2026, m, day, 12, 0, 0, 0, utc)
				if ts.Month() != m {
					break // rolled into the next month
				}
				if d.Matches(ts) {
					hits++
				}
			}
			if hits != 1 {
				t.Errorf("%s 2026 matched %d days, want exactly 1", m, hits)
			}
		}
	})
}

func TestParseDays(t *testing.T) {
	utc := time.UTC
	t.Run("accepted forms", func(t *testing.T) {
		for _, tc := range []struct {
			expr, when string
			want       bool
		}{
			{"", "2026-03-04 09:00", true},
			{"Sun", "2026-03-01 09:00", true},
			{"Sun", "2026-03-02 09:00", false},
			{"Sat,Sun", "2026-03-07 09:00", true},
			{"Mon..Fri", "2026-03-04 09:00", true},
			{"Mon..Fri", "2026-03-07 09:00", false},
			// cron's hyphen, accepted because the intent is unmistakable.
			{"Mon-Fri", "2026-03-04 09:00", true},
			// Wrapping range: Friday through Monday.
			{"Fri..Mon", "2026-03-07 09:00", true},
			{"Fri..Mon", "2026-03-04 09:00", false},
			{"*-*-01", "2026-03-01 09:00", true},
			{"*-*-01", "2026-03-02 09:00", false},
			{"*-01,07-*", "2026-07-15 09:00", true},
			{"*-01,07-*", "2026-06-15 09:00", false},
			{"*-*-01..07", "2026-03-05 09:00", true},
			{"*-*-01..07", "2026-03-08 09:00", false},
		} {
			t.Run(tc.expr+" @ "+tc.when, func(t *testing.T) {
				d, err := ParseDays(tc.expr)
				if err != nil {
					t.Fatalf("ParseDays(%q): %v", tc.expr, err)
				}
				if got := d.Matches(at(t, utc, tc.when)); got != tc.want {
					t.Errorf("Matches = %v, want %v", got, tc.want)
				}
			})
		}
	})

	// Each of these is REAL systemd. Silently half-understanding one is the
	// failure this package is most exposed to, so each is refused by name.
	t.Run("real systemd this does not support is refused by name", func(t *testing.T) {
		for _, tc := range []struct{ expr, mentions string }{
			{"Sun *-*-01..07 02:00:00", "window"},
			{"*-*~01", "~"},
			{"Mon *-*-* 00/2:00", "/"},
			{"daily", "shorthand"},
			{"weekly", "shorthand"},
			{"Sun *-*-01 UTC", "timezone"},
		} {
			t.Run(tc.expr, func(t *testing.T) {
				_, err := ParseDays(tc.expr)
				if err == nil {
					t.Fatalf("ParseDays(%q) was accepted; a half-understood expression fires on the wrong days", tc.expr)
				}
				if !strings.Contains(strings.ToLower(err.Error()), tc.mentions) {
					t.Errorf("error %q does not mention %q, so it does not say what to do instead", err, tc.mentions)
				}
			})
		}
	})

	t.Run("malformed input is refused", func(t *testing.T) {
		for _, expr := range []string{
			"Frunday", "*-*", "*-13-*", "*-*-32", "*-*-07..01", "Sun Mon *-*-01", "*-*-0",
		} {
			if _, err := ParseDays(expr); err == nil {
				t.Errorf("ParseDays(%q) was accepted", expr)
			}
		}
	})
}

func TestParseWindow(t *testing.T) {
	for _, tc := range []struct {
		in string
		ok bool
	}{
		{"", true},
		{"02:00-12:00", true},
		{"22:00-04:00", true},
		{"00:00-23:59", true},
		{"2:00-12:00", true},
		{"02:00", false},
		{"02:00-", false},
		{"25:00-12:00", false},
		{"02:60-12:00", false},
		{"ab:cd-12:00", false},
		{"02:00-02:00", false}, // never opens
	} {
		t.Run(tc.in, func(t *testing.T) {
			_, err := ParseWindow(tc.in)
			if tc.ok && err != nil {
				t.Errorf("ParseWindow(%q) = %v, want nil", tc.in, err)
			}
			if !tc.ok && err == nil {
				t.Errorf("ParseWindow(%q) was accepted", tc.in)
			}
		})
	}
}

func TestOccurrenceStartAndDue(t *testing.T) {
	utc := time.UTC
	s, err := Parse("Sun *-*-01..07", "02:00-12:00")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	t.Run("inside the window on a matching day", func(t *testing.T) {
		start, in := s.OccurrenceStart(at(t, utc, "2026-03-01 03:30"))
		if !in {
			t.Fatal("not in an occurrence at 03:30 on the first Sunday")
		}
		if want := at(t, utc, "2026-03-01 02:00"); !start.Equal(want) {
			t.Errorf("start = %v, want %v", start, want)
		}
	})

	t.Run("the boundaries are closed at the start and open at the end", func(t *testing.T) {
		if _, in := s.OccurrenceStart(at(t, utc, "2026-03-01 02:00")); !in {
			t.Error("02:00 is outside; the start must be included")
		}
		if _, in := s.OccurrenceStart(at(t, utc, "2026-03-01 12:00")); in {
			t.Error("12:00 is inside; the end must be excluded, or two windows could abut and overlap")
		}
		if _, in := s.OccurrenceStart(at(t, utc, "2026-03-01 11:59")); !in {
			t.Error("11:59 is outside")
		}
	})

	t.Run("outside the window, and on the wrong day", func(t *testing.T) {
		if _, in := s.OccurrenceStart(at(t, utc, "2026-03-01 01:00")); in {
			t.Error("01:00 is inside the 02:00 window")
		}
		if _, in := s.OccurrenceStart(at(t, utc, "2026-03-08 03:30")); in {
			t.Error("the second Sunday is a matching day")
		}
	})

	// The reason OccurrenceStart returns a time at all: a ten-hour window and
	// a ten-second tick would otherwise fire thousands of times.
	t.Run("Due fires once per occurrence, not once per tick", func(t *testing.T) {
		var lastActed time.Time
		fires := 0
		// Every 10 minutes across the whole window.
		for m := 0; m < 12*60; m += 10 {
			now := at(t, utc, "2026-03-01 00:00").Add(time.Duration(m) * time.Minute)
			if start, due := s.Due(now, lastActed); due {
				fires++
				lastActed = start
			}
		}
		if fires != 1 {
			t.Errorf("fired %d times across one window, want exactly 1", fires)
		}
	})

	t.Run("the next month is a new occurrence", func(t *testing.T) {
		lastActed := at(t, utc, "2026-03-01 02:00")
		if _, due := s.Due(at(t, utc, "2026-04-05 03:00"), lastActed); !due {
			t.Error("April's first Sunday did not fire after March's was acted on")
		}
	})

	t.Run("a missed window is simply missed", func(t *testing.T) {
		// No catch-up, by decision: the operator asked for that window, and a
		// verification fired outside it is the thing they were avoiding.
		lastActed := at(t, utc, "2026-03-01 02:00")
		if _, due := s.Due(at(t, utc, "2026-04-06 03:00"), lastActed); due {
			t.Error("fired on the Monday after a missed Sunday window")
		}
	})

	t.Run("no window means the whole matching day", func(t *testing.T) {
		s, err := Parse("Sun", "")
		if err != nil {
			t.Fatal(err)
		}
		start, in := s.OccurrenceStart(at(t, utc, "2026-03-01 23:30"))
		if !in {
			t.Fatal("23:30 on a Sunday is outside an all-day occurrence")
		}
		if want := at(t, utc, "2026-03-01 00:00"); !start.Equal(want) {
			t.Errorf("start = %v, want midnight %v", start, want)
		}
	})
}

// A window running into the next day is an ordinary maintenance shape, and
// the rule that makes it unambiguous is that a window OPENS on a matching
// day: "Sun 22:00-04:00" is Sunday night into Monday morning.
func TestWindowCrossingMidnight(t *testing.T) {
	utc := time.UTC
	s, err := Parse("Sun", "22:00-04:00")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	sundayOpen := at(t, utc, "2026-03-01 22:00")

	t.Run("opens on the matching day", func(t *testing.T) {
		start, in := s.OccurrenceStart(at(t, utc, "2026-03-01 23:00"))
		if !in || !start.Equal(sundayOpen) {
			t.Errorf("start = %v, in = %v; want %v, true", start, in, sundayOpen)
		}
	})

	t.Run("continues into the following morning as the SAME occurrence", func(t *testing.T) {
		start, in := s.OccurrenceStart(at(t, utc, "2026-03-02 03:00"))
		if !in {
			t.Fatal("Monday 03:00 is outside a Sunday 22:00-04:00 window")
		}
		if !start.Equal(sundayOpen) {
			t.Errorf("start = %v, want Sunday's %v -- a crossing window must not count as two occurrences", start, sundayOpen)
		}
	})

	t.Run("does not open on Sunday's own small hours", func(t *testing.T) {
		// That would be Saturday's window, and Saturday does not match.
		if _, in := s.OccurrenceStart(at(t, utc, "2026-03-01 03:00")); in {
			t.Error("Sunday 03:00 is inside; that belongs to a Saturday window, which does not match")
		}
	})

	t.Run("closes at the end", func(t *testing.T) {
		if _, in := s.OccurrenceStart(at(t, utc, "2026-03-02 04:00")); in {
			t.Error("Monday 04:00 is inside; the end must be excluded")
		}
	})

	t.Run("fires once across the whole crossing window", func(t *testing.T) {
		var lastActed time.Time
		fires := 0
		for m := 0; m < 8*60; m += 10 {
			now := at(t, utc, "2026-03-01 21:00").Add(time.Duration(m) * time.Minute)
			if start, due := s.Due(now, lastActed); due {
				fires++
				lastActed = start
			}
		}
		if fires != 1 {
			t.Errorf("fired %d times across one crossing window, want 1", fires)
		}
	})
}

// DST is not an edge case here, it is a certainty: Europe's transitions ARE
// the last Sunday of March and October, so a Sunday rule meets one twice a
// year. The window is compared as WALL CLOCK for exactly this reason -- any
// arithmetic on its length would be wrong on both days.
func TestDSTTransitionDays(t *testing.T) {
	loc := paris(t)

	t.Run("spring forward: the window opens even though 02:00 does not exist", func(t *testing.T) {
		// 29 March 2026, Europe/Paris: 02:00 jumps to 03:00. A window
		// starting at 02:00 has no 02:00 to start at, and must still open.
		s, err := Parse("Sun", "02:00-12:00")
		if err != nil {
			t.Fatal(err)
		}
		// 03:00 local is the first instant after the gap.
		if _, in := s.OccurrenceStart(at(t, loc, "2026-03-29 03:00")); !in {
			t.Error("the window never opened on a spring-forward day; a monthly verification would silently skip")
		}
		if _, in := s.OccurrenceStart(at(t, loc, "2026-03-29 11:30")); !in {
			t.Error("the window closed early on a spring-forward day")
		}
		if _, in := s.OccurrenceStart(at(t, loc, "2026-03-29 12:30")); in {
			t.Error("the window ran past its wall-clock end on a spring-forward day")
		}
	})

	t.Run("autumn back: the repeated hour stays one occurrence", func(t *testing.T) {
		// 25 October 2026, Europe/Paris: 03:00 falls back to 02:00, so 02:30
		// happens twice. Both instants are inside the same window and must
		// not fire twice.
		s, err := Parse("Sun", "02:00-12:00")
		if err != nil {
			t.Fatal(err)
		}
		var lastActed time.Time
		fires := 0
		// Walk real instants across the fold, an hour either side.
		begin := time.Date(2026, time.October, 25, 0, 0, 0, 0, loc)
		for d := time.Duration(0); d < 13*time.Hour; d += 10 * time.Minute {
			if start, due := s.Due(begin.Add(d), lastActed); due {
				fires++
				lastActed = start
			}
		}
		if fires != 1 {
			t.Errorf("fired %d times across an autumn-back day, want 1 -- the repeated hour was counted as a second occurrence", fires)
		}
	})

	t.Run("a crossing window survives a transition night", func(t *testing.T) {
		s, err := Parse("Sat", "22:00-06:00")
		if err != nil {
			t.Fatal(err)
		}
		// Saturday 24 October into the long Sunday.
		var lastActed time.Time
		fires := 0
		begin := time.Date(2026, time.October, 24, 21, 0, 0, 0, loc)
		for d := time.Duration(0); d < 10*time.Hour; d += 10 * time.Minute {
			if start, due := s.Due(begin.Add(d), lastActed); due {
				fires++
				lastActed = start
			}
		}
		if fires != 1 {
			t.Errorf("fired %d times, want 1", fires)
		}
	})
}

func TestScheduleIsZero(t *testing.T) {
	// The fallback signal: nothing configured means the caller should use its
	// plain interval instead.
	s, err := Parse("", "")
	if err != nil {
		t.Fatal(err)
	}
	if !s.IsZero() {
		t.Error("an empty schedule is not reported as zero, so the interval fallback would never be used")
	}
	if s2, _ := Parse("Sun", ""); s2.IsZero() {
		t.Error("a day expression alone is reported as zero")
	}
	if s3, _ := Parse("", "02:00-12:00"); s3.IsZero() {
		t.Error("a window alone is reported as zero")
	}
}

// The one case where "yesterday" is genuinely ambiguous, and the reason
// OccurrenceStart uses AddDate rather than subtracting 24 hours.
//
// A spring-forward Sunday is 23 real hours long. So from Monday 00:30,
// stepping back 24 HOURS lands on SATURDAY 23:30 -- a different weekday --
// while stepping back one calendar DAY lands on Sunday, which is what the
// expression means. With a Sunday-night window, the duration version reports
// the window closed on precisely the night it should be open, once a year,
// and the missed occurrence is never made up.
func TestCrossingWindowUsesCalendarDaysNotDurations(t *testing.T) {
	loc := paris(t)
	s, err := Parse("Sun", "22:00-04:00")
	if err != nil {
		t.Fatal(err)
	}

	// 29 March 2026 is Europe/Paris's spring-forward Sunday.
	now := at(t, loc, "2026-03-30 00:30")
	start, in := s.OccurrenceStart(now)
	if !in {
		t.Fatal("the Sunday-night window is closed at Monday 00:30 after a 23-hour Sunday; " +
			"stepping back 24 hours lands on Saturday and loses the occurrence")
	}
	if want := at(t, loc, "2026-03-29 22:00"); !start.Equal(want) {
		t.Errorf("start = %v, want Sunday's %v", start, want)
	}

	// And the mirror: a 25-hour autumn Sunday must not make Monday's small
	// hours belong to two different days at once.
	now = at(t, loc, "2026-10-26 00:30")
	start, in = s.OccurrenceStart(now)
	if !in {
		t.Fatal("the Sunday-night window is closed at Monday 00:30 after a 25-hour Sunday")
	}
	if want := at(t, loc, "2026-10-25 22:00"); !start.Equal(want) {
		t.Errorf("start = %v, want Sunday's %v", start, want)
	}
}

// Four ways a malformed expression used to be accepted and silently mean
// something the operator did not write. Each is a schedule that looks
// configured and is not, which is the failure mode this package exists to
// prevent -- so each is a refusal, not a warning.
func TestSilentMisreadingsAreRefused(t *testing.T) {
	t.Run("an empty date component is not \"any\"", func(t *testing.T) {
		// "*-*-" is a trailing-hyphen slip for "*-*-01". It still splits into
		// three fields, so it passed the length guard, and the empty day
		// widened to "any": 365 matching days a year instead of 12. A stray
		// keystroke must not turn a monthly full-image verify into a daily one.
		for _, expr := range []string{"*-*-", "*--*", "-*-*", "*-01-"} {
			t.Run(expr, func(t *testing.T) {
				if _, err := ParseDays(expr); err == nil {
					d, _ := ParseDays(expr)
					t.Errorf("ParseDays(%q) was accepted and matches %d days in 2026",
						expr, countMatches(d, 2026))
				}
			})
		}
	})

	t.Run("a date that cannot exist is refused, not silently never fired", func(t *testing.T) {
		// Parses cleanly -- 2 is a valid month, 30 a valid day -- and then
		// matches nothing forever. Indistinguishable from a working schedule
		// until somebody asks why a replica has not been verified in a year.
		for _, expr := range []string{"*-02-30", "*-02-31", "*-04-31", "*-06-31", "*-11-31"} {
			if _, err := ParseDays(expr); err == nil {
				t.Errorf("ParseDays(%q) was accepted; it can never match", expr)
			}
		}
		// Still reachable, so still accepted.
		for _, expr := range []string{"*-02-29", "*-01-31", "*-*-31", "*-02-28"} {
			if _, err := ParseDays(expr); err != nil {
				t.Errorf("ParseDays(%q) was refused: %v", expr, err)
			}
		}
		// 29 February needs a leap year, so a pinned non-leap year makes it
		// impossible and a pinned leap year does not.
		if _, err := ParseDays("2026-02-29"); err == nil {
			t.Error("2026-02-29 was accepted; 2026 is not a leap year")
		}
		if _, err := ParseDays("2028-02-29"); err != nil {
			t.Errorf("2028-02-29 was refused: %v; 2028 is a leap year", err)
		}
	})

	t.Run("the time-component suggestion keeps the schedule that was written", func(t *testing.T) {
		// The message says "write it like this instead". Taking only the first
		// field threw the date part away, so a monthly expression was answered
		// with a weekly one -- a wrong correction, delivered in the voice of a
		// right one.
		_, err := ParseDays("Sun *-*-01..07 02:00:00")
		if err == nil {
			t.Fatal("a time component was accepted")
		}
		if !strings.Contains(err.Error(), "Sun *-*-01..07") {
			t.Errorf("error %q drops the date part, so it suggests a more frequent schedule than was written", err)
		}
	})

	t.Run("every timezone suffix is refused, not just UTC", func(t *testing.T) {
		// Matching the literal "UTC" covered one timezone out of several
		// hundred; the rest fell through to an unrelated error.
		for _, expr := range []string{
			"Sun *-*-01 UTC", "Sun *-*-01 GMT", "Sun *-*-01 CET", "Sun *-*-01 CEST",
			"Mon Europe/Paris", "Sat +0200", "Sat -0500",
		} {
			t.Run(expr, func(t *testing.T) {
				err := parseDaysErr(t, expr)
				if err == nil {
					t.Fatalf("ParseDays(%q) was accepted", expr)
				}
				if !strings.Contains(strings.ToLower(err.Error()), "timezone") {
					t.Errorf("error %q does not say it is a timezone, so it does not tell the operator what to remove", err)
				}
			})
		}
		// And a plain expression is not mistaken for one.
		for _, expr := range []string{"Sun", "Mon..Fri", "Sat,Sun", "Sun *-*-01..07", "*-01,07-*"} {
			if _, err := ParseDays(expr); err != nil {
				t.Errorf("ParseDays(%q) was refused as a timezone: %v", expr, err)
			}
		}
	})
}

func parseDaysErr(t *testing.T, expr string) error {
	t.Helper()
	_, err := ParseDays(expr)
	return err
}

func countMatches(d Days, year int) int {
	n := 0
	for day := time.Date(year, 1, 1, 12, 0, 0, 0, time.UTC); day.Year() == year; day = day.AddDate(0, 0, 1) {
		if d.Matches(day) {
			n++
		}
	}
	return n
}

// oddZones are the real timezones that break whole-hour assumptions. A
// scheduler that only ever meets UTC and Europe/Paris has not met either of
// the first two.
var oddZones = []string{
	"UTC",
	"Europe/Paris",        // 1h DST shift, transition ON a Sunday
	"Australia/Lord_Howe", // 30-MINUTE DST shift
	"Pacific/Chatham",     // :45 base offset, 1h shift
	"America/St_Johns",    // :30 base offset
	"Asia/Kolkata",        // :30 base offset, no DST at all
	"America/Santiago",    // southern-hemisphere transitions
}

// A weekly window must open the same number of times a year whatever the host
// zone is, and must never claim to have started in the future.
//
// The invariants are stated as properties over a whole year rather than as
// fixed dates, because the point is that no zone is special-cased: the agent
// reads the host clock, so every one of these is somebody's production host.
func TestWindowHoldsInEveryZone(t *testing.T) {
	for _, name := range oddZones {
		t.Run(name, func(t *testing.T) {
			loc := zone(t, name)
			s, err := Parse("Sun", "02:00-04:00")
			if err != nil {
				t.Fatal(err)
			}

			starts := map[string]bool{}
			for ts := time.Date(2026, 1, 1, 0, 0, 0, 0, loc); ts.Year() == 2026; ts = ts.Add(5 * time.Minute) {
				start, in := s.OccurrenceStart(ts)
				if !in {
					continue
				}
				// A start may read as slightly FUTURE during a repeated hour
				// on an autumn-back day: the wall clock occurs twice and
				// time.Date resolves to one of the pair, so on the first pass
				// the chosen instant has not happened yet. That is accepted --
				// see atMinute. What is not accepted is a start unrelated to
				// the moment inside it, so the bound is one DST shift.
				if d := start.Sub(ts); d > 2*time.Hour {
					t.Fatalf("at %s the occurrence start is %s, %v in the future -- more than any DST shift",
						ts.Format("2006-01-02 15:04 -0700"),
						start.Format("2006-01-02 15:04 -0700"), d)
				}
				starts[start.Format(time.RFC3339)] = true
			}

			// 2026 has 52 Sundays. Every one must produce exactly one
			// occurrence: a DST day must not drop one or split it in two.
			if len(starts) != 52 {
				t.Errorf("%d distinct occurrences in 2026, want 52 (one per Sunday)", len(starts))
			}
		})
	}
}

// Due must fire exactly once per occurrence in every zone, which is the
// property the whole lastVerified dance exists for. A doubled hour is the case
// most likely to fire twice, and a missing hour the case most likely to fire
// never.
func TestDueFiresOncePerOccurrenceInEveryZone(t *testing.T) {
	for _, name := range oddZones {
		t.Run(name, func(t *testing.T) {
			loc := zone(t, name)
			s, err := Parse("Sun", "02:00-04:00")
			if err != nil {
				t.Fatal(err)
			}
			var lastActed time.Time
			fires := 0
			for ts := time.Date(2026, 1, 1, 0, 0, 0, 0, loc); ts.Year() == 2026; ts = ts.Add(5 * time.Minute) {
				start, due := s.Due(ts, lastActed)
				if due {
					fires++
					lastActed = start
				}
			}
			if fires != 52 {
				t.Errorf("fired %d times in 2026, want 52 -- once per Sunday", fires)
			}
		})
	}
}

// Australia/Lord_Howe shifts by THIRTY minutes, so a fix that assumes DST is
// always an hour is wrong there and nowhere else. Spelled out separately
// because the year-long sweep above would only show it as a count.
func TestLordHoweHalfHourShift(t *testing.T) {
	loc := zone(t, "Australia/Lord_Howe")
	s, err := Parse("Sun", "02:00-04:00")
	if err != nil {
		t.Fatal(err)
	}
	// 4 October 2026: 02:00 springs forward to 02:30.
	for _, hhmm := range []string{"02:30", "02:45", "03:00", "03:59"} {
		ts := at(t, loc, "2026-10-04 "+hhmm)
		start, in := s.OccurrenceStart(ts)
		if !in {
			t.Errorf("%s is outside the window on the spring-forward Sunday", hhmm)
			continue
		}
		if start.After(ts) {
			t.Errorf("at %s the start %s is in the future", hhmm, start)
		}
	}
	// And the autumn-back Sunday, where 01:30-02:00 happens twice.
	ts := at(t, loc, "2026-04-05 02:00")
	if start, in := s.OccurrenceStart(ts); !in || start.After(ts) {
		t.Errorf("autumn-back Sunday: in=%v start=%v now=%v", in, start, ts)
	}
}

// midnightGapZones move their clocks forward at MIDNIGHT, so the previous
// day's small hours are the ones that do not exist.
//
// That is what makes them different from Europe/Paris and friends, whose gap
// starts at 01:00 or later: taking "yesterday at this wall clock" from an
// early-morning instant lands on a time that never happened, and Go resolves
// it BACKWARD across midnight -- two calendar days back, the wrong weekday.
//
// Each date below is the transition Sunday itself.
var midnightGapZones = []struct{ zone, springForward string }{
	{"America/Santiago", "2026-09-06"},
	{"America/Havana", "2026-03-08"},
	{"Atlantic/Azores", "2026-03-29"},
}

// A window that crosses midnight out of one of those Sundays must stay open
// for the whole of Monday's small hours.
//
// This is the case the rest of the suite could not reach. oddZones already
// lists America/Santiago, but only ever drives it with the NON-crossing
// "02:00-04:00" -- and every crossing-window test ran in UTC or Europe/Paris,
// neither of which has a midnight gap. So the branch was exercised and the
// zones were exercised, just never both at once.
func TestCrossingWindowOutOfAMidnightGapDay(t *testing.T) {
	for _, z := range midnightGapZones {
		t.Run(z.zone, func(t *testing.T) {
			loc := zone(t, z.zone)
			sunday := at(t, loc, z.springForward+" 12:00") // midday: always exists
			if sunday.Weekday() != time.Sunday {
				t.Fatalf("%s is a %s, not a Sunday -- the fixture is wrong", z.springForward, sunday.Weekday())
			}
			s, err := Parse("Sun", "23:00-01:00")
			if err != nil {
				t.Fatal(err)
			}

			// The tail of the occurrence, on the Monday. Built with time.Date
			// rather than by parsing a bare date: parsing "<date>" alone asks
			// for midnight, which in these zones does not exist and silently
			// resolves to the previous evening -- the same trap the code had.
			monday := sunday.AddDate(0, 0, 1)
			for i := 0; i < 60; i++ {
				ts := time.Date(monday.Year(), monday.Month(), monday.Day(), 0, i, 0, 0, loc)
				start, in := s.OccurrenceStart(ts)
				if !in {
					t.Fatalf("at %s the Sunday-night window reports CLOSED; taking yesterday's date landed on the wrong weekday, so half the occurrence is silently lost",
						ts.Format("2006-01-02 15:04 -0700"))
				}
				if start.Weekday() != time.Sunday {
					t.Errorf("at %s the occurrence start is %s, a %s -- it must be the Sunday the window opened on",
						ts.Format("15:04"), start.Format("2006-01-02 15:04 -0700"), start.Weekday())
				}
			}

			// And the head of it, on the Sunday, must not have been broken in
			// the process.
			for i := 0; i < 60; i++ {
				ts := time.Date(sunday.Year(), sunday.Month(), sunday.Day(), 23, i, 0, 0, loc)
				if _, in := s.OccurrenceStart(ts); !in {
					t.Fatalf("at %s the window reports closed on the Sunday it opens on", ts.Format("2006-01-02 15:04 -0700"))
				}
			}
		})
	}
}

// The invariant, for a CROSSING window, across every zone. oddZones only ever
// checked it for a non-crossing one.
func TestCrossingWindowFiresOncePerOccurrenceInEveryZone(t *testing.T) {
	zones := append([]string{}, oddZones...)
	for _, z := range midnightGapZones {
		zones = append(zones, z.zone)
	}
	for _, name := range zones {
		t.Run(name, func(t *testing.T) {
			loc := zone(t, name)
			s, err := Parse("Sun", "22:00-04:00")
			if err != nil {
				t.Fatal(err)
			}
			var last time.Time
			fires := 0
			for ts := time.Date(2026, 1, 1, 0, 0, 0, 0, loc); ts.Year() == 2026; ts = ts.Add(5 * time.Minute) {
				if start, due := s.Due(ts, last); due {
					fires++
					last = start
				}
			}
			if fires != 52 {
				t.Errorf("fired %d times in 2026, want 52 -- one per Sunday night", fires)
			}
		})
	}
}

// The YEAR field, which nothing exercised at all: the whole of it -- parsed,
// stored, and checked in Matches -- could be deleted and the suite stayed
// green. A year-pinned expression is unusual but legal, and one that quietly
// matched every year would run a "2027 only" window forever.
func TestYearPinnedExpressions(t *testing.T) {
	utc := time.UTC
	d, err := ParseDays("2027-*-*")
	if err != nil {
		t.Fatalf("ParseDays: %v", err)
	}
	for _, tc := range []struct {
		when string
		want bool
	}{
		{"2027-03-01 12:00", true},
		{"2026-03-01 12:00", false},
		{"2028-03-01 12:00", false},
	} {
		if got := d.Matches(at(t, utc, tc.when)); got != tc.want {
			t.Errorf("2027-*-* matched %v at %s, want %v -- the year is being ignored", got, tc.when, tc.want)
		}
	}

	t.Run("a year set", func(t *testing.T) {
		d, err := ParseDays("2026,2028-*-*")
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			when string
			want bool
		}{
			{"2026-06-01 12:00", true},
			{"2028-06-01 12:00", true},
			{"2027-06-01 12:00", false},
		} {
			if got := d.Matches(at(t, utc, tc.when)); got != tc.want {
				t.Errorf("2026,2028-*-* matched %v at %s, want %v", got, tc.when, tc.want)
			}
		}
	})

	t.Run("the leap-year rule is the real one, not just divisible by four", func(t *testing.T) {
		// feasible() asks whether 29 February can exist in the pinned years.
		// A naive y%4 check calls 2100 a leap year and accepts a schedule
		// that can never fire.
		if _, err := ParseDays("2100-02-29"); err == nil {
			t.Error("2100-02-29 was accepted; 2100 is divisible by 4 but NOT a leap year")
		}
		if _, err := ParseDays("2000-02-29"); err != nil {
			t.Errorf("2000-02-29 was refused: %v; 2000 is divisible by 400, so it IS a leap year", err)
		}
		if _, err := ParseDays("2028-02-29"); err != nil {
			t.Errorf("2028-02-29 was refused: %v", err)
		}
	})
}

// parseNumSet's UPPER bound was pinned by nothing: "*-13-*" and "*-*-32" are
// both refused by feasible() instead, so deleting `to > max` kept the suite
// green while letting a month 14 and a day 32 through.
//
// Each case below is out of range but still FEASIBLE-looking, so only the
// range check can refuse it.
func TestNumericRangesAreBounded(t *testing.T) {
	for _, tc := range []struct{ expr, why string }{
		{"*-*-01,32", "day 32 in a set whose other member is valid"},
		{"*-01,14-*", "month 14 in a set whose other member is valid"},
		{"3500-*-*", "a year past the upper bound"},
		{"1900-*-*", "a year below the lower bound"},
		{"*-*-01..40", "a day range whose upper end is out of range"},
	} {
		t.Run(tc.expr, func(t *testing.T) {
			if _, err := ParseDays(tc.expr); err == nil {
				t.Errorf("ParseDays(%q) was accepted -- %s", tc.expr, tc.why)
			}
		})
	}

	// And the boundaries themselves stay legal.
	for _, expr := range []string{"*-01-*", "*-12-*", "*-*-01", "*-*-31", "1970-*-*", "2999-*-*"} {
		if _, err := ParseDays(expr); err != nil {
			t.Errorf("ParseDays(%q) was refused: %v", expr, err)
		}
	}
}

// NextOccurrence exists so the AGENT can tell the console when a VM next
// verifies. The console must not work that out for itself: it does not know
// the host's timezone and is not the authority on the resolved schedule.
func TestNextOccurrence(t *testing.T) {
	utc := time.UTC

	t.Run("the first Sunday of next month", func(t *testing.T) {
		s, err := Parse("Sun *-*-01..07", "02:00-12:00")
		if err != nil {
			t.Fatal(err)
		}
		// Mid-March: the first Sunday of March is past, so April's is next.
		got, ok := s.NextOccurrence(at(t, utc, "2026-03-15 09:00"))
		if !ok {
			t.Fatal("no next occurrence found")
		}
		if want := at(t, utc, "2026-04-05 02:00"); !got.Equal(want) {
			t.Errorf("next = %v, want %v", got, want)
		}
	})

	t.Run("an occurrence already open returns its own start", func(t *testing.T) {
		// "Next" means the next one an operator would see, and one that is
		// open right now is it -- otherwise the console would say a VM
		// verifies next month while it is verifying.
		s, _ := Parse("Sun *-*-01..07", "02:00-12:00")
		got, ok := s.NextOccurrence(at(t, utc, "2026-03-01 06:00"))
		if !ok {
			t.Fatal("no occurrence found while inside one")
		}
		if want := at(t, utc, "2026-03-01 02:00"); !got.Equal(want) {
			t.Errorf("next = %v, want the open occurrence's start %v", got, want)
		}
	})

	t.Run("a window that has already closed today moves to the next day", func(t *testing.T) {
		s, _ := Parse("", "02:00-04:00")
		got, ok := s.NextOccurrence(at(t, utc, "2026-03-04 09:00"))
		if !ok {
			t.Fatal("no next occurrence")
		}
		if want := at(t, utc, "2026-03-05 02:00"); !got.Equal(want) {
			t.Errorf("next = %v, want tomorrow's %v", got, want)
		}
	})

	t.Run("a days-only schedule opens at the start of the day", func(t *testing.T) {
		s, _ := Parse("Sun", "")
		got, ok := s.NextOccurrence(at(t, utc, "2026-03-03 09:00"))
		if !ok {
			t.Fatal("no next occurrence")
		}
		if want := at(t, utc, "2026-03-08 00:00"); !got.Equal(want) {
			t.Errorf("next = %v, want %v", got, want)
		}
	})

	t.Run("a year pinned beyond the horizon reports none rather than guessing", func(t *testing.T) {
		s, err := Parse("2999-*-*", "02:00-04:00")
		if err != nil {
			t.Fatal(err)
		}
		if got, ok := s.NextOccurrence(at(t, utc, "2026-03-01 09:00")); ok {
			t.Errorf("found %v; a schedule further out than the horizon must report none", got)
		}
	})

	t.Run("a year pinned within the horizon IS found", func(t *testing.T) {
		s, _ := Parse("2028-*-01", "02:00-04:00")
		got, ok := s.NextOccurrence(at(t, utc, "2026-03-01 09:00"))
		if !ok {
			t.Fatal("a schedule two years out was not found; the horizon is too short")
		}
		if got.Year() != 2028 {
			t.Errorf("next = %v, want a 2028 date", got)
		}
	})

	// The search walks calendar days, so it is exposed to exactly the trap the
	// crossing-midnight branch was: in a midnight-gap zone, a day stepped by
	// duration lands on the wrong date.
	t.Run("the walk is DST-safe in every zone", func(t *testing.T) {
		for _, name := range append(append([]string{}, oddZones...),
			"America/Santiago", "America/Havana", "Atlantic/Azores") {
			t.Run(name, func(t *testing.T) {
				loc := zone(t, name)
				s, _ := Parse("Sun", "22:00-04:00")
				// Step through a whole year asking for the next occurrence;
				// every answer must be a Sunday, and never in the past.
				for d := 0; d < 365; d++ {
					now := time.Date(2026, 1, 1, 12, 0, 0, 0, loc).AddDate(0, 0, d)
					got, ok := s.NextOccurrence(now)
					if !ok {
						t.Fatalf("day %d: no next occurrence", d)
					}
					if got.Weekday() != time.Sunday {
						t.Fatalf("day %d: next = %v, a %s -- the walk landed on the wrong date",
							d, got.Format("2006-01-02 15:04 -0700"), got.Weekday())
					}
					if got.Before(now.Add(-24 * time.Hour)) {
						t.Fatalf("day %d: next = %v is in the past relative to %v", d, got, now)
					}
				}
			})
		}
	})
}
