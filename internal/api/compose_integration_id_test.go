// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// composeIntegrationStore is fakeSiteConfigStore (site_config_test.go) plus a
// ListWorkspaces returning `workspaces` verbatim (nil by default — no
// onboarded rows): the full compose pipeline's setup-checklist stage
// (deriveSetupItems -> referencedWorkspaces) AND its model-access resolution
// stage (primaryWorkspaceLLMRef) call Store.ListWorkspaces whenever a Store
// is configured at all, which fakeSiteConfigStore's embedded nil store.Store
// does not implement.
type composeIntegrationStore struct {
	fakeSiteConfigStore
	workspaces []types.Workspace
}

func (s composeIntegrationStore) ListWorkspaces(context.Context) ([]types.Workspace, error) {
	return s.workspaces, nil
}

// TestComposeRun_IntegrationID_UsesNamedAPIKeySecret proves compose's new
// integration_id field (Task 2) actually steers the proposed grant: an
// explicit ai_provider integration overrides ensureLLMGrant's PROVIDER-
// DEFAULT secret ("anthropic-api-key") with the pinned integration's OWN.
func TestComposeRun_IntegrationID_UsesNamedAPIKeySecret(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.Composer = singleBackendRegistry(t, &composer.FakeComposer{Result: composer.Proposal{
		Run:          composer.RunInput{Agent: "claude-code", Task: "build a small website"},
		InlinePolicy: types.RunPolicySpec{AllowedDomains: []string{"github.com"}},
		Summary:      "throwaway sandbox",
	}})
	h.srv.cfg.Store = &composeIntegrationStore{fakeSiteConfigStore: fakeSiteConfigStore{cfg: types.SiteConfig{Integrations: []types.Integration{
		apiKeyIntegration("acme-anthropic", "acme-anthropic-key"),
	}}}}
	h.srv.cfg.Secrets = &memSecrets{m: map[string][]byte{"acme-anthropic-key": []byte("sk-acme")}}
	// clampGrants (composer/clamp.go) matches an eligible grant by KIND only
	// (any scope), so the operator ceiling just needs ONE api_key template —
	// newHarness's DefaultPolicy carries none, which would otherwise drop
	// every proposed grant regardless of which secret it names.
	h.srv.cfg.DefaultPolicy.EligibleGrants = []types.GrantSpec{{Kind: types.GrantAPIKey}}

	body := `{"prompt":"build a small website","workspace":{"kind":"ephemeral"},"mode":"skip","integration_id":"acme-anthropic"}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs/compose", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("compose code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp composeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	g, ok := apiKeyGrantForHost(&resp.Proposed.InlinePolicy, "api.anthropic.com")
	if !ok {
		t.Fatalf("expected an api_key grant for api.anthropic.com; policy=%+v", resp.Proposed.InlinePolicy)
	}
	var scope map[string]string
	_ = json.Unmarshal(g.Scope, &scope)
	if scope["secret_name"] != "acme-anthropic-key" {
		t.Errorf("grant secret = %q, want the pinned integration's own secret (acme-anthropic-key)", scope["secret_name"])
	}
	if resp.LLMAccess == nil || !resp.LLMAccess.Provisioned {
		t.Errorf("LLMAccess = %+v, want provisioned=true", resp.LLMAccess)
	}
}

// TestComposeRun_PrimaryWorkspacePin_SteersProposal proves the compose-time
// live bug fix (primaryWorkspaceLLMRef, compose.go): with NO integration_id on
// the request, an ONBOARDED local_dir workspace's own LLMCred.IntegrationRef
// steers the proposed grant exactly like an explicit integration_id does above
// — a workspace pinned to a subscription now previews at Review exactly what
// foldRunIntegration already granted at Launch, instead of silently diverging.
func TestComposeRun_PrimaryWorkspacePin_SteersProposal(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.Composer = singleBackendRegistry(t, &composer.FakeComposer{Result: composer.Proposal{
		Run:          composer.RunInput{Agent: "claude-code", Task: "build a small website"},
		InlinePolicy: types.RunPolicySpec{AllowedDomains: []string{"github.com"}},
		Summary:      "throwaway sandbox",
	}})
	const wsPath = "/home/ops/acme-app"
	h.srv.cfg.Store = &composeIntegrationStore{
		fakeSiteConfigStore: fakeSiteConfigStore{cfg: types.SiteConfig{Integrations: []types.Integration{
			apiKeyIntegration("acme-anthropic", "acme-anthropic-key"),
		}}},
		workspaces: []types.Workspace{{
			Name:    "acme-app",
			Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: wsPath}},
			LLMCred: &types.WorkspaceLLMCred{IntegrationRef: "acme-anthropic"},
		}},
	}
	h.srv.cfg.Secrets = &memSecrets{m: map[string][]byte{"acme-anthropic-key": []byte("sk-acme")}}
	// Same reason as TestComposeRun_IntegrationID_UsesNamedAPIKeySecret: clampGrants
	// matches by KIND only, so the ceiling just needs ONE api_key template.
	h.srv.cfg.DefaultPolicy.EligibleGrants = []types.GrantSpec{{Kind: types.GrantAPIKey}}

	body := `{"prompt":"build a small website","workspace":{"kind":"local","path":"` + wsPath + `"},"mode":"skip"}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs/compose", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("compose code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp composeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	g, ok := apiKeyGrantForHost(&resp.Proposed.InlinePolicy, "api.anthropic.com")
	if !ok {
		t.Fatalf("expected an api_key grant for api.anthropic.com; policy=%+v", resp.Proposed.InlinePolicy)
	}
	var scope map[string]string
	_ = json.Unmarshal(g.Scope, &scope)
	if scope["secret_name"] != "acme-anthropic-key" {
		t.Errorf("grant secret = %q, want the pinned workspace's own integration secret (acme-anthropic-key)", scope["secret_name"])
	}
	if resp.LLMAccess == nil || !resp.LLMAccess.Provisioned {
		t.Errorf("LLMAccess = %+v, want provisioned=true", resp.LLMAccess)
	}
}

// TestComposeRun_IntegrationID_UsesBedrockRegionOverride proves the compose
// preview folds ANY pinned non-subscription ai_provider integration through
// applyIntegrationCreds — not just the two direct-api-key types: a pinned
// bedrock integration's region/model override reaches the proposed policy
// (the region's own Bedrock hosts, not the default Anthropic api-key grant
// ensureLLMGrant would otherwise add) and the resolved WorkspaceBedrockRef is
// threaded into the proposal instead of discarded.
func TestComposeRun_IntegrationID_UsesBedrockRegionOverride(t *testing.T) {
	const region, model = "eu-central-1", "eu.anthropic.claude-sonnet-4-5-20250929-v1:0"
	h := newHarness(t)
	h.srv.cfg.Composer = singleBackendRegistry(t, &composer.FakeComposer{Result: composer.Proposal{
		Run:          composer.RunInput{Agent: "claude-code", Task: "build a small website"},
		InlinePolicy: types.RunPolicySpec{AllowedDomains: []string{"github.com"}},
		Summary:      "throwaway sandbox",
	}})
	h.srv.cfg.Store = &composeIntegrationStore{fakeSiteConfigStore: fakeSiteConfigStore{cfg: types.SiteConfig{Integrations: []types.Integration{
		{ID: "acme-bedrock", Category: types.IntegrationAIProvider, Type: "bedrock",
			Config: mustJSON(map[string]any{"region": region, "model": model})},
	}}}}
	// The operator ceiling must bless the region's own Bedrock hosts for them
	// to survive Clamp's allow-list intersection (composer/clamp.go) — the
	// same reason TestComposeRun_IntegrationID_UsesNamedAPIKeySecret extends
	// DefaultPolicy.EligibleGrants for its own grant to survive.
	h.srv.cfg.DefaultPolicy.AllowedDomains = append(h.srv.cfg.DefaultPolicy.AllowedDomains,
		bedrockRuntimeHost(region), bedrockControlHost(region))

	body := `{"prompt":"build a small website","workspace":{"kind":"ephemeral"},"mode":"skip","integration_id":"acme-bedrock"}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs/compose", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("compose code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp composeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, want := range []string{bedrockRuntimeHost(region), bedrockControlHost(region)} {
		if !domainAllowedExact(resp.Proposed.InlinePolicy.AllowedDomains, want) {
			t.Errorf("AllowedDomains missing %q (the pinned integration's own region); got %v",
				want, resp.Proposed.InlinePolicy.AllowedDomains)
		}
	}
	if _, ok := apiKeyGrantForHost(&resp.Proposed.InlinePolicy, "api.anthropic.com"); ok {
		t.Error("compose added a default Anthropic api_key grant despite a pinned Bedrock integration — the region/model override was discarded")
	}
	if resp.Proposed.BedrockRef == nil || resp.Proposed.BedrockRef.Region != region || resp.Proposed.BedrockRef.Model != model {
		t.Errorf("Proposed.BedrockRef = %+v, want region=%s model=%s", resp.Proposed.BedrockRef, region, model)
	}
	// The regression this item pins: reconcileLLMAccess only recognizes the
	// anthropic/openai api_key shape, and a bedrock fold deliberately adds no
	// such grant — so without the compose.go override this read provisioned=false
	// ("no model access"), contradicting the run itself (launch re-resolves
	// Bedrock auth from integration_id independently of any grant).
	if resp.LLMAccess == nil || !resp.LLMAccess.Provisioned {
		t.Errorf("LLMAccess = %+v, want provisioned=true (a pinned Bedrock integration IS model access)", resp.LLMAccess)
	}
}

// TestComposeRun_IntegrationID_BedrockUnsetIsNotModelAccess pins the OTHER
// half of the override above: a bedrock row is derived from AWS creds/mount
// alone and validates with region and model both EMPTY. For that row
// resolveBedrockAuth returns unready at launch and dispatch falls back to the
// api-key path whose grant applyIntegrationCreds already removed — so the run
// reaches no model at all, and bedrockCaps says exactly that
// (model_api: needs_setup). Compose must not claim otherwise.
func TestComposeRun_IntegrationID_BedrockUnsetIsNotModelAccess(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.Composer = singleBackendRegistry(t, &composer.FakeComposer{Result: composer.Proposal{
		Run:          composer.RunInput{Agent: "claude-code", Task: "build a small website"},
		InlinePolicy: types.RunPolicySpec{AllowedDomains: []string{"github.com"}},
		Summary:      "throwaway sandbox",
	}})
	h.srv.cfg.Store = &composeIntegrationStore{fakeSiteConfigStore: fakeSiteConfigStore{cfg: types.SiteConfig{Integrations: []types.Integration{
		{ID: "acme-bedrock", Category: types.IntegrationAIProvider, Type: "bedrock"},
	}}}}
	// No global fallback either: s.cfg.BedrockRegion/Model are the other half
	// of the same effective-config rule bedrockCaps applies.
	h.srv.cfg.BedrockRegion, h.srv.cfg.BedrockModel = "", ""

	body := `{"prompt":"build a small website","workspace":{"kind":"ephemeral"},"mode":"skip","integration_id":"acme-bedrock"}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs/compose", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("compose code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp composeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.LLMAccess != nil && resp.LLMAccess.Provisioned {
		t.Errorf("LLMAccess = %+v, want NOT provisioned: an unset-region/model bedrock row reaches no model, "+
			"and the capability matrix reports needs_setup for the same integration", resp.LLMAccess)
	}

	// ...and the global config IS the fallback: set it and the same row resolves.
	h.srv.cfg.BedrockRegion, h.srv.cfg.BedrockModel = "us-east-1", "us.anthropic.claude-sonnet-4-5-20250929-v1:0"
	w = do(t, h.srv, http.MethodPost, "/api/v1/runs/compose", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("compose code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	resp = composeResponse{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.LLMAccess == nil || !resp.LLMAccess.Provisioned {
		t.Errorf("LLMAccess = %+v, want provisioned=true once the global Bedrock config supplies region+model", resp.LLMAccess)
	}
}

// TestComposeRun_IntegrationID_NonAIProviderIs400 pins the shared rule
// (runs_create.go's decodeAndValidateCreateRun applies the identical check for
// plain create-run): naming a non-ai_provider integration is a 400, checked
// eagerly before any pipeline stage runs (the zero-value FakeComposer below is
// never even invoked).
func TestComposeRun_IntegrationID_NonAIProviderIs400(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.Composer = singleBackendRegistry(t, &composer.FakeComposer{})
	h.srv.cfg.Store = &fakeSiteConfigStore{cfg: types.SiteConfig{Integrations: []types.Integration{
		{ID: "acme-scm", Category: types.IntegrationSCMHost, Type: "git_host"},
	}}}

	body := `{"prompt":"build a small website","workspace":{"kind":"ephemeral"},"mode":"skip","integration_id":"acme-scm"}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs/compose", adminToken, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("compose code = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// TestComposeRun_ResidentHostSubscriptionPin_MountsCredsAndSurfacesConfigPair
// closes the M3 coverage gap: the compose subscription lane (a workspace pinned
// to a resident_host anthropic_subscription integration, under a ceiling that
// blesses the claude cred mount) had zero assertions — only the api_key/bedrock
// lanes were pinned above. Same ceiling shape compose_llm_grant_test.go's
// applyLLMCredMount unit tests use (subscriptionCeiling), layered onto
// newHarness's baseline DefaultPolicy rather than replacing it wholesale.
func TestComposeRun_ResidentHostSubscriptionPin_MountsCredsAndSurfacesConfigPair(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.Composer = singleBackendRegistry(t, &composer.FakeComposer{Result: composer.Proposal{
		Run:          composer.RunInput{Agent: "claude-code", Task: "build a small website"},
		InlinePolicy: types.RunPolicySpec{AllowedDomains: []string{"github.com"}},
		Summary:      "throwaway sandbox",
	}})
	const wsPath = "/home/ops/acme-sub-app"
	h.srv.cfg.Store = &composeIntegrationStore{
		fakeSiteConfigStore: fakeSiteConfigStore{cfg: types.SiteConfig{Integrations: []types.Integration{
			{ID: "acme-subscription", Category: types.IntegrationAIProvider, Type: "anthropic_subscription",
				Config: mustJSON(map[string]any{"lane": "resident_host"})},
		}}},
		workspaces: []types.Workspace{{
			Name:    "acme-sub-app",
			Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: wsPath}},
			LLMCred: &types.WorkspaceLLMCred{IntegrationRef: "acme-subscription"},
		}},
	}
	h.srv.cfg.DefaultPolicy.AllowedDomains = append(h.srv.cfg.DefaultPolicy.AllowedDomains, "*.anthropic.com", "github.com")
	h.srv.cfg.DefaultPolicy.WorkspaceMounts = append(h.srv.cfg.DefaultPolicy.WorkspaceMounts,
		types.WorkspaceMount{Source: "/home/op/.wardyn/claude-creds/.claude", Target: claudeCredTarget, ReadOnly: boolPtr(true)},
		types.WorkspaceMount{Source: "/home/op/.wardyn/claude-creds/.claude.json", Target: claudeCredJSONTarget, ReadOnly: boolPtr(true)},
	)

	body := `{"prompt":"build a small website","workspace":{"kind":"local","path":"` + wsPath + `"},"mode":"skip"}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs/compose", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("compose code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp composeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !specHasMountTarget(&resp.Proposed.InlinePolicy, claudeCredTarget) {
		t.Errorf("expected the claude cred mount in the proposal's inline_policy; mounts=%+v", resp.Proposed.InlinePolicy.WorkspaceMounts)
	}
	if _, ok := apiKeyGrantForHost(&resp.Proposed.InlinePolicy, "api.anthropic.com"); ok {
		t.Error("subscription mode must not carry an anthropic api_key grant")
	}
	it, ok := findItem(resp.SetupItems, "config_pair:use_subscription:claude_cred_mount")
	if !ok || it.Status != "satisfied" {
		t.Errorf("config_pair setup item = %+v (ok=%v), want a satisfied config_pair:use_subscription:claude_cred_mount row", it, ok)
	}
}

// TestPrimaryWorkspaceLLMRef_MirrorsReferencedWorkspacesPrimary is the H2 unit
// gap: primaryWorkspaceLLMRef must resolve EXACTLY the workspace
// referencedWorkspaces would pick as its primary at launch — every local-kind
// descriptor before any git-kind one, regardless of the request's own
// ordering (applyWorkspaces buckets local dirs into spec.WorkspaceMounts and
// repos into spec.WorkspaceRepos, and referencedWorkspaces resolves ALL mounts
// before ANY repo — see both doc comments).
func TestPrimaryWorkspaceLLMRef_MirrorsReferencedWorkspacesPrimary(t *testing.T) {
	const localPath = "/home/ops/local-app"
	const repoSlug = "acme/repo-app"
	localWS := types.Workspace{
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: localPath}},
		LLMCred: &types.WorkspaceLLMCred{IntegrationRef: "local-pin"},
	}
	repoWS := types.Workspace{
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: repoSlug}},
		LLMCred: &types.WorkspaceLLMCred{IntegrationRef: "repo-pin"},
	}

	tests := []struct {
		name       string
		workspaces []types.Workspace
		wss        []composer.Workspace
		want       string
	}{
		{
			name:       "local-only resolves the local's pin",
			workspaces: []types.Workspace{localWS},
			wss:        []composer.Workspace{{Kind: composer.WorkspaceLocal, Path: localPath}},
			want:       "local-pin",
		},
		{
			// The regression case: applyWorkspaces routes the repo into
			// WorkspaceRepos and the local into WorkspaceMounts, and
			// referencedWorkspaces resolves ALL mounts before any repo — so
			// launch's primary is the LOCAL even though the repo was picked first.
			name:       "repo picked first, local second: primary is still the LOCAL",
			workspaces: []types.Workspace{localWS, repoWS},
			wss: []composer.Workspace{
				{Kind: composer.WorkspaceGit, Repo: repoSlug},
				{Kind: composer.WorkspaceLocal, Path: localPath},
			},
			want: "local-pin",
		},
		{
			name:       "repo-only resolves the repo's pin",
			workspaces: []types.Workspace{repoWS},
			wss:        []composer.Workspace{{Kind: composer.WorkspaceGit, Repo: repoSlug}},
			want:       "repo-pin",
		},
		{
			name:       "an unonboarded local resolves nothing",
			workspaces: nil,
			wss:        []composer.Workspace{{Kind: composer.WorkspaceLocal, Path: "/not/onboarded"}},
			want:       "",
		},
		{
			name:       "an ephemeral descriptor resolves nothing",
			workspaces: []types.Workspace{localWS},
			wss:        []composer.Workspace{{Kind: composer.WorkspaceEphemeral}},
			want:       "",
		},
		{
			// The trivial empty-key guard: a multi-source workspace's single-source
			// mirror fields (Path/Repo here stand in for that) can be blank — must
			// degrade, not look up idx.localDir[""].
			name: "an empty-path descriptor is skipped, never looked up as \"\"",
			workspaces: []types.Workspace{{
				Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: ""}},
				LLMCred: &types.WorkspaceLLMCred{IntegrationRef: "should-not-resolve"},
			}},
			wss:  []composer.Workspace{{Kind: composer.WorkspaceLocal, Path: ""}},
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{cfg: Config{Store: &composeIntegrationStore{workspaces: tc.workspaces}}}
			if got := s.primaryWorkspaceLLMRef(context.Background(), tc.wss); got != tc.want {
				t.Errorf("primaryWorkspaceLLMRef = %q, want %q", got, tc.want)
			}
		})
	}
}
