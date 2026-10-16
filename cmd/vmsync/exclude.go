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

// excludeReadOnlyModes are the verbs an exclusion must NOT block.
//
// AN ALLOWLIST, not a blocklist, so a mode added later is refused on an
// excluded domain by default rather than permitted by omission. Getting
// that default the wrong way round is how a gate quietly stops covering
// the thing it was added for.
//
// All three only READ. Blocking them would be worse than useless:
// -explain-domain is the verb an operator reaches for to find out WHY
// nothing is happening to a VM, and refusing to explain an excluded
// domain would hide the exclusion behind itself. -list-restore-points and
// -read-fence are likewise questions, not actions, and the agent asks
// -read-fence of peers it does not own.
var excludeReadOnlyModes = map[string]bool{
	"-read-fence":          true,
	"-list-restore-points": true,
	"-explain-domain":      true,
}

// resolvedTargetDomain is the domain a mode verb will actually act on,
// resolved the same way the verbs resolve it themselves.
//
// Its own function so the gate and the verbs cannot drift: -update-role
// and -release-promotion both read "-target-domain, or -source-domain if
// that is empty" (main.go), and -invert applies the same default inside
// its handler, which runs AFTER the gate. A gate that read
// cfg.TargetDomain alone checked the wrong domain, or -- for the
// documented `-target-uri X -source-domain web01` spelling -- no domain
// at all.
func resolvedTargetDomain(cfg syncConfig) string {
	if cfg.TargetDomain != "" {
		return cfg.TargetDomain
	}
	return cfg.SourceDomain
}

// refuseExcludedMode refuses one of the mode verbs when either end of the
// pair carries an exclusion.
//
// Both ends, as the sync path does, and for the same reason: the field is
// usually on the source and inherited by the replica, but a replica
// excluded on its own is exactly the case a source-only check would miss.
//
// IT FAILS OPEN on an unreadable domain, matching the sync path and the
// agent. A domain whose metadata cannot be read is one the verb itself is
// about to fail on, with a better message than this could give.
//
// NO ACTION-JOURNAL RECORD, and that is a knowing trade rather than an
// oversight. Running before the handler means running before
// journal.Intent, so an exclusion is the one refusal of -promote,
// -update-role or -release-promotion that leaves nothing in the journal.
// The alternative is a copy of this check inside each handler, which is
// how a gate comes to cover five verbs and miss the sixth -- and the
// refusal is logged and exits non-zero either way. If the journal ever
// becomes the audit trail of record for refusals, this is the line to
// revisit.
func refuseExcludedMode(cfg syncConfig, mode string) error {
	if excludeReadOnlyModes[mode] {
		return nil
	}
	// THE SAME FALLBACK THE VERBS THEMSELVES APPLY, and leaving it out made
	// three of these gates do nothing at all.
	//
	// -update-role and -release-promotion address their domain as
	// "-target-domain, or -source-domain if that is empty", and -invert
	// fills the default in INSIDE its handler, after this runs. Reading
	// cfg.TargetDomain alone meant that the documented
	// `-target-uri X -source-domain web01` spelling skipped the target side
	// for having no domain and the source side for having no URI, so the
	// gate read nothing and returned nil -- on `-update-role=target`, which
	// is precisely the write that makes an excluded domain syncable again.
	for _, side := range []struct{ what, uri, domain string }{
		{"target", cfg.TargetURI, resolvedTargetDomain(cfg)},
		{"source", cfg.SourceURI, cfg.SourceDomain},
	} {
		if side.uri == "" || side.domain == "" {
			continue
		}
		mgr, err := libvirtsync.Connect(side.uri)
		if err != nil {
			// WARNING, not debug, and the difference matters: this is the
			// gate failing to run at all. Everything after it proceeds as
			// though the domain were not excluded, so if it was, vmsync is
			// about to do the thing somebody forbade. The run still goes
			// ahead -- an unreachable host is not evidence of an exclusion
			// -- but it does not go ahead quietly.
			trace.Warning("could not connect to check whether a domain is excluded; proceeding as though it is not",
				"side", side.what, "uri", side.uri, "error", err)
			continue
		}
		raw, rerr := libvirtsync.ReadDomainMetadataField(mgr, side.domain, libvirtsync.MetadataFieldExclude)
		mgr.Close()
		if rerr != nil {
			// Debug, unlike the connect failure above: an absent vmsync
			// element reads as "" with no error, so the common cause here
			// is a domain that does not exist on that side -- which the
			// verb itself is about to report, by name, far better than
			// this could.
			trace.Debug("could not read the exclude metadata field; proceeding",
				"side", side.what, "vm", side.domain, "error", rerr)
			continue
		}
		if ex := libvirtsync.Excluded(raw); ex.ReportOnly || ex.Hidden {
			return fmt.Errorf("%w: %s refused -- %s",
				libvirtsync.ErrExcludeRefusesAction, mode, libvirtsync.ExcludeRefusal(side.domain, ex))
		}
	}
	return nil
}
