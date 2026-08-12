// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// foldRef exercises resolution TIER 3 (the workspace's own binding) through the
// same foldRunIntegration launch and preflight both call, and returns the
// applied integration id (or "") so these tests keep asserting exactly what
// they always did. A bare createRunRequest names no integration_id, so tier 1
// cannot pre-empt the workspace binding under test.
func foldRef(s *Server, spec *types.RunPolicySpec, ws *types.Workspace, agent string) string {
	var refs []types.Workspace
	if ws != nil {
		refs = []types.Workspace{*ws}
	}
	integ, kind, _ := s.foldRunIntegration(context.Background(), spec, createRunRequest{Agent: agent}, refs)
	if kind == "" {
		return ""
	}
	return integ.ID
}

// applyWorkspaceCreds/applyPrimaryWorkspaceCreds fold a workspace/run's
// resolved ai_provider Integration into a run's policy at create
// (resolveWorkspaceIntegration / resolveRunIntegration / applyIntegrationCreds,
// llmcred.go). These restore the coverage deleted in 1e2fda5 ("Eight tests for
// workspace credential bindings are deleted rather than ported. The binding is
// inert until integrations resolve it... Keeping green tests over a stub
// would have been the lie.") against the NEW Integration-ref shape now that
// resolution is real: api_key binding grants+egress, an absent secret falling
// back honestly, managed dropping a competing api-key grant, a bedrock ref
// overriding the global region/model, and no binding staying a no-op.

// integrationTestServer builds a Server whose SiteConfig.Integrations is
// exactly `rows` (via fakeSiteConfigStore, site_config_test.go) plus the given
// stored secrets (memSecrets, injection_test.go) — the two ingredients
// resolveWorkspaceIntegration/resolveRunIntegration/applyIntegrationCreds need
// to do real work.
func integrationTestServer(t *testing.T, rows []types.Integration, secretNames ...string) *Server {
	t.Helper()
	m := &memSecrets{m: map[string][]byte{}}
	for _, n := range secretNames {
		m.m[n] = []byte("x")
	}
	fake := &fakeSiteConfigStore{cfg: types.SiteConfig{Integrations: rows}}
	return New(Config{Secrets: m, Store: fake})
}

func apiKeyIntegration(id, credSecret string) types.Integration {
	return types.Integration{
		ID: id, Category: types.IntegrationAIProvider, Type: "anthropic_api_key",
		Credentials: map[string]string{"api_key": credSecret},
	}
}

// TestApplyWorkspaceCreds_APIKeyIntegration_AppendsGrantAndEgress restores
// TestApplyWorkspaceCreds_APIKey_AppendsGrantAndEgress (pre-Integration) —
// same assertions, now driven by a workspace ref resolving to a STORED
// ai_provider integration instead of an inline {mode, api_key_secret}.
func TestApplyWorkspaceCreds_APIKeyIntegration_AppendsGrantAndEgress(t *testing.T) {
	s := integrationTestServer(t, []types.Integration{apiKeyIntegration("acme-anthropic", "acme-anthropic-key")}, "acme-anthropic-key")
	spec := &types.RunPolicySpec{}
	ws := &types.Workspace{LLMCred: &types.WorkspaceLLMCred{IntegrationRef: "acme-anthropic"}}
	if ref := foldRef(s, spec, ws, "claude-code"); ref != "acme-anthropic" {
		t.Fatalf("ref = %q, want acme-anthropic", ref)
	}
	g, ok := apiKeyGrantForHost(spec, "api.anthropic.com")
	if !ok {
		t.Fatal("expected an api_key grant for api.anthropic.com")
	}
	var scope map[string]string
	_ = json.Unmarshal(g.Scope, &scope)
	if scope["secret_name"] != "acme-anthropic-key" {
		t.Fatalf("grant secret = %q, want the integration's own secret", scope["secret_name"])
	}
	if !domainAllowedExact(spec.AllowedDomains, "api.anthropic.com") {
		t.Fatal("expected api.anthropic.com egress coupled to the grant")
	}
}

// TestApplyWorkspaceCreds_APIKeyIntegration_AbsentSecretFallsBack restores
// TestApplyWorkspaceCreds_APIKey_AbsentSecretFallsBack: an integration naming
// a secret that isn't actually stored must fall back honestly (no binding),
// never mint a grant the proxy would fail closed on.
func TestApplyWorkspaceCreds_APIKeyIntegration_AbsentSecretFallsBack(t *testing.T) {
	s := integrationTestServer(t, []types.Integration{apiKeyIntegration("acme-anthropic", "missing-key")}) // secret NOT stored
	spec := &types.RunPolicySpec{}
	ws := &types.Workspace{LLMCred: &types.WorkspaceLLMCred{IntegrationRef: "acme-anthropic"}}
	if ref := foldRef(s, spec, ws, "claude-code"); ref != "" {
		t.Fatalf("expected fall-back (no binding) for an absent secret, got %q", ref)
	}
	if len(spec.EligibleGrants) != 0 {
		t.Fatal("must not append a grant whose secret would fail the proxy closed")
	}
}

// TestApplyWorkspaceCreds_ManagedIntegration_EnsuresEgressDropsCompetingAPIKey
// restores TestApplyWorkspaceCreds_Managed_EnsuresEgressDropsCompetingAPIKey.
func TestApplyWorkspaceCreds_ManagedIntegration_EnsuresEgressDropsCompetingAPIKey(t *testing.T) {
	s := integrationTestServer(t, []types.Integration{{
		ID: "acme-sub", Category: types.IntegrationAIProvider, Type: "anthropic_subscription",
		Config: mustJSON(map[string]any{"lane": "managed"}),
	}})
	scope, _ := json.Marshal(map[string]string{"host": "api.anthropic.com", "secret_name": "stray"})
	spec := &types.RunPolicySpec{EligibleGrants: []types.GrantSpec{{Kind: types.GrantAPIKey, Scope: scope}}}
	ws := &types.Workspace{LLMCred: &types.WorkspaceLLMCred{IntegrationRef: "acme-sub"}}
	if ref := foldRef(s, spec, ws, "claude-code"); ref != "acme-sub" {
		t.Fatalf("ref = %q, want acme-sub", ref)
	}
	if _, ok := apiKeyGrantForHost(spec, "api.anthropic.com"); ok {
		t.Fatal("managed must drop a competing api-key grant so the managed token injects")
	}
	if !domainAllowedExact(spec.AllowedDomains, "api.anthropic.com") {
		t.Fatal("managed must ensure api.anthropic.com egress")
	}
}

// TestApplyWorkspaceCreds_ResidentHostIntegration_InjectsCeilingMount covers
// the OTHER subscription lane: resident_host must go through
// applyLLMCredMount — THE single subscription gate (unchanged) — rather than
// the managed (no-mount) path.
func TestApplyWorkspaceCreds_ResidentHostIntegration_InjectsCeilingMount(t *testing.T) {
	s := integrationTestServer(t, []types.Integration{{
		ID: "acme-sub-host", Category: types.IntegrationAIProvider, Type: "anthropic_subscription",
		Config: mustJSON(map[string]any{"lane": "resident_host"}),
	}})
	s.cfg.DefaultPolicy = types.RunPolicySpec{
		AllowedDomains:  []string{"api.anthropic.com"},
		WorkspaceMounts: []types.WorkspaceMount{{Source: "/host/.claude", Target: claudeCredTarget}},
	}
	spec := &types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}}
	ws := &types.Workspace{LLMCred: &types.WorkspaceLLMCred{IntegrationRef: "acme-sub-host"}}
	if ref := foldRef(s, spec, ws, "claude-code"); ref != "acme-sub-host" {
		t.Fatalf("ref = %q, want acme-sub-host", ref)
	}
	if !specHasMountTarget(spec, claudeCredTarget) {
		t.Fatal("resident_host lane must inject the ceiling's Claude credential mount (applyLLMCredMount)")
	}
}

// TestApplyPrimaryWorkspaceCreds_BedrockIntegration_OverridesGlobalRegionModel
// restores TestCreateRun_WorkspaceBedrockCred_DisplacesAPIKeyGrantAndAllowsRegion's
// core claim (the workspace's region/model beats the server's global Bedrock
// config) at the fold level: foldRunIntegration is exactly what runs.go calls
// at create, unchanged in position, so this proves the same thing without
// re-standing-up the full dispatch harness that commit deleted alongside it.
func TestApplyPrimaryWorkspaceCreds_BedrockIntegration_OverridesGlobalRegionModel(t *testing.T) {
	const wsRegion, wsModel = "eu-central-1", "eu.anthropic.claude-sonnet-4-5-20250929-v1:0"
	s := integrationTestServer(t, []types.Integration{{
		ID: "acme-bedrock", Category: types.IntegrationAIProvider, Type: "bedrock",
		Config: mustJSON(map[string]any{"region": wsRegion, "model": wsModel}),
	}})
	s.cfg.BedrockRegion, s.cfg.BedrockModel = "us-east-1", "us.anthropic.claude-sonnet-4-5-20250929-v1:0"
	spec := &types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}}
	ws := types.Workspace{LLMCred: &types.WorkspaceLLMCred{IntegrationRef: "acme-bedrock"}}
	req := createRunRequest{Agent: "claude-code"}

	_, _, bedrockRef := s.foldRunIntegration(context.Background(), spec, req, []types.Workspace{ws})
	if bedrockRef == nil || bedrockRef.Region != wsRegion || bedrockRef.Model != wsModel {
		t.Fatalf("bedrockRef = %+v, want region=%s model=%s", bedrockRef, wsRegion, wsModel)
	}
	for _, want := range []string{bedrockRuntimeHost(wsRegion), bedrockControlHost(wsRegion)} {
		if !domainAllowedExact(spec.AllowedDomains, want) {
			t.Errorf("AllowedDomains missing %q; got %v", want, spec.AllowedDomains)
		}
	}
	if domainAllowedExact(spec.AllowedDomains, bedrockRuntimeHost("us-east-1")) {
		t.Errorf("AllowedDomains carries the GLOBAL region's bedrock host; the integration override must replace it: %v",
			spec.AllowedDomains)
	}
}

// TestApplyIntegrationCreds_IncompatibleAgentProvider_FoldsNothing pins SPINE-1:
// applyIntegrationCreds consults the harness catalog (harnessProviderReason) and
// folds NOTHING — no grant, no removal — for an agent×provider combination the
// catalog forbids. Without it, an openai_api_key integration on a claude-code run
// injected the OpenAI key as x-api-key onto api.anthropic.com (credential leaked
// to the wrong vendor).
func TestApplyIntegrationCreds_IncompatibleAgentProvider_FoldsNothing(t *testing.T) {
	openaiInteg := types.Integration{
		ID: "acme-openai", Category: types.IntegrationAIProvider, Type: "openai_api_key",
		Credentials: map[string]string{"api_key": "acme-openai-key"},
	}
	s := integrationTestServer(t, []types.Integration{openaiInteg}, "acme-openai-key")
	// A pre-existing working Anthropic api_key grant the fold must NOT remove.
	spec := &types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}}
	scope := mustJSON(map[string]string{"host": "api.anthropic.com", "header": "x-api-key", "format": "%s", "secret_name": "anthropic-api-key"})
	spec.EligibleGrants = []types.GrantSpec{{Kind: types.GrantAPIKey, Scope: scope}}

	kind, bedrockRef := s.applyIntegrationCreds(context.Background(), spec, openaiInteg, "claude-code")
	if kind != "" || bedrockRef != nil {
		t.Fatalf("applyIntegrationCreds(openai_api_key, claude-code) = (%q, %v), want (\"\", nil) — no fold", kind, bedrockRef)
	}
	// The OpenAI secret must NOT have been grafted onto api.anthropic.com.
	g, ok := apiKeyGrantForHost(spec, "api.anthropic.com")
	if !ok {
		t.Fatal("the pre-existing anthropic grant was removed — the incompatible fold must be a no-op")
	}
	if got := apiKeyGrantScopeSecret(g.Scope); got != "anthropic-api-key" {
		t.Errorf("api.anthropic.com grant secret = %q, want the untouched anthropic-api-key (the OpenAI key must never reach Anthropic)", got)
	}
}

// TestApplyPrimaryWorkspaceCreds_DefaultForAgentRuns_AppliesWithNoWorkspace
// proves resolution tier 3 (the operator's site-wide default) fires even for
// a run with NO workspace at all — new behavior an Integration-less world
// could never offer.
func TestApplyPrimaryWorkspaceCreds_DefaultForAgentRuns_AppliesWithNoWorkspace(t *testing.T) {
	in := apiKeyIntegration("acme-anthropic", "acme-anthropic-key")
	in.DefaultFor = []string{"agent_runs"}
	s := integrationTestServer(t, []types.Integration{in}, "acme-anthropic-key")
	spec := &types.RunPolicySpec{}
	req := createRunRequest{Agent: "claude-code"}

	_, _, bedrockRef := s.foldRunIntegration(context.Background(), spec, req, nil)
	if bedrockRef != nil {
		t.Errorf("bedrockRef = %+v, want nil for an api_key integration", bedrockRef)
	}
	if _, ok := apiKeyGrantForHost(spec, "api.anthropic.com"); !ok {
		t.Fatal("expected the DefaultFor:agent_runs integration to apply with zero workspaces attached")
	}
}

// TestApplyPrimaryWorkspaceCreds_ExecRunBindsNoIntegration pins the exec
// contract at the grant-folding layer: a task-mode=exec run ("no agent, no LLM
// credentials") must NOT fold the operator's site-wide default ai_provider
// integration, or persistRunGrants would inject the operator's api key + widen
// egress to api.anthropic.com for a plain shell command that never asked for a
// model (the api-key sibling of the subscription/managed injection resolveLLM-
// Transport already suppresses for exec). Same server/default as the test above,
// which DOES fold it for a normal run — the only difference is TaskMode.
func TestApplyPrimaryWorkspaceCreds_ExecRunBindsNoIntegration(t *testing.T) {
	in := apiKeyIntegration("acme-anthropic", "acme-anthropic-key")
	in.DefaultFor = []string{"agent_runs"}
	s := integrationTestServer(t, []types.Integration{in}, "acme-anthropic-key")
	spec := &types.RunPolicySpec{}
	req := createRunRequest{Agent: "claude-code", TaskMode: "exec"}

	integ, kind, bedrockRef := s.foldRunIntegration(context.Background(), spec, req, nil)
	if integ.ID != "" || kind != "" || bedrockRef != nil {
		t.Errorf("exec run folded an integration: integ=%q kind=%q bedrockRef=%v, want all empty", integ.ID, kind, bedrockRef)
	}
	if _, ok := apiKeyGrantForHost(spec, "api.anthropic.com"); ok {
		t.Fatal("exec run got an api-key grant folded — the operator's default must not inject a credential into a plain-command run")
	}
	if len(spec.AllowedDomains) != 0 {
		t.Fatalf("exec run had egress widened to %v — the integration host must not be appended", spec.AllowedDomains)
	}
}

// TestApplyPrimaryWorkspaceCreds_DanglingWorkspaceRef_DoesNotCascadeToDefault
// pins resolveRunIntegration's documented non-cascade (llmcred.go): a
// workspace bound to a SPECIFIC integration ref that no longer resolves
// (deleted, or renamed to something non-ai_provider) must NOT silently fall
// through to the operator's site-wide DefaultFor:agent_runs default — even
// though, per the test above, that default is live and would otherwise fire.
// A stale binding silently promoting to a different integration is a
// credential surprise, not a convenience.
func TestApplyPrimaryWorkspaceCreds_DanglingWorkspaceRef_DoesNotCascadeToDefault(t *testing.T) {
	def := apiKeyIntegration("acme-default", "acme-default-key")
	def.DefaultFor = []string{"agent_runs"}
	s := integrationTestServer(t, []types.Integration{def}, "acme-default-key")
	spec := &types.RunPolicySpec{}
	ws := types.Workspace{LLMCred: &types.WorkspaceLLMCred{IntegrationRef: "does-not-exist"}}
	req := createRunRequest{Agent: "claude-code"}

	_, _, bedrockRef := s.foldRunIntegration(context.Background(), spec, req, []types.Workspace{ws})
	if bedrockRef != nil {
		t.Errorf("bedrockRef = %+v, want nil (nothing should have folded)", bedrockRef)
	}
	if _, ok := apiKeyGrantForHost(spec, "api.anthropic.com"); ok {
		t.Error("a dangling workspace ref cascaded to the operator's site-wide default — resolveRunIntegration's documented non-cascade was violated")
	}
}

// TestApplyPrimaryWorkspaceCreds_ExplicitIntegrationID_WinsOverWorkspaceRef
// pins resolution tier 1 (run-explicit) over tier 3 (workspace ref): an
// explicit choice at create time overrides whatever the workspace itself
// bound.
func TestApplyPrimaryWorkspaceCreds_ExplicitIntegrationID_WinsOverWorkspaceRef(t *testing.T) {
	s := integrationTestServer(t, []types.Integration{
		apiKeyIntegration("workspace-pick", "workspace-secret"),
		apiKeyIntegration("explicit-pick", "explicit-secret"),
	}, "workspace-secret", "explicit-secret")
	spec := &types.RunPolicySpec{}
	ws := types.Workspace{LLMCred: &types.WorkspaceLLMCred{IntegrationRef: "workspace-pick"}}
	req := createRunRequest{Agent: "claude-code", IntegrationID: "explicit-pick"}

	s.foldRunIntegration(context.Background(), spec, req, []types.Workspace{ws})
	g, ok := apiKeyGrantForHost(spec, "api.anthropic.com")
	if !ok {
		t.Fatal("expected an api_key grant")
	}
	var scope map[string]string
	_ = json.Unmarshal(g.Scope, &scope)
	if scope["secret_name"] != "explicit-secret" {
		t.Fatalf("grant secret = %q, want the run-explicit integration's secret (explicit-secret), not the workspace's", scope["secret_name"])
	}
}

// TestResolveRunIntegration_ExplicitResidentHostID_RefusedWithoutWorkspacePin
// pins SECMODEL-3: a resident_host subscription mounts the OPERATOR's own
// resident ~/.claude OAuth credentials, a durable property of whichever
// workspace the operator pinned it to (§5.1a) — not a bearer token any run
// author may claim by naming its integration_id. The run-explicit tier may
// carry a resident_host lane ONLY when the run's own primary workspace is
// pinned to that exact same integration.
func TestResolveRunIntegration_ExplicitResidentHostID_RefusedWithoutWorkspacePin(t *testing.T) {
	s := integrationTestServer(t, []types.Integration{{
		ID: "acme-sub-host", Category: types.IntegrationAIProvider, Type: "anthropic_subscription",
		Config: mustJSON(map[string]any{"lane": "resident_host"}),
	}})

	// No workspace pin at all (a run with no workspace, or one the operator
	// never pinned) — the explicit tier must refuse, not silently grant it.
	if _, ok := s.resolveRunIntegration(context.Background(), "acme-sub-host", ""); ok {
		t.Error("resident_host claimed by run-explicit id with no workspace pin at all")
	}
	// A DIFFERENT workspace's own pin does not launder an unrelated run's claim.
	if _, ok := s.resolveRunIntegration(context.Background(), "acme-sub-host", "some-other-integration"); ok {
		t.Error("resident_host claimed by run-explicit id while the primary workspace is pinned elsewhere")
	}
	// The workspace genuinely pinned to THIS integration may still use it via
	// the explicit tier — the two tiers naming the same row is consent, not a conflict.
	if _, ok := s.resolveRunIntegration(context.Background(), "acme-sub-host", "acme-sub-host"); !ok {
		t.Error("resident_host refused even though the run's own primary workspace is pinned to it")
	}
	// The managed lane (no resident host credentials involved) is untouched by
	// this gate — an explicit id alone is enough, exactly as before.
	managed := integrationTestServer(t, []types.Integration{{
		ID: "acme-sub-managed", Category: types.IntegrationAIProvider, Type: "anthropic_subscription",
		Config: mustJSON(map[string]any{"lane": "managed"}),
	}})
	if _, ok := managed.resolveRunIntegration(context.Background(), "acme-sub-managed", ""); !ok {
		t.Error("managed-lane subscription must still resolve via the explicit tier alone")
	}
}

// TestApplyWorkspaceCreds_NoBindingIsNoOp restores TestApplyWorkspaceCreds_NoBindingIsNoOp
// (both the original pre-Integration version and its interim W5-stub
// replacement): nil binding, an empty ref, a non-LLM agent, and — now that
// resolution is real — a ref that resolves to nothing at all (a bare Server
// with no stored/derivable integrations) must all stay a no-op.
func TestApplyWorkspaceCreds_NoBindingIsNoOp(t *testing.T) {
	s := New(Config{})
	spec := &types.RunPolicySpec{}

	// nil binding
	if ref := foldRef(s, spec, &types.Workspace{}, "claude-code"); ref != "" {
		t.Fatalf("nil binding: ref = %q, want empty", ref)
	}
	// explicit empty ref
	ws := &types.Workspace{LLMCred: &types.WorkspaceLLMCred{IntegrationRef: ""}}
	if ref := foldRef(s, spec, ws, "claude-code"); ref != "" {
		t.Fatalf("empty ref: ref = %q, want empty", ref)
	}
	// non-LLM agent: nothing to bind even with a ref set
	ws2 := &types.Workspace{LLMCred: &types.WorkspaceLLMCred{IntegrationRef: "acme-anthropic"}}
	if ref := foldRef(s, spec, ws2, "some-other-agent"); ref != "" {
		t.Fatalf("non-LLM agent: ref = %q, want empty", ref)
	}
	// LLM agent WITH a ref set, but nothing configured anywhere to resolve it
	// against (a bare Server: no Store, no Secrets) — an unresolvable ref falls
	// back honestly rather than erroring.
	if ref := foldRef(s, spec, ws2, "claude-code"); ref != "" {
		t.Fatalf("unresolvable ref: ref = %q, want empty", ref)
	}
	if len(spec.EligibleGrants) != 0 || len(spec.AllowedDomains) != 0 {
		t.Fatal("no-op cases must not mutate the spec")
	}
}

// TestApplyPrimaryWorkspaceCreds_NoneConfiguredIsNoOp is the create-path twin
// of TestApplyWorkspaceCreds_NoBindingIsNoOp: zero workspaces, zero
// integrations, no explicit integration_id — must resolve to nothing. kind==""
// is exactly the gate runs.go keys the run.workspace.creds audit off (it emits
// only when kind != ""), so an empty kind is what proves the no-op never
// audits.
func TestApplyPrimaryWorkspaceCreds_NoneConfiguredIsNoOp(t *testing.T) {
	s := New(Config{})
	spec := &types.RunPolicySpec{}
	req := createRunRequest{Agent: "claude-code"}

	_, kind, bedrockRef := s.foldRunIntegration(context.Background(), spec, req, nil)
	if bedrockRef != nil {
		t.Fatalf("bedrockRef = %+v, want nil", bedrockRef)
	}
	if kind != "" {
		t.Fatalf("kind = %q, want empty (a no-op must not audit run.workspace.creds)", kind)
	}
	if len(spec.EligibleGrants) != 0 || len(spec.AllowedDomains) != 0 {
		t.Fatal("no-op case must not mutate the spec")
	}
}
