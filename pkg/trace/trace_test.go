/*
	Copyright (C) 2026  Michael Ablassmeier <abi@grinser.de>

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

package trace

import "testing"

// WarningCount/ErrorCount are process-lifetime counters with no reset (see
// their own doc comments), so these tests assert the *delta* Warning/Error
// calls produce rather than an absolute value -- keeping them correct
// regardless of test execution order or repeated runs (-count=N) within the
// same process.

func TestWarningCountIncrementsOnWarning(t *testing.T) {
	before := WarningCount()

	Warning("test warning one")
	Warning("test warning two", "key", "value")

	if got := WarningCount() - before; got != 2 {
		t.Errorf("WarningCount() delta = %d, want 2", got)
	}
}

func TestErrorCountIncrementsOnError(t *testing.T) {
	before := ErrorCount()

	Error("test error one")

	if got := ErrorCount() - before; got != 1 {
		t.Errorf("ErrorCount() delta = %d, want 1", got)
	}
}

func TestInfoDebugDoNotAffectWarningOrErrorCount(t *testing.T) {
	warningBefore := WarningCount()
	errorBefore := ErrorCount()

	Info("test info")
	Debug("test debug")
	DebugError("test debug error", nil)

	if got := WarningCount() - warningBefore; got != 0 {
		t.Errorf("WarningCount() delta after Info/Debug = %d, want 0", got)
	}
	if got := ErrorCount() - errorBefore; got != 0 {
		t.Errorf("ErrorCount() delta after Info/Debug = %d, want 0", got)
	}
}
