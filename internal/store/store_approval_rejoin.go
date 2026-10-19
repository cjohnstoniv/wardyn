// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// StampApprovalRejoin records that a held request re-entered the hold on approval id at `at`, and
// returns the row. Only a PENDING row of runID and kind is stamped; for any other ok is false, so a
// rejoin never reaches another run's row or one already decided.
func (s PG) StampApprovalRejoin(ctx context.Context, id, runID uuid.UUID, kind types.ApprovalKind, at time.Time) (types.ApprovalRequest, bool, error) {
	const q = `
		UPDATE approvals SET last_rejoined_at = $4
		WHERE id = $1 AND run_id = $2 AND kind = $3 AND state = 'PENDING'
		RETURNING ` + approvalCols
	ap, err := scanApproval(s.Pool.QueryRow(ctx, q, id, runID, string(kind), at))
	if errors.Is(err, ErrNotFound) {
		return types.ApprovalRequest{}, false, nil
	}
	return ap, err == nil, err
}
