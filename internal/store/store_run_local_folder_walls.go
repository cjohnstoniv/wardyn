// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RunLocalFolderWallsStore keeps a run's folder walls on its row (agent_runs.local_folder_walls), where audit
// retention cannot drop them. Optional: a store without it keeps no walls, and every run then has none.
type RunLocalFolderWallsStore interface {
	// KeepRunLocalFolderWalls writes rec once; a run that already holds a record keeps it. ErrNotFound when no
	// run has id.
	KeepRunLocalFolderWalls(ctx context.Context, id uuid.UUID, rec types.LocalFolderWallsRecord) error
	// RunLocalFolderWalls is the run's record, the zero record when it holds none. ErrNotFound when no run has id.
	RunLocalFolderWalls(ctx context.Context, id uuid.UUID) (types.LocalFolderWallsRecord, error)
}

var _ RunLocalFolderWallsStore = PG{}

// KeepRunLocalFolderWalls — see RunLocalFolderWallsStore. The first write wins: COALESCE keeps a stored record.
func (s PG) KeepRunLocalFolderWalls(ctx context.Context, id uuid.UUID, rec types.LocalFolderWallsRecord) error {
	raw, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("store: keep run folder walls: %w", err)
	}
	tag, err := s.Pool.Exec(ctx, `UPDATE agent_runs SET local_folder_walls = COALESCE(local_folder_walls, $2::jsonb) WHERE id = $1`, id, raw)
	if err != nil {
		return fmt.Errorf("store: keep run folder walls: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RunLocalFolderWalls — see RunLocalFolderWallsStore. NULL is the zero record: no walls.
func (s PG) RunLocalFolderWalls(ctx context.Context, id uuid.UUID) (types.LocalFolderWallsRecord, error) {
	var raw []byte
	err := s.Pool.QueryRow(ctx, `SELECT local_folder_walls FROM agent_runs WHERE id = $1`, id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.LocalFolderWallsRecord{}, ErrNotFound
	}
	if err != nil {
		return types.LocalFolderWallsRecord{}, fmt.Errorf("store: read run folder walls: %w", err)
	}
	var rec types.LocalFolderWallsRecord
	if raw == nil {
		return rec, nil
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		return types.LocalFolderWallsRecord{}, fmt.Errorf("store: decode run folder walls: %w", err)
	}
	return rec, nil
}
