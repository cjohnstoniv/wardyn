// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Integration tests for the paginated read surface (store.*Page + the LIMIT/
// OFFSET clause). Guarded by WARDYN_TEST_PG. Run with:
//
//	WARDYN_TEST_PG=postgres://... go test ./internal/store/...
//
// The run-scoped audit-events path is the deterministic, isolated way to prove
// LIMIT/OFFSET/order end-to-end: a unique run_id means these assertions never
// depend on any other rows in the shared DB. ListRunsPage is covered for its
// bound/window contract with a marker set the test fully owns.
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_QueryAuditEventsPage_LimitOffset proves the per-run audit page bounds at
// LIMIT, walks forward with OFFSET, stays in seq (ASC) order, and returns the
// whole trail when unbounded — the contract the /audit endpoint's ?limit=&offset=
// and X-Wardyn-Truncated disclosure depend on.
func TestPG_QueryAuditEventsPage_LimitOffset(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)

	runID := uuid.New()
	const n = 7
	for i := 0; i < n; i++ {
		ev := types.AuditEvent{
			ID:        uuid.New(),
			Time:      time.Now().UTC(),
			RunID:     &runID,
			ActorType: types.ActorSystem,
			Actor:     "pagination-test",
			Action:    "test.event",
			Outcome:   "success",
		}
		if err := store.InsertAuditEvent(ctx, pool, ev); err != nil {
			t.Fatalf("insert audit event %d: %v", i, err)
		}
	}
	// audit_events is append-only (no DELETE), so no cleanup — the unique runID
	// keeps every assertion below scoped to just these n rows.

	// Unbounded returns the whole trail in insertion (seq ASC) order.
	all, err := pg.QueryAuditEventsPage(ctx, runID, store.Page{})
	if err != nil {
		t.Fatalf("unbounded: %v", err)
	}
	if len(all) != n {
		t.Fatalf("unbounded len = %d, want %d", len(all), n)
	}
	for i := 1; i < len(all); i++ {
		if !all[i].Time.Before(all[i-1].Time) && all[i].ID == all[i-1].ID {
			t.Fatalf("duplicate/unordered event at %d", i)
		}
	}

	// LIMIT bounds the page.
	page1, err := pg.QueryAuditEventsPage(ctx, runID, store.Page{Limit: 3})
	if err != nil {
		t.Fatalf("limit page: %v", err)
	}
	if len(page1) != 3 {
		t.Fatalf("limit=3 len = %d, want 3", len(page1))
	}
	// First page equals the first 3 of the full ASC trail.
	for i := 0; i < 3; i++ {
		if page1[i].ID != all[i].ID {
			t.Fatalf("page1[%d].ID = %s, want %s", i, page1[i].ID, all[i].ID)
		}
	}

	// OFFSET walks forward, disjoint from page1 and still ASC.
	page2, err := pg.QueryAuditEventsPage(ctx, runID, store.Page{Limit: 3, Offset: 3})
	if err != nil {
		t.Fatalf("offset page: %v", err)
	}
	if len(page2) != 3 {
		t.Fatalf("limit=3 offset=3 len = %d, want 3", len(page2))
	}
	for i := 0; i < 3; i++ {
		if page2[i].ID != all[i+3].ID {
			t.Fatalf("page2[%d].ID = %s, want %s", i, page2[i].ID, all[i+3].ID)
		}
	}

	// The final short page (offset past all but the last event) proves OFFSET
	// reaches the newest event under ASC order — the /audit paging that lets a
	// client walk to run.complete instead of losing it to a cap.
	last, err := pg.QueryAuditEventsPage(ctx, runID, store.Page{Limit: 3, Offset: 6})
	if err != nil {
		t.Fatalf("last page: %v", err)
	}
	if len(last) != 1 || last[0].ID != all[n-1].ID {
		t.Fatalf("last page = %v, want the single newest event %s", last, all[n-1].ID)
	}
}

// TestPG_ListRunsPage_BoundAndWindow proves ListRunsPage caps at LIMIT and that
// OFFSET shifts the window, using a marker set the test fully owns so the
// assertions hold regardless of other rows in the shared DB.
func TestPG_ListRunsPage_BoundAndWindow(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)

	marker := "pager-" + uuid.NewString()
	const n = 5
	base := time.Now().UTC()
	for i := 0; i < n; i++ {
		r := newRun(types.RunRunning)
		r.CreatedBy = marker
		// Strictly decreasing created_at so ORDER BY created_at DESC gives a
		// deterministic order among the marker rows (newest first = i==0).
		r.CreatedAt = base.Add(time.Duration(-i) * time.Second)
		r.UpdatedAt = r.CreatedAt
		persistRun(t, ctx, pool, r)
	}

	// A LIMIT smaller than the total table never returns more than LIMIT rows.
	bounded, err := pg.ListRunsPage(ctx, store.Page{Limit: 2})
	if err != nil {
		t.Fatalf("bounded: %v", err)
	}
	if len(bounded) != 2 {
		t.Fatalf("limit=2 len = %d, want 2 (>=5 rows exist)", len(bounded))
	}

	// Collect just the marker rows across the whole table; they must appear in
	// created_at DESC order (newest CreatedBy==marker first).
	all, err := pg.ListRunsPage(ctx, store.Page{})
	if err != nil {
		t.Fatalf("unbounded: %v", err)
	}
	var mine []types.AgentRun
	for _, r := range all {
		if r.CreatedBy == marker {
			mine = append(mine, r)
		}
	}
	if len(mine) != n {
		t.Fatalf("marker rows = %d, want %d", len(mine), n)
	}
	for i := 1; i < len(mine); i++ {
		if mine[i].CreatedAt.After(mine[i-1].CreatedAt) {
			t.Fatalf("marker rows not in created_at DESC order at %d", i)
		}
	}
}
