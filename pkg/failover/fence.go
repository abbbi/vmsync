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

package failover

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

// Fencing shuts down a source that a failover has displaced, so one VM does
// not end up running in two places.
//
// The decision is ARMED AT PROMOTION, never inferred afterwards. An earlier
// design tried to infer it from the target's state -- role==promoted plus a
// promoted_from naming this host -- and that cannot work: a DR drill, a real
// failover, and a months-old promotion someone resolved by hand all write an
// identical record. Two of those three must not shut anything down, and
// nothing in the metadata distinguishes them, because the distinction is
// about intent rather than about state.
//
// So a promotion may write a token saying "this failover displaces
// <host:domain>, and that source must not run". A drill promotes without
// arming. The source acts on the token, never on an inference.

// FenceToken is the record a promotion writes when it arms a fence.
//
// Every field earns its place:
//
//   - ID makes the decision single-use. The agent records it in its ledger,
//     so a token acted on once is never acted on again -- across restarts,
//     across upgrades. This is what stops a January token firing in August.
//   - Source is addressed: exactly one "host:domain" may be shut down, and
//     the evaluating host verifies the token names IT. A token can never
//     take down a bystander.
//   - ArmedAt/ArmedBy put the decision in the audit trail, so a VM that was
//     stopped is explained rather than merely observed.
type FenceToken struct {
	ID      string `json:"id,omitempty"`
	Source  string `json:"source,omitempty"`
	ArmedAt int64  `json:"armed_at,omitempty"`
	ArmedBy string `json:"armed_by,omitempty"`
}

// Armed reports whether a promotion actually requested fencing.
func (t FenceToken) Armed() bool { return t.ID != "" && t.Source != "" }

// NewFenceID mints the identity of one fencing decision.
//
// Random rather than derived from the pair and a timestamp, because the
// whole value of the id is that two promotions of the same VM produce
// different ones. A derived id would let a second failover reuse the first's
// ledger entry, and the fence would be skipped as already acted on -- the
// one failure mode that leaves two copies running, which is what this whole
// mechanism exists to prevent.
//
// 16 bytes, hex, matching the control plane's own operation ids.
func NewFenceID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate a fence id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// FenceObservation is what the evaluating host read from its peer.
//
// Read from the PEER's own libvirt, not from the control plane. That is what
// makes the evidence trustworthy: a UI that is wrong, or compromised, cannot
// manufacture a token without also holding the target hypervisor.
type FenceObservation struct {
	Token FenceToken
	// TargetRole is the promoted domain's replication_role.
	TargetRole string
	// TargetActive is whether that domain is running right now.
	TargetActive bool
	// TargetRef is the peer's own "host:domain", for the explanation.
	TargetRef string
}

// FenceVerdict is the decision plus why, because a VM being shut down
// automatically must be explainable afterwards.
type FenceVerdict struct {
	Fence bool
	// Reason is always set: for a refusal it says what was missing, and for
	// a fence it says what justified it. Both end up in the log and in the
	// audit trail.
	Reason string
	// Alarm means a split brain is happening RIGHT NOW: this host is running
	// a VM that a live promoted peer holds an armed fence against.
	//
	// True alongside Fence, and -- this is the part that matters -- also
	// true when the ONLY thing stopping the fence is that it was already
	// acted on. That combination says the shutdown did not take, or somebody
	// started the domain again afterwards. Latching means refusing to retry;
	// it must not also mean going quiet, because "we tried once and it did
	// not work" is the single most important thing to still be saying.
	Alarm bool
}

// AssessFence decides whether this host must stop its own copy.
//
// self is this host's own "host:domain" for the VM being considered.
// alreadyActed is whether this token has been acted on before, from the
// agent's durable ledger.
//
// Every condition is required, and each rules out a specific way of shutting
// down a VM that should have stayed up.
func AssessFence(obs FenceObservation, self string, alreadyActed bool) FenceVerdict {
	if !obs.Token.Armed() {
		// The ordinary case for every promotion that was a drill, a
		// staging exercise, or simply performed without asking for the
		// source to be stopped.
		return FenceVerdict{Reason: "the promotion did not arm a fence, so nothing here is authorised to stop this domain"}
	}
	if !equalRef(obs.Token.Source, self) {
		// A token belonging to a different pair. Refusing loudly rather
		// than ignoring: a token that names somebody else on a host that
		// found it is worth an operator's attention.
		return FenceVerdict{Reason: fmt.Sprintf("the fence names %s, and this is %s", obs.Token.Source, self)}
	}
	if obs.TargetRole != RolePromoted {
		// The token outlived the promotion it belonged to -- the replica
		// was set back to a target, or inverted. The failover it described
		// is no longer in force.
		return FenceVerdict{Reason: fmt.Sprintf("%s is no longer promoted (role %q), so the failover this fence belongs to is over", obs.TargetRef, obs.TargetRole)}
	}
	if !obs.TargetActive {
		// The promoted copy is not serving. Stopping this one would leave
		// ZERO copies running -- and this is not a rare shape: a promotion
		// writes its metadata before starting the domain, so "promoted but
		// not started" is exactly what a staged or failed promotion leaves
		// behind. It is also how an operator says "actually, the original
		// wins": they shut the promoted copy down.
		//
		// Which copy is left running IS the decision, and this respects it.
		return FenceVerdict{Reason: fmt.Sprintf("%s is promoted but not running, so stopping this domain would leave no copy serving", obs.TargetRef)}
	}
	if alreadyActed {
		// Once per token, ever. A fence that failed is NOT retried: a guest
		// ignoring ACPI would otherwise be power-buttoned on every cycle
		// forever, which is the escalation this design refuses to perform,
		// arrived at by repetition.
		//
		// Alarm, though. Every other condition for fencing holds, so this
		// domain is running while a promoted peer serves the same VM. The
		// attempt is not repeated; the fact is reported until it stops
		// being true.
		return FenceVerdict{
			Alarm:  true,
			Reason: "this fence has already been acted on, yet this domain is still running alongside the promoted copy; a failed fence needs a person, not another attempt",
		}
	}
	return FenceVerdict{
		Fence: true,
		Alarm: true,
		Reason: fmt.Sprintf("%s was promoted at %d by %s and is running; this failover armed a fence naming %s",
			obs.TargetRef, obs.Token.ArmedAt, orUnknown(obs.Token.ArmedBy), self),
	}
}

// equalRef compares two "host:domain" references, case-insensitively on the
// host and exactly on the domain -- the same rule used wherever else in
// vmsync two replica references are matched.
func equalRef(a, b string) bool {
	ah, ad := splitRef(strings.TrimSpace(a))
	bh, bd := splitRef(strings.TrimSpace(b))
	return ah != "" && strings.EqualFold(ah, bh) && ad == bd
}

func splitRef(ref string) (host, domain string) {
	i := strings.LastIndex(ref, ":")
	if i < 0 {
		return "", ""
	}
	return ref[:i], ref[i+1:]
}

func orUnknown(s string) string {
	if s == "" {
		return "an unrecorded operator"
	}
	return s
}
