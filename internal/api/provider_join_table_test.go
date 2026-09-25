// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The joined dispatch (#551), every kind through every door: create and Review
// answer the same refusal (#532's wire contract), and dispatch strips every
// legacy model credential before the kind's arm authors its one grant.

const (
	joinCreateOwner = "sub-admit-admin" // admitAdminSession's subject
	joinProxyURL    = "http://wardyn-proxy:3128"
	joinPlaceholder = "wardyn-proxy-injected"
	joinAnyValue    = "\x00set" // env expectation: present and non-empty, value opaque
)

// joinKind is one provider kind's fixture: its record, the agent it serves,
// how its owner's own credential is stored, and what its arm authors and sets.
type joinKind struct {
	p         types.ModelProvider
	agent     string
	store     func(st secretstore.Store, p types.ModelProvider) error
	credState string // the state its credential refusal names
	grantHost string
	grantName string
	env       map[string]string // each key must hold exactly this value (joinAnyValue: any non-empty)
	noEnv     []string          // each key must be absent
}

func joinKinds() []joinKind {
	later := time.Now().Add(8 * time.Hour)
	putKey := func(st secretstore.Store, p types.ModelProvider) error {
		return st.Put(context.Background(), providerSecretName(p.UID, providerKeyPart), []byte("sk-own-"+p.UID))
	}
	endpoint := types.ModelProvider{ID: "corp-gw", UID: "uid-gw", Kind: types.ModelProviderCustomEndpoint,
		BaseURL: "https://gw.corp.example", Auth: &types.ProviderAuth{Header: "Authorization", Format: "Bearer %s"},
		Harnesses: []types.ProviderHarness{{Harness: "claude-code", Path: "/anthropic", Model: "m-gw"}}}
	sub := subProvider("claude")
	sub.UID = "uid-sub"
	sso, bearer := brSSOProvider(), brBearerProvider()
	sso.UID, bearer.UID = "uid-sso", "uid-bearer"
	bedrockEnv := map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1", "AWS_REGION": "us-west-2", "ANTHROPIC_MODEL": brModel}
	return []joinKind{
		{p: types.ModelProvider{ID: "anthropic-key", UID: "uid-ak", Kind: types.ModelProviderAnthropicAPIKey,
			Harnesses: []types.ProviderHarness{{Harness: "claude-code", Model: "m-ak"}}},
			agent: "claude-code", store: putKey, credState: mpRunNoKey,
			grantHost: "api.anthropic.com", grantName: providerSecretName("uid-ak", providerKeyPart),
			env:   map[string]string{"ANTHROPIC_API_KEY": joinPlaceholder, "ANTHROPIC_MODEL": "m-ak"},
			noEnv: []string{"ANTHROPIC_BASE_URL", "CLAUDE_CODE_USE_BEDROCK", "WARDYN_CLAUDE_MANAGED_B64"}},
		{p: types.ModelProvider{ID: "openai-key", UID: "uid-ok", Kind: types.ModelProviderOpenAIAPIKey,
			Harnesses: []types.ProviderHarness{{Harness: "codex-cli"}}},
			agent: "codex-cli", store: putKey, credState: mpRunNoKey,
			grantHost: "api.openai.com", grantName: providerSecretName("uid-ok", providerKeyPart),
			env:   map[string]string{"OPENAI_API_KEY": joinPlaceholder, "OPENAI_BASE_URL": joinProxyURL + "/wardyn/llm/openai"},
			noEnv: []string{"ANTHROPIC_API_KEY"}},
		{p: endpoint, agent: "claude-code", store: putKey, credState: mpRunNoToken,
			grantHost: "gw.corp.example", grantName: providerSecretName("uid-gw", providerKeyPart),
			env:   map[string]string{"ANTHROPIC_API_KEY": joinPlaceholder, "ANTHROPIC_MODEL": "m-gw"},
			noEnv: []string{"ANTHROPIC_BASE_URL", "CLAUDE_CODE_USE_BEDROCK"}},
		{p: sub, agent: "claude-code", credState: mpSubNotSignedIn,
			store: func(st secretstore.Store, p types.ModelProvider) error {
				return st.Put(context.Background(), providerSecretName(p.UID, providerOAuthPart), subBlob("sk-ant-oat-own"))
			},
			grantHost: "api.anthropic.com", grantName: providerSecretName("uid-sub", providerOAuthPart),
			env: map[string]string{"ANTHROPIC_BASE_URL": "https://api.anthropic.com", "CLAUDE_CONFIG_DIR": "/home/agent/.claude-run",
				"WARDYN_CLAUDE_MANAGED_B64": joinAnyValue, "ANTHROPIC_MODEL": "claude-opus-4-1"},
			noEnv: []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_USE_BEDROCK"}},
		{p: sso, agent: "claude-code", credState: mpBRNotSignedIn,
			store: func(st secretstore.Store, p types.ModelProvider) error {
				return st.Put(context.Background(), providerSecretName(p.UID, providerSSOPart), brBlob(brOwnerToken, "123456789012", "BedrockUser", later))
			},
			grantHost: "portal.sso.us-east-1.amazonaws.com", grantName: types.AWSSSOAccessTokenSecret,
			env: bedrockEnv, noEnv: []string{"ANTHROPIC_API_KEY", "AWS_BEARER_TOKEN_BEDROCK", "WARDYN_CLAUDE_MANAGED_B64"}},
		{p: bearer, agent: "claude-code", store: putKey, credState: mpRunNoKey,
			grantHost: "vpce-1.bedrock-runtime.us-west-2.vpce.amazonaws.com", grantName: providerSecretName("uid-bearer", providerKeyPart),
			env: func() map[string]string {
				e := map[string]string{"AWS_BEARER_TOKEN_BEDROCK": joinPlaceholder, "ANTHROPIC_BEDROCK_BASE_URL": brBaseURL}
				for k, v := range bedrockEnv {
					e[k] = v
				}
				return e
			}(),
			noEnv: []string{"ANTHROPIC_API_KEY", "WARDYN_CLAUDE_MANAGED_B64"}},
	}
}

// joinScenario is one row of the door × state matrix, applied to every kind.
type joinScenario struct {
	name    string
	mutate  func(*types.ModelProvider, string) // the record, given the kind's agent
	stored  bool                               // the owner's own credential is stored
	wedged  bool                               // the secret store fails every read
	siteErr bool                               // the provider block cannot be read
	chosen  bool                               // the run chose the provider (dispatch) / the request names it (create)
}

func joinScenarios() []joinScenario {
	return []joinScenario{
		{name: "happy", stored: true, chosen: true},
		{name: "credential-missing", chosen: true},
		{name: "off", stored: true, chosen: true, mutate: func(p *types.ModelProvider, _ string) { p.Disabled = true }},
		{name: "not-serving", stored: true, chosen: true, mutate: func(p *types.ModelProvider, agent string) {
			p.Harnesses = []types.ProviderHarness{{Harness: map[string]string{"claude-code": "codex-cli", "codex-cli": "claude-code"}[agent]}}
		}},
		{name: "provider-unreadable", stored: true, wedged: true, chosen: true},
		{name: "block-unreadable", stored: true, siteErr: true, chosen: true},
		{name: "no-provider-chosen-under-block", stored: true},
	}
}

// joinRefusal is what a refused run must answer: the sentence, the provider and
// kind it names ("" when it names none), and whether it is a credential one.
type joinRefusal struct {
	msg, provider string
	kind          types.ModelProviderKind
	credential    bool
}

// expectedRefusal is the refusal k's run gets under sc; ok=false: none.
func (k joinKind) expectedRefusal(sc joinScenario) (joinRefusal, bool) {
	id, kind := k.p.ID, k.p.Kind
	switch sc.name {
	case "credential-missing":
		return joinRefusal{connectDenial(id, k.credState).msg, id, kind, true}, true
	case "off":
		return joinRefusal{stateDenial(id, mpRunStateOff, mpRunRemedy).msg, id, kind, false}, true
	case "not-serving":
		return joinRefusal{stateDenial(id, fmt.Sprintf(mpRunStateNotServing, k.agent), mpRunRemedy).msg, id, kind, false}, true
	case "provider-unreadable":
		return joinRefusal{msg: providerReadFailed(k.p), provider: id, kind: kind}, true
	case "block-unreadable":
		return joinRefusal{msg: mpRunUnreadable, provider: id}, true
	}
	return joinRefusal{}, false
}

func (k joinKind) record(sc joinScenario) types.ModelProvider {
	p := k.p
	p.Harnesses = slices.Clone(p.Harnesses)
	if sc.mutate != nil {
		sc.mutate(&p, k.agent)
	}
	return p
}

// joinLegacy is every model credential a stored, inline or recorded policy
// could hand a provider run, keyed by why it must go; plus one injection
// nothing may touch. It names the arm's own secret and host, so an arm whose
// grant ran before the strip would lose it.
func (k joinKind) joinLegacy(withLaneHost bool) (legacy []runner.InjectionGrant, unrelated runner.InjectionGrant) {
	conv, _ := agentLLMProvider(k.agent)
	ig := func(host, secret string) runner.InjectionGrant {
		return runner.InjectionGrant{GrantID: uuid.New(), Rule: egress.InjectionRule{Host: host, Header: "x-api-key", Format: "%s", SecretName: secret}}
	}
	otherVendor := map[string]string{"api.anthropic.com": "api.openai.com", "api.openai.com": "api.anthropic.com"}[conv.host]
	legacy = []runner.InjectionGrant{
		ig(conv.host, "operator-model-key"),
		ig(otherVendor, "operator-other-vendor-key"), // still a model credential on the other harness's vendor host
		ig("api.anthropic.com", types.SubscriptionOAuthSecret),
		ig("api.anthropic.com", types.ManagedOAuthSecret),
		ig("portal.sso.us-east-1.amazonaws.com", types.AWSSSOAccessTokenSecret),
		ig("bedrock-runtime.us-west-2.amazonaws.com", bedrockAPIKeySecret),
		ig("evil.example", providerSecretName(k.p.UID, providerKeyPart)),
		ig("evil.example", providerSecretName(k.p.UID, providerOAuthPart)),
		ig("evil.example", k.grantName),
	}
	if withLaneHost {
		legacy = append(legacy, ig(k.grantHost, "operator-secret-on-the-arms-host"))
	}
	return legacy, ig("registry.corp.example", "npm-token")
}

// joinDispatch runs the real LLM phase for k under sc on a fresh harness.
func joinDispatch(t *testing.T, k joinKind, sc joinScenario, grants ...types.GrantSpec) (dispatchLLMPlan, bool, *subStore, *harness, types.RunPolicySpec, map[string]string, []runner.InjectionGrant, runner.InjectionGrant) {
	t.Helper()
	p := k.record(sc)
	h, st, sec := subHarness(t, p)
	h.srv.cfg.AWSSSOProxyInject = true
	st.run.Agent = k.agent
	if !sc.chosen {
		st.run.ModelProviderID = ""
	}
	// The operator's row under every name the owner's could be read from: it
	// must never stand in.
	for _, part := range []string{providerKeyPart, providerOAuthPart, providerSSOPart} {
		sec.m[providerSecretName(p.UID, part)] = []byte("operator-row-must-never-serve")
	}
	// subHarness stores the owner's own sign-in for p; this matrix decides
	// what the owner holds.
	for _, part := range []string{providerKeyPart, providerOAuthPart, providerSSOPart} {
		_ = sec.For(subOwner).Delete(context.Background(), providerSecretName(p.UID, part))
	}
	if sc.stored {
		if err := k.store(sec.For(subOwner), p); err != nil {
			t.Fatal(err)
		}
	}
	if sc.wedged {
		h.srv.cfg.Secrets = wedgedSecrets{err: errors.New("age: no identity matched")}
	}
	legacy, unrelated := k.joinLegacy(sc.chosen)
	policy := types.RunPolicySpec{EligibleGrants: grants, WorkspaceMounts: []types.WorkspaceMount{{Source: "/home/op/.claude", Target: claudeCredTarget}}}
	env := map[string]string{}
	plan, ok := h.srv.resolveLLMInjections(context.Background(), st.run, dispatchParams{}, &policy, env,
		append([]runner.InjectionGrant{unrelated}, legacy...), joinProxyURL, artifactRedirectPlan{}, false, st.site, !sc.siteErr,
		false, bedrockCredUngraded())
	return plan, ok, st, h, policy, env, legacy, unrelated
}

// TestProviderJoin_DispatchEveryKind is the dispatch half of the matrix: the
// exact grant set, env and strip on a launch; the exact run.create failure row
// on a refusal; nothing legacy under a block that serves the run no provider.
func TestProviderJoin_DispatchEveryKind(t *testing.T) {
	for _, k := range joinKinds() {
		for _, sc := range joinScenarios() {
			t.Run(string(k.p.Kind)+"/"+sc.name, func(t *testing.T) {
				plan, ok, st, h, policy, env, legacy, unrelated := joinDispatch(t, k, sc)
				if want, refused := k.expectedRefusal(sc); refused {
					assertJoinDispatchRefusal(t, h, st, ok, plan, want)
					return
				}
				if !ok {
					t.Fatalf("dispatch refused: %q", st.failed)
				}
				assertJoinStripped(t, h, plan, legacy, unrelated, len(st.grants))
				if len(policy.WorkspaceMounts) != 0 {
					t.Errorf("the operator's ~/.claude mount survived: %+v", policy.WorkspaceMounts)
				}
				if !sc.chosen {
					assertJoinNoProvider(t, k, st, plan, env)
					return
				}
				assertJoinLaunch(t, k, st, plan, env)
			})
		}
	}
}

func assertJoinDispatchRefusal(t *testing.T, h *harness, st *subStore, ok bool, plan dispatchLLMPlan, want joinRefusal) {
	t.Helper()
	if ok || len(plan.injections) != 0 || len(st.grants) != 0 {
		t.Fatalf("a refused run dispatched (ok=%v) with injections %v and grants %d", ok, plan.injections, len(st.grants))
	}
	if st.failed != want.msg {
		t.Errorf("failure hint = %q\nwant          %q", st.failed, want.msg)
	}
	var rows []map[string]any
	for _, ev := range h.audit.snapshot() {
		if ev.Action == "run.create" && ev.Outcome == "failure" {
			var d map[string]any
			_ = json.Unmarshal(ev.Data, &d)
			rows = append(rows, d)
		}
	}
	if len(rows) != 1 {
		t.Fatalf("run.create failure rows = %v, want exactly one", rows)
	}
	wantRow := map[string]any{"error": want.msg, "provider": want.provider}
	if want.kind != "" {
		wantRow["kind"], wantRow["mechanism"] = string(want.kind), string(want.kind)
	}
	if want.credential {
		wantRow["reason"] = llmRefusalAuditReason
	}
	if got, _ := json.Marshal(rows[0]); string(got) != string(mustJSON(wantRow)) {
		t.Errorf("run.create failure row = %s\nwant                   %s", got, mustJSON(wantRow))
	}
}

// assertJoinStripped: every legacy injection is gone, each audited once as the
// strip's, and the unrelated one leads the list the arm then appended to.
func assertJoinStripped(t *testing.T, h *harness, plan dispatchLLMPlan, legacy []runner.InjectionGrant, unrelated runner.InjectionGrant, authored int) {
	t.Helper()
	if len(plan.injections) != 1+authored || plan.injections[0].GrantID != unrelated.GrantID {
		t.Fatalf("injections = %+v, want the unrelated one then the %d the arm authored", plan.injections, authored)
	}
	dropped := map[string]int{}
	for _, ev := range h.audit.snapshot() {
		if ev.Action == "run.injection.dropped" {
			var d struct{ Reason string }
			_ = json.Unmarshal(ev.Data, &d)
			dropped[ev.Target+"/"+d.Reason]++
		}
	}
	for _, ig := range legacy {
		if dropped[ig.GrantID.String()+"/model_credential_not_provider_authored"] != 1 {
			t.Errorf("legacy injection %s on %s was not dropped once by the strip (drops %v)", ig.Rule.SecretName, ig.Rule.Host, dropped)
		}
	}
}

func assertJoinLaunch(t *testing.T, k joinKind, st *subStore, plan dispatchLLMPlan, env map[string]string) {
	t.Helper()
	if len(st.grants) != 1 {
		t.Fatalf("grants authored = %d, want exactly the arm's one", len(st.grants))
	}
	if r := plan.injections[1].Rule; r.SecretName != k.grantName || r.Host != k.grantHost || plan.injections[1].GrantID != st.grants[0].ID {
		t.Errorf("authored injection = %+v, want %s on %s", r, k.grantName, k.grantHost)
	}
	var scope struct {
		Snapshot map[string]any `json:"snapshot"`
	}
	_ = json.Unmarshal(st.grants[0].Spec.Scope, &scope)
	if scope.Snapshot["owner_subject"] != subOwner || scope.Snapshot["provider_uid"] != k.p.UID {
		t.Errorf("grant record = %v, want the owner %s and provider %s", scope.Snapshot, subOwner, k.p.UID)
	}
	if plan.llmUnavailableDetail != "" {
		t.Errorf("detail = %q, want none on a credentialed run", plan.llmUnavailableDetail)
	}
	assertJoinEnv(t, env, k.env, k.noEnv)
}

func assertJoinNoProvider(t *testing.T, k joinKind, st *subStore, plan dispatchLLMPlan, env map[string]string) {
	t.Helper()
	if len(st.grants) != 0 {
		t.Fatalf("a run no provider serves got %d grants", len(st.grants))
	}
	if plan.llmUnavailableDetail != mpNoProviderDetail || plan.llmUpstreams != nil || plan.llm.provider != nil {
		t.Errorf("detail %q upstreams %v provider %v, want the no-provider detail and nothing else", plan.llmUnavailableDetail, plan.llmUpstreams, plan.llm.provider)
	}
	placeholder := map[string]map[string]string{
		"claude-code": {"ANTHROPIC_API_KEY": joinPlaceholder},
		"codex-cli":   {"OPENAI_API_KEY": joinPlaceholder, "OPENAI_BASE_URL": joinProxyURL + "/wardyn/llm/openai"},
	}[k.agent]
	assertJoinEnv(t, env, placeholder, []string{"CLAUDE_CODE_USE_BEDROCK", "WARDYN_CLAUDE_MANAGED_B64", "AWS_BEARER_TOKEN_BEDROCK", "ANTHROPIC_BASE_URL"})
}

func assertJoinEnv(t *testing.T, env, want map[string]string, absent []string) {
	t.Helper()
	for key, v := range want {
		if got, ok := env[key]; !ok || (v == joinAnyValue && got == "") || (v != joinAnyValue && got != v) {
			t.Errorf("env[%s] = %q (set=%v), want %q", key, got, ok, v)
		}
	}
	for _, key := range absent {
		if _, ok := env[key]; ok {
			t.Errorf("env[%s] = %q, want it unset", key, env[key])
		}
	}
}

// TestProviderJoin_DoorsEveryKind is the create/Review half of the matrix:
// both doors answer every kind's refusal identically, carrying #532's
// provider, kind and — only on a credential refusal — reason; an unreadable
// credential or block answers 503 with the sentence alone.
func TestProviderJoin_DoorsEveryKind(t *testing.T) {
	for _, k := range joinKinds() {
		for _, sc := range joinScenarios() {
			t.Run(string(k.p.Kind)+"/"+sc.name, func(t *testing.T) {
				want, refused := k.expectedRefusal(sc)
				body := fmt.Sprintf(`{"agent":%q,"task":"t"}`, k.agent)
				if sc.chosen {
					body = fmt.Sprintf(`{"agent":%q,"task":"t","model_provider":%q}`, k.agent, k.p.ID)
				}
				for _, path := range []string{"/api/v1/runs/preflight", "/api/v1/runs"} {
					w := joinCreate(t, k, sc, path, body)
					var got errorBody
					_ = json.Unmarshal(w.Body.Bytes(), &got)
					if !refused {
						wantCode := map[string]int{"/api/v1/runs": http.StatusCreated, "/api/v1/runs/preflight": http.StatusOK}[path]
						if w.Code != wantCode {
							t.Fatalf("%s = %d %s, want %d", path, w.Code, w.Body.String(), wantCode)
						}
						var resp createRunResponse
						_ = json.Unmarshal(w.Body.Bytes(), &resp)
						if !sc.chosen && path == "/api/v1/runs" && !slices.Contains(resp.Warnings, fmt.Sprintf(mpAccessNoProvider, k.agent)) {
							t.Errorf("%s: warnings %q, want the no-provider model-access note", path, resp.Warnings)
						}
						continue
					}
					wantBody := errorBody{Error: want.msg}
					wantCode := http.StatusServiceUnavailable
					if sc.name != "provider-unreadable" && sc.name != "block-unreadable" {
						wantCode, wantBody.Provider, wantBody.Kind = http.StatusUnprocessableEntity, want.provider, string(want.kind)
						if want.credential {
							wantBody.Reason = llmRefusalAuditReason
						}
					}
					if w.Code != wantCode || got != wantBody {
						t.Errorf("%s = %d %s\nwant %d %+v", path, w.Code, w.Body.String(), wantCode, wantBody)
					}
				}
			})
		}
	}
}

// joinCreate drives path for k under sc as an admin session. An unreadable
// block is driven at enforceRunModelProvider, which both doors call: every
// earlier create step reads the same site config, so the door would never see
// this read fail alone.
// policies are stored first, for a body that names one by policy_id.
func joinCreate(t *testing.T, k joinKind, sc joinScenario, path, body string, policies ...types.RunPolicy) *httptest.ResponseRecorder {
	t.Helper()
	p := k.record(sc)
	if !sc.chosen {
		// A block that serves no provider for this agent.
		p.Harnesses = []types.ProviderHarness{{Harness: map[string]string{"claude-code": "codex-cli", "codex-cli": "claude-code"}[k.agent]}}
	}
	srv := providerRunFixture(t, types.SiteConfig{ModelProviders: providerBlock(p)}, &capStore{}, nil)
	for _, pol := range policies {
		srv.cfg.Store.(*integStore).policies[pol.ID] = pol
	}
	srv.cfg.AgentImages = map[string]string{"claude-code": "wardyn/agent-claude-code:local"}
	srv.cfg.Runner = nil
	sec := srv.cfg.Secrets.(*memSecrets)
	for _, part := range []string{providerKeyPart, providerOAuthPart, providerSSOPart} {
		sec.m[providerSecretName(p.UID, part)] = []byte("operator-row-must-never-serve")
	}
	if sc.stored {
		if err := k.store(sec.For(joinCreateOwner), p); err != nil {
			t.Fatal(err)
		}
	}
	if sc.wedged {
		srv.cfg.Secrets = wedgedSecrets{err: errors.New("age: no identity matched")}
	}
	if sc.siteErr {
		srv.cfg.Store = siteErrStore{srv.cfg.Store.(*integStore)}
		var req createRunRequest
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		if _, ok := srv.enforceRunModelProvider(w, httptest.NewRequest(http.MethodPost, path, nil), req, types.RunPolicySpec{}, nil); ok {
			t.Fatal("an unreadable provider block admitted the run")
		}
		return w
	}
	return doSSO(t, srv, http.MethodPost, path, admitAdminSession(t), body)
}
