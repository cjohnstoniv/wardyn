// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/test/entrafake"
)

// The azure_foundry dispatch arm and its sink, against test/entrafake and the in-memory doubles: an Azure
// run holds no key, the proxy gets a gate, a MITM entry and a channel host, and the person's own sign-in
// is redeemed for the audience the dispatch snapshot named. Nothing here reaches a real tenant.

const (
	azA3Deployment = "claude-deploy"
	azA3Fast       = "claude-fast"
	azA3Responses  = "responses-deploy"
	azA3Host       = "res.services.ai.azure.com"
	azA3OpenAIHost = "res.openai.azure.com"
)

// azA3Store is the fixture's run, grant and approval store: what dispatch writes and the sink reads back.
type azA3Store struct {
	*azureSiteStore
	approvals *fakeApprovals
	mu        sync.Mutex
	run       types.AgentRun
	grants    []types.CredentialGrant
	hint      string
	failedTo  types.RunState
}

func (s *azA3Store) GetRun(context.Context, uuid.UUID) (types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.run, nil
}

func (s *azA3Store) CreateGrant(_ context.Context, g types.CredentialGrant) (types.CredentialGrant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grants = append(s.grants, g)
	return g, nil
}

func (s *azA3Store) ListGrantsByRun(context.Context, uuid.UUID) ([]types.CredentialGrant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.grants), nil
}

func (s *azA3Store) UpdateRunStateIf(_ context.Context, _ uuid.UUID, _, to types.RunState) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failedTo = to
	return true, nil
}

func (s *azA3Store) SetRunFailureHint(_ context.Context, _ uuid.UUID, hint string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hint = hint
	return nil
}

func (s *azA3Store) ResolveReauthApproval(_ context.Context, id uuid.UUID, d types.ApprovalDecision, _ types.AuditEvent) (types.ApprovalRequest, error) {
	s.approvals.mu.Lock()
	defer s.approvals.mu.Unlock()
	ap := s.approvals.byID[id]
	if ap.State != types.ApprovalPending || ap.Kind != types.ApprovalCredentialReauth {
		return types.ApprovalRequest{}, errStoreNotFound
	}
	ap.State, ap.DecidedBy = d.State, d.DecidedBy
	s.approvals.byID[id] = ap
	return ap, nil
}

type azA3Fixture struct {
	*azureFixture
	st    *azA3Store
	owner string
}

func newAzA3Fixture(t *testing.T) *azA3Fixture {
	t.Helper()
	f := newAzureFixture(t)
	f.site.set(func(b *types.ModelProviders) {
		for i := range b.Providers {
			switch b.Providers[i].UID {
			case azUIDAnthropic:
				b.Providers[i].Harnesses = []types.ProviderHarness{{Harness: "claude-code", Model: azA3Deployment, FastModel: azA3Fast}}
			case azUIDOpenAI:
				b.Providers[i].Harnesses = []types.ProviderHarness{{Harness: "codex-cli", Model: azA3Responses}}
			}
		}
	})
	fa := newFakeApprovals()
	st := &azA3Store{azureSiteStore: f.site, approvals: fa}
	f.srv.cfg.Store, f.srv.cfg.Approvals = st, fa
	a := &azA3Fixture{azureFixture: f, st: st, owner: f.fake.Subject()}
	st.run = types.AgentRun{ID: uuid.New(), CreatedBy: a.owner}
	return a
}

// signIn captures the owner's sign-in for the provider with uid.
func (f *azA3Fixture) signIn(t *testing.T, uid string) {
	t.Helper()
	if w := f.capture(t, f.owner, uid); w.Code != http.StatusFound || strings.Contains(w.Header().Get("Location"), "error") {
		t.Fatalf("sign-in for %s: %d %s", uid, w.Code, w.Header().Get("Location"))
	}
}

// dispatch runs the REAL LLM phase for a run on agent that chose providerID.
func (f *azA3Fixture) dispatch(t *testing.T, agent, providerID string) (dispatchLLMPlan, map[string]string, *types.RunPolicySpec, bool) {
	t.Helper()
	f.st.run.Agent, f.st.run.ModelProviderID = agent, providerID
	site, err := f.site.GetSiteConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	env, policy := map[string]string{}, &types.RunPolicySpec{}
	plan, ok := f.srv.resolveLLMInjections(context.Background(), f.st.run, dispatchParams{}, policy, env, nil,
		"http://wardyn-proxy:3128", artifactRedirectPlan{}, false, site, true, false, bedrockCredUngraded())
	return plan, env, policy, ok
}

// resolve is one sink resolve for the grant dispatch authored, as the proxy makes it.
func (f *azA3Fixture) resolve(t *testing.T, plan dispatchLLMPlan, query string) *httptest.ResponseRecorder {
	t.Helper()
	if len(plan.injections) != 1 || len(f.st.grants) == 0 {
		t.Fatalf("want one authored injection and grant, got %d and %d", len(plan.injections), len(f.st.grants))
	}
	rule := plan.injections[0].Rule
	return f.resolveRule(rule, plan.injections[0].GrantID, f.owner, query)
}

func (f *azA3Fixture) resolveRule(rule egress.InjectionRule, grantID uuid.UUID, sub, query string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/internal/injection/"+grantID.String()+query, nil)
	f.srv.resolveAzureFoundryInjection(w, r,
		&identity.Claims{RunID: f.st.run.ID, Sub: sub, SPIFFEID: "spiffe://wardyn.local/run"},
		broker.Minted{JTI: "jti-" + uuid.NewString(), Injection: &rule}, grantID)
	return w
}

func scopeOf(t *testing.T, g types.CredentialGrant) map[string]any {
	t.Helper()
	var sc map[string]any
	if err := json.Unmarshal(g.Spec.Scope, &sc); err != nil {
		t.Fatal(err)
	}
	return sc
}

// The Messages harness: Foundry's own switch and token variable, every alias inside the pinned set, and no
// key variable of any other lane. The proxy gets the grant, the MITM entry, the gate and the channel host,
// and nothing about the host is an LLMUpstreams entry.
func TestAzureDispatch_MessagesHarnessWiring(t *testing.T) {
	f := newAzA3Fixture(t)
	f.signIn(t, azUIDAnthropic)
	plan, env, policy, ok := f.dispatch(t, "claude-code", "foundry-claude")
	if !ok {
		t.Fatalf("dispatch refused the run: hint=%q failedTo=%s", f.st.hint, f.st.failedTo)
	}
	want := map[string]string{
		"CLAUDE_CODE_USE_FOUNDRY":        "1",
		"ANTHROPIC_FOUNDRY_BASE_URL":     "https://" + azA3Host + "/anthropic",
		"ANTHROPIC_FOUNDRY_AUTH_TOKEN":   "wardyn-proxy-injected",
		"ANTHROPIC_MODEL":                azA3Deployment,
		"ANTHROPIC_DEFAULT_OPUS_MODEL":   azA3Deployment,
		"ANTHROPIC_DEFAULT_SONNET_MODEL": azA3Deployment,
		"ANTHROPIC_DEFAULT_HAIKU_MODEL":  azA3Fast,
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("env[%s] = %q, want %q", k, env[k], v)
		}
	}
	for _, k := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "OPENAI_API_KEY", "OPENAI_BASE_URL", "WARDYN_CODEX_BASE_URL"} {
		if v, set := env[k]; set {
			t.Errorf("env[%s] = %q on an Azure Messages run, want it absent", k, v)
		}
	}
	if plan.llmUpstreams != nil {
		t.Errorf("llmUpstreams = %v: the Azure host must never be a gateway upstream", plan.llmUpstreams)
	}
	entry := azA3Host + ":443"
	if !slices.Equal(plan.azure.mitmHosts, []string{entry}) || !slices.Contains(policy.AllowedDomains, entry) {
		t.Errorf("mitm hosts %v, allowlist %v: want %q in both", plan.azure.mitmHosts, policy.AllowedDomains, entry)
	}
	wantGates := []proxy.AzureGateConfig{{Host: azA3Host, Route: types.AzureRouteAnthropic, Models: []string{azA3Deployment, azA3Fast}}}
	if len(plan.azure.gates) != 1 || plan.azure.gates[0].Host != wantGates[0].Host || plan.azure.gates[0].Route != wantGates[0].Route ||
		!slices.Equal(plan.azure.gates[0].Models, wantGates[0].Models) {
		t.Errorf("gates = %+v, want %+v", plan.azure.gates, wantGates)
	}
	if plan.azure.channelHosts[azA3Host] != "api.anthropic.com" || len(plan.azure.channelHosts) != 1 {
		t.Errorf("channel hosts = %v, want the host as the Anthropic dialect", plan.azure.channelHosts)
	}
	// A CA with no other MITM consumer in the run: the azure lane alone asked for it.
	if plan.mitmCACertPEM == "" || plan.mitmCAKeyPEM == "" || plan.mitmLLM || len(plan.bedrockMITMHosts) != 0 {
		t.Errorf("CA cert=%t key=%t mitmLLM=%t bedrock mitm=%v, want a CA and no other consumer",
			plan.mitmCACertPEM != "", plan.mitmCAKeyPEM != "", plan.mitmLLM, plan.bedrockMITMHosts)
	}

	// The one grant: bare host, TLS only, the route's own pins, the owner's sealed sign-in, and the snapshot.
	if len(plan.injections) != 1 || len(f.st.grants) != 1 {
		t.Fatalf("injections=%d grants=%d, want one each", len(plan.injections), len(f.st.grants))
	}
	rule := plan.injections[0].Rule
	if rule.Host != azA3Host || !rule.RequireTLS || rule.SecretName != providerSecretName(azUIDAnthropic, providerEntraPart) ||
		!slices.Equal(rule.PinRoutes, proxy.AzureRoutePins(types.AzureRouteAnthropic)) || rule.PinPath != "" {
		t.Errorf("rule = %+v", rule)
	}
	snap, _ := scopeOf(t, f.st.grants[0])["snapshot"].(map[string]any)
	if snap["provider_uid"] != azUIDAnthropic || snap["owner_subject"] != f.owner || snap["audience"] != azureFoundryAudience {
		t.Errorf("snapshot = %v, want the provider, the owner and the Foundry audience", snap)
	}
}

// The small-model alias falls back to the main deployment when the row sets no fast one.
func TestAzureDispatch_HaikuAliasFallsBackToTheDeployment(t *testing.T) {
	f := newAzA3Fixture(t)
	f.site.set(func(b *types.ModelProviders) { b.Providers[0].Harnesses[0].FastModel = "" })
	f.signIn(t, azUIDAnthropic)
	plan, env, _, ok := f.dispatch(t, "claude-code", "foundry-claude")
	if !ok {
		t.Fatalf("dispatch refused: %q", f.st.hint)
	}
	if env["ANTHROPIC_DEFAULT_HAIKU_MODEL"] != azA3Deployment || !slices.Equal(plan.azure.gates[0].Models, []string{azA3Deployment}) {
		t.Errorf("haiku = %q, pinned = %v, want the one deployment", env["ANTHROPIC_DEFAULT_HAIKU_MODEL"], plan.azure.gates[0].Models)
	}
}

// The Responses harness: its own route, deployment and sentinel through the variables the image's prep
// turns into config.toml, and neither of the OPENAI_* pair that would point it at the brokered route.
func TestAzureDispatch_ResponsesHarnessWiring(t *testing.T) {
	f := newAzA3Fixture(t)
	f.signIn(t, azUIDOpenAI)
	plan, env, policy, ok := f.dispatch(t, "codex-cli", "foundry-openai")
	if !ok {
		t.Fatalf("dispatch refused the run: hint=%q failedTo=%s", f.st.hint, f.st.failedTo)
	}
	want := map[string]string{
		"WARDYN_CODEX_BASE_URL": "https://" + azA3OpenAIHost + "/openai/v1",
		"WARDYN_CODEX_MODEL":    azA3Responses,
		"CODEX_API_KEY":         "wardyn-proxy-injected",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("env[%s] = %q, want %q", k, env[k], v)
		}
	}
	for _, k := range []string{"OPENAI_API_KEY", "OPENAI_BASE_URL", "ANTHROPIC_API_KEY", "CLAUDE_CODE_USE_FOUNDRY"} {
		if v, set := env[k]; set {
			t.Errorf("env[%s] = %q on an Azure Responses run, want it absent", k, v)
		}
	}
	if len(plan.azure.gates) != 1 || plan.azure.gates[0].Route != types.AzureRouteOpenAIV1 || !slices.Equal(plan.azure.gates[0].Models, []string{azA3Responses}) {
		t.Errorf("gates = %+v", plan.azure.gates)
	}
	if plan.azure.channelHosts[azA3OpenAIHost] != "api.openai.com" {
		t.Errorf("channel hosts = %v, want the host as the OpenAI dialect", plan.azure.channelHosts)
	}
	if !slices.Contains(policy.AllowedDomains, azA3OpenAIHost+":443") || plan.llmUpstreams != nil {
		t.Errorf("allowlist %v upstreams %v", policy.AllowedDomains, plan.llmUpstreams)
	}
	if aud := scopeOf(t, f.st.grants[0])["snapshot"].(map[string]any)["audience"]; aud != azureCognitiveServicesAudience {
		t.Errorf("snapshot audience = %v, want the Cognitive Services audience", aud)
	}
}

// A run on any other kind carries none of the Azure lane: the proxy config keys stay empty.
func TestAzureDispatch_OtherKindsCarryNoAzureLane(t *testing.T) {
	f := newAzA3Fixture(t)
	f.signIn(t, azUIDAnthropic)
	f.site.set(func(b *types.ModelProviders) {
		b.Providers[2].Harnesses = []types.ProviderHarness{{Harness: "claude-code", Model: "claude-x"}}
	})
	f.st.run.Agent, f.st.run.ModelProviderID = "claude-code", "plain"
	f.srv.cfg.Secrets.For(f.owner).Put(context.Background(), providerSecretName(azUIDOther, providerKeyPart), []byte("sk-own")) //nolint:errcheck // memSecrets
	site, _ := f.site.GetSiteConfig(context.Background())
	plan, ok := f.srv.resolveLLMInjections(context.Background(), f.st.run, dispatchParams{}, &types.RunPolicySpec{}, map[string]string{}, nil,
		"http://wardyn-proxy:3128", artifactRedirectPlan{}, false, site, true, false, bedrockCredUngraded())
	if !ok {
		t.Fatalf("dispatch refused a plain key provider: %q", f.st.hint)
	}
	if len(plan.azure.gates) != 0 || len(plan.azure.mitmHosts) != 0 || len(plan.azure.channelHosts) != 0 || plan.mitmCACertPEM != "" {
		t.Errorf("a key provider run carries an Azure lane or a CA: %+v", plan)
	}
}

// Liveness: a person with no sign-in, or one a renewal has found ended, is refused with a sign-in remedy;
// a captured one passes without spending a refresh token.
func TestAzureLiveness(t *testing.T) {
	f := newAzA3Fixture(t)
	site, _ := f.site.GetSiteConfig(context.Background())
	p, _ := modelProviderByID(site.ModelProviders, "foundry-claude")
	live := func() providerDenial {
		d, err := f.srv.providerAzureRefusal(context.Background(), p, "claude-code", f.owner)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	if d := live(); !d.credential || !strings.Contains(d.msg, mpAZNotSignedIn) {
		t.Errorf("no sign-in: %+v, want a credential denial saying so", d)
	}
	f.signIn(t, azUIDAnthropic)
	if d := live(); d.msg != "" {
		t.Errorf("captured sign-in: %+v, want no denial", d)
	}
	if d, _ := f.srv.providerAzureRefusal(context.Background(), p, "codex-cli", f.owner); d.credential || d.msg == "" {
		t.Errorf("a harness the row does not serve: %+v, want a non-credential denial", d)
	}
	if d, _ := f.srv.providerAzureRefusal(context.Background(), p, "claude-code", ""); d.msg == "" {
		t.Error("no owner was served")
	}
	// A sign-in a renewal found ended says so.
	f.fake.SetInteractionRequired(true)
	plan, _, _, ok := f.dispatch(t, "claude-code", "foundry-claude")
	if !ok {
		t.Fatal("dispatch refused a captured sign-in")
	}
	f.st.approvals = newFakeApprovals()
	f.srv.cfg.Approvals = f.st.approvals
	f.srv.cfg.Now = func() time.Time { return time.Now().Add(time.Minute) }
	if w := f.resolve(t, plan, "?phase=boot"); w.Code != http.StatusForbidden {
		t.Fatalf("boot resolve: %d %s", w.Code, w.Body.String())
	}
	if d := live(); !d.credential || !strings.Contains(d.msg, mpAZSignInEnded) {
		t.Errorf("an ended sign-in: %+v, want the ended-sign-in denial", d)
	}
}

// The sink redeems the snapshot's audience for the owner, formats one Bearer header, and records the grant,
// the audience and what the authority said the token carries; nothing else.
func TestAzureSink_RedeemsTheSnapshotAudience(t *testing.T) {
	for _, tc := range []struct {
		name, agent, provider, uid, host, want, other string
	}{
		{"Foundry audience", "claude-code", "foundry-claude", azUIDAnthropic, azA3Host, azFoundryScope, azCognitiveScope},
		{"Cognitive Services audience", "codex-cli", "foundry-openai", azUIDOpenAI, azA3OpenAIHost, azCognitiveScope, azFoundryScope},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAzA3Fixture(t)
			f.signIn(t, tc.uid)
			plan, _, _, _ := f.dispatch(t, tc.agent, tc.provider)
			var carried []string
			f.fake.OnIssue(func(it entrafake.IssuedToken) { carried = append(carried, it.Scopes...) })
			w := f.resolve(t, plan, "")
			if w.Code != http.StatusOK {
				t.Fatalf("resolve: %d %s", w.Code, w.Body.String())
			}
			var resp types.ResolvedInjection
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if resp.Host != tc.host || resp.Header != "Authorization" || !strings.HasPrefix(resp.Value, "Bearer ") || len(resp.Value) < 20 {
				t.Errorf("response = %+v, want one Bearer header for the endpoint host", resp)
			}
			// Entra answers a <resource>/.default request with the permissions under that one resource.
			if !slices.Contains(carried, tc.want) || slices.Contains(carried, tc.other) {
				t.Errorf("the redeemed token carries %v, want %s and not %s", carried, tc.want, tc.other)
			}
			rows := f.audit.find("secret.read")
			if len(rows) == 0 || rows[len(rows)-1].Outcome != "success" ||
				!strings.Contains(string(rows[len(rows)-1].Data), tc.want) || strings.Contains(string(rows[len(rows)-1].Data), strings.TrimPrefix(resp.Value, "Bearer ")) {
				t.Errorf("secret.read rows = %+v, want a success naming the granted scope and never the token", rows)
			}
		})
	}
}

// Every way a resolve could re-authorize the run against something it was not dispatched with is a 403
// and mints nothing: a hand-authored grant, another person's token, a provider that moved, was turned off
// or was re-created, and a host that is not the endpoint.
func TestAzureSink_RefusesAnythingTheSnapshotDoesNotBless(t *testing.T) {
	type tc struct {
		name   string
		change func(f *azA3Fixture, plan dispatchLLMPlan) (sub string, rule egress.InjectionRule, grant uuid.UUID)
		code   int
		reason string
	}
	same := func(f *azA3Fixture, plan dispatchLLMPlan) (string, egress.InjectionRule, uuid.UUID) {
		return f.owner, plan.injections[0].Rule, plan.injections[0].GrantID
	}
	for _, c := range []tc{
		{"another person's run token", func(f *azA3Fixture, p dispatchLLMPlan) (string, egress.InjectionRule, uuid.UUID) {
			_, r, g := same(f, p)
			return "someone-else", r, g
		}, http.StatusForbidden, reasonOwnerNotCaller},
		{"a grant with no snapshot", func(f *azA3Fixture, p dispatchLLMPlan) (string, egress.InjectionRule, uuid.UUID) {
			id := uuid.New()
			f.st.grants = append(f.st.grants, types.CredentialGrant{ID: id, RunID: f.st.run.ID, Spec: types.GrantSpec{
				Kind: types.GrantAPIKey, Scope: json.RawMessage(`{"host":"` + azA3Host + `","secret_name":"` + p.injections[0].Rule.SecretName + `"}`)}})
			o, r, _ := same(f, p)
			return o, r, id
		}, http.StatusForbidden, reasonMissingScopeSnapshot},
		{"a snapshot for another provider", func(f *azA3Fixture, p dispatchLLMPlan) (string, egress.InjectionRule, uuid.UUID) {
			o, r, g := same(f, p)
			r.SecretName = providerSecretName(azUIDOpenAI, providerEntraPart)
			return o, r, g
		}, http.StatusForbidden, reasonMissingScopeSnapshot},
		{"the provider turned off", func(f *azA3Fixture, p dispatchLLMPlan) (string, egress.InjectionRule, uuid.UUID) {
			f.site.set(func(b *types.ModelProviders) { b.Providers[0].Disabled = true })
			return same(f, p)
		}, http.StatusForbidden, reasonScopeChanged},
		{"the provider re-created under a new uid", func(f *azA3Fixture, p dispatchLLMPlan) (string, egress.InjectionRule, uuid.UUID) {
			f.site.set(func(b *types.ModelProviders) { b.Providers[0].UID = "22222222-aaaa-4aaa-8aaa-000000000009" })
			return same(f, p)
		}, http.StatusForbidden, reasonScopeChanged},
		{"the provider no longer serving the harness", func(f *azA3Fixture, p dispatchLLMPlan) (string, egress.InjectionRule, uuid.UUID) {
			f.site.set(func(b *types.ModelProviders) { b.Providers[0].Harnesses = nil })
			return same(f, p)
		}, http.StatusForbidden, reasonScopeChanged},
		{"a host that is not the endpoint", func(f *azA3Fixture, p dispatchLLMPlan) (string, egress.InjectionRule, uuid.UUID) {
			o, r, g := same(f, p)
			r.Host = "evil.example"
			return o, r, g
		}, http.StatusForbidden, reasonHostNotEndpoint},
		{"the endpoint re-pointed", func(f *azA3Fixture, p dispatchLLMPlan) (string, egress.InjectionRule, uuid.UUID) {
			f.site.set(func(b *types.ModelProviders) { b.Providers[0].Azure.Endpoint = "https://other.services.ai.azure.com" })
			return same(f, p)
		}, http.StatusForbidden, reasonHostNotEndpoint},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newAzA3Fixture(t)
			f.signIn(t, azUIDAnthropic)
			plan, _, _, _ := f.dispatch(t, "claude-code", "foundry-claude")
			issued := 0
			f.fake.OnIssue(func(entrafake.IssuedToken) { issued++ })
			sub, rule, grant := c.change(f, plan)
			w := f.resolveRule(rule, grant, sub, "")
			if w.Code != c.code || !strings.Contains(w.Body.String(), `"reason":"`+c.reason+`"`) {
				t.Fatalf("resolve = %d %s, want %d %s", w.Code, w.Body.String(), c.code, c.reason)
			}
			if issued != 0 {
				t.Errorf("a refused resolve redeemed the sign-in %d time(s)", issued)
			}
			if rows := f.audit.find("secret.read"); len(rows) == 0 || rows[len(rows)-1].Outcome != "failure" {
				t.Errorf("secret.read rows = %+v, want a failure row", rows)
			}
		})
	}
}

// A name that is not an Azure sign-in is not this arm's.
func TestAzureSink_StandsAsideForOtherNames(t *testing.T) {
	f := newAzA3Fixture(t)
	for _, name := range []string{
		providerSecretName(azUIDAnthropic, providerKeyPart), providerSecretName(azUIDAnthropic, providerOAuthPart),
		"wardyn-provider--entra", "some-token", "wardyn-provider-" + azUIDAnthropic,
	} {
		w := httptest.NewRecorder()
		handled := f.srv.resolveAzureFoundryInjection(w, httptest.NewRequest(http.MethodGet, "/x", nil),
			&identity.Claims{RunID: f.st.run.ID, Sub: f.owner}, broker.Minted{Injection: &egress.InjectionRule{Host: azA3Host, SecretName: name}}, uuid.New())
		if handled || w.Body.Len() != 0 {
			t.Errorf("%q: handled=%v body=%s, want this arm to stand aside", name, handled, w.Body.String())
		}
	}
}

// A refresh token that dies MID-RUN, or a Conditional Access policy that wants the person present, holds
// the request on a sign-in request raised under the azure lane for the run's owner; the person's next
// capture for that provider resolves it, and the held request's re-resolve goes through. The Azure DevOps
// lane never sees the row.
func TestAzureSink_MidRunHoldIsResolvedByACapture(t *testing.T) {
	for name, fault := range map[ADOEntraFailure]func(*azA3Fixture, bool){
		ADOEntraFailureDeadCredential:      func(f *azA3Fixture, on bool) { f.fake.SetInvalidGrant(on) },
		ADOEntraFailureInteractionRequired: func(f *azA3Fixture, on bool) { f.fake.SetInteractionRequired(on) },
	} {
		t.Run(string(name), func(t *testing.T) {
			f := newAzA3Fixture(t)
			f.signIn(t, azUIDAnthropic)
			plan, _, _, _ := f.dispatch(t, "claude-code", "foundry-claude")
			if w := f.resolve(t, plan, "?phase=boot"); w.Code != http.StatusOK {
				t.Fatalf("boot resolve: %d %s", w.Code, w.Body.String())
			}
			fault(f, true)
			id := pendingID(t, f.resolve(t, plan, ""), reauthPendingState)
			row, _ := f.st.approvals.Get(context.Background(), id)
			sc, ok := signInScopeFor(row, azureApprovalLane)
			if !ok || sc.Owner != f.owner || sc.ProviderID != azUIDAnthropic || sc.Reason != adoSignInReason {
				t.Fatalf("raised row = %+v, want an azure sign-in request for the owner and provider", row)
			}
			if _, ado := adoSignInScope(row); ado {
				t.Fatal("the row is visible to the Azure DevOps lane")
			}
			if again := pendingID(t, f.resolve(t, plan, ""), reauthPendingState); again != id {
				t.Fatalf("a second resolve raised %s, want the same request %s", again, id)
			}
			req := f.audit.find("credential.reauth.request")
			if len(req) != 1 || !strings.Contains(string(req[0].Data), `"reason":"`+string(name)+`"`) ||
				!strings.Contains(string(req[0].Data), `"provider":"`+azureApprovalLane+`"`) {
				t.Fatalf("credential.reauth.request rows = %+v, want one for the azure lane with reason %s", req, name)
			}
			if got := f.srv.reconcileADOReauthOnRead(context.Background(), row); got.State != types.ApprovalPending {
				t.Fatalf("the Azure DevOps reconcile resolved an azure row: %s", got.State)
			}
			if got := f.srv.reconcileAzureReauthOnRead(context.Background(), row); got.State != types.ApprovalPending {
				t.Fatalf("resolved before any sign-in: %s", got.State)
			}

			fault(f, false)
			f.srv.cfg.Now = func() time.Time { return time.Now().Add(2 * time.Minute) }
			f.signIn(t, azUIDAnthropic)
			if row, _ := f.st.approvals.Get(context.Background(), id); row.State != types.ApprovalApproved {
				t.Fatalf("after the capture the request is %s, want APPROVED by the capture itself", row.State)
			}
			if w := f.resolve(t, plan, ""); w.Code != http.StatusOK {
				t.Fatalf("the held request's re-resolve: %d %s, want 200", w.Code, w.Body.String())
			}
		})
	}
}

// At the sidecar's boot there is no request to hold: the run fails with the remedy as its hint, naming the
// policy class a Conditional Access refusal came from.
func TestAzureSink_BootFailureCarriesTheRemedy(t *testing.T) {
	f := newAzA3Fixture(t)
	f.signIn(t, azUIDAnthropic)
	plan, _, _, _ := f.dispatch(t, "claude-code", "foundry-claude")
	f.fake.SetInteractionRequired(true)
	w := f.resolve(t, plan, "?phase=boot")
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "multi-factor authentication") ||
		!strings.Contains(w.Body.String(), "AADSTS50076") {
		t.Fatalf("boot resolve = %d %s, want a 403 naming the policy class", w.Code, w.Body.String())
	}
	if !strings.Contains(f.st.hint, "multi-factor authentication") {
		t.Errorf("failure hint = %q, want the same remedy", f.st.hint)
	}
	if rows := f.st.approvals.requested; len(rows) != 0 {
		t.Errorf("a boot resolve raised %d request(s), want none", len(rows))
	}
}

func TestAzureSink_ConsentAndAuthorityFailuresAnswerByClass(t *testing.T) {
	for name, tc := range map[string]struct {
		arrange func(*azA3Fixture)
		code    int
		reason  string
	}{
		"consent withdrawn": {func(f *azA3Fixture) { f.fake.SetConsentRequired(true) }, http.StatusForbidden, string(ADOEntraFailureConsentRequired)},
		"no sign-in": {func(f *azA3Fixture) {
			_ = f.srv.cfg.Secrets.For(f.owner).Delete(context.Background(), providerSecretName(azUIDAnthropic, providerEntraPart))
		}, http.StatusForbidden, string(ADOEntraFailureNotCaptured)},
	} {
		t.Run(name, func(t *testing.T) {
			f := newAzA3Fixture(t)
			f.signIn(t, azUIDAnthropic)
			plan, _, _, _ := f.dispatch(t, "claude-code", "foundry-claude")
			tc.arrange(f)
			w := f.resolve(t, plan, "?phase=boot")
			if w.Code != tc.code || !strings.Contains(w.Body.String(), `"reason":"`+tc.reason+`"`) {
				t.Fatalf("resolve = %d %s, want %d %s", w.Code, w.Body.String(), tc.code, tc.reason)
			}
		})
	}
	f := newAzA3Fixture(t)
	f.signIn(t, azUIDAnthropic)
	plan, _, _, _ := f.dispatch(t, "claude-code", "foundry-claude")
	f.srv.cfg.AzureFoundryEntra = func(context.Context, string) (ADOEntraConfig, bool, error) {
		return ADOEntraConfig{}, false, errors.New("boom")
	}
	if w := f.resolve(t, plan, ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("an unreadable Entra application: %d %s, want 503", w.Code, w.Body.String())
	}
}

func TestAzureConditionalAccessClass(t *testing.T) {
	for desc, want := range map[string]string{
		"interaction_required (AADSTS50076)":  "multi-factor authentication",
		"interaction_required (AADSTS53000)":  "compliant device",
		"interaction_required (AADSTS53003)":  "blocks token issuance",
		"interaction_required (AADSTS999999)": "AADSTS999999",
		"interaction_required":                "Conditional Access policy refused the renewal",
	} {
		if got := azureConditionalAccessClass(errors.New(desc)); !strings.Contains(got, want) {
			t.Errorf("%q -> %q, want it to name %q", desc, got, want)
		}
	}
}
