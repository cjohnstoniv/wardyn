// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Handler tests for the ?limit=&offset= pagination contract and the
// X-Wardyn-Truncated disclosure header, driven through a store.Pager fake so the
// production DB-level path (probe limit+1, trim, disclose) is exercised without a
// live database.
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

// pagerFake embeds store.Store (nil — unused methods panic) and implements
// store.Pager over canned slices, honouring Limit/Offset exactly as the SQL
// LIMIT/OFFSET does, so the handlers take their production pager path.
type pagerFake struct {
	store.Store
	runs        []types.AgentRun
	auditByRun  map[uuid.UUID][]types.AuditEvent
	recentAudit []types.AuditEvent
}

func pagerSlice[T any](items []T, p store.Page) []T {
	off := p.Offset
	if off > len(items) {
		off = len(items)
	}
	rest := items[off:]
	if p.Limit > 0 && p.Limit < len(rest) {
		rest = rest[:p.Limit]
	}
	return rest
}

func (f *pagerFake) ListRunsPage(_ context.Context, p store.Page) ([]types.AgentRun, error) {
	return pagerSlice(f.runs, p), nil
}
func (f *pagerFake) ListPoliciesPage(context.Context, store.Page) ([]types.RunPolicy, error) {
	return nil, nil
}
func (f *pagerFake) ListWorkspacesPage(context.Context, store.Page) ([]types.Workspace, error) {
	return nil, nil
}
func (f *pagerFake) ListApprovalsPage(context.Context, types.ApprovalState, store.Page) ([]types.ApprovalRequest, error) {
	return nil, nil
}
func (f *pagerFake) QueryAuditEventsPage(_ context.Context, runID uuid.UUID, p store.Page) ([]types.AuditEvent, error) {
	return pagerSlice(f.auditByRun[runID], p), nil
}
func (f *pagerFake) QueryRecentAuditEventsPage(_ context.Context, p store.Page) ([]types.AuditEvent, error) {
	return pagerSlice(f.recentAudit, p), nil
}

func makeRuns(n int) []types.AgentRun {
	out := make([]types.AgentRun, n)
	for i := range out {
		out[i] = types.AgentRun{ID: uuid.New(), State: types.RunRunning}
	}
	return out
}

func makeEvents(n int, runID *uuid.UUID) []types.AuditEvent {
	out := make([]types.AuditEvent, n)
	for i := range out {
		out[i] = types.AuditEvent{ID: uuid.New(), RunID: runID, ActorType: types.ActorSystem, Action: "test.event", Outcome: "success"}
	}
	return out
}

// TestPaginationDisclosure covers both the runs list and the audit trail: a page
// smaller than the data truncates and flags X-Wardyn-Truncated; a page large
// enough does not; offset walks forward; a malformed limit is a 400.
func TestPaginationDisclosure(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	fake := &pagerFake{
		runs:        makeRuns(5),
		auditByRun:  map[uuid.UUID][]types.AuditEvent{runID: makeEvents(5, &runID)},
		recentAudit: makeEvents(5, nil),
	}
	srv := New(baseTestConfig(h, fake))

	countJSON := func(t *testing.T, body []byte) int {
		t.Helper()
		var arr []json.RawMessage
		if err := json.Unmarshal(body, &arr); err != nil {
			t.Fatalf("decode array: %v (body=%s)", err, body)
		}
		return len(arr)
	}

	cases := []struct {
		name      string
		path      string
		wantCode  int
		wantLen   int
		wantTrunc bool
	}{
		{"runs truncated", "/api/v1/runs?limit=2", http.StatusOK, 2, true},
		{"runs full page", "/api/v1/runs?limit=10", http.StatusOK, 5, false},
		{"runs offset tail", "/api/v1/runs?limit=2&offset=4", http.StatusOK, 1, false},
		{"runs bad limit", "/api/v1/runs?limit=nope", http.StatusBadRequest, 0, false},
		{"runs negative offset", "/api/v1/runs?offset=-1", http.StatusBadRequest, 0, false},
		{"audit global truncated", "/api/v1/audit?limit=3", http.StatusOK, 3, true},
		{"audit per-run truncated", "/api/v1/audit?run_id=" + runID.String() + "&limit=2", http.StatusOK, 2, true},
		{"audit per-run full", "/api/v1/audit?run_id=" + runID.String() + "&limit=100", http.StatusOK, 5, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := do(t, srv, http.MethodGet, c.path, adminToken, "")
			if w.Code != c.wantCode {
				t.Fatalf("code = %d, want %d (body=%s)", w.Code, c.wantCode, w.Body.String())
			}
			if c.wantCode != http.StatusOK {
				return
			}
			trunc := w.Header().Get("X-Wardyn-Truncated") == "true"
			if trunc != c.wantTrunc {
				t.Errorf("X-Wardyn-Truncated = %v, want %v", trunc, c.wantTrunc)
			}
			if n := countJSON(t, w.Body.Bytes()); n != c.wantLen {
				t.Errorf("len = %d, want %d", n, c.wantLen)
			}
		})
	}
}
