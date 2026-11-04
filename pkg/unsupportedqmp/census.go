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

// Package unsupportedqmp asks qemu directly which dirty bitmaps it holds.
//
// THIS PACKAGE IS MEANT TO BE DELETED. It exists for one reason: no supported
// libvirt API enumerates dirty bitmaps. Checked against libvirt 12.0.0 --
// nothing in `domstats --block` or `domblkinfo` carries a bitmap field, no
// virsh subcommand mentions bitmaps, and `checkpoint-dumpxml` names a bitmap
// only for a checkpoint that still exists, which an orphan by definition does
// not. When libvirt grows a supported enumeration, everything here goes and
// the single caller reverts to the image read alone.
//
// It is a separate package so that deleting it is a compile error everywhere
// it is still used rather than a silent behaviour change, and so that
// `grep -rl unsupportedqmp` returns the whole of the blast radius. Nothing in
// pkg/ may import it; the dependency direction is asserted by its own test.
//
// WHAT IT COSTS, measured rather than assumed. virDomainQemuMonitorCommand
// makes libvirt log `Domain id=N is tainted: custom-monitor` against the
// domain's current RUN -- once, before qemu sees the command, without
// inspecting it, and for a pure query as much as for a write. Restarting the
// domain is the only thing that clears it. Upstream calls the passthrough
// library unsupported with no stable API, so this route can break on a libvirt
// upgrade with no deprecation cycle. Every one of those failures is a warning
// here and never an error; see Census.Problems and the deliberate absence of
// an error result from Map.
//
// WHY IT CANNOT SIMPLY REPLACE THE IMAGE READ. Neither reader is a superset of
// the other, measured both ways on the same host. `qemu-img info` reports the
// bitmap directory the FILE holds, so it is blind to a bitmap created during
// the running qemu's life -- the orphan a failed run leaves. This reports what
// qemu HOLDS, so it is blind to a bitmap sitting in the file that qemu never
// loaded, which is what a save/restore leaves behind once the metadata is
// gone. Substituting either for the other hides a reachable orphan, which is
// why Union only ever adds.
package unsupportedqmp

import (
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"

	"vmsync/pkg/disk"
)

// qmpQueryNamedBlockNodes is sent verbatim, with no `arguments` key.
//
// Bare on purpose. An argument an older qemu rejects would switch the census
// off on that host and the operator would see a warning rather than a census,
// which is the failure this whole package exists to avoid. The same string is
// already exercised against real hardware by contrib/bench/bench.sh's bitmap
// probe, so the wire format is not a guess.
const qmpQueryNamedBlockNodes = `{"execute":"query-named-block-nodes"}`

// Reasons a node's bitmaps were seen but not claimed. Each one is reported by
// name rather than dropped: a bitmap qemu holds that vmsync will not act on is
// exactly the thing an operator needs told, and silence here would be the
// defect this codebase keeps finding -- an absent measurement reading as
// health.
const (
	ReasonBacking  = "backing"  // the file is a managed disk's backing file, not the disk itself
	ReasonNoTarget = "notarget" // the matched disk has no <target dev>, so no adoption XML can name it
	ReasonNoFile   = "nofile"   // the node reported no file at all
	ReasonUnknown  = "unknown"  // the file matches nothing this domain manages
)

// Bitmap is one dirty bitmap as qemu reports it, with the node it sits on.
type Bitmap struct {
	Name       string
	NodeName   string
	File       string
	Persistent bool
	Busy       bool
	Recording  bool
}

// UnclaimedNode is a bitmap qemu holds that the census deliberately did not
// put in its result, and why.
type UnclaimedNode struct {
	NodeName string
	File     string
	Reason   string
	Bitmaps  []string
}

// Census is what qemu answered, sorted into what vmsync may act on and
// everything it may not.
//
// ByDisk is keyed by QcowDisk.Source -- the same key leftoverCheckpointBitmaps
// returns and the same key adoptionPlan translates back into a target device.
// That is deliberate and it is what keeps this package out of the clearing
// path: the union is a map merge, and adoptionPlan, clearOrphanBitmapsOnline
// and orphanBitmaps never learn that a second reader exists.
type Census struct {
	ByDisk    map[string][]string
	Busy      map[string][]string
	Transient map[string][]string
	Unclaimed []UnclaimedNode
	Uncovered []string
	Nodes     int
	Problems  []string
}

// Proven reports whether this census can be treated as a complete answer.
//
// THE ONLY SUBTRACTIVE POWER THIS PACKAGE HAS, which is why it is the most
// conservative value in it. A true answer here lets the caller stop warning
// that an empty result proves nothing -- so a census that was partial in any
// way at all must not return true, or this route would end up silencing a
// warning it had not earned. Six conditions, all of them necessary:
// nothing complained, qemu described at least one node, every managed disk was
// covered by some node, nothing was left unclaimed, and nothing was set aside
// for being busy or non-persistent.
func (c Census) Proven() bool {
	return len(c.Problems) == 0 &&
		c.Nodes > 0 &&
		len(c.Uncovered) == 0 &&
		len(c.Unclaimed) == 0 &&
		len(c.Busy) == 0 &&
		len(c.Transient) == 0
}

// nodesReply is query-named-block-nodes, typed down to the three fields that
// decide anything.
//
// Return is a POINTER to a slice so a reply with no `return` key at all can be
// told from one carrying an empty array. The first is a failed read; the
// second is qemu saying it has no block nodes, which on a domain with disks is
// also a failed read. Conflating them is how this would report an orphaned
// source as clean.
type nodesReply struct {
	Return *[]struct {
		NodeName string `json:"node-name"`
		File     string `json:"file"`
		Drv      string `json:"drv"`
		Bitmaps  []struct {
			Name       string `json:"name"`
			Persistent bool   `json:"persistent"`
			Busy       bool   `json:"busy"`
			Recording  bool   `json:"recording"`
		} `json:"dirty-bitmaps"`
	} `json:"return"`
	Error *struct {
		Class string `json:"class"`
		Desc  string `json:"desc"`
	} `json:"error"`
}

// Parse reads qemu's reply into the bitmaps it reported and how many block
// nodes it described.
//
// Reads `return[].dirty-bitmaps[]` and NOT `image.format-specific.data.bitmaps`,
// which is present in the same reply, is what `qemu-img info` reads, and is
// EMPTY for exactly the bitmaps this package exists to find. Reading the wrong
// one would produce a census that agrees with the image read on every host and
// finds nothing new, which no test of a parser against its own fixture would
// catch.
//
// The node count is returned separately because a node carrying no bitmap
// still proves qemu answered. Without it an empty census and an empty reply
// are the same value.
func Parse(reply string) (found []Bitmap, nodes int, err error) {
	var r nodesReply
	if uerr := json.Unmarshal([]byte(reply), &r); uerr != nil {
		return nil, 0, fmt.Errorf("qemu's reply to query-named-block-nodes is not JSON this understands: %w", uerr)
	}
	// EQUIVALENT UNDER MUTATION, and kept anyway. Removing this branch changes
	// nothing a test can observe: a QMP error reply carries no `return` key, so
	// the check below refuses it with a less specific message. libvirt also
	// converts a QMP error into a libvirt error of its own before the reply
	// ever reaches here, which is why no reachable input distinguishes the two.
	// Three lines for a better message on a reply shape qemu does define, and
	// not the guard anything depends on -- that is the `return` check below.
	if r.Error != nil {
		return nil, 0, fmt.Errorf("qemu refused query-named-block-nodes: %s: %s", r.Error.Class, r.Error.Desc)
	}
	if r.Return == nil {
		return nil, 0, fmt.Errorf("qemu's reply to query-named-block-nodes carries no `return` key")
	}
	for _, n := range *r.Return {
		nodes++
		for _, b := range n.Bitmaps {
			if b.Name == "" {
				continue
			}
			found = append(found, Bitmap{
				Name:       b.Name,
				NodeName:   n.NodeName,
				File:       n.File,
				Persistent: b.Persistent,
				Busy:       b.Busy,
				Recording:  b.Recording,
			})
		}
	}
	return found, nodes, nil
}

// Map sorts what qemu reported against the disks vmsync manages.
//
// isManaged decides which names are vmsync's -- injected rather than imported
// so this package does not depend on pkg/libvirtsync, which keeps the
// dependency arrow pointing one way and makes every rule below testable with a
// one-line predicate.
//
// THE KEY IS THE NODE'S FILE PATH, never the node name. Measured on a two-disk
// domain: `libvirt-1-format` was vdb and `libvirt-2-format` was vda, so the
// node number does not follow disk order and anything reading it cross-maps
// one disk's bitmaps onto another silently. The file field is an absolute path
// that matched QcowDisk.Source exactly.
//
// Returns no error. Every complaint is a string in Problems, so there is no
// error value for a caller to accidentally propagate into a failed run.
func Map(found []Bitmap, nodes int, disks []disk.QcowDisk, isManaged func(string) bool) Census {
	c := Census{
		ByDisk:    map[string][]string{},
		Busy:      map[string][]string{},
		Transient: map[string][]string{},
		Nodes:     nodes,
	}

	// A reply describing no block nodes at all, on a domain that has disks, is
	// a failed read wearing the shape of a clean one.
	if nodes == 0 && len(disks) > 0 {
		c.Problems = append(c.Problems,
			"qemu reported no block nodes at all for a domain that has disks, so this is a failed read and not an empty one")
		return c
	}

	// Two lookups: the top layer a bitmap may be claimed on, and the backing
	// files that must never be claimed. Source == RootSource when there is no
	// backing chain, so backing is only recorded where it differs.
	type managed struct {
		source    string
		targetDev string
	}
	bySource := map[string]managed{}
	byClean := map[string]managed{}
	backing := map[string]bool{}
	for _, d := range disks {
		if d.Source == "" {
			continue
		}
		m := managed{source: d.Source, targetDev: d.TargetDev}
		bySource[d.Source] = m
		byClean[path.Clean(d.Source)] = m
		if d.RootSource != "" && d.RootSource != d.Source {
			backing[d.RootSource] = true
			backing[path.Clean(d.RootSource)] = true
		}
	}

	// Per node, so one unclaimed entry carries every bitmap on that node
	// rather than one entry per bitmap.
	type pending struct {
		node    UnclaimedNode
		bitmaps []string
	}
	unclaimed := map[string]*pending{}
	addUnclaimed := func(b Bitmap, reason string) {
		key := b.NodeName + "\x00" + reason
		p, ok := unclaimed[key]
		if !ok {
			p = &pending{node: UnclaimedNode{NodeName: b.NodeName, File: b.File, Reason: reason}}
			unclaimed[key] = p
		}
		p.bitmaps = append(p.bitmaps, b.Name)
	}

	seen := map[string]bool{} // (path, name), so a disk's format and storage nodes do not both land
	covered := map[string]bool{}

	for _, b := range found {
		// Before anything else: libvirt makes its own bitmaps for an in-flight
		// backup, and vmsync's verify window is deliberately unprefixed. Names
		// that are not vmsync's checkpoint names must never reach the
		// adopt-and-delete route, which deletes whatever it is pointed at.
		if !isManaged(b.Name) {
			continue
		}
		if b.File == "" {
			addUnclaimed(b, ReasonNoFile)
			continue
		}
		// Two lookups on the same key, exact then cleaned, which makes either
		// one alone non-load-bearing: mutating only the first to read
		// b.NodeName is equivalent, because the second still keys on the file
		// and matches. Mutating BOTH is caught by seven tests. The property
		// that matters is that the key is the FILE -- a node's number does not
		// follow disk order, measured, so reading it cross-maps one disk's
		// bitmaps onto another with nothing in the result to show it.
		m, ok := bySource[b.File]
		if !ok {
			m, ok = byClean[path.Clean(b.File)]
		}
		if !ok {
			// A backing file's bitmap belongs to whatever owns that file, not
			// to this domain's chain. Same rule, same reason, as the image
			// read's top-layer-only restriction.
			if backing[b.File] || backing[path.Clean(b.File)] {
				addUnclaimed(b, ReasonBacking)
				continue
			}
			addUnclaimed(b, ReasonUnknown)
			continue
		}
		covered[m.source] = true

		// THE ADMISSION RULE, and it is identical to what adoptionPlan itself
		// requires. A disk with a <source file> and no <target dev> parses
		// into QcowDisk{TargetDev: ""}, which adoptionPlan routes to its
		// unmapped set and clearOrphanBitmapsOnline turns into an error that
		// refuses the whole rebuild. Admitting such a bitmap would make this
		// package able to fail a run, which is the one thing it must not do.
		if m.targetDev == "" {
			addUnclaimed(b, ReasonNoTarget)
			continue
		}
		switch {
		case b.Busy:
			// An in-flight backup holds it. Adopting and deleting it now would
			// pull the bitmap out from under a running job.
			c.Busy[m.source] = appendUnique(c.Busy[m.source], b.Name)
		case !b.Persistent:
			// Nothing to clear: it was never written to the image and goes
			// when qemu closes it.
			c.Transient[m.source] = appendUnique(c.Transient[m.source], b.Name)
		default:
			// recording == false is INCLUDED deliberately. A disabled bitmap
			// is still a bitmap taking space in the qcow2 with no checkpoint
			// behind it, and it is still removable by adopt-and-delete.
			key := m.source + "\x00" + b.Name
			if seen[key] {
				continue
			}
			seen[key] = true
			c.ByDisk[m.source] = append(c.ByDisk[m.source], b.Name)
		}
	}

	// A managed disk no node's file matched. Says which files WERE reported,
	// so a symlinked or relative <source file> is diagnosable from the line.
	var reported []string
	for _, b := range found {
		if b.File != "" {
			reported = appendUnique(reported, b.File)
		}
	}
	sort.Strings(reported)
	for _, d := range disks {
		if d.Source == "" || d.TargetDev == "" || covered[d.Source] {
			continue
		}
		c.Uncovered = append(c.Uncovered, fmt.Sprintf("%s (%s); qemu described %s",
			d.Source, d.TargetDev, describeFiles(reported)))
	}

	for _, p := range unclaimed {
		p.node.Bitmaps = p.bitmaps
		sort.Strings(p.node.Bitmaps)
		c.Unclaimed = append(c.Unclaimed, p.node)
	}

	sortCensus(&c)
	return c
}

// Union merges the image read with qemu's, per disk path.
//
// ONLY EVER ADDS, and that is a load-bearing property rather than a
// convenience. Every key of fromFile survives into the result, so the
// supported path's behaviour never depends on the unsupported one and deleting
// this package cannot make vmsync act on less than the image read found. The
// admission rules in Map gate whether a name qemu reported gets IN; nothing
// anywhere removes a name the file reported.
func Union(fromFile, fromQemu map[string][]string) map[string][]string {
	out := map[string][]string{}
	for p, names := range fromFile {
		for _, n := range names {
			out[p] = appendUnique(out[p], n)
		}
	}
	for p, names := range fromQemu {
		for _, n := range names {
			out[p] = appendUnique(out[p], n)
		}
	}
	for p, names := range out {
		if len(names) == 0 {
			delete(out, p)
			continue
		}
		sort.Strings(names)
		out[p] = names
	}
	return out
}

// OnlyIn reports the names in a that are not in b, per disk path.
//
// What makes a clearing failure's severity decidable: a bitmap the image read
// also found is one vmsync could already see and already refused a rebuild
// over, while one only qemu reported is newly visible, and a run that used to
// succeed must not start failing because vmsync can now see more.
func OnlyIn(a, b map[string][]string) map[string][]string {
	out := map[string][]string{}
	for p, names := range a {
		have := map[string]bool{}
		for _, n := range b[p] {
			have[n] = true
		}
		for _, n := range names {
			if !have[n] {
				out[p] = appendUnique(out[p], n)
			}
		}
		if len(out[p]) > 0 {
			sort.Strings(out[p])
		}
	}
	return out
}

func appendUnique(dst []string, s string) []string {
	for _, existing := range dst {
		if existing == s {
			return dst
		}
	}
	return append(dst, s)
}

func describeFiles(files []string) string {
	if len(files) == 0 {
		return "no files at all"
	}
	return strings.Join(files, ", ")
}

func sortCensus(c *Census) {
	for _, m := range []map[string][]string{c.ByDisk, c.Busy, c.Transient} {
		for k := range m {
			sort.Strings(m[k])
		}
	}
	sort.Strings(c.Uncovered)
	sort.Strings(c.Problems)
	sort.Slice(c.Unclaimed, func(i, j int) bool {
		if c.Unclaimed[i].NodeName != c.Unclaimed[j].NodeName {
			return c.Unclaimed[i].NodeName < c.Unclaimed[j].NodeName
		}
		return c.Unclaimed[i].Reason < c.Unclaimed[j].Reason
	})
}

// taintNotice is said once per domain per process, and said BEFORE the call.
//
// Before, because libvirt taints the domain before qemu sees the command: a
// run killed or wedged inside the monitor call must still have recorded that
// it marked the domain. Once per domain rather than once per process, because
// a second pair in the same run would otherwise go unannounced, and once
// rather than per call because a reinit asks twice and two identical
// paragraphs train the reader to skip them.
var (
	taintMu     sync.Mutex
	taintSpoken = map[string]bool{}
)

func taintNotice(domainName string) string {
	taintMu.Lock()
	defer taintMu.Unlock()
	if taintSpoken[domainName] {
		return ""
	}
	taintSpoken[domainName] = true
	return fmt.Sprintf(`about to ask qemu directly which dirty bitmaps %s holds, because no supported libvirt API enumerates them -- and libvirt will mark this domain "tainted: custom-monitor" in its own log for doing so.

What that costs: the mark lands against this domain's CURRENT RUN, once, whether or not the query returns anything, and restarting the domain is the only thing that clears it. The query is read-only (query-named-block-nodes) and changes nothing in qemu. A read-only libvirt connection refuses the call outright and taints nothing, in which case vmsync warns and carries on with the image read alone.

Why it is paid: a dirty bitmap created during the running qemu's life is not in the qcow2 file yet, so the image read cannot see the one an interrupted run leaves behind. This route exists to be retired as soon as libvirt offers a supported way to list bitmaps.`, domainName)
}

// ResetTaintNoticeForTest lets a test exercise the once-per-domain rule
// without a second process.
func ResetTaintNoticeForTest() {
	taintMu.Lock()
	defer taintMu.Unlock()
	taintSpoken = map[string]bool{}
}
