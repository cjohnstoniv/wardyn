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
		p, err := f.srv.localResolvedPlan(context.Background(), run, false, types.SiteConfig{}, spec, nil, llmTransport{}, adoEntraRun{})
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
		cfg := proxy.Config{RunID: run.ID, UpstreamProxyURL: "https://user:password@parent", TrustedCAPEM: "corporate", InternalHosts: []types.InternalHost{{HostSuffix: "corp"}}, LLMUpstreams: map[string]string{"vendor": "corp"}}
		ref := f.srv.reviveOwnerRecheck(context.Background(), run, &cfg, types.ActorSystem, "wardynd")
		want := placement.ReasonPlacementUnavailable
		if secretRef != "" {
			want = placement.ReasonPlacementCredential
		}
		if ref == nil || ref.reason != string(want) {
			t.Fatalf("revive=%v wanted %s", ref, want)
		}
		if secretRef == "" && (cfg.UpstreamProxyURL != "" || cfg.TrustedCAPEM != "" || len(cfg.InternalHosts)+len(cfg.LLMUpstreams) != 0) {
			t.Fatal("revive retained organisation network configuration")
		}
	}
}

func TestLocalAllModelProviderKindsHaveOwnClassification(t *testing.T) {
	f := newComponentFixture(t)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(utCtx("user", "", nil))
	for kind := range types.ClosedModelProviderKinds {
		intent, known := f.srv.localProviderIntent(r, capSub, types.ModelProvider{ID: "selected", UID: "uid", Kind: kind})
		if !known || intent.Origin.Class != placement.ClassOwn || !intent.Origin.Stored || intent.Origin.OwnNamespace {
			t.Fatalf("provider %s is unclassified or missing namespace allowed: %+v", kind, intent)
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
	if srv.classifyLocalDispatch(context.Background(), run, ceiling, types.SiteConfig{}, &spec, nil, llmTransport{}, adoEntraRun{}, false) {
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
