// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Lost-run surface (long-holds design rev 4, RL-9; migration 0083): token
// stamp, lapsed-token read, and lost claim. Split out of store.go for size,
// like store_run_lease.go.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RunLoser is the lost-run surface, kept optional because test doubles that
// embed store.Store would otherwise route these calls to a nil interface;
// the api layer type-asserts, and production is always PG.
type RunLoser interface {
	// StampRunTokenRenewed records id's token renewal at the database's clock,
	// with the renewal's new token id and the id of the token it presented.
	// false means the run is already kept (lost/ended), so the renew must be
	// refused — a lost run's identity must never carry forward.
	// ErrRunTokenLineage means prevJTI is not a token of the run's current
	// lineage (one RetireRunTokenLineage cut off): nothing is recorded.
	StampRunTokenRenewed(ctx context.Context, id uuid.UUID, jti, prevJTI string) (bool, error)
	// RetireRunTokenLineage makes fresh the only token a renewal of id may
	// present from now on, and returns the ids the last renewal recorded (the
	// tokens a revive must revoke beside the one it replaces). The read and the
	// cut are one statement, so no renewal lands between them.
	RetireRunTokenLineage(ctx context.Context, id uuid.UUID, fresh string) (jti, prev string, err error)
	// RestoreRunTokenLineage puts back the lineage RetireRunTokenLineage cut,
	// only while fresh still heads it alone.
	RestoreRunTokenLineage(ctx context.Context, id uuid.UUID, fresh, jti, prev string) error
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

// ErrRunTokenLineage is StampRunTokenRenewed's refusal of a token outside the run's current lineage.
var ErrRunTokenLineage = errors.New("store: the presented token is not of the run's current token lineage")

// StampRunTokenRenewed — see RunLoser. A run with no recorded renewal has no lineage yet; otherwise
// the presented token must be the last one minted, or the one that renewal presented (a proxy keeps
// it when the lease send behind a renewal fails).
func (s PG) StampRunTokenRenewed(ctx context.Context, id uuid.UUID, jti, prevJTI string) (bool, error) {
	var stamped, live bool
	err := s.Pool.QueryRow(ctx, `
		WITH stamped AS (
			UPDATE agent_runs SET token_renewed_at = now(), token_jti = $2, token_prev_jti = $3
			WHERE id=$1 AND lost_at IS NULL
			  AND (COALESCE(token_jti, '') = '' OR $3 IN (token_jti, token_prev_jti))
			RETURNING 1)
		SELECT EXISTS (SELECT 1 FROM stamped), EXISTS (SELECT 1 FROM agent_runs WHERE id=$1 AND lost_at IS NULL)`,
		id, jti, prevJTI).Scan(&stamped, &live)
	switch {
	case err != nil:
		return false, fmt.Errorf("store: stamp run token renewed: %w", err)
	case stamped:
		return true, nil
	case live:
		return false, ErrRunTokenLineage
	}
	return false, nil
}

// RetireRunTokenLineage — see RunLoser. It touches only the two columns, so runCols and scanRun are untouched.
func (s PG) RetireRunTokenLineage(ctx context.Context, id uuid.UUID, fresh string) (jti, prev string, err error) {
	var j, p *string
	err = s.Pool.QueryRow(ctx, `
		UPDATE agent_runs r SET token_jti = $2, token_prev_jti = NULL
		FROM (SELECT id, token_jti, token_prev_jti FROM agent_runs WHERE id=$1 FOR UPDATE) old
		WHERE r.id = old.id
		RETURNING old.token_jti, old.token_prev_jti`, id, fresh).Scan(&j, &p)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("store: retire run token lineage: %w", err)
	}
	if j != nil {
		jti = *j
	}
	if p != nil {
		prev = *p
	}
	return jti, prev, nil
}

// RestoreRunTokenLineage — see RunLoser.
func (s PG) RestoreRunTokenLineage(ctx context.Context, id uuid.UUID, fresh, jti, prev string) error {
	if _, err := s.Pool.Exec(ctx, `
		UPDATE agent_runs SET token_jti = NULLIF($3, ''), token_prev_jti = NULLIF($4, '')
		WHERE id=$1 AND token_jti = $2 AND token_prev_jti IS NULL`, id, fresh, jti, prev); err != nil {
		return fmt.Errorf("store: restore run token lineage: %w", err)
	}
	return nil
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
	tag, err := s.Pool.Exec(ctx, markRunLostSQL,
		id, now, string(reason), string(types.RunRunning), tokenLife.String(), string(types.LostReboot))
	if err != nil {
		return false, fmt.Errorf("store: mark run lost: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// Shared by the authenticated runner loss claim and the existing loss sweeper.
const markRunLostSQL = `
		UPDATE agent_runs SET lost_at=$2, lost_reason=$3,
			paused_at=CASE WHEN $3=$6 THEN NULL ELSE paused_at END,
			paused_reason=CASE WHEN $3=$6 THEN '' ELSE paused_reason END
		WHERE id=$1 AND state=$4 AND lost_at IS NULL
		  AND ($5::interval = interval '0' OR token_renewed_at < now() - $5::interval)`
