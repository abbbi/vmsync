package main

import (
	"strings"
	"testing"
)

// A delta must never land on a replica whose own metadata says a full copy into
// it did not finish.
//
// The reachable route, and the reason this is a refusal rather than a warning:
// a full sync writes no pending_checkpoint, on the reasoning that a failed full
// sync is followed by another full sync regardless. That holds only while
// checkpoint names do not repeat, and they restart at vmsync-cpt-000001 after a
// chain drop (CI-04). So a full sync that armed the marker, created a fresh
// vmsync-cpt-000001 and died mid-copy leaves a target whose last_checkpoint is
// the PREVIOUS generation's vmsync-cpt-000001 -- equal to the next run's
// parent, so checkpointChainConsistent passes and the delta lands on a
// half-written image. The run then succeeds and clears the marker, taking the
// evidence with it.
func TestAnIncrementalIsRefusedOntoAnIncompleteReplica(t *testing.T) {
	for _, tc := range []struct {
		name   string
		marker string
		refuse bool
	}{
		{"no marker: the ordinary case", "", false},
		{"whitespace only, which is how an emptied field reads", "   \n", false},
		{"a full sync did not finish", "full-sync:hyper01p:1799999999", true},
		// Presence is the finding, so a value this build cannot parse still
		// refuses -- the same policy pkg/failover and the displaced-set sweep
		// apply to this field.
		{"a value this build cannot read", "something-from-a-later-version", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := incompleteReplicaRefusal("web01", tc.marker)
			if tc.refuse && err == nil {
				t.Fatal("an incremental was permitted onto a replica marked incomplete, which lands a delta on a half-written image and then reports success")
			}
			if !tc.refuse && err != nil {
				t.Fatalf("a healthy replica was refused: %v", err)
			}
			if err == nil {
				return
			}
			msg := err.Error()
			for _, want := range []string{"web01", tc.marker, "replica_incomplete", "-reinit"} {
				if !strings.Contains(msg, want) {
					t.Errorf("the refusal does not mention %q, and an operator has to be able to see which replica, what it carries and what to do: %s", want, msg)
				}
			}
		})
	}
}
