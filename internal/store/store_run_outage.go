// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RunOutageTermer records and reads a run's outage terms.
type RunOutageTermer interface {
	// SetRunOutageTerms records id's terms once and returns the recorded terms: a second call keeps, and returns, the
	// first.
	SetRunOutageTerms(ctx context.Context, id uuid.UUID, t types.OutageTerms) (types.OutageTerms, error)
	// RunOutageTerms is id's recorded terms, or the defaults for a run that has none.
	RunOutageTerms(ctx context.Context, id uuid.UUID) (types.OutageTerms, error)
}

var _ RunOutageTermer = PG{}

func (s PG) SetRunOutageTerms(ctx context.Context, id uuid.UUID, t types.OutageTerms) (types.OutageTerms, error) {
	// One statement: the inserted row, or on a conflict the row already there (read in the statement's snapshot).
	got := types.DefaultOutageTerms()
	err := s.Pool.QueryRow(ctx, `WITH ins AS (
		INSERT INTO run_outage_terms (run_id, allowance_s, strict, end_unattended) VALUES ($1, $2, $3, $4)
		ON CONFLICT (run_id) DO NOTHING RETURNING allowance_s, strict, end_unattended)
	SELECT allowance_s, strict, end_unattended FROM ins
	UNION ALL
	SELECT allowance_s, strict, end_unattended FROM run_outage_terms WHERE run_id = $1 AND NOT EXISTS (SELECT 1 FROM ins)`,
		id, t.AllowanceSec, t.Strict, t.EndUnattended).Scan(&got.AllowanceSec, &got.Strict, &got.EndUnattended)
	if errors.Is(err, pgx.ErrNoRows) {
		// A concurrent first write of the same run committed after this statement's snapshot: the insert
		// conflicted and the read could not see that row. It is committed now, so a fresh read returns it.
		return s.RunOutageTerms(ctx, id)
	}
	if err != nil {
		return types.OutageTerms{}, fmt.Errorf("store: record run outage terms: %w", err)
	}
	return got, nil
}

func (s PG) RunOutageTerms(ctx context.Context, id uuid.UUID) (types.OutageTerms, error) {
	t := types.DefaultOutageTerms()
	err := s.Pool.QueryRow(ctx, `SELECT allowance_s, strict, end_unattended FROM run_outage_terms WHERE run_id = $1`, id).
		Scan(&t.AllowanceSec, &t.Strict, &t.EndUnattended)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.DefaultOutageTerms(), nil
	}
	if err != nil {
		return types.OutageTerms{}, fmt.Errorf("store: read run outage terms: %w", err)
	}
	return t, nil
}
