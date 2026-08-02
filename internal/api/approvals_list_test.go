// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
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

// pagedApprovals wraps a harness ApprovalService with the OPTIONAL
// approvalPageLister, recording the Page the handler pushed down — proving the
// un-filtered list rides the DB-paged branch instead of fetch-all.
type pagedApprovals struct {
	ApprovalService
	gotPage store.Page
	rows    []types.ApprovalRequest
}

func (p *pagedApprovals) ListApprovalsPage(_ context.Context, _ types.ApprovalState, pg store.Page) ([]types.ApprovalRequest, error) {
	p.gotPage = pg
	if len(p.rows) > pg.Limit {
		return p.rows[:pg.Limit], nil
	}
	return p.rows, nil
}

// TestListApprovals_PagedLister pins the threaded-Page path: a lister that
// implements approvalPageLister gets LIMIT+1/OFFSET pushed to it (servePage's
// truncation probe), and the response is windowed + marked truncated. ?run_id=
// deliberately bypasses it (the filter must run before windowing).
func TestListApprovals_PagedLister(t *testing.T) {
	h := newHarness(t)
	rows := make([]types.ApprovalRequest, 3)
	for i := range rows {
		rows[i] = types.ApprovalRequest{ID: uuid.New(), RunID: uuid.New(), Kind: types.ApprovalEgressDomain}
	}
	pl := &pagedApprovals{ApprovalService: h.srv.cfg.Approvals, rows: rows}
	h.srv.cfg.Approvals = pl

	w := do(t, h.srv, http.MethodGet, "/api/v1/approvals?limit=2", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d; body=%s", w.Code, w.Body.String())
	}
	if pl.gotPage.Limit != 3 { // limit+1 truncation probe
		t.Errorf("pushed-down page = %+v, want Limit=3 (2+1 probe)", pl.gotPage)
	}
	var got []types.ApprovalRequest
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 || w.Header().Get("X-Wardyn-Truncated") != "true" {
		t.Errorf("got %d rows, truncated=%q; want 2 rows + truncated", len(got), w.Header().Get("X-Wardyn-Truncated"))
	}

	// run_id set -> the paged lister must NOT be consulted (filter-then-window).
	pl.gotPage = store.Page{}
	if w := do(t, h.srv, http.MethodGet, "/api/v1/approvals?run_id="+uuid.New().String(), adminToken, ""); w.Code != http.StatusOK {
		t.Fatalf("run_id path code = %d", w.Code)
	}
	if pl.gotPage != (store.Page{}) {
		t.Errorf("run_id path consulted the paged lister with %+v; must use fetch-all", pl.gotPage)
	}
}
