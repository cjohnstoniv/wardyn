// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RunLocalFolders is the local-folder seed surface (0195). Optional for RunReviver's reason: the
// test doubles that embed store.Store would route these calls to a nil interface. The api layer
// type-asserts, and refuses a local folder on a store without it; production is always PG.
type RunLocalFolders interface {
	// RunLocalFolderSeed reads run id's local folder and the outcome of its first sync. The folder
	// is nil on a run that has none.
	RunLocalFolderSeed(ctx context.Context, id uuid.UUID) (*types.RunLocalFolder, LocalFolderSeed, error)
	// SetRunLocalFolderSeed records the first sync's outcome, once: seeded when errText is empty,
	// failed with errText otherwise. False when the run has no local folder or an outcome is
	// already recorded.
	SetRunLocalFolderSeed(ctx context.Context, id uuid.UUID, errText string) (bool, error)
}

// LocalFolderSeed is the outcome of a run's first sync: neither field set while it is awaited.
type LocalFolderSeed struct {
	SeededAt *time.Time
	Error    string
}

// RunLocalFolderSeed implements RunLocalFolders.
func (s PG) RunLocalFolderSeed(ctx context.Context, id uuid.UUID) (*types.RunLocalFolder, LocalFolderSeed, error) {
	var raw []byte
	var seed LocalFolderSeed
	var errText *string
	err := s.Pool.QueryRow(ctx, `SELECT local_folder, local_folder_seeded_at, local_folder_seed_error FROM agent_runs WHERE id = $1`, id).
		Scan(&raw, &seed.SeededAt, &errText)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, LocalFolderSeed{}, ErrNotFound
	}
	if err != nil {
		return nil, LocalFolderSeed{}, fmt.Errorf("store: read run local folder: %w", err)
	}
	if errText != nil {
		seed.Error = *errText
	}
	if raw == nil {
		return nil, seed, nil
	}
	var f types.RunLocalFolder
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, LocalFolderSeed{}, fmt.Errorf("store: unmarshal run local folder: %w", err)
	}
	return &f, seed, nil
}

// SetRunLocalFolderSeed implements RunLocalFolders.
func (s PG) SetRunLocalFolderSeed(ctx context.Context, id uuid.UUID, errText string) (bool, error) {
	const q = `
		UPDATE agent_runs SET
			local_folder_seeded_at = CASE WHEN $2 = '' THEN now() END,
			local_folder_seed_error = NULLIF($2, '')
		WHERE id = $1 AND local_folder IS NOT NULL
		  AND local_folder_seeded_at IS NULL AND local_folder_seed_error IS NULL`
	tag, err := s.Pool.Exec(ctx, q, id, errText)
	if err != nil {
		return false, fmt.Errorf("store: record run local folder seed: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
