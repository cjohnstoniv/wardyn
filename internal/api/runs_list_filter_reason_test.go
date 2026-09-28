// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"
	"testing"
)

// TestListRunsFiltered_Reasons pins the machine-readable `reason` (#656) on
// every GET /runs query-param refusal parseRunsListParams answers. Each row is
// a distinct cause with its own reason constant — a value invalid on its own
// terms (invalid_status_param) never shares a reason with a value that is
// individually valid but conflicts with another param (status_needs_exclusive).
func TestListRunsFiltered_Reasons(t *testing.T) {
	cases := []struct {
		name       string
		query      string
		wantStatus int
		wantReason string
	}{
		{"invalid view", "view=nonsense", http.StatusBadRequest, reasonInvalidViewParam},
		{"invalid owner", "owner=nonsense", http.StatusBadRequest, reasonInvalidOwnerParam},
		{"invalid status", "status=nonsense", http.StatusBadRequest, reasonInvalidStatusParam},
		{"status=needs combined with another status", "status=needs&status=active", http.StatusBadRequest, reasonStatusNeedsExclusive},
		{"status=needs with no view", "status=needs", http.StatusBadRequest, reasonStatusNeedsRequiresView},
		{"invalid ended_within", "ended_within=nonsense", http.StatusBadRequest, reasonInvalidEndedWithinParam},
		{"invalid include_killed", "include_killed=nonsense", http.StatusBadRequest, reasonInvalidIncludeKilledParam},
		{"q too long", "q=" + strings.Repeat("q", maxRunsListQueryLen+1), http.StatusBadRequest, reasonRunsSearchQueryTooLong},
		// A validly-shaped filter still 500s on this harness's nil store, which
		// cannot implement RunsFilteredPager — the SAME reason GET /approvals'
		// sibling capability check answers (#656: one reason per shape, shared
		// across resources).
		{"valid filter, unscoped backend", "view=user", http.StatusInternalServerError, reasonListingUnscopedBackend},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			w := do(t, h.srv, http.MethodGet, "/api/v1/runs?"+tc.query, adminToken, "")
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, tc.wantStatus, w.Body.String())
			}
			if got := errorReason(w); got != tc.wantReason {
				t.Errorf("reason = %q, want %q; body=%s", got, tc.wantReason, w.Body.String())
			}
		})
	}
}
