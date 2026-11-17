// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// handleViaOrg serves /wardyn/git/ only for an Azure DevOps grant whose own host the path names, whatever authority
// the runner reports for an origin-form request, and hands it to the engine's broker; /wardyn/gh/ goes only with the
// GitHub grant. The engine holds no Azure DevOps grant, so its broker answers "not a brokered route": that answer
// is the broker's, never the header-injected forward's.
func TestHandleViaOrg_ADOGitRouteIsTheGrantsAndBrokered(t *testing.T) {
	const host = "dev.azure.com"
	srv, ls, _, run := localDispatchFixture(t, types.PlacementLocal)
	ado := adoViaOrgGrantRow(run.ID, host)
	gh := types.CredentialGrant{ID: uuid.New(), RunID: run.ID, Delivery: types.GrantDeliveryViaOrg, Spec: types.GrantSpec{Kind: types.GrantGitHubToken}}
	ls.dispatchTestStore.grants = []types.CredentialGrant{ado, gh}
	eng, err := proxy.NewViaOrgEngine(context.Background(), &proxy.Config{
		RunID: run.ID, ControlPlaneURL: "http://127.0.0.1:9", RunToken: "run-token", DecisionBufferSize: 16,
	}, proxy.ViaOrgEngineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Close(context.Background()) })
	runnerID := uuid.New()
	srv.viaOrg.runs = map[uuid.UUID]*viaOrgRun{run.ID: {id: run.ID, runner: runnerID, bytes: new(atomic.Int64), done: make(chan struct{}),
		streams: map[*viaOrgStream]struct{}{}, idleSince: time.Now(), engine: &viaOrgGen{engine: eng},
		routes:  []proxy.ViaOrgRoute{{GrantID: ado.ID, Host: host}, {GrantID: gh.ID, Host: githubHost}},
		byteCap: viaOrgMaxBytes, idle: time.Minute, period: time.Hour}}
	serve := func(g types.CredentialGrant, rest, authority string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, proxy.ViaOrgPathPrefix+g.ID.String()+rest, nil)
		r.Header.Set(proxy.ViaOrgAuthorityHeader, authority)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("grantID", g.ID.String())
		ctx := context.WithValue(r.Context(), chi.RouteCtxKey, rctx)
		ctx = context.WithValue(ctx, claimsCtxKey{}, &identity.Claims{RunID: run.ID})
		ctx = context.WithValue(ctx, relayRunnerKey{}, runnerID)
		w := httptest.NewRecorder()
		srv.handleViaOrg(w, r.WithContext(ctx))
		return w
	}
	refused := func(w *httptest.ResponseRecorder) bool {
		return strings.Contains(w.Body.String(), string(placement.ReasonViaOrgDestinationRefused))
	}

	// The sandbox's own Host header rides as the authority; the path names the grant's host.
	w := serve(ado, "/wardyn/git/"+host+"/acme/_git/repo/info/refs", "sandbox.internal")
	if refused(w) || !strings.Contains(w.Body.String(), "via_org: not a brokered route") {
		t.Fatalf("git route on the Azure DevOps grant = %d %q, want it admitted and served by the engine's broker", w.Code, w.Body)
	}
	for _, tc := range []struct {
		name string
		g    types.CredentialGrant
		rest string
	}{
		{name: "git route naming another host", g: ado, rest: "/wardyn/git/other.example.com/acme/_git/repo/info/refs"},
		{name: "gh route on the Azure DevOps grant", g: ado, rest: "/wardyn/gh/octocat/hello-world/info/refs"},
		{name: "git route on the GitHub grant", g: gh, rest: "/wardyn/git/" + host + "/acme/_git/repo/info/refs"},
	} {
		if w := serve(tc.g, tc.rest, host); w.Code != http.StatusForbidden || !refused(w) {
			t.Errorf("%s = %d %q, want 403 %s", tc.name, w.Code, w.Body, placement.ReasonViaOrgDestinationRefused)
		}
	}
}

// declSource is the source text of the function or method name declared in file, one of this package's.
func declSource(t *testing.T, file, name string) string {
	t.Helper()
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, src, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == name {
			return string(src[fset.Position(fd.Pos()).Offset:fset.Position(fd.End()).Offset])
		}
	}
	t.Fatalf("%s declares no %s", file, name)
	return ""
}

// inOrder reports whether src holds each of parts, each after the one before.
func inOrder(src string, parts ...string) bool {
	at := 0
	for _, p := range parts {
		i := strings.Index(src[at:], p)
		if i < 0 {
			return false
		}
		at += i + len(p)
	}
	return true
}

// Dispatch judges the dispatched spec (mixed Azure DevOps delivery included) and seals the gate it read before the
// laptop copy dropped it: one write of the sealed row, after the classification.
func TestClassifyLocalDispatch_JudgesTheDispatchedSpecAndSealsItsGate(t *testing.T) {
	src := declSource(t, "run_placement_spec.go", "classifyLocalDispatch")
	if !inOrder(src, "s.localResolvedPlan(ctx, run, ceiling.localSelfDefinedComponents, sc, *spec,", "placement.LocalEligibility(p)") {
		t.Error("classifyLocalDispatch does not judge the plan of the dispatched spec")
	}
	if !inOrder(src, "adoGate := spec.ProxyConfig.ADOGrant", "placement.LocalEligibility(p)", "*spec = stripped",
		"s.keepRunProxyConfig(ctx, run.ID, withADOGate(withInspectionCorpus(spec.ProxyConfig, corpus), adoGate))") {
		t.Error("the sealed row is not written with the gate read before the laptop copy dropped it")
	}
	if n := strings.Count(src, "keepRunProxyConfig("); n != 1 {
		t.Errorf("classifyLocalDispatch writes the run's proxy config %d times, want once", n)
	}
}

// A revive reads the gate after the ceiling kept or withdrew the lane and before the owner recheck re-classifies the
// laptop copy (which drops it), and stores the row with it.
func TestReviveRunProxy_ReadsTheGateBetweenTheCeilingAndTheLaptopCopy(t *testing.T) {
	src := declSource(t, "run_revive.go", "reviveRunProxy")
	if !inOrder(src, "reassertProxyCeiling(", "adoGate := cfg.ADOGrant", "s.reviveOwnerRecheck(ctx, run, cfg,",
		"revivedStoredConfig(ctx, run, cfg, corpus, adoGate,") {
		t.Error("reviveRunProxy does not read the gate between the ceiling and the owner recheck, or stores the row without it")
	}
}

// The one engine build takes the engine options, so the run's engine is built with the 540 s hold ceiling.
func TestViaOrgEngine_IsBuiltWithTheEngineOptions(t *testing.T) {
	if src := declSource(t, "via_org.go", "viaOrgEngine"); !strings.Contains(src, "proxy.NewViaOrgEngine(s.cfg.BaseCtx, cfg, s.viaOrgEngineOptions(run))") {
		t.Error("viaOrgEngine does not build the engine with viaOrgEngineOptions")
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	builds := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		src, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatal(err)
		}
		builds += strings.Count(string(src), "proxy.NewViaOrgEngine(")
	}
	if builds != 1 {
		t.Errorf("%d engine builds in the package, want the one in viaOrgEngine", builds)
	}
}
