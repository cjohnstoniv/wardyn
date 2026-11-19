// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"strconv"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// SpendFileDownloadApproval spends a reviewed download's approval: it writes jti into the row's minted_jti if,
// and only if, the row is an APPROVED, once-decided, unspent file_download of runID whose scope names want's
// file, size and hash. spent=false means it is not (another run's, another kind's, another file's, refused, or
// spent already) and the caller must send nothing. Its own predicate, never SpendApprovalOnce's tool_call one:
// an approval for one kind is never spent as another. The empty-minted_jti condition is the exactly-once
// argument, as there.
//
// Not part of the Store interface (see SpendApprovalOnce): the api package type-asserts for it.
func (s PG) SpendFileDownloadApproval(ctx context.Context, id, runID uuid.UUID, want types.FileDownloadScope, jti string) (bool, error) {
	if jti == "" {
		return false, fmt.Errorf("store: SpendFileDownloadApproval: an empty jti would leave the approval unspent")
	}
	tag, err := s.Pool.Exec(ctx, `
		UPDATE approvals SET minted_jti = $3
		WHERE id = $1 AND run_id = $2 AND minted_jti = '' AND state = 'APPROVED' AND kind = 'file_download'
		  AND decision_scope = 'once' AND grant_id IS NULL
		  AND requested_scope->>'name' = $4 AND requested_scope->>'sha256' = $5 AND requested_scope->>'bytes' = $6`,
		id, runID, jti, want.Name, want.SHA256, strconv.FormatInt(want.Bytes, 10))
	if err != nil {
		return false, fmt.Errorf("store: spend file download approval: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
