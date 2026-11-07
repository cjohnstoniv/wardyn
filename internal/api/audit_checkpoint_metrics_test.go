// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auditsign"
)

// TestMetricsAuditCheckpoint: with signing off (nil status) no checkpoint series is written; with a signer,
// lag, last-signed time and every failure reason (zero included) are.
func TestMetricsAuditCheckpoint(t *testing.T) {
	h := newHarness(t)
	w := do(t, New(baseTestConfig(h, nil)), http.MethodGet, "/metrics", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("/metrics = %d, want 200", w.Code)
	}
	if strings.Contains(w.Body.String(), "wardyn_audit_checkpoint_") {
		t.Errorf("/metrics carries checkpoint series with signing off:\n%s", w.Body.String())
	}

	st := &auditsign.Status{}
	st.KeyUnavailable()
	cfg := baseTestConfig(h, nil)
	cfg.AuditCheckpointStatus = st
	w = do(t, New(cfg), http.MethodGet, "/metrics", adminToken, "")
	body := w.Body.String()
	for _, want := range []string{
		"# TYPE wardyn_audit_checkpoint_lag_rows gauge",
		"wardyn_audit_checkpoint_lag_rows 0",
		"# TYPE wardyn_audit_checkpoint_last_timestamp_seconds gauge",
		"wardyn_audit_checkpoint_last_timestamp_seconds 0",
		"# TYPE wardyn_audit_checkpoint_failures_total counter",
		`wardyn_audit_checkpoint_failures_total{reason="key_unavailable"} 1`,
		`wardyn_audit_checkpoint_failures_total{reason="chain_break"} 0`,
		`wardyn_audit_checkpoint_failures_total{reason="foreign_row"} 0`,
		`wardyn_audit_checkpoint_failures_total{reason="write"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics body missing %q:\n%s", want, body)
		}
	}
}
