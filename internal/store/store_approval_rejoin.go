// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// StampApprovalRejoin records that a held request re-entered the hold on approval id, and returns
// the row. The stamp is the database's clock, never a replica's, and never earlier than
// requested_at (which the raising replica wrote), so a skewed replica cannot order a rejoin before
// the row it rejoins. Only a PENDING row of runID and kind is stamped; for any other ok is false,
// so a rejoin never reaches another run's row or one already decided.
func (s PG) StampApprovalRejoin(ctx context.Context, id, runID uuid.UUID, kind types.ApprovalKind) (types.ApprovalRequest, bool, error) {
	const q = `
		UPDATE approvals SET last_rejoined_at = GREATEST(now(), requested_at)
		WHERE id = $1 AND run_id = $2 AND kind = $3 AND state = 'PENDING'
		RETURNING ` + approvalCols
	ap, err := scanApproval(s.Pool.QueryRow(ctx, q, id, runID, string(kind)))
	if errors.Is(err, ErrNotFound) {
		return types.ApprovalRequest{}, false, nil
	}
	return ap, err == nil, err
}
