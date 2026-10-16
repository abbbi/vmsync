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

package main

import (
	"fmt"

	"vmsync/pkg/libvirtsync"
	"vmsync/pkg/trace"
)

// domainExclusion reads one domain's exclude decision off this host.
//
// For the paths that act on a domain NAMED FROM OUTSIDE -- a control-plane
// operation, which arrives as a VM name and nothing else -- where there is
// no inventory scan already in hand to read it from. Anything iterating
// inventory.Scan results should test d.Exclude directly instead of paying
// for a second connection.
//
// IT FAILS OPEN, and that is the same direction every other "could not
// ask" in this agent takes: a foreign run lock that cannot be confirmed
// reads as not held, a fence peer that cannot be queried reads as silent.
// The reasoning holds here too and is worth stating because the instinct
// is the other way. An operator sending a failover is usually having a bad
// day; refusing it because libvirt was briefly unreachable turns a
// transient fault into a blocked emergency. A domain whose metadata cannot
// be read is in any case a domain the operation itself is about to fail
// on, with a better message than this could give.
func domainExclusion(cfg agentConfig, vm string) libvirtsync.ExcludeDecision {
	mgr, err := libvirtsync.Connect(cfg.LibvirtURI)
	if err != nil {
		trace.Warning("could not reach libvirt to check whether a domain is excluded; proceeding",
			"vm", vm, "uri", cfg.LibvirtURI, "error", err)
		return libvirtsync.ExcludeDecision{}
	}
	defer mgr.Close()

	raw, err := libvirtsync.ReadDomainMetadataField(mgr, vm, libvirtsync.MetadataFieldExclude)
	if err != nil {
		trace.Debug("could not read the exclude metadata field; proceeding",
			"vm", vm, "error", err)
		return libvirtsync.ExcludeDecision{}
	}
	return libvirtsync.Excluded(raw)
}

// excludedFromAction reports whether an operator has told vmsync to leave
// this domain alone, with the sentence to report if so.
//
// BOTH VALUES BLOCK, which is worth saying because only one of them is
// ever seen here. A fully-ignored domain is dropped by inventory.Scan, so
// the paths that iterate a scan never meet one; a control-plane operation
// naming a VM directly bypasses the scan entirely and can. Treating the
// two differently at this point would mean an operation could act on a
// domain the agent does not admit exists.
func excludedFromAction(cfg agentConfig, vm string) (bool, string) {
	d := domainExclusion(cfg, vm)
	if !d.ReportOnly && !d.Hidden {
		return false, ""
	}
	return true, libvirtsync.ExcludeRefusal(vm, d)
}

// skipExcludedDomain logs and reports whether a scanned domain is excluded
// from being acted on, for the loops that already hold inventory data.
//
// Debug rather than info: a host with a dozen excluded domains would
// otherwise repeat the same lines every sweep, and the operator who set
// the field is not the audience for being told it is working.
func skipExcludedDomain(vm, exclude, what string) bool {
	d := libvirtsync.Excluded(exclude)
	if !d.ReportOnly && !d.Hidden {
		return false
	}
	trace.Debug(fmt.Sprintf("%s skipped: domain excluded by vmsync metadata", what),
		"vm", vm, "exclude", exclude)
	return true
}
