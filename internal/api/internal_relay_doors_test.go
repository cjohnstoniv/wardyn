// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// A runner's relay carries a local run's proxy traffic to the org's internal
// listener and nothing else. Every human entry to a run is a route the relay
// filter refuses, so a runner cannot reach a terminal, a take-over, a ticket
// mint or the UI gateway through its own stream.

// TestRelay_NeverReachesAHumanEntryRoute sends each human door through the relay
// filter with the strongest credential there is (the admin token). The filter
// answers relay_route_refused and the router behind it is never reached.
func TestRelay_NeverReachesAHumanEntryRoute(t *testing.T) {
	srv := New(Config{AdminToken: adminToken, Audit: &syncAudit{}})
	reached := false
	h := srv.relayRoutes(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	run := uuid.New().String()
	base := "/api/v1/runs/" + run
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, base + "/attach?ticket=x"},
		{http.MethodPost, base + "/attach-ticket"},
		{http.MethodPost, base + "/attach/ticket"},
		{http.MethodPost, base + "/attach/takeover"},
		{http.MethodGet, base + "/attach-holder"},
		{http.MethodGet, base + "/sign-in"},
		{http.MethodGet, base + "/files"},
		{http.MethodGet, uiEnterPath + "?run=" + run + "&app=code&ticket=x"},
		{http.MethodGet, "/api/v1/internal/../runs/" + run + "/attach"},
		{http.MethodGet, "/api/v1/internal/%2e%2e/runs/" + run + "/attach"},
	} {
		reached = false
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("Authorization", "Bearer "+adminToken)
		req = req.WithContext(relayConnContext(req.Context(), relayedConn{runner: uuid.New()}))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden || reasonOf(t, w) != string(placement.ReasonRelayRouteRefused) {
			t.Errorf("%s %s = %d %s, want 403 relay_route_refused", tc.method, tc.path, w.Code, w.Body.String())
		}
		if reached {
			t.Errorf("%s %s reached the router through the relay", tc.method, tc.path)
		}
	}
}

// TestRelayAllowList_IsInternalOnly: no route the relay serves is outside
// /api/v1/internal/, whatever is classified later.
func TestRelayAllowList_IsInternalOnly(t *testing.T) {
	for route, served := range relayedInternalRoutes {
		_, pattern, _ := strings.Cut(route, " ")
		if !strings.HasPrefix(pattern, "/api/v1/internal/") {
			t.Errorf("relayedInternalRoutes names %q, outside /api/v1/internal/ (served=%v)", route, served)
		}
		if strings.Contains(pattern, "/runs/") {
			t.Errorf("relayedInternalRoutes names %q: a human run route", route)
		}
	}
}

// TestRelayedViaOrgRoutesAreInternalOnly: the via_org door is the relay's
// alone. Every route that serves it sits under /api/v1/internal/ (run-token
// internalAuth) on the relay allow-list, and a human's admin token is refused.
func TestRelayedViaOrgRoutesAreInternalOnly(t *testing.T) {
	recordings, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := New(Config{AdminToken: adminToken, Identity: mustIDP(t), Secrets: getErrStore{getErr: secretstore.ErrNotFound}, RecordingStore: recordings})
	var served []string
	walk := func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if strings.Contains(route, "via-org") || strings.Contains(route, "via_org") {
			served = append(served, method+" "+route)
		}
		return nil
	}
	if err := chi.Walk(srv.router, walk); err != nil {
		t.Fatal(err)
	}
	if len(served) != len(viaOrgMethods) {
		t.Errorf("via_org routes = %v, want one per method in %v", served, viaOrgMethods)
	}
	for _, route := range served {
		if _, path, _ := strings.Cut(route, " "); path != "/api/v1"+viaOrgRoutePattern {
			t.Errorf("%s serves via_org outside the internal surface", route)
		}
		if !relayedInternalRoutes[route] {
			t.Errorf("%s is not on the relay allow-list", route)
		}
	}
	for _, m := range viaOrgMethods {
		r := httptest.NewRequest(m, "/api/v1/internal/via-org/"+uuid.NewString()+"/repos", nil)
		r.Header.Set("Authorization", "Bearer "+adminToken)
		w := httptest.NewRecorder()
		panicFails(t, srv.Handler()).ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s via_org with a human admin token = %d %s, want 401", m, w.Code, w.Body.String())
		}
	}
}
