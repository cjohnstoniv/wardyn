// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
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
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics body missing %q:\n%s", want, body)
		}
	}
}
