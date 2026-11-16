// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// RunLocalFolderResumes is the resume read for a folder record that cannot be decoded, optional like
// RunLocalFolderStatuses: a stop that is about to clear a run's sandbox ref still finds a resume the record holds,
// and gives that copy back up with its given_up row. The give-up statement (GiveUpRunLocalFolder) needs no decoded
// record either.
type RunLocalFolderResumes interface {
	// RunLocalFolderHoldsResume reports whether run id's stored record holds a resume: present and not JSON null,
	// as decoding reads it (folderDoneSQL and retirementFolderKeepSQL read it the same way). A run with no record,
	// and an unknown run, hold none. An error is a read that failed, and no answer.
	RunLocalFolderHoldsResume(ctx context.Context, id uuid.UUID) (bool, error)
}

var _ RunLocalFolderResumes = PG{}

// RunLocalFolderHoldsResume implements RunLocalFolderResumes.
func (s PG) RunLocalFolderHoldsResume(ctx context.Context, id uuid.UUID) (bool, error) {
	var held bool
	err := s.Pool.QueryRow(ctx, `SELECT COALESCE(local_folder_status->'resume', 'null'::jsonb) <> 'null'::jsonb
		FROM agent_runs WHERE id = $1`, id).Scan(&held)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: read whether a folder record holds a resume: %w", err)
	}
	return held, nil
}
