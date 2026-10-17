// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestNodeRecordAttestationRules pins the organisation's half of the node record rules (design K1 §3.1
// (b), owner #8): after a recorded name reuse only "node powered off" is taken, and "no prior holder" is
// for the no-association path only.
func TestNodeRecordAttestationRules(t *testing.T) {
	for _, c := range []struct {
		name        string
		attestation string
		held        store.RunContainmentPendingRow
		refused     string
	}{
		{"powered off on a causal run", types.NodeRecordPoweredOff, store.RunContainmentPendingRow{}, ""},
		{"powered off after a reuse", types.NodeRecordPoweredOff, store.RunContainmentPendingRow{NameReused: true}, ""},
		{"no prior holder on the no-association path", types.NodeRecordNoPriorHolder, store.RunContainmentPendingRow{NoCausalAssociation: true}, ""},
		{"no prior holder after a reuse", types.NodeRecordNoPriorHolder, store.RunContainmentPendingRow{NameReused: true, NoCausalAssociation: true}, "node name was reused"},
		{"no prior holder on a causal run", types.NodeRecordNoPriorHolder, store.RunContainmentPendingRow{}, "only for a run with no causal association"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := nodeRecordAttestationRefusal(c.attestation, c.held)
			if (c.refused == "") != (got == "") || !strings.Contains(got, c.refused) {
				t.Fatalf("refusal = %q, want one containing %q", got, c.refused)
			}
		})
	}
}

// TestNodeRecordNeedsTwoPeople: a recorded reuse and the no-association path always need two people,
// whatever the four-eyes setting; a causal run needs two only under four-eyes.
func TestNodeRecordNeedsTwoPeople(t *testing.T) {
	t.Setenv(envGovernanceSecondHuman, "")
	for name, c := range map[string]struct {
		held store.RunContainmentPendingRow
		want bool
	}{
		"causal":         {store.RunContainmentPendingRow{}, false},
		"name reused":    {store.RunContainmentPendingRow{NameReused: true}, true},
		"no association": {store.RunContainmentPendingRow{NoCausalAssociation: true}, true},
	} {
		if got := nodeRecordNeedsTwo(c.held); got != c.want {
			t.Errorf("%s: needs two = %v, want %v", name, got, c.want)
		}
	}
	t.Setenv(envGovernanceSecondHuman, "true")
	if !nodeRecordNeedsTwo(store.RunContainmentPendingRow{}) {
		t.Error("a causal run's record under four-eyes needs one person")
	}
}

// TestNodeRecordApproverIsPseudonymous: the runner tells people apart by case-folded principal and email,
// and the action row that carries the record holds neither in the clear.
func TestNodeRecordApproverIsPseudonymous(t *testing.T) {
	a, b := nodeRecordApprover("Sub-Alice", "Alice@Corp.Example"), nodeRecordApprover(" sub-alice", "alice@corp.example ")
	if a != b || !a.Human || !strings.HasPrefix(a.Principal, "sha256:") || strings.Contains(a.Principal+a.Email, "alice") {
		t.Fatalf("approvers %+v and %+v", a, b)
	}
	if nodeRecordApprover("sub-alice", "").Email != "" {
		t.Fatal("an absent email was filled in")
	}
}

// TestRelayIPFencedIsItsOwnFailure: a runner's relay_ip_fenced refusal ends the run with its own reason,
// and nothing else carries it.
func TestRelayIPFencedIsItsOwnFailure(t *testing.T) {
	fenced := &runnerwire.Error{Code: runnerwire.CodeInternal, Message: "k8s: proxy pod 10.244.1.7: relay_ip_fenced: the proxy pod's IP is still fenced for another run whose termination is unknown"}
	if hint, reason := sandboxCreateFailure(fenced); reason != reasonRelayIPFenced || !strings.Contains(hint, "still held for an earlier run") {
		t.Fatalf("fenced = %q %q", hint, reason)
	}
	for _, err := range []error{
		&runnerwire.Error{Code: runnerwire.CodeRefused, Message: "relay_ip_fenced: x"},
		&runnerwire.Error{Code: runnerwire.CodeInternal, Message: "pod relay_ip_fenced without its colon"},
		errors.New("relay_ip_fenced: not from the runner"),
	} {
		if _, reason := sandboxCreateFailure(err); reason == reasonRelayIPFenced {
			t.Errorf("%v was read as relay_ip_fenced", err)
		}
	}
}
