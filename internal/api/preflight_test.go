// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPreflight_HappyPath drives POST /runs/preflight through the HTTP path with
// an inline policy that fully credentials a claude-code run: the response is 200,
// carries the deterministic setup checklist, and reports the enforced confinement
// class. The llm_access and secret rows are satisfied (anthropic api_key grant +
// stored secret + matching egress) — proving the endpoint reuses the SAME
// reconcileLLMAccess/deriveSetupItems verdict the compose Review panel shows.
func TestPreflight_HappyPath(t *testing.T) {
	h, _ := newSecretsHarness(t) // memSecrets seeded with "anthropic-api-key"
	body := `{"agent":"claude-code","repo":"ephemeral","inline_policy":{` +
		`"min_confinement_class":"CC2","allowed_domains":["api.anthropic.com"],` +
		`"eligible_grants":[{"kind":"api_key","scope":{"host":"api.anthropic.com",` +
		`"header":"x-api-key","format":"%s","secret_name":"anthropic-api-key"}}]}}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs/preflight", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("preflight happy path: code=%d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp preflightResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, w.Body.String())
	}
	if resp.EnforcedConfinementClass != types.CC2 {
		t.Errorf("enforced_confinement_class = %q, want CC2", resp.EnforcedConfinementClass)
	}
	if len(resp.SetupItems) == 0 {
		t.Fatal("expected a non-empty setup checklist")
	}
	if it, ok := findItem(resp.SetupItems, "llm_access:claude-code"); !ok || it.Status != "satisfied" {
		t.Errorf("llm_access row = %+v (ok=%v), want satisfied", it, ok)
	}
	if it, ok := findItem(resp.SetupItems, "secret:anthropic-api-key"); !ok || it.Status != "satisfied" {
		t.Errorf("secret row = %+v (ok=%v), want satisfied", it, ok)
	}
}

// TestPreflight_BlastRadiusRaisesToCC3 asserts the enforced class mirrors
// handleCreateRun's deterministic blast-radius floor: a write-capable github_token
// grant raises the run to Vault (CC3) even though the operator requested CC2 and
// the policy floor is CC2. This is the exact fact the wizard's inline "raised
// automatically because this run holds write-capable credentials" line renders.
func TestPreflight_BlastRadiusRaisesToCC3(t *testing.T) {
	h, _ := newSecretsHarness(t)
	body := `{"agent":"claude-code","repo":"octocat/Hello-World","confinement_class":"CC2","inline_policy":{` +
		`"min_confinement_class":"CC2","allowed_domains":["api.anthropic.com"],` +
		`"eligible_grants":[{"kind":"github_token","scope":{"repos":["octocat/Hello-World"],` +
		`"permissions":{"contents":"write"}}}]}}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs/preflight", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("preflight CC3 raise: code=%d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp preflightResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, w.Body.String())
	}
	if resp.EnforcedConfinementClass != types.CC3 {
		t.Errorf("enforced_confinement_class = %q, want CC3 (write-capable grant raise)", resp.EnforcedConfinementClass)
	}
}

// TestPreflight_UnknownSecret422Passthrough asserts preflight surfaces the REAL
// launch error: an inline api_key grant naming a secret that is not stored 422s
// through the SAME resolveRunPolicy chokepoint handleCreateRun uses — so Review
// never reports cleaner than launch behaves.
func TestPreflight_UnknownSecret422Passthrough(t *testing.T) {
	h, _ := newSecretsHarness(t)
	body := `{"agent":"claude-code","repo":"ephemeral","inline_policy":{` +
		`"min_confinement_class":"CC2","eligible_grants":[{"kind":"api_key",` +
		`"scope":{"host":"api.example.com","secret_name":"nope-not-here"}}]}}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs/preflight", adminToken, body)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("preflight unknown secret: code=%d, want 422; body=%s", w.Code, w.Body.String())
	}
}
