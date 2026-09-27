// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// SpendApprovalOnce spends a `once` Azure DevOps capability approval: it writes
// jti into the row's minted_jti if, and only if, nothing has spent it yet.
// spent=false means another request already did (or the row is not an approved,
// once-scoped, control-plane-raised escalation) and the caller must not forward.
//
// The empty-minted_jti condition is the whole exactly-once argument: two
// concurrent re-resolves both reach this UPDATE, Postgres serialises them on the
// row, and the second finds the column already written. It is the credential mint's own
// write-back column, so "approval X let request Z through" is the same join
// "approval X minted credential Z" already is.
//
// Not part of the Store interface, for ResolveReauthApproval's reason: the api
// package type-asserts for it, and a store without it spends nothing.
func (s PG) SpendApprovalOnce(ctx context.Context, id uuid.UUID, jti string) (bool, error) {
	if jti == "" {
		return false, fmt.Errorf("store: SpendApprovalOnce: an empty jti would leave the approval unspent")
	}
	tag, err := s.Pool.Exec(ctx, `
		UPDATE approvals SET minted_jti = $2
		WHERE id = $1 AND minted_jti = '' AND state = 'APPROVED' AND kind = 'tool_call'
		  AND decision_scope = 'once' AND grant_id IS NOT NULL`, id, jti)
	if err != nil {
		return false, fmt.Errorf("store: spend once approval: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
