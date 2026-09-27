// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// RunTitler is the run-rename surface (#1197 L2, design.md §3.4). Optional
// like RunLeaser and for the same reason: the ~30 hand-written test doubles
// that implement Store directly would otherwise need a mechanical update for
// a route most of them never exercise. The api layer type-asserts; production
// is always PG.
type RunTitler interface {
	// SetRunTitle overwrites run id's title unconditionally — a rename is
	// allowed in any run state (design.md §3.4), unlike the lease's end/wait,
	// so there is no state guard and no compare-and-swap here.
	SetRunTitle(ctx context.Context, id uuid.UUID, title string) error
}

var _ RunTitler = PG{}

// SetRunTitle — see RunTitler.
func (s PG) SetRunTitle(ctx context.Context, id uuid.UUID, title string) error {
	if _, err := s.Pool.Exec(ctx, `UPDATE agent_runs SET title=$2 WHERE id=$1`, id, title); err != nil {
		return fmt.Errorf("store: set run title: %w", err)
	}
	return nil
}
