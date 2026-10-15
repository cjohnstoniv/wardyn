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

	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A runner's limits (migrations 0172, 0174), both kinds: placement never puts a run on a runner past
// its Runs at once (max_runs) or past one of its totals, and never a run larger than its per-run limit.
// Every writer that makes a run hold a sandbox on a runner (a create, a promotion, the start of a
// promoted run, a revive that starts an agent) takes the runner's row FOR NO KEY UPDATE, sums what its
// slot-holders reserve with runnerUsageSQL and writes in the same transaction. Every such writer takes
// its locks in one order, skipping those it does not need: the run's pool row (CreateRunUnderCap,
// lockPoolForRevive, promoteAdmitted), then the deployment cap's advisory lock (lockAndCountActiveRuns),
// then self-hosted runner rows in id order, then remote-hosted runner rows in id order
// (lockRunnerRooms). A promoted run's start takes runner rows only.
//
// A runner is named by its key: a self-hosted runner's id as text, or a remote-hosted runner's id,
// which never reads as a UUID (0173's check), so one key space holds both kinds.

// ErrRunnerAtCapacity is the refusal when a runner has no room for the run: it already holds its Runs
// at once, a total would be passed, the run is past its per-run limit, or it declares no limits.
var ErrRunnerAtCapacity = errors.New("store: runner is running its limit of runs at once")

// RunnerAtCapacityError names the runner and its Runs at once for the sentence; it is ErrRunnerAtCapacity.
// NeverFits says the run could not go there however empty the runner is: it is past the per-run limit,
// or the runner declares no limits.
type RunnerAtCapacityError struct {
	Runner    string
	Max       int
	NeverFits bool
}

func (e *RunnerAtCapacityError) Error() string { return ErrRunnerAtCapacity.Error() }
func (e *RunnerAtCapacityError) Unwrap() error { return ErrRunnerAtCapacity }

// runnerLimitCols are a runner's six limit columns (0174), named alike on runners and remote_runners.
const runnerLimitCols = `total_cpu_millis, total_memory_mib, total_disk_mib, run_cpu_millis, run_memory_mib, run_disk_mib`

// limitCols scans runnerLimitCols, any of which may be NULL.
type limitCols [6]*int64

func (l *limitCols) targets() []any {
	out := make([]any, len(l))
	for i := range l {
		out[i] = &l[i]
	}
	return out
}

// limits is the runner's limit set with Runs at once runs, nil when any of it is unset.
func (l limitCols) limits(runs *int) *types.RunnerLimits {
	if runs == nil || slices.Contains(l[:], nil) {
		return nil
	}
	return &types.RunnerLimits{RunsAtOnce: *runs,
		Total:  types.RunnerAmounts{CPUMillis: *l[0], MemoryMiB: *l[1], DiskMiB: *l[2]},
		PerRun: types.RunnerAmounts{CPUMillis: *l[3], MemoryMiB: *l[4], DiskMiB: *l[5]}}
}

// limitArgs are the six column values of l, all NULL for nil.
func limitArgs(l *types.RunnerLimits) []any {
	if l == nil {
		return []any{nil, nil, nil, nil, nil, nil}
	}
	return []any{l.Total.CPUMillis, l.Total.MemoryMiB, l.Total.DiskMiB, l.PerRun.CPUMillis, l.PerRun.MemoryMiB, l.PerRun.DiskMiB}
}

// runnerUsageSQL is a runner's In use, the one predicate both admission (under the runner row's lock)
// and every read (without one) run: per runner key, the runs holding a sandbox there in a slot-holding
// state whose agent still runs (HoldsSandboxSQL), never a QUEUED run, and the sum of what they reserve.
// $1 is the states, $2 the self-hosted runner ids, $3 the remote-hosted runner ids, $4 a run never
// counted (uuid.Nil for none).
const runnerUsageSQL = `SELECT COALESCE(runner_id::text, runner_pool_executor), count(*)::int,
	COALESCE(sum(reserved_cpu_millis), 0)::bigint, COALESCE(sum(reserved_memory_mib), 0)::bigint, COALESCE(sum(reserved_disk_mib), 0)::bigint
	FROM agent_runs WHERE (runner_id = ANY($2) OR (runner_id IS NULL AND runner_pool_executor = ANY($3)))
	AND state = ANY($1) AND id <> $4 AND ` + HoldsSandboxSQL + ` GROUP BY 1`

// splitRunnerKeys sorts keys into self-hosted runner ids and remote-hosted runner ids.
func splitRunnerKeys(keys []string) (runners []uuid.UUID, remotes []string) {
	for _, k := range keys {
		if id, err := uuid.Parse(k); err == nil {
			runners = append(runners, id)
		} else if k != "" {
			remotes = append(remotes, k)
		}
	}
	return runners, remotes
}

func runnerUsage(ctx context.Context, q Querier, runners []uuid.UUID, remotes []string, except uuid.UUID) (map[string]types.RunnerTally, error) {
	out := map[string]types.RunnerTally{}
	if len(runners) == 0 && len(remotes) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, runnerUsageSQL, slotHoldingStateNames(), runners, remotes, except)
	if err != nil {
		return nil, fmt.Errorf("store: runner usage: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var t types.RunnerTally
		if err := rows.Scan(&key, &t.Runs, &t.CPUMillis, &t.MemoryMiB, &t.DiskMiB); err != nil {
			return nil, fmt.Errorf("store: runner usage: %w", err)
		}
		out[key] = t
	}
	return out, rows.Err()
}

// RunnerUsage is the In use of each runner named, by key, without a lock: what every read shows and
// placement chooses by, never the authority. except is a run never counted (uuid.Nil for none): a
// promoted run's start does not count the run's own place. A runner with nothing on it is absent.
func (s PG) RunnerUsage(ctx context.Context, runners []uuid.UUID, remotes []string, except uuid.UUID) (map[string]types.RunnerTally, error) {
	return runnerUsage(ctx, s.Pool, runners, remotes, except)
}

// RunnerSlotHolders is the number of sandbox-holding runs on each of ids (RunnerUsage's runs). A runner
// with none is absent.
func (s PG) RunnerSlotHolders(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]int, error) {
	used, err := s.RunnerUsage(ctx, ids, nil, uuid.Nil)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]int, len(used))
	for k, t := range used {
		out[uuid.MustParse(k)] = t.Runs
	}
	return out, nil
}

// runnerRoom is one locked runner's limits and what its runs hold.
type runnerRoom struct {
	name   string
	limits *types.RunnerLimits
	used   types.RunnerTally
}

func (r runnerRoom) fits(need types.RunnerAmounts) bool {
	return runnerpool.Fits(r.limits, r.used, need) == runnerpool.FitOK
}

// take counts one more run reserving need, so a pass that admits several sees each.
func (r *runnerRoom) take(need types.RunnerAmounts) {
	r.used.Runs++
	r.used.CPUMillis += need.CPUMillis
	r.used.MemoryMiB += need.MemoryMiB
	r.used.DiskMiB += need.DiskMiB
}

// runsLeft is how many more runs it takes at once.
func (r runnerRoom) runsLeft() int {
	if r.limits == nil {
		return 0
	}
	return r.limits.RunsAtOnce - r.used.Runs
}

func (r runnerRoom) refusal(need types.RunnerAmounts) error {
	runs := 0
	if r.limits != nil {
		runs = r.limits.RunsAtOnce
	}
	return &RunnerAtCapacityError{Runner: r.name, Max: runs, NeverFits: runnerpool.Fits(r.limits, r.used, need) != runnerpool.FitNoRoom}
}

// lockRunnerRooms locks the runners keys name, self-hosted rows then remote-hosted rows, each FOR NO KEY
// UPDATE in id order (so two writers locking several never deadlock), and sums what each one's
// slot-holders other than except reserve. q must be a pgx.Tx. A runner that no longer exists is absent.
func lockRunnerRooms(ctx context.Context, q Querier, keys []string, except uuid.UUID) (map[string]runnerRoom, error) {
	runners, remotes := splitRunnerKeys(keys)
	slices.SortFunc(runners, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	runners = slices.Compact(runners)
	slices.Sort(remotes)
	remotes = slices.Compact(remotes)
	type locked struct {
		key  string
		room runnerRoom
	}
	scan := func(row pgx.Row) (locked, error) {
		var l locked
		var runs *int
		var lim limitCols
		err := row.Scan(append([]any{&l.key, &l.room.name, &runs}, lim.targets()...)...)
		l.room.limits = lim.limits(runs)
		return l, err
	}
	var rows []locked
	if len(runners) > 0 {
		got, err := collect(ctx, q, "lock", "runners", `SELECT id::text, name, max_runs, `+runnerLimitCols+`
			FROM runners WHERE id = ANY($1) ORDER BY id FOR NO KEY UPDATE`, []any{runners}, scan)
		if err != nil {
			return nil, err
		}
		rows = append(rows, got...)
	}
	if len(remotes) > 0 {
		got, err := collect(ctx, q, "lock", "remote runners", `SELECT id, name, max_runs, `+runnerLimitCols+`
			FROM remote_runners WHERE id = ANY($1) ORDER BY id FOR NO KEY UPDATE`, []any{remotes}, scan)
		if err != nil {
			return nil, err
		}
		rows = append(rows, got...)
	}
	used, err := runnerUsage(ctx, q, runners, remotes, except)
	if err != nil {
		return nil, err
	}
	out := make(map[string]runnerRoom, len(rows))
	for _, l := range rows {
		l.room.used = used[l.key]
		out[l.key] = l.room
	}
	return out, nil
}

// admitOnFirstRunner locks the runners keys name and returns the first, in keys' order, that has room
// for a run reserving need beside its slot-holders other than except; a RunnerAtCapacityError naming
// the first when none has. q must be a pgx.Tx.
func admitOnFirstRunner(ctx context.Context, q Querier, keys []string, need types.RunnerAmounts, except uuid.UUID) (string, error) {
	rooms, err := lockRunnerRooms(ctx, q, keys, except)
	if err != nil {
		return "", err
	}
	for _, k := range keys {
		if room, ok := rooms[k]; ok && room.fits(need) {
			return k, nil
		}
	}
	return "", rooms[keys[0]].refusal(need)
}

// admitOnRunner locks one runner and refuses with a RunnerAtCapacityError when a run reserving need
// does not fit beside its slot-holders other than except. q must be a pgx.Tx. A runner that no longer
// exists is left to the caller's own checks, as before limits.
func admitOnRunner(ctx context.Context, q Querier, key string, need types.RunnerAmounts, except uuid.UUID) error {
	rooms, err := lockRunnerRooms(ctx, q, []string{key}, except)
	if err != nil {
		return err
	}
	if room, ok := rooms[key]; ok && !room.fits(need) {
		return room.refusal(need)
	}
	return nil
}

// runnerKeyOf is the key of the runner r holds a sandbox on: its self-hosted runner, else its
// remote-hosted runner, else "".
func runnerKeyOf(runner *uuid.UUID, remote string) string {
	if runner != nil {
		return runner.String()
	}
	return remote
}

// holdsRunnerSlot reports whether inserting r puts a sandbox-holding run on a runner of either kind.
func holdsRunnerSlot(r types.AgentRun) bool {
	return runnerKeyOf(r.RunnerID, r.RunnerPoolExecutor) != "" && slices.Contains(types.SlotHoldingRunStates, r.State)
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

// SetRunnerArch records the CPU architecture a runner reported on connect, the latest kept; a revoked
// runner keeps the one it had. The caller has checked it with types.ValidArch.
func (s PG) SetRunnerArch(ctx context.Context, id uuid.UUID, arch string) error {
	if _, err := s.Pool.Exec(ctx, `UPDATE runners SET arch = $2 WHERE id = $1 AND state <> 'revoked' AND arch <> $2`, id, arch); err != nil {
		return fmt.Errorf("store: set runner arch: %w", err)
	}
	return nil
}

// UpdateRunnerLimits declares the limits of a runner its owner holds, unclaimed or claimed, and returns
// the set it had (nil for none) and the row as it is now. Another person's runner, a revoked one and an
// absent id are ErrNotFound alike. Lowering a limit ends no run: the runs over it keep running, and no
// new run is placed there until the runner is under it.
func (s PG) UpdateRunnerLimits(ctx context.Context, id uuid.UUID, owner string, limits types.RunnerLimits) (before *types.RunnerLimits, after types.Runner, err error) {
	err = s.inTx(ctx, func(q Querier) error {
		row, err := scanRunner(q.QueryRow(ctx, `SELECT `+runnerCols+` FROM runners WHERE id = $1 AND owner = $2 AND state <> 'revoked' FOR NO KEY UPDATE`, id, owner))
		if err != nil {
			return err
		}
		before = row.Limits
		after, err = scanRunner(q.QueryRow(ctx, `UPDATE runners SET max_runs = $2, (`+runnerLimitCols+`) = ($3, $4, $5, $6, $7, $8)
			WHERE id = $1 RETURNING `+runnerCols, append([]any{id, limits.RunsAtOnce}, limitArgs(&limits)...)...))
		return err
	})
	return before, after, err
}
