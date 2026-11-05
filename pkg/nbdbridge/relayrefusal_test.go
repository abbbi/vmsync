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

package nbdbridge

import (
	"context"
	"strings"
	"testing"
)

// requestedBy is what lets a refusal name the operator's own flag instead of
// a feature list, so every value Enabled() can be true for has to produce one.
// The table IS the completeness claim: Enabled() is Compress ||
// NetBufferEnabled(), and a new way to enable bridging that is not covered
// here would fall through to the default and be reported as -netbuffer.
func TestRequestedByNamesTheFlagThatAskedForTheRelay(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"compression alone", Config{Compress: true}, "-compress"},
		{"buffering by block size", Config{NetBufferBlock: "128k"}, "-netbuffer"},
		{"buffering by buffer size", Config{NetBufferSize: "1G"}, "-netbuffer"},
		{"both", Config{Compress: true, NetBufferBlock: "128k"}, "-compress and -netbuffer"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !c.cfg.Enabled() {
				t.Fatalf("Config%+v does not enable bridging, so this case is not testing the refusal path at all", c.cfg)
			}
			if got := requestedBy(c.cfg); got != c.want {
				t.Errorf("requestedBy = %q, want %q -- the refusal would name the wrong flag, which is worse than naming none", got, c.want)
			}
		})
	}
}

// The absent case. Everything in the message is load-bearing: the reason, the
// flag that required the helper, the host it is required on, both ways on, and
// the one thing an operator reaching for -no-checksum has to be told.
func TestTheAbsentRelayHelperRefusalNamesBothWaysOn(t *testing.T) {
	err := relayHelperVerdict(HelperStatus{Reason: "not found at /opt/vbh on hyperB"}, "-compress", "hyperB")
	if err == nil {
		t.Fatal("relayHelperVerdict returned no error for an absent helper, so a bridged run proceeds with no relay to bridge through")
	}
	got := err.Error()
	for _, want := range []struct{ text, why string }{
		{"not found at /opt/vbh on hyperB", "the probe's reason, without which the operator cannot tell absent from unreadable"},
		{"-compress", "the flag that required the helper, so the operator knows which of their own flags did this"},
		{"hyperB", "the host it is required on, which is not always the target"},
		{"deploy it there yourself", "the first way on"},
		{"drop -compress", "the second way on, which no wording said before 2026-11-05"},
		{"-no-checksum does not get past this one", "the flag an operator reaches for after meeting the other refusal, and which is evaluated before bridging"},
	} {
		if !strings.Contains(got, want.text) {
			t.Errorf("the absent-helper refusal does not say %q (%s):\n%s", want.text, want.why, got)
		}
	}
}

// The version-skew case, which is the likelier way an estate arrives here: a
// vmsync upgrade leaves every target one build behind. It must not read as
// "deploy a helper" when one is sitting right there.
func TestTheSkewedRelayHelperRefusalSaysRedeployAndNotDeploy(t *testing.T) {
	st := HelperStatus{Present: true, Version: "0.49", Reason: `at /opt/vbh on hyperB is version "0.49", but this vmsync is version "0.50"`}
	err := relayHelperVerdict(st, "-netbuffer", "hyperB")
	if err == nil {
		t.Fatal("relayHelperVerdict accepted a version-skewed helper, so a bridged run would relay through a binary whose wire behaviour it cannot predict -- Present is not the gate, Usable is")
	}
	got := err.Error()
	for _, want := range []struct{ text, why string }{
		{"Rebuild and redeploy", "the remedy for a helper that IS deployed, just not at this version"},
		{"-netbuffer", "the flag that required it"},
		{"refused exactly like one that is absent", "that there is no compatibility range, which is the surprising part"},
		{"drop -netbuffer", "the other way on"},
	} {
		if !strings.Contains(got, want.text) {
			t.Errorf("the version-skew refusal does not say %q (%s):\n%s", want.text, want.why, got)
		}
	}
	// The discriminating half: this case must NOT read as a missing binary.
	// Without this, a message that lost its Present branch entirely would pass
	// every assertion above.
	if strings.Contains(got, "deploy it there yourself") {
		t.Errorf("the version-skew refusal tells the operator to deploy a helper that is already deployed, which sends them looking for a file that is present:\n%s", got)
	}
}

// Both refusals must be ONE LINE, which the absent one was not until a bench
// check went looking for the remedy on the line vmsync died on and did not
// find it. An embedded newline puts half the message on a line with no
// timestamp and no level: trace's key=value framing breaks across it, a log
// shipper files the halves as two events or drops the second, and whoever
// greps the log gets the half that says what is missing without the half that
// says what to do.
func TestNeitherRelayRefusalSpansTwoLines(t *testing.T) {
	for _, c := range []struct {
		name string
		st   HelperStatus
	}{
		{"absent", HelperStatus{Reason: "not found at /opt/vbh on hyperB"}},
		{"version skew", HelperStatus{Present: true, Reason: "is version x, but this vmsync is y"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := relayHelperVerdict(c.st, "-compress", "hyperB")
			if err == nil {
				t.Fatalf("relayHelperVerdict accepted an unusable helper (%+v)", c.st)
			}
			if strings.ContainsAny(err.Error(), "\n\r") {
				t.Errorf("the %s refusal contains a line break, so half of it reaches the log without a timestamp or a level:\n%s", c.name, err.Error())
			}
		})
	}
}

// THE GATE, stated on its own because the two refusals above cannot state it.
//
// A usable helper must pass, and that is the assertion that stops the verdict
// from being "refuse everything" -- without it, a function that refused
// unconditionally would satisfy every other test in this file.
func TestOnlyAUsableHelperPassesTheRelayGate(t *testing.T) {
	if err := relayHelperVerdict(HelperStatus{Present: true, Version: "0.50", Usable: true}, "-compress", "hyperB"); err != nil {
		t.Errorf("relayHelperVerdict refused a usable helper: %v -- no bridged run could ever proceed", err)
	}
}

// THE WIRING. CheckRemote is the only caller, and it is the gate itself: a
// bridged run whose helper is unusable has to stop here, before the target
// disk directories and the journal intent that follow.
//
// A nil client is the one injectable failure -- ProbeHelper's first Run fails,
// so this exercises the absent branch through the real call path rather than
// the pure function alone.
func TestCheckRemoteRefusesABridgedRunAndNamesTheFlag(t *testing.T) {
	err := CheckRemote(context.Background(), nil, Config{Compress: true}, "hyperB")
	if err == nil {
		t.Fatal("CheckRemote accepted a bridged run it could not probe, so the refusal is no longer wired to the gate")
	}
	if !strings.Contains(err.Error(), "-compress") {
		t.Errorf("CheckRemote's refusal does not name -compress, so the wording is no longer coming from relayHelperRefusal:\n%s", err.Error())
	}
}

// And the inverse, so a gate that refused EVERYTHING would not pass the test
// above. An unbridged run must never be refused for the relay's helper: that
// run's own gate is run()'s checksum switch, which has a different way out.
func TestCheckRemoteLeavesAnUnbridgedRunAlone(t *testing.T) {
	if err := CheckRemote(context.Background(), nil, Config{}, "hyperB"); err != nil {
		t.Errorf("CheckRemote refused a run that asked for no bridging: %v -- that run's helper requirement is the integrity check's, and it is worded and escaped differently", err)
	}
}
