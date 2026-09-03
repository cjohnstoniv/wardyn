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

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
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

// TestListApprovals_MemberSeesOnlyTheirOwnRuns is the ownership-scoping pin, and
// it exists because NOTHING asserted that behaviour in any lane.
//
// The two tests above this one both drive the list with adminToken, i.e. the
// security-tier branch that deliberately skips scoping. authz_test.go classifies
// GET /api/v1/approvals as classMember, but the classMember arm only calls
// assertNotBlocked — a status code, never the body — so it cannot see WHICH rows
// come back. rbac_test.go used to claim in prose that "its real (scoped,
// non-500) behavior is covered by the chi.Walk-enumerated matrix in
// authz_test.go instead"; that claim was false and is corrected there.
// docs/TEST-GAPS.md independently lists ListApprovalsPageByRunCreator as
// untested in BOTH the union and the Postgres lane.
//
// COUNTERFACTUAL, executed: deleting the whole member branch from
// handleListApprovals — so a member is served the fleet-wide queue — left
// `go test ./internal/api/` fully green before this test existed.
func TestListApprovals_MemberSeesOnlyTheirOwnRuns(t *testing.T) {
	const memberSub = "sub-list-member"
	srv, ast, aap, _ := newAuthzMatrixServer(t)

	seedRunOwnedBy := func(createdBy string) uuid.UUID {
		id := uuid.New()
		ast.mu.Lock()
		ast.runs[id] = types.AgentRun{ID: id, CreatedBy: createdBy, State: types.RunRunning, Agent: "claude-code"}
		ast.mu.Unlock()
		return id
	}
	mineRun, foreignRun := seedRunOwnedBy(memberSub), seedRunOwnedBy("sub-someone-else")
	mineAP := aap.seed(mineRun)
	foreignAP := aap.seed(foreignRun)

	member := ssoSession(t, memberSub, "list-member@corp.example", oidc.RoleMember)
	w := doSSO(t, srv, http.MethodGet, "/api/v1/approvals", member, "")
	if w.Code != http.StatusOK {
		t.Fatalf("member list = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got []types.ApprovalRequest
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	// BOTH directions on the same body: the owned row is present AND the foreign
	// one is absent. Asserting only the count would pass on a backend that
	// returned one arbitrary row.
	ids := map[uuid.UUID]bool{}
	for _, ap := range got {
		ids[ap.ID] = true
	}
	if !ids[mineAP] {
		t.Errorf("member's own approval %s is MISSING from their queue — the scoping must narrow, not blind", mineAP)
	}
	if ids[foreignAP] {
		t.Errorf("member was served approval %s on a run created by someone else (%d row(s) total) — "+
			"the approvals queue is fleet-wide for members", foreignAP, len(got))
	}

	// An ADMIN sees both, so the fixture really does hold two rows and the
	// assertion above is about scoping rather than about an empty store.
	admin := ssoSession(t, "sub-list-admin", "admin@corp.example", oidc.RoleAdmin)
	wa := doSSO(t, srv, http.MethodGet, "/api/v1/approvals", admin, "")
	if wa.Code != http.StatusOK {
		t.Fatalf("admin list = %d, want 200; body=%s", wa.Code, wa.Body.String())
	}
	var all []types.ApprovalRequest
	if err := json.Unmarshal(wa.Body.Bytes(), &all); err != nil {
		t.Fatalf("decode admin: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("admin sees %d approval(s), want both — the fixture stopped exercising the split", len(all))
	}
}

// unscopedApprovals wraps the fixture so it satisfies ApprovalService but NOT
// store.ApprovalsByRunCreatorPager: the embedded interface exposes only the
// service methods, so the scoped read is genuinely absent rather than present
// and stubbed. That is the shape handleListApprovals must fail CLOSED on —
// without that read it cannot narrow the queue to a member's own runs, and
// serving the fleet-wide list instead is exactly the disclosure the scoping
// exists to prevent.
type unscopedApprovals struct{ ApprovalService }

// TestListApprovals_MemberFailsClosedWithoutTheScopedRead pins the OTHER half of
// the member branch: a backend that cannot answer "which approvals are on runs
// this principal created" must 500, never fall through to the fleet-wide list.
// The fall-through is the failure mode that would look like a working feature.
func TestListApprovals_MemberFailsClosedWithoutTheScopedRead(t *testing.T) {
	srv, ast, aap, _ := newAuthzMatrixServer(t)
	runID := uuid.New()
	ast.mu.Lock()
	ast.runs[runID] = types.AgentRun{ID: runID, CreatedBy: "sub-someone-else", State: types.RunRunning}
	ast.mu.Unlock()
	foreign := aap.seed(runID)

	// Swap in a backend WITHOUT the scoped pager. srv.cfg is unexported but this
	// test is in-package, which is the only reason this substitution is possible
	// — and it is the whole point: production always wires PG, so the !capable
	// arm is unreachable outside a test and would otherwise never be exercised.
	srv.cfg.Approvals = unscopedApprovals{aap}

	member := ssoSession(t, "sub-list-member", "list-member@corp.example", oidc.RoleMember)
	w := doSSO(t, srv, http.MethodGet, "/api/v1/approvals", member, "")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("member list on an unscoped backend = %d, want 500 — falling through to the fleet-wide "+
			"list is the disclosure the scoping exists to prevent; body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), foreign.String()) {
		t.Errorf("the 500 body carries a foreign approval id: %s", w.Body.String())
	}
}
