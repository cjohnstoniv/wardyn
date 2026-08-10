// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// gitPrimaryTestWorkspace is an onboarded, scanned, GIT-sourced workspace
// carrying a required egress row, a required secret row, and an optional
// egress row — the fixture both tests below attach as a compose PRIMARY
// selection (Workspaces[0].Kind == git).
func gitPrimaryTestWorkspace() types.Workspace {
	return types.Workspace{
		ID:      uuid.New(),
		Name:    "acme-api",
		Status:  types.WorkspaceScanned,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: "acme/api"}},
		Requirements: map[string]types.WorkspaceRequirement{
			"egress:internal.acme.example": {Level: "required", Provenance: "operator_set"},
			"secret:acme-deploy-key":       {Level: "required", Provenance: "operator_set"},
			"egress:optional.acme.example": {Level: "optional", Provenance: "operator_set"},
		},
	}
}

// TestComposeWorkspaceIdentity_GitPrimaryFoldsRequiredContract is the direct
// regression test for the git-primary gap: applyWorkspaces used to put a git
// repo into spec.WorkspaceRepos only when it was NOT index 0, so a composed
// run's PRIMARY selection never resolved in referencedWorkspaces/wsRefs and its
// Required contract rows never folded. Drives the REAL compose HTTP endpoint
// (proving the wire-level fix), then runs the identical wsRefs ->
// applyWorkspaceRequirements sequence handleCreateRun runs before
// persistRunGrants (runs.go:175,185) over what compose produced — proving the
// consequence, not just the resolution.
func TestComposeWorkspaceIdentity_GitPrimaryFoldsRequiredContract(t *testing.T) {
	h := newHarness(t)
	ws := gitPrimaryTestWorkspace()
	h.srv.cfg.Store = &workspaceStoreFake{ws: ws}
	h.srv.cfg.Secrets = &memSecrets{m: map[string][]byte{"acme-deploy-key": []byte("v")}}
	// agent "claude" (not "claude-code") keeps compose's OWN LLM-grant path
	// (ensureLLMGrant) out of play, so the later api_key-grant assertion can
	// only be explained by the WORKSPACE's own required secret row — see the
	// secret-fold block below, which calls applyWorkspaceRequirements with
	// "claude-code" directly (bypassing the compose pipeline) for that reason.
	h.srv.cfg.Composer = singleBackendRegistry(t, &composer.FakeComposer{Result: composer.Proposal{
		Run:          composer.RunInput{Agent: "claude", Task: "ship the fix"},
		InlinePolicy: types.RunPolicySpec{},
		Summary:      "throwaway sandbox",
	}})

	body := `{"prompt":"ship the fix","workspace":{"kind":"git","repo":"acme/api"},"mode":"skip"}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs/compose", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("compose code = %d (want 200), body=%s", w.Code, w.Body.String())
	}
	var resp composeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Fix, on the wire: the PRIMARY git repo resolves in WorkspaceRepos, not
	// just the run.Repo scalar label.
	if len(resp.Proposed.InlinePolicy.WorkspaceRepos) != 1 || resp.Proposed.InlinePolicy.WorkspaceRepos[0].Repo != "acme/api" {
		t.Fatalf("Proposed.InlinePolicy.WorkspaceRepos = %+v, want [{Repo: acme/api}]", resp.Proposed.InlinePolicy.WorkspaceRepos)
	}
	if resp.Proposed.Run.Repo != "acme/api" {
		t.Errorf("Proposed.Run.Repo = %q, want acme/api (unchanged label)", resp.Proposed.Run.Repo)
	}

	// Consequence #1: wsRefs — the SAME resolution handleCreateRun uses — now
	// finds the git-primary workspace.
	ctx := context.Background()
	wsRefs := h.srv.referencedWorkspaces(ctx, resp.Proposed.InlinePolicy)
	if len(wsRefs) != 1 || wsRefs[0].ID != ws.ID {
		t.Fatalf("referencedWorkspaces = %+v, want exactly [%s]", wsRefs, ws.ID)
	}

	// Consequence #2: the required EGRESS row folds into a copy of the composed
	// spec (mirrors runs.go's unconditional applyWorkspaceRequirements call).
	egressSpec := resp.Proposed.InlinePolicy
	h.srv.applyWorkspaceRequirements(ctx, &egressSpec, "claude", wsRefs, nil)
	if !slices.Contains(egressSpec.AllowedDomains, "internal.acme.example") {
		t.Errorf("AllowedDomains = %v, want internal.acme.example folded in (required)", egressSpec.AllowedDomains)
	}

	// Consequence #3: the required SECRET row folds too, over the SAME wsRefs —
	// isolated to a fresh spec + "claude-code" so ensureLLMGrant (which never ran
	// above, agent was "claude") cannot be mistaken for this workspace-driven grant.
	// The fixture ALSO carries the required egress row, so this fold produces
	// that event too — the assertion below looks for the secret event
	// specifically rather than assuming it is the only one.
	secretSpec := types.RunPolicySpec{}
	events := h.srv.applyWorkspaceRequirements(ctx, &secretSpec, "claude-code", wsRefs, nil)
	if len(secretSpec.EligibleGrants) != 1 || secretSpec.EligibleGrants[0].Kind != types.GrantAPIKey {
		t.Fatalf("EligibleGrants = %+v, want exactly one api_key grant for the required secret", secretSpec.EligibleGrants)
	}
	if !slices.ContainsFunc(events, func(e requirementAuditEntry) bool {
		return e.action == "run.workspace.requirement.secret" && e.target == "acme-deploy-key"
	}) {
		t.Errorf("fold events = %+v, want a run.workspace.requirement.secret entry for acme-deploy-key", events)
	}
}

// TestComposeWorkspaceIdentity_OptionalSelectionEchoedAndFolds is the second
// required Stage-2 proof: an Optional contract row enabled via the compose
// request's new workspace_selections reaches the spec. It also pins the echo
// contract (composeRequest.WorkspaceSelections -> composeProposed.
// WorkspaceSelections) approveLaunch depends on to forward the operator's
// opt-in to POST /runs without re-deriving it from separate UI state.
func TestComposeWorkspaceIdentity_OptionalSelectionEchoedAndFolds(t *testing.T) {
	h := newHarness(t)
	ws := gitPrimaryTestWorkspace()
	h.srv.cfg.Store = &workspaceStoreFake{ws: ws}
	h.srv.cfg.Composer = singleBackendRegistry(t, &composer.FakeComposer{Result: composer.Proposal{
		Run:          composer.RunInput{Agent: "claude", Task: "ship the fix"},
		InlinePolicy: types.RunPolicySpec{},
		Summary:      "throwaway sandbox",
	}})

	body := fmt.Sprintf(
		`{"prompt":"ship the fix","workspace":{"kind":"git","repo":"acme/api"},"mode":"skip",`+
			`"workspace_selections":[{"workspace_id":%q,"enabled_optional":["egress:optional.acme.example"]}]}`,
		ws.ID.String())
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs/compose", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("compose code = %d (want 200), body=%s", w.Code, w.Body.String())
	}
	var resp composeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// The proposal echoes the selection back verbatim.
	sel := resp.Proposed.WorkspaceSelections
	if len(sel) != 1 || sel[0].WorkspaceID != ws.ID.String() ||
		len(sel[0].EnabledOptional) != 1 || sel[0].EnabledOptional[0] != "egress:optional.acme.example" {
		t.Fatalf("Proposed.WorkspaceSelections = %+v, want the sent selection echoed verbatim", sel)
	}

	// approveLaunch's exact next move: forward the echoed selection as
	// CreateRunRequest.Workspaces and resolve it exactly as handleCreateRun does.
	ctx := context.Background()
	wsRefs := h.srv.referencedWorkspaces(ctx, resp.Proposed.InlinePolicy)
	createReq := createRunRequest{Agent: "claude", Workspaces: sel}
	enabledSpec := resp.Proposed.InlinePolicy
	h.srv.applyWorkspaceRequirements(ctx, &enabledSpec, "claude", wsRefs, resolveWorkspaceSelections(createReq))
	if !slices.Contains(enabledSpec.AllowedDomains, "optional.acme.example") {
		t.Errorf("AllowedDomains = %v, want optional.acme.example folded in (enabled via the echoed selection)", enabledSpec.AllowedDomains)
	}

	// Negative control: the IDENTICAL optional row, with NO selection (today's
	// pre-fix approveLaunch sent none) must NOT fold — isolating enabled_optional,
	// not mere wsRefs resolution, as what made the difference above.
	bareSpec := resp.Proposed.InlinePolicy
	h.srv.applyWorkspaceRequirements(ctx, &bareSpec, "claude", wsRefs, nil)
	if slices.Contains(bareSpec.AllowedDomains, "optional.acme.example") {
		t.Error("optional egress folded in with NO selection — the negative control is broken")
	}
}
