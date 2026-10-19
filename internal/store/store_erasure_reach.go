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
// needs it. It returns the rows it deleted.
func (s PG) DeleteSettledRunPATsOf(ctx context.Context, owners []string) (int64, error) {
	if len(owners) == 0 {
		return 0, nil
	}
	tag, err := s.Pool.Exec(ctx,
		`DELETE FROM ado_run_pats WHERE owner = ANY($1) AND (revoked_at IS NOT NULL OR valid_to <= now())`, owners)
	if err != nil {
		return 0, fmt.Errorf("store: delete settled run tokens: %w", err)
	}
	return tag.RowsAffected(), nil
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
