// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestListApprovals_RunIDFilter pins the ?run_id= predicate. Without it a run's
// Approvals tab pulls the WHOLE fleet's list and filters in the browser, capped
// at maxListLimit over a requested_at DESC read — so past that many lifetime
// approvals an older run's own approvals vanish from its detail page and its
// PENDING badge reads 0. The filter runs inside the fetch-all closure, i.e.
// before servePage windows the result, which is what actually fixes that.
func TestListApprovals_RunIDFilter(t *testing.T) {
	h := newHarness(t)
	mine, other := uuid.New(), uuid.New()
	for _, rid := range []uuid.UUID{mine, other} {
		if _, err := h.approvals.Request(context.Background(), types.ApprovalRequest{
			RunID: rid, Kind: types.ApprovalEgressDomain,
		}); err != nil {
			t.Fatalf("seed approval for %s: %v", rid, err)
		}
	}

	w := do(t, h.srv, http.MethodGet, "/api/v1/approvals?run_id="+mine.String(), adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got []types.ApprovalRequest
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0].RunID != mine {
		t.Errorf("got %d approval(s) %+v, want only the requested run's", len(got), got)
	}

	if w := do(t, h.srv, http.MethodGet, "/api/v1/approvals?run_id=not-a-uuid", adminToken, ""); w.Code != http.StatusBadRequest {
		t.Errorf("malformed run_id: code = %d, want 400", w.Code)
	}
}
