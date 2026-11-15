// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// FolderGivenUp is what a run's folder record says of its give-up, read from the stored JSON without decoding the
// record: when the changes not yet back were given up (nil: they were not), and how many there were (nil: nobody
// counted them, or the stored count is not a whole number).
type FolderGivenUp struct {
	At    *time.Time
	Count *int
}

// RunLocalFolderGivenUps is the give-up read for a folder record that cannot be decoded, optional like
// RunLocalFolderStatuses: the run view still shows a give-up that the record itself cannot be read for. The
// give-up statement (GiveUpRunLocalFolder) needs no decoded record either, so such a record can be given up.
type RunLocalFolderGivenUps interface {
	// RunLocalFolderGivenUp reads run id's give-up. ErrNotFound when the run is gone.
	RunLocalFolderGivenUp(ctx context.Context, id uuid.UUID) (FolderGivenUp, error)
}

var _ RunLocalFolderGivenUps = PG{}

// RunLocalFolderGivenUp implements RunLocalFolderGivenUps. A stored given_up_at that is not a time is an error.
func (s PG) RunLocalFolderGivenUp(ctx context.Context, id uuid.UUID) (FolderGivenUp, error) {
	var at, count *string
	err := s.Pool.QueryRow(ctx, `SELECT local_folder_status->'back'->>'given_up_at', local_folder_status->'back'->>'given_up_count'
		FROM agent_runs WHERE id = $1`, id).Scan(&at, &count)
	if errors.Is(err, pgx.ErrNoRows) {
		return FolderGivenUp{}, ErrNotFound
	}
	if err != nil {
		return FolderGivenUp{}, fmt.Errorf("store: read a folder record's give-up: %w", err)
	}
	var g FolderGivenUp
	if at != nil {
		t, err := time.Parse(time.RFC3339Nano, *at)
		if err != nil {
			return FolderGivenUp{}, fmt.Errorf("store: read a folder record's give-up time: %w", err)
		}
		g.At = &t
	}
	if count != nil {
		if n, err := strconv.Atoi(*count); err == nil {
			g.Count = &n
		}
	}
	return g, nil
}
