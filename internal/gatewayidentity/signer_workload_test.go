// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package gatewayidentity

import (
	"context"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/identity/embedded"
)

// A workload's run is named by its layered SPIFFE ID in act.sub and by the flat wardyn_workload claim;
// any other run keeps act.sub as it was and carries no wardyn_workload. A prior actor a portal named
// stays nested under the run, and a caller's own act.sub is never signed.
func TestSignNamesAWorkloadsRun(t *testing.T) {
	s := testSigner(t, testRotate(t, nil, false), testRotatedAt)
	run := testClaims().Run
	for _, tc := range []struct {
		name, workload, wantSub string
		prior                   bool
	}{
		{"an ad-hoc run", "", "spiffe://wardyn.local/agent-run/" + run, false},
		{"a workload's run", "service:ci-bot/nightly", "spiffe://wardyn.local/workload/service_3Aci-bot/nightly/run/" + run, false},
		{"an owner holding a slash", "auth0|ab/cd/web", "spiffe://wardyn.local/workload/auth0_7Cab_2Fcd/web/run/" + run, false},
		{"a workload's run and a prior actor", "service:ci-bot/nightly", "spiffe://wardyn.local/workload/service_3Aci-bot/nightly/run/" + run, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClaims()
			c.Workload = tc.workload
			c.Actor.Sub = "spiffe://evil/agent-run/other"
			want := map[string]any{"sub": tc.wantSub}
			if tc.prior {
				c.NamePriorActor("agent-sub", "https://idp.example")
				want["act"] = map[string]any{"sub": "agent-sub", "iss": "https://idp.example"}
			}
			token, _, err := s.Sign(c, 0)
			if err != nil {
				t.Fatalf("Sign: %v", err)
			}
			m := claimsOf(t, token)
			if !reflect.DeepEqual(m["act"], want) {
				t.Errorf("act = %v, want %v", m["act"], want)
			}
			if v, has := m["wardyn_workload"]; tc.workload == "" && has || tc.workload != "" && v != tc.workload {
				t.Errorf("wardyn_workload = %v (present %v), want %q", v, has, tc.workload)
			}
		})
	}
}

// A workload that is not "<owner>/<name>" with a workload name is refused, never signed under another
// act.sub.
func TestSignRefusesAMalformedWorkload(t *testing.T) {
	s := testSigner(t, testRotate(t, nil, false), testRotatedAt)
	for _, workload := range []string{"nightly", "service:ci-bot/", "/nightly", "service:ci-bot/Nightly"} {
		c := testClaims()
		c.Workload = workload
		if _, _, err := s.Sign(c, 0); err == nil {
			t.Errorf("Sign with workload %q: signed, want a refusal", workload)
		}
	}
}

// act.sub of a workload's run is the run token's SPIFFE ID byte for byte: the embedded provider's mint
// and the signer write one string, so a gateway can hold the client certificate's URI SAN, which is
// the run token's ID, equal to act.sub.
func TestSignWorkloadActorIsTheRunTokensID(t *testing.T) {
	p, err := embedded.New(nil, "", testNoRevocations{}, testNoAudit{})
	if err != nil {
		t.Fatal(err)
	}
	w := identity.Workload{Owner: "alice@example.com", Name: "web"}
	runID := uuid.New()
	ri, err := p.MintWorkloadRunIdentity(context.Background(), runID, w, "alice@example.com", "", "wardyn-internal", false)
	if err != nil {
		t.Fatal(err)
	}
	c := testClaims()
	c.Run, c.Workload = runID.String(), w.String()
	token, _, err := testSigner(t, testRotate(t, nil, false), testRotatedAt).Sign(c, 0)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if act, _ := claimsOf(t, token)["act"].(map[string]any); act["sub"] != ri.SPIFFEID {
		t.Errorf("act.sub = %v, want the run token's %q", act["sub"], ri.SPIFFEID)
	}
}
