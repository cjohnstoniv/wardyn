// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestDenyRefusalHeaders pins D26: a policy deny carries
// X-Wardyn-Egress: denied + X-Wardyn-Host so the sandbox can tell a permanent
// block from a retry-after-approval pending.
func TestDenyRefusalHeaders(t *testing.T) {
	cu := captureUpstream(t, false, "")
	p, _ := newTestProxy(t, types.RunPolicySpec{
		AllowedDomains: []string{"allowed.test"},
	}, upstreamAddr(cu.srv), nil, nil)

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "http://denied.test/"))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if got := rec.Header().Get("X-Wardyn-Egress"); got != "denied" {
		t.Fatalf("X-Wardyn-Egress = %q, want denied", got)
	}
	if got := rec.Header().Get("X-Wardyn-Host"); got != "denied.test" {
		t.Fatalf("X-Wardyn-Host = %q, want denied.test", got)
	}
}

// TestPendingRefusalHeaders pins D26 for the pending branch: an unknown host
// under deny_with_review carries X-Wardyn-Egress: approval-pending.
func TestPendingRefusalHeaders(t *testing.T) {
	apID := uuid.New()
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: apID, State: types.ApprovalPending})
			return
		}
		http.Error(w, "unexpected", http.StatusTeapot)
	}))
	defer cp.Close()

	ap := newApprovalClient(cp.URL, newTokenSource("tok"), uuid.New(), cp.Client())
	p, _ := newTestProxy(t, types.RunPolicySpec{
		AllowedDomains:   []string{"known.test"},
		FirstUseApproval: types.FirstUseDenyWithReview,
	}, "127.0.0.1:1", ap, nil)

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "http://unknown.test/"))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if got := rec.Header().Get("X-Wardyn-Egress"); got != "approval-pending" {
		t.Fatalf("X-Wardyn-Egress = %q, want approval-pending", got)
	}
	if got := rec.Header().Get("X-Wardyn-Host"); got != "unknown.test" {
		t.Fatalf("X-Wardyn-Host = %q, want unknown.test", got)
	}
}

// TestConnectDenyRefusalHeaders proves the header survives on a CONNECT refusal,
// where the 403 body is discarded — the whole point of using headers (D26).
func TestConnectDenyRefusalHeaders(t *testing.T) {
	p, _ := newTestProxy(t, types.RunPolicySpec{AllowedDomains: []string{"tls.test"}}, "127.0.0.1:1", nil, nil)
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	conn, status := connectThrough(t, proxySrv.URL, "blocked.test:443")
	defer conn.Close()
	if !strings.Contains(status, "403") {
		t.Fatalf("CONNECT deny status = %q, want 403", status)
	}
	if !strings.Contains(status, "X-Wardyn-Egress: denied") {
		t.Fatalf("CONNECT deny missing X-Wardyn-Egress header: %q", status)
	}
	if !strings.Contains(status, "X-Wardyn-Host: blocked.test") {
		t.Fatalf("CONNECT deny missing X-Wardyn-Host header: %q", status)
	}
}

// B4: "denied" alone is ambiguous. EIGHT distinct outcomes collapse into it,
// and they call for completely different actions — ask the operator to allowlist
// a host, versus stop trying because a human already refused, versus fix a
// broken policy. The reason header carries the decision log's own RuleSource so
// the sandbox can tell them apart.
func TestEgressRefusalHeaders_CarryTheReason(t *testing.T) {
	cases := []struct {
		name, reason string
	}{
		{"not on the allowlist", "policy:default-deny"},
		{"an explicit deny-list hit", "policy:denied"},
		{"a human refused the approval", "approval:denied"},
		{"the private-IP guard", "builtin:private-ip"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			log := &egress.DecisionLog{Decision: egress.Deny, RuleSource: tc.reason}
			setEgressRefusalHeadersWithReason(w, egressRefusalDenied, "example.org", decisionReason(log))
			if got := w.Header().Get(egressHeaderStatus); got != egressRefusalDenied {
				t.Errorf("%s = %q, want %q", egressHeaderStatus, got, egressRefusalDenied)
			}
			if got := w.Header().Get(egressHeaderReason); got != tc.reason {
				t.Errorf("%s = %q, want %q — without it every deny looks identical to the sandbox", egressHeaderReason, got, tc.reason)
			}
		})
	}

	// A nil log must not panic and must not invent a reason.
	t.Run("no reason available is omitted, not faked", func(t *testing.T) {
		w := httptest.NewRecorder()
		setEgressRefusalHeadersWithReason(w, egressRefusalPending, "example.org", decisionReason(nil))
		if got := w.Header().Get(egressHeaderReason); got != "" {
			t.Errorf("%s = %q, want empty when no decision log is in scope", egressHeaderReason, got)
		}
		if got := w.Header().Get(egressHeaderStatus); got != egressRefusalPending {
			t.Errorf("the status header must still be set, got %q", got)
		}
	})
}
