// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// callAs runs one request as a signed-in person straight through a handler, so
// the actor is a human without standing up OIDC.
func callAs(h http.HandlerFunc, person string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	r = r.WithContext(context.WithValue(r.Context(), oidcHumanCtxKey{}, person))
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

func preflightLimited(w *httptest.ResponseRecorder) bool {
	return w.Code == http.StatusTooManyRequests && errorReason(w) == "preflight_rate_limited"
}

func TestPreflightRateLimit(t *testing.T) {
	h := newHarness(t)
	var mu sync.Mutex
	now := time.Unix(1_700_000_000, 0)
	cfg := baseTestConfig(h, nil)
	cfg.PreflightRatePerMin = 20
	cfg.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	srv := New(cfg)
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }

	for i := 1; i <= 5; i++ {
		if preflightLimited(callAs(srv.handlePreflightRun, "alice")) {
			t.Fatalf("call %d limited, want admitted", i)
		}
	}
	before := len(h.audit.events)
	if w := callAs(srv.handlePreflightRun, "alice"); !preflightLimited(w) {
		t.Fatalf("sixth call = %d %s, want 429 preflight_rate_limited", w.Code, w.Body.String())
	}
	if got := len(h.audit.events); got != before {
		t.Errorf("limited call wrote %d audit row(s), want none", got-before)
	}
	if preflightLimited(callAs(srv.handlePreflightRun, "bob")) {
		t.Error("a second person was limited by the first")
	}
	// 20/min is one token every 3s.
	advance(4 * time.Second)
	if preflightLimited(callAs(srv.handlePreflightRun, "alice")) {
		t.Error("bucket did not refill")
	}
	if !preflightLimited(callAs(srv.handlePreflightRun, "alice")) {
		t.Error("refill granted more than one token")
	}
	// POST /runs is never limited.
	for i := 0; i < 10; i++ {
		if w := callAs(srv.handleCreateRun, "carol"); w.Code == http.StatusTooManyRequests {
			t.Fatalf("POST /runs call %d limited", i)
		}
	}
}

func TestPreflightRateLimitAdminTokenExempt(t *testing.T) {
	h := newHarness(t)
	cfg := baseTestConfig(h, nil)
	cfg.PreflightRatePerMin = 20
	srv := New(cfg)
	for i := 0; i < 8; i++ {
		if w := do(t, srv, http.MethodPost, "/api/v1/runs/preflight", adminToken, `{}`); w.Code == http.StatusTooManyRequests {
			t.Fatalf("admin-token call %d limited", i+1)
		}
	}
}

func TestPreflightRateLimitZeroDisables(t *testing.T) {
	h := newHarness(t)
	srv := New(baseTestConfig(h, nil)) // PreflightRatePerMin 0
	if srv.preflightLimiter != nil {
		t.Fatal("zero rate built a limiter")
	}
	for i := 0; i < 10; i++ {
		if w := callAs(srv.handlePreflightRun, "alice"); w.Code == http.StatusTooManyRequests {
			t.Fatalf("call %d limited with the limit off", i+1)
		}
	}
}
