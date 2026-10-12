// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A runner's "Runs at once" (migration 0172): placement never puts more than a runner's max_runs
// sandbox-holding runs on it. Every writer that makes a run hold a sandbox on a runner (a create, a
// promotion, the start of a promoted run, a revive that starts an agent) takes the runner's row
// FOR NO KEY UPDATE, counts its slot-holders and writes in the same transaction, after the pool row and
// the deployment cap's advisory lock: the one order every writer uses.

// ErrRunnerAtCapacity is the refusal when a runner already holds max_runs sandbox-holding runs.
var ErrRunnerAtCapacity = errors.New("store: runner is running its limit of runs at once")

// RunnerAtCapacityError names the runner and its limit for the sentence; it is ErrRunnerAtCapacity.
type RunnerAtCapacityError struct {
	Runner string
	Max    int
}

func (e *RunnerAtCapacityError) Error() string { return ErrRunnerAtCapacity.Error() }
func (e *RunnerAtCapacityError) Unwrap() error { return ErrRunnerAtCapacity }

// runnerSlotHoldersSQL counts the runs holding a sandbox on each runner: a slot-holding state whose
// agent still runs (HoldsSandboxSQL), never a QUEUED run. $1 is the states, $2 the runner ids, $3 a
// run never counted (uuid.Nil for none).
const runnerSlotHoldersSQL = `SELECT runner_id, count(*) FROM agent_runs
	WHERE runner_id = ANY($2) AND state = ANY($1) AND id <> $3 AND ` + HoldsSandboxSQL + ` GROUP BY runner_id`

func countRunnerSlotHolders(ctx context.Context, q Querier, ids []uuid.UUID, except uuid.UUID) (map[uuid.UUID]int, error) {
	out := map[uuid.UUID]int{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, runnerSlotHoldersSQL, slotHoldingStateNames(), ids, except)
	if err != nil {
		return nil, fmt.Errorf("store: count runner slot holders: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("store: count runner slot holders: %w", err)
		}
		out[id] = n
	}
	return out, rows.Err()
}

// RunnerSlotHolders is the number of sandbox-holding runs on each of ids, without a lock: a read for
// a display and for placement's choice, never the authority. A runner with none is absent.
func (s PG) RunnerSlotHolders(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]int, error) {
	return countRunnerSlotHolders(ctx, s.Pool, ids, uuid.Nil)
}

// runnerRoom is one locked runner's limit and its slot-holders.
type runnerRoom struct {
	name      string
	max, used int
}

func (r runnerRoom) free() bool { return r.used < r.max }

// lockRunnerRooms locks the runners of ids (FOR NO KEY UPDATE, in id order, so two writers locking
// several never deadlock) and counts each one's slot-holders other than except. q must be a pgx.Tx.
// A runner that no longer exists is absent.
func lockRunnerRooms(ctx context.Context, q Querier, ids []uuid.UUID, except uuid.UUID) (map[uuid.UUID]runnerRoom, error) {
	ids = slices.Clone(ids)
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	ids = slices.Compact(ids)
	type locked struct {
		id   uuid.UUID
		name string
		max  int
	}
	rows, err := collect(ctx, q, "lock", "runners", `SELECT id, name, max_runs FROM runners WHERE id = ANY($1) ORDER BY id FOR NO KEY UPDATE`,
		[]any{ids}, func(row pgx.Row) (locked, error) {
			var l locked
			err := row.Scan(&l.id, &l.name, &l.max)
			return l, err
		})
	if err != nil {
		return nil, err
	}
	used, err := countRunnerSlotHolders(ctx, q, ids, except)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]runnerRoom, len(rows))
	for _, l := range rows {
		out[l.id] = runnerRoom{name: l.name, max: l.max, used: used[l.id]}
	}
	return out, nil
}

// admitOnRunner locks one runner and refuses with a RunnerAtCapacityError when its slot-holders other
// than except already reach its max_runs. q must be a pgx.Tx.
func admitOnRunner(ctx context.Context, q Querier, runnerID, except uuid.UUID) error {
	rooms, err := lockRunnerRooms(ctx, q, []uuid.UUID{runnerID}, except)
	if err != nil {
		return err
	}
	if room, ok := rooms[runnerID]; ok && !room.free() {
		return &RunnerAtCapacityError{Runner: room.name, Max: room.max}
	}
	return nil
}

// holdsRunnerSlot reports whether inserting r puts a sandbox-holding run on a registered runner.
func holdsRunnerSlot(r types.AgentRun) bool {
	return r.RunnerID != nil && slices.Contains(types.SlotHoldingRunStates, r.State)
}

// RunnerPoolsOf — see RunnerInventoryStore. Only a pool whose owner is the runner's owner is named, so a
// runner never lists a pool of anyone else's, in one read for the whole page.
func (s PG) RunnerPoolsOf(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID][]types.RunnerPoolRef, error) {
	out := map[uuid.UUID][]types.RunnerPoolRef{}
	if len(ids) == 0 {
		return out, nil
	}
	type membership struct {
		runner uuid.UUID
		pool   types.RunnerPoolRef
	}
	rows, err := collect(ctx, s.Pool, "list", "runner pool memberships", `
		SELECT m.runner_id, p.id, p.name FROM runner_pool_members m
		JOIN runner_pools p ON p.id = m.pool_id JOIN runners r ON r.id = m.runner_id
		WHERE m.runner_id = ANY($1) AND p.state <> 'deleted' AND p.owner = r.owner
		ORDER BY lower(p.name), p.id`, []any{ids}, func(row pgx.Row) (membership, error) {
		var m membership
		err := row.Scan(&m.runner, &m.pool.ID, &m.pool.Name)
		return m, err
	})
	if err != nil {
		return nil, err
	}
	for _, m := range rows {
		out[m.runner] = append(out[m.runner], m.pool)
	}
	return out, nil
}

// ListRunnerPoolRunners is a Self-Hosted pool's member runners, newest first: only runners its owner
// holds, so a pool never shows anyone else's runner. A Remote Provided pool has none.
func (s PG) ListRunnerPoolRunners(ctx context.Context, poolID uuid.UUID) ([]types.Runner, error) {
	return collect(ctx, s.Pool, "list", "runner pool runners", `SELECT `+prefixedRunnerCols+`
		FROM runner_pool_members m JOIN runner_pools p ON p.id = m.pool_id JOIN runners r ON r.id = m.runner_id
		WHERE m.pool_id = $1 AND r.owner = p.owner ORDER BY r.created_at DESC, r.id`, []any{poolID}, scanRunner)
}

// SetRunnerMachineName records the host name a runner reported on connect, the latest kept; a revoked
// runner keeps the one it had. The caller has checked it with types.ValidMachineName.
func (s PG) SetRunnerMachineName(ctx context.Context, id uuid.UUID, name string) error {
	if _, err := s.Pool.Exec(ctx, `UPDATE runners SET machine_name = $2 WHERE id = $1 AND state <> 'revoked' AND machine_name <> $2`, id, name); err != nil {
		return fmt.Errorf("store: set runner machine name: %w", err)
	}
	return nil
}

// UpdateRunnerMaxRuns sets the max_runs of a runner its owner holds, unclaimed or claimed, and returns
// the value it had and the row as it is now. Another person's runner, a revoked one and an absent id
// are ErrNotFound alike. Lowering it ends no run: the runs over the new limit keep running, and no new
// run is placed there until the runner is under it.
func (s PG) UpdateRunnerMaxRuns(ctx context.Context, id uuid.UUID, owner string, maxRuns int) (before int, after types.Runner, err error) {
	err = s.inTx(ctx, func(q Querier) error {
		if err := q.QueryRow(ctx, `SELECT max_runs FROM runners WHERE id = $1 AND owner = $2 AND state <> 'revoked' FOR NO KEY UPDATE`,
			id, owner).Scan(&before); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("store: lock runner: %w", err)
		}
		var err error
		after, err = scanRunner(q.QueryRow(ctx, `UPDATE runners SET max_runs = $2 WHERE id = $1 RETURNING `+runnerCols, id, maxRuns))
		return err
	})
	return before, after, err
}
