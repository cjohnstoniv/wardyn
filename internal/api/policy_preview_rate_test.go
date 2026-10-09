// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func policyPreviewLimited(w *httptest.ResponseRecorder) bool {
	return w.Code == http.StatusTooManyRequests && errorReason(w) == "policy_preview_rate_limited"
}

func TestPolicyPreviewRateLimit(t *testing.T) {
	h := newHarness(t)
	var mu sync.Mutex
	now := time.Unix(1_700_000_000, 0)
	cfg := baseTestConfig(h, nil)
	cfg.PolicyPreviewRatePerMin = 60
	cfg.PreflightRatePerMin = 60
	cfg.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	srv := New(cfg)
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }

	for i := 1; i <= 15; i++ {
		if policyPreviewLimited(callAs(srv.handlePolicyPreview, "alice")) {
			t.Fatalf("call %d limited, want admitted", i)
		}
	}
	before := len(h.audit.events)
	if got := srv.policyPreviewLimiter.max; got != 16384 {
		t.Fatalf("principal cap %d", got)
	}
	if w := callAs(srv.handlePolicyPreview, "alice"); !policyPreviewLimited(w) {
		t.Fatalf("sixteenth call = %d %s, want 429 policy_preview_rate_limited", w.Code, w.Body.String())
	}
	if w := callAs(srv.handlePolicyPreview, "alice"); w.Header().Get("Retry-After") != "1" {
		t.Fatalf("Retry-After = %q", w.Header().Get("Retry-After"))
	}
	if callAs(srv.handlePreflightRun, "alice").Code == http.StatusTooManyRequests {
		t.Fatal("preview consumed preflight bucket")
	}
	for i := 0; i < 18; i++ {
		callAs(srv.handlePreflightRun, "carol")
	}
	if policyPreviewLimited(callAs(srv.handlePolicyPreview, "carol")) {
		t.Fatal("preflight consumed preview bucket")
	}
	if got := len(h.audit.events); got != before {
		t.Errorf("limited call wrote %d audit row(s), want none", got-before)
	}
	if policyPreviewLimited(callAs(srv.handlePolicyPreview, "bob")) {
		t.Error("a second person was limited by the first")
	}
	// 60/min refills one token per second.
	advance(1100 * time.Millisecond)
	if policyPreviewLimited(callAs(srv.handlePolicyPreview, "alice")) {
		t.Error("bucket did not refill")
	}
	if !policyPreviewLimited(callAs(srv.handlePolicyPreview, "alice")) {
		t.Error("refill granted more than one token")
	}
	// POST /runs is never limited.
	for i := 0; i < 18; i++ {
		if w := callAs(srv.handleCreateRun, "carol"); w.Code == http.StatusTooManyRequests {
			t.Fatalf("POST /runs call %d limited", i)
		}
	}
}

func TestPolicyPreviewRateLimitAdminTokenExempt(t *testing.T) {
	h := newHarness(t)
	cfg := baseTestConfig(h, nil)
	cfg.PolicyPreviewRatePerMin = 60
	srv := New(cfg)
	for i := 0; i < 18; i++ {
		if w := do(t, srv, http.MethodPost, policyPreviewPath, adminToken, `{}`); w.Code == http.StatusTooManyRequests {
			t.Fatalf("admin-token call %d limited", i+1)
		}
	}
}

func TestPolicyPreviewRateLimitZeroDisables(t *testing.T) {
	h := newHarness(t)
	srv := New(baseTestConfig(h, nil)) // PolicyPreviewRatePerMin 0
	if srv.policyPreviewLimiter != nil {
		t.Fatal("zero rate built a limiter")
	}
	for i := 0; i < 18; i++ {
		if w := callAs(srv.handlePolicyPreview, "alice"); w.Code == http.StatusTooManyRequests {
			t.Fatalf("call %d limited with the limit off", i+1)
		}
	}
}

func TestPolicyPreviewRetryAfterRounding(t *testing.T) {
	for _, tc := range []struct {
		rate int
		want string
	}{{7, "9"}, {120, "1"}} {
		h := newHarness(t)
		cfg := baseTestConfig(h, nil)
		cfg.PolicyPreviewRatePerMin = tc.rate
		cfg.Now = func() time.Time { return time.Unix(1_700_000_000, 0) }
		srv := New(cfg)
		for i := 0; i < 15; i++ {
			callAs(srv.handlePolicyPreview, "alice")
		}
		w := callAs(srv.handlePolicyPreview, "alice")
		if !policyPreviewLimited(w) || w.Header().Get("Retry-After") != tc.want {
			t.Fatalf("rate%d: %d Retry-After=%q", tc.rate, w.Code, w.Header().Get("Retry-After"))
		}
	}
}
