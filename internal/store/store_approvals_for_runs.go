// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// #1197 L1b: the ONE read the attention projection needs beyond what
// GET /runs already scoped — every PENDING approval on a batch of runs, in
// one query, so projecting attention onto a page of runs costs one extra
// round trip regardless of how many of them are actually held.
package store

import (
	"context"

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
