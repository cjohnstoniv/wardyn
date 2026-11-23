// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The states of a run's clone status row (#3159). cloning is the launcher's in-progress marker seen: the run
// reached the clone step, and a final state replaces it once. A run that never cloned has none or a none row.
const (
	CloneStateCloning     = "cloning"
	CloneStateReported    = "reported"
	CloneStateNotReported = "not_reported"
	CloneStateNone        = "none"
)

// RunCloneStatus is what the launcher said about a run's repository clones, as the server read it. Repos is the
// entries, a JSON array; the api package owns their shape.
type RunCloneStatus struct {
	RunID      uuid.UUID
	State      string
	Repos      json.RawMessage
	ObservedAt time.Time
}

// RunCloneStatusStore is the clone-status surface. Optional like ServiceRunStore: the api layer reads no clone
// status when a store lacks it.
type RunCloneStatusStore interface {
	PutRunCloneStatus(ctx context.Context, st RunCloneStatus) (bool, error)
	GetRunCloneStatus(ctx context.Context, runID uuid.UUID) (RunCloneStatus, error)
	ListCloneStatusRuns(ctx context.Context) ([]types.AgentRun, error)
}

var _ RunCloneStatusStore = PG{}

// PutRunCloneStatus writes a run's clone status. A cloning row is replaced once by a final state; a final row is
// never overwritten. False when nothing was written.
func (s PG) PutRunCloneStatus(ctx context.Context, st RunCloneStatus) (bool, error) {
	repos := st.Repos
	if len(repos) == 0 {
		repos = json.RawMessage("[]")
	}
	tag, err := s.Pool.Exec(ctx, `INSERT INTO run_clone_status (run_id, state, repos, observed_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (run_id) DO UPDATE
		   SET state = EXCLUDED.state, repos = EXCLUDED.repos, observed_at = EXCLUDED.observed_at
		 WHERE run_clone_status.state = 'cloning' AND EXCLUDED.state <> 'cloning'`,
		st.RunID, st.State, []byte(repos), st.ObservedAt)
	if err != nil {
		return false, fmt.Errorf("store: put run clone status: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// GetRunCloneStatus reads a run's clone status; ErrNotFound when there is no row.
func (s PG) GetRunCloneStatus(ctx context.Context, runID uuid.UUID) (RunCloneStatus, error) {
	st := RunCloneStatus{RunID: runID}
	var repos []byte
	err := s.Pool.QueryRow(ctx, `SELECT state, repos, observed_at FROM run_clone_status WHERE run_id = $1`, runID).
		Scan(&st.State, &repos, &st.ObservedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return RunCloneStatus{}, ErrNotFound
	}
	if err != nil {
		return RunCloneStatus{}, fmt.Errorf("store: get run clone status: %w", err)
	}
	st.Repos = repos
	return st, nil
}

// ListCloneStatusRuns reads the runs whose launcher may still say something about its clones: RUNNING with a
// sandbox, neither lost nor paused, and no final clone status yet (a cloning row keeps the run listed). It is
// ListServiceRuns' filter and shape, with no LIMIT: the api pass picks fairly from the list and its budget bounds
// the reads.
func (s PG) ListCloneStatusRuns(ctx context.Context) ([]types.AgentRun, error) {
	q := `SELECT ` + runCols + ` FROM agent_runs r
		WHERE r.state = $1 AND r.sandbox_ref <> '' AND r.lost_at IS NULL AND r.paused_at IS NULL
		  AND NOT EXISTS (SELECT 1 FROM run_clone_status c WHERE c.run_id = r.id AND c.state <> 'cloning')
		ORDER BY r.created_at, r.id`
	return collect(ctx, s.Pool, "list", "clone status runs", q, []any{string(types.RunRunning)}, scanRun)
}
