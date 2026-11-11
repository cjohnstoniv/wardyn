// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The run lease (long-holds design rev 4; migration 0073): the reads and the
// conditional writes the lease sweep needs. Kept out of store.go for the same
// size reason as store_watcher.go.
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

// RunLeaser is the lease surface. Optional like RunWatcherLeaser and for the
// same reason: the ~30 test doubles that embed store.Store would route these
// calls to a nil interface. The api layer type-asserts and runs no lease sweep
// when a store lacks it; production is always PG.
type RunLeaser interface {
	// ListLeasedRuns returns every RUNNING run that has an end, a lifetime cap or is kept.
	ListLeasedRuns(ctx context.Context) ([]types.AgentRun, error)
	// MarkRunEnded marks run id kept-and-ended at now, only while still
	// RUNNING, not already kept, and its end is at or before now; clears a
	// pause. The conditional UPDATE is the cross-replica mutual exclusion:
	// exactly one caller sees true and runs the end.
	MarkRunEnded(ctx context.Context, id uuid.UUID, now time.Time) (bool, error)
	// MarkRunEndingSoon records that the thresholdSec-before-endsAt warning
	// went out, reporting whether this call recorded it: false when that
	// warning (or a closer one) already went for this end, or the end changed.
	MarkRunEndingSoon(ctx context.Context, id uuid.UUID, endsAt time.Time, thresholdSec int) (bool, error)
	// SetRunEndAndWait moves run id's end/wait from (fromEnd, fromWait) to
	// (toEnd, toWait), only while the run still has those values, limits
	// fromLimits, and isn't terminal. A run lost to a reboot/outage may still
	// move its end — extending it is how it becomes revivable again. A run
	// kept by its OWN end (LostEnded) moves only with ended set, exactly as
	// EndedKept describes. false means the run changed since the read (end,
	// wait, or a re-clamped profile); nil ends mean "no end". Moving the end
	// clears end_tightened_at.
	SetRunEndAndWait(ctx context.Context, id uuid.UUID, fromLimits types.RunLimits, fromEnd *time.Time, fromWait int, toEnd *time.Time, toWait int, ended *EndedKept) (bool, error)
	// StopKeptRunIf makes a kept run (RUNNING with a lost/end mark) terminal,
	// only while the row still carries the EXACT kept mark read (lostAt,
	// lostReason, endsAt), checked atomically with the state guard in one
	// UPDATE. Closes the same TOCTOU class UpdateRunStateIfIdle closes for the
	// idle reaper: a stale lease-sweep row must never win the destructive
	// RUNNING->terminal transition and tear down a run it no longer describes.
	// false means the mark or state changed; the caller's re-assert next sweep
	// pass makes dropping that safe.
	StopKeptRunIf(ctx context.Context, id uuid.UUID, to types.RunState, lostAt *time.Time, lostReason types.LostReason, endsAt *time.Time) (bool, error)
	// SetRunContainmentError records that a kept run's stop (proxy or agent)
	// failed with msg, only while RUNNING and kept. Message refreshes on every
	// call; containment_error_at keeps the first failure's time.
	SetRunContainmentError(ctx context.Context, id uuid.UUID, msg string, now time.Time) error
	// ClearRunContainmentError clears it once the stop is confirmed; true only
	// for the call that cleared a recorded error, so resolution is audited
	// once across replicas.
	ClearRunContainmentError(ctx context.Context, id uuid.UUID) (bool, error)
	// MarkRunAgentStopped records that the agent of id, a run kept after an
	// outage (lostAt is its mark), was stopped, so it holds no sandbox whatever
	// an extension later does to its end (HoldsSandboxSQL). A no-op unless the
	// row is RUNNING and still carries that outage mark with no stop recorded;
	// a revive clears it.
	MarkRunAgentStopped(ctx context.Context, id uuid.UUID, lostAt time.Time) error
}

// ForcedStopper is the forced end's surface, optional like store.Pager: the api layer asserts it at the call,
// so the RunLeaser fakes need no change and a store without it never forces an end (migration 0276).
type ForcedStopper interface {
	// StopKeptRunForcedIf is StopKeptRunIf, and in the same statement stamps forced_stop_at. It answers the
	// stamp the row now carries, for ClearForcedStop. applied is false when the mark or state changed.
	StopKeptRunForcedIf(ctx context.Context, id uuid.UUID, to types.RunState, lostAt *time.Time, lostReason types.LostReason, endsAt *time.Time) (stamp time.Time, applied bool, err error)
	// ListForcedStops lists the runs whose forced stop is not yet confirmed.
	ListForcedStops(ctx context.Context) ([]ForcedStop, error)
	// ClearForcedStop clears run id's mark, only while it still carries stamp (the one the caller read).
	ClearForcedStop(ctx context.Context, id uuid.UUID, stamp time.Time) (bool, error)
}

// ForcedStop is a run with an unconfirmed forced stop, and the stamp its mark carries.
type ForcedStop struct {
	RunID uuid.UUID
	At    time.Time
}

// EndedKept is the condition a write on a run kept by its OWN end (LostEnded)
// lands under during its files grace: the row must still carry the exact
// ended mark decided on (LostAt), after KeptAfter (Now less the ended-run
// grace). A grace that runs out between check and write refuses the write;
// nothing here moves the mark, so no write renews the grace. A revive also
// needs the run's end to be after Now.
type EndedKept struct {
	LostAt, KeptAfter, Now time.Time
}

// endedArgs is ended's mark and cutoff as nullable query arguments.
func endedArgs(ended *EndedKept) (lostAt, keptAfter, now *time.Time) {
	if ended == nil {
		return nil, nil, nil
	}
	return &ended.LostAt, &ended.KeptAfter, &ended.Now
}

var _ RunLeaser = PG{}

// ListLeasedRuns reads the runs the lease sweep acts on. RUNNING only: a run
// still starting reaches its end on the first sweep after it is up, and a
// terminal run has nothing left to end.
func (s PG) ListLeasedRuns(ctx context.Context) ([]types.AgentRun, error) {
	q := `SELECT ` + runCols + ` FROM agent_runs
		WHERE state = $1 AND (ends_at IS NOT NULL OR lost_at IS NOT NULL OR lifetime_cap_at IS NOT NULL)`
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
func (s PG) SetRunEndAndWait(ctx context.Context, id uuid.UUID, fromLimits types.RunLimits, fromEnd *time.Time, fromWait int, toEnd *time.Time, toWait int, ended *EndedKept) (bool, error) {
	limitsJSON, err := json.Marshal(fromLimits)
	if err != nil {
		return false, fmt.Errorf("store: marshal run limits: %w", err)
	}
	lostAt, keptAfter, _ := endedArgs(ended)
	tag, err := s.Pool.Exec(ctx, `
		UPDATE agent_runs SET ends_at=$4, wait_budget_sec=$5,
			end_tightened_at = CASE WHEN ends_at IS DISTINCT FROM $4 THEN NULL ELSE end_tightened_at END
		WHERE id=$1 AND ends_at IS NOT DISTINCT FROM $2 AND wait_budget_sec=$3 AND state = ANY($6)
		  AND run_limits = $10
		  AND CASE WHEN $8::timestamptz IS NULL THEN lost_at IS NULL OR lost_reason <> $7
		           ELSE lost_reason = $7 AND lost_at = $8 AND lost_at > $9::timestamptz END`,
		id, fromEnd, fromWait, toEnd, toWait, slotHoldingStateNames(), string(types.LostEnded), lostAt, keptAfter, limitsJSON)
	if err != nil {
		return false, fmt.Errorf("store: set run end and wait: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// KillRunAtLifetimeIf moves a RUNNING run to KILLED and writes its status_detail in
// the one statement, but only while the reason still holds on the row as it stands
// at now: the lease sweep decides from a row it listed earlier, and a person may
// have moved the end, or another replica kept the run at its end, since. The
// conditions are the reason's own, as MarkRunEnded's are for the keep:
//   - "end_reached": a Background run whose end is at or before now and before its
//     lifetime cap (an end at the cap is the maximum);
//   - "max_lifetime_reached": the lifetime cap is at or before now, and the run is
//     not a Background run whose end came first (that run is end_reached).
//
// Both refuse a run its own end already kept (lost_reason 'ended'): a kept run
// keeps its files grace. False with a nil error is a no-op for the caller: no kill,
// no audit row. detail is what status_detail becomes ("lifetime: <reason>").
func (s PG) KillRunAtLifetimeIf(ctx context.Context, id uuid.UUID, reason types.RunEndReason, detail string, now time.Time) (bool, error) {
	var cond string
	switch reason {
	case types.RunEndEndReached:
		cond = `experience = 'background' AND ends_at <= $3 AND (lifetime_cap_at IS NULL OR ends_at < lifetime_cap_at)`
	case types.RunEndMaxLifetimeReached:
		cond = `lifetime_cap_at <= $3 AND NOT (experience = 'background' AND COALESCE(ends_at < lifetime_cap_at, false))`
	default:
		return false, fmt.Errorf("store: kill run at lifetime: unknown reason %q", reason)
	}
	tag, err := s.Pool.Exec(ctx,
		`UPDATE agent_runs SET state='KILLED', status_detail=$2, updated_at=now(), ended_at=now()
		 WHERE id=$1 AND state='RUNNING' AND NOT (lost_at IS NOT NULL AND lost_reason = 'ended') AND `+cond,
		id, detail, now,
	)
	if err != nil {
		return false, fmt.Errorf("store: kill run at lifetime: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// StopKeptRunIf — see RunLeaser.
func (s PG) StopKeptRunIf(ctx context.Context, id uuid.UUID, to types.RunState, lostAt *time.Time, lostReason types.LostReason, endsAt *time.Time) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE agent_runs SET state=$2, updated_at=now(), ended_at=CASE WHEN $6 THEN now() ELSE ended_at END
		WHERE id=$1 AND state='RUNNING' AND lost_at IS NOT DISTINCT FROM $3
		  AND lost_reason=$4 AND ends_at IS NOT DISTINCT FROM $5`,
		id, string(to), lostAt, string(lostReason), endsAt, to.IsTerminal())
	if err != nil {
		return false, fmt.Errorf("store: stop kept run if: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// StopKeptRunForcedIf — see ForcedStopper. It is StopKeptRunIf's UPDATE with the same SET and WHERE, plus the mark.
func (s PG) StopKeptRunForcedIf(ctx context.Context, id uuid.UUID, to types.RunState, lostAt *time.Time, lostReason types.LostReason, endsAt *time.Time) (time.Time, bool, error) {
	var stamp time.Time
	err := s.Pool.QueryRow(ctx, `
		UPDATE agent_runs SET state=$2, updated_at=now(), ended_at=CASE WHEN $6 THEN now() ELSE ended_at END, forced_stop_at=now()
		WHERE id=$1 AND state='RUNNING' AND lost_at IS NOT DISTINCT FROM $3
		  AND lost_reason=$4 AND ends_at IS NOT DISTINCT FROM $5
		RETURNING forced_stop_at`,
		id, string(to), lostAt, string(lostReason), endsAt, to.IsTerminal()).Scan(&stamp)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store: stop kept run forced if: %w", err)
	}
	return stamp, true, nil
}

// ListForcedStops — see ForcedStopper.
func (s PG) ListForcedStops(ctx context.Context) ([]ForcedStop, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, forced_stop_at FROM agent_runs WHERE forced_stop_at IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("store: list forced stops: %w", err)
	}
	defer rows.Close()
	var out []ForcedStop
	for rows.Next() {
		var f ForcedStop
		if err := rows.Scan(&f.RunID, &f.At); err != nil {
			return nil, fmt.Errorf("store: list forced stops: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ClearForcedStop — see ForcedStopper.
func (s PG) ClearForcedStop(ctx context.Context, id uuid.UUID, stamp time.Time) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `UPDATE agent_runs SET forced_stop_at=NULL WHERE id=$1 AND forced_stop_at=$2`, id, stamp)
	if err != nil {
		return false, fmt.Errorf("store: clear forced stop: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// nonTerminalStateNames is types.NonTerminalRunStates as the text[] a
// `state = ANY($n)` parameter takes.
func nonTerminalStateNames() []string { return stateNames(types.NonTerminalRunStates) }

// slotHoldingStateNames is types.SlotHoldingRunStates in the same form: the states a pool,
// deployment or host slot is counted over, and the ones a run's end, limits or age are written to.
// A QUEUED run is in neither.
func slotHoldingStateNames() []string { return stateNames(types.SlotHoldingRunStates) }

func stateNames(in []types.RunState) []string {
	states := make([]string, 0, len(in))
	for _, st := range in {
		states = append(states, string(st))
	}
	return states
}

// SetRunContainmentError — see RunLeaser.
func (s PG) SetRunContainmentError(ctx context.Context, id uuid.UUID, msg string, now time.Time) error {
	if _, err := s.Pool.Exec(ctx, `
		UPDATE agent_runs SET containment_error=$2, containment_error_at=COALESCE(containment_error_at, $3)
		WHERE id=$1 AND state='RUNNING' AND lost_at IS NOT NULL`,
		id, msg, now); err != nil {
		return fmt.Errorf("store: set run containment error: %w", err)
	}
	return nil
}

// MarkRunAgentStopped — see RunLeaser.
func (s PG) MarkRunAgentStopped(ctx context.Context, id uuid.UUID, lostAt time.Time) error {
	if _, err := s.Pool.Exec(ctx, `
		UPDATE agent_runs SET agent_stopped_at=now()
		WHERE id=$1 AND state=$2 AND lost_at=$3 AND lost_reason=$4 AND agent_stopped_at IS NULL`,
		id, string(types.RunRunning), lostAt, string(types.LostOutage)); err != nil {
		return fmt.Errorf("store: mark run agent stopped: %w", err)
	}
	return nil
}

// RunAgentStopped reports whether MarkRunAgentStopped persisted a stop for id: its agent_stopped_at is set.
func (s PG) RunAgentStopped(ctx context.Context, id uuid.UUID) (bool, error) {
	var stopped bool
	if err := s.Pool.QueryRow(ctx, `SELECT agent_stopped_at IS NOT NULL FROM agent_runs WHERE id=$1`, id).Scan(&stopped); err != nil {
		return false, fmt.Errorf("store: read run agent stop: %w", err)
	}
	return stopped, nil
}

// ClearRunContainmentError — see RunLeaser.
func (s PG) ClearRunContainmentError(ctx context.Context, id uuid.UUID) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE agent_runs SET containment_error=NULL, containment_error_at=NULL
		WHERE id=$1 AND containment_error IS NOT NULL`, id)
	if err != nil {
		return false, fmt.Errorf("store: clear run containment error: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}
