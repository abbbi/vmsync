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
	"strings"
	"testing"
)

// armedFence is the shape that SHOULD fence: a live promoted peer holding a
// token that names us, never acted on. Each test below starts from this and
// breaks exactly one thing, so a failure names the condition that stopped
// mattering rather than leaving it to be worked out from a struct literal.
func armedFence() FenceObservation {
	return FenceObservation{
		Token: FenceToken{
			ID:      "f7c1e4a2-0b3d-4c6e-9a11-8d2f5e7b0c93",
			Source:  "prod01:web01",
			ArmedAt: 1_755_000_000,
			ArmedBy: "alice",
		},
		TargetRole:   RolePromoted,
		TargetActive: true,
		TargetRef:    "dr01:web01",
	}
}

const self = "prod01:web01"

func TestAssessFenceActsOnAnArmedTokenThatNamesThisHost(t *testing.T) {
	v := AssessFence(armedFence(), self, false)
	if !v.Fence {
		t.Fatalf("a live promoted peer holding a fence naming this host must fence, got refusal: %s", v.Reason)
	}
	// The reason is not decoration: it is what an operator reads when they
	// find a production VM shut down and want to know who decided that.
	for _, want := range []string{"dr01:web01", "alice", self} {
		if !strings.Contains(v.Reason, want) {
			t.Errorf("the justification must name %q so the shutdown is explainable, got: %s", want, v.Reason)
		}
	}
}

// An unarmed promotion is the ordinary case -- a drill, a staging exercise,
// or any promotion performed without asking for the source to be stopped.
// This is the single most important refusal in the file: getting it wrong
// means a DR test shuts down production.
func TestAssessFenceRefusesWhenNoFenceWasArmed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token FenceToken
	}{
		{"nothing at all", FenceToken{}},
		{"a drill that recorded who and when but armed nothing", FenceToken{ArmedAt: 1_755_000_000, ArmedBy: "alice"}},
		{"an id with no addressee", FenceToken{ID: "f7c1e4a2"}},
		{"an addressee with no id", FenceToken{Source: self}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obs := armedFence()
			obs.Token = tc.token
			if v := AssessFence(obs, self, false); v.Fence {
				t.Fatalf("a promotion that armed no fence must never stop a source; reason given: %s", v.Reason)
			}
		})
	}
}

// A token is addressed. Finding one is not permission to act on it.
func TestAssessFenceRefusesATokenAddressedToSomebodyElse(t *testing.T) {
	for _, tc := range []struct {
		name, source string
	}{
		{"another host entirely", "prod02:web01"},
		{"the same host, a different VM", "prod01:web02"},
		{"a different VM differing only by case, which libvirt treats as distinct", "prod01:WEB01"},
		{"a bare domain with no host", "web01"},
		{"empty after the colon", "prod01:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obs := armedFence()
			obs.Token.Source = tc.source
			if v := AssessFence(obs, self, false); v.Fence {
				t.Fatalf("a fence naming %q must not stop %q; reason given: %s", tc.source, self, v.Reason)
			}
		})
	}
}

// Hostnames are case-insensitive and pick up trailing whitespace in
// hand-edited XML; neither should stop a real fence from being honoured.
func TestAssessFenceMatchesHostnamesTheWayDNSDoes(t *testing.T) {
	for _, source := range []string{"PROD01:web01", "Prod01:web01", " prod01:web01 "} {
		obs := armedFence()
		obs.Token.Source = source
		if v := AssessFence(obs, self, false); !v.Fence {
			t.Errorf("token source %q names this host and must fence, got refusal: %s", source, v.Reason)
		}
	}
}

// The token outlived the failover it described: somebody set the replica back
// to a target, or inverted the pair. Either way the promotion is over.
func TestAssessFenceRefusesOnceThePeerIsNoLongerPromoted(t *testing.T) {
	for _, role := range []string{RoleTarget, RoleSource, RolePaused, "", "some-future-role"} {
		obs := armedFence()
		obs.TargetRole = role
		if v := AssessFence(obs, self, false); v.Fence {
			t.Errorf("a peer whose role is %q is not serving a failover and must not stop this source; reason: %s", role, v.Reason)
		}
	}
}

// The condition that keeps a fence from taking the last copy down. A
// promotion writes its metadata BEFORE starting the domain, so "promoted but
// not running" is a real intermediate state and not a contrived one.
func TestAssessFenceRefusesWhenThePromotedPeerIsNotRunning(t *testing.T) {
	obs := armedFence()
	obs.TargetActive = false
	v := AssessFence(obs, self, false)
	if v.Fence {
		t.Fatalf("stopping this source while the promoted copy is down would leave nothing serving; reason: %s", v.Reason)
	}
	if !strings.Contains(v.Reason, "no copy serving") {
		t.Errorf("the refusal should say why, got: %s", v.Reason)
	}
}

// Latch once, per the agreed design: a fence that was attempted is never
// attempted again, INCLUDING one that failed. A guest ignoring ACPI must
// summon a person, not an escalating retry loop.
func TestAssessFenceNeverActsOnTheSameTokenTwice(t *testing.T) {
	v := AssessFence(armedFence(), self, true)
	if v.Fence {
		t.Fatalf("a token already acted on must not fire again; reason: %s", v.Reason)
	}
	// Not retrying is not the same as going quiet. Every other condition
	// holds, so this domain is running alongside a live promoted copy right
	// now -- the exact situation somebody has to be told about, and the one
	// a latch could otherwise hide forever.
	if !v.Alarm {
		t.Error("a fence that was already acted on, while the domain still runs beside a live promoted peer, is an active split brain and must still raise the alarm")
	}
}

// Alarm marks "a split brain is happening now", which is a narrower claim
// than "no fence fired". Getting this wrong in the loud direction trains
// people to ignore the alert; getting it wrong in the quiet direction is
// worse.
func TestAssessFenceRaisesTheAlarmOnlyForARealSplitBrain(t *testing.T) {
	for _, tc := range []struct {
		name        string
		obs         FenceObservation
		alreadyDone bool
		want        bool
	}{
		{"fencing now", armedFence(), false, true},
		{"already acted on, still running beside a live promoted peer", armedFence(), true, true},
		{"no fence was ever armed", func() FenceObservation {
			o := armedFence()
			o.Token = FenceToken{}
			return o
		}(), false, false},
		{"the promoted peer is not running, so only one copy serves", func() FenceObservation {
			o := armedFence()
			o.TargetActive = false
			return o
		}(), false, false},
		{"the peer is no longer promoted", func() FenceObservation {
			o := armedFence()
			o.TargetRole = RoleTarget
			return o
		}(), false, false},
		{"the token names another pair", func() FenceObservation {
			o := armedFence()
			o.Token.Source = "prod02:web09"
			return o
		}(), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := AssessFence(tc.obs, self, tc.alreadyDone).Alarm; got != tc.want {
				t.Errorf("Alarm = %v, want %v", got, tc.want)
			}
		})
	}
}

// Every refusal must explain itself. A silent no is indistinguishable from a
// fence that never ran, which is the hardest kind of DR failure to diagnose.
func TestAssessFenceAlwaysExplainsItself(t *testing.T) {
	cases := map[string]FenceObservation{
		"unarmed":      {TargetRole: RolePromoted, TargetActive: true},
		"someone else": func() FenceObservation { o := armedFence(); o.Token.Source = "other:vm"; return o }(),
		"not promoted": func() FenceObservation { o := armedFence(); o.TargetRole = RoleTarget; return o }(),
		"not running":  func() FenceObservation { o := armedFence(); o.TargetActive = false; return o }(),
	}
	for name, obs := range cases {
		if v := AssessFence(obs, self, false); v.Reason == "" {
			t.Errorf("%s: refused without saying why", name)
		}
	}
	if v := AssessFence(armedFence(), self, true); v.Reason == "" {
		t.Error("already acted on: refused without saying why")
	}
}

// A missing self-identity must not turn into a match against a malformed
// token -- both sides empty is exactly the shape a bug upstream produces.
func TestAssessFenceRefusesWhenThisHostCannotIdentifyItself(t *testing.T) {
	obs := armedFence()
	obs.Token.Source = ""
	if v := AssessFence(obs, "", false); v.Fence {
		t.Fatal("two empty references must never be treated as naming each other")
	}
	obs = armedFence()
	obs.Token.ID = "f7c1e4a2"
	obs.Token.Source = ":"
	if v := AssessFence(obs, ":", false); v.Fence {
		t.Fatal("a degenerate reference must never match")
	}
}

func TestFenceTokenArmedRequiresBothIdentityAndAddressee(t *testing.T) {
	for _, tc := range []struct {
		token FenceToken
		want  bool
	}{
		{FenceToken{}, false},
		{FenceToken{ID: "x"}, false},
		{FenceToken{Source: "h:v"}, false},
		{FenceToken{ID: "x", Source: "h:v"}, true},
	} {
		if got := tc.token.Armed(); got != tc.want {
			t.Errorf("FenceToken%+v.Armed() = %v, want %v", tc.token, got, tc.want)
		}
	}
}
