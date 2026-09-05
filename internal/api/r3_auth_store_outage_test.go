// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSessionRevocationOutageIsCounted is F202, and it is the SSO half of
// TestAPITokenStoreErrorIsCounted's argument.
//
// Both lanes abandon an authentication because a store read failed. Only the
// api-token lane said so. A revocation-store outage 401s every SSO human with
// "missing bearer token", wrote no log line, left wardyn_auth_store_errors_total
// at 0 and wardyn_store_up at 1 — and pushed the flood into
// wardyn_auth_failed_suppressed_total, the series OPERATIONS.md defines as the
// credential-stuffing signature. An operator following their own runbook was
// hunting an attacker during a database incident.
//
// The COUNT IS PER REQUEST, not per audited row, and that is the half worth
// leading with: the auth.failed limiter is 1/sec with a burst of 5, so a
// counter reached only past the limiter would report a tenth of an outage —
// measured, 50 requests produced 5 rows and 45 suppressions.
func TestSessionRevocationOutageIsCounted(t *testing.T) {
	h := newHarness(t)
	r := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil)
	r.RemoteAddr = "203.0.113.7:5555"

	// The same volume the probe drove, in the same second, so the limiter is
	// well past its burst for most of them.
	const requests = 50
	for range requests {
		h.srv.auditAuthFailed(r, sessionRevocationUnavailable)
	}

	body := do(t, h.srv, http.MethodGet, "/metrics", adminToken, "").Body.String()
	if !strings.Contains(body, "wardyn_auth_store_errors_total 50") {
		t.Errorf("want wardyn_auth_store_errors_total 50 after %d requests during a revocation-store outage — "+
			"the SSO lane's outage must be countable in the same series the api-token lane's is, and it must count "+
			"REQUESTS rather than the handful of audit rows the limiter let through.\nbody:\n%s", requests, body)
	}
	// The wrong series must still show the suppressions — the fix is that the
	// outage is ALSO counted where an operator can tell it apart, not that the
	// limiter stopped working.
	if !strings.Contains(body, "wardyn_auth_failed_suppressed_total") {
		t.Errorf("/metrics lost wardyn_auth_failed_suppressed_total:\n%s", body)
	}
}

// TestOrdinaryAuthFailureIsNotAStoreError is the control that makes the pin
// above mean something: a genuine bad credential must NOT move the store-error
// series, or the operator's new signal is as useless as the old one.
func TestOrdinaryAuthFailureIsNotAStoreError(t *testing.T) {
	h := newHarness(t)
	r := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil)
	r.RemoteAddr = "203.0.113.8:5555"
	for range 50 {
		h.srv.auditAuthFailed(r, "invalid_admin_token")
	}
	body := do(t, h.srv, http.MethodGet, "/metrics", adminToken, "").Body.String()
	if !strings.Contains(body, "wardyn_auth_store_errors_total 0") {
		t.Errorf("a credential-stuffing burst moved wardyn_auth_store_errors_total; the two causes must stay "+
			"distinguishable in both directions.\nbody:\n%s", body)
	}
}
