// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

type localPlanTestStore struct{ *componentTestStore }

func (s localPlanTestStore) ListRunComponents(_ context.Context, runID uuid.UUID) ([]types.RunComponent, error) {
	s.cmu.Lock()
	defer s.cmu.Unlock()
	return s.snapshots[runID], nil
}

func TestLocalResolvedInjectionRequiresExactScopePinsAndStoredOwnerOnly(t *testing.T) {
	f := newComponentFixture(t)
	f.srv.cfg.Store = localPlanTestStore{f.st}
	run := types.AgentRun{ID: uuid.New(), CreatedBy: capSub, Placement: types.PlacementLocal}
	g := types.CredentialGrant{ID: uuid.New(), RunID: run.ID, Spec: types.GrantSpec{Kind: types.GrantAPIKey, OwnerOnly: true,
		Scope: json.RawMessage(`{"host":"api.example","secret_name":"person-secret","require_tls":true,"pin_path":"/allowed","pin_query":{"account":"own"},"pin_routes":[{"method":"GET","path":"/allowed"}]}`)}}
	rule, err := injectionRuleFromScope(g.Spec.Scope)
	if err != nil {
		t.Fatal(err)
	}
	f.st.grants = []types.CredentialGrant{g}
	spec := runner.SandboxSpec{ProxyConfig: runner.ProxyConfig{Injection: []runner.InjectionGrant{{GrantID: g.ID, Rule: rule}}}}
	classify := func(t *testing.T, spec runner.SandboxSpec, wantRefusal bool) {
		t.Helper()
		p, err := f.srv.localResolvedPlan(context.Background(), run, false, types.SiteConfig{}, spec, nil, llmTransport{})
		if err != nil {
			t.Fatal(err)
		}
		_, denied := placement.LocalEligibility(p)
		if (denied != nil) != wantRefusal {
			t.Fatalf("denied=%v wanted refusal=%v", denied, wantRefusal)
		}
	}
	classify(t, spec, false)
	for _, mutate := range []func(*runner.InjectionGrant){
		func(in *runner.InjectionGrant) { in.Rule.RequireTLS = false },
		func(in *runner.InjectionGrant) { in.Rule.PinPath = "" },
		func(in *runner.InjectionGrant) { in.Rule.PinQuery = nil },
		func(in *runner.InjectionGrant) { in.Rule.PinRoutes = nil },
		func(in *runner.InjectionGrant) { in.GrantID = uuid.New() },
	} {
		changed := spec
		changed.ProxyConfig.Injection = []runner.InjectionGrant{{GrantID: g.ID, Rule: rule}}
		mutate(&changed.ProxyConfig.Injection[0])
		classify(t, changed, true)
	}
	f.st.grants[0].Spec.OwnerOnly = false
	classify(t, spec, true)
}

func TestLocalReviveRefusesBeforeOperatorSecretReadsAndStripsCopy(t *testing.T) {
	f := newComponentFixture(t)
	f.srv.cfg.Store = localPlanTestStore{f.st}
	f.srv.cfg.Secrets = previewSecretsSpy{Store: f.srv.cfg.Secrets, t: t}
	run := types.AgentRun{ID: uuid.New(), CreatedBy: capSub, Placement: types.PlacementLocal}
	for _, secretRef := range []string{"corporate-secret", ""} {
		f.st.siteConfig.UpstreamProxySecretRef = secretRef
		f.st.siteConfig.InternalHosts = []types.InternalHost{{HostSuffix: "corp"}}
		// "removed" was declared at dispatch and has since been deleted: a revive must not keep lifting it.
		cfg := proxy.Config{RunID: run.ID, UpstreamProxyURL: "https://user:password@parent", TrustedCAPEM: "corporate", InternalHosts: []types.InternalHost{{HostSuffix: "corp"}, {HostSuffix: "removed"}}}
		ref := f.srv.reviveOwnerRecheck(context.Background(), run, &cfg, types.ActorSystem, "wardynd")
		// An own plan revives; the organisation's upstream proxy secret is never sent to a runner.
		if secretRef != "" && (ref == nil || ref.reason != string(placement.ReasonPlacementCredential)) {
			t.Fatalf("revive=%v wanted %s", ref, placement.ReasonPlacementCredential)
		}
		if secretRef == "" && ref != nil {
			t.Fatalf("revive of an own plan = %v, want it admitted", ref)
		}
		if secretRef == "" && (cfg.UpstreamProxyURL != "" || cfg.TrustedCAPEM != "" || len(cfg.LLMUpstreams) != 0 || len(cfg.InternalHosts) != 1 || cfg.InternalHosts[0].HostSuffix != "corp") {
			t.Fatal("revive retained organisation network configuration")
		}
	}
}

// A revive classifies the stored identity credentials from the run and today's
// site configuration, as dispatch does: the owner's own-PAT Azure DevOps lane and
// own Azure Foundry sign-in revive with the same authority, and a revive whose
// owner, provider, delivery or capabilities drifted since dispatch is refused.
func TestLocalReviveClassifiesOwnADOAndAzureLanesAsDispatchDoes(t *testing.T) {
	foundry := func() types.ModelProvider {
		return types.ModelProvider{ID: "foundry", UID: "uid-foundry", Kind: types.ModelProviderAzureFoundry,
			Azure:     &types.AzureSettings{Endpoint: "https://res.services.ai.azure.com", Route: types.AzureRouteAnthropic},
			Harnesses: []types.ProviderHarness{{Harness: azureMessagesHarness, Model: "deploy-1"}}}
	}
	adoCfg := func(id uuid.UUID) proxy.Config {
		return proxy.Config{RunID: id,
			Policy:   types.RunPolicySpec{WorkspaceRepos: []types.WorkspaceRepo{{Repo: ownPATOrgKey + "/proj/_git/repo"}}},
			ADOGrant: &proxy.ADOGrantConfig{Organization: "contoso", Capabilities: []adoscope.Capability{adoscope.CapCodeRead}, Hosts: []string{"dev.azure.com"}}}
	}
	azureCfg := func(id uuid.UUID) proxy.Config {
		return proxy.Config{RunID: id, AzureGates: []proxy.AzureGateConfig{{Host: "res.services.ai.azure.com", Route: types.AzureRouteAnthropic, Models: []string{"deploy-1"}}}}
	}
	for _, tc := range []struct {
		name  string
		cfg   func(uuid.UUID) proxy.Config
		drift func(sc *types.SiteConfig, owned map[string][]byte, other map[string][]byte)
		admit bool
	}{
		{"own-PAT Azure DevOps lane", adoCfg, nil, true},
		{"Azure Foundry gate", azureCfg, nil, true},
		{"owner drift: the PAT is another person's", adoCfg, func(_ *types.SiteConfig, owned, other map[string][]byte) {
			delete(owned, adoOwnPATSecretName(ownPATRowID))
			other[adoOwnPATSecretName(ownPATRowID)] = []byte("pat")
		}, false},
		{"owner drift: the sign-in is another person's", azureCfg, func(_ *types.SiteConfig, owned, other map[string][]byte) {
			delete(owned, providerSecretName("uid-foundry", providerEntraPart))
			other[providerSecretName("uid-foundry", providerEntraPart)] = []byte("sign-in")
		}, false},
		{"provider drift: the endpoint moved", azureCfg, func(sc *types.SiteConfig, _, _ map[string][]byte) {
			sc.ModelProviders.Providers[0].Azure.Endpoint = "https://other.services.ai.azure.com"
		}, false},
		{"provider drift: the provider is gone", azureCfg, func(sc *types.SiteConfig, _, _ map[string][]byte) {
			sc.ModelProviders = nil
		}, false},
		{"provider drift: the grant names another organisation", func(id uuid.UUID) proxy.Config {
			c := adoCfg(id)
			c.ADOGrant.Organization = "fabrikam"
			return c
		}, nil, false},
		{"delivery drift: the row now mints tokens", adoCfg, func(sc *types.SiteConfig, _, _ map[string][]byte) {
			sc.WorkspaceProviders.Git[0].Entra.TokenMode = types.ADOTokenModeBearer
		}, false},
		{"capability drift: the ceiling narrowed", adoCfg, func(sc *types.SiteConfig, _, _ map[string][]byte) {
			sc.WorkspaceProviders.Git[0].Entra.CapabilityCeiling = []adoscope.Capability{adoscope.CapProjectRead}
		}, false},
		{"capability drift: the deployment changed", azureCfg, func(sc *types.SiteConfig, _, _ map[string][]byte) {
			sc.ModelProviders.Providers[0].Harnesses[0].Model = "deploy-2"
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newComponentFixture(t)
			f.srv.cfg.Store = localPlanTestStore{f.st}
			f.st.siteConfig = adoSite(ownPATTestRow())
			f.st.siteConfig.ModelProviders = &types.ModelProviders{Providers: []types.ModelProvider{foundry()}}
			secrets := f.srv.cfg.Secrets.(*memSecrets)
			owned, other := secrets.owned[capSub], map[string][]byte{}
			secrets.owned["someone-else"] = other
			owned[adoOwnPATSecretName(ownPATRowID)] = []byte("pat")
			owned[providerSecretName("uid-foundry", providerEntraPart)] = []byte("sign-in")
			if tc.drift != nil {
				tc.drift(&f.st.siteConfig, owned, other)
			}
			run := types.AgentRun{ID: uuid.New(), CreatedBy: capSub, Placement: types.PlacementLocal, Agent: azureMessagesHarness, ModelProviderID: "foundry"}
			cfg := tc.cfg(run.ID)
			ref := f.srv.reviveOwnerRecheck(context.Background(), run, &cfg, types.ActorSystem, "wardynd")
			if tc.admit && ref != nil {
				t.Fatalf("revive = %+v, want it admitted", ref)
			}
			if !tc.admit && (ref == nil || ref.reason != string(placement.ReasonPlacementCredential)) {
				t.Fatalf("revive = %+v, want %s", ref, placement.ReasonPlacementCredential)
			}
		})
	}
}

func TestLocalAllModelProviderKindsHaveOwnClassification(t *testing.T) {
	f := newComponentFixture(t)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(utCtx("user", "", nil))
	for kind := range types.ClosedModelProviderKinds {
		intents, known := f.srv.localProviderIntent(r, capSub, types.ModelProvider{ID: "selected", UID: "uid", Kind: kind})
		if !known || len(intents) == 0 {
			t.Fatalf("provider %s is unclassified", kind)
		}
		for _, intent := range intents {
			if intent.Origin.Class != placement.ClassOwn || !intent.Origin.Stored || intent.Origin.OwnNamespace {
				t.Fatalf("provider %s is unclassified or missing namespace allowed: %+v", kind, intent)
			}
		}
	}
	if _, known := f.srv.localProviderIntent(r, capSub, types.ModelProvider{Kind: "future"}); known {
		t.Fatal("unimplemented provider kind accepted")
	}
}

type localClassifyStore struct {
	*dispatchTestStore
	store.ComponentStore
	comps []types.RunComponent
}

func (s localClassifyStore) ListRunComponents(context.Context, uuid.UUID) ([]types.RunComponent, error) {
	return s.comps, nil
}

// P3 at dispatch: the limit rides the resolved ceiling, the refusal is
// audited by reason alone and the run's hint names no personal component.
func TestLocalDispatchP3RidesTheCeilingAndNamesNoPersonalComponent(t *testing.T) {
	const personal = "Private Personal Tool"
	for _, allowed := range []bool{false, true} {
		c := ceilingForDispatch(governanceCeiling{Profile: &ResolvedProfile{Name: "p"}, Limits: types.GovernanceLimits{LocalSelfDefinedComponents: allowed}}, adoEntraUngraded(), bedrockCredUngraded())
		if c.localSelfDefinedComponents != allowed {
			t.Fatalf("ceiling carried %v for %v", c.localSelfDefinedComponents, allowed)
		}
	}
	srv, st, audit, run := dispatchTeardownFixture(t, &fakeRunner{}, types.RunStarting)
	run.Placement = types.PlacementLocal
	st.run = run
	srv.cfg.Store = localClassifyStore{dispatchTestStore: st, comps: []types.RunComponent{{Name: personal, SelfDefined: true}}}
	ceiling := ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded())
	spec := runner.SandboxSpec{RunID: run.ID}
	if srv.classifyLocalDispatch(context.Background(), run, ceiling, types.SiteConfig{}, &spec, nil, llmTransport{}, false) {
		t.Fatal("a self-defined component passed P3 by default")
	}
	ev := findAudit(audit.events, run.ID, "run.dispatch", "failure")
	if ev == nil || !strings.Contains(string(ev.Data), string(placement.ReasonPlacementComponentSelfDefine)) {
		t.Fatalf("no canonical refusal: %s", auditDump(audit.events, run.ID))
	}
	for _, e := range audit.events {
		if strings.Contains(string(e.Data), personal) {
			t.Fatalf("personal component name reached append-only %s", e.Action)
		}
	}
	if st.failureHint == "" || strings.Contains(st.failureHint, personal) {
		t.Fatalf("failure hint %q", st.failureHint)
	}
}

// The org never chooses a path on the laptop: a stored proxy config's
// DecisionSpoolDir does not reach the laptop-bound spec; the runner sets its own.
func TestLocalStoredSpecNeverCarriesTheDecisionSpoolDir(t *testing.T) {
	b, err := json.Marshal(localStoredSpec(proxy.Config{RunID: uuid.New(), DecisionSpoolDir: "/org/chosen/spool"}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "/org/chosen/spool") {
		t.Fatalf("the laptop-bound spec carries the org's spool path: %s", b)
	}
}

// fix-r1 Opus F2: a stored scope matches the policy copy only when a typed decode reads both alike. jsonb
// reorders keys, so two keys that differ only in case decode by order and must not match; integers past
// 2^53 compare exactly. Spacing and key order alone do not matter.
func TestSameScopeComparesTheTypedDecode(t *testing.T) {
	for _, tc := range []struct {
		name        string
		copy, row   string
		wantSimilar bool
	}{
		{"reordered and respaced", `{"name":"FOO","secret_name":"mine"}`, `{"secret_name": "mine", "name": "FOO"}`, true},
		{"case-variant duplicate key, reordered by jsonb", `{"name":"FOO","secret_name":"mine","Secret_Name":"orgsec"}`,
			`{"name": "FOO", "Secret_Name": "orgsec", "secret_name": "mine"}`, false},
		{"Unicode-folded duplicate key (U+017F), reordered by jsonb", `{"name":"FOO","ſecret_name":"orgsec","secret_name":"mine"}`,
			`{"name": "FOO", "secret_name": "mine", "ſecret_name": "orgsec"}`, false},
		{"integers past 2^53", `{"ttl":9007199254740993}`, `{"ttl": 9007199254740992}`, false},
		{"a different value", `{"name":"FOO"}`, `{"name":"BAR"}`, false},
	} {
		if got := sameScope(json.RawMessage(tc.copy), json.RawMessage(tc.row)); got != tc.wantSimilar {
			t.Errorf("%s: sameScope = %v, want %v", tc.name, got, tc.wantSimilar)
		}
	}
}
