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

	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Promotion and the other queue exits that need more than LeaveRunQueue: one transaction per pool
// that moves waiting runs to PENDING under the pool row's lock, the timeout scan, the stale-lease
// scan and the start's own column write. The state sets, the queue row and the single-statement exit
// are store_run_queue.go's.

// PromotePass is one promotion pass over one pool.
type PromotePass struct {
	PoolID uuid.UUID
	// DeploymentCap is WARDYN_MAX_CONCURRENT_RUNS, 0 for none. The pass counts the deployment's
	// slot-holders under the same advisory lock a create takes, so it never exceeds the cap.
	DeploymentCap int
	// ScanLimit bounds how many waiting runs the pass looks at, so one pool with a long queue of
	// unplaceable heads cannot hold its row lock for an unbounded time. 0 is every row.
	ScanLimit int
	// WatcherOwner is the lease holder written on each promoted run: the promoting process.
	WatcherOwner string
}

// QueueVerdict is the pass's answer for one waiting run, decided without any lock beyond the pool's.
type QueueVerdict struct {
	// Admit promotes the run. Wait is the reason it keeps its place otherwise, "" for none to say.
	Admit bool
	Wait  string
}

// QueueAdmit judges one waiting run while the pass holds the pool row. It runs inside the transaction,
// so it must be quick and must not write to the queue.
type QueueAdmit func(ctx context.Context, pool types.RunnerPool, e RunQueueEntry) (QueueVerdict, error)

// PromotedRun is a run the pass moved to PENDING, with the queue row it carried.
type PromotedRun struct {
	Run   types.AgentRun
	Entry RunQueueEntry
}

// QueueHeld is a waiting run the pass left in its place and why.
type QueueHeld struct {
	RunID uuid.UUID
	Wait  string
}

// PromoteRunQueue moves waiting runs of one pool to PENDING, oldest first, while the pool has a free
// slot, in one transaction. Lock order is always the pool row (FOR NO KEY UPDATE, the lock an enqueue
// takes) and then the deployment cap's advisory lock, so no create, revive or pass can deadlock with
// another. A pool that is gone or not active promotes nothing. A run the verdict will not admit keeps
// its place and the pass tries the next, so one run that cannot be placed never blocks another's.
func (s PG) PromoteRunQueue(ctx context.Context, pass PromotePass, admit QueueAdmit) ([]PromotedRun, []QueueHeld, error) {
	var promoted []PromotedRun
	var held []QueueHeld
	err := s.inTx(ctx, func(q Querier) error {
		promoted, held = nil, nil
		pool, err := scanRunnerPool(q.QueryRow(ctx, `SELECT `+runnerPoolCols+` FROM runner_pools WHERE id = $1 FOR NO KEY UPDATE`, pass.PoolID))
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if pool.State != types.RunnerPoolActive {
			return nil
		}
		deployment := 0
		if pass.DeploymentCap > 0 {
			if deployment, err = lockAndCountActiveRuns(ctx, q, uuid.Nil); err != nil {
				return err
			}
		}
		running, err := countPoolSlotHolders(ctx, q, pool.ID)
		if err != nil {
			return err
		}
		entries, err := listPromotable(ctx, q, pool.ID, pass.ScanLimit)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if runnerpool.AtCapacity(pool, running) != nil || (pass.DeploymentCap > 0 && deployment >= pass.DeploymentCap) {
				break
			}
			v, err := admit(ctx, pool, e)
			if err != nil {
				return err
			}
			if !v.Admit {
				held = append(held, QueueHeld{RunID: e.RunID, Wait: v.Wait})
				continue
			}
			run, ok, err := promoteQueuedRun(ctx, q, e.RunID, pass.WatcherOwner)
			if err != nil {
				return err
			}
			if !ok {
				// The run left QUEUED some other way and left its row behind; a row exists only while its
				// run is QUEUED, so put that right instead of judging it again every pass.
				if _, err := q.Exec(ctx, `DELETE FROM run_queue WHERE run_id = $1`, e.RunID); err != nil {
					return fmt.Errorf("store: drop orphan queue row: %w", err)
				}
				continue
			}
			promoted = append(promoted, PromotedRun{Run: run, Entry: e})
			running++
			deployment++
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return promoted, held, nil
}

// listPromotable is the pass's scan: the pool's queue rows oldest first (queued_at, then run_id), each
// locked FOR UPDATE SKIP LOCKED with its run, the run row before its queue row. Whether the run is
// still QUEUED is the promotion CAS's to say, not the scan's. That is the order every exit from
// the queue takes (LeaveRunQueue, ExitQueuedRun and the pool delete update the run, then delete its
// queue row), so a kill that lands while the pass judges a run waits for the pass instead of
// deadlocking with it, and a kill already holding a run is skipped. q must be a transaction that holds
// the pool row's lock.
func listPromotable(ctx context.Context, q Querier, poolID uuid.UUID, limit int) ([]RunQueueEntry, error) {
	if limit <= 0 {
		limit = 1 << 30
	}
	rows, err := q.Query(ctx, `
		SELECT q.run_id, q.pool_id, q.owner, q.queued_at, q.deadline_at, q.policy_ref, q.authority, q.launch
		FROM agent_runs r JOIN run_queue q ON q.run_id = r.id
		WHERE q.pool_id = $1
		ORDER BY q.queued_at, q.run_id LIMIT $2 FOR UPDATE OF r, q SKIP LOCKED`, poolID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list promotable runs: %w", err)
	}
	defer rows.Close()
	var out []RunQueueEntry
	for rows.Next() {
		e, err := scanRunQueueEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// promoteQueuedRun is the promotion CAS: QUEUED to PENDING, admitted_at and the watcher lease stamped,
// and the run_queue row deleted in the same statement. ok is false when another exit won first.
func promoteQueuedRun(ctx context.Context, q Querier, id uuid.UUID, owner string) (types.AgentRun, bool, error) {
	run, err := scanRun(q.QueryRow(ctx, `
		WITH moved AS (
			UPDATE agent_runs SET state=$2, updated_at=now(), admitted_at=now(), watcher_owner=$3, watcher_heartbeat=now()
			WHERE id=$1 AND state=$4 RETURNING `+runCols+`),
		gone AS (DELETE FROM run_queue WHERE run_id IN (SELECT id FROM moved) RETURNING run_id)
		SELECT `+runCols+` FROM moved`,
		id, string(types.RunPending), owner, string(types.RunQueued)))
	if errors.Is(err, ErrNotFound) {
		return types.AgentRun{}, false, nil
	}
	if err != nil {
		return types.AgentRun{}, false, fmt.Errorf("store: promote queued run: %w", err)
	}
	return run, true, nil
}

// ExpiredQueueEntries are the waiting runs whose deadline is at or before now, oldest deadline first.
func (s PG) ExpiredQueueEntries(ctx context.Context, now time.Time, limit int) ([]RunQueueEntry, error) {
	if limit <= 0 {
		limit = 1 << 30
	}
	return collect(ctx, s.Pool, "list", "expired run queue entries",
		`SELECT `+runQueueCols+` FROM run_queue WHERE deadline_at <= $1 ORDER BY deadline_at, run_id LIMIT $2`,
		[]any{now, limit}, func(row pgx.Row) (RunQueueEntry, error) { return scanRunQueueEntry(row) })
}

// QueuePoolIDs are the pools that have waiting runs: the sweep's and a kick's work list.
func (s PG) QueuePoolIDs(ctx context.Context) ([]uuid.UUID, error) {
	return collect(ctx, s.Pool, "list", "pools with waiting runs",
		`SELECT DISTINCT pool_id FROM run_queue ORDER BY pool_id`, nil,
		func(row pgx.Row) (uuid.UUID, error) {
			var id uuid.UUID
			err := row.Scan(&id)
			return id, err
		})
}

// QueueOwnerEntries are the waiting runs of one owner, the leaver purge's and an erasure's work list.
func (s PG) QueueOwnerEntries(ctx context.Context, owner string) ([]RunQueueEntry, error) {
	return collect(ctx, s.Pool, "list", "run queue entries of an owner",
		`SELECT `+runQueueCols+` FROM run_queue WHERE owner = $1 ORDER BY queued_at, run_id`,
		[]any{owner}, func(row pgx.Row) (RunQueueEntry, error) { return scanRunQueueEntry(row) })
}

// GetRunQueueEntry is one waiting run's queue row, or ErrNotFound once the run has left the queue.
func (s PG) GetRunQueueEntry(ctx context.Context, id uuid.UUID) (RunQueueEntry, error) {
	e, err := scanRunQueueEntry(s.Pool.QueryRow(ctx, `SELECT `+runQueueCols+` FROM run_queue WHERE run_id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return RunQueueEntry{}, ErrNotFound
	}
	return e, err
}

// QueueDepth is the number of waiting runs per pool that has any.
func (s PG) QueueDepth(ctx context.Context) (map[uuid.UUID]int, error) {
	return s.poolCounts(ctx, `SELECT pool_id, count(*) FROM run_queue GROUP BY pool_id`)
}

// PoolSlotsUsed is the number of slot-holding runs per pool that has any, the count promotion judges
// a pool's cap by.
func (s PG) PoolSlotsUsed(ctx context.Context) (map[uuid.UUID]int, error) {
	return s.poolCounts(ctx, `SELECT runner_pool_id, count(*) FROM agent_runs
		WHERE runner_pool_id IS NOT NULL AND state = ANY($1) AND `+HoldsSandboxSQL+` GROUP BY runner_pool_id`, slotHoldingStateNames())
}

func (s PG) poolCounts(ctx context.Context, q string, args ...any) (map[uuid.UUID]int, error) {
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: count by pool: %w", err)
	}
	defer rows.Close()
	out := map[uuid.UUID]int{}
	for rows.Next() {
		var id uuid.UUID
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("store: count by pool: %w", err)
		}
		out[id] = n
	}
	return out, rows.Err()
}

// StalePromotedRuns are the runs that were promoted out of the queue, are still PENDING with no
// sandbox, and whose start has not touched its lease for staleAfter: the process that promoted them
// is gone. Only promotion writes admitted_at, so a plain run in its image build, whose heartbeat is
// its insert time, is never listed.
func (s PG) StalePromotedRuns(ctx context.Context, staleAfter time.Duration) ([]types.AgentRun, error) {
	if staleAfter < 0 {
		staleAfter = 0
	}
	return collect(ctx, s.Pool, "list", "stale promoted runs",
		`SELECT `+runCols+` FROM agent_runs
		 WHERE state = $1 AND admitted_at IS NOT NULL AND sandbox_ref = '' AND lost_at IS NULL
		   AND watcher_heartbeat < now() - $2::interval
		 ORDER BY admitted_at`,
		[]any{string(types.RunPending), staleAfter.String()}, scanRun)
}

// QueueStart is the columns the start of a promoted run writes: everything a QUEUED run left empty
// (the identity, the end and its cap, the governance capture) and the placement the start settled.
type QueueStart struct {
	SPIFFEID            string
	ConfinementClass    types.ConfinementClass
	ModelProviderID     string
	AutonomyLevel       types.AutonomyLevel
	EndsAt              *time.Time
	WaitBudgetSec       int
	RunLimits           types.RunLimits
	GovernanceProfileID *uuid.UUID
	LifetimeCapAt       *time.Time
	LifetimeCapSource   types.LimitSource
	Placement           types.Placement
	PlacementFilled     bool
	RunnerID            *uuid.UUID
	EvidenceSource      types.RunEvidenceSource
	RunnerPoolExecutor  string
}

// StartQueuedRun writes the start's columns on a run that was promoted (PENDING, admitted_at set) and
// has not been dispatched, and returns the row. ok is false when the run is no longer in that state,
// so a kill that won leaves nothing written.
func (s PG) StartQueuedRun(ctx context.Context, id uuid.UUID, st QueueStart) (types.AgentRun, bool, error) {
	limits, err := json.Marshal(st.RunLimits)
	if err != nil {
		return types.AgentRun{}, false, fmt.Errorf("store: marshal run limits: %w", err)
	}
	run, err := scanRun(s.Pool.QueryRow(ctx, `
		UPDATE agent_runs SET spiffe_id=$2, confinement_class=$3, model_provider_id=$4, autonomy_level=$5, ends_at=$6,
			wait_budget_sec=$7, run_limits=$8, governance_profile_id=$9, lifetime_cap_at=$10, lifetime_cap_source=$11,
			placement=$12, placement_filled=$13, runner_id=$14, evidence_source=$15, runner_pool_executor=$16, updated_at=now()
		WHERE id=$1 AND state=$17 AND admitted_at IS NOT NULL AND sandbox_ref = ''
		RETURNING `+runCols,
		id, st.SPIFFEID, string(st.ConfinementClass), st.ModelProviderID, string(st.AutonomyLevel), st.EndsAt,
		st.WaitBudgetSec, limits, st.GovernanceProfileID, st.LifetimeCapAt, string(st.LifetimeCapSource),
		string(st.Placement), st.PlacementFilled, st.RunnerID, string(st.EvidenceSource), st.RunnerPoolExecutor,
		string(types.RunPending)))
	if errors.Is(err, ErrNotFound) {
		return types.AgentRun{}, false, nil
	}
	if err != nil {
		return types.AgentRun{}, false, fmt.Errorf("store: start queued run: %w", err)
	}
	return run, true, nil
}

// SetPromotedStatusDetail writes the status_detail of a promoted run that has not been dispatched,
// so the reason a start refused it lands before the FAILED transition and a reader never sees a
// failed queue run without its token. It is a no-op once the run has left PENDING.
func (s PG) SetPromotedStatusDetail(ctx context.Context, id uuid.UUID, detail string) error {
	if _, err := s.Pool.Exec(ctx, `UPDATE agent_runs SET status_detail=$2 WHERE id=$1 AND state=$3 AND admitted_at IS NOT NULL`,
		id, detail, string(types.RunPending)); err != nil {
		return fmt.Errorf("store: set promoted status detail: %w", err)
	}
	return nil
}

// SetQueueWait records why a waiting run keeps its place: its "queue: <token>" status_detail, written
// only when it differs from what the run carries and only while the run is still QUEUED.
func (s PG) SetQueueWait(ctx context.Context, id uuid.UUID, detail string) error {
	if _, err := s.Pool.Exec(ctx, `UPDATE agent_runs SET status_detail=$2 WHERE id=$1 AND state=$3 AND status_detail <> $2`,
		id, detail, string(types.RunQueued)); err != nil {
		return fmt.Errorf("store: set queue wait: %w", err)
	}
	return nil
}

// ExitQueuedRun moves a QUEUED run to a terminal state through LeaveRunQueue and, in the same
// statement, writes the reason the queue ended it: status_detail ("queue: <token>") and the sentence
// for failure_hint. false means another exit won first.
func (s PG) ExitQueuedRun(ctx context.Context, id uuid.UUID, to types.RunState, detail, hint string) (bool, error) {
	var moved int
	if err := s.Pool.QueryRow(ctx, `
		WITH moved AS (
			UPDATE agent_runs SET state=$2, updated_at=now(), ended_at=now(), status_detail=$3, failure_hint=$4
			WHERE id=$1 AND state=$5 RETURNING id),
		gone AS (DELETE FROM run_queue WHERE run_id IN (SELECT id FROM moved) RETURNING run_id)
		SELECT count(*) FROM moved`,
		id, string(to), detail, hint, string(types.RunQueued)).Scan(&moved); err != nil {
		return false, fmt.Errorf("store: exit queued run: %w", err)
	}
	return moved > 0, nil
}

// cancelPoolQueue ends every run waiting for poolID as KILLED with the removal reason, deleting the
// queue rows with them, inside the caller's transaction, and returns the run ids. The pool delete runs
// it under the pool row's lock, so no enqueue can add a run behind it.
func cancelPoolQueue(ctx context.Context, q Querier, poolID uuid.UUID, detail, hint string) ([]uuid.UUID, error) {
	rows, err := q.Query(ctx, `
		WITH moved AS (
			UPDATE agent_runs SET state=$2, updated_at=now(), ended_at=now(), status_detail=$3, failure_hint=$4
			WHERE state=$5 AND id IN (SELECT run_id FROM run_queue WHERE pool_id=$1) RETURNING id),
		gone AS (DELETE FROM run_queue WHERE pool_id=$1 RETURNING run_id)
		SELECT id FROM moved`,
		poolID, string(types.RunKilled), detail, hint, string(types.RunQueued))
	if err != nil {
		return nil, fmt.Errorf("store: cancel pool queue: %w", err)
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: cancel pool queue: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
