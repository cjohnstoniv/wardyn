// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
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
		p, err := f.srv.localResolvedPlan(context.Background(), run, dispatchCeiling{}, types.SiteConfig{}, spec, nil, llmTransport{}, adoEntraRun{})
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
