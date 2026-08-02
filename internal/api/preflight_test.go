// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

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

// TestPreflight_WorkspaceIDSeeded pins launch parity for --workspace: preflight
// must run the SAME seedRequestWorkspace launch runs, so a workspace_id that
// launch would refuse (here: a container-kind workspace) fails the dry run with
// the identical error instead of passing a rosier preflight (the false-green
// this endpoint exists to prevent).
func TestPreflight_WorkspaceIDSeeded(t *testing.T) {
	h := newHarness(t)
	wsID := uuid.New()
	h.srv.cfg.Store = &workspaceStoreFake{
		Store: h.srv.cfg.Store,
		ws:    types.Workspace{ID: wsID, Kind: types.WorkspaceKindContainer, Source: "ghcr.io/acme/base:1"},
	}
	body := `{"agent":"claude-code","repo":"ephemeral","workspace_id":"` + wsID.String() + `",` +
		`"inline_policy":{"min_confinement_class":"CC1"}}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs/preflight", adminToken, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("preflight with a container workspace_id: code=%d, want 400 (same refusal as launch); body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "workspace_id:") {
		t.Errorf("error must carry the launch path's workspace_id prefix; body=%s", w.Body.String())
	}

	// The other half of parity: a workspace launch WOULD accept must surface on
	// the checklist — the seeded mount resolves back through referencedWorkspaces
	// to a "workspace" setup row, exactly what the pre-fix code silently dropped.
	h.srv.cfg.Store = &workspaceStoreFake{
		Store: h.srv.cfg.Store,
		ws:    types.Workspace{ID: wsID, Kind: types.WorkspaceKindLocalDir, Source: "/srv/app", Status: types.WorkspaceReady},
	}
	body = `{"agent":"claude-code","workspace_id":"` + wsID.String() + `",` +
		`"inline_policy":{"min_confinement_class":"CC1"}}`
	w = do(t, h.srv, http.MethodPost, "/api/v1/runs/preflight", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("preflight with an onboarded local_dir workspace: code=%d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp preflightResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if _, ok := findItem(resp.SetupItems, "workspace:"+wsID.String()); !ok {
		t.Errorf("checklist must carry the seeded workspace's row; items=%+v", resp.SetupItems)
	}
}

// TestPreflight_WorkspaceAPIKeyCredFold pins the credential half of launch
// parity: a workspace bound to an api_key secret with a NON-convention name
// (the whole point of per-workspace bindings) must yield a SATISFIED
// llm_access row naming that secret — not the false "add anthropic-api-key"
// red the pre-fix verdict produced by keying on the provider convention name.
func TestPreflight_WorkspaceAPIKeyCredFold(t *testing.T) {
	h, sec := newSecretsHarness(t)
	delete(sec.m, "anthropic-api-key")
	sec.m["acme-anthropic-key"] = []byte("sk-ant-workspace")
	wsID := uuid.New()
	h.srv.cfg.Store = &workspaceStoreFake{
		Store: h.srv.cfg.Store,
		ws: types.Workspace{
			ID: wsID, Kind: types.WorkspaceKindLocalDir, Source: "/srv/app", Status: types.WorkspaceReady,
			LLMCred: &types.WorkspaceLLMCred{Mode: types.WorkspaceLLMCredAPIKey, APIKeySecret: "acme-anthropic-key"},
		},
	}
	body := `{"agent":"claude-code","workspace_id":"` + wsID.String() + `",` +
		`"inline_policy":{"min_confinement_class":"CC1"}}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs/preflight", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp preflightResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	it, ok := findItem(resp.SetupItems, "llm_access:claude-code")
	if !ok || it.Status != "satisfied" {
		t.Fatalf("llm_access row = %+v (ok=%v), want satisfied via the workspace-bound secret", it, ok)
	}
	if !strings.Contains(it.Detail, "acme-anthropic-key") {
		t.Errorf("detail must name the workspace's own secret, got %q", it.Detail)
	}

	// Inverse: the bound secret absent -> missing. applyWorkspaceCreds folds no
	// grant for an absent secret (same at launch), so the CTA falls back to the
	// convention name a composed run would use — the honest no-binding message.
	delete(sec.m, "acme-anthropic-key")
	w = do(t, h.srv, http.MethodPost, "/api/v1/runs/preflight", adminToken, body)
	var resp2 preflightResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	it2, ok2 := findItem(resp2.SetupItems, "llm_access:claude-code")
	if !ok2 || it2.Status == "satisfied" {
		t.Fatalf("llm_access row = %+v (ok=%v), want missing when the bound secret is absent", it2, ok2)
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
