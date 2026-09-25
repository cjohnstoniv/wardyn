// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// A held push's complete path list (migration 0085_push_content_paths).
package store

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// PushPathListStore keeps each push_content approval's types.PushPathList.
// Like PrincipalPrefStore it is not part of Store, so the api test doubles
// need not grow it; the api type-asserts it.
type PushPathListStore interface {
	// RecordPushPathList stores approvalID's list once. A second call for the
	// same approval stores nothing and reports false.
	RecordPushPathList(ctx context.Context, approvalID uuid.UUID, l types.PushPathList) (bool, error)
	// GetPushPathList returns approvalID's list, or ErrNotFound.
	GetPushPathList(ctx context.Context, approvalID uuid.UUID) (types.PushPathList, error)
	// CountPushPathLists counts the lists stored for runID's approvals.
	CountPushPathLists(ctx context.Context, runID uuid.UUID) (int, error)
}

var _ PushPathListStore = PG{}

func (s PG) RecordPushPathList(ctx context.Context, approvalID uuid.UUID, l types.PushPathList) (bool, error) {
	// Appended onto an empty slice: pgx sends a nil one as NULL, and paths is
	// NOT NULL even when a truncated list kept none.
	tag, err := s.Pool.Exec(ctx, `
		INSERT INTO push_content_paths (approval_id, paths, truncated) VALUES ($1, $2, $3)
		ON CONFLICT (approval_id) DO NOTHING`,
		approvalID, append([]string{}, l.Paths...), l.Truncated)
	return tag.RowsAffected() == 1, err
}

func (s PG) GetPushPathList(ctx context.Context, approvalID uuid.UUID) (types.PushPathList, error) {
	var l types.PushPathList
	err := s.Pool.QueryRow(ctx, `SELECT paths, truncated FROM push_content_paths WHERE approval_id = $1`,
		approvalID).Scan(&l.Paths, &l.Truncated)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.PushPathList{}, ErrNotFound
	}
	return l, err
}

func (s PG) CountPushPathLists(ctx context.Context, runID uuid.UUID) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `
		SELECT count(*) FROM push_content_paths p JOIN approvals a ON a.id = p.approval_id
		WHERE a.run_id = $1`, runID).Scan(&n)
	return n, err
}
