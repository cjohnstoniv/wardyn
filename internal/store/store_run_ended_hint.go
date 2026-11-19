// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"

	"github.com/google/uuid"
)

// SetEndedRunFailureHint scoped-writes ONLY failure_hint, as SetRunFailureHint does, for a run that is over.
// Deliberately does NOT bump updated_at, as SetRunStatusDetail does not: a terminal run's tail-upload grace is
// measured from that column, so a hint a late report carries must never hold that door open again.
// ErrNotFound when no row matched.
func (s PG) SetEndedRunFailureHint(ctx context.Context, id uuid.UUID, hint string) error {
	return s.execRun(ctx, "set ended run failure hint", `UPDATE agent_runs SET failure_hint=$1 WHERE id=$2`, hint, id)
}
