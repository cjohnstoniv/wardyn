// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// #1197: the reads the attention projection and GET /me/attention need
// beyond what GET /runs already scoped — every PENDING approval on a batch
// of runs, in one query, so projecting attention onto a page of runs costs
// one extra round trip regardless of how many of them are actually held;
// and the two scoped PENDING *counts* /me/attention's pending_approvals
// answers, counted in the database rather than by listing every row (the
// same rule CountApprovalsForRun states, store.go).
package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ApprovalsForRunsPager is the attention projection's read surface — kept out
// of Store for the reason every capability interface in this package is
// (ApprovalsByRunCreatorPager, RunsFilteredPager, ...): widening Store would
// silently route a test double's embedded-but-not-overridden method to the
// unscoped behaviour. An absent implementation fails CLOSED at the api-layer
// call site (attention projection refuses rather than silently omitting
// `attention` from every run).
type ApprovalsForRunsPager interface {
	// ListPendingApprovalsForRuns returns every state=PENDING approval whose
	// run_id is in runIDs — no other state, since only a PENDING row can be
	// held (approval.Hold's own state gate) or contribute to Pending's count.
	ListPendingApprovalsForRuns(ctx context.Context, runIDs []uuid.UUID) ([]types.ApprovalRequest, error)
	// CountPendingApprovals returns how many approvals in the WHOLE
	// deployment are state=PENDING — GET /me/attention's admin-view
	// pending_approvals, counted in the database rather than by listing
	// every row (the cost CountApprovalsForRun's own doc names).
	CountPendingApprovals(ctx context.Context) (int, error)
	// CountPendingApprovalsByRunCreator returns how many PENDING approvals
	// are raised on runs createdBy owns — GET /me/attention's user-view
	// pending_approvals, the same scope ListApprovalsPageByRunCreator lists,
	// counted rather than listed.
	CountPendingApprovalsByRunCreator(ctx context.Context, createdBy string) (int, error)
}

var _ ApprovalsForRunsPager = PG{}

// ListPendingApprovalsForRuns — see ApprovalsForRunsPager. Backed by
// approvals_run_idx (0001_init.sql) and the partial approvals_state_idx
// (state='PENDING'): this is an index scan over run_id = ANY($1) intersected
// with the partial index, not a table scan.
func (s PG) ListPendingApprovalsForRuns(ctx context.Context, runIDs []uuid.UUID) ([]types.ApprovalRequest, error) {
	if len(runIDs) == 0 {
		return []types.ApprovalRequest{}, nil
	}
	q := `
		SELECT ` + approvalCols + `
		FROM approvals
		WHERE state = 'PENDING' AND run_id = ANY($1)`
	return collect(ctx, s.Pool, "list", "pending approvals for runs", q, []any{runIDs}, scanApproval)
}

// CountPendingApprovals — see ApprovalsForRunsPager. Backed by the partial
// approvals_state_idx (state='PENDING'): an index-only count, not a table
// scan.
func (s PG) CountPendingApprovals(ctx context.Context) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM approvals WHERE state = 'PENDING'`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count pending approvals: %w", err)
	}
	return n, nil
}

// CountPendingApprovalsByRunCreator — see ApprovalsForRunsPager. Same
// semi-join ListApprovalsPageByRunCreator's own query uses (approvals has no
// created_by column of its own), counted instead of listed.
func (s PG) CountPendingApprovalsByRunCreator(ctx context.Context, createdBy string) (int, error) {
	var n int
	const q = `
		SELECT count(*) FROM approvals
		WHERE state = 'PENDING' AND run_id IN (SELECT id FROM agent_runs WHERE created_by = $1)`
	if err := s.Pool.QueryRow(ctx, q, createdBy).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count pending approvals by run creator: %w", err)
	}
	return n, nil
}
