// Copyright 2026 The Wardyn Authors
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

// RunEndsReader is a lost run's end, decided on the database's clock, never on a server's own: the stored
// ends_at is compared with now() in SQL and the caller gets booleans. It is optional; the api layer
// type-asserts it, and a store without it answers the stronger stop (see api's outageProxyOnly).
type RunEndsReader interface {
	// RunEndsOpen answers, for one run, endsOpen (it has no end, or its end is after now()) and keepable (it
	// has no end, or its end plus grace is after now(): the files grace of an ended run has not run out).
	// Both are strict, so a run exactly at its end is past it. ErrNotFound for a run that does not exist.
	RunEndsOpen(ctx context.Context, id uuid.UUID, grace time.Duration) (endsOpen, keepable bool, err error)
}

var _ RunEndsReader = PG{}

// runEndsSQL is the two booleans of RunEndsOpen over agent_runs' own columns, $2 being the grace in seconds
// (make_interval's secs, as dueFlagsSQL passes its keep). A NULL ends_at is TRUE for both, never NULL.
const runEndsSQL = `SELECT ends_at IS NULL OR ends_at > now(),
	ends_at IS NULL OR ends_at + make_interval(secs => $2) > now()
	FROM agent_runs WHERE id = $1`

// RunEndsOpen implements RunEndsReader.
func (s PG) RunEndsOpen(ctx context.Context, id uuid.UUID, grace time.Duration) (endsOpen, keepable bool, err error) {
	err = s.Pool.QueryRow(ctx, runEndsSQL, id, grace.Seconds()).Scan(&endsOpen, &keepable)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, ErrNotFound
	}
	if err != nil {
		return false, false, fmt.Errorf("store: read run end flags: %w", err)
	}
	return endsOpen, keepable, nil
}
