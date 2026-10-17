// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestMetricsListenerHandler pins the dedicated listener's whole surface:
// GET /metrics with no credential answers the body the operator-gated console
// route answers, every other method and path is a 404, and the console route
// still refuses an anonymous scrape.
func TestMetricsListenerHandler(t *testing.T) {
	h := newHarness(t)
	listener := h.srv.MetricsListenerHandler()
	scrape := func(method, path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		listener.ServeHTTP(w, httptest.NewRequest(method, path, nil))
		return w
	}

	if w := do(t, h.srv, http.MethodGet, "/metrics", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous console /metrics = %d, want 401", w.Code)
	}

	got := scrape(http.MethodGet, "/metrics")
	if got.Code != http.StatusOK {
		t.Fatalf("listener GET /metrics = %d, want 200", got.Code)
	}
	if ct := got.Header().Get("Content-Type"); ct != "text/plain; version=0.0.4; charset=utf-8" {
		t.Errorf("listener Content-Type = %q", ct)
	}
	console := do(t, h.srv, http.MethodGet, "/metrics", adminToken, "")
	if console.Code != http.StatusOK {
		t.Fatalf("admin console /metrics = %d, want 200", console.Code)
	}
	if got.Body.String() != console.Body.String() {
		t.Errorf("listener body differs from the console's:\nlistener:\n%s\nconsole:\n%s", got.Body, console.Body)
	}

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/"},
		{http.MethodGet, "/healthz"},
		{http.MethodGet, "/readyz"},
		{http.MethodGet, "/metrics/"},
		{http.MethodGet, "/api/v1/runs"},
		{http.MethodGet, "/wardyn/metrics"},
		{http.MethodPost, "/metrics"},
		{http.MethodHead, "/metrics"},
		{http.MethodDelete, "/metrics"},
	} {
		if w := scrape(tc.method, tc.path); w.Code != http.StatusNotFound {
			t.Errorf("listener %s %s = %d, want 404", tc.method, tc.path, w.Code)
		}
	}
}
