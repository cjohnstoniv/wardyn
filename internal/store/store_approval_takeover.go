// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TakeOverApproval gives holder control on a human_takeover approval: a PENDING row moves to APPROVED
// with holder as decider and holder, and a row someone else holds (APPROVED, not handed back) gets
// holder as its new holder. It returns the row and the holder it replaced ("" when it was PENDING).
// A row in any other state, or already held by holder, is ErrAlreadyDecided; an absent one ErrNotFound.
// One UPDATE, so two people taking over at once leave exactly one holder.
func (s PG) TakeOverApproval(ctx context.Context, id uuid.UUID, holder string) (types.ApprovalRequest, string, error) {
	// The previous holder is read in the same statement from the locked row (a CTE read in RETURNING
	// would run after the update and see nothing).
	const q = `
		UPDATE approvals SET
			state = 'APPROVED',
			decided_at = CASE WHEN state = 'PENDING' THEN $3 ELSE decided_at END,
			decided_by = CASE WHEN state = 'PENDING' THEN $2 ELSE decided_by END,
			takeover_holder = $2
		FROM (SELECT id AS pid, takeover_holder AS prev_holder FROM approvals WHERE id = $1 FOR UPDATE) p
		WHERE approvals.id = p.pid AND kind = 'human_takeover' AND takeover_holder <> $2 AND (state = 'PENDING'
			OR (state = 'APPROVED' AND handed_back_at IS NULL))
		RETURNING ` + approvalCols + `, p.prev_holder`
	// Stamped like DecideApproval's decided_at, so the boot-heal guard reads the same clock.
	decidedAt := s.now()
	anchor, err := db.CaptureAppClock(ctx, s.Pool, s.now)
	if err != nil {
		return types.ApprovalRequest{}, "", err
	}
	var prev string
	ap, err := scanApproval(rowWithTail{s.Pool.QueryRow(ctx, q, id, holder, anchor.Translate(decidedAt)), &prev})
	if errors.Is(err, ErrNotFound) {
		return types.ApprovalRequest{}, "", s.decidedOrMissing(ctx, id)
	}
	return ap, prev, err
}

// ErrTakeoverNotHolder is a hand back by someone who does not have control of an open takeover.
var ErrTakeoverNotHolder = errors.New("store: takeover is held by someone else")

// HandBackApproval records that holder, the person with control of a human_takeover approval, handed
// it back at `at`. Only an APPROVED row not yet handed back, held by holder, moves: one held by
// someone else is ErrTakeoverNotHolder, any other ErrAlreadyDecided, an absent one ErrNotFound. The
// holder is in the UPDATE, so a take over by someone else that lands first leaves the row open.
func (s PG) HandBackApproval(ctx context.Context, id uuid.UUID, holder string, at time.Time) (types.ApprovalRequest, error) {
	const q = `
		UPDATE approvals SET handed_back_at = $2
		WHERE id = $1 AND kind = 'human_takeover' AND state = 'APPROVED' AND handed_back_at IS NULL
			AND takeover_holder = $3
		RETURNING ` + approvalCols
	ap, err := scanApproval(s.Pool.QueryRow(ctx, q, id, at, holder))
	if !errors.Is(err, ErrNotFound) {
		return ap, err
	}
	var open bool
	err = s.Pool.QueryRow(ctx, `SELECT kind = 'human_takeover' AND state = 'APPROVED' AND handed_back_at IS NULL
		FROM approvals WHERE id = $1`, id).Scan(&open)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return types.ApprovalRequest{}, ErrNotFound
	case err != nil:
		return types.ApprovalRequest{}, err
	case open:
		return types.ApprovalRequest{}, ErrTakeoverNotHolder
	}
	return types.ApprovalRequest{}, ErrAlreadyDecided
}

// decidedOrMissing tells a row that exists in the wrong state (ErrAlreadyDecided) from an absent one.
func (s PG) decidedOrMissing(ctx context.Context, id uuid.UUID) error {
	var exists bool
	if err := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM approvals WHERE id=$1)`, id).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return ErrAlreadyDecided
	}
	return ErrNotFound
}

// rowWithTail scans approvalCols into scanApproval's targets plus extra trailing columns.
type rowWithTail struct {
	row  interface{ Scan(...any) error }
	tail any
}

func (r rowWithTail) Scan(dest ...any) error { return r.row.Scan(append(dest, r.tail)...) }
