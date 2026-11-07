// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package gatewayidentity

import (
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// claimsOf decodes the token's body without verifying it: what a gateway rule reads.
func claimsOf(t *testing.T, token string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts", len(parts))
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// The agent a portal named rides under the run: act is the Wardyn run, act.act the agent that asked
// for it (RFC 8693 section 4.1: the outermost act is the current actor). Sign sets the run's own
// subject and keeps the caller's prior actor.
func TestSignKeepsThePriorActor(t *testing.T) {
	s := testSigner(t, testRotate(t, nil, false), testRotatedAt)
	c := testClaims()
	c.Delegate = "11111111-2222-3333-4444-555555555555"
	c.NamePriorActor("agent-sub", "https://idp.example")
	token, signed, err := s.Sign(c, 0)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	m := claimsOf(t, token)
	act, _ := m["act"].(map[string]any)
	if act["sub"] != "spiffe://wardyn.local/agent-run/"+c.Run {
		t.Errorf("act.sub = %v, want the run's SPIFFE id", act["sub"])
	}
	inner, _ := act["act"].(map[string]any)
	if inner["sub"] != "agent-sub" || inner["iss"] != "https://idp.example" || len(inner) != 2 {
		t.Errorf("act.act = %v, want sub and iss of the agent alone", inner)
	}
	if m["wardyn_delegate"] != c.Delegate {
		t.Errorf("wardyn_delegate = %v, want %s", m["wardyn_delegate"], c.Delegate)
	}
	if signed.Actor.Act == nil || signed.Actor.Act.Sub != "agent-sub" {
		t.Errorf("the returned claims lost the prior actor: %+v", signed.Actor)
	}
}

// A portal launch that named no agent gets the portal claim and today's flat act.
func TestSignDelegateWithoutActor(t *testing.T) {
	s := testSigner(t, testRotate(t, nil, false), testRotatedAt)
	c := testClaims()
	c.Delegate = "11111111-2222-3333-4444-555555555555"
	token, _, err := s.Sign(c, 0)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	m := claimsOf(t, token)
	if m["wardyn_delegate"] != c.Delegate {
		t.Errorf("wardyn_delegate = %v", m["wardyn_delegate"])
	}
	if act, _ := m["act"].(map[string]any); len(act) != 1 || act["sub"] == nil {
		t.Errorf("act = %v, want the run's sub alone", m["act"])
	}
}

// A token for a run nobody delegated is exactly what it was: the same keys, no portal claim, and an
// act of one key.
func TestSignWithoutActorUnchanged(t *testing.T) {
	want := []string{"act", "aud", "exp", "iat", "iss", "jti", "sub", "wardyn_format", "wardyn_provider", "wardyn_run", "wardyn_runner"}
	s := testSigner(t, testRotate(t, nil, false), testRotatedAt)
	token, _, err := s.Sign(testClaims(), 0)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	m := claimsOf(t, token)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, want) {
		t.Errorf("claim keys = %v, want %v", keys, want)
	}
	if _, has := m["wardyn_delegate"]; has {
		t.Error("a non-delegated token carries wardyn_delegate")
	}
	if act, _ := m["act"].(map[string]any); len(act) != 1 {
		t.Errorf("act = %v, want the run's sub alone", m["act"])
	}
}

// A caller cannot overwrite the run's own subject in act: Sign sets it from the issuer and run.
func TestSignActorSubIsAlwaysTheRun(t *testing.T) {
	s := testSigner(t, testRotate(t, nil, false), testRotatedAt)
	c := testClaims()
	c.Actor.Sub = "spiffe://evil/agent-run/other"
	c.NamePriorActor("agent-sub", "")
	token, _, err := s.Sign(c, 0)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	act, _ := claimsOf(t, token)["act"].(map[string]any)
	if act["sub"] != "spiffe://wardyn.local/agent-run/"+c.Run {
		t.Errorf("act.sub = %v, want the run's SPIFFE id", act["sub"])
	}
}
