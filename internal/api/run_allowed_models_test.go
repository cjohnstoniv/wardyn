// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func testModelLimit() allowedModelsLimit {
	return allowedModelsLimit{models: []string{"model-a", "small-model-a"}, profileID: uuid.New(), profile: "Contractors"}
}

// An empty list is no limit; only an assigned profile's non-empty list binds a run.
func TestModelLimitOf(t *testing.T) {
	unit := &ResolvedUnit{ID: uuid.New(), Name: "Contractors"}
	if modelLimitOf(governanceCeiling{Limits: types.GovernanceLimits{AllowedModels: []string{"model-a"}}}).set() {
		t.Error("a ceiling with no profile bound the run")
	}
	if modelLimitOf(governanceCeiling{Unit: unit}).set() {
		t.Error("an empty list bound the run: empty is no limit")
	}
	got := modelLimitOf(governanceCeiling{Unit: unit, Limits: types.GovernanceLimits{AllowedModels: []string{"model-a"}}})
	if !slices.Equal(got.models, []string{"model-a"}) || got.profileID != unit.ID || got.profile != "Contractors" {
		t.Errorf("limit = %+v", got)
	}
	if got.proxyList() == nil || (allowedModelsLimit{}).proxyList() != nil {
		t.Error("the proxy list is set exactly when the limit is")
	}
	d := ceilingForDispatch(governanceCeiling{Unit: unit, Limits: types.GovernanceLimits{AllowedModels: []string{"model-a"}}},
		adoEntraUngraded(), bedrockCredUngraded())
	if !slices.Equal(d.modelLimit.models, []string{"model-a"}) {
		t.Errorf("dispatch ceiling limit = %+v", d.modelLimit)
	}
}

// Who is bound is the effective ceiling's answer: with GovernAdminRuns on, an admin's SSO session and
// admin-role personal token resolve their profile and its list; the admin token, local mode and the
// exempt marker stay outside governance. With the switch off, no admin is bound; a member always is.
func TestAllowedModels_GovernedAdminBound(t *testing.T) {
	profile := govProfile("Contractors")
	profile.Limits.AllowedModels = []string{"model-a"}
	token := governAdminTokenCtx(t)
	member := withOIDCGroupsTruncated(withOIDCGroups(operatorCtx(governAdminSub, "owner@corp.example", oidc.RoleUser), []string{"eng"}), false)
	for _, c := range []struct {
		name              string
		ctx               context.Context
		boundOn, boundOff bool
	}{
		{"SSO admin session", governAdminSessionCtx(), true, false},
		{"admin-role personal token", token, true, false},
		{"admin token", context.Background(), false, false},
		{"local mode", withLocalPrincipal(context.Background(), "local:dev"), false, false},
		{"exempt marker", context.WithValue(governAdminSessionCtx(), governanceExemptKey{}, true), false, false},
		{"member", member, true, true},
	} {
		for _, on := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/switch %v", c.name, on), func(t *testing.T) {
				s := New(Config{Store: &capStore{govProfile: profile, govTier: types.CapabilitySubjectUser}, Audit: &recRecorder{}, GovernAdminRuns: on})
				ceiling, err := s.effectiveCeiling(c.ctx)
				if err != nil {
					t.Fatalf("effectiveCeiling: %v", err)
				}
				want := map[bool]bool{true: c.boundOn, false: c.boundOff}[on]
				if got := modelLimitOf(ceiling).set(); got != want {
					t.Errorf("bound = %v, want %v", got, want)
				}
			})
		}
	}
}

func TestAllowedModels_WriteValidation(t *testing.T) {
	for _, tc := range []struct {
		list []string
		want string
	}{
		{nil, ""},
		{[]string{"model-a"}, ""},
		{[]string{"model-a", "model-a"}, `limits.allowed_models: "model-a" is listed twice`},
		{[]string{""}, "limits.allowed_models: entry 1 is empty"},
		{[]string{"model-a "}, `limits.allowed_models: "model-a " has leading or trailing space`},
	} {
		if got := governanceLimitsRefusal(types.GovernanceLimits{AllowedModels: tc.list}); got != tc.want {
			t.Errorf("%q: refusal %q, want %q", tc.list, got, tc.want)
		}
	}
}

// A provider's calls are unchecked only where the sandbox signs AWS requests itself.
func TestAllowedModels_UncheckedProviders(t *testing.T) {
	sso, bearer := brSSOProvider(), brBearerProvider()
	off, resigned := &Server{}, &Server{cfg: Config{AWSSigV4AtProxy: true, AWSSSOProxyInject: true}}
	if !off.providerModelsUnchecked(sso) || resigned.providerModelsUnchecked(sso) {
		t.Error("a Bedrock sign-in is unchecked exactly when the proxy does not re-sign it")
	}
	for _, p := range []types.ModelProvider{bearer, subProvider("claude"),
		{Kind: types.ModelProviderAnthropicAPIKey}, {Kind: types.ModelProviderOpenAIAPIKey},
		{Kind: types.ModelProviderCustomEndpoint}, {Kind: types.ModelProviderAzureFoundry}} {
		if off.providerModelsUnchecked(p) {
			t.Errorf("%s: unchecked, want checked", p.Kind)
		}
	}
	for _, tc := range []struct {
		name string
		l    llmTransport
		want bool
	}{
		{"a sandbox-signed Bedrock lane", llmTransport{bedrockReady: true}, true},
		{"a bearer lane", llmTransport{bedrockReady: true, injectBedrockBearer: true}, false},
		{"a re-signed lane", llmTransport{bedrockReady: true, bedrock: bedrockAuth{sigv4Hosts: []string{"bedrock-runtime.us-west-2.amazonaws.com"}}}, false},
		{"a key lane", llmTransport{}, false},
	} {
		if got := laneModelsUnchecked(tc.l); got != tc.want {
			t.Errorf("%s: unchecked = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Create and preflight refuse a run held to a list whose chosen provider (its own, or an included
// harness's) the proxy cannot check; with no list the same run passes.
func TestAllowedModels_DoorsRefuseAnUncheckedProvider(t *testing.T) {
	i := slices.IndexFunc(runFoldSteps, func(st runFoldStep) bool {
		return strings.HasSuffix(runtime.FuncForPC(reflect.ValueOf(st.run).Pointer()).Name(), ".stepAllowedModelsChecked")
	})
	if i < 0 || runFoldSteps[i].modes != foldCreate|foldPreflight {
		t.Fatal("stepAllowedModelsChecked is not a create and preflight fold step")
	}
	sso := brSSOProvider()
	sso.Name = "Corp Bedrock"
	ssoChoice := runProviderChoice{chosen: true, provider: sso}
	key := runProviderChoice{chosen: true, provider: types.ModelProvider{ID: "anthropic-key", Kind: types.ModelProviderAnthropicAPIKey}}
	limited := governanceCeiling{Unit: &ResolvedUnit{ID: uuid.New(), Name: "Contractors"}, Limits: types.GovernanceLimits{AllowedModels: []string{"model-a"}}}
	for _, tc := range []struct {
		name    string
		ceiling governanceCeiling
		primary runProviderChoice
		set     []harnessChoice
		refused bool
	}{
		{"the run's provider", limited, ssoChoice, nil, true},
		{"an included harness's", limited, key, []harnessChoice{{harness: "codex-cli", choice: ssoChoice}}, true},
		{"a checked provider", limited, key, nil, false},
		{"no list", governanceCeiling{}, ssoChoice, nil, false},
		{"an empty list", governanceCeiling{Unit: limited.Unit}, ssoChoice, nil, false},
	} {
		w := httptest.NewRecorder()
		f := &runFold{w: w, r: httptest.NewRequest(http.MethodPost, "/", nil), ceiling: tc.ceiling, mpChoice: tc.primary, harnessChoices: tc.set}
		ok := (&Server{}).stepAllowedModelsChecked(f, &foldRecorder{})
		if ok == tc.refused {
			t.Errorf("%s: step passed = %t, want %t", tc.name, ok, !tc.refused)
		}
		if tc.refused && (w.Code != http.StatusForbidden || reasonOf(t, w) != reasonAllowedModelsUnchecked ||
			!strings.Contains(w.Body.String(), "can't check the models Corp Bedrock is asked for")) {
			t.Errorf("%s: answer %d %s", tc.name, w.Code, w.Body.String())
		}
	}
}

// limitedDispatch runs the real LLM phase for k's happy path with limit on the run.
func limitedDispatch(t *testing.T, k joinKind, limit allowedModelsLimit) (dispatchLLMPlan, bool, *subStore) {
	t.Helper()
	p := k.record(joinScenario{name: "happy", stored: true, chosen: true})
	h, st, sec := subHarness(t, p)
	h.srv.cfg.AWSSSOProxyInject = true
	st.run.Agent = k.agent
	for _, part := range []string{providerKeyPart, providerOAuthPart, providerSSOPart} {
		_ = sec.For(subOwner).Delete(context.Background(), providerSecretName(p.UID, part))
	}
	if err := k.store(sec.For(subOwner), p); err != nil {
		t.Fatal(err)
	}
	plan, ok := h.srv.resolveLLMInjections(context.Background(), st.run, dispatchParams{modelLimit: limit}, &types.RunPolicySpec{},
		map[string]string{}, nil, joinProxyURL, artifactRedirectPlan{}, false, st.site, true, false, bedrockCredUngraded())
	return plan, ok, st
}

// dispatchedProxyConfig is the proxy config dispatchRun writes from plan's model fields, through the
// one mapping the sidecar's config comes from (runner.ProxyConfigOf).
func dispatchedProxyConfig(runID uuid.UUID, plan dispatchLLMPlan) proxy.Config {
	return runner.ProxyConfigOf(runID, runner.ProxyConfig{
		Injection: plan.injections, MITMCACertPEM: plan.mitmCACertPEM, MITMCAKeyPEM: plan.mitmCAKeyPEM,
		MITMHosts: slices.Concat(plan.bedrockMITMHosts, plan.azure.mitmHosts), MITMLLM: plan.mitmLLM,
		AWSSigV4: plan.awsSigV4, AzureGates: plan.azure.gates, LLMChannelHosts: plan.channelHosts,
		LLMUpstreams: plan.llmUpstreams, LocalModels: plan.localModels, AllowedModels: plan.allowedModels,
	}, runner.ProxyListenPort)
}

// intercepted reports whether the proxy config cfg has the run's proxy read a connection to host, so
// the allowed-models gate sees each request on it: a vendor host under mitm_llm, any other under an
// interception entry or a re-signed AWS host, always with the per-run CA. A host it does not read is
// refused as an opaque tunnel while the list is set.
func intercepted(cfg proxy.Config, host string) bool {
	if cfg.MITMCAKeyPEM == "" {
		return false
	}
	if host == "api.anthropic.com" || host == "api.openai.com" {
		return cfg.MITMLLM
	}
	for _, e := range cfg.MITMHosts {
		if h, _, err := net.SplitHostPort(e); err == nil {
			e = h
		}
		if strings.EqualFold(e, host) {
			return true
		}
	}
	return cfg.AWSSigV4 != nil && slices.Contains(cfg.AWSSigV4.Hosts, host)
}

// Every hosted provider kind, through the real dispatch: with a list set the run's proxy config holds the
// list, intercepts every host the kind's model credential is authored for, and keeps a brokered gateway's
// route (the proxy refuses an unlisted model on each of these paths:
// internal/egress/proxy TestAllowedModels_EveryProviderKindChecked); a Bedrock sign-in the sandbox signs
// is refused before it starts. azure_foundry is TestAllowedModels_AzureDispatchIntercepted.
func TestAllowedModels_DispatchEveryKind(t *testing.T) {
	kinds := joinKinds()
	gateway := kinds[0]
	gateway.p.ID, gateway.p.UID, gateway.p.BaseURL = "anthropic-gateway", "uid-agw", "https://llm-gw.corp.example/v1"
	gateway.grantHost, gateway.grantName = "llm-gw.corp.example", providerSecretName("uid-agw", providerKeyPart)
	kinds = append(kinds, gateway)
	limit := testModelLimit()
	for _, k := range kinds {
		name := string(k.p.Kind)
		if k.p.BaseURL != "" && k.p.Kind == types.ModelProviderAnthropicAPIKey {
			name = "gateway"
		}
		t.Run(name, func(t *testing.T) {
			plan, ok, st := limitedDispatch(t, k, limit)
			if k.p.Kind == types.ModelProviderBedrockSSO {
				want := fmt.Sprintf(allowedModelsUncheckedMsg, cmp.Or(k.p.Name, k.p.ID))
				if ok || st.failed != want {
					t.Fatalf("ok=%v failed=%q, want the run refused with %q", ok, st.failed, want)
				}
				return
			}
			if !ok {
				t.Fatalf("dispatch refused: %q", st.failed)
			}
			cfg := dispatchedProxyConfig(st.run.ID, plan)
			if cfg.AllowedModels == nil || !slices.Equal(*cfg.AllowedModels, limit.models) {
				t.Fatalf("proxy allowed_models = %v, want %v", cfg.AllowedModels, limit.models)
			}
			if !intercepted(cfg, k.grantHost) {
				t.Errorf("%s is not read by the proxy: mitm_llm=%v mitm_hosts=%v", k.grantHost, cfg.MITMLLM, cfg.MITMHosts)
			}
			for _, vendor := range []string{"api.anthropic.com", "api.openai.com"} {
				if !intercepted(cfg, vendor) {
					t.Errorf("a direct connection to %s is not read by the proxy", vendor)
				}
			}
			if k.p.BaseURL != "" && k.p.Kind != types.ModelProviderAnthropicSubscription && len(cfg.LLMUpstreams) == 0 {
				t.Errorf("the gateway lost its brokered route: %v", cfg.LLMUpstreams)
			}
		})
	}
}

// Without a list the dispatch is what it was: no allowed_models key, and interception only where
// another consumer asked for it.
func TestAllowedModels_EmptyIsNoLimitAtDispatch(t *testing.T) {
	k := joinKinds()[0] // anthropic_api_key: nothing else intercepts it
	plan, ok, st := limitedDispatch(t, k, allowedModelsLimit{})
	if !ok {
		t.Fatalf("dispatch refused: %q", st.failed)
	}
	if plan.allowedModels != nil || plan.mitmLLM || plan.mitmCAKeyPEM != "" {
		t.Errorf("no list: allowed_models=%v mitm_llm=%v ca=%v, want none", plan.allowedModels, plan.mitmLLM, plan.mitmCAKeyPEM != "")
	}
	plan, ok, st = limitedDispatch(t, k, testModelLimit())
	if !ok || !plan.mitmLLM || plan.mitmCAKeyPEM == "" || plan.allowedModels == nil {
		t.Errorf("a list: ok=%v mitm_llm=%v ca=%v allowed_models=%v (failed %q), want interception on", ok, plan.mitmLLM, plan.mitmCAKeyPEM != "", plan.allowedModels, st.failed)
	}
}

// azure_foundry's Messages lane, through the real dispatch: its endpoint is intercepted and the list set.
func TestAllowedModels_AzureDispatchIntercepted(t *testing.T) {
	f := newAzA3Fixture(t)
	f.signIn(t, azUIDAnthropic)
	f.st.run.Agent, f.st.run.ModelProviderID = "claude-code", "foundry-claude"
	site, err := f.site.GetSiteConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	limit := testModelLimit()
	plan, ok := f.srv.resolveLLMInjections(context.Background(), f.st.run, dispatchParams{modelLimit: limit}, &types.RunPolicySpec{},
		map[string]string{}, nil, "http://wardyn-proxy:3128", artifactRedirectPlan{}, false, site, true, false, bedrockCredUngraded())
	if !ok {
		t.Fatalf("dispatch refused the run: hint=%q", f.st.hint)
	}
	cfg := dispatchedProxyConfig(f.st.run.ID, plan)
	if cfg.AllowedModels == nil || !slices.Equal(*cfg.AllowedModels, limit.models) || !intercepted(cfg, azA3Host) {
		t.Errorf("allowed_models=%v mitm_hosts=%v, want the list and %s intercepted", cfg.AllowedModels, cfg.MITMHosts, azA3Host)
	}
}

// The gateway entries name each https upstream once, never a local model or a host already intercepted.
func TestModelGatewayMITMHosts(t *testing.T) {
	got := modelGatewayMITMHosts(map[string]string{
		"api.anthropic.com": "https://gw.corp.example/anthropic",
		"api.openai.com":    "https://gw.corp.example:8443/v1",
	}, nil, nil)
	if !slices.Equal(got, []string{"gw.corp.example:443"}) && !slices.Equal(got, []string{"gw.corp.example:8443"}) {
		t.Errorf("one host twice: %v", got)
	}
	local := map[string]types.LocalModelSettings{"https://models.corp.example": {}}
	if got := modelGatewayMITMHosts(map[string]string{"api.anthropic.com": "https://models.corp.example"}, local, nil); len(got) != 0 {
		t.Errorf("a local model was intercepted: %v", got)
	}
	if got := modelGatewayMITMHosts(map[string]string{"api.anthropic.com": "https://route.corp.example/a"}, nil, []string{"route.corp.example:443"}); len(got) != 0 {
		t.Errorf("a host already intercepted was named again: %v", got)
	}
}

// allowedModelsStore holds the run record in memory.
type allowedModelsStore struct {
	store.Store
	rec map[uuid.UUID][]byte
}

func (s *allowedModelsStore) RunAllowedModels(_ context.Context, id uuid.UUID) ([]byte, error) {
	return s.rec[id], nil
}

func (s *allowedModelsStore) SetRunAllowedModels(_ context.Context, id uuid.UUID, record []byte) error {
	s.rec[id] = record
	return nil
}

// The record the run page reads is the list dispatch handed the proxy and the profile that set it; a
// dispatch with no limit clears it.
func TestAllowedModels_RunRecord(t *testing.T) {
	st := &allowedModelsStore{rec: map[uuid.UUID][]byte{}}
	s := New(Config{Store: st, Audit: &recRecorder{}})
	id, limit := uuid.New(), testModelLimit()
	s.recordAllowedModels(context.Background(), id, limit)
	got := s.runAllowedModels(context.Background(), id)
	if got == nil || !slices.Equal(got.Models, limit.models) || got.Profile.ID != limit.profileID.String() || got.Profile.Name != "Contractors" {
		t.Fatalf("record = %+v", got)
	}
	s.recordAllowedModels(context.Background(), id, allowedModelsLimit{})
	if got := s.runAllowedModels(context.Background(), id); got != nil {
		t.Errorf("a dispatch with no limit left %+v", got)
	}
}

// A revive replays the stored config: the ceiling re-assertion and the encode keep the list, and the local
// revive's eligibility copy carries it.
func TestAllowedModels_ReviveReplaysTheStoredList(t *testing.T) {
	run := types.AgentRun{ID: uuid.New()}
	list := []string{"model-a"}
	cfg := &proxy.Config{RunID: run.ID, AllowedModels: &list, MITMLLM: true, MITMHosts: []string{"gw.corp.example:443"}}
	if _, rerr := reassertProxyCeiling(run, cfg, ownerCeiling{deny: []string{"evil.example"}}, nil); rerr != nil {
		t.Fatalf("re-assertion refused: %+v", rerr)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var back proxy.Config
	if err := json.Unmarshal(raw, &back); err != nil || back.AllowedModels == nil || !slices.Equal(*back.AllowedModels, list) || !back.MITMLLM {
		t.Fatalf("revived config allowed_models=%v mitm_llm=%v (%v)", back.AllowedModels, back.MITMLLM, err)
	}
	if pc := localStoredSpec(*cfg).ProxyConfig; pc.AllowedModels == nil || !slices.Equal(*pc.AllowedModels, list) {
		t.Errorf("the local revive's copy dropped the list: %v", pc.AllowedModels)
	}
}
