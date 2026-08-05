// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
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

// TestPreflight_MemberInlineClampSurfacesWarnings is the L6 review fix's
// dedicated coverage: composer.Clamp's notices are DISCARDED at launch (see
// resolveRunPolicy's doc comment — "launch may stay silent") but SURFACED
// here, so Review can tell a member WHY their inline_policy differs from what
// they typed, before they launch. Same scenario TestCreateRun_MemberInlineClamped
// pins for the audit trail (CC1 clamped up to the operator's CC2 ceiling), on
// the preflight path this time — an admin's identical request is unclamped,
// so it carries no warnings at all.
func TestPreflight_MemberInlineClampSurfacesWarnings(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.OIDC = &oidc.Authenticator{}
	h.srv.cfg.DefaultPolicy = types.RunPolicySpec{MinConfinementClass: types.CC2, AllowedDomains: []string{"api.anthropic.com"}}
	h.srv.router = h.srv.routes()

	const body = `{"agent":"claude-code","repo":"acme/widgets","inline_policy":{"min_confinement_class":"CC1"}}`

	member := ssoSession(t, "sub-member", "member@corp.example", oidc.RoleMember)
	w := doSSO(t, h.srv, http.MethodPost, "/api/v1/runs/preflight", member, body)
	if w.Code != http.StatusOK {
		t.Fatalf("member preflight: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var memberResp preflightResponse
	if err := json.Unmarshal(w.Body.Bytes(), &memberResp); err != nil {
		t.Fatalf("decode member response: %v; body=%s", err, w.Body.String())
	}
	found := false
	for _, msg := range memberResp.Warnings {
		if strings.Contains(msg, "confinement raised") {
			found = true
		}
	}
	if !found {
		t.Errorf("member: warnings = %v, want a confinement-raise clamp note", memberResp.Warnings)
	}

	admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)
	w = doSSO(t, h.srv, http.MethodPost, "/api/v1/runs/preflight", admin, body)
	if w.Code != http.StatusOK {
		t.Fatalf("admin preflight: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var adminResp preflightResponse
	if err := json.Unmarshal(w.Body.Bytes(), &adminResp); err != nil {
		t.Fatalf("decode admin response: %v; body=%s", err, w.Body.String())
	}
	if len(adminResp.Warnings) != 0 {
		t.Errorf("admin: warnings = %v, want empty (admin is unclamped)", adminResp.Warnings)
	}
}
