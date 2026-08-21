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

package libvirtsync

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"vmsync/pkg/trace"

	"libvirt.org/go/libvirt"
)

// FailoverState is the observed state of a domain, as the decision rules in
// pkg/failover need it. Gathering it is all this package does; deciding
// what it means happens there, where it can be tested without libvirt.
type FailoverState struct {
	Exists           bool
	Role             string
	Active           bool
	LastCheckpoint   string
	LastSyncUnix     int64
	CheckpointAtUnix int64
	ReplicaSource    string
	ReplicaTargets   []string
	FailureCount     int
	HasCheckpoints   bool
	// SourceStoppedAtSync: the source was already shut off when this
	// replica's checkpoint was taken, so nothing was written after it.
	SourceStoppedAtSync bool
}

// ReadFailoverState collects everything a promotion or an inversion needs
// to decide, in one XML read.
//
// A domain that does not exist is reported with Exists false rather than as
// an error: "there is nothing here to promote" is a decision the caller
// should make with a good message, not a libvirt lookup failure.
func ReadFailoverState(mgr *Manager, domainName string) (FailoverState, error) {
	var st FailoverState

	dom, err := mgr.Conn.LookupDomainByName(domainName)
	if err != nil {
		if lvErr, ok := err.(libvirt.Error); ok && lvErr.Code == libvirt.ERR_NO_DOMAIN {
			return st, nil
		}
		return st, fmt.Errorf("look up domain %s: %w", domainName, err)
	}
	defer dom.Free()
	st.Exists = true

	active, err := DomainActive(dom)
	if err != nil {
		return st, fmt.Errorf("read domain %s runtime state: %w", domainName, err)
	}
	st.Active = active

	// INACTIVE for the same reason every other metadata read here uses it:
	// the persistent definition is where vmsync's metadata lives, and the
	// live one would mix in runtime-only elements.
	domXML, err := dom.GetXMLDesc(libvirt.DOMAIN_XML_INACTIVE)
	if err != nil {
		return st, fmt.Errorf("read domain %s xml: %w", domainName, err)
	}

	st.Role, _ = ParseMetadata(domXML, MetadataFieldReplicationRole)
	st.LastCheckpoint, _ = ParseMetadata(domXML, MetadataFieldLastCheckpoint)
	st.ReplicaSource, _ = ParseMetadata(domXML, MetadataFieldReplicaSource)
	st.LastSyncUnix = parseUnix(domXML, MetadataFieldLastSync)
	st.CheckpointAtUnix = parseUnix(domXML, MetadataFieldCheckpointAt)
	if v, err := ParseMetadata(domXML, MetadataFieldSourceStoppedAtSync); err == nil && v != "" {
		st.SourceStoppedAtSync = true
	}

	if v, err := ParseMetadata(domXML, MetadataFieldFailureCount); err == nil && v != "" {
		if n, convErr := strconv.Atoi(v); convErr == nil {
			st.FailureCount = n
		}
	}
	if v, err := ParseMetadata(domXML, MetadataFieldReplicaTargets); err == nil && v != "" {
		for _, e := range strings.Split(v, ",") {
			if e = strings.TrimSpace(e); e != "" {
				st.ReplicaTargets = append(st.ReplicaTargets, e)
			}
		}
	}

	// The real checkpoint objects, as opposed to the last_checkpoint string.
	// These are what a later sync would try to chain onto, and what blocks
	// an undefine, so an inversion has to know about them.
	cpts, err := ListManagedCheckpoints(dom)
	if err != nil {
		return st, fmt.Errorf("list checkpoints on domain %s: %w", domainName, err)
	}
	st.HasCheckpoints = len(cpts) > 0

	return st, nil
}

func parseUnix(domXML, field string) int64 {
	v, err := ParseMetadata(domXML, field)
	if err != nil || v == "" {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// ApplyMetadata merges updates and drops removals on a domain's vmsync
// metadata.
//
// A thin name over SetDomainMetadataFields, kept because the failover paths
// read better for it. See metadata.go for why this no longer redefines the
// domain, and why refusing on a concurrent change beats retrying.
func ApplyMetadata(mgr *Manager, domainName string, updates map[string]string, removals ...string) error {
	return SetDomainMetadataFields(mgr, domainName, updates, removals...)
}

// ShutdownDomain asks a domain to shut down cleanly and waits for it,
// bounded by timeout.
//
// It never falls back to destroying the domain. A graceful shutdown that
// does not complete means the guest is not responding to ACPI -- a stuck
// service, a dialog waiting on someone, an OS mid-update -- and pulling its
// power is a decision with consequences inside that guest, so it belongs to
// a person rather than to a timeout expiring. The caller is told plainly
// what happened and can destroy it themselves.
//
// A domain that is already shut off satisfies this immediately. That makes
// the operation convergent, which matters because the one thing a caller
// does after a partial failure is run it again.
func ShutdownDomain(ctx context.Context, mgr *Manager, domainName string, timeout time.Duration) error {
	dom, err := mgr.LookupDomain(domainName)
	if err != nil {
		return err
	}
	defer dom.Free()

	state, _, err := dom.GetState()
	if err != nil {
		return fmt.Errorf("read domain %s state: %w", domainName, err)
	}
	if state == libvirt.DOMAIN_SHUTOFF {
		trace.Info("domain is already shut down", "vm", domainName)
		return nil
	}

	trace.Info("asking domain to shut down", "vm", domainName, "timeout", timeout)
	if err := dom.ShutdownFlags(libvirt.DOMAIN_SHUTDOWN_DEFAULT); err != nil {
		return fmt.Errorf("request shutdown of domain %s: %w", domainName, err)
	}

	deadline := time.Now().Add(timeout)
	for {
		state, _, err := dom.GetState()
		if err != nil {
			return fmt.Errorf("poll domain %s state during shutdown: %w", domainName, err)
		}
		if state == libvirt.DOMAIN_SHUTOFF {
			trace.Info("domain shut down cleanly", "vm", domainName)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("domain %s did not shut down within %s and is still running -- it is not responding to a clean shutdown request; stop it yourself and retry, rather than having this destroy it", domainName, timeout)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for domain %s to shut down: %w", domainName, ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}

// StartDomain boots a domain, treating one that is already running as
// success.
//
// Convergence again, and here it is load-bearing rather than tidy: a
// promotion writes its metadata before starting the domain, so "promoted
// but not running" is a state a failed promotion legitimately leaves
// behind. Re-issuing the promotion has to be able to finish the job, and
// libvirt's own "domain is already running" error would turn the retry into
// a failure.
func StartDomain(mgr *Manager, domainName string) error {
	dom, err := mgr.LookupDomain(domainName)
	if err != nil {
		return err
	}
	defer dom.Free()

	active, err := DomainActive(dom)
	if err != nil {
		return fmt.Errorf("read domain %s runtime state: %w", domainName, err)
	}
	if active {
		trace.Info("domain is already running", "vm", domainName)
		return nil
	}
	if err := dom.Create(); err != nil {
		return fmt.Errorf("start domain %s: %w", domainName, err)
	}
	trace.Info("domain started", "vm", domainName)
	return nil
}
