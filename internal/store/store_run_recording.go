// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// SetRunNotRecorded writes why the run has no session recording ("" when it is recorded), as its
// dispatch decided it.
func (s PG) SetRunNotRecorded(ctx context.Context, id uuid.UUID, v types.RunNotRecorded) error {
	return s.execRun(ctx, "set run not recorded", `UPDATE agent_runs SET not_recorded=$2 WHERE id=$1`, id, string(v))
}
