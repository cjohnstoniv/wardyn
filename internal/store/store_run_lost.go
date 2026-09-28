// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Lost-run surface (long-holds design rev 4, RL-9; migration 0083): token
// stamp, lapsed-token read, and lost claim. Split out of store.go for size,
// like store_run_lease.go.
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RunLoser is the lost-run surface, kept optional because test doubles that
// embed store.Store would otherwise route these calls to a nil interface;
// the api layer type-asserts, and production is always PG.
type RunLoser interface {
	// StampRunTokenRenewed records id's token renewal at the database's clock.
	// false means the run is already kept (lost/ended), so the renew must be
	// refused — a lost run's identity must never carry forward.
	StampRunTokenRenewed(ctx context.Context, id uuid.UUID) (bool, error)
	// ListLapsedTokenRuns returns every RUNNING, not-kept run whose token was
	// last renewed more than life ago, by the database's clock.
	ListLapsedTokenRuns(ctx context.Context, life time.Duration) ([]types.AgentRun, error)
	// MarkRunLost marks id kept and lost for reason at now, only while RUNNING
	// and not already kept; with tokenLife>0 it re-requires the token still
	// lapsed by that much, so a race with a renew resolves to the renew.
	// Exactly one caller sees true. It clears a pause only for reboot — the
	// one reason whose container is truly gone — since an outage's agent
	// keeps running (frozen or not) and clearing here would make resume skip
	// a thaw a still-frozen agent still needs.
	MarkRunLost(ctx context.Context, id uuid.UUID, reason types.LostReason, now time.Time, tokenLife time.Duration) (bool, error)
}

var _ RunLoser = PG{}

// StampRunTokenRenewed — see RunLoser.
func (s PG) StampRunTokenRenewed(ctx context.Context, id uuid.UUID) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE agent_runs SET token_renewed_at = now()
		WHERE id=$1 AND lost_at IS NULL`, id)
	if err != nil {
		return false, fmt.Errorf("store: stamp run token renewed: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ListLapsedTokenRuns — see RunLoser. Both sides use the database's clock, so
// replica clock skew cannot make a live run look lapsed.
func (s PG) ListLapsedTokenRuns(ctx context.Context, life time.Duration) ([]types.AgentRun, error) {
	q := `SELECT ` + runCols + ` FROM agent_runs
		WHERE state = $1 AND lost_at IS NULL
		  AND token_renewed_at < now() - $2::interval`
	return collect(ctx, s.Pool, "list", "lapsed token runs", q,
		[]any{string(types.RunRunning), life.String()}, scanRun)
}

// MarkRunLost — see RunLoser.
func (s PG) MarkRunLost(ctx context.Context, id uuid.UUID, reason types.LostReason, now time.Time, tokenLife time.Duration) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE agent_runs SET lost_at=$2, lost_reason=$3,
			paused_at=CASE WHEN $3=$6 THEN NULL ELSE paused_at END,
			paused_reason=CASE WHEN $3=$6 THEN '' ELSE paused_reason END
		WHERE id=$1 AND state=$4 AND lost_at IS NULL
		  AND ($5::interval = interval '0' OR token_renewed_at < now() - $5::interval)`,
		id, now, string(reason), string(types.RunRunning), tokenLife.String(), string(types.LostReboot))
	if err != nil {
		return false, fmt.Errorf("store: mark run lost: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}
