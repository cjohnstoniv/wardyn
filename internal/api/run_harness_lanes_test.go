// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/agentpolicy"
	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A run's harness set beyond its own agent: each harness's model credential
// reaches the proxy on its own lane, each sink accepts a grant only for a
// (harness, provider) pair the run was launched with, and the owner re-checks
// cover every pair.

// harnessRows answers a run's run_harnesses rows for a test store.
type harnessRows []types.RunHarness

func (r harnessRows) ListRunHarnesses(context.Context, uuid.UUID) ([]types.RunHarness, error) {
	return r, nil
}

func (harnessRows) PutRunHarnesses(context.Context, uuid.UUID, []types.RunHarness) error { return nil }

// withHarnesses is st that also keeps the run's harness set.
type withHarnesses struct {
	store.Store
	harnessRows
}

var _ store.RunHarnessStore = withHarnesses{}

// resolveAs asks the injection sink to resolve grantID for run as the proxy
// would, for the rule the grant carries.
func resolveAs(t *testing.T, h *harness, runID, grantID uuid.UUID, rule egress.InjectionRule) (int, string) {
	t.Helper()
	h.broker.minted = broker.Minted{Kind: types.GrantAPIKey, JTI: "jti-" + grantID.String(), Injection: &rule}
	rr := do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+grantID.String(), h.mintRunToken(t, runID), "")
	return rr.Code, rr.Body.String()
}

// grantFor is the authored injection whose secret is secret.
func grantFor(t *testing.T, injections []runner.InjectionGrant, secret string) runner.InjectionGrant {
	t.Helper()
	i := slices.IndexFunc(injections, func(ig runner.InjectionGrant) bool { return ig.Rule.SecretName == secret })
	if i < 0 {
		t.Fatalf("no injection for %s in %+v", secret, injections)
	}
	return injections[i]
}

const (
	laneOwnerOpenAIKey = "sk-alice-own-openai-key"
	laneSecondHarness  = "codex-cli"
)

// twoKeyLanes is a claude-code run on Anthropic key provider a whose set also
// carries codex-cli on OpenAI key provider b, with the owner's own keys for
// both and the operator's rows under both names.
func twoKeyLanes(t *testing.T) (*harness, *bearerGuardStore, types.ModelProvider, types.ModelProvider, harnessRows) {
	t.Helper()
	a := mpKeyProvider("anthropic", "uid-a", types.ModelProviderAnthropicAPIKey, types.ProviderHarness{Harness: "claude-code", Model: "claude-opus-test"})
	b := mpKeyProvider("openai", "uid-b", types.ModelProviderOpenAIAPIKey, types.ProviderHarness{Harness: laneSecondHarness, Model: "gpt-test"})
	st := &bearerGuardStore{
		run:  types.AgentRun{ID: uuid.New(), Agent: "claude-code", CreatedBy: mpOwner, ModelProviderID: a.ID},
		site: types.SiteConfig{ModelProviders: providerBlock(a, b)},
	}
	h, sec := bearerGuardHarness(t, st)
	for uid, own := range map[string]string{a.UID: mpOwnerKey, b.UID: laneOwnerOpenAIKey} {
		name := providerSecretName(uid, providerKeyPart)
		sec.m[name] = []byte(mpOperatorValue)
		if err := sec.For(mpOwner).Put(context.Background(), name, []byte(own)); err != nil {
			t.Fatal(err)
		}
	}
	rows := harnessRows{{Harness: "claude-code", ModelProviderID: a.ID, Starts: true}, {Harness: laneSecondHarness, ModelProviderID: b.ID}}
	return h, st, a, b, rows
}

// laneCaptureStore captures the grants dispatch writes and lets a refusal fail
// the run.
type laneCaptureStore struct {
	captureGrantStore
	hint string
}

func (s *laneCaptureStore) UpdateRunStateIf(context.Context, uuid.UUID, types.RunState, types.RunState) (bool, error) {
	return true, nil
}

func (s *laneCaptureStore) SetRunFailureHint(_ context.Context, _ uuid.UUID, hint string) error {
	s.hint = hint
	return nil
}

// dispatchLanes runs the REAL LLM phase for st's run with its harness set.
func dispatchLanes(t *testing.T, h *harness, st *bearerGuardStore, rows harnessRows, policy *types.RunPolicySpec, injections []runner.InjectionGrant,
) (dispatchLLMPlan, map[string]string, []types.CredentialGrant, bool) {
	t.Helper()
	captured := &laneCaptureStore{}
	h.srv.cfg.Store = captured
	defer func() { h.srv.cfg.Store = st }()
	env := map[string]string{}
	plan, ok := h.srv.resolveLLMInjections(context.Background(), st.run, dispatchParams{Harnesses: rows},
		policy, env, injections, "http://wardyn-proxy:3128", artifactRedirectPlan{}, false, st.site, true, false, bedrockCredUngraded())
	st.grants = append(st.grants, captured.grants...)
	return plan, env, captured.grants, ok
}

// T12: two harnesses with two providers get two lanes. The one strip runs over
// both lanes' hosts before either authors, so a legacy credential on either
// vendor host is dropped and both authored grants survive; each grant records
// its own harness, and each harness family's env points it at the proxy.
func TestDispatch_EachHarnessLaneInjected(t *testing.T) {
	h, st, a, b, rows := twoKeyLanes(t)
	unrelated := mpInjection("registry.corp.example", "npm-token")
	legacy := []runner.InjectionGrant{mpInjection("api.anthropic.com", "anthropic-api-key"), mpInjection("api.openai.com", "openai-api-key")}
	policy := types.RunPolicySpec{}
	plan, env, grants, ok := dispatchLanes(t, h, st, rows, &policy, append([]runner.InjectionGrant{unrelated}, legacy...))
	if !ok || len(grants) != 2 {
		t.Fatalf("dispatch ok=%v grants=%d, want one authored grant per lane", ok, len(grants))
	}
	for i, want := range []struct {
		host, harness string
		p             types.ModelProvider
	}{{"api.anthropic.com", "claude-code", a}, {"api.openai.com", laneSecondHarness, b}} {
		sc := grantScope(t, grants[i])
		snap, _ := sc["snapshot"].(map[string]any)
		if sc["host"] != want.host || sc["secret_name"] != providerSecretName(want.p.UID, providerKeyPart) ||
			snap["harness"] != want.harness || snap["provider_uid"] != want.p.UID {
			t.Errorf("grant %d scope = %v, want %s's key on %s for %s", i, sc, want.p.ID, want.host, want.harness)
		}
		if !slices.ContainsFunc(plan.injections, func(ig runner.InjectionGrant) bool { return ig.GrantID == grants[i].ID }) {
			t.Errorf("the strip dropped the %s lane's own grant", want.harness)
		}
		if !slices.Contains(policy.AllowedDomains, want.host) {
			t.Errorf("allowlist %v lacks %s", policy.AllowedDomains, want.host)
		}
	}
	if len(plan.injections) != 3 || plan.injections[0].GrantID != unrelated.GrantID {
		t.Errorf("injections = %+v, want the unrelated one and the two authored grants", plan.injections)
	}
	if n := strings.Count(auditDatums(h, "run.injection.drop"), "model_credential_not_provider_authored"); n != len(legacy) {
		t.Errorf("drops audited = %d, want %d", n, len(legacy))
	}
	if env[envAnthropicAPIKey] != "wardyn-proxy-injected" || env[envAnthropicModel] != "claude-opus-test" ||
		env[envCodexAPIKey] != "wardyn-proxy-injected" || !strings.HasPrefix(env[envCodexBaseURL], "http://wardyn-proxy:3128/wardyn/llm/openai") {
		t.Errorf("env = %v, want both families' placeholders and routes", env)
	}
}

// Two harnesses on one key provider share its one grant; two providers that
// would credential one host are refused before anything is authored.
func TestDispatch_HarnessLanesShareOrConflict(t *testing.T) {
	endpoint := func(id, uid string) types.ModelProvider {
		return types.ModelProvider{ID: id, UID: uid, Kind: types.ModelProviderCustomEndpoint, BaseURL: "https://gw.corp.example",
			Harnesses: []types.ProviderHarness{{Harness: "claude-code", Path: "/anthropic"}, {Harness: laneSecondHarness, Path: "/openai"}}}
	}
	for _, tc := range []struct {
		name   string
		second types.ModelProvider
		grants int
		ok     bool
	}{
		{"one provider for both harnesses", endpoint("gw", "uid-gw"), 1, true},
		{"a second provider on the same host", endpoint("gw-2", "uid-gw-2"), 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := endpoint("gw", "uid-gw")
			providers := []types.ModelProvider{first}
			if tc.second.ID != first.ID {
				providers = append(providers, tc.second)
			}
			st := &bearerGuardStore{
				run:  types.AgentRun{ID: uuid.New(), Agent: "claude-code", CreatedBy: mpOwner, ModelProviderID: first.ID},
				site: types.SiteConfig{ModelProviders: providerBlock(providers...)},
			}
			h, sec := bearerGuardHarness(t, st)
			for _, uid := range []string{first.UID, tc.second.UID} {
				_ = sec.For(mpOwner).Put(context.Background(), providerSecretName(uid, providerKeyPart), []byte(mpOwnerKey))
			}
			rows := harnessRows{{Harness: "claude-code", ModelProviderID: first.ID, Starts: true}, {Harness: laneSecondHarness, ModelProviderID: tc.second.ID}}
			plan, _, grants, ok := dispatchLanes(t, h, st, rows, &types.RunPolicySpec{}, nil)
			if ok != tc.ok || len(grants) != tc.grants {
				t.Fatalf("dispatch ok=%v grants=%d, want ok=%v grants=%d", ok, len(grants), tc.ok, tc.grants)
			}
			if tc.ok && !mapsEqual(plan.llmUpstreams, map[string]string{
				"api.anthropic.com": "https://gw.corp.example/anthropic", "api.openai.com": "https://gw.corp.example/openai"}) {
				t.Errorf("upstreams = %v, want each harness's dialect routed to its own path", plan.llmUpstreams)
			}
			if !tc.ok && !strings.Contains(auditDatums(h, "run.create"), "gw-2") {
				t.Errorf("the refusal's run.create row does not name the second provider: %s", auditDatums(h, "run.create"))
			}
		})
	}
}

// T13: each sink accepts a grant for a (harness, provider) pair the run was
// launched with although that harness is not the run's own agent, and refuses
// the same grant once the pair is not in the run's set.
func TestInjectionSink_SecondHarnessProvider(t *testing.T) {
	// check resolves inj for runID twice: with the set rows (in), it answers
	// 200 carrying want; with the second harness gone from the set, it is
	// refused with reason.
	check := func(t *testing.T, h *harness, base store.Store, runID uuid.UUID, rows harnessRows, inj runner.InjectionGrant, want, reason string) {
		t.Helper()
		h.srv.cfg.Store = withHarnesses{base, rows}
		if code, body := resolveAs(t, h, runID, inj.GrantID, inj.Rule); code != http.StatusOK || !strings.Contains(body, want) {
			t.Fatalf("in the set: sink = %d %s, want 200 carrying the owner's own credential", code, body)
		}
		h.srv.cfg.Store = withHarnesses{base, rows[:1]}
		if code, body := resolveAs(t, h, runID, inj.GrantID, inj.Rule); code != http.StatusForbidden || errorReasonOf(body) != reason {
			t.Fatalf("out of the set: sink = %d %s, want 403 %s", code, body, reason)
		}
	}

	t.Run("key", func(t *testing.T) {
		h, st, _, b, rows := twoKeyLanes(t)
		plan, _, _, ok := dispatchLanes(t, h, st, rows, &types.RunPolicySpec{}, nil)
		if !ok {
			t.Fatal("dispatch refused")
		}
		check(t, h, st, st.run.ID, rows, grantFor(t, plan.injections, providerSecretName(b.UID, providerKeyPart)), laneOwnerOpenAIKey, "provider_changed")
	})

	// The run's own agent is codex-cli with no provider; claude-code is the
	// second harness, on a Claude subscription or a Bedrock key.
	secondClaude := func(t *testing.T, h *harness, st *subStore, p types.ModelProvider) (dispatchLLMPlan, harnessRows) {
		t.Helper()
		st.run.Agent, st.run.ModelProviderID = laneSecondHarness, ""
		rows := harnessRows{{Harness: laneSecondHarness, Starts: true}, {Harness: "claude-code", ModelProviderID: p.ID}}
		plan, ok := h.srv.resolveLLMInjections(context.Background(), st.run, dispatchParams{Harnesses: rows},
			&types.RunPolicySpec{}, map[string]string{}, nil, "http://wardyn-proxy:3128", artifactRedirectPlan{}, false, st.site, true, false, bedrockCredUngraded())
		if !ok || len(st.grants) != 1 {
			t.Fatalf("dispatch ok=%v grants=%d (failed: %q)", ok, len(st.grants), st.failed)
		}
		var scope struct {
			Snapshot providerGrantSnapshot `json:"snapshot"`
		}
		_ = json.Unmarshal(st.grants[0].Spec.Scope, &scope)
		if scope.Snapshot.Harness != "claude-code" || scope.Snapshot.ProviderUID != p.UID {
			t.Fatalf("snapshot = %+v, want claude-code's own provider", scope.Snapshot)
		}
		return plan, rows
	}

	t.Run("subscription", func(t *testing.T) {
		p := subProvider("claude")
		h, st, _ := subHarness(t, p)
		plan, rows := secondClaude(t, h, st, p)
		check(t, h, st, st.run.ID, rows, grantFor(t, plan.injections, providerSecretName(p.UID, providerOAuthPart)), subOwnerToken, "provider_changed")
	})

	t.Run("bedrock", func(t *testing.T) {
		p := brBearerProvider()
		h, st, _ := brHarness(t, p)
		plan, rows := secondClaude(t, h, st, p)
		check(t, h, st, st.run.ID, rows, grantFor(t, plan.injections, providerSecretName(p.UID, providerKeyPart)), brOwnerKey, "provider_changed")
	})

	t.Run("azure", func(t *testing.T) {
		f := newAzA3Fixture(t)
		f.signIn(t, azUIDAnthropic)
		f.st.run.Agent, f.st.run.ModelProviderID = laneSecondHarness, ""
		rows := harnessRows{{Harness: laneSecondHarness, Starts: true}, {Harness: "claude-code", ModelProviderID: "foundry-claude"}}
		site, err := f.site.GetSiteConfig(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		plan, ok := f.srv.resolveLLMInjections(context.Background(), f.st.run, dispatchParams{Harnesses: rows}, &types.RunPolicySpec{}, map[string]string{}, nil,
			"http://wardyn-proxy:3128", artifactRedirectPlan{}, false, site, true, false, bedrockCredUngraded())
		if !ok {
			t.Fatalf("dispatch refused: %s", f.st.hint)
		}
		f.srv.cfg.Store = withHarnesses{f.st, rows}
		if w := f.resolve(t, plan, ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Bearer ") {
			t.Fatalf("in the set: sink = %d %s, want 200 with the owner's token", w.Code, w.Body.String())
		}
		f.srv.cfg.Store = withHarnesses{f.st, rows[:1]}
		if w := f.resolve(t, plan, ""); w.Code != http.StatusForbidden || errorReasonOf(w.Body.String()) != reasonScopeChanged {
			t.Fatalf("out of the set: sink = %d %s, want 403 %s", w.Code, w.Body.String(), reasonScopeChanged)
		}
	})
}

// T15: the owner re-checks cover every harness and provider of the set. With
// the second harness's agent or provider door closed for the owner, the revive
// is refused naming it; with both open it revives.
func TestOwnerDoors_HarnessProvider(t *testing.T) {
	for _, door := range []struct{ kind, closed string }{
		{capModelProvider, "codex-provider"},
		{capAgent, laneSecondHarness},
	} {
		for _, open := range []bool{false, true} {
			t.Run(door.kind+map[bool]string{true: "/open", false: "/closed"}[open], func(t *testing.T) {
				f, _ := newOwnerFixture(t)
				f.st.run.ModelProviderID = "anthro"
				f.st.site.ModelProviders = providerBlock(keyProvider("anthro", "claude-code"), keyProvider("codex-provider", laneSecondHarness))
				rows := harnessRows{{Harness: "claude-code", ModelProviderID: "anthro", Starts: true}, {Harness: laneSecondHarness, ModelProviderID: "codex-provider"}}
				f.srv.cfg.Store = &struct {
					*reviveStore
					harnessRows
				}{f.rs, rows}
				own := map[string]string{capModelProvider: "anthro", capAgent: "claude-code"}[door.kind]
				f.st.caps = []types.CapabilityGrant{grant(types.CapabilitySubjectUser, f.run.CreatedBy, door.kind, own, types.CapabilityAllow)}
				if open {
					f.st.caps = append(f.st.caps, grant(types.CapabilitySubjectUser, f.run.CreatedBy, door.kind, door.closed, types.CapabilityAllow))
				}
				f.st.enf = map[string]bool{door.kind: true}
				if open {
					if code, body := f.reviveAs(t, true); code != http.StatusOK {
						t.Fatalf("revive with every door open = %d %s, want 200", code, body)
					}
					return
				}
				for _, owner := range []bool{true, false} {
					code, body := f.reviveAs(t, owner)
					if want := "the " + door.kind + " capability for " + door.closed; code != http.StatusForbidden || !strings.Contains(body, want) {
						t.Fatalf("revive (owner %v) = %d %s, want 403 naming %q", owner, code, body, want)
					}
				}
			})
		}
	}
}

// T16: the managed settings follow the set. A run whose started harness is
// codex-cli, with claude-code in its set, gets Claude Code's managed file.
func TestManagedSettings_ClaudeInSetNotStarted(t *testing.T) {
	rn := &fakeRunner{}
	srv, _, _, run := dispatchTeardownFixture(t, rn, types.RunPending)
	run.Task, run.Agent, run.AutonomyLevel = "", laneSecondHarness, types.AutonomyL1
	srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
		RunToken: "run-token", Image: "img", Interactive: true,
		Harnesses: []types.RunHarness{{Harness: laneSecondHarness, Starts: true}, {Harness: "claude-code"}},
	})
	path, _, ok := agentpolicy.ForAgent("claude-code", types.AutonomyL1, false, false)
	if !ok || rn.createCalls != 1 {
		t.Fatalf("fixture: claude-code at L1 has a managed file %v; sandbox created %d", ok, rn.createCalls)
	}
	if !slices.ContainsFunc(rn.lastSpec.ManagedFiles, func(f runner.ManagedFile) bool { return f.Path == path }) {
		t.Errorf("managed files = %+v, want claude-code's %s", rn.lastSpec.ManagedFiles, path)
	}
	if rn.lastSpec.Env["WARDYN_HARNESSES"] != laneSecondHarness+" claude-code" || rn.lastSpec.Env["WARDYN_START_HARNESS"] != laneSecondHarness {
		t.Errorf("harness env = %q / %q, want the set in order and the started harness",
			rn.lastSpec.Env["WARDYN_HARNESSES"], rn.lastSpec.Env["WARDYN_START_HARNESS"])
	}
}

// T17: off the hold lane nothing is minted. An interactive run whose set has
// per-harness tool rules gets no credential file, no credential in any env and
// no digest in its proxy config, so any tool-gate call it makes is decided as a
// call from no harness is.
func TestNoHarnessCredentialOffHoldLane(t *testing.T) {
	rn := &fakeRunner{}
	srv, _, audit, run := dispatchTeardownFixture(t, rn, types.RunPending)
	run.Task = ""
	srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
		RunToken: "run-token", Image: "img", Interactive: true, HarnessToolRules: twoHarnessSets,
		Harnesses: []types.RunHarness{{Harness: "claude-code", Starts: true}, {Harness: laneSecondHarness}},
	})
	spec := rn.lastSpec
	if rn.createCalls != 1 {
		t.Fatal("the sandbox was not created")
	}
	for _, f := range spec.ManagedFiles {
		if strings.HasPrefix(f.Path, runner.ComponentSecretDir+"/"+toolgateCredentialFilePrefix) {
			t.Errorf("a credential file was delivered: %s", f.Path)
		}
	}
	for _, env := range []map[string]string{spec.Env, spec.SecretEnv} {
		if _, ok := env["WARDYN_TOOLGATE_CREDENTIAL"]; ok {
			t.Error("the sandbox env carries a tool-approval credential")
		}
	}
	if len(spec.ProxyConfig.HarnessCredentials) != 0 || len(spec.ProxyConfig.HarnessToolRules) != 2 {
		t.Errorf("proxy: %d digests, %d rule sets; want no digest and both rule sets", len(spec.ProxyConfig.HarnessCredentials), len(spec.ProxyConfig.HarnessToolRules))
	}
	if rows := toolgateCredentialRows(t, audit.events, run.ID); len(rows) != 0 {
		t.Errorf("credential rows = %v, want none: nothing was minted", rows)
	}
}

// Item 5 of the harness-set dispatch: a revive brings the tool-approval
// credential back nowhere but where dispatch put it. A revive replaces the
// proxy (digests only) and restarts the same sandbox; it builds no sandbox
// spec, so the container env it restarts is dispatch's, which carries no
// credential, and the revived proxy config carries the digests and never the
// plaintext.
func TestNoToolgateCredentialInContainerEnvAfterRevive(t *testing.T) {
	rn := &fakeRunner{}
	_, _, _ = dispatchTwoHarnessRun(t, rn, dispatchParams{})
	spec := rn.lastSpec
	cred := toolgateCredentialOf(t, spec)
	digests := spec.ProxyConfig.HarnessCredentials
	if len(digests) != 2 {
		t.Fatalf("dispatch digests = %d, want 2", len(digests))
	}

	f := newReviveFixture(t)
	f.editConfig(t, func(c *proxy.Config) {
		c.HarnessToolRules, c.HarnessCredentials = spec.ProxyConfig.HarnessToolRules, digests
	})
	creates := f.rr.createCalls
	if code, body := f.reviveAs(t, false); code != http.StatusOK {
		t.Fatalf("revive = %d %s, want 200", code, body)
	}
	if f.rr.createCalls != creates {
		t.Error("the revive created a sandbox: it must restart the dispatched one")
	}
	if strings.Contains(string(f.rr.replaced[0]), cred) {
		t.Error("the revived proxy config carries the plaintext credential")
	}
	if got := f.newConfig(t).HarnessCredentials; !slices.Equal(got, digests) {
		t.Errorf("revived digests = %+v, want the dispatched %+v", got, digests)
	}
}

// Create and dispatch refuse the same provider pairs: dispatch's lane test is
// the create door's (harnessProviderPairsConflict), so a run create admits is
// never refused at dispatch for a pair create judged otherwise, and the
// reverse. One Azure Foundry provider on two harnesses is a conflict.
func TestHarnessProviderConflict_CreateAndDispatchAgree(t *testing.T) {
	endpoint := func(id, base string, harnesses ...types.ProviderHarness) types.ModelProvider {
		return types.ModelProvider{ID: id, UID: "uid-" + id, Kind: types.ModelProviderCustomEndpoint, BaseURL: base, Harnesses: harnesses}
	}
	claude, codex := types.ProviderHarness{Harness: "claude-code"}, types.ProviderHarness{Harness: laneSecondHarness}
	codexOwnHeader := types.ProviderHarness{Harness: laneSecondHarness, AuthHeader: "x-codex-key", AuthFormat: "%s"}
	azure := func(id string) types.ModelProvider {
		return types.ModelProvider{ID: id, UID: "uid-" + id, Kind: types.ModelProviderAzureFoundry, Harnesses: []types.ProviderHarness{claude, codex}}
	}
	shared := endpoint("gw", "https://gw.example.com", claude, codex)
	ownHeader := endpoint("gw", "https://gw.example.com", claude, codexOwnHeader)
	oneAzure := azure("az")
	subThrough := func(base string) types.ModelProvider {
		return types.ModelProvider{ID: "sub", UID: "uid-sub", Kind: types.ModelProviderAnthropicSubscription, BaseURL: base, Harnesses: []types.ProviderHarness{claude}}
	}
	bedrockKey := types.ModelProvider{ID: "br", UID: "uid-br", Kind: types.ModelProviderBedrockBearer,
		Bedrock: &types.BedrockSettings{Region: "us-west-2"}, Harnesses: []types.ProviderHarness{claude}}
	for _, tc := range []struct {
		name     string
		pa, pb   types.ModelProvider
		conflict bool
	}{
		{"one endpoint, the same key sent the same way", shared, shared, false},
		{"one endpoint whose header differs per harness", ownHeader, ownHeader, true},
		{"two providers on one host", endpoint("gw-a", "https://gw.example.com", claude), endpoint("gw-b", "https://gw.example.com", codex), true},
		{"two providers on two hosts", endpoint("gw-a", "https://gw.example.com", claude), endpoint("gw-b", "https://other.example.com", codex), false},
		{"two Azure Foundry providers", azure("az-1"), azure("az-2"), true},
		{"one Azure Foundry provider on two harnesses", oneAzure, oneAzure, true},
		{"a Claude subscription through the endpoint's host", subThrough("https://gw.example.com"), endpoint("gw-b", "https://gw.example.com", codex), true},
		{"a Claude subscription on the vendor host", subThrough(""), endpoint("gw-b", "https://gw.example.com", codex), false},
		{"a gateway-routed subscription beside an endpoint on the vendor host", subThrough("https://gw.example.com"), endpoint("gw-b", "https://api.anthropic.com", codex), true},
		{"Bedrock beside an endpoint on its runtime host", bedrockKey, endpoint("gw-b", "https://bedrock-runtime.us-west-2.amazonaws.com", codex), true},
		{"Bedrock beside an endpoint on its control host", bedrockKey, endpoint("gw-b", "https://bedrock.us-west-2.amazonaws.com", codex), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, atCreate := harnessProvidersConflict([]harnessChoice{
				{harness: "claude-code", choice: runProviderChoice{provider: tc.pa, chosen: true}},
				{harness: laneSecondHarness, choice: runProviderChoice{provider: tc.pb, chosen: true}},
			})
			pairs := []types.RunHarness{{Harness: "claude-code", ModelProviderID: tc.pa.ID}, {Harness: laneSecondHarness, ModelProviderID: tc.pb.ID}}
			lanes := make([]providerLane, len(pairs))
			for i, p := range []types.ModelProvider{tc.pa, tc.pb} {
				lanes[i].chosen = &chosenProvider{provider: p}
				lanes[i].key, _ = providerKeyLaneFor(p, pairs[i].Harness)
				lanes[i].azure, _ = providerAzureLaneFor(p, pairs[i].Harness)
			}
			j, _ := laneConflict(pairs, lanes)
			if atCreate != tc.conflict || (j > 0) != tc.conflict {
				t.Errorf("conflict at create %v, at dispatch %v; want both %v", atCreate, j > 0, tc.conflict)
			}
		})
	}
}
