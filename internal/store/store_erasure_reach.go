// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
)

// EraseApprovalDecisionPersonalFields clears the decider and the typed reason of every approval
// decided by one of principals, the approvals-table twin of EraseGovernanceChangePersonalFields: the
// audit copy of the reason is sealed, this one is not. The state, its time and the decided scope stay.
// It returns the rows it touched.
func (s PG) EraseApprovalDecisionPersonalFields(ctx context.Context, principals []string) (int, error) {
	if len(principals) == 0 {
		return 0, nil
	}
	tag, err := s.Pool.Exec(ctx, `UPDATE approvals SET decided_by = '', reason = '' WHERE decided_by = ANY($1)`, principals)
	if err != nil {
		return 0, fmt.Errorf("store: erase approval decision personal fields: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// DeleteSettledRunPATsOf deletes the Azure DevOps token history of owners: every ado_run_pats row
// already revoked or past its valid_to. A row still live at Azure DevOps stays, since revoking it
// needs it. It returns the rows it deleted and the live rows it left.
func (s PG) DeleteSettledRunPATsOf(ctx context.Context, owners []string) (deleted, liveLeft int64, err error) {
	if len(owners) == 0 {
		return 0, 0, nil
	}
	// The count reads the snapshot from before the delete, so it is the rows the delete leaves.
	err = s.Pool.QueryRow(ctx, `
		WITH gone AS (DELETE FROM ado_run_pats WHERE owner = ANY($1) AND (revoked_at IS NOT NULL OR valid_to <= now()) RETURNING 1)
		SELECT (SELECT count(*) FROM gone),
		       (SELECT count(*) FROM ado_run_pats WHERE owner = ANY($1) AND revoked_at IS NULL AND (valid_to IS NULL OR valid_to > now()))`,
		owners).Scan(&deleted, &liveLeft)
	if err != nil {
		return 0, 0, fmt.Errorf("store: delete settled run tokens: %w", err)
	}
	return deleted, liveLeft, nil
}

// DeleteDelegatedTokensOf deletes every delegated token minted for one of principals, live or
// expired. It returns the rows it deleted.
func (s PG) DeleteDelegatedTokensOf(ctx context.Context, principals []string) (int64, error) {
	if len(principals) == 0 {
		return 0, nil
	}
	tag, err := s.Pool.Exec(ctx, `DELETE FROM delegated_tokens WHERE principal = ANY($1)`, principals)
	if err != nil {
		return 0, fmt.Errorf("store: delete delegated tokens: %w", err)
	}
	return tag.RowsAffected(), nil
}

// DeleteRunCreateKeysOf deletes every POST /runs Idempotency-Key of one of principals, direct or through
// a delegate, bound or not: a kept answer names the person's run. It returns the rows it deleted.
func (s PG) DeleteRunCreateKeysOf(ctx context.Context, principals []string) (int64, error) {
	if len(principals) == 0 {
		return 0, nil
	}
	tag, err := s.Pool.Exec(ctx, `DELETE FROM run_create_keys WHERE principal = ANY($1)`, principals)
	if err != nil {
		return 0, fmt.Errorf("store: delete idempotency keys: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ClearRunCreateResponsesOf clears the answers kept for the POST /runs Idempotency-Keys that name a run
// createdBy created, and marks them so no later save refills them; each key stays bound to its run, so a
// retry is rebuilt from the erased run and never makes a second one. It returns the keys it marked.
func (s PG) ClearRunCreateResponsesOf(ctx context.Context, createdBy string) (int64, error) {
	tag, err := s.Pool.Exec(ctx, `UPDATE run_create_keys k
		SET response_status = NULL, response_location = NULL, response_body = NULL, response_erased = true
		FROM agent_runs r WHERE r.id = k.run_id AND r.created_by = $1 AND NOT k.response_erased`, createdBy)
	if err != nil {
		return 0, fmt.Errorf("store: clear a person's idempotency key answers: %w", err)
	}
	return tag.RowsAffected(), nil
}
