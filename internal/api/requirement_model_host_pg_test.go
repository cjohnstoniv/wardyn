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

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// requirementModelHostFixture is the security review's probe shape (#547), on
// the real PG store and the real age secretstore: the OPERATOR stores
// anthropic-api-key, and an operator-onboarded workspace requires
// `secret:anthropic-api-key` (operator_set). A member (alice, who holds no row
// of her own) launches a run against it.
func requirementModelHostFixture(t *testing.T) (*harness, types.Workspace) {
	t.Helper()
	h, sec := newRunOwnerPGHarness(t)
	h.srv.cfg.Runner = &fakeRunner{}
	h.srv.cfg.Broker = h.broker
	// The model host is allowed; the ceiling proposes no grant of its own, so
	// any model-host grant the run holds came from the requirement fold.
	h.srv.cfg.DefaultPolicy = types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}, MinConfinementClass: types.CC2}
	ctx := context.Background()
	if err := sec.Put(ctx, "anthropic-api-key", []byte("sk-ant-OPERATOR-fake-0000000000")); err != nil {
		t.Fatalf("seed operator key: %v", err)
	}
	ws, err := h.srv.cfg.Store.CreateWorkspace(ctx, types.Workspace{
		ID: uuid.New(), Name: "payments", Status: types.WorkspaceScanned,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: govWorkspaceRepo}},
		Requirements: map[string]types.WorkspaceRequirement{
			"secret:anthropic-api-key": {Level: "required", Provenance: "operator_set"},
		},
	})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	return h, ws
}

// launchAsAlice posts a member run against ws and returns it once dispatch has
// settled, with the grants its create persisted and the injections dispatch
// handed the proxy.
func launchAsAlice(t *testing.T, h *harness, ws types.Workspace, extra ...string) (types.AgentRun, []types.CredentialGrant, []string) {
	t.Helper()
	alice := ssoSession(t, "alice", "alice@corp.example", oidc.RoleUser)
	w := doSSO(t, h.srv, http.MethodPost, "/api/v1/runs", alice,
		`{"agent":"claude-code","task":"t","workspace_id":"`+ws.ID.String()+`"`+strings.Join(extra, "")+`}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create run: %d, want 201: %s", w.Code, w.Body.String())
	}
	var created createRunResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	ctx := context.Background()
	waitFor(t, "run to settle", func() bool {
		got, err := h.srv.cfg.Store.GetRun(ctx, created.ID)
		return err == nil && got.State != types.RunPending && got.State != types.RunStarting
	})
	grants, err := h.srv.cfg.Store.ListGrantsByRun(ctx, created.ID)
	if err != nil {
		t.Fatalf("ListGrantsByRun: %v", err)
	}
	fr := h.srv.cfg.Runner.(*fakeRunner)
	fr.mu.Lock()
	defer fr.mu.Unlock()
	var injected []string
	for _, ig := range fr.lastSpec.ProxyConfig.Injection {
		injected = append(injected, ig.Rule.Host+"="+ig.Rule.SecretName)
	}
	return created.AgentRun, grants, injected
}

func grantSecretOnHost(t *testing.T, grants []types.CredentialGrant, host string) []string {
	t.Helper()
	var out []string
	for _, g := range grants {
		if g.Spec.Kind != types.GrantAPIKey {
			continue
		}
		var scope struct {
			Host       string `json:"host"`
			SecretName string `json:"secret_name"`
		}
		if err := json.Unmarshal(g.Spec.Scope, &scope); err != nil {
			t.Fatalf("decode grant scope: %v", err)
		}
		if scope.Host == host {
			out = append(out, scope.SecretName)
		}
	}
	return out
}

// TestPG_SecretRequirement_NoModelHostGrant: with no model-provider block, the
// run gets NO api_key grant on the model host from the requirement, so there is
// nothing for the proxy's injection lookup to resolve the operator's key for;
// the skip is audited on the run.
func TestPG_SecretRequirement_NoModelHostGrant(t *testing.T) {
	h, ws := requirementModelHostFixture(t)
	run, grants, injected := launchAsAlice(t, h, ws)
	if got := grantSecretOnHost(t, grants, "api.anthropic.com"); len(got) != 0 {
		t.Fatalf("the run holds an api_key grant on the model host naming %v — the operator's key reaches a member's run", got)
	}
	for _, in := range injected {
		if strings.HasPrefix(in, "api.anthropic.com=") {
			t.Fatalf("dispatch handed the proxy a model-host injection: %v", injected)
		}
	}
	var skipped bool
	for _, ev := range h.audit.snapshot() {
		if ev.Action == "run.requirement.skip" && ev.RunID != nil && *ev.RunID == run.ID && ev.Outcome == "denied" {
			skipped = true
		}
	}
	if !skipped {
		t.Error("no run.requirement.skip audit row for the skipped requirement")
	}
}

// TestPG_SecretRequirement_ProviderBlock: under a model-provider block the
// requirement authors no grant either, and the ONLY injection on the model host
// is the one the provider's arm authors from the run owner's own key — so a
// requirement grant can never sit beside it and win buildInjector's
// last-writer-wins by host.
func TestPG_SecretRequirement_ProviderBlock(t *testing.T) {
	h, ws := requirementModelHostFixture(t)
	w := do(t, h.srv, http.MethodPut, "/api/v1/model-providers", adminToken,
		`{"providers":[{"id":"anthropic","kind":"anthropic_api_key","harnesses":[{"harness":"claude-code"}]}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT /model-providers: %d: %s", w.Code, w.Body.String())
	}
	alice := ssoSession(t, "alice", "alice@corp.example", oidc.RoleUser)
	w = doSSO(t, h.srv, http.MethodPut, "/api/v1/model-providers/anthropic/credential", alice, `{"value":"sk-ant-alice-own-0000000000"}`)
	if w.Code != http.StatusNoContent && w.Code != http.StatusOK {
		t.Fatalf("PUT own credential: %d: %s", w.Code, w.Body.String())
	}
	_, grants, injected := launchAsAlice(t, h, ws)
	onHost := grantSecretOnHost(t, grants, "api.anthropic.com")
	if len(onHost) != 1 || !strings.HasPrefix(onHost[0], providerSecretPrefix) {
		t.Fatalf("model-host grants = %v, want exactly the provider arm's own %s…-key", onHost, providerSecretPrefix)
	}
	var modelInjections []string
	for _, in := range injected {
		if strings.HasPrefix(in, "api.anthropic.com=") {
			modelInjections = append(modelInjections, in)
		}
	}
	if len(modelInjections) != 1 || !strings.Contains(modelInjections[0], "="+providerSecretPrefix) {
		t.Fatalf("model-host injections = %v, want exactly the provider arm's", modelInjections)
	}
}

// integrationOnProviderHost is the round-2 review's probe shape (#547): a
// provider block where alice runs on the anthropic provider with her own key,
// a SECOND provider row serving a model at modelHost, and a stored git_host
// row — written before integration writes refused it — that presents the
// OPERATOR's corp-llm-key as a header credential on that host. The workspace
// requires that row.
func integrationOnProviderHost(t *testing.T, secondProvider, modelHost string) (*harness, types.Workspace) {
	t.Helper()
	h, sec := newRunOwnerPGHarness(t)
	h.srv.cfg.Runner = &fakeRunner{}
	h.srv.cfg.Broker = h.broker
	h.srv.cfg.DefaultPolicy = types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}, MinConfinementClass: types.CC2}
	ctx := context.Background()
	if err := sec.Put(ctx, "corp-llm-key", []byte("sk-OPERATOR-gateway-0000000000")); err != nil {
		t.Fatalf("seed operator key: %v", err)
	}
	w := do(t, h.srv, http.MethodPut, "/api/v1/model-providers", adminToken,
		`{"providers":[{"id":"anthropic","kind":"anthropic_api_key","harnesses":[{"harness":"claude-code"}]},`+secondProvider+`]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT /model-providers: %d: %s", w.Code, w.Body.String())
	}
	alice := ssoSession(t, "alice", "alice@corp.example", oidc.RoleUser)
	w = doSSO(t, h.srv, http.MethodPut, "/api/v1/model-providers/anthropic/credential", alice, `{"value":"sk-ant-alice-own-0000000000"}`)
	if w.Code != http.StatusNoContent && w.Code != http.StatusOK {
		t.Fatalf("PUT own credential: %d: %s", w.Code, w.Body.String())
	}
	sc, err := h.srv.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		t.Fatalf("GetSiteConfig: %v", err)
	}
	sc.Integrations = append(sc.Integrations, types.Integration{
		ID: "git_host:" + modelHost, Name: modelHost, Kind: types.IntegrationKindGitHost,
		Egress: []string{modelHost},
		Secrets: []types.IntegrationSecret{{Role: "pat", SecretName: "corp-llm-key",
			Delivery: &types.IntegrationDelivery{Mode: types.DeliveryProxyHeader, Header: "Authorization", Format: "Bearer %s"}}},
	})
	if _, err := h.srv.cfg.Store.PutSiteConfig(ctx, sc); err != nil {
		t.Fatalf("PutSiteConfig: %v", err)
	}
	ws, err := h.srv.cfg.Store.CreateWorkspace(ctx, types.Workspace{
		ID: uuid.New(), Name: "gateway", Status: types.WorkspaceScanned,
		Sources:      []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: govWorkspaceRepo}},
		Requirements: map[string]types.WorkspaceRequirement{"integration:git_host:" + modelHost: {Level: "required", Provenance: "operator_set"}},
	})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	return h, ws
}

func assertNothingOnHost(t *testing.T, h *harness, run types.AgentRun, grants []types.CredentialGrant, injected []string, host string) {
	t.Helper()
	if got := grantSecretOnHost(t, grants, host); len(got) != 0 {
		t.Fatalf("the run holds an api_key grant on %s naming %v — the operator's key rides a member's run on a model host", host, got)
	}
	for _, in := range injected {
		if strings.HasPrefix(in, host+"=") {
			t.Fatalf("dispatch handed the proxy an injection on %s: %v", host, injected)
		}
	}
	var skipped bool
	for _, ev := range h.audit.snapshot() {
		if ev.Action == "run.requirement.skip" && ev.RunID != nil && *ev.RunID == run.ID && strings.Contains(string(ev.Data), host) {
			skipped = true
		}
	}
	if !skipped {
		t.Errorf("no run.requirement.skip naming %s on the run", host)
	}
}

// TestPG_IntegrationRequirement_CustomEndpointProviderHost: a second provider
// row's custom endpoint is a model-serving host, so the stored row's operator
// key is neither granted nor injected there.
func TestPG_IntegrationRequirement_CustomEndpointProviderHost(t *testing.T) {
	const host = "llm.corp.example"
	h, ws := integrationOnProviderHost(t,
		`{"id":"corp-gateway","kind":"custom_endpoint","base_url":"https://`+host+`","harnesses":[{"harness":"claude-code","path":"/anthropic"}]}`, host)
	run, grants, injected := launchAsAlice(t, h, ws, `,"model_provider":"anthropic"`)
	assertNothingOnHost(t, h, run, grants, injected, host)
}

// TestPG_IntegrationRequirement_BedrockProviderRegionHost: a Bedrock provider
// row's regional runtime host is model-serving too, even with no boot-time
// Bedrock region configured.
func TestPG_IntegrationRequirement_BedrockProviderRegionHost(t *testing.T) {
	const host = "bedrock-runtime.eu-west-1.amazonaws.com"
	h, ws := integrationOnProviderHost(t,
		`{"id":"bedrock-eu","kind":"bedrock_bearer","bedrock":{"region":"eu-west-1"},"harnesses":[{"harness":"claude-code","model":"eu.anthropic.claude-sonnet-4-5-20250929-v1:0"}]}`, host)
	run, grants, injected := launchAsAlice(t, h, ws, `,"model_provider":"anthropic"`)
	assertNothingOnHost(t, h, run, grants, injected, host)
}
