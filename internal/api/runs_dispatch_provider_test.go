// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The key and endpoint kinds' dispatch arm (#528). Every test seeds the
// OPERATOR namespace with a distinguishable value under the same provider key
// name the owner's is stored under, so "the operator's credential stood in" is
// an assertion about which namespace was read rather than about an error.

const (
	mpOwner         = bedrockGuardMember // mintRunToken's subject
	mpOwnerKey      = "sk-alice-own-provider-key"
	mpOperatorValue = "sk-operator-row-must-never-serve"
)

func mpKeyProvider(id, uid string, kind types.ModelProviderKind, harnesses ...types.ProviderHarness) types.ModelProvider {
	return types.ModelProvider{ID: id, UID: uid, Kind: kind, Harnesses: harnesses}
}

// mpDispatch runs the REAL LLM phase (resolveLLMInjections) for run under
// site, with the store swapped for a capturing one, and hands the authored
// grants to st so the sink can read them.
func mpDispatch(t *testing.T, h *harness, st *bearerGuardStore, policy *types.RunPolicySpec, injections []runner.InjectionGrant,
) (dispatchLLMPlan, map[string]string, []types.CredentialGrant, bool) {
	t.Helper()
	captured := &captureGrantStore{}
	h.srv.cfg.Store = captured
	defer func() { h.srv.cfg.Store = st }()
	env := map[string]string{}
	plan, ok := h.srv.resolveLLMInjections(context.Background(), st.run, dispatchParams{},
		policy, env, injections, "http://wardyn-proxy:3128", artifactRedirectPlan{}, false, st.site, true, false, bedrockCredUngraded())
	st.grants = append(st.grants, captured.grants...)
	return plan, env, captured.grants, ok
}

func mpHarness(t *testing.T, st *bearerGuardStore, p types.ModelProvider) *harness {
	t.Helper()
	h, sec := bearerGuardHarness(t, st)
	name := providerSecretName(p.UID, providerKeyPart)
	sec.m[name] = []byte(mpOperatorValue)
	if err := sec.For(mpOwner).Put(context.Background(), name, []byte(mpOwnerKey)); err != nil {
		t.Fatal(err)
	}
	// A boot gateway that must reach no provider run.
	h.srv.cfg.LLMGateways = map[string]string{"api.anthropic.com": "https://boot-gw.corp.example"}
	return h
}

func grantScope(t *testing.T, g types.CredentialGrant) map[string]any {
	t.Helper()
	var sc map[string]any
	if err := json.Unmarshal(g.Spec.Scope, &sc); err != nil {
		t.Fatal(err)
	}
	return sc
}

func mpInjection(host, secret string) runner.InjectionGrant {
	return runner.InjectionGrant{GrantID: uuid.New(), Rule: egress.InjectionRule{
		Host: host, Header: "x-api-key", Format: "%s", SecretName: secret}}
}

// TestProviderDispatch_KeyLaneServesTheOwnersKeyOnly: a run that chose an
// Anthropic key provider is credentialed by ONE grant, the owner's own key on
// the vendor host, and the sink injects the owner's key — never the
// operator's. Every legacy model credential the policy carried is dropped and
// audited; an unrelated injection survives.
func TestProviderDispatch_KeyLaneServesTheOwnersKeyOnly(t *testing.T) {
	p := mpKeyProvider("anthropic", "uid-a", types.ModelProviderAnthropicAPIKey,
		types.ProviderHarness{Harness: "claude-code", Model: "claude-opus-test"})
	st := &bearerGuardStore{
		run:  types.AgentRun{ID: uuid.New(), Agent: "claude-code", CreatedBy: mpOwner, ModelProviderID: p.ID},
		site: types.SiteConfig{ModelProviders: providerBlock(p)},
	}
	h := mpHarness(t, st, p)
	unrelated := mpInjection("registry.corp.example", "npm-token")
	legacy := []runner.InjectionGrant{
		mpInjection("api.anthropic.com", "anthropic-api-key"),
		mpInjection("api.anthropic.com", types.SubscriptionOAuthSecret),
		mpInjection("boot-gw.corp.example", "anthropic-api-key"),
		mpInjection("evil.example", providerSecretName("uid-a", providerKeyPart)),
	}
	policy := types.RunPolicySpec{WorkspaceMounts: []types.WorkspaceMount{{Source: "/home/op/.claude", Target: claudeCredTarget}}}
	plan, env, grants, ok := mpDispatch(t, h, st, &policy, append([]runner.InjectionGrant{unrelated}, legacy...))
	if !ok || len(grants) != 1 {
		t.Fatalf("dispatch ok=%v grants=%d, want exactly one authored grant", ok, len(grants))
	}
	sc := grantScope(t, grants[0])
	if sc["host"] != "api.anthropic.com" || sc["header"] != "x-api-key" || sc["format"] != "%s" ||
		sc["secret_name"] != providerSecretName("uid-a", providerKeyPart) {
		t.Errorf("grant scope = %v, want the owner's provider key on api.anthropic.com as x-api-key", sc)
	}
	if got := []uuid.UUID{plan.injections[0].GrantID, plan.injections[len(plan.injections)-1].GrantID}; len(plan.injections) != 2 ||
		got[0] != unrelated.GrantID || got[1] != grants[0].ID {
		t.Errorf("injections = %+v, want only the unrelated one and the authored grant", plan.injections)
	}
	if n := strings.Count(auditDatums(h, "run.injection.dropped"), "model_credential_not_provider_authored"); n != len(legacy) {
		t.Errorf("drops audited = %d, want %d", n, len(legacy))
	}
	if !slices.Contains(policy.AllowedDomains, "api.anthropic.com") || len(policy.WorkspaceMounts) != 0 {
		t.Errorf("allowlist %v mounts %v, want the exact vendor host and no ~/.claude mount", policy.AllowedDomains, policy.WorkspaceMounts)
	}
	if plan.llmUpstreams != nil {
		t.Errorf("upstreams = %v, want none — the boot gateway reaches no provider run", plan.llmUpstreams)
	}
	if env["ANTHROPIC_API_KEY"] != "wardyn-proxy-injected" || env["ANTHROPIC_MODEL"] != "claude-opus-test" {
		t.Errorf("env = %v, want the placeholder and the provider's model", env)
	}

	h.broker.minted = broker.Minted{Kind: types.GrantAPIKey, JTI: "j1", Injection: &egress.InjectionRule{
		Host: "api.anthropic.com", Header: "x-api-key", Format: "%s", SecretName: providerSecretName("uid-a", providerKeyPart)}}
	rr := do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+grants[0].ID.String(), h.mintRunToken(t, st.run.ID), "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), mpOwnerKey) || strings.Contains(rr.Body.String(), mpOperatorValue) {
		t.Fatalf("sink = %d %s, want 200 with the owner's own key", rr.Code, rr.Body.String())
	}
}

func auditDatums(h *harness, action string) string {
	var b strings.Builder
	for _, ev := range h.audit.events {
		if ev.Action == action {
			b.Write(ev.Data)
		}
	}
	return b.String()
}

// TestProviderDispatch_EndpointAndRouteThrough: where requests go and how the
// person's credential rides, per kind and harness — a custom endpoint's
// BaseURL+Path becomes this run's own upstream for the harness's dialect, and a
// key kind's route-through sends the vendor's header to the gateway host.
func TestProviderDispatch_EndpointAndRouteThrough(t *testing.T) {
	endpoint := func(h types.ProviderHarness) types.ModelProvider {
		p := mpKeyProvider("corp-gw", "uid-gw", types.ModelProviderCustomEndpoint, h)
		p.BaseURL, p.Auth = "https://gw.corp.example", &types.ProviderAuth{Header: "Authorization", Format: "Bearer %s"}
		return p
	}
	routed := mpKeyProvider("anthropic", "uid-a", types.ModelProviderAnthropicAPIKey, types.ProviderHarness{Harness: "claude-code"})
	routed.BaseURL = "https://route.corp.example"
	for _, tc := range []struct {
		name                 string
		agent                string
		p                    types.ModelProvider
		host, header, format string
		upstreams            map[string]string
	}{
		{"endpoint on claude-code", "claude-code", endpoint(types.ProviderHarness{Harness: "claude-code", Path: "/anthropic"}),
			"gw.corp.example", "Authorization", "Bearer %s", map[string]string{"api.anthropic.com": "https://gw.corp.example/anthropic"}},
		{"endpoint on codex-cli with a per-harness header", "codex-cli",
			endpoint(types.ProviderHarness{Harness: "codex-cli", Path: "/v1", AuthHeader: "X-Token", AuthFormat: "%s"}),
			"gw.corp.example", "X-Token", "%s", map[string]string{"api.openai.com": "https://gw.corp.example/v1"}},
		{"key kind routed through a gateway", "claude-code", routed,
			"route.corp.example", "x-api-key", "%s", map[string]string{"api.anthropic.com": "https://route.corp.example"}},
		{"openai key on the vendor host", "codex-cli",
			mpKeyProvider("openai", "uid-o", types.ModelProviderOpenAIAPIKey, types.ProviderHarness{Harness: "codex-cli"}),
			"api.openai.com", "Authorization", "Bearer %s", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &bearerGuardStore{
				run:  types.AgentRun{ID: uuid.New(), Agent: tc.agent, CreatedBy: mpOwner, ModelProviderID: tc.p.ID},
				site: types.SiteConfig{ModelProviders: providerBlock(tc.p)},
			}
			h := mpHarness(t, st, tc.p)
			policy := types.RunPolicySpec{}
			plan, env, grants, ok := mpDispatch(t, h, st, &policy, nil)
			if !ok || len(grants) != 1 {
				t.Fatalf("dispatch ok=%v grants=%d", ok, len(grants))
			}
			sc := grantScope(t, grants[0])
			if sc["host"] != tc.host || sc["header"] != tc.header || sc["format"] != tc.format {
				t.Errorf("grant scope = %v, want host %s header %s format %s", sc, tc.host, tc.header, tc.format)
			}
			if !slices.Equal(policy.AllowedDomains, []string{tc.host}) {
				t.Errorf("allowlist = %v, want exactly %s", policy.AllowedDomains, tc.host)
			}
			if len(plan.llmUpstreams) != len(tc.upstreams) || !mapsEqual(plan.llmUpstreams, tc.upstreams) {
				t.Errorf("upstreams = %v, want %v", plan.llmUpstreams, tc.upstreams)
			}
			if tc.agent == "codex-cli" && env["OPENAI_BASE_URL"] != "http://wardyn-proxy:3128/wardyn/llm/openai" {
				t.Errorf("OPENAI_BASE_URL = %q, want the brokered route", env["OPENAI_BASE_URL"])
			}
		})
	}
}

func mapsEqual(a, b map[string]string) bool {
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return len(a) == len(b)
}

// TestProviderDispatch_RefusesNamingTheProvider: dispatch re-reads the
// provider the run chose and refuses — FAILED, naming it, before any
// credential is authored — when it is gone, off, no longer serves the agent,
// is of a kind with no arm, when the provider block cannot be read, and when
// the owner's own key is absent even though the operator namespace holds a
// value under that very name.
func TestProviderDispatch_RefusesNamingTheProvider(t *testing.T) {
	p := mpKeyProvider("anthropic", "uid-a", types.ModelProviderAnthropicAPIKey, types.ProviderHarness{Harness: "claude-code"})
	with := func(mut func(*types.ModelProvider)) types.SiteConfig {
		q := p
		mut(&q)
		return types.SiteConfig{ModelProviders: providerBlock(q)}
	}
	for _, tc := range []struct {
		name     string
		provider string
		site     types.SiteConfig
		siteOK   bool
		ownerKey bool
		want     string
	}{
		{"deleted", "anthropic", types.SiteConfig{ModelProviders: providerBlock()}, true, true, providerRefusal("anthropic", mpRunStateMissing).refusal},
		{"turned off", "anthropic", with(func(q *types.ModelProvider) { q.Disabled = true }), true, true, providerRefusal("anthropic", mpRunStateOff).refusal},
		{"no longer serves the agent", "anthropic", with(func(q *types.ModelProvider) { q.Harnesses = nil }), true, true,
			providerRefusal("anthropic", "it is not available to claude-code").refusal},
		{"re-kinded to one with no arm", "anthropic", with(func(q *types.ModelProvider) { q.Kind = types.ModelProviderBedrockBearer }), true, true,
			"This run's model provider is anthropic, and provider dispatch for that kind is not yet available on this build — nothing was started."},
		{"the block cannot be read", "anthropic", types.SiteConfig{}, false, true, mpRunUnreadable},
		// Created under a block that serves no provider for claude-code: nothing
		// on the row says so, and the legacy chain would serve the operator's key.
		{"no provider chosen and the block cannot be read", "", types.SiteConfig{}, false, true, mpRunUnreadable},
		{"the owner's own key is absent", "anthropic", with(func(*types.ModelProvider) {}), true, false,
			"This run's model provider is anthropic, and " + mpRunNoKey + " — " + mpRunConnectRemedy + " Wardyn does not substitute a different model provider."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := providerRunFixture(t, tc.site, &capStore{}, nil)
			st := &mpRefuseStore{integStore: srv.cfg.Store.(*integStore)}
			srv.cfg.Store = st
			sec := srv.cfg.Secrets.(*memSecrets)
			sec.m[providerSecretName("uid-a", providerKeyPart)] = []byte(mpOperatorValue)
			if tc.ownerKey {
				_ = sec.For(mpOwner).Put(context.Background(), providerSecretName("uid-a", providerKeyPart), []byte(mpOwnerKey))
			}
			ctx := context.Background()
			run, err := srv.cfg.Store.CreateRun(ctx, types.AgentRun{ID: uuid.New(), Agent: "claude-code", CreatedBy: mpOwner,
				ModelProviderID: tc.provider, State: types.RunStarting})
			if err != nil {
				t.Fatal(err)
			}
			policy := types.RunPolicySpec{}
			legacy := []runner.InjectionGrant{mpInjection("api.anthropic.com", "anthropic-api-key")}
			if plan, ok := srv.resolveLLMInjections(ctx, run, dispatchParams{}, &policy, map[string]string{}, legacy, "",
				artifactRedirectPlan{}, false, tc.site, tc.siteOK, false, bedrockCredUngraded()); ok || len(plan.injections) != 0 {
				t.Fatalf("dispatch admitted the run (ok=%v) or handed over %v", ok, plan.injections)
			}
			got, _ := srv.cfg.Store.GetRun(ctx, run.ID)
			if got.State != types.RunFailed || st.hint != tc.want {
				t.Errorf("run = %s %q\nwant FAILED %q", got.State, st.hint, tc.want)
			}
			if len(st.grants) != 0 || len(policy.AllowedDomains) != 0 {
				t.Errorf("a refused run authored grants %v / egress %v", st.grants, policy.AllowedDomains)
			}
		})
	}
}

// mpRefuseStore records the failure hint and every grant written.
type mpRefuseStore struct {
	*integStore
	hint   string
	grants []types.CredentialGrant
}

func (s *mpRefuseStore) SetRunFailureHint(_ context.Context, _ uuid.UUID, hint string) error {
	s.hint = hint
	return nil
}

func (s *mpRefuseStore) CreateGrant(_ context.Context, g types.CredentialGrant) (types.CredentialGrant, error) {
	s.grants = append(s.grants, g)
	return g, nil
}

// TestProviderDispatch_NoServingProviderGetsNoLegacyCredential: under a block
// that serves no provider for this agent, a model run gets no model credential
// at all — the operator's key grant and ~/.claude mount the policy carried are
// dropped, no boot gateway is handed over, and the route's 404 says why.
func TestProviderDispatch_NoServingProviderGetsNoLegacyCredential(t *testing.T) {
	codex := mpKeyProvider("openai", "uid-o", types.ModelProviderOpenAIAPIKey, types.ProviderHarness{Harness: "codex-cli"})
	st := &bearerGuardStore{
		run:  types.AgentRun{ID: uuid.New(), Agent: "claude-code", CreatedBy: mpOwner},
		site: types.SiteConfig{ModelProviders: providerBlock(codex)},
	}
	h := mpHarness(t, st, codex)
	policy := types.RunPolicySpec{WorkspaceMounts: []types.WorkspaceMount{{Source: "/home/op/.claude", Target: claudeCredTarget}}}
	plan, _, grants, ok := mpDispatch(t, h, st, &policy, []runner.InjectionGrant{mpInjection("api.anthropic.com", "anthropic-api-key")})
	if !ok || len(grants) != 0 || len(plan.injections) != 0 || len(policy.WorkspaceMounts) != 0 {
		t.Fatalf("ok=%v grants=%d injections=%v mounts=%v, want a run with no model credential", ok, len(grants), plan.injections, policy.WorkspaceMounts)
	}
	if plan.llmUpstreams != nil || plan.llmUnavailableDetail != mpNoProviderDetail {
		t.Errorf("upstreams %v detail %q, want none and the no-provider detail", plan.llmUpstreams, plan.llmUnavailableDetail)
	}
}

// TestLegacyDispatch_DropsUnauthoredProviderKeyInjection: with no provider
// block, a policy grant naming a provider key never reaches the proxy.
func TestLegacyDispatch_DropsUnauthoredProviderKeyInjection(t *testing.T) {
	st := &bearerGuardStore{run: types.AgentRun{ID: uuid.New(), Agent: "claude-code", CreatedBy: mpOwner}}
	h, _ := bearerGuardHarness(t, st)
	stale := mpInjection("evil.example", providerSecretName("uid-a", providerKeyPart))
	plan, _, _, ok := mpDispatch(t, h, st, &types.RunPolicySpec{}, []runner.InjectionGrant{stale})
	if !ok || slices.ContainsFunc(plan.injections, func(ig runner.InjectionGrant) bool { return ig.GrantID == stale.GrantID }) {
		t.Fatalf("ok=%v injections=%v, want the unauthored provider key dropped", ok, plan.injections)
	}
	if !strings.Contains(auditDatums(h, "run.injection.dropped"), "model_provider_not_dispatch_authored") {
		t.Error("the drop was not audited")
	}
}

// TestProviderKeySink_RefusesAnythingButTheOwnersRecordedGrant: the sink
// resolves a provider key only from a grant whose snapshot names that key and
// the run's own subject, and only from that namespace — never the operator's.
func TestProviderKeySink_RefusesAnythingButTheOwnersRecordedGrant(t *testing.T) {
	name := providerSecretName("uid-a", providerKeyPart)
	for _, tc := range []struct {
		name     string
		snapshot any
		ownerKey bool
		want     int
	}{
		{"no snapshot", nil, true, http.StatusForbidden},
		{"a snapshot naming another provider", providerKeySnapshot{OwnerSubject: mpOwner, ProviderUID: "uid-b"}, true, http.StatusForbidden},
		{"a snapshot naming another person", providerKeySnapshot{OwnerSubject: "bob@example.com", ProviderUID: "uid-a"}, true, http.StatusForbidden},
		{"the owner's key is absent, the operator's row is not read", providerKeySnapshot{OwnerSubject: mpOwner, ProviderUID: "uid-a"}, false, http.StatusFailedDependency},
		{"the owner's recorded grant", providerKeySnapshot{OwnerSubject: mpOwner, ProviderUID: "uid-a"}, true, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := mpKeyProvider("anthropic", "uid-a", types.ModelProviderAnthropicAPIKey, types.ProviderHarness{Harness: "claude-code"})
			st := &bearerGuardStore{
				run:  types.AgentRun{ID: uuid.New(), Agent: "claude-code", CreatedBy: mpOwner, ModelProviderID: p.ID},
				site: types.SiteConfig{ModelProviders: providerBlock(p)},
			}
			h, sec := bearerGuardHarness(t, st)
			sec.m[name] = []byte(mpOperatorValue)
			if tc.ownerKey {
				_ = sec.For(mpOwner).Put(context.Background(), name, []byte(mpOwnerKey))
			}
			scope := map[string]any{"host": "api.anthropic.com", "header": "x-api-key", "format": "%s", "secret_name": name}
			if tc.snapshot != nil {
				scope["snapshot"] = tc.snapshot
			}
			raw, _ := json.Marshal(scope)
			id := uuid.New()
			st.grants = append(st.grants, types.CredentialGrant{ID: id, RunID: st.run.ID, Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: raw}})
			h.broker.minted = broker.Minted{Kind: types.GrantAPIKey, JTI: "j1", Injection: &egress.InjectionRule{
				Host: "api.anthropic.com", Header: "x-api-key", Format: "%s", SecretName: name}}
			rr := do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+id.String(), h.mintRunToken(t, st.run.ID), "")
			if rr.Code != tc.want || strings.Contains(rr.Body.String(), mpOperatorValue) {
				t.Fatalf("sink = %d %s, want %d and never the operator's value", rr.Code, rr.Body.String(), tc.want)
			}
			if tc.want == http.StatusOK && !strings.Contains(rr.Body.String(), mpOwnerKey) {
				t.Errorf("sink body %s lacks the owner's key", rr.Body.String())
			}
		})
	}
}

// TestLaunchRecordRun_ChoosesAModelProvider: a record session under a
// provider block chooses one like any model run — freezing it on the row and
// authoring only the owner's provider grant, never the operator default
// integration's — and is refused before anything launches when the
// launcher's own key is absent.
func TestLaunchRecordRun_ChoosesAModelProvider(t *testing.T) {
	p := mpKeyProvider("anthropic", "uid-a", types.ModelProviderAnthropicAPIKey, types.ProviderHarness{Harness: "claude-code"})
	launch := func(t *testing.T, ownerKey bool) (*recordLLMModeStore, *fakeRunner, error) {
		h := newHarness(t)
		ws := types.Workspace{ID: uuid.New(), Status: types.WorkspaceScanned}
		fake := newRecordLLMModeStore(ws)
		fake.sc = types.SiteConfig{
			ModelProviders: providerBlock(p),
			Integrations: []types.Integration{{ID: "corp-default", Kind: types.IntegrationKindAnthropicAPIKey,
				DefaultFor: []string{"agent_runs"}, Secrets: []types.IntegrationSecret{{Role: "api_key", SecretName: "corp-anthropic-key"}}}},
		}
		fr := &fakeRunner{}
		cfg := baseTestConfig(h, fake)
		cfg.Runner, cfg.Broker = fr, h.broker
		sec := &memSecrets{m: map[string][]byte{"corp-anthropic-key": []byte("sk-corp")}}
		if ownerKey {
			_ = sec.For(mpOwner).Put(context.Background(), providerSecretName("uid-a", providerKeyPart), []byte(mpOwnerKey))
		}
		cfg.Secrets = sec
		_, _, err := New(cfg).launchRecordRun(context.Background(), mpOwner, ws, "build", "build", false)
		return fake, fr, err
	}

	t.Run("the launcher's own key", func(t *testing.T) {
		fake, fr, err := launch(t, true)
		if err != nil {
			t.Fatalf("launchRecordRun: %v", err)
		}
		fake.mu.Lock()
		defer fake.mu.Unlock()
		if run := fake.runs[fr.lastSpec.RunID]; run.ModelProviderID != "anthropic" {
			t.Errorf("run.model_provider_id = %q, want anthropic", run.ModelProviderID)
		}
		var secrets []string
		for _, g := range fake.grants {
			if g.Spec.Kind == types.GrantAPIKey {
				secrets = append(secrets, grantScope(t, g)["secret_name"].(string))
			}
		}
		if !slices.Equal(secrets, []string{providerSecretName("uid-a", providerKeyPart)}) {
			t.Errorf("api_key grants name %v, want only the owner's provider key", secrets)
		}
		if got := fake.records[len(fake.records)-1].LLMMode; got != "api-key" {
			t.Errorf("llm_mode = %q, want api-key", got)
		}
	})
	t.Run("no key of their own", func(t *testing.T) {
		_, fr, err := launch(t, false)
		if !errors.Is(err, errModelProviderRefused) || !strings.Contains(err.Error(), mpRunNoKey) {
			t.Fatalf("err = %v, want the model-provider refusal naming the missing key", err)
		}
		if fr.lastSpec.RunID != uuid.Nil {
			t.Error("a refused record launch reached the runner")
		}
	})
}

// TestFoldRunIntegration_FoldsNothingUnderAProviderBlock: once the block is
// set, the operator's default AI integration no longer folds a grant reading
// the operator's secret into a run's policy.
func TestFoldRunIntegration_FoldsNothingUnderAProviderBlock(t *testing.T) {
	integ := types.Integration{ID: "corp-default", Kind: types.IntegrationKindAnthropicAPIKey, DefaultFor: []string{"agent_runs"},
		Secrets: []types.IntegrationSecret{{Role: "api_key", SecretName: "corp-anthropic-key"}}}
	for _, tc := range []struct {
		name  string
		block *types.ModelProviders
		want  int
	}{{"no block: today's fold", nil, 1}, {"a block", providerBlock(), 0}} {
		t.Run(tc.name, func(t *testing.T) {
			srv := providerRunFixture(t, types.SiteConfig{ModelProviders: tc.block, Integrations: []types.Integration{integ}}, &capStore{}, nil)
			srv.cfg.Secrets.(*memSecrets).m["corp-anthropic-key"] = []byte("sk-corp")
			spec := types.RunPolicySpec{}
			srv.foldRunIntegration(context.Background(), "", &spec, createRunRequest{Agent: "claude-code"}, nil)
			if len(spec.EligibleGrants) != tc.want {
				t.Errorf("folded grants = %v, want %d", spec.EligibleGrants, tc.want)
			}
		})
	}
}
