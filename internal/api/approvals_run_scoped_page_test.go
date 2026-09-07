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

// runScopedApprovals records which read surface the handler actually used for a
// ?run_id= list: the UNBOUNDED List (the whole approvals table, filtered in Go)
// or the run-scoped paged reader.
type runScopedApprovals struct {
	ApprovalService
	listCalls  int // calls to the unbounded List
	byRunCalls int
	gotRunID   uuid.UUID
	gotPage    store.Page
	gotState   types.ApprovalState
	rows       []types.ApprovalRequest // every row in the "table", across runs
}

func (a *runScopedApprovals) List(_ context.Context, _ types.ApprovalState) ([]types.ApprovalRequest, error) {
	a.listCalls++
	return a.rows, nil
}

// ListApprovalsPageByRun is the capability under test. Declared as a plain
// method (not via the store interface) so this file compiles against a tree
// that does not have that interface yet — the red is then an assertion, not a
// build failure.
func (a *runScopedApprovals) ListApprovalsPageByRun(_ context.Context, runID uuid.UUID, state types.ApprovalState, p store.Page) ([]types.ApprovalRequest, error) {
	a.byRunCalls++
	a.gotRunID, a.gotState, a.gotPage = runID, state, p
	out := make([]types.ApprovalRequest, 0, len(a.rows))
	for _, ap := range a.rows {
		if ap.RunID == runID && (state == "" || ap.State == state) {
			out = append(out, ap)
		}
	}
	if p.Limit > 0 && len(out) > p.Limit {
		out = out[:p.Limit]
	}
	return out, nil
}

// TestListApprovals_RunScopedReadIsBounded is the pin for F072.
//
// handleListApprovals installed the DB-paged reader only when run_id was
// ABSENT. With ?run_id= set, pageFn was nil, so servePage took the allFn branch
// -> Approvals.List -> store.ListApprovals -> ListApprovalsPage(ctx, state,
// Page{}) -> Page.appendTo with Limit<=0, which emits NO LIMIT clause. One
// run-scoped request therefore materialised EVERY approval row the deployment
// had ever written, in Go, and discarded all but one run's — on a path the CLI
// (`wardyn approvals --run`) and the console's run detail page poll, over a
// table whose decided rows are never deleted.
//
// The pin holds the property, not a row count: the run-scoped read must go to
// the run-scoped, WINDOWED reader, carrying the run id, the state filter and a
// bounded Limit — and must not fetch the whole table.
func TestListApprovals_RunScopedReadIsBounded(t *testing.T) {
	h := newHarness(t)
	mine, other := uuid.New(), uuid.New()
	rows := []types.ApprovalRequest{
		{ID: uuid.New(), RunID: mine, Kind: types.ApprovalEgressDomain, State: types.ApprovalPending},
		{ID: uuid.New(), RunID: other, Kind: types.ApprovalEgressDomain, State: types.ApprovalPending},
		{ID: uuid.New(), RunID: other, Kind: types.ApprovalToolCall, State: types.ApprovalApproved},
	}
	a := &runScopedApprovals{ApprovalService: h.srv.cfg.Approvals, rows: rows}
	h.srv.cfg.Approvals = a

	w := do(t, h.srv, http.MethodGet, "/api/v1/approvals?run_id="+mine.String()+"&state=PENDING&limit=25", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d; body=%s", w.Code, w.Body.String())
	}

	if a.listCalls != 0 {
		t.Errorf("a run-scoped ?run_id= list called the UNBOUNDED List %d time(s): that read is "+
			"ListApprovalsPage(ctx, state, Page{}), and Page.appendTo emits no LIMIT for Limit<=0 — so the "+
			"whole approvals table (which nothing prunes) is materialised in Go on every poll of a run's "+
			"detail page", a.listCalls)
	}
	if a.byRunCalls == 0 {
		t.Fatalf("the run-scoped paged reader was never consulted; the filter must run AT THE DB")
	}
	if a.gotRunID != mine {
		t.Errorf("pushed-down run id = %s, want %s", a.gotRunID, mine)
	}
	if a.gotState != types.ApprovalPending {
		t.Errorf("pushed-down state = %q, want PENDING (the state filter must ride the same query)", a.gotState)
	}
	if a.gotPage.Limit <= 0 {
		t.Errorf("pushed-down page = %+v, want a positive Limit — an unbounded Page is exactly the bug", a.gotPage)
	}
	if a.gotPage.Limit != 26 { // servePage's limit+1 truncation probe
		t.Errorf("pushed-down page = %+v, want Limit=26 (25+1 truncation probe), same as the unfiltered lane", a.gotPage)
	}

	// Correctness is unchanged: still exactly this run's rows, filtered before
	// any window (the DB applies WHERE before LIMIT by construction).
	var got []types.ApprovalRequest
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0].RunID != mine {
		t.Errorf("got %d approval(s) %+v, want only the requested run's", len(got), got)
	}
}
