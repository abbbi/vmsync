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
	"strings"

	"vmsync/pkg/util"

	"github.com/beevik/etree"
)

// Every rewrite of a domain document that will be handed to
// DomainDefineXML happens here, by patching a parsed tree in place.
//
// The alternative -- unmarshal into libvirtxml.Domain, mutate, re-marshal --
// RECONSTRUCTS the document from a typed model, so anything that model does
// not describe is silently dropped. warnIfXMLElementsDropped exists because
// that happens; it reports the loss rather than preventing it. On a target
// this is not cosmetic: the replica's definition is what boots when the
// replica is promoted, so a dropped element is a DR failure discovered at
// exactly the wrong moment.
//
// This is the approach virt-xml takes for the same reason. It reads the
// domain into a libxml2 tree, edits the nodes it was asked to edit, and
// serialises that same tree -- so unmodelled content survives verbatim.
// Go's stdlib encoding/xml cannot do this: a token-level round-trip mangles
// namespaces, turning xmlns:qemu="..." into xmlns:_xmlns="xmlns"
// _xmlns:qemu="..." and rewriting <qemu:commandline> as <commandline
// xmlns="...">, which changes what libvirt reads. etree keeps prefixes as
// literal text and does no namespace resolution, which is exactly what is
// wanted here.
//
// Formatting is deliberately NOT normalised (no Indent call): unedited
// regions come out byte-identical, and libvirt re-formats the document when
// it stores it anyway, so there is nothing to gain by tidying it here.

// parseDomainDoc reads a domain document for patching.
func parseDomainDoc(domainXML string) (*etree.Document, error) {
	doc := etree.NewDocument()
	if err := doc.ReadFromString(domainXML); err != nil {
		return nil, fmt.Errorf("parse domain xml: %w", err)
	}
	if doc.Root() == nil {
		return nil, fmt.Errorf("domain xml has no root element")
	}
	return doc, nil
}

// replaceDomainName sets <name>, which is what makes the target a distinct
// domain rather than a second definition of the source.
func replaceDomainName(domainXML, name string) (string, error) {
	doc, err := parseDomainDoc(domainXML)
	if err != nil {
		return "", err
	}
	el := doc.Root().FindElement("./name")
	if el == nil {
		el = doc.Root().CreateElement("name")
	}
	el.SetText(name)
	return doc.WriteToString()
}

// stripDomainUUID removes <uuid> so libvirt assigns a fresh one.
//
// Removing the element is not the same as blanking it: an empty <uuid/>
// makes libvirt reject the definition, whereas an absent one makes it
// generate a UUID, which is the intent.
func stripDomainUUID(domainXML string) (string, error) {
	doc, err := parseDomainDoc(domainXML)
	if err != nil {
		return "", fmt.Errorf("parse domain xml to strip uuid: %w", err)
	}
	root := doc.Root()
	for _, el := range root.FindElements("./uuid") {
		root.RemoveChild(el)
	}
	return doc.WriteToString()
}

// replaceDomainDiskPath rewrites each replicated disk's source path to where
// that disk actually lives on the target, and drops the backing chain.
//
// rootSourceByLiveSource maps a disk's live source (which may be an external
// snapshot overlay) to the resolved base file the copy actually wrote under.
// A disk present here but missing from that map is fatal rather than passed
// through: writing a live overlay path into the target's persistent
// definition would point the domain at a file that was never replicated.
func replaceDomainDiskPath(domainXML, targetDiskPath string, rootSourceByLiveSource map[string]string) (string, error) {
	doc, err := parseDomainDoc(domainXML)
	if err != nil {
		return "", err
	}

	for _, disk := range doc.Root().FindElements("./devices/disk") {
		if ignoreDiskElement(disk) {
			continue
		}
		source := disk.FindElement("./source")
		if source == nil {
			continue
		}
		fileAttr := source.SelectAttr("file")
		if fileAttr == nil {
			continue
		}

		liveSource := fileAttr.Value
		rootSource, ok := rootSourceByLiveSource[liveSource]
		if !ok {
			return "", fmt.Errorf("disk %s: no resolved root source available, refusing to write its live path into the target's persistent definition", liveSource)
		}
		fileAttr.Value = util.SetTargetPath(targetDiskPath, rootSource)

		// The chain the source sits on does not exist on the target: the copy
		// wrote one flat file. Leaving it would point the replica at backing
		// files that are not there.
		for _, bs := range disk.FindElements("./backingStore") {
			disk.RemoveChild(bs)
		}
	}
	return doc.WriteToString()
}

// ignoreDiskElement mirrors util.IgnoreDevice for a parsed element: a cdrom,
// or anything not confirmed qcow2, is not a disk vmsync replicates.
//
// Expressed against the tree rather than by unmarshalling into
// libvirtxml.DomainDisk so that this file needs no typed model at all --
// the point of it being here is that nothing gets reconstructed.
func ignoreDiskElement(disk *etree.Element) bool {
	if disk.SelectAttrValue("device", "") == "cdrom" {
		return true
	}
	driver := disk.FindElement("./driver")
	if driver == nil {
		return true
	}
	return driver.SelectAttrValue("type", "") != "qcow2"
}

// setMetadataFieldsInDoc merges vmsync's metadata fields into a domain
// document without disturbing anything else in it.
//
// Only vmsync's own <vmsync:vmsync> element is replaced. Any other tool's
// metadata under <metadata> is left exactly where it was -- the same promise
// the old whole-document implementation made, now kept structurally rather
// than by a typed model happening to round-trip it.
// isVmsyncMetadataElement recognises vmsync's own metadata element in either
// form it can legitimately take.
//
// etree matches on the literal PREFIX, not the namespace URI, so this has to
// know about both spellings:
//
//   - `<vmsync:vmsync xmlns:vmsync="...">` -- what every domain written
//     before the SetMetadata fix carries, and what libvirt itself produces
//     when it injects its prefix;
//   - `<vmsync xmlns="...">` -- the default-namespace form vmsync now emits,
//     because a self-declared prefix made virDomainSetMetadata fail. See
//     metadataStart.
//
// Missing either one would not merely skip an update: the caller adds its
// rebuilt element afterwards, so an unrecognised existing one stays put and
// the domain ends up with two vmsync metadata blocks, whose fields disagree
// from that point on.
//
// The unprefixed form is only accepted when it actually declares vmsync's
// namespace, so a `<vmsync>` element belonging to something else is left
// alone -- the whole promise of this function is that other tools' metadata
// is untouched.
func isVmsyncMetadataElement(el *etree.Element) bool {
	if el.Tag != metadataPrefix {
		return false
	}
	switch el.Space {
	case metadataPrefix:
		return true
	case "":
		return el.SelectAttrValue("xmlns", "") == metadataNamespace
	}
	return false
}

func setMetadataFieldsInDoc(domainXML string, updates map[string]string, removeFields ...string) (string, error) {
	doc, err := parseDomainDoc(domainXML)
	if err != nil {
		return "", err
	}
	root := doc.Root()

	metadata := root.FindElement("./metadata")
	if metadata == nil {
		metadata = root.CreateElement("metadata")
	}

	var existing *etree.Element
	for _, child := range metadata.ChildElements() {
		if isVmsyncMetadataElement(child) {
			existing = child
			break
		}
	}

	fragment := ""
	if existing != nil {
		frag, err := serialiseElement(existing)
		if err != nil {
			return "", err
		}
		fragment = frag
	}

	merged, err := mergeMetadataFields(fragment, updates, removeFields...)
	if err != nil {
		return "", err
	}

	if existing != nil {
		metadata.RemoveChild(existing)
	}
	if merged != "" {
		fragDoc := etree.NewDocument()
		if err := fragDoc.ReadFromString(merged); err != nil {
			return "", fmt.Errorf("parse rebuilt vmsync metadata: %w", err)
		}
		metadata.AddChild(fragDoc.Root().Copy())
	}
	// An empty <metadata/> is legal but pointless; drop it if vmsync's
	// element was the only thing in it and it is now gone.
	if len(metadata.ChildElements()) == 0 {
		root.RemoveChild(metadata)
	}
	return doc.WriteToString()
}

// serialiseElement renders one element as a standalone fragment, ensuring
// the vmsync namespace is declared on it.
//
// The declaration may legitimately live on an ancestor -- vmsync always
// writes it on the element itself, but a document assembled by something
// else may not -- and a fragment torn out without it would parse as being
// in no namespace, so the fields would read as absent and be silently lost
// on the merge.
func serialiseElement(el *etree.Element) (string, error) {
	cp := el.Copy()
	if cp.SelectAttr("xmlns:"+metadataPrefix) == nil {
		cp.CreateAttr("xmlns:"+metadataPrefix, metadataNamespace)
	}
	doc := etree.NewDocument()
	doc.SetRoot(cp)
	out, err := doc.WriteToString()
	if err != nil {
		return "", fmt.Errorf("serialise vmsync metadata: %w", err)
	}
	return strings.TrimSpace(out), nil
}
