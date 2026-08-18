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

package portalloc

import (
	"strings"
	"testing"
)

func TestParseSpec(t *testing.T) {
	const defLow, defHigh = 20809, 21008

	t.Run("a bare number is a fixed base port", func(t *testing.T) {
		got, err := ParseSpec("20809", defLow, defHigh)
		if err != nil {
			t.Fatalf("ParseSpec() error = %v", err)
		}
		if !got.IsFixed() || got.Fixed != 20809 {
			t.Errorf("ParseSpec(\"20809\") = %+v, want a fixed 20809", got)
		}
	})

	t.Run("a range is parsed inclusive", func(t *testing.T) {
		got, err := ParseSpec("20000-20100", defLow, defHigh)
		if err != nil {
			t.Fatalf("ParseSpec() error = %v", err)
		}
		if got.IsFixed() || got.Low != 20000 || got.High != 20100 {
			t.Errorf("ParseSpec(\"20000-20100\") = %+v, want the range 20000-20100", got)
		}
	})

	t.Run("auto takes the caller's default range, case-insensitively", func(t *testing.T) {
		for _, in := range []string{"auto", "AUTO", "Auto"} {
			got, err := ParseSpec(in, defLow, defHigh)
			if err != nil {
				t.Fatalf("ParseSpec(%q) error = %v", in, err)
			}
			if got.IsFixed() || got.Low != defLow || got.High != defHigh {
				t.Errorf("ParseSpec(%q) = %+v, want the default range %d-%d", in, got, defLow, defHigh)
			}
		}
	})

	t.Run("surrounding and internal whitespace is tolerated", func(t *testing.T) {
		got, err := ParseSpec("  20000 - 20100  ", defLow, defHigh)
		if err != nil {
			t.Fatalf("ParseSpec() error = %v", err)
		}
		if got.Low != 20000 || got.High != 20100 {
			t.Errorf("ParseSpec(padded range) = %+v, want 20000-20100", got)
		}
	})

	rejected := []struct {
		in  string
		why string
	}{
		{"", "an empty value is a missing flag, not a request for a default"},
		{"notaport", "non-numeric"},
		{"20100-20000", "inverted bounds would make every range search fail confusingly later"},
		{"80-100", "below 1024 needs privileges qemu-nbd may not have"},
		{"20000-70000", "above 65535 is not a port"},
		{"0", "port 0 is not a base port vmsync can derive a layout from"},
		{"20000-", "a range missing its upper bound"},
		{"-20100", "a range missing its lower bound"},
	}
	for _, tc := range rejected {
		t.Run("rejects "+tc.in, func(t *testing.T) {
			if _, err := ParseSpec(tc.in, defLow, defHigh); err == nil {
				t.Errorf("ParseSpec(%q) returned no error -- %s", tc.in, tc.why)
			}
		})
	}
}

func TestSelectBaseFixedSpecIsReturnedUnchecked(t *testing.T) {
	// A fixed port is the operator's explicit instruction. Reporting it as
	// busy here would only pre-empt, less precisely, the bind error that
	// follows -- and would break every existing deployment whose port is
	// legitimately still held by a previous run's export that is about to
	// be replaced.
	used := map[int]bool{20809: true, 20810: true, 20811: true}
	got, err := SelectBase(used, Spec{Fixed: 20809}, 3, 0)
	if err != nil {
		t.Fatalf("SelectBase() error = %v", err)
	}
	if got != 20809 {
		t.Errorf("SelectBase(fixed 20809, all busy) = %d, want 20809 regardless", got)
	}
}

func TestSelectBaseFindsAFreeBlock(t *testing.T) {
	spec := Spec{Low: 20000, High: 20019}

	t.Run("skips a block whose ports are partly in use", func(t *testing.T) {
		// Skew 0 starts the scan at Low. Bases 20000..20004 are each
		// blocked: 20000 by itself, 20001-20003 because their 4-port block
		// reaches 20004, and 20004 by itself. 20005 is the first clear one.
		used := map[int]bool{20000: true, 20004: true}
		got, err := SelectBase(used, spec, 4, 0)
		if err != nil {
			t.Fatalf("SelectBase() error = %v", err)
		}
		if got != 20005 {
			t.Errorf("SelectBase() = %d, want 20005 (first block of 4 clear of both busy ports)", got)
		}
		for p := got; p < got+4; p++ {
			if used[p] {
				t.Errorf("SelectBase() returned base %d, but %d within the block is in use", got, p)
			}
		}
	})

	t.Run("an empty host yields the range's own start when unskewed", func(t *testing.T) {
		got, err := SelectBase(map[int]bool{}, spec, 4, 0)
		if err != nil {
			t.Fatalf("SelectBase() error = %v", err)
		}
		if got != 20000 {
			t.Errorf("SelectBase() = %d, want 20000", got)
		}
	})

	t.Run("the whole range fits exactly one maximal block", func(t *testing.T) {
		got, err := SelectBase(map[int]bool{}, spec, 20, 12345)
		if err != nil {
			t.Fatalf("SelectBase() error = %v", err)
		}
		if got != 20000 {
			t.Errorf("SelectBase() = %d, want 20000 -- only one block of 20 exists, so skew cannot move it", got)
		}
	})
}

func TestSelectBaseRefusesWhenItCannotFit(t *testing.T) {
	t.Run("range smaller than the block", func(t *testing.T) {
		_, err := SelectBase(map[int]bool{}, Spec{Low: 20000, High: 20004}, 6, 0)
		if err == nil {
			t.Fatal("SelectBase() returned no error for a 5-port range asked for 6 consecutive ports")
		}
		// The operator's fix is to widen the range, so the message has to
		// carry both numbers -- this surfaces from a cron job whose only
		// output is a log line.
		for _, want := range []string{"20000-20004", "6"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q", err.Error(), want)
			}
		}
	})

	t.Run("range large enough but too fragmented", func(t *testing.T) {
		// Every third port busy: no three consecutive ports anywhere.
		used := map[int]bool{}
		for p := 20000; p <= 20020; p += 3 {
			used[p] = true
		}
		if _, err := SelectBase(used, Spec{Low: 20000, High: 20020}, 3, 0); err == nil {
			t.Fatal("SelectBase() returned no error despite no free block of 3 existing")
		}
	})
}

// TestSelectBaseSkewSeparatesDifferentVMs is the property the skew exists
// for: vmsync's run lock is keyed by source domain, so two syncs of
// DIFFERENT vms into the same target host run concurrently with nothing
// serializing them. Scanning from the bottom of the range would make both
// pick the same first free block and race for it.
func TestSelectBaseSkewSeparatesDifferentVMs(t *testing.T) {
	spec := Spec{Low: 20000, High: 20199}
	const need = 4

	bases := map[int]string{}
	vms := []string{"web01", "db01", "mail01", "ldap01", "ci01", "nfs01", "dns01", "proxy01"}
	for _, vm := range vms {
		base, err := SelectBase(map[int]bool{}, spec, need, Skew(vm))
		if err != nil {
			t.Fatalf("SelectBase(%s) error = %v", vm, err)
		}
		if other, seen := bases[base]; seen {
			t.Logf("%s and %s both chose base %d", vm, other, base)
		}
		bases[base] = vm
	}
	// Asserted loosely on purpose. The exact spread is a property of FNV,
	// not of this package, and pinning it would turn an unrelated hash
	// change into a failure here. What must hold is that the skew separates
	// vms at all: without it every one of these lands on 20000, so a result
	// anywhere near len(vms) proves the mechanism works.
	if len(bases) < 6 {
		t.Errorf("%d vms produced only %d distinct bases on an idle host -- the skew is not separating them", len(vms), len(bases))
	}
}

// TestSkewIsStable pins the property that makes a skewed scan debuggable:
// the same vm must land on the same ports run after run, so firewall logs
// and tcpdump filters stay valid and a failure is reproducible.
func TestSkewIsStable(t *testing.T) {
	first := Skew("web01")
	for i := 0; i < 100; i++ {
		if got := Skew("web01"); got != first {
			t.Fatalf("Skew(\"web01\") returned %d then %d -- must be stable within a process", first, got)
		}
	}
	if Skew("web01") == Skew("web02") {
		t.Error("Skew() collided on two obviously different names")
	}
}

func TestParseListening(t *testing.T) {
	// Real `ss -Htln` output shapes: IPv4, IPv6, wildcard, loopback, and a
	// high port from a concurrent vmsync run.
	const out = `LISTEN 0      4096         0.0.0.0:22         0.0.0.0:*
LISTEN 0      4096            [::]:22            [::]:*
LISTEN 0      128                *:80                 *:*
LISTEN 0      4096       127.0.0.1:20809      0.0.0.0:*
LISTEN 0      4096   192.168.1.5:20810      0.0.0.0:*
`
	got := ParseListening(out)
	for _, want := range []int{22, 80, 20809, 20810} {
		if !got[want] {
			t.Errorf("ParseListening() missed port %d (full result: %v)", want, got)
		}
	}
	if len(got) != 4 {
		t.Errorf("ParseListening() = %v, want exactly 4 distinct ports", got)
	}
}

func TestParseListeningSkipsUnparsableLinesRatherThanFailing(t *testing.T) {
	// The result is used to AVOID ports, so a line this cannot read costs
	// at worst one collision that the bind then reports. Rejecting the
	// whole output would instead take out port selection entirely on any
	// host whose ss prints something unexpected.
	const out = `LISTEN 0      4096         0.0.0.0:22         0.0.0.0:*
this is not ss output at all
LISTEN 0
LISTEN 0      4096         0.0.0.0:notaport   0.0.0.0:*
LISTEN 0      4096         0.0.0.0:20809      0.0.0.0:*
`
	got := ParseListening(out)
	if !got[22] || !got[20809] {
		t.Errorf("ParseListening() = %v, want the two well-formed lines still parsed", got)
	}
	if len(got) != 2 {
		t.Errorf("ParseListening() = %v, want exactly the 2 parsable ports", got)
	}
}

func TestParseListeningEmptyOutput(t *testing.T) {
	got := ParseListening("")
	if got == nil {
		t.Fatal("ParseListening(\"\") = nil, want an empty non-nil map -- a host with nothing listening is a valid answer, not a failure")
	}
	if len(got) != 0 {
		t.Errorf("ParseListening(\"\") = %v, want empty", got)
	}
}

func TestSpecString(t *testing.T) {
	if got := (Spec{Fixed: 20809}).String(); got != "20809" {
		t.Errorf("Spec{Fixed: 20809}.String() = %q, want \"20809\"", got)
	}
	if got := (Spec{Low: 20000, High: 20100}).String(); got != "20000-20100" {
		t.Errorf("Spec{20000,20100}.String() = %q, want \"20000-20100\"", got)
	}
}
