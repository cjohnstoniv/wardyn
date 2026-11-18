// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A paused run is refused at the via_org route for every via_org grant, an Azure DevOps, a GitHub and a plain api_key
// one alike: 409 run_paused, audited with run_state paused. Once the run is resumed the same requests are not refused
// so; the Azure DevOps one then reaches the engine's broker.
func TestHandleViaOrg_RefusesAPausedRunForEveryGrant(t *testing.T) {
	const adoHost, keyHost = "dev.azure.com", "api.example.com"
	srv, ls, rec, run := localDispatchFixture(t, types.PlacementLocal)
	ado := adoViaOrgGrantRow(run.ID, adoHost)
	gh := types.CredentialGrant{ID: uuid.New(), RunID: run.ID, Delivery: types.GrantDeliveryViaOrg, Spec: types.GrantSpec{Kind: types.GrantGitHubToken}}
	keyScope, _ := json.Marshal(map[string]any{"host": keyHost, "header": "X-Api-Key", "secret_name": "EXAMPLE_API_KEY", "require_tls": true})
	key := types.CredentialGrant{ID: uuid.New(), RunID: run.ID, Delivery: types.GrantDeliveryViaOrg, Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: keyScope}}
	ls.dispatchTestStore.grants = []types.CredentialGrant{ado, gh, key}
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
		routes:  []proxy.ViaOrgRoute{{GrantID: ado.ID, Host: adoHost}, {GrantID: gh.ID, Host: githubHost}, {GrantID: key.ID, Host: keyHost}},
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
	adoGit := "/wardyn/git/" + adoHost + "/acme/_git/repo/info/refs"
	requests := []struct {
		name            string
		g               types.CredentialGrant
		rest, authority string
	}{
		{name: "Azure DevOps grant", g: ado, rest: adoGit, authority: "sandbox.internal"},
		{name: "GitHub grant", g: gh, rest: "/wardyn/gh/octocat/hello-world/info/refs", authority: githubHost},
		{name: "api_key grant", g: key, rest: "/v1/models", authority: keyHost + ":443"},
	}

	paused := time.Now()
	ls.dispatchTestStore.run.PausedAt = &paused
	for _, tc := range requests {
		w := serve(tc.g, tc.rest, tc.authority)
		if w.Code != http.StatusConflict || errorReason(w) != string(authz.ReasonRunPaused) {
			t.Errorf("%s on a paused run = %d %q, want 409 %s", tc.name, w.Code, w.Body, authz.ReasonRunPaused)
		}
	}
	rows := 0
	for _, ev := range rec.snapshot() {
		var data map[string]any
		if ev.Action != authz.AuditAction || ev.RunID == nil || *ev.RunID != run.ID || json.Unmarshal(ev.Data, &data) != nil {
			continue
		}
		if data["reason"] == string(authz.ReasonRunPaused) && data["run_state"] == "paused" {
			rows++
		}
	}
	if rows != len(requests) {
		t.Errorf("authz.denied rows with reason %s and run_state paused = %d, want %d", authz.ReasonRunPaused, rows, len(requests))
	}

	ls.dispatchTestStore.run.PausedAt = nil
	for _, tc := range requests {
		if w := serve(tc.g, tc.rest, tc.authority); errorReason(w) == string(authz.ReasonRunPaused) {
			t.Errorf("%s on a resumed run = %d %q, want no %s", tc.name, w.Code, w.Body, authz.ReasonRunPaused)
		}
	}
	if w := serve(ado, adoGit, "sandbox.internal"); !strings.Contains(w.Body.String(), "via_org: not a brokered route") {
		t.Errorf("Azure DevOps git on a resumed run = %d %q, want it served by the engine's broker", w.Code, w.Body)
	}
}
