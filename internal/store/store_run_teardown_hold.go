// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

// The organisation's copy of a runner's teardown-hold report (migration 0286_run_teardown_holds): a kill or
// end of a run that the runner refused because it could not see it finish, kept open until the runner
// reports that it finished.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RunTeardownHoldRow is the stored report. ClearedAt is nil while the runner holds the run.
type RunTeardownHoldRow struct {
	Kind      string
	Since     time.Time
	ClearedAt *time.Time
}

// syncRunTeardownHoldTx keeps the organisation's copy of row's teardown-hold report, for a run the page's
// own checks bound to runnerID. An open report opens the copy, or keeps it open. A cleared report replaces a
// copy whose since is not later than the report's, so a report of an older hold never closes a newer one,
// and is stored as cleared when there is no copy. A row with no report, or one this release cannot read,
// changes nothing: only the runner's own word that the kill or end finished clears a hold.
func syncRunTeardownHoldTx(ctx context.Context, tx pgx.Tx, runnerID uuid.UUID, row runnerwire.RunState) error {
	h := row.TeardownHold
	if h == nil || (h.Kind != types.RunnerActionKill && h.Kind != types.RunnerActionEnd) || h.Since.IsZero() || (h.ClearedAt != nil && h.ClearedAt.IsZero()) {
		return nil
	}
	if h.ClearedAt == nil {
		_, err := tx.Exec(ctx, `INSERT INTO run_teardown_holds (run_id, runner_id, kind, since) VALUES ($1, $2, $3, $4)
			ON CONFLICT (run_id) DO UPDATE SET kind = EXCLUDED.kind, since = EXCLUDED.since, cleared_at = NULL, observed_at = clock_timestamp()
			WHERE run_teardown_holds.runner_id = EXCLUDED.runner_id`, row.RunID, runnerID, string(h.Kind), h.Since.UTC())
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO run_teardown_holds (run_id, runner_id, kind, since, cleared_at) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (run_id) DO UPDATE SET kind = EXCLUDED.kind, since = EXCLUDED.since, cleared_at = EXCLUDED.cleared_at, observed_at = clock_timestamp()
		WHERE run_teardown_holds.runner_id = EXCLUDED.runner_id AND run_teardown_holds.since <= EXCLUDED.since`, row.RunID, runnerID, string(h.Kind), h.Since.UTC(), h.ClearedAt.UTC())
	return err
}

// GetRunTeardownHold is the run's stored teardown-hold report, or ErrNotFound.
func (s PG) GetRunTeardownHold(ctx context.Context, runID uuid.UUID) (RunTeardownHoldRow, error) {
	var out RunTeardownHoldRow
	err := s.Pool.QueryRow(ctx, `SELECT kind, since, cleared_at FROM run_teardown_holds WHERE run_id = $1`, runID).
		Scan(&out.Kind, &out.Since, &out.ClearedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrNotFound
	}
	return out, err
}

// RunTeardownHoldClearedAfterRefusal reports whether runID's runner has reported, on a state page, that the
// kill or end it held the run for finished later than its latest refusal of a kill of the run at ref. Both are
// normally the runner's own times: the hold's cleared_at, and the observed_at of the refused kill's receipt
// (an action the organisation refused for its runner carries the organisation's time).
func (s PG) RunTeardownHoldClearedAfterRefusal(ctx context.Context, runID uuid.UUID, ref string) (bool, error) {
	var cleared bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM run_teardown_holds h WHERE h.run_id = $1 AND h.cleared_at > (
		SELECT max(t.observed_at) FROM runner_pending_actions t WHERE t.run_id = $1 AND t.ref = $2 AND t.kind = 'kill' AND t.outcome = 'refused'))`,
		runID, ref).Scan(&cleared)
	if err != nil {
		return false, fmt.Errorf("store: read run teardown hold: %w", err)
	}
	return cleared, nil
}
