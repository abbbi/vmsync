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
	"fmt"

	"libvirt.org/go/libvirt"
)

// Metadata writes go through libvirt's own metadata API rather than through
// a domain redefine.
//
// The difference matters most on a SOURCE domain, which is production. The
// redefine path reads the whole domain XML, unmarshals it into
// libvirtxml.Domain, mutates, re-marshals and defines the result -- so the
// document is RECONSTRUCTED from a typed model, and anything that model
// does not describe is silently dropped. warnIfXMLElementsDropped exists
// precisely because that happens; it turns the loss into a warning rather
// than preventing it. Recording a timestamp is not worth that risk.
//
// virDomainSetMetadata replaces only the <vmsync:vmsync> element in our own
// namespace and leaves every other byte of the definition alone. libvirtd
// does the splice; vmsync never parses the rest of the domain at all.
//
// vmsync already stored its data in exactly the shape this API manages -- a
// custom namespaced element -- so this changes the write path and nothing
// about the representation. Existing domains need no migration and
// `virsh dumpxml` shows the same thing it did before.
//
// DefineDomain is deliberately NOT converted: it replaces a target's entire
// definition with one derived from the source's XML, which is what makes a
// replica a replica. That is a redefine by nature, not a metadata write.

// metadataPrefix is the namespace prefix libvirt records for our element.
const metadataPrefix = "vmsync"

// ReadDomainMetadata returns the domain's <vmsync:vmsync> fragment, or an
// empty string when it carries none.
//
// A domain with no vmsync metadata is the ordinary first-run state, not an
// error: libvirt reports it as VIR_ERR_NO_DOMAIN_METADATA, which is
// translated to "" here so callers do not each have to know that.
func ReadDomainMetadata(dom *libvirt.Domain) (string, error) {
	frag, err := dom.GetMetadata(libvirt.DOMAIN_METADATA_ELEMENT, metadataNamespace, libvirt.DOMAIN_AFFECT_CONFIG)
	if err != nil {
		if lvErr, ok := err.(libvirt.Error); ok && lvErr.Code == libvirt.ERR_NO_DOMAIN_METADATA {
			return "", nil
		}
		return "", fmt.Errorf("read vmsync metadata: %w", err)
	}
	return frag, nil
}

// mergeMetadataFields applies updates and removals to an existing
// <vmsync:vmsync> fragment and returns the new one.
//
// Pure, and operating on the fragment rather than a whole domain: the same
// merge semantics SetMetadataFields documents -- fields not mentioned are
// preserved, removals win over updates -- but with nothing else in scope to
// damage. An empty result means the element should be dropped entirely.
func mergeMetadataFields(fragment string, updates map[string]string, removals ...string) (string, error) {
	for field := range updates {
		if !metadataFieldNameRe.MatchString(field) {
			return "", fmt.Errorf("invalid vmsync metadata field name %q: must start with a letter or underscore and contain only letters, digits, '_', '.' or '-'", field)
		}
	}

	fields := allMetadataFields(fragment)
	for field, value := range updates {
		fields[field] = value
	}
	// After the updates, so a field named in both is removed -- matching
	// SetMetadataFields, where that ordering is what lets a caller express
	// "set these, drop those" in one call without ordering its own arguments.
	for _, field := range removals {
		delete(fields, field)
	}
	if len(fields) == 0 {
		return "", nil
	}
	return buildMetadataEntry(fields), nil
}

// SetDomainMetadataFields merges field changes into a domain's vmsync
// metadata without redefining the domain.
//
// The re-read before writing is the same concurrent-writer guard
// SetReplicationRole has always had, but it now compares a small fragment
// instead of two whole domain documents -- so it detects the thing it cares
// about (somebody else changed vmsync's metadata) without being defeated by
// an unrelated change elsewhere in the definition.
func SetDomainMetadataFields(mgr *Manager, domainName string, updates map[string]string, removals ...string) error {
	dom, err := mgr.Conn.LookupDomainByName(domainName)
	if err != nil {
		return fmt.Errorf("look up domain %s: %w", domainName, err)
	}
	defer dom.Free()

	before, err := ReadDomainMetadata(dom)
	if err != nil {
		return fmt.Errorf("domain %s: %w", domainName, err)
	}
	merged, err := mergeMetadataFields(before, updates, removals...)
	if err != nil {
		return fmt.Errorf("domain %s: %w", domainName, err)
	}
	if merged == before {
		return nil // nothing to write
	}

	latest, err := ReadDomainMetadata(dom)
	if err != nil {
		return fmt.Errorf("domain %s: %w", domainName, err)
	}
	if latest != before {
		return fmt.Errorf("domain %s had its vmsync metadata changed by something else while this update was being prepared; refusing to overwrite it -- check whether another vmsync invocation or an external tool is also managing this domain", domainName)
	}

	// AFFECT_CONFIG: the persistent definition is the record, and it is what
	// every read here uses (DOMAIN_XML_INACTIVE). Writing LIVE as well would
	// make a running domain's runtime XML agree, but it would also mean
	// touching a running production domain to record a timestamp, which is
	// the thing this whole change exists to stop doing.
	if err := dom.SetMetadata(libvirt.DOMAIN_METADATA_ELEMENT, merged, metadataPrefix, metadataNamespace, libvirt.DOMAIN_AFFECT_CONFIG); err != nil {
		return fmt.Errorf("write vmsync metadata on domain %s: %w", domainName, err)
	}
	return nil
}

// ReadDomainMetadataField returns one field from a domain's vmsync
// metadata, "" when absent.
func ReadDomainMetadataField(mgr *Manager, domainName, field string) (string, error) {
	dom, err := mgr.Conn.LookupDomainByName(domainName)
	if err != nil {
		return "", fmt.Errorf("look up domain %s: %w", domainName, err)
	}
	defer dom.Free()

	frag, err := ReadDomainMetadata(dom)
	if err != nil {
		return "", fmt.Errorf("domain %s: %w", domainName, err)
	}
	return allMetadataFields(frag)[field], nil
}
