// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
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

// TestMetricsSinkDrops covers the D2 wiring: when AuditSinkDrops is set (the
// cmd/wardynd audit Fanout), /metrics exposes wardyn_audit_sink_drops_total per
// sink, so a SIEM sink silently shedding events is visible on the scrape surface.
// Nil AuditSinkDrops (no SIEM configured) omits the series — asserted by the
// suites above, whose bodies never carry it.
func TestMetricsSinkDrops(t *testing.T) {
	h := newHarness(t)
	cfg := baseTestConfig(h, nil)
	cfg.AuditSinkDrops = func() map[string]int64 {
		return map[string]int64{"webhook": 7, "syslog": 0}
	}
	srv := New(cfg)

	w := do(t, srv, http.MethodGet, "/metrics", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("/metrics = %d, want 200", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"# TYPE wardyn_audit_sink_drops_total counter",
		`wardyn_audit_sink_drops_total{sink="webhook"} 7`,
		`wardyn_audit_sink_drops_total{sink="syslog"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics body missing %q:\n%s", want, body)
		}
	}
}

// ─── the auth lane's silent failures (group B) ───────────────────────────────

// TestAuthFailedSuppressionIsCounted is the whole point of the rate limiter
// being safe to have. auditAuthFailed caps auth.failed audit rows at ~1/sec
// with a small burst, so past that burst the audit trail STOPS describing the
// volume it is bounding — a credential-stuffing run and a handful of typos
// leave the same handful of rows, and the attack reads QUIETER the harder it is
// pushed. The suppressed emits therefore have to be countable, not merely
// dropped: a flat auth.failed row count with this series climbing is the
// signal, and it is a series so it can be alerted on rather than grepped for.
//
// Counterfactual: `return` without the increment in auditAuthFailed and the
// suppressed count stays 0 while 200 failures produce ~5 audit rows — the
// 195-event blind spot this pins.
func TestAuthFailedSuppressionIsCounted(t *testing.T) {
	h := newHarness(t)

	// The clock does not advance, so the bucket never refills: exactly
	// authFailedBurst emits are admitted and every further one is dropped.
	const attempts = 200
	for i := 0; i < attempts; i++ {
		if w := do(t, h.srv, http.MethodGet, "/api/v1/runs", "wrong-token", ""); w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i, w.Code)
		}
	}

	rows := 0
	for _, ev := range h.audit.events {
		if ev.Action == "auth.failed" {
			rows++
		}
	}
	if rows == 0 || rows >= attempts {
		t.Fatalf("auth.failed rows = %d of %d attempts; the fixture must actually exercise SUPPRESSION "+
			"(some admitted, most dropped) or this test proves nothing", rows, attempts)
	}

	w := do(t, h.srv, http.MethodGet, "/metrics", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("/metrics = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "# TYPE wardyn_auth_failed_suppressed_total counter") {
		t.Errorf("/metrics carries no wardyn_auth_failed_suppressed_total series:\n%s", body)
	}
	// Every attempt is either an audit row or a counted suppression. A drop that
	// is neither is exactly the blind spot.
	wantSuppressed := attempts - rows
	if !strings.Contains(body, fmt.Sprintf("wardyn_auth_failed_suppressed_total %d", wantSuppressed)) {
		t.Errorf("want wardyn_auth_failed_suppressed_total %d (%d attempts - %d audited rows); body:\n%s",
			wantSuppressed, attempts, rows, body)
	}
}

// TestAPITokenStoreErrorIsCounted covers the other half: a failure that 500s an
// entire authentication lane while leaving no audit row (there is no
// authenticated principal to attribute one to), no log line from writeError, and
// — the part worth leading with — wardyn_store_up still scraping 1.
//
// That gauge is not lying. It answers a PING, and a pool that pings can still
// fail an individual query: one table denying a read, one statement timing out.
// A health signal that stays green through an outage it appears to cover argues
// AGAINST the operator's own evidence, so the fix is to say what the gauge
// asserts and give the lane its own series.
//
// Counterfactual: drop the authStoreErrorInc call and this lane 500s with every
// metric flat and wardyn_store_up green — observable only from the client side.
func TestAPITokenStoreErrorIsCounted(t *testing.T) {
	h := newHarness(t)
	boom := errors.New("pg: permission denied for table api_tokens")
	h.srv.cfg.Store = &apiTokenErrStore{err: boom}

	w := do(t, h.srv, http.MethodGet, "/api/v1/runs", "wdn_deadbeefdeadbeefdeadbeefdeadbeef", "")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("token lookup failure = %d, want 500; body=%s", w.Code, w.Body.String())
	}

	m := do(t, h.srv, http.MethodGet, "/metrics", adminToken, "")
	body := m.Body.String()
	if !strings.Contains(body, "wardyn_auth_store_errors_total 1") {
		t.Errorf("want wardyn_auth_store_errors_total 1 after a lookup failure; body:\n%s", body)
	}
	// The gauge's own claim, now stated rather than implied: it pings.
	if !strings.Contains(body, "wardyn_store_up") {
		t.Fatalf("/metrics lost wardyn_store_up:\n%s", body)
	}
	if !strings.Contains(body, "Reachability only") {
		t.Errorf("wardyn_store_up's HELP does not say what it actually asserts — an operator reading it as " +
			"'the store works' is why this failure looked invisible")
	}
}

// apiTokenErrStore fails ONLY the api-token lookup, which is the shape that
// makes the point: the pool is fine, the ping passes, one query does not.
type apiTokenErrStore struct {
	store.Store
	err error
}

func (s *apiTokenErrStore) GetAPITokenByRaw(context.Context, string) (types.APIToken, error) {
	return types.APIToken{}, s.err
}
func (s *apiTokenErrStore) Ping(context.Context) error { return nil }
