// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// #1197 L1b: ListPendingApprovalsForRuns — the attention projection's one
// extra read. Guarded by WARDYN_TEST_PG, same as every other
// store_*_pg_test.go file.
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
