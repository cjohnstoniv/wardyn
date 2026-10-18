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

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A run_queue row exists exactly while its run is QUEUED (migration 0162): CreateQueuedRun inserts it
// with the run, and every exit from QUEUED goes through LeaveRunQueue, which changes the state and
// deletes the row in one statement. The per-owner count, the position and the promoter's scan read
// this table alone.

// ErrRunQueueFull is CreateQueuedRun's refusal: the owner already has the most runs waiting for the pool.
var ErrRunQueueFull = errors.New("store: run queue is full for this owner")

// errQueuedLeave stops a state CAS that is not LeaveRunQueue from moving a run out of QUEUED, which
// would leave its run_queue row behind.
var errQueuedLeave = errors.New("store: a QUEUED run leaves its state through LeaveRunQueue")

// RunQueueEntry is one waiting run's queue row. PolicyRef, Authority and Launch are JSON documents the
// enqueue path owns; Authority and Launch are personal data and go with the row.
type RunQueueEntry struct {
	RunID      uuid.UUID
	PoolID     uuid.UUID
	Owner      string
	QueuedAt   time.Time
	DeadlineAt time.Time
	PolicyRef  json.RawMessage
	Authority  json.RawMessage
	Launch     json.RawMessage
	// Components and Harnesses are the run's authorised snapshots, which CreateQueuedRun writes in
	// the queue row's own transaction, so no promotable row exists without them. Write-only: a
	// queue row read back never carries them.
	Components []types.RunComponent
	Harnesses  []types.RunHarness
}

const runQueueCols = `run_id, pool_id, owner, queued_at, deadline_at, policy_ref, authority, launch`

func scanRunQueueEntry(row pgx.Row) (RunQueueEntry, error) {
	var e RunQueueEntry
	if err := row.Scan(&e.RunID, &e.PoolID, &e.Owner, &e.QueuedAt, &e.DeadlineAt, &e.PolicyRef, &e.Authority, &e.Launch); err != nil {
		return RunQueueEntry{}, fmt.Errorf("store: scan run queue entry: %w", err)
	}
	return e, nil
}

// jsonOrEmpty is a JSONB column's value: the document, or {} for none.
func jsonOrEmpty(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	return raw
}

// CreateQueuedRun inserts r (state QUEUED, bound to a pool), its launch intent when it carries one,
// its queue row and e's component and harness snapshots in one transaction that holds the pool row FOR NO KEY UPDATE, the lock promotion
// takes first, so the owner's waiting count and the insert cannot interleave with a promotion.
// maxPerOwner <= 0 is unlimited; otherwise an owner with that many runs already waiting for the pool
// gets ErrRunQueueFull and nothing is written. The queue row's pool and owner are the run's own.
func (s PG) CreateQueuedRun(ctx context.Context, r types.AgentRun, e RunQueueEntry, maxPerOwner int) (types.AgentRun, error) {
	if r.State != types.RunQueued || r.RunnerPoolID == nil {
		return types.AgentRun{}, errors.New("store: a queued run is QUEUED and bound to a runner pool")
	}
	args, err := s.createRunArgs(r)
	if err != nil {
		return types.AgentRun{}, err
	}
	g, guarded := identityGuardFrom(ctx)
	var since int64
	if guarded {
		if since, err = ReadRevocationSeq(ctx, s.Pool); err != nil {
			return types.AgentRun{}, err
		}
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return types.AgentRun{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // a no-op after Commit
	if guarded {
		if err := checkIdentityGuard(ctx, tx, g, since); err != nil {
			return types.AgentRun{}, err
		}
	}
	keep, err := lockLaunchIntent(ctx, tx, r)
	if err != nil {
		return types.AgentRun{}, err
	}
	var locked uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM runner_pools WHERE id = $1 FOR NO KEY UPDATE`, *r.RunnerPoolID).Scan(&locked); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return types.AgentRun{}, ErrRunnerPoolStale
		}
		return types.AgentRun{}, fmt.Errorf("store: lock runner pool: %w", err)
	}
	if maxPerOwner > 0 {
		var waiting int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM run_queue WHERE pool_id = $1 AND owner = $2`,
			*r.RunnerPoolID, r.CreatedBy).Scan(&waiting); err != nil {
			return types.AgentRun{}, fmt.Errorf("store: count waiting runs: %w", err)
		}
		if waiting >= maxPerOwner {
			return types.AgentRun{}, ErrRunQueueFull
		}
	}
	anchor, err := db.CaptureAppClock(ctx, tx, s.now)
	if err != nil {
		return types.AgentRun{}, err
	}
	if r.UpdatedAt.IsZero() {
		args[2] = anchor.DatabaseAt
	} else {
		args[2] = anchor.Translate(r.UpdatedAt)
	}
	created, err := scanRun(tx.QueryRow(ctx, createRunSQL, args...))
	if err != nil {
		return types.AgentRun{}, runInsertErr(err)
	}
	if keep {
		if created, err = insertLaunchIntent(ctx, tx, created, r.LaunchIntent); err != nil {
			return types.AgentRun{}, err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO run_queue (`+runQueueCols+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		created.ID, *r.RunnerPoolID, r.CreatedBy, e.QueuedAt, e.DeadlineAt,
		jsonOrEmpty(e.PolicyRef), jsonOrEmpty(e.Authority), jsonOrEmpty(e.Launch)); err != nil {
		return types.AgentRun{}, fmt.Errorf("store: insert run queue row: %w", err)
	}
	if len(e.Components) > 0 {
		if err := insertRunComponents(ctx, tx, created.ID, e.Components); err != nil {
			return types.AgentRun{}, fmt.Errorf("store: put run components: %w", err)
		}
	}
	if len(e.Harnesses) > 0 {
		if err := insertRunHarnesses(ctx, tx, created.ID, e.Harnesses); err != nil {
			return types.AgentRun{}, fmt.Errorf("store: put run harnesses: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return types.AgentRun{}, err
	}
	return created, nil
}

// LeaveRunQueue is the one way a run leaves QUEUED: one statement moves it to `to` (only while it is
// still QUEUED) and deletes its run_queue row, so no exit can keep the row or drop it without the
// state. admittedAt is promotion's stamp (QUEUED to PENDING) and nil for every other exit. false with a
// nil error means another exit won first. UpdateRunStateIf from QUEUED is this call with no stamp.
func (s PG) LeaveRunQueue(ctx context.Context, id uuid.UUID, to types.RunState, admittedAt *time.Time) (bool, error) {
	var moved int
	if err := s.Pool.QueryRow(ctx, `
		WITH moved AS (
			UPDATE agent_runs SET state=$2, updated_at=now(), ended_at=CASE WHEN $3 THEN now() ELSE ended_at END,
				admitted_at=COALESCE($4::timestamptz, admitted_at)
			WHERE id=$1 AND state=$5 RETURNING id),
		gone AS (DELETE FROM run_queue WHERE run_id IN (SELECT id FROM moved) RETURNING run_id)
		SELECT count(*) FROM moved`,
		id, string(to), to.IsTerminal(), admittedAt, string(types.RunQueued)).Scan(&moved); err != nil {
		return false, fmt.Errorf("store: leave run queue: %w", err)
	}
	return moved > 0, nil
}

// RunQueuePosition is how many runs wait ahead of run id in its pool (a count, never whose), or
// ErrNotFound when the run is not waiting.
func (s PG) RunQueuePosition(ctx context.Context, id uuid.UUID) (int, error) {
	var ahead int
	err := s.Pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM run_queue p WHERE p.pool_id = q.pool_id AND (p.queued_at, p.run_id) < (q.queued_at, q.run_id))
		FROM run_queue q WHERE q.run_id = $1`, id).Scan(&ahead)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("store: run queue position: %w", err)
	}
	return ahead, nil
}

// countPoolSlotHolders counts the runs holding one of poolID's slots: a slot-holding state whose
// agent still runs, never a QUEUED run. Promotion and enqueue read it under the pool row's lock;
// a count taken outside it lets two launches both pass.
func countPoolSlotHolders(ctx context.Context, q Querier, poolID uuid.UUID) (int, error) {
	var n int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM agent_runs WHERE runner_pool_id = $1 AND state = ANY($2) AND `+HoldsSandboxSQL,
		poolID, slotHoldingStateNames()).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count pool slot holders: %w", err)
	}
	return n, nil
}

// CountRunnerPoolSlotHolders is countPoolSlotHolders without the lock, a read for a display; the
// authority is the in-transaction count.
func (s PG) CountRunnerPoolSlotHolders(ctx context.Context, poolID uuid.UUID) (int, error) {
	return countPoolSlotHolders(ctx, s.Pool, poolID)
}
