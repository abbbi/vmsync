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

package unsupportedqmp

import (
	"fmt"

	"libvirt.org/go/libvirt"

	"vmsync/pkg/trace"
)

// Query is the ONLY virDomainQemuMonitorCommand call in this tree.
//
// Separated from census.go so everything that DECIDES anything is in a file
// with no libvirt import: Parse, Map, Union and OnlyIn are pure and testable
// without a connection, and this is the three statements that are not.
//
// DOMAIN_QEMU_MONITOR_COMMAND_DEFAULT, never _HMP. The HMP variant answers
// with the human-readable monitor output, which no parser can read and which
// would make Parse fail on every host while the census silently reported
// nothing -- a failure mode indistinguishable from a host that has no orphans.
//
// Emits the taint notice BEFORE the call, because libvirt marks the domain
// before qemu sees the command. A run that is killed or wedges inside this
// call must still have left a record that it tainted the domain.
//
// Returns libvirt's error verbatim and decides nothing: whether a failure
// matters is the caller's judgement, and the caller's answer is always "warn
// and carry on with the image read".
func Query(dom *libvirt.Domain, domainName string) (string, error) {
	if notice := taintNotice(domainName); notice != "" {
		trace.Warning(notice, "vm", domainName)
	}
	reply, err := dom.QemuMonitorCommand(qmpQueryNamedBlockNodes, libvirt.DOMAIN_QEMU_MONITOR_COMMAND_DEFAULT)
	if err != nil {
		return "", fmt.Errorf("ask qemu which dirty bitmaps %s holds, through virDomainQemuMonitorCommand: %w", domainName, err)
	}
	return reply, nil
}
