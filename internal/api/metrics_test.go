// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestMetricsAdminGatedAndCounts covers the scrape surface: it must NOT be
// anonymous (the public API fails closed without a credential, so an open
// /metrics would contradict that posture), and a decision at the service
// chokepoint must land in the exposition.
func TestMetricsAdminGatedAndCounts(t *testing.T) {
	h := newHarness(t)

	if w := do(t, h.srv, http.MethodGet, "/metrics", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous /metrics = %d, want 401", w.Code)
	}

	ap, _ := h.approvals.Request(context.Background(), types.ApprovalRequest{
		RunID: uuid.New(), Kind: types.ApprovalEgressDomain, RequestedScope: json.RawMessage(`{"host":"x"}`),
	})
	if w := do(t, h.srv, http.MethodPost, "/api/v1/approvals/"+ap.ID.String()+"/deny", adminToken, ""); w.Code != http.StatusOK {
		t.Fatalf("deny approval = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	w := do(t, h.srv, http.MethodGet, "/metrics", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("/metrics = %d, want 200", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`wardyn_approval_decisions_total{decision="denied"} 1`,
		"# TYPE wardyn_egress_denies_total counter",
		"wardyn_credential_mints_total 0",
		"wardyn_sandbox_launch_seconds_count 0",
		// No Store and no spool configured: nothing to be down, nothing backed up.
		"wardyn_store_up 1",
		"wardyn_audit_spool_lines 0",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics body missing %q:\n%s", want, body)
		}
	}
}

// TestMetricsHealthGaugesSeeAnOutage covers what the two gauges exist for. Every
// counter above only moves on a SUCCESS path, so a control plane whose Postgres
// has gone away looks exactly like an idle one on a scrape. wardyn_store_up says
// which, and wardyn_audit_spool_lines says how much audit history is stranded on
// local disk waiting to drain back — the C1 fallback working, and visible.
func TestMetricsHealthGaugesSeeAnOutage(t *testing.T) {
	spool, err := NewAuditSpool(filepath.Join(t.TempDir(), "audit-spool.jsonl"))
	if err != nil {
		t.Fatalf("NewAuditSpool: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := spool.Append(types.AuditEvent{ID: uuid.New()}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	h := newHarness(t)
	cfg := baseTestConfig(h, &pingStore{err: errors.New("dial tcp: connection refused")})
	cfg.AuditSpool = spool
	srv := New(cfg)

	w := do(t, srv, http.MethodGet, "/metrics", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("/metrics = %d, want 200 (a dead store must not take the scrape surface with it)", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"# TYPE wardyn_store_up gauge",
		"wardyn_store_up 0",
		"# TYPE wardyn_audit_spool_lines gauge",
		"wardyn_audit_spool_lines 2",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics body missing %q:\n%s", want, body)
		}
	}
}
