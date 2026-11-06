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

// domainNameFromXML reads <name> from a domain document.
//
// Needed because DefineDomain has to know what the SOURCE was called after it
// has already renamed the document to the target: asking the rewritten XML
// would answer with the target's name, and every rewrite keyed on the source's
// name would then do nothing.
func domainNameFromXML(domainXML string) (string, error) {
	doc, err := parseDomainDoc(domainXML)
	if err != nil {
		return "", err
	}
	el := doc.Root().FindElement("./name")
	if el == nil {
		return "", fmt.Errorf("domain xml has no <name>")
	}
	return strings.TrimSpace(el.Text()), nil
}

// replaceDomainNvramPath repoints <os><nvram> at the target domain's own
// varstore, leaving every other element alone.
//
// In here with the other rewrites, and patched in the parsed tree rather than
// round-tripped through libvirtxml, for the reason this whole file exists: a
// typed re-marshal drops what the model does not describe, and on a target that
// is a dropped element in the definition that boots when the replica is
// promoted. TargetNvramPath decides what the new path should be; this only
// applies it.
//
// A domain with no <nvram> is returned untouched, which is most domains.
func replaceDomainNvramPath(domainXML, sourceDomain, targetDomain string) (string, error) {
	if sourceDomain == "" || sourceDomain == targetDomain {
		return domainXML, nil
	}
	doc, err := parseDomainDoc(domainXML)
	if err != nil {
		return "", err
	}
	el := doc.Root().FindElement("./os/nvram")
	if el == nil {
		return domainXML, nil
	}
	current := strings.TrimSpace(el.Text())
	want := TargetNvramPath(current, sourceDomain, targetDomain)
	if want == current {
		return domainXML, nil
	}
	el.SetText(want)
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
// ignoredDevs are the disks the operator said to leave in the definition
// exactly as the source declares them (-ignore-disk). They are not repathed:
// no copy of them exists on the target, so there is no target-side name to
// write, and the whole point of that flag is that the storage is provided
// there by other means under the name the source already uses.
func replaceDomainDiskPath(domainXML, targetDiskPath string, rootSourceByLiveSource map[string]string, ignoredDevs []string) (string, error) {
	doc, err := parseDomainDoc(domainXML)
	if err != nil {
		return "", err
	}
	ignored := make(map[string]bool, len(ignoredDevs))
	for _, dev := range ignoredDevs {
		ignored[dev] = true
	}

	for _, disk := range doc.Root().FindElements("./devices/disk") {
		dev := diskTargetDev(disk)
		switch dispositionOf(disk) {
		case dispositionCdrom:
			// Verbatim, including its <source file> -- so a cdrom with an ISO
			// keeps the SOURCE host's path in the replica's definition. That
			// is pre-existing and deliberately left alone here: it is a
			// decision about empty trays versus dangling paths, not about
			// which disks get replicated. -drop-disk removes a cdrom like any
			// other device for an operator who wants it gone.
			continue
		case dispositionUnreplicable:
			if ignored[dev] {
				continue
			}
			// THE BACKSTOP, and it is never supposed to fire. The gate in
			// cmd/vmsync refuses this run long before any copy happens, with
			// a message naming both flags. Reaching here means some caller
			// skipped that gate, and a definition naming a disk nobody copied
			// is the one outcome this package must not be able to produce:
			// where the source's path happens to resolve on the target -- a
			// shared mount, or the same images directory -- the promoted
			// replica opens a file nobody replicated, and if that storage is
			// shared with the source it writes into live source data.
			name := dev
			if name == "" {
				name = "a disk with no <target dev=>"
			}
			return "", fmt.Errorf("refusing to write the target's definition: %s is not a qcow2 disk, so no copy of it exists, and it was not named in -ignore-disk or -drop-disk", name)
		}
		if ignored[dev] {
			// A qcow2 disk the operator excluded: no copy, so nothing to
			// repath, and -ignore-disk says to leave the element alone.
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

// diskDisposition is what the target's definition should say about one disk.
//
// A boolean "is this ignored" conflated two cases the replica has to treat
// differently: a cdrom carries no replicated data and is expected on nearly
// every domain, while a non-qcow2 disk is storage the replica does not have.
// Reading the same answer for both is what let the second pass unremarked.
type diskDisposition int

const (
	// dispositionReplicate: a qcow2 disk whose bytes this run copied, so its
	// path is rewritten to where the copy actually put them.
	dispositionReplicate diskDisposition = iota
	// dispositionCdrom: left verbatim. See the note in replaceDomainDiskPath
	// about what that means for the ISO path.
	dispositionCdrom
	// dispositionUnreplicable: not qcow2, so no copy exists. Either the
	// operator named it, or the definition must not be written at all.
	dispositionUnreplicable
)

// dispositionOf mirrors util.IgnoreDevice, split into the reasons.
//
// It MUST stay a mirror: the gate in cmd/vmsync refuses from util.IgnoreDevice
// via disk.UnreplicableDisks, and this decides what the definition says. A
// disk the gate refuses but this one keeps stalls the run at the root-source
// lookup below; a disk the gate accepts but this one treats as unreplicable
// fails the define outright. Both directions are silent until they fire.
//
// Expressed against the tree rather than by unmarshalling into
// libvirtxml.DomainDisk so that this file needs no typed model at all -- the
// point of it being here is that nothing gets reconstructed.
func dispositionOf(disk *etree.Element) diskDisposition {
	if disk.SelectAttrValue("device", "") == "cdrom" {
		return dispositionCdrom
	}
	driver := disk.FindElement("./driver")
	if driver == nil || driver.SelectAttrValue("type", "") != "qcow2" {
		return dispositionUnreplicable
	}
	return dispositionReplicate
}

// diskTargetDev is the <target dev=> an operator names in -ignore-disk or
// -drop-disk, or "" for a disk that declares none.
//
// An empty answer is not a lookup failure to paper over: libvirt's schema
// allows a <disk> with no <target>, such a disk cannot be named by any flag,
// and the gate in cmd/vmsync refuses rather than letting one through.
func diskTargetDev(disk *etree.Element) string {
	target := disk.FindElement("./target")
	if target == nil {
		return ""
	}
	return target.SelectAttrValue("dev", "")
}

// removeDomainDisks deletes the named disks from a domain document, and
// reports which ones it actually removed.
//
// Its own pass, not folded into replaceDomainDiskPath, because that one runs
// only when there are paths to rewrite (shouldRewriteDiskPaths). Removal has
// to happen whatever the disk paths are doing, and a caller passing no root
// sources -- which DefineDomain's own documentation invites -- would otherwise
// get a definition still naming a disk it was told to remove.
//
// The removed set is returned rather than counted so the caller can assert it
// is exactly what was asked for. That assertion is what makes it safe to tell
// the configuration-loss tripwire that these elements were meant to go: a
// removal that matched the wrong disk would otherwise be invisible, having
// just been declared expected.
func removeDomainDisks(domainXML string, devs []string) (string, []string, error) {
	if len(devs) == 0 {
		return domainXML, nil, nil
	}
	wanted := make(map[string]bool, len(devs))
	for _, dev := range devs {
		wanted[dev] = true
	}

	doc, err := parseDomainDoc(domainXML)
	if err != nil {
		return "", nil, fmt.Errorf("parse domain xml to remove disks: %w", err)
	}
	devices := doc.Root().FindElement("./devices")
	if devices == nil {
		return "", nil, fmt.Errorf("domain xml has no <devices> element, so %s cannot be removed from it", strings.Join(devs, ", "))
	}

	var removed []string
	// FindElements returns a snapshot, so removing from the live child list
	// while ranging over it does not disturb the iteration.
	for _, disk := range devices.FindElements("./disk") {
		dev := diskTargetDev(disk)
		if dev == "" || !wanted[dev] {
			continue
		}
		devices.RemoveChild(disk)
		removed = append(removed, dev)
	}

	out, err := doc.WriteToString()
	if err != nil {
		return "", nil, err
	}
	return out, removed, nil
}

// setMetadataFieldsInDoc merges vmsync's metadata fields into a domain
// document without disturbing anything else in it.
//
// Only vmsync's own <vmsync:vmsync> element is replaced. Any other tool's
// metadata under <metadata> is left exactly where it was -- the same promise
// the old whole-document implementation made, now kept structurally rather
// than by a typed model happening to round-trip it.
// isVmsyncMetadataElement recognises vmsync's own metadata element in every
// form it can legitimately take.
//
// etree does no namespace resolution at all -- it keeps prefixes as literal
// text -- so this cannot match on the uri the way libvirt does. It applies
// the same rule libvirt.go's isMetadataContainer applies on the encoding/xml
// side instead: an element named `vmsync` that either carries vmsync's own
// prefix or DECLARES vmsync's namespace, under any prefix or as the default.
// The spellings that reach here in practice:
//
//   - `<vmsync:vmsync xmlns:vmsync="...">` -- what this writer emits, what
//     libvirt stores for the other writer, and what every domain written
//     before any of this carries;
//   - `<vmsync xmlns="...">` and `<vms:vmsync xmlns:vms="...">` -- two
//     intermediate spellings, from builds that knew a self-declared `vmsync`
//     prefix made virDomainSetMetadata fail but not yet what the extractor
//     does to a fragment that declares the uri at all (see
//     metadataFragmentStart);
//   - `<vmsync xmlns:vmsync="...">` with unprefixed children -- what libvirt
//     handed back for both of those, the declaration turned prefixed and
//     unused, the tag left bare.
//
// Missing any of them would not merely skip an update: the caller adds its
// rebuilt element afterwards regardless, so an unrecognised existing one
// stays put and the domain ends up with two vmsync metadata blocks, whose
// fields disagree from that point on.
//
// A `<vmsync>` element that neither carries the prefix nor names the uri is
// left alone -- the whole promise of this function is that other tools'
// metadata is untouched.
func isVmsyncMetadataElement(el *etree.Element) bool {
	if el.Tag != metadataPrefix {
		return false
	}
	if el.Space == metadataPrefix {
		// vmsync's own prefix. Its declaration may sit on an ancestor and so
		// not be visible on the element, which is why the absence of one is
		// not disqualifying -- but a declaration on the element naming
		// somebody ELSE's uri is.
		uri := el.SelectAttrValue("xmlns:"+metadataPrefix, "")
		return uri == "" || uri == metadataNamespace
	}
	return declaresMetadataNamespace(el)
}

// declaresMetadataNamespace reports whether el carries a declaration of
// vmsync's namespace on itself, as the default or under any prefix.
func declaresMetadataNamespace(el *etree.Element) bool {
	for _, attr := range el.Attr {
		if isMetadataNamespaceDeclaration(attr.Space, attr.Key, attr.Value) {
			return true
		}
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

	fields, err := metadataFieldsFromFragment(fragment)
	if err != nil {
		return "", err
	}
	merged, err := mergeMetadataFields(fields, updates, removeFields...)
	if err != nil {
		return "", err
	}

	if existing != nil {
		metadata.RemoveChild(existing)
	}
	if len(merged) > 0 {
		// buildMetadataElement, not buildMetadataFragment: this element is
		// going straight into a domain document, where nothing will bind it
		// and where every define runs virXMLNodeSanitizeNamespaces, which
		// deletes namespace-less children of <metadata> outright. See
		// metadataElementStart.
		fragDoc := etree.NewDocument()
		if err := fragDoc.ReadFromString(buildMetadataElement(merged)); err != nil {
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
// writes it on the element itself, but libvirt has been observed moving it,
// and a document assembled by something else may never have had it there --
// and a fragment torn out without it parses as being in no namespace.
//
// The declaration added binds the uri to the prefix the element ACTUALLY
// uses, rather than to a fixed one: the point is that the fragment resolves
// on its own once separated from the document, and a declaration for a prefix
// nothing in the fragment references does not achieve that.
func serialiseElement(el *etree.Element) (string, error) {
	cp := el.Copy()
	if !declaresMetadataNamespace(cp) {
		if cp.Space == "" {
			cp.CreateAttr("xmlns", metadataNamespace)
		} else {
			cp.CreateAttr("xmlns:"+cp.Space, metadataNamespace)
		}
	}
	doc := etree.NewDocument()
	doc.SetRoot(cp)
	out, err := doc.WriteToString()
	if err != nil {
		return "", fmt.Errorf("serialise vmsync metadata: %w", err)
	}
	return strings.TrimSpace(out), nil
}
