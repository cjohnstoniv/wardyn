// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// RevokeOwedCursor is the position the revoke worker resumes from: the last row of its previous page,
// in (revoke_owed_at, id) order.
type RevokeOwedCursor struct {
	At time.Time
	ID uuid.UUID
}

// RevokeOwed is the durable "revoke owed" mark on agent_runs.revoke_owed_at, set from the moment a run's
// revoke starts until one returns nil. It is optional: the api reads it by type assertion, so the test
// doubles that embed Store need not carry it.
type RevokeOwed interface {
	// MarkRevokeOwed stamps the run, keeping the first stamp when it is already set.
	MarkRevokeOwed(ctx context.Context, id uuid.UUID) error
	// ClearRevokeOwed removes the stamp; a run with none is left alone.
	ClearRevokeOwed(ctx context.Context, id uuid.UUID) error
	// ListRevokeOwedAfter returns up to limit runs owed for at least olderThan, oldest stamp first, after
	// the cursor when one is given. The returned cursor is the last row's, and nil when the page was
	// short: the caller then starts again from the oldest.
	ListRevokeOwedAfter(ctx context.Context, olderThan time.Duration, after *RevokeOwedCursor, limit int) ([]uuid.UUID, *RevokeOwedCursor, error)
	// RevokeOwedSet reports whether the run carries the mark.
	RevokeOwedSet(ctx context.Context, id uuid.UUID) (bool, error)
}

// MarkRevokeOwed — see RevokeOwed.
func (s PG) MarkRevokeOwed(ctx context.Context, id uuid.UUID) error {
	if _, err := s.Pool.Exec(ctx,
		`UPDATE agent_runs SET revoke_owed_at = COALESCE(revoke_owed_at, now()) WHERE id = $1`, id); err != nil {
		return fmt.Errorf("store: mark revoke owed: %w", err)
	}
	return nil
}

// ClearRevokeOwed — see RevokeOwed.
func (s PG) ClearRevokeOwed(ctx context.Context, id uuid.UUID) error {
	if _, err := s.Pool.Exec(ctx,
		`UPDATE agent_runs SET revoke_owed_at = NULL WHERE id = $1 AND revoke_owed_at IS NOT NULL`, id); err != nil {
		return fmt.Errorf("store: clear revoke owed: %w", err)
	}
	return nil
}

// ListRevokeOwedAfter — see RevokeOwed. It reads agent_runs_revoke_owed_idx (migration 0277).
func (s PG) ListRevokeOwedAfter(ctx context.Context, olderThan time.Duration, after *RevokeOwedCursor, limit int) ([]uuid.UUID, *RevokeOwedCursor, error) {
	const head = `SELECT id, revoke_owed_at FROM agent_runs
		WHERE revoke_owed_at IS NOT NULL AND revoke_owed_at <= now() - make_interval(secs => $1::float8)`
	const tail = ` ORDER BY revoke_owed_at, id LIMIT `
	var rows pgx.Rows
	var err error
	if after != nil {
		rows, err = s.Pool.Query(ctx, head+` AND (revoke_owed_at, id) > ($2, $3)`+tail+`$4`,
			olderThan.Seconds(), after.At, after.ID, limit)
	} else {
		rows, err = s.Pool.Query(ctx, head+tail+`$2`, olderThan.Seconds(), limit)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("store: list revoke owed: %w", err)
	}
	defer rows.Close()
	var ids []uuid.UUID
	var last RevokeOwedCursor
	for rows.Next() {
		var id uuid.UUID
		var at time.Time
		if err := rows.Scan(&id, &at); err != nil {
			return nil, nil, fmt.Errorf("store: scan revoke owed: %w", err)
		}
		ids = append(ids, id)
		last = RevokeOwedCursor{At: at, ID: id}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("store: list revoke owed: %w", err)
	}
	if limit <= 0 || len(ids) < limit {
		return ids, nil, nil
	}
	return ids, &last, nil
}

// RevokeOwedSet — see RevokeOwed. An unknown run reads as not set.
func (s PG) RevokeOwedSet(ctx context.Context, id uuid.UUID) (bool, error) {
	var set bool
	err := s.Pool.QueryRow(ctx, `SELECT revoke_owed_at IS NOT NULL FROM agent_runs WHERE id = $1`, id).Scan(&set)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: read revoke owed: %w", err)
	}
	return set, nil
}

var _ RevokeOwed = PG{}
