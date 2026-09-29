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

package util

import (
	"os"
	"syscall"
)

// FlockExclusiveNB takes the exclusive advisory lock every vmsync run lock is
// built on, and fails rather than waits when somebody else holds it.
//
// One function for the whole project, because the property the callers depend on
// is a property of the OPEN FILE DESCRIPTION rather than of the file: the lock is
// released when the last descriptor referring to that description closes, which
// happens when the process exits for any reason at all -- a clean return, a
// panic, a SIGKILL, the host losing power. That is what lets both run locks be
// correct without a cleanup path: AcquireRunLock for a process on this host, and
// vmsync-bridge-helper's -hold-run-lock for one on the target.
//
// It also means the lock is NOT inherited in a way that surprises: a child that
// inherits the descriptor shares the same lock, so anything that must not extend
// the lock's life must not keep that descriptor open.
func FlockExclusiveNB(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}
