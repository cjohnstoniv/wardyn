// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RunnerInventoryStore is the optional capability behind the runners management routes: the
// unused registration tokens and the count of runs on each runner. Revoking a runner is the
// revoke door's, not this one's.
type RunnerInventoryStore interface {
	RunnerStore
	ListUnusedRunnerRegistrationTokens(ctx context.Context, now time.Time) ([]types.RunnerRegistrationToken, error)
	RevokeRunnerRegistrationToken(ctx context.Context, id uuid.UUID, now time.Time) (types.RunnerRegistrationToken, error)
	CountActiveRunsByRunner(ctx context.Context) (map[uuid.UUID]int, error)
}

var _ RunnerInventoryStore = PG{}

// ListUnusedRunnerRegistrationTokens returns every token still redeemable at now, newest first.
// Never a token value: the table holds only its hash, which the scan does not serialize.
func (s PG) ListUnusedRunnerRegistrationTokens(ctx context.Context, now time.Time) ([]types.RunnerRegistrationToken, error) {
	const q = `SELECT ` + runnerRegistrationCols + ` FROM runner_registration_tokens
		WHERE consumed_at IS NULL AND expires_at > $1 ORDER BY created_at DESC, id`
	return collect(ctx, s.Pool, "list", "runner registration tokens", q, []any{now}, scanRunnerRegistrationToken)
}

// RevokeRunnerRegistrationToken spends an unused token by setting the column
// ConsumeRunnerRegistrationToken requires NULL, in one conditional UPDATE: a revoke and a
// redemption of the same token take the same row lock, so exactly one of them wins. A token that
// was redeemed, revoked, expired or never existed is ErrNotFound.
func (s PG) RevokeRunnerRegistrationToken(ctx context.Context, id uuid.UUID, now time.Time) (types.RunnerRegistrationToken, error) {
	const q = `UPDATE runner_registration_tokens SET consumed_at = $2
		WHERE id = $1 AND consumed_at IS NULL AND expires_at > $2
		RETURNING ` + runnerRegistrationCols
	return scanRunnerRegistrationToken(s.Pool.QueryRow(ctx, q, id, now))
}

// CountActiveRunsByRunner is how many non-terminal runs sit on each runner, for the runners that
// have any.
func (s PG) CountActiveRunsByRunner(ctx context.Context) (map[uuid.UUID]int, error) {
	const q = `SELECT runner_id, count(*) FROM agent_runs
		WHERE runner_id IS NOT NULL AND state = ANY($1) GROUP BY runner_id`
	type runnerRuns struct {
		id uuid.UUID
		n  int
	}
	rows, err := collect(ctx, s.Pool, "count", "active runs by runner", q, []any{nonTerminalStateStrings()}, func(r pgx.Row) (runnerRuns, error) {
		var v runnerRuns
		err := r.Scan(&v.id, &v.n)
		return v, err
	})
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]int, len(rows))
	for _, v := range rows {
		out[v.id] = v.n
	}
	return out, nil
}
