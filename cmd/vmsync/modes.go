/*
	Copyright (C) 2026  Orsiris de Jong <ozy@netpower.fr>
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

package main

import (
	"context"

	"vmsync/pkg/libvirtsync"
)

// cliMode is one of vmsync's mutually exclusive non-sync modes: each changes
// state on one or two domains and exits, syncing nothing, so none of the
// sync-path validation applies to any of them.
//
// run is what makes a mode reachable. A mode whose run is nil is handled by
// its own block in main instead, because it needs exit codes this shared
// path does not offer -- and main dispatches on that field rather than on
// the mode's name, which is the whole point of the field existing. Keyed by
// name, a mode listed here with no case to match it exits 0 having done
// nothing at all: the flag parses, the mutual-exclusion check accepts it,
// and the operator is told it succeeded. That is not hypothetical --
// -release-promotion shipped that way, so every promotion record it was
// asked to clear survived the command that reported clearing it, and every
// later sync, restore and force-clean into that copy stayed refused with no
// way to lift it.
type cliMode struct {
	on   bool
	name string
	run  func(context.Context) error
}

// cliModes lists every mode flag, in the order they are reported when two
// are combined. Its own function, rather than a literal inside main, so the
// table can be asserted against modesHandledInline in a test -- the one
// place that can catch a mode which is neither dispatched here nor handled
// below before an operator does.
func cliModes(cfg syncConfig) []cliMode {
	return []cliMode{
		{cfg.Promote, "-promote", func(ctx context.Context) error { return runPromote(ctx, cfg) }},
		{cfg.Invert, "-invert", func(ctx context.Context) error { return runInvert(ctx, cfg) }},
		{cfg.ShutdownDomain, "-shutdown-domain", func(ctx context.Context) error { return runShutdownDomain(ctx, cfg) }},
		{cfg.FenceDomain, "-fence-domain", func(ctx context.Context) error { return runFenceDomain(ctx, cfg) }},
		{cfg.ReadFence, "-read-fence", func(context.Context) error { return runReadFence(cfg) }},
		{cfg.UpdateRole != "", "-update-role", nil},
		{cfg.ReleasePromotion, "-" + libvirtsync.FlagReleasePromotion, nil},
		{cfg.BreakTargetLock, "-break-target-lock", func(ctx context.Context) error { return runBreakTargetLock(ctx, cfg) }},
		{cfg.ListRestorePoints, "-list-restore-points", func(ctx context.Context) error { return runListRestorePoints(ctx, cfg) }},
		{cfg.CloneRestorePoint != "", "-clone-restore-point", func(ctx context.Context) error {
			return runCloneRestorePoint(ctx, cfg, cfg.CloneRestorePoint, cfg.CloneRestorePointTo)
		}},
		{cfg.RestoreRestorePoint != "", "-restore-restore-point", func(ctx context.Context) error {
			return runRestoreRestorePoint(ctx, cfg, cfg.RestoreRestorePoint)
		}},
		{cfg.ExplainDomain != "", "-explain-domain", func(context.Context) error { return runExplainDomain(cfg, cfg.ExplainDomain) }},
	}
}

// modesHandledInline names the modes cliModes deliberately leaves without a
// handler, because each needs an exit code the shared dispatch cannot give:
//
//   - -update-role runs before the source-side argument check, since it
//     requires neither a source domain nor a qemu+ssh:// URI.
//   - -release-promotion exits 2 on a configuration error, and distinguishes
//     a release that cleared a record from one that found none to clear --
//     the first is logged at ERROR level on success, being the last trace
//     that those disks ever held live data.
//
// The test that compares this list against cliModes is what makes the list
// worth having: a new mode added with a nil handler and no block of its own
// fails that test rather than silently succeeding in production.
var modesHandledInline = []string{"-update-role", "-" + libvirtsync.FlagReleasePromotion}
