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
	"time"

	"vmsync/pkg/inventory"
	"vmsync/pkg/libvirtsync"
	"vmsync/pkg/trace"
)

// syncableVMs returns the domains on this host a default template may cover:
// those recording replica_targets.
//
// That filter is the whole safety argument for auto-applying a default.
// replica_targets is written by a SUCCESSFUL sync, so it reaches only pairs
// somebody established by hand at least once -- a default template cannot
// start replicating a VM nobody set up. A never-synced VM would in any case
// be refused by buildSyncRequest, with a message telling the operator to run
// one sync by hand first.
//
// Cached for syncableTTL, and only called when a default template exists, so
// an estate not using templates adds no libvirt work to a loop whose virtue
// is being cheap when nothing is due.
//
// A failed scan returns the PREVIOUS list rather than an empty one, and that
// direction is deliberate: empty would mean "synthesise nothing", so a
// transient libvirt hiccup would silently drop every template-covered VM out
// of the schedule for a minute. Returning what was last known keeps them
// scheduled, and buildSyncRequest re-reads the domain per launch anyway --
// so a VM that has genuinely gone away fails there, named, rather than
// vanishing from the schedule unremarked.
func (s *Scheduler) syncableVMs(cfg *agentConfig, now time.Time) []string {
	s.mu.Lock()
	if !s.syncableAt.IsZero() && now.Sub(s.syncableAt) < syncableTTL {
		vms := s.syncable
		s.mu.Unlock()
		return vms
	}
	last := s.syncable
	s.mu.Unlock()

	mgr, err := libvirtsync.Connect(cfg.LibvirtURI)
	if err != nil {
		trace.Debug("could not connect to libvirt to list template-covered VMs; keeping the previous list",
			"uri", cfg.LibvirtURI, "error", err)
		return last
	}
	defer mgr.Close()

	domains, err := inventory.Scan(mgr)
	if err != nil {
		trace.Debug("could not scan domains to list template-covered VMs; keeping the previous list", "error", err)
		return last
	}

	vms := make([]string, 0, len(domains))
	for _, d := range domains {
		if len(d.ReplicaTargets) == 0 {
			continue
		}
		vms = append(vms, d.Name)
	}

	s.mu.Lock()
	s.syncable, s.syncableAt = vms, now
	s.mu.Unlock()
	return vms
}
