// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The run-watcher lease (migration 0027): the two writes that make "who is
// responsible for finishing this run" a fact in Postgres instead of a goroutine
// in one process. Kept out of store.go on purpose (it sits at a lint size
// boundary); the run columns they read back are the same ones store.go's
// CreateRun/GetRun and pagination.go's ListRunsPage select.
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

// RunWatcherLeaser is the watcher-lease surface. Like Pager (pagination.go) and
// for the same reason, it is deliberately NOT part of the Store interface: the
// control plane has ~30 test doubles that embed store.Store and override a
// handful of methods, so widening Store would route their lease writes to the
// embedded nil interface — and a lease write happens inside the watcher
// goroutine, whose own recover() would swallow the panic and kill the watcher
// silently. The api layer type-asserts and simply does not lease when a store
// lacks the surface; production is always PG, which has it.
type RunWatcherLeaser interface {
	ClaimStaleRunWatchers(ctx context.Context, owner string, staleAfter time.Duration) ([]types.AgentRun, error)
	HeartbeatRunWatcher(ctx context.Context, id uuid.UUID, owner string) error
	// RunWatcherFresh reports whether run id's watcher lease is still fresh (its
	// heartbeat is younger than staleAfter) — a live process is responsible for it.
	// The undispatched-run reaper consults it so it never false-fails a RUNNING run
	// whose sandbox_ref write was merely lost but whose live watcher is holding the
	// lease (GAP-RECONCILE-4). A missing row reads as NOT fresh (reap it).
	RunWatcherFresh(ctx context.Context, id uuid.UUID, staleAfter time.Duration) (bool, error)
}

// Compile-time assertion: PG satisfies RunWatcherLeaser.
var _ RunWatcherLeaser = PG{}

// nonTerminalRunStates is the SQL value list for the complement of
// types.RunState.IsTerminal — the states a stale-lease sweep may claim. The set
// is single-sourced HERE rather than written out at each use because
// types.RunState declares itself the one definition of terminal-ness and a
// hand-copied set has already shipped a bug once (a live Kill button on finished
// runs; internal/types/terminal_parity_test.go is that scar).
// TestNonTerminalRunStates_MatchesTypes derives the expected set from types and
// fails if this const — or migration 0027's partial index, which must repeat the
// list literally because an index predicate cannot be parameterised — drifts.
//
// DESIGN NOTE for the WAITING_FOR_CONFIRMATION producer (types.RunWaiting is a
// reserved, not-yet-produced state): the sweep WILL claim runs sitting in it. If
// the human-in-the-loop pause is ever implemented as "the agent process exits and
// the run parks", adoption would probe a dead agent and finalize a run that is
// merely waiting on a human. That producer must keep the agent in-process — or
// drop this state from the list (and the index) when it lands.
const nonTerminalRunStates = `'PENDING','STARTING','RUNNING','WAITING_FOR_CONFIRMATION'`

// ClaimStaleRunWatchers atomically takes the watcher lease, for owner, on every
// non-terminal run that HAS a sandbox and whose lease has been silent longer
// than staleAfter — returning exactly the runs it claimed, for the caller to
// re-adopt.
//
// The single conditional UPDATE ... RETURNING *is* the mutual exclusion. Two
// replicas sweeping at the same instant both re-evaluate the WHERE against the
// row version they blocked on, so the loser sees the winner's fresh heartbeat
// and returns no row — no advisory lock needed (and none wanted:
// db.TryAdvisoryLock borrows a pool connection for the entire hold, so one lock
// per in-flight run would exhaust the pool).
//
// Two predicates carry the safety of the whole sweep:
//   - the state list (nonTerminalRunStates) is the complement of
//     types.RunState.IsTerminal, and matches agent_runs_watcher_sweep_idx's
//     predicate verbatim so the partial index serves it. A finished run is never
//     re-adopted.
//   - a non-empty sandbox_ref restricts the sweep to runs that actually have
//     something to watch. A run row exists for its whole pre-dispatch window
//     (grant writes, then a multi-minute image build), and claiming one of those
//     would hand the caller a run that merely LOOKS abandoned. Cleaning those up
//     stays boot-only, where the dispatching process is known to be gone.
//
// updated_at is deliberately NOT touched: it is the idle reaper's activity
// signal, and a lease write is not run activity.
func (s PG) ClaimStaleRunWatchers(ctx context.Context, owner string, staleAfter time.Duration) ([]types.AgentRun, error) {
	if staleAfter < 0 {
		// Duration.String() renders "-1m30s", and ::interval binds the sign to the
		// FIRST field only (-1m +30s = -30s), so a negative window would silently
		// become a shorter POSITIVE one and claim live leases. Clamp to now.
		staleAfter = 0
	}
	const q = `
		UPDATE agent_runs SET watcher_owner=$1, watcher_heartbeat=now()
		WHERE state IN (` + nonTerminalRunStates + `)
		  AND sandbox_ref <> ''
		  AND watcher_heartbeat < now() - $2::interval
		RETURNING id, created_at, updated_at, created_by, agent, repo, task,
			policy_id, confinement_class, state, spiffe_id, runner_target, sandbox_ref, interactive, workspace_path, workspace_id, source_id, image, auto_stop_after_sec, agent_exec_id`
	return collect(ctx, s.Pool, "claim", "stale run watchers", q, []any{owner, staleAfter.String()}, scanRun)
}

// HeartbeatRunWatcher refreshes the lease on id for owner — the "I am still
// watching this run" write every watcher goroutine repeats while it lives. Its
// silence is what lets another replica's ClaimStaleRunWatchers take over.
//
// Unconditional on the current owner: a watcher cannot abort its blocking
// Runner.Wait anyway, so learning it lost the lease would give it nothing to do,
// and two watchers are already harmless (the terminal transition is a CAS, so
// only one can win). Like the claim, it does NOT bump updated_at — a 30s
// heartbeat on the idle reaper's activity column would make every run look
// forever-active and silently disable idle auto-stop. A missing row is not an
// error: the lease is advisory.
func (s PG) HeartbeatRunWatcher(ctx context.Context, id uuid.UUID, owner string) error {
	_, err := s.Pool.Exec(ctx,
		`UPDATE agent_runs SET watcher_owner=$2, watcher_heartbeat=now() WHERE id=$1`, id, owner)
	if err != nil {
		return fmt.Errorf("store: heartbeat run watcher: %w", err)
	}
	return nil
}

// RunWatcherFresh reports whether id's watcher lease is younger than staleAfter —
// mirrors ClaimStaleRunWatchers' own staleness predicate (a live watcher owns it).
// A missing row → (false, nil): nothing is watching it, so the reaper may proceed.
func (s PG) RunWatcherFresh(ctx context.Context, id uuid.UUID, staleAfter time.Duration) (bool, error) {
	if staleAfter < 0 {
		staleAfter = 0
	}
	var fresh bool
	err := s.Pool.QueryRow(ctx,
		`SELECT watcher_heartbeat > now() - $2::interval FROM agent_runs WHERE id=$1`,
		id, staleAfter.String()).Scan(&fresh)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("store: run watcher fresh: %w", err)
	}
	return fresh, nil
}
