// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// RunImageUse is what this database's runs say about one image ref.
type RunImageUse struct {
	Ref string
	// LastCreated is the newest creation time of a run that records Ref as its image.
	LastCreated time.Time
	// InUse is true while a run that records Ref has not ended, or has ended and keeps its sandbox.
	InUse bool
}

// RunImageUseReader is deliberately not part of Store (like ModelUsageStore): the reuse image reclaim type-asserts it
// and removes nothing when a store lacks it. PG always has it.
type RunImageUseReader interface {
	// RunImageUse answers one row for each of refs that some run records as its image; a ref no run records has none.
	RunImageUse(ctx context.Context, refs []string) ([]RunImageUse, error)
}

var _ RunImageUseReader = PG{}

// RunImageUse — see RunImageUseReader. One scan of agent_runs, made only for tags already past the reclaim's age.
func (s PG) RunImageUse(ctx context.Context, refs []string) ([]RunImageUse, error) {
	if len(refs) == 0 {
		return []RunImageUse{}, nil
	}
	const q = `SELECT image, max(created_at), bool_or(NOT (state = ANY($2)) OR COALESCE(sandbox_ref, '') <> '')
		FROM agent_runs WHERE image = ANY($1) GROUP BY image`
	return collect(ctx, s.Pool, "read", "run image use", q, []any{refs, terminalRunStates}, func(r pgx.Row) (RunImageUse, error) {
		var u RunImageUse
		err := r.Scan(&u.Ref, &u.LastCreated, &u.InUse)
		return u, err
	})
}
