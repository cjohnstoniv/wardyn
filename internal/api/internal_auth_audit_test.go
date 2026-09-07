// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// authFailedRows returns the auth.failed rows recorded so far, with their
// principal and reason decoded.
func authFailedRows(t *testing.T, h *harness) []struct{ Actor, Reason, Target string } {
	t.Helper()
	var out []struct{ Actor, Reason, Target string }
	for _, ev := range h.audit.events {
		if ev.Action != "auth.failed" {
			continue
		}
		var d struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		out = append(out, struct{ Actor, Reason, Target string }{ev.Actor, d.Reason, ev.Target})
	}
	return out
}

// TestInternalLaneRefusalsAreAudited is the pin for F068.
//
// internalAuth and internalAuthGroundtruth are the trust boundary between the
// UNTRUSTED sandbox / host sensor and the control plane, and
// handleInternalRequestApproval's kind switch is the guard against a sidecar
// forging a `credential` approval — the exact thing its own comment says must
// never come from an untrusted sidecar. All of them answered 401/400 and
// recorded NOTHING: no audit row, no log line, no metric. A process inside a
// sandbox brute-forcing run tokens against /api/v1/internal/*, or a compromised
// sidecar probing for the credential-approval path, left no trace anywhere,
// while the public lane has audited every one of its 401s as auth.failed since
// #19a.
//
// Each refusal now emits the SAME rate-bound auth.failed row the public lane
// does, with a principal that says WHICH boundary refused (the internal lane and
// the public lane are different incidents with different runbooks) and a
// bounded reason.
func TestInternalLaneRefusalsAreAudited(t *testing.T) {
	runID := uuid.New()
	goodRunTok := "" // minted per-subtest where needed

	for _, tc := range []struct {
		name         string
		method, path string
		bearer       string
		body         string
		wantStatus   int
		wantActor    string
		wantReason   string
	}{
		{
			name: "no bearer on the run-token lane", method: http.MethodPost, path: "/api/v1/internal/approvals",
			wantStatus: http.StatusUnauthorized, wantActor: "wardyn/internalAuth", wantReason: "missing_run_token",
		},
		{
			name: "forged run token", method: http.MethodPost, path: "/api/v1/internal/approvals",
			bearer:     "not-a-real-token",
			wantStatus: http.StatusUnauthorized, wantActor: "wardyn/internalAuth", wantReason: "invalid_run_token",
		},
		{
			name: "no bearer on the host-sensor lane", method: http.MethodPost, path: "/api/v1/internal/groundtruth",
			wantStatus: http.StatusUnauthorized, wantActor: "wardyn/internalAuthGroundtruth", wantReason: "missing_sensor_token",
		},
		{
			name: "a run token presented to the host-sensor lane (wrong audience)", method: http.MethodPost,
			path: "/api/v1/internal/groundtruth", bearer: "@runtoken",
			wantStatus: http.StatusUnauthorized, wantActor: "wardyn/internalAuthGroundtruth", wantReason: "invalid_sensor_token",
		},
		{
			name: "sidecar forging a credential approval", method: http.MethodPost, path: "/api/v1/internal/approvals",
			bearer: "@runtoken", body: `{"kind":"credential","requested_scope":{"secret_name":"anthropic-api-key"}}`,
			wantStatus: http.StatusBadRequest, wantActor: "wardyn/internalApproval",
			wantReason: "unsupported_internal_approval_kind",
		},
		{
			name: "empty requested_scope", method: http.MethodPost, path: "/api/v1/internal/approvals",
			bearer: "@runtoken", body: `{"kind":"egress_domain"}`,
			wantStatus: http.StatusBadRequest, wantActor: "wardyn/internalApproval", wantReason: "missing_requested_scope",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			bearer := tc.bearer
			if bearer == "@runtoken" {
				goodRunTok = h.mintRunToken(t, runID)
				bearer = goodRunTok
			}
			rr := do(t, h.srv, tc.method, tc.path, bearer, tc.body)
			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rr.Code, tc.wantStatus, rr.Body.String())
			}
			rows := authFailedRows(t, h)
			if len(rows) == 0 {
				t.Fatalf("the internal lane refused this request (%d) and recorded NO auth.failed row — "+
					"a sandbox process brute-forcing run tokens, or a sidecar probing for the credential-approval "+
					"path, leaves no trace anywhere", rr.Code)
			}
			last := rows[len(rows)-1]
			if last.Actor != tc.wantActor {
				t.Errorf("auth.failed actor = %q, want %q — the row must say WHICH boundary refused: "+
					"credential stuffing on the public lane and a probing sidecar on the internal one are "+
					"different incidents", last.Actor, tc.wantActor)
			}
			if last.Reason != tc.wantReason {
				t.Errorf("auth.failed reason = %q, want %q", last.Reason, tc.wantReason)
			}
			if last.Target != tc.path {
				t.Errorf("auth.failed target = %q, want the request path %q", last.Target, tc.path)
			}
		})
	}
}

// TestInternalLaneRefusalsShareTheLimiterAndCounter pins that the internal lane
// got the SAME rate-bound emit as the public one, not a second unbounded copy:
// a burst of refusals produces a handful of rows and counts the rest in
// wardyn_auth_failed_suppressed_total, so the audit log cannot be flooded from
// inside a sandbox and the real volume is still visible on /metrics.
func TestInternalLaneRefusalsShareTheLimiterAndCounter(t *testing.T) {
	h := newHarness(t)
	const requests = 50
	for range requests {
		do(t, h.srv, http.MethodPost, "/api/v1/internal/approvals", "not-a-real-token", "")
	}
	rows := authFailedRows(t, h)
	if len(rows) == 0 {
		t.Fatalf("a 50-request run-token brute force produced no auth.failed rows at all")
	}
	if len(rows) >= requests {
		t.Errorf("auth.failed rows = %d for %d refusals; the internal lane must share the public lane's "+
			"~1/sec limiter, or a sandbox can flood the append-only audit log", len(rows), requests)
	}
	body := do(t, h.srv, http.MethodGet, "/metrics", adminToken, "").Body.String()
	if strings.Contains(body, "wardyn_auth_failed_suppressed_total 0\n") ||
		!strings.Contains(body, "wardyn_auth_failed_suppressed_total") {
		t.Errorf("wardyn_auth_failed_suppressed_total did not move past 0 during a %d-request internal-lane "+
			"burst; the dropped rows must stay countable.\nbody:\n%s", requests, body)
	}
}
