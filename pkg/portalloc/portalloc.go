/*
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

// Package portalloc picks the base TCP port a sync's NBD exports occupy,
// either from an explicit number the caller gave or by finding a free
// contiguous block inside a range.
//
// vmsync derives every port in a run from a single base: the target side
// uses [base, base+N) for N disk exports and [base+N, base+2N) for their
// bridge helpers, and the source side uses base and base+1. So choosing a
// port only ever means choosing that base -- all the offset arithmetic in
// cmd/vmsync is unchanged by this package's existence.
//
// Everything here is pure: the caller is responsible for asking the host
// which ports are already listening (over SSH, or locally) and handing the
// result in. That keeps the actual decision -- which block to take --
// directly testable without a live host, which matters because getting it
// wrong means two concurrent syncs silently fight over the same ports.
package portalloc

import (
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
)

// The DEFAULT ranges -- what a run uses when nothing is passed at all.
//
// Anchored at the historical fixed defaults so firewall guidance stays
// recognizable: a host already allowing 10809/20809 only needs the range
// widened, not moved.
//
// These used to be reachable only by typing "auto", with the default being
// the single fixed port. That was the wrong way round. A range costs nothing
// when one run is active and is the difference between working and colliding
// when two are, so it is what an operator who has not thought about ports
// should get -- and an operator who HAS thought about them can still pin one.
// Nothing in an agent-managed estate ever set a port range (no preset, no form
// field, no handler), so every VM took the fixed port and any two concurrent
// syncs to one target host shared every port.
//
// Sized in the unit that actually matters, which is ports per DISK on the
// target: a run takes 4 per disk with -verify and -compress together (3 with
// verify alone, 2 with bridging alone, 1 plain). 200 target ports is therefore
// 50 concurrent single-disk replicas, or 12 four-disk ones. The source side is
// per RUN rather than per disk -- 1 port, 2 with a bridge -- so 100 is already
// far more than an estate can use.
const (
	DefaultSourceAutoLow  = 10809
	DefaultSourceAutoHigh = 10908
	DefaultTargetAutoLow  = 20809
	DefaultTargetAutoHigh = 21008
)

// DefaultSourceSpec and DefaultTargetSpec are the flag defaults, as strings,
// so the CLI default and the range above cannot drift apart.
var (
	DefaultSourceSpec = fmt.Sprintf("%d-%d", DefaultSourceAutoLow, DefaultSourceAutoHigh)
	DefaultTargetSpec = fmt.Sprintf("%d-%d", DefaultTargetAutoLow, DefaultTargetAutoHigh)
)

// Spec is a parsed -source-nbd-port / -target-nbd-port value: either one
// fixed base port, or an inclusive range to choose a free block from.
type Spec struct {
	// Fixed is the explicit base port, or 0 when Low/High apply instead.
	Fixed int
	// Low/High bound the search when Fixed is 0. Inclusive.
	Low, High int
}

// IsFixed reports whether this spec names one exact base port, in which
// case SelectBase returns it without consulting anything.
func (s Spec) IsFixed() bool { return s.Fixed > 0 }

func (s Spec) String() string {
	if s.IsFixed() {
		return strconv.Itoa(s.Fixed)
	}
	return fmt.Sprintf("%d-%d", s.Low, s.High)
}

// ParseSpec parses a port flag value. Two accepted forms:
//
//	"20000-20100"  choose a free contiguous block inside this range
//	"20809"        pin one exact base port
//
// The overloading is deliberate rather than a separate -port-range flag:
// the source export and the target exports live on different hosts with
// different firewall policies, so a range belongs per side, and this CLI
// already overloads -compress, -verify and -netbuffer the same way.
//
// There used to be a third form, the keyword "auto", meaning "choose inside
// [defLow, defHigh]". It is gone because the default became a range: "auto"
// then meant precisely what passing nothing means, and a keyword whose only
// effect is to restate the default is a thing to explain rather than a thing
// to use. An empty value is still an error rather than silently defaulting --
// the flag has a default, so an empty string reaching here means a caller
// built one, which is a bug worth surfacing rather than papering over.
//
// defLow/defHigh are still parameters because callers pass the side-specific
// range and the error text quotes it; they are no longer selected by a
// keyword.
func ParseSpec(value string, defLow, defHigh int) (Spec, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return Spec{}, fmt.Errorf("port specification is empty: give a range (%d-%d) or one fixed port (%d)", defLow, defHigh, defLow)
	}

	// Accepted for one release, so a config or unit file carrying "auto" does
	// not fail closed on upgrade. It now resolves to the same range the
	// default does, which is what it always meant.
	if strings.EqualFold(value, "auto") {
		return newRange(defLow, defHigh, value)
	}

	if low, high, ok := strings.Cut(value, "-"); ok {
		l, err := parsePort(strings.TrimSpace(low))
		if err != nil {
			return Spec{}, fmt.Errorf("port range %q: lower bound: %w", value, err)
		}
		h, err := parsePort(strings.TrimSpace(high))
		if err != nil {
			return Spec{}, fmt.Errorf("port range %q: upper bound: %w", value, err)
		}
		return newRange(l, h, value)
	}

	p, err := parsePort(value)
	if err != nil {
		return Spec{}, err
	}
	return Spec{Fixed: p}, nil
}

func newRange(low, high int, original string) (Spec, error) {
	if low > high {
		return Spec{}, fmt.Errorf("port range %q: lower bound %d is above upper bound %d", original, low, high)
	}
	return Spec{Low: low, High: high}, nil
}

func parsePort(s string) (int, error) {
	p, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a port number", s)
	}
	// Below 1024 needs privileges qemu-nbd and the bridge helper are not
	// guaranteed to have, and colliding with a well-known service is a
	// worse failure than being told no here.
	if p < 1024 || p > 65535 {
		return 0, fmt.Errorf("port %d is out of range (must be 1024-65535)", p)
	}
	return p, nil
}

// SelectBase returns the base port a run should build its layout from.
//
// A fixed spec is returned as-is: the caller asked for exactly that port,
// and reporting it as busy here would only duplicate, less precisely, the
// bind error that follows. A range spec is searched for the first block of
// `need` consecutive ports where none is in `used`.
//
// The search does not start at the bottom of the range. It starts at an
// offset derived from skew (see Skew) and wraps, so that two syncs of
// DIFFERENT vms into the same target host -- which nothing serializes,
// since vmsync's run lock is keyed by source domain -- tend to pick
// different blocks rather than both racing for the first free one. Two
// syncs of the same vm cannot happen concurrently, so a per-vm skew has no
// downside: a given vm lands on the same ports run after run as long as the
// host's occupancy is unchanged, which keeps firewall logs and tcpdump
// filters stable and makes the choice reproducible when debugging.
func SelectBase(used map[int]bool, spec Spec, need int, skew uint32) (int, error) {
	if spec.IsFixed() {
		return spec.Fixed, nil
	}
	if need < 1 {
		return 0, fmt.Errorf("need %d ports: must be at least 1", need)
	}

	span := spec.High - spec.Low + 1
	candidates := span - need + 1
	if candidates < 1 {
		return 0, fmt.Errorf("port range %s holds %d ports, but this run needs %d consecutive ones -- widen the range", spec, span, need)
	}

	start := int(skew % uint32(candidates))
	for i := 0; i < candidates; i++ {
		base := spec.Low + (start+i)%candidates
		if blockFree(used, base, need) {
			return base, nil
		}
	}
	return 0, fmt.Errorf("no %d consecutive free ports in range %s -- %d of its %d ports are already in use", need, spec, countUsedIn(used, spec), span)
}

func blockFree(used map[int]bool, base, need int) bool {
	for p := base; p < base+need; p++ {
		if used[p] {
			return false
		}
	}
	return true
}

func countUsedIn(used map[int]bool, spec Spec) int {
	n := 0
	for p := range used {
		if p >= spec.Low && p <= spec.High {
			n++
		}
	}
	return n
}

// Skew turns a stable identifier (vmsync passes the target domain name)
// into the search offset SelectBase starts from. Any hash would do; FNV-1a
// is in the standard library, needs no seeding, and is stable across
// processes and releases -- which is the property that matters, since a
// changing skew would move a vm's ports from run to run for no reason.
func Skew(id string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	return h.Sum32()
}

// ParseListening extracts the set of listening TCP ports from `ss -Htln`
// output. The relevant column is the local address, whose port is whatever
// follows the final colon -- a form that covers every address shape ss
// prints: 0.0.0.0:22, [::]:22, *:80 and 127.0.0.1:10809 alike.
//
// Unparsable lines are skipped rather than failing the whole parse. The
// result is used to AVOID ports, so a line this doesn't understand costs at
// worst a collision that the bind itself will then report -- whereas
// rejecting the output outright would take out port selection entirely on
// any host whose ss prints something unexpected.
func ParseListening(ssOutput string) map[int]bool {
	used := map[int]bool{}
	for _, line := range strings.Split(ssOutput, "\n") {
		fields := strings.Fields(line)
		// State Recv-Q Send-Q Local:Port Peer:Port [Process]
		if len(fields) < 4 {
			continue
		}
		local := fields[3]
		idx := strings.LastIndex(local, ":")
		if idx < 0 || idx == len(local)-1 {
			continue
		}
		if p, err := strconv.Atoi(local[idx+1:]); err == nil && p > 0 {
			used[p] = true
		}
	}
	return used
}

// ListeningCommand is the shell command whose output ParseListening
// expects. Deliberately without ss's -p flag: the owning process is
// irrelevant here, and -p needs privileges this may not have.
const ListeningCommand = "ss -Htln"
