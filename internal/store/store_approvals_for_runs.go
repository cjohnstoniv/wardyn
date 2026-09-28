// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The reads the attention projection and GET /me/attention need beyond what
// GET /runs already scoped: every PENDING approval on a batch of runs, in one
// query, so paging runs costs one extra round trip regardless of how many are
// held; and the two scoped PENDING counts /me/attention answers, counted in
// the database rather than by listing every row.
package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ApprovalsForRunsPager is the attention projection's read surface — kept out
// of Store like every capability interface in this package, since widening
// Store would silently route a test double's unoverridden method to the
// unscoped behaviour. SECURITY: an absent implementation fails CLOSED at the
// api-layer call site (attention projection refuses rather than silently
// omitting `attention` from every run).
type ApprovalsForRunsPager interface {
	// ListPendingApprovalsForRuns returns every state=PENDING approval whose
	// run_id is in runIDs — no other state, since only PENDING can be held or
	// contribute to Pending's count.
	ListPendingApprovalsForRuns(ctx context.Context, runIDs []uuid.UUID) ([]types.ApprovalRequest, error)
	// CountPendingApprovals returns how many approvals in the whole deployment
	// are state=PENDING — GET /me/attention's admin-view pending_approvals,
	// counted in the database rather than by listing every row.
	CountPendingApprovals(ctx context.Context) (int, error)
	// CountPendingApprovalsByRunCreator returns how many PENDING approvals are
	// raised on runs createdBy owns — GET /me/attention's user-view
	// pending_approvals, counted rather than listed.
	CountPendingApprovalsByRunCreator(ctx context.Context, createdBy string) (int, error)
}

var _ ApprovalsForRunsPager = PG{}

// ListPendingApprovalsForRuns — see ApprovalsForRunsPager. Backed by
// approvals_run_idx and the partial approvals_state_idx: an index scan over
// run_id = ANY($1) intersected with the partial index, not a table scan.
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
// approvals_state_idx: an index-only count, not a table scan.
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
