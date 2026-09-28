// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// #1197: ListPendingApprovalsForRuns and the two scoped PENDING counts — the
// attention projection's and GET /me/attention's own reads. Guarded by
// WARDYN_TEST_PG, same as every other store_*_pg_test.go file.
package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_ListPendingApprovalsForRuns pins ApprovalsForRunsPager's contract:
// only PENDING rows, only for the named runs, and nothing else — a decided
// row and a foreign run's row must never leak in.
func TestPG_ListPendingApprovalsForRuns(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)

	runA := persistRun(t, ctx, pool, newRun(types.RunRunning)).ID
	runB := persistRun(t, ctx, pool, newRun(types.RunRunning)).ID
	runC := persistRun(t, ctx, pool, newRun(types.RunRunning)).ID // never queried — the "foreign run" check

	// Each call needs its OWN scope hash: the partial dedup index
	// (0002_approval_uniqueness.sql) refuses a second PENDING row on the same
	// run+kind+scope, so two PENDING rows on the same run (wantPending,
	// decided-before-it-is-decided) must not collide.
	mk := func(runID uuid.UUID, state types.ApprovalState) types.ApprovalRequest {
		ap, err := pg.CreateApproval(ctx, types.ApprovalRequest{
			ID: uuid.New(), RunID: runID, Kind: types.ApprovalEgressDomain,
			RequestedScope: json.RawMessage(`{"host":"h` + uuid.New().String() + `","mode":"wait_for_review"}`),
			State:          types.ApprovalPending, RequestedAt: time.Now().UTC(),
		})
		if err != nil {
			t.Fatalf("create approval: %v", err)
		}
		if state != types.ApprovalPending {
			if _, err := pg.DecideApproval(ctx, ap.ID, types.ApprovalDecision{State: state, DecidedBy: "tester"}); err != nil {
				t.Fatalf("decide approval: %v", err)
			}
		}
		return ap
	}

	wantPending := mk(runA, types.ApprovalPending)
	decided := mk(runA, types.ApprovalApproved)
	otherRunPending := mk(runB, types.ApprovalPending)
	foreignPending := mk(runC, types.ApprovalPending)

	got, err := pg.ListPendingApprovalsForRuns(ctx, []uuid.UUID{runA, runB})
	if err != nil {
		t.Fatalf("ListPendingApprovalsForRuns: %v", err)
	}
	byID := map[uuid.UUID]bool{}
	for _, ap := range got {
		byID[ap.ID] = true
	}
	if !byID[wantPending.ID] {
		t.Errorf("missing runA's own PENDING row")
	}
	if byID[decided.ID] {
		t.Errorf("a DECIDED row on a queried run leaked in")
	}
	if !byID[otherRunPending.ID] {
		t.Errorf("missing runB's own PENDING row (a second queried run)")
	}
	if byID[foreignPending.ID] {
		t.Errorf("a PENDING row on a run NOT in runIDs leaked in")
	}

	empty, err := pg.ListPendingApprovalsForRuns(ctx, nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty runIDs: got %v, %v; want an empty slice, no error", empty, err)
	}
}

// TestPG_CountPendingApprovals pins the deployment-wide count GET
// /me/attention's admin view answers with: every PENDING row, regardless of
// which run raised it, and nothing else.
func TestPG_CountPendingApprovals(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)

	runA := persistRun(t, ctx, pool, newRun(types.RunRunning)).ID
	runB := persistRun(t, ctx, pool, newRun(types.RunRunning)).ID

	before, err := pg.CountPendingApprovals(ctx)
	if err != nil {
		t.Fatalf("CountPendingApprovals (before): %v", err)
	}

	mkPending := func(runID uuid.UUID) types.ApprovalRequest {
		ap, err := pg.CreateApproval(ctx, types.ApprovalRequest{
			ID: uuid.New(), RunID: runID, Kind: types.ApprovalEgressDomain,
			RequestedScope: json.RawMessage(`{"host":"h` + uuid.New().String() + `","mode":"wait_for_review"}`),
			State:          types.ApprovalPending, RequestedAt: time.Now().UTC(),
		})
		if err != nil {
			t.Fatalf("create approval: %v", err)
		}
		return ap
	}
	mkDecided := func(runID uuid.UUID) {
		ap := mkPending(runID)
		if _, err := pg.DecideApproval(ctx, ap.ID, types.ApprovalDecision{State: types.ApprovalApproved, DecidedBy: "tester"}); err != nil {
			t.Fatalf("decide approval: %v", err)
		}
	}

	mkPending(runA) // +1 PENDING
	mkPending(runB) // +1 PENDING, a DIFFERENT run — the count is deployment-wide, not per-run
	mkDecided(runA) // a decided row must not count

	got, err := pg.CountPendingApprovals(ctx)
	if err != nil {
		t.Fatalf("CountPendingApprovals (after): %v", err)
	}
	if got != before+2 {
		t.Fatalf("CountPendingApprovals = %d, want %d (before=%d, +2 PENDING, +0 for the decided row)", got, before+2, before)
	}
}

// TestPG_CountPendingApprovalsByRunCreator pins the owner-scoped count GET
// /me/attention's user view answers with: only PENDING rows on runs this
// principal created — a foreign run's own PENDING row must never count.
func TestPG_CountPendingApprovalsByRunCreator(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)

	owner := "counter-owner-" + uuid.New().String()
	other := "counter-other-" + uuid.New().String()
	ownRun := persistRun(t, ctx, pool, func() types.AgentRun { r := newRun(types.RunRunning); r.CreatedBy = owner; return r }()).ID
	foreignRun := persistRun(t, ctx, pool, func() types.AgentRun { r := newRun(types.RunRunning); r.CreatedBy = other; return r }()).ID

	mk := func(runID uuid.UUID, state types.ApprovalState) {
		ap, err := pg.CreateApproval(ctx, types.ApprovalRequest{
			ID: uuid.New(), RunID: runID, Kind: types.ApprovalEgressDomain,
			RequestedScope: json.RawMessage(`{"host":"h` + uuid.New().String() + `","mode":"wait_for_review"}`),
			State:          types.ApprovalPending, RequestedAt: time.Now().UTC(),
		})
		if err != nil {
			t.Fatalf("create approval: %v", err)
		}
		if state != types.ApprovalPending {
			if _, err := pg.DecideApproval(ctx, ap.ID, types.ApprovalDecision{State: state, DecidedBy: "tester"}); err != nil {
				t.Fatalf("decide approval: %v", err)
			}
		}
	}

	mk(ownRun, types.ApprovalPending)  // counts
	mk(ownRun, types.ApprovalApproved) // decided — does not count
	mk(foreignRun, types.ApprovalPending) // a foreign run's own PENDING row — must never count

	got, err := pg.CountPendingApprovalsByRunCreator(ctx, owner)
	if err != nil {
		t.Fatalf("CountPendingApprovalsByRunCreator: %v", err)
	}
	if got != 1 {
		t.Fatalf("CountPendingApprovalsByRunCreator(%q) = %d, want 1 (only the owner's own PENDING row)", owner, got)
	}

	gotOther, err := pg.CountPendingApprovalsByRunCreator(ctx, other)
	if err != nil {
		t.Fatalf("CountPendingApprovalsByRunCreator (other): %v", err)
	}
	if gotOther != 1 {
		t.Fatalf("CountPendingApprovalsByRunCreator(%q) = %d, want 1 (the foreign run's own PENDING row)", other, gotOther)
	}
}
