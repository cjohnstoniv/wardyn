// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The run-watcher lease (migration 0027): two writes that make "who is
// responsible for finishing this run" a fact in Postgres, not a goroutine in
// one process. Kept out of store.go (lint size); shares run columns with
// store.go's CreateRun/GetRun and pagination.go's ListRunsPage.
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

// RunWatcherLeaser is deliberately not part of Store (like Pager): ~30 test
// doubles embed store.Store overriding only a few methods, so widening Store
// would route lease writes to the embedded nil interface inside the watcher
// goroutine, whose recover() would swallow the panic and kill it silently.
// The api layer type-asserts and skips leasing when a store lacks this; PG always has it.
type RunWatcherLeaser interface {
	ClaimStaleRunWatchers(ctx context.Context, owner string, staleAfter time.Duration) ([]types.AgentRun, error)
	HeartbeatRunWatcher(ctx context.Context, id uuid.UUID, owner string) error
	// RunWatcherFresh reports whether id's lease heartbeat is younger than
	// staleAfter (a live process owns it). The undispatched-run reaper checks
	// this so it never false-fails a RUNNING run whose sandbox_ref write was
	// lost but whose watcher still holds the lease. Missing row = not fresh (reap it).
	RunWatcherFresh(ctx context.Context, id uuid.UUID, staleAfter time.Duration) (bool, error)
}

var _ RunWatcherLeaser = PG{}

// nonTerminalRunStates is the SQL list for the complement of
// types.RunState.IsTerminal — states a stale-lease sweep may claim, kept
// single-sourced since a hand-copied set already shipped a bug once (a live
// Kill button on a finished run; terminal_parity_test.go is that scar).
// TestNonTerminalRunStates_MatchesTypes fails if this, or migration 0027's
// partial index (must repeat the list literally; predicates can't be
// parameterised), drifts from types.
//
// Design note: types.RunWaiting (reserved, unproduced) stays in this list,
// so the sweep claims runs sitting in it — a future human-in-the-loop pause
// built as "the agent exits and the run parks" must keep the agent
// in-process, or drop this state here (and from the index).
const nonTerminalRunStates = `'PENDING','STARTING','RUNNING','WAITING_FOR_CONFIRMATION'`

// ClaimStaleRunWatchers atomically takes the watcher lease, for owner, on
// every non-terminal run with a sandbox whose lease has been silent longer
// than staleAfter, returning the runs claimed for re-adoption.
//
// The conditional UPDATE ... RETURNING is the mutual exclusion: replicas
// sweeping at once re-evaluate WHERE against the row version they blocked
// on, so the loser sees the winner's fresh heartbeat and gets no row — no
// advisory lock needed (one per in-flight run via db.TryAdvisoryLock would
// exhaust the pool, which holds a connection for the whole lock).
//
// Two predicates carry the sweep's safety: nonTerminalRunStates matches
// agent_runs_watcher_sweep_idx's predicate verbatim (complement of
// types.RunState.IsTerminal, so a finished run is never re-adopted); and a
// non-empty sandbox_ref limits it to runs with something to watch — a row
// exists through the whole pre-dispatch window (grant writes, then a
// multi-minute build) and would otherwise look abandoned, so cleanup there
// stays boot-only.
//
// A KEPT run (lost_at set, migration 0073) is also never claimed: its agent
// is stopped on purpose, and a watcher would read that as exit and finalize
// the run, tearing down files it's kept for. updated_at is deliberately not
// touched: it's the idle reaper's activity signal, and a lease write isn't activity.
func (s PG) ClaimStaleRunWatchers(ctx context.Context, owner string, staleAfter time.Duration) ([]types.AgentRun, error) {
	if staleAfter < 0 {
		// Duration.String() renders "-1m30s" but ::interval binds the sign only
		// to the first field (-1m +30s = -30s): clamp, or a negative window
		// silently becomes a shorter positive one that claims live leases.
		staleAfter = 0
	}
	const q = `
		UPDATE agent_runs SET watcher_owner=$1, watcher_heartbeat=now()
		WHERE state IN (` + nonTerminalRunStates + `)
		  AND sandbox_ref <> ''
		  AND watcher_heartbeat < now() - $2::interval
		  AND lost_at IS NULL
		RETURNING ` + runCols
	return collect(ctx, s.Pool, "claim", "stale run watchers", q, []any{owner, staleAfter.String()}, scanRun)
}

// HeartbeatRunWatcher refreshes the lease on id for owner: the "still
// watching" write every watcher goroutine repeats while alive; its silence
// lets another replica's ClaimStaleRunWatchers take over.
//
// Unconditional on current owner, since a watcher can't abort its blocking
// Runner.Wait anyway and two watchers are harmless (the terminal transition
// is a CAS). Doesn't bump updated_at either, like the claim: a heartbeat
// there would make every run look forever-active and disable idle
// auto-stop. A missing row isn't an error; the lease is advisory.
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
