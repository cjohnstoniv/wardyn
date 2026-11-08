// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ApprovalsOnUnitsPager is the read behind GET /approvals?decidable_by=me for a caller who decides holds
// they did not raise: the PENDING approvals on runs launched in a set of units. A capability interface like
// ApprovalsForRunsPager. An absent implementation fails CLOSED at the api call site (the list refuses
// rather than leaving a decider's holds out).
type ApprovalsOnUnitsPager interface {
	// ListPendingApprovalsOnUnits returns every PENDING approval whose run was launched in one of units, newest
	// request first. The set is small: a pending request stops waiting when its budget or its run ends.
	ListPendingApprovalsOnUnits(ctx context.Context, units []uuid.UUID) ([]types.ApprovalRequest, error)
}

var _ ApprovalsOnUnitsPager = PG{}

// ListPendingApprovalsOnUnits — see ApprovalsOnUnitsPager. The partial pending index bounds the read to the
// pending set; the unit test is a semi-join over agent_runs.
func (s PG) ListPendingApprovalsOnUnits(ctx context.Context, units []uuid.UUID) ([]types.ApprovalRequest, error) {
	if len(units) == 0 {
		return []types.ApprovalRequest{}, nil
	}
	q := `
		SELECT ` + approvalCols + `
		FROM approvals
		WHERE state = 'PENDING' AND run_id IN (SELECT id FROM agent_runs WHERE unit_id = ANY($1))
		ORDER BY requested_at DESC`
	return collect(ctx, s.Pool, "list", "pending approvals on units", q, []any{units}, scanApproval)
}
