// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"testing"
)

// TestListApprovals_QueryParamReasons pins the machine-readable `reason`
// (#656) on GET /approvals' own query-param refusals, beyond ?view= (already
// pinned by TestListApprovals_InvalidView). ?limit=/?offset= go through
// parseListPage, shared by every paginated list route in this package (GET
// /runs, /audit, /policies, /secrets, ...) — one reason for that one shape,
// wherever it is asked.
func TestListApprovals_QueryParamReasons(t *testing.T) {
	cases := []struct {
		name       string
		query      string
		wantReason string
	}{
		{"invalid state", "state=bogus", reasonInvalidApprovalState},
		{"invalid run_id", "run_id=not-a-uuid", reasonInvalidRunIDParam},
		{"invalid limit", "limit=-1", reasonInvalidLimitParam},
		{"invalid offset", "offset=-1", reasonInvalidOffsetParam},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			w := do(t, h.srv, http.MethodGet, "/api/v1/approvals?"+tc.query, adminToken, "")
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
			}
			if got := errorReason(w); got != tc.wantReason {
				t.Errorf("reason = %q, want %q; body=%s", got, tc.wantReason, w.Body.String())
			}
		})
	}
}
