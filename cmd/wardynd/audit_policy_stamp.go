// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// withPolicyStamp puts audit.PolicyStampRecorder above next, reading each run's digest from agent_runs. A
// chain with no pool (the maintenance modes) records no run rows, so it has nothing to stamp and next is
// returned as it is.
func withPolicyStamp(pool *pgxpool.Pool, next audit.Recorder) audit.Recorder {
	if pool == nil {
		return next
	}
	return audit.PolicyStampRecorder{Inner: next, Lookup: func(ctx context.Context, runID uuid.UUID) (string, error) {
		digest, _, err := store.RunPolicyDigest(ctx, pool, runID)
		return digest, err
	}}
}
