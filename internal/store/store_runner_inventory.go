// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RunnerInventoryStore is the optional capability behind the runners management routes: a page of
// the inventory, the unused registration tokens and the count of runs on each runner. Revoking a
// runner is the revoke door's, not this one's.
type RunnerInventoryStore interface {
	RunnerStore
	// ListRunnersPage lists the runners the filter names, newest first, never an unclaimed one that
	// has outlived its wait at now (the sweeper may not have deleted the row yet).
	ListRunnersPage(ctx context.Context, filter types.RunnerFilter, now time.Time, p Page) ([]types.Runner, error)
	// ListUnusedRunnerRegistrationTokensPage lists tokens redeemable at now, newest first, for one
	// owner or, with owner "", every owner.
	ListUnusedRunnerRegistrationTokensPage(ctx context.Context, now time.Time, owner string, p Page) ([]types.RunnerRegistrationToken, error)
	RevokeRunnerRegistrationToken(ctx context.Context, id uuid.UUID, now time.Time) (types.RunnerRegistrationToken, error)
	// CountActiveRunsByRunner counts non-terminal runs on each of ids; a runner with none is absent.
	CountActiveRunsByRunner(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]int, error)
}

var _ RunnerInventoryStore = PG{}

// ListRunnersPage — see RunnerInventoryStore.
func (s PG) ListRunnersPage(ctx context.Context, filter types.RunnerFilter, now time.Time, p Page) ([]types.Runner, error) {
	stateClause := map[types.RunnerFilter]string{
		types.RunnerFilterActive: `state <> 'revoked'`, types.RunnerFilterRevoked: `state = 'revoked'`, types.RunnerFilterAll: `true`,
	}[filter]
	if stateClause == "" {
		return nil, fmt.Errorf("store: list runners: unknown filter %q", filter)
	}
	q, args := p.appendTo(`SELECT `+runnerCols+` FROM runners WHERE `+stateClause+
		` AND (state <> 'unclaimed' OR created_at > $1) ORDER BY created_at DESC, id`, []any{now.Add(-types.RunnerUnclaimedTTL)})
	return collect(ctx, s.Pool, "list", "runners", q, args, scanRunner)
}

// ListUnusedRunnerRegistrationTokensPage — see RunnerInventoryStore. Never a token value: the table
// holds only its hash, which the wire type does not serialize.
func (s PG) ListUnusedRunnerRegistrationTokensPage(ctx context.Context, now time.Time, owner string, p Page) ([]types.RunnerRegistrationToken, error) {
	q, args := p.appendTo(`SELECT `+runnerRegistrationCols+` FROM runner_registration_tokens
		WHERE consumed_at IS NULL AND expires_at > $1 AND ($2 = '' OR owner = $2) ORDER BY created_at DESC, id`, []any{now, owner})
	return collect(ctx, s.Pool, "list", "runner registration tokens", q, args, scanRunnerRegistrationToken)
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

// CountActiveRunsByRunner — see RunnerInventoryStore.
func (s PG) CountActiveRunsByRunner(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]int, error) {
	if len(ids) == 0 {
		return map[uuid.UUID]int{}, nil
	}
	const q = `SELECT runner_id, count(*) FROM agent_runs
		WHERE runner_id = ANY($2) AND state = ANY($1) GROUP BY runner_id`
	type runnerRuns struct {
		id uuid.UUID
		n  int
	}
	rows, err := collect(ctx, s.Pool, "count", "active runs by runner", q, []any{nonTerminalStateStrings(), ids}, func(r pgx.Row) (runnerRuns, error) {
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
