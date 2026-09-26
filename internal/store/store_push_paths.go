// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// A held push's complete path list (migration 0085_push_content_paths).
package store

import (
	"context"
	"errors"
	"hash/crc32"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ErrPushPathListCap is RecordPushPathList's refusal: the run already has
// its limit of lists.
var ErrPushPathListCap = errors.New("store: this run has stored its limit of push path lists")

// PushPathListStore keeps each push_content approval's types.PushPathList.
// Like PrincipalPrefStore it is not part of Store, so the api test doubles
// need not grow it; the api type-asserts it.
type PushPathListStore interface {
	// RecordPushPathList stores approvalID's list once, for runID. A second
	// call for the same approval stores nothing and reports false; a list
	// past the run's limit stores nothing and returns ErrPushPathListCap. The
	// count and the insert are one step: concurrent calls for a run serialize.
	RecordPushPathList(ctx context.Context, approvalID, runID uuid.UUID, l types.PushPathList, limit int) (bool, error)
	// GetPushPathList returns approvalID's list, or ErrNotFound.
	GetPushPathList(ctx context.Context, approvalID uuid.UUID) (types.PushPathList, error)
	// CountPushPathLists counts the lists stored for runID's approvals.
	CountPushPathLists(ctx context.Context, runID uuid.UUID) (int, error)
}

var _ PushPathListStore = PG{}

func (s PG) RecordPushPathList(ctx context.Context, approvalID, runID uuid.UUID, l types.PushPathList, limit int) (bool, error) {
	// READ COMMITTED, pinned: the count after the lock must see the rows the
	// previous holder committed, which a REPEATABLE READ snapshot taken by the
	// lock statement itself would not.
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // a no-op after Commit
	// A crc32 collision only serializes two runs' raises.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, $2)`,
		db.PushPathListLockClass, int32(crc32.ChecksumIEEE(runID[:]))); err != nil {
		return false, err
	}
	var exists bool
	var n int
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM push_content_paths WHERE approval_id = $1),
		       (SELECT count(*) FROM push_content_paths p JOIN approvals a ON a.id = p.approval_id WHERE a.run_id = $2)`,
		approvalID, runID).Scan(&exists, &n); err != nil {
		return false, err
	}
	switch {
	case exists:
		return false, nil
	case n >= limit:
		return false, ErrPushPathListCap
	}
	// Appended onto an empty slice: pgx sends a nil one as NULL, and paths is
	// NOT NULL.
	if _, err := tx.Exec(ctx, `INSERT INTO push_content_paths (approval_id, paths, truncated) VALUES ($1, $2, $3)`,
		approvalID, append([]string{}, l.Paths...), l.Truncated); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
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
