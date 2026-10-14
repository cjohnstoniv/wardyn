// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// UpdateRunStateIf conditionally transitions a run from fromState to toState in
// a single UPDATE ... WHERE id=$ AND state=$from, returning whether it applied.
// TOCTOU-safe: a concurrent kill/stop that already moved the run to a terminal
// state is never clobbered. From QUEUED it is LeaveRunQueue, so the run's queue row goes in the same statement. False with a nil error means someone else won the
// transition (or the run doesn't exist); the caller does nothing.
func (s PG) UpdateRunStateIf(ctx context.Context, id uuid.UUID, fromState, toState types.RunState) (bool, error) {
	if fromState == types.RunQueued {
		return s.LeaveRunQueue(ctx, id, toState, nil)
	}
	tag, err := s.Pool.Exec(ctx,
		`UPDATE agent_runs SET state=$1, updated_at=now(), ended_at=CASE WHEN $4 THEN now() ELSE ended_at END
		 WHERE id=$2 AND state=$3`,
		string(toState), id, string(fromState), toState.IsTerminal(),
	)
	if err != nil {
		return false, fmt.Errorf("store: conditional update run state: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// UpdateRunStateIfIdle is UpdateRunStateIf plus an idleness guard: it transitions
// a run only when still in fromState, updated_at has not advanced past notAfter
// (the caller's snapshot), it has no lost/end mark, and it has no open hold-aware
// request within its wait. Closes the reaper's idleness TOCTOU — an active
// `wardyn run attach` TouchRun can bump updated_at between snapshot and UPDATE,
// so guarding on state alone could stop a now-active run; notAfter makes a
// touched-since-snapshot run no-op instead. The hold-aware clause enforces "idle
// stop never fires while a request is open within its wait" inside the same
// atomic CAS, since a request raised in that window is visible only to a check
// inside the statement.
func (s PG) UpdateRunStateIfIdle(ctx context.Context, id uuid.UUID, fromState, toState types.RunState, notAfter time.Time) (bool, error) {
	if fromState == types.RunQueued {
		return false, errQueuedLeave
	}
	tag, err := s.Pool.Exec(ctx,
		`UPDATE agent_runs SET state=$1, updated_at=now(), ended_at=CASE WHEN $5 THEN now() ELSE ended_at END
		 WHERE id=$2 AND state=$3 AND updated_at <= $4 AND lost_at IS NULL AND NOT (`+openHoldSQL+`)`,
		string(toState), id, string(fromState), notAfter, toState.IsTerminal(),
	)
	if err != nil {
		return false, fmt.Errorf("store: conditional idle update run state: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// UpdateRunStateIfCreatedBefore transitions a run only when still in fromState,
// its age anchor (admitted_at when it waited in the pool queue, else created_at) is at or before createdNotAfter, and its agent still runs
// (HoldsSandboxSQL: not kept, or kept after an outage with no agent stop recorded; a
// run kept with its agent stopped is left to its files grace). The
// max-age stop's own predicate on the run's age: it carries neither the idleness
// guard nor the open-request guard of UpdateRunStateIfIdle, because an absolute
// age cap exists to end a run that is not going to finish by itself.
func (s PG) UpdateRunStateIfCreatedBefore(ctx context.Context, id uuid.UUID, fromState, toState types.RunState, createdNotAfter time.Time) (bool, error) {
	if fromState == types.RunQueued {
		return false, errQueuedLeave
	}
	tag, err := s.Pool.Exec(ctx,
		`UPDATE agent_runs SET state=$1, updated_at=now(), ended_at=CASE WHEN $5 THEN now() ELSE ended_at END
		 WHERE id=$2 AND state=$3 AND COALESCE(admitted_at, created_at) <= $4 AND `+HoldsSandboxSQL,
		string(toState), id, string(fromState), createdNotAfter, toState.IsTerminal(),
	)
	if err != nil {
		return false, fmt.Errorf("store: conditional age update run state: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}
