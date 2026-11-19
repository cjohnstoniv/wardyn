// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// A source scan and a workspace import step claim their row's active_run_id before they write the run
// row. A daemon that dies between the two leaves a claim naming a run that never existed, and no run end
// will release it: the source refuses every scan and the workspace every record and delete, across
// restarts. ReleaseStrandedRunClaims frees them.
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// StrandedRunClaim is one claim ReleaseStrandedRunClaims released: Kind is "source" or "workspace", ID
// that row, RunID the run the claim named.
type StrandedRunClaim struct {
	Kind  string
	ID    uuid.UUID
	RunID uuid.UUID
}

// ReleaseStrandedRunClaims clears every source and workspace claim whose run row does not exist and whose
// row nothing has written for olderThan. The claim sets updated_at and every later write only moves it
// forward, so a launch between its claim and its run insert is released only once it has been stuck there
// for olderThan; a claim whose run row exists is never touched here (its run's end releases it). A source
// goes back to error with its profile kept; a workspace's status goes to error if the step left it
// scanning. Each release is fenced on the claim it read, and rows another writer holds are skipped.
func (s PG) ReleaseStrandedRunClaims(ctx context.Context, olderThan time.Duration) ([]StrandedRunClaim, error) {
	const q = `
		WITH stranded_sources AS (
			SELECT id, active_run_id AS run_id FROM sources
			 WHERE active_run_id IS NOT NULL AND updated_at < now() - make_interval(secs => $1)
			   AND NOT EXISTS (SELECT 1 FROM agent_runs r WHERE r.id = sources.active_run_id)
			   FOR UPDATE SKIP LOCKED
		), freed_sources AS (
			UPDATE sources s SET active_run_id = NULL, status = 'error', updated_at = now()
			  FROM stranded_sources x WHERE s.id = x.id AND s.active_run_id = x.run_id
			RETURNING 'source' AS kind, s.id, x.run_id
		), stranded_workspaces AS (
			SELECT id, active_run_id AS run_id FROM workspaces
			 WHERE active_run_id IS NOT NULL AND updated_at < now() - make_interval(secs => $1)
			   AND NOT EXISTS (SELECT 1 FROM agent_runs r WHERE r.id = workspaces.active_run_id)
			   FOR UPDATE SKIP LOCKED
		), freed_workspaces AS (
			UPDATE workspaces w SET active_run_id = NULL,
			       status = CASE WHEN w.status = 'scanning' THEN 'error' ELSE w.status END, updated_at = now()
			  FROM stranded_workspaces x WHERE w.id = x.id AND w.active_run_id = x.run_id
			RETURNING 'workspace' AS kind, w.id, x.run_id
		)
		SELECT kind, id, run_id FROM freed_sources
		UNION ALL
		SELECT kind, id, run_id FROM freed_workspaces`
	rows, err := s.Pool.Query(ctx, q, olderThan.Seconds())
	if err != nil {
		return nil, fmt.Errorf("store: release stranded run claims: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (StrandedRunClaim, error) {
		var c StrandedRunClaim
		err := r.Scan(&c.Kind, &c.ID, &c.RunID)
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: release stranded run claims: %w", err)
	}
	return out, nil
}
