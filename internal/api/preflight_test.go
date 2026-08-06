// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
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
	// A migrated container-kind workspace is now an ephemeral source + a custom
	// BaseImage (0029). seedRequestWorkspace seeds req.Image from BaseImage (no
	// refusal by itself), but the SAME validateImageBuildRequest gate
	// handlePreflightRun runs immediately after still refuses it because no
	// ImageBuilder is wired here — so the end-to-end 400 survives (see the
	// identical restructuring in TestSeedRequestWorkspace/runs_workspace_id_test.go).
	h.srv.cfg.Store = &workspaceStoreFake{
		Store: h.srv.cfg.Store,
		ws: types.Workspace{
			ID:        wsID,
			Sources:   []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/work"}},
			BaseImage: &types.WorkspaceBaseImage{Kind: "custom", Image: "ghcr.io/acme/base:1"},
		},
	}
	body := `{"agent":"claude-code","repo":"ephemeral","workspace_id":"` + wsID.String() + `",` +
		`"inline_policy":{"min_confinement_class":"CC1"}}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs/preflight", adminToken, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("preflight with a container-shaped workspace_id (no image builder wired): code=%d, want 400 (same refusal as launch); body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "no image builder wired") {
		t.Errorf("error must carry the builder-gate refusal; body=%s", w.Body.String())
	}

	// The other half of parity: a workspace launch WOULD accept must surface on
	// the checklist — the seeded mount resolves back through referencedWorkspaces
	// to a "workspace" setup row, exactly what the pre-fix code silently dropped.
	h.srv.cfg.Store = &workspaceStoreFake{
		Store: h.srv.cfg.Store,
		ws: types.Workspace{
			ID:      wsID,
			Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: "/srv/app"}},
			Status:  types.WorkspaceScanned,
		},
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

// preflightIntegrationStore serves GetSiteConfig for resolveIntegrationRef
// (the eager integration_id check plus foldRunIntegration's tier 1) from a
// seeded stored Integration, and ListWorkspaces=none — referencedWorkspaces
// (workspace_run.go) calls it unconditionally whenever a Store is configured
// at all, which the embedded nil store.Store does not implement.
type preflightIntegrationStore struct {
	store.Store
	cfg types.SiteConfig
}

func (s preflightIntegrationStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return s.cfg, nil
}
func (preflightIntegrationStore) ListWorkspaces(context.Context) ([]types.Workspace, error) {
	return nil, nil
}

// TestPreflight_ModelAccessFromExplicitIntegrationID is the audit-row-55 test:
// preflight must fold the WHOLE run-level integration precedence chain
// (foldRunIntegration), not just the workspace tier. A run naming an explicit
// integration_id and NO workspace at all must still see model access
// satisfied on the checklist — proving the fold reaches tier 1, not only the
// workspace-ref tier the pre-fix code was limited to.
func TestPreflight_ModelAccessFromExplicitIntegrationID(t *testing.T) {
	h := newHarness(t)
	st := preflightIntegrationStore{cfg: types.SiteConfig{Integrations: []types.Integration{
		{ID: "acme-anthropic", Category: types.IntegrationAIProvider, Type: "anthropic_api_key",
			Credentials: map[string]string{"api_key": "acme-anthropic-key"}},
	}}}
	cfg := baseTestConfig(h, st)
	cfg.Secrets = &memSecrets{m: map[string][]byte{"acme-anthropic-key": []byte("sk-acme")}}
	cfg.DefaultPolicy = types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}, MinConfinementClass: types.CC2}
	srv := New(cfg)

	body := `{"agent":"claude-code","repo":"ephemeral","integration_id":"acme-anthropic","inline_policy":{"min_confinement_class":"CC2"}}`
	w := do(t, srv, http.MethodPost, "/api/v1/runs/preflight", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("preflight code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp preflightResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, w.Body.String())
	}
	if it, ok := findItem(resp.SetupItems, "llm_access:claude-code"); !ok || it.Status != "satisfied" {
		t.Errorf("llm_access row = %+v (ok=%v), want satisfied (model access from the explicit integration_id, no workspace involved)", it, ok)
	}
}

// TestPreflight_IntegrationID_NonAIProviderIs400 pins the shared rule launch
// applies (decodeAndValidateCreateRun, runs_create.go) and compose replicates
// (handleComposeRun, compose.go): a typo or a non-ai_provider integration_id
// 400s here too, instead of previewing as "no model access" and only 400ing
// for real once the operator clicks launch.
func TestPreflight_IntegrationID_NonAIProviderIs400(t *testing.T) {
	h := newHarness(t)
	st := preflightIntegrationStore{cfg: types.SiteConfig{Integrations: []types.Integration{
		{ID: "acme-scm", Category: types.IntegrationSCMHost, Type: "git_host"},
	}}}
	srv := New(baseTestConfig(h, st))

	body := `{"agent":"claude-code","repo":"ephemeral","integration_id":"acme-scm","inline_policy":{"min_confinement_class":"CC2"}}`
	w := do(t, srv, http.MethodPost, "/api/v1/runs/preflight", adminToken, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("preflight with a non-ai_provider integration_id: code = %d, want 400; body=%s", w.Code, w.Body.String())
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
