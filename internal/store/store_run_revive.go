// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Revive and restart with current limits (long-holds design rev 4, RL-10 and
// RL-11; migration 0084): the claim a revive makes before it replaces a run's
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
	// MarkRunRevived claims run id for a new proxy: while it is RUNNING and
	// still as the revive read it, live (from "") or lost to an outage or a
	// reboot (from that reason; never its end), it clears the lost mark, stamps
	// the token as just renewed (the revive mints a fresh one, and the
	// lapsed-token sweep must not read the old stamp) and refreshes the watcher
	// lease (a rebooted agent is started only after the new proxy, and the
	// watcher sweep must not probe it before then). false means the run went
	// terminal, ended, was lost or revived since, and must get no proxy.
	MarkRunRevived(ctx context.Context, id uuid.UUID, from types.LostReason) (bool, error)
	// SetRunProxyRelease records release as the one that started run id's
	// proxy, once a revive's new proxy runs.
	SetRunProxyRelease(ctx context.Context, id uuid.UUID, release string) error
	// ListRunProxyReleases returns every non-terminal run that has a sandbox,
	// with the release that started its proxy ("" before migration 0084).
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
func (s PG) MarkRunRevived(ctx context.Context, id uuid.UUID, from types.LostReason) (bool, error) {
	if from != "" && from != types.LostOutage && from != types.LostReboot {
		return false, nil
	}
	tag, err := s.Pool.Exec(ctx, `
		UPDATE agent_runs SET lost_at=NULL, lost_reason='', token_renewed_at=now(), watcher_heartbeat=now(), updated_at=now()
		WHERE id=$1 AND state=$2 AND (lost_at IS NOT NULL) = ($3 <> '') AND lost_reason=$3`,
		id, string(types.RunRunning), string(from))
	if err != nil {
		return false, fmt.Errorf("store: mark run revived: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// SetRunProxyRelease — see RunReviver.
func (s PG) SetRunProxyRelease(ctx context.Context, id uuid.UUID, release string) error {
	if _, err := s.Pool.Exec(ctx, `UPDATE agent_runs SET proxy_release=$2, updated_at=now() WHERE id=$1`, id, release); err != nil {
		return fmt.Errorf("store: set run proxy release: %w", err)
	}
	return nil
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
