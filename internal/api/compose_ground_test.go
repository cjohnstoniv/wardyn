// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestComposeWorkspaceGitRepos proves the (github, other) split composeWorkspaceGitRepos
// feeds into groundGitHubGrants: a bare slug and a github.com URL both resolve
// to a CASE-PRESERVED "owner/repo", a non-GitHub URL falls to "other", and a
// non-git workspace selection is ignored entirely.
func TestComposeWorkspaceGitRepos(t *testing.T) {
	wss := []composer.Workspace{
		{Kind: composer.WorkspaceGit, Repo: "octocat/Hello-World"},
		{Kind: composer.WorkspaceGit, Repo: "https://github.com/Octocat/Spoon-Knife"},
		{Kind: composer.WorkspaceGit, Repo: "https://dev.azure.com/org/project/_git/repo"},
		{Kind: composer.WorkspaceLocal, Path: "/x"}, // not a git selection: ignored
	}
	github, other := composeWorkspaceGitRepos(wss)
	if len(github) != 2 || github[0] != "octocat/Hello-World" || github[1] != "Octocat/Spoon-Knife" {
		t.Errorf("github = %v, want case-preserved [octocat/Hello-World Octocat/Spoon-Knife]", github)
	}
	if len(other) != 1 || other[0] != "https://dev.azure.com/org/project/_git/repo" {
		t.Errorf("other = %v, want the ADO URL", other)
	}
}

// TestWidenCeilingRepoAllowlist_DoesNotMutateShared proves widenCeilingRepoAllowlist
// never writes into the ceiling it was handed — critical because callers pass
// s.cfg.DefaultPolicy, the process-global default every run resolves against.
func TestWidenCeilingRepoAllowlist_DoesNotMutateShared(t *testing.T) {
	shared := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{
		{Kind: types.GrantGitHubToken, Scope: json.RawMessage(`{"repos":[],"permissions":{"contents":"read"}}`)},
	}}
	widened := widenCeilingRepoAllowlist(shared, []string{"acme/widgets"})
	if repos := ghScopeRepos(widened.EligibleGrants[0].Scope); len(repos) != 1 || repos[0] != "acme/widgets" {
		t.Fatalf("widened repos = %v, want [acme/widgets]", repos)
	}
	if repos := ghScopeRepos(shared.EligibleGrants[0].Scope); len(repos) != 0 {
		t.Errorf("widenCeilingRepoAllowlist mutated the shared ceiling in place: %v", repos)
	}
	// A ceiling that already restricts explicitly is left alone (not widened
	// past what the operator actually set).
	restricted := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{
		{Kind: types.GrantGitHubToken, Scope: json.RawMessage(`{"repos":["acme/only-this"],"permissions":{"contents":"read"}}`)},
	}}
	got := widenCeilingRepoAllowlist(restricted, []string{"acme/widgets"})
	if repos := ghScopeRepos(got.EligibleGrants[0].Scope); len(repos) != 1 || repos[0] != "acme/only-this" {
		t.Errorf("an explicit ceiling repo list must not be widened, got %v", repos)
	}
}

// TestComposeRun_WorkspaceGitGroundsGitHubGrant is W15-b: a composed run whose
// workspace is an EXPLICIT git pick (composer.WorkspaceGit), not a local
// directory Wardyn can `git remote -v` inspect, never ran grounding at all —
// the analyzer's OWN repo guess for a github_token grant survived all the way
// to composer.Clamp, whose any-repo-when-ceiling-empty rule assumed every
// proposal reaching it was already grounded to reality (W23-S1-3 tightens
// that rule to deny-all for anything that isn't). The fix folds the
// operator's own workspace SELECTION into the same grounding
// groundGitHubGrants already applies to a detected local remote, and widens
// the pipeline's own ceiling copy to match (widenCeilingRepoAllowlist) so the
// now-strict Clamp doesn't strip the very grounding this proves legitimate.
func TestComposeRun_WorkspaceGitGroundsGitHubGrant(t *testing.T) {
	h := newHarness(t)
	// default.json's own shape: a github_token TEMPLATE with no repo allowlist.
	h.srv.cfg.DefaultPolicy.EligibleGrants = []types.GrantSpec{{Kind: types.GrantGitHubToken}}
	// The onboarding gate (validateWorkspaceSources) requires the selected repo
	// to be a pre-onboarded workspace; composeIntegrationStore (compose_integration_id_test.go)
	// already wires ListWorkspaces for exactly this purpose.
	h.srv.cfg.Store = &composeIntegrationStore{workspaces: []types.Workspace{
		{ID: uuid.New(), Name: "real-repo", Sources: []types.WorkspaceSource{
			{Type: types.WorkspaceSourceTypeRepo, Source: "real-org/real-repo"},
		}},
	}}
	h.srv.cfg.Composer = singleBackendRegistry(t, &composer.FakeComposer{Result: composer.Proposal{
		Run: composer.RunInput{Agent: "claude-code", Task: "fix a bug"},
		InlinePolicy: types.RunPolicySpec{
			AllowedDomains: []string{"api.anthropic.com"},
			EligibleGrants: []types.GrantSpec{
				// The analyzer's own (hallucinated) guess — must NOT survive.
				{Kind: types.GrantGitHubToken, Scope: json.RawMessage(
					`{"repos":["hallucinated-org/wrong-repo"],"permissions":{"contents":"read"}}`)},
			},
		},
		Summary: "fix",
	}})

	body := `{"prompt":"fix a bug","workspace":{"kind":"git","repo":"real-org/real-repo"},"mode":"skip"}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs/compose", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("compose code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp composeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var gh *types.GrantSpec
	for i, g := range resp.Proposed.InlinePolicy.EligibleGrants {
		if g.Kind == types.GrantGitHubToken {
			gh = &resp.Proposed.InlinePolicy.EligibleGrants[i]
		}
	}
	if gh == nil {
		t.Fatalf("expected a surviving github_token grant, got %+v", resp.Proposed.InlinePolicy.EligibleGrants)
	}
	repos := ghScopeRepos(gh.Scope)
	if len(repos) != 1 || repos[0] != "real-org/real-repo" {
		t.Errorf("W15-b: github_token repos = %v, want grounded to the SELECTED workspace repo [real-org/real-repo], not the analyzer's guess", repos)
	}
}
