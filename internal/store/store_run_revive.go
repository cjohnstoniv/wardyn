// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Proxy-only revive and restart with current limits (long-holds design rev 4,
// RL-10; migration 0072): the claim a revive makes before it replaces a run's
// proxy, and the read the admin version-window listing makes.
package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RunReviver is the revive surface. Optional for RunLeaser's reason: the test
// doubles that embed store.Store would route these calls to a nil interface.
// The api layer type-asserts; production is always PG.
type RunReviver interface {
	// MarkRunRevived claims run id for a new proxy started by release: while it
	// is RUNNING and either live or lost to an outage (the one kept run whose
	// agent still runs), it clears the lost mark, stamps the token as just
	// renewed (the revive mints a fresh one, and the lapsed-token sweep must
	// not read the old stamp) and records release. false means the run went
	// terminal, ended or was lost otherwise, and must get no proxy.
	MarkRunRevived(ctx context.Context, id uuid.UUID, release string) (bool, error)
	// ListRunProxyReleases returns every non-terminal run that has a sandbox,
	// with the release that started its proxy ("" before migration 0072).
	ListRunProxyReleases(ctx context.Context) ([]RunProxyRelease, error)
}

// RunProxyRelease is one row of the version-window listing.
type RunProxyRelease struct {
	RunID      uuid.UUID        `json:"run_id"`
	CreatedBy  string           `json:"created_by"`
	State      types.RunState   `json:"state"`
	LostReason types.LostReason `json:"lost_reason,omitempty"`
	Release    string           `json:"proxy_release"`
}

var _ RunReviver = PG{}

// MarkRunRevived — see RunReviver.
func (s PG) MarkRunRevived(ctx context.Context, id uuid.UUID, release string) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE agent_runs SET lost_at=NULL, lost_reason='', token_renewed_at=now(),
		       proxy_release=$2, updated_at=now()
		WHERE id=$1 AND state=$3 AND (lost_at IS NULL OR lost_reason=$4)`,
		id, release, string(types.RunRunning), string(types.LostOutage))
	if err != nil {
		return false, fmt.Errorf("store: mark run revived: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ListRunProxyReleases — see RunReviver.
func (s PG) ListRunProxyReleases(ctx context.Context) ([]RunProxyRelease, error) {
	states := make([]string, 0, len(types.NonTerminalRunStates))
	for _, st := range types.NonTerminalRunStates {
		states = append(states, string(st))
	}
	q := `SELECT id, created_by, state, lost_reason, proxy_release FROM agent_runs
		WHERE state = ANY($1) AND sandbox_ref <> '' ORDER BY created_at`
	return collect(ctx, s.Pool, "list", "run proxy releases", q, []any{states},
		func(row pgx.Row) (RunProxyRelease, error) {
			var r RunProxyRelease
			var state, reason string
			err := row.Scan(&r.RunID, &r.CreatedBy, &state, &reason, &r.Release)
			r.State, r.LostReason = types.RunState(state), types.LostReason(reason)
			return r, err
		})
}
