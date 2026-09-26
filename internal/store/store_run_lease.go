// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The run lease (long-holds design rev 4, RL-3; migration 0073): the reads and
// the two conditional writes the lease sweep needs. Kept out of store.go for
// the same size reason as store_watcher.go.
package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RunLeaser is the lease surface. Optional like RunWatcherLeaser and for the
// same reason: the ~30 test doubles that embed store.Store would route these
// calls to a nil interface. The api layer type-asserts and runs no lease sweep
// when a store lacks it; production is always PG.
type RunLeaser interface {
	// ListLeasedRuns returns every RUNNING run that has an end or is kept.
	ListLeasedRuns(ctx context.Context) ([]types.AgentRun, error)
	// MarkRunEnded marks run id kept-and-ended at now, but only while it is
	// still RUNNING, not already kept, and its end is at or before now. It
	// clears a pause: the end stops the agent, paused or not. The
	// conditional UPDATE is the mutual exclusion between replicas: exactly one
	// caller sees true and runs the end.
	MarkRunEnded(ctx context.Context, id uuid.UUID, now time.Time) (bool, error)
	// MarkRunEndingSoon records that the warning for thresholdSec before endsAt
	// went out, and reports whether this call is the one that recorded it: false
	// when that warning (or a closer one) was already sent for this same end, or
	// the run's end is no longer endsAt.
	MarkRunEndingSoon(ctx context.Context, id uuid.UUID, endsAt time.Time, thresholdSec int) (bool, error)
	// SetRunEndAndWait moves run id's end and wait from (fromEnd, fromWait) to
	// (toEnd, toWait), but only while the run still has those values and the
	// run limits fromLimits the caller decided against, is not terminal and is
	// not kept by its OWN end (LostEnded). A run lost to a reboot or a
	// control-plane outage may still move its end (F1, long-holds design rev 4
	// §2.3): extending it is how it becomes revivable again. false means the
	// run changed since the caller read it — its end, its wait, or a tightened
	// profile re-clamping its limits; nil ends are "no end". Moving the end
	// clears end_tightened_at.
	SetRunEndAndWait(ctx context.Context, id uuid.UUID, fromLimits types.RunLimits, fromEnd *time.Time, fromWait int, toEnd *time.Time, toWait int) (bool, error)
	// StopKeptRunIf makes a kept run (RUNNING with a lost/end mark) terminal, but
	// only while the row still carries the EXACT kept mark the caller read:
	// lostAt, lostReason and endsAt, compared atomically with the state guard in
	// one UPDATE (F04, long-holds review). This closes the same class of TOCTOU
	// UpdateRunStateIfIdle closes for the idle reaper: a stale lease-sweep row —
	// read before a successful extension, a revive, or a fresher end landed —
	// must never win the destructive RUNNING->terminal transition and tear down
	// a run that is no longer the one it read. state='RUNNING' stays in the
	// predicate too, so a concurrent kill's outcome is still preserved. false
	// means the mark or the state changed since the read; the caller's re-assert
	// on the next sweep pass is what makes that safe to just drop.
	StopKeptRunIf(ctx context.Context, id uuid.UUID, to types.RunState, lostAt *time.Time, lostReason types.LostReason, endsAt *time.Time) (bool, error)
}

var _ RunLeaser = PG{}

// ListLeasedRuns reads the runs the lease sweep acts on. RUNNING only: a run
// still starting reaches its end on the first sweep after it is up, and a
// terminal run has nothing left to end.
func (s PG) ListLeasedRuns(ctx context.Context) ([]types.AgentRun, error) {
	q := `SELECT ` + runCols + ` FROM agent_runs
		WHERE state = $1 AND (ends_at IS NOT NULL OR lost_at IS NOT NULL)`
	return collect(ctx, s.Pool, "list", "leased runs", q, []any{string(types.RunRunning)}, scanRun)
}

// MarkRunEnded — see RunLeaser.
func (s PG) MarkRunEnded(ctx context.Context, id uuid.UUID, now time.Time) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE agent_runs SET lost_at=$2, lost_reason=$3, paused_at=NULL, paused_reason=''
		WHERE id=$1 AND state=$4 AND lost_at IS NULL AND ends_at <= $2`,
		id, now, string(types.LostEnded), string(types.RunRunning))
	if err != nil {
		return false, fmt.Errorf("store: mark run ended: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// MarkRunEndingSoon — see RunLeaser. Keyed on the end itself, so moving the
// end re-arms every warning without anyone having to reset a counter.
func (s PG) MarkRunEndingSoon(ctx context.Context, id uuid.UUID, endsAt time.Time, thresholdSec int) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE agent_runs SET ending_soon_for=$2, ending_soon_sec=$3
		WHERE id=$1 AND ends_at=$2 AND lost_at IS NULL
		  AND (ending_soon_for IS DISTINCT FROM $2 OR ending_soon_sec > $3)`,
		id, endsAt, thresholdSec)
	if err != nil {
		return false, fmt.Errorf("store: mark run ending soon: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// SetRunEndAndWait — see RunLeaser. The compare on the values the caller read
// is what makes the caller's clamp and gate decision the one that lands: a
// concurrent change, the lease sweep ending the run, or a re-clamp tightening
// the limits the gate was read from turns this into a no-op.
func (s PG) SetRunEndAndWait(ctx context.Context, id uuid.UUID, fromLimits types.RunLimits, fromEnd *time.Time, fromWait int, toEnd *time.Time, toWait int) (bool, error) {
	limitsJSON, err := json.Marshal(fromLimits)
	if err != nil {
		return false, fmt.Errorf("store: marshal run limits: %w", err)
	}
	tag, err := s.Pool.Exec(ctx, `
		UPDATE agent_runs SET ends_at=$4, wait_budget_sec=$5,
			end_tightened_at = CASE WHEN ends_at IS DISTINCT FROM $4 THEN NULL ELSE end_tightened_at END
		WHERE id=$1 AND ends_at IS NOT DISTINCT FROM $2 AND wait_budget_sec=$3
		  AND run_limits = $7 AND (lost_at IS NULL OR lost_reason <> $8) AND state = ANY($6)`,
		id, fromEnd, fromWait, toEnd, toWait, nonTerminalStateNames(), limitsJSON, string(types.LostEnded))
	if err != nil {
		return false, fmt.Errorf("store: set run end and wait: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// StopKeptRunIf — see RunLeaser.
func (s PG) StopKeptRunIf(ctx context.Context, id uuid.UUID, to types.RunState, lostAt *time.Time, lostReason types.LostReason, endsAt *time.Time) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE agent_runs SET state=$2, updated_at=now()
		WHERE id=$1 AND state='RUNNING' AND lost_at IS NOT DISTINCT FROM $3
		  AND lost_reason=$4 AND ends_at IS NOT DISTINCT FROM $5`,
		id, string(to), lostAt, string(lostReason), endsAt)
	if err != nil {
		return false, fmt.Errorf("store: stop kept run if: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// nonTerminalStateNames is types.NonTerminalRunStates as the text[] a
// `state = ANY($n)` parameter takes.
func nonTerminalStateNames() []string {
	states := make([]string, 0, len(types.NonTerminalRunStates))
	for _, st := range types.NonTerminalRunStates {
		states = append(states, string(st))
	}
	return states
}
