// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Lost runs (long-holds design rev 4, RL-9; migration 0076): the token stamp
// the renew door writes, the read the lapsed-token sweep makes, and the claim
// that marks a run lost. Kept out of store.go for the same size reason as
// store_run_lease.go.
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RunLoser is the lost-run surface. Optional for RunLeaser's reason: the test
// doubles that embed store.Store would route these calls to a nil interface.
// The api layer type-asserts; production is always PG.
type RunLoser interface {
	// StampRunTokenRenewed records that run id's token was just renewed, at the
	// database's clock. false means the run is kept (lost or ended), so the
	// renew must be refused: nothing may carry a lost run's identity forward.
	StampRunTokenRenewed(ctx context.Context, id uuid.UUID) (bool, error)
	// ListLapsedTokenRuns returns every RUNNING run that is not kept and whose
	// token was last renewed more than life ago, by the database's clock.
	ListLapsedTokenRuns(ctx context.Context, life time.Duration) ([]types.AgentRun, error)
	// MarkRunLost marks run id kept and lost for reason at now, but only while
	// it is RUNNING and not already kept. With tokenLife > 0 it also requires
	// the token to be lapsed by that much still, so a renew that lands between
	// the list and the mark wins. Exactly one caller sees true.
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

// ListLapsedTokenRuns — see RunLoser. Both sides of the comparison are the
// database's clock (the stamp is its now() too), so replica clock skew cannot
// make a live run look lapsed.
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
		UPDATE agent_runs SET lost_at=$2, lost_reason=$3
		WHERE id=$1 AND state=$4 AND lost_at IS NULL
		  AND ($5::interval = interval '0' OR token_renewed_at < now() - $5::interval)`,
		id, now, string(reason), string(types.RunRunning), tokenLife.String())
	if err != nil {
		return false, fmt.Errorf("store: mark run lost: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}
