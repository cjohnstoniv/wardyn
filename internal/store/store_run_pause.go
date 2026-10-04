// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Pause and resume (long-holds design rev 4, §3, RL-7; migration 0097): the
// presence clock, the reads the pause sweep needs and its conditional writes.
// Kept out of store.go for the same size reason as store_run_lease.go.
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

// ReauthHoldMax is the longest a credential re-auth hold keeps the agent's
// connection parked (WARDYN_CREDENTIAL_REAUTH_TIMEOUT's ceiling). A re-auth
// request counts toward a waiting pause only once it is older than this: until
// then the agent is still inside the connection that asked, and freezing it
// would cut a sign-in the person may be finishing.
const ReauthHoldMax = 1800 * time.Second

// waitingHoldSQL is openHoldSQL narrowed to the requests a waiting pause
// counts: a re-auth row only once its connection hold is over.
var waitingHoldSQL = fmt.Sprintf(`EXISTS (SELECT 1 FROM approvals a WHERE %s
		  AND (a.kind <> '%s' OR a.requested_at < now() - make_interval(secs => %d)))`,
	openHoldCond, types.ApprovalCredentialReauth, int(ReauthHoldMax.Seconds()))

// PauseCandidate is one live run as the pause sweep sees it.
type PauseCandidate struct {
	Run types.AgentRun
	// OpenRequest: the run has a PENDING request inside its wait (openHoldSQL).
	OpenRequest bool
	// WaitingRequest: one of those counts toward a waiting pause
	// (waitingHoldSQL).
	WaitingRequest bool
}

// RunPauser is the pause surface. Optional like RunLeaser and for the same
// reason; the api layer runs no pause sweep, and stamps no presence, when a
// store lacks it. Production is always PG.
type RunPauser interface {
	// ListPauseCandidates returns every RUNNING, not-kept run with a sandbox,
	// paused or not, and the store's own clock read with them.
	ListPauseCandidates(ctx context.Context) ([]PauseCandidate, time.Time, error)
	// StampRunActive moves run id's presence clock to now, and reports whether
	// the run is paused. A terminal or missing run is not stamped (false, nil).
	StampRunActive(ctx context.Context, id uuid.UUID) (paused bool, err error)
	// MarkRunPaused marks run id paused for reason, but only while it is still
	// RUNNING, not kept, not paused, its presence clock still reads activeAt
	// (the snapshot the caller judged idle; nil is "never stamped"), and — for
	// a waiting pause — it still has a request the waiting pause counts, or —
	// for an idle pause — it has no open request. The conditional UPDATE is
	// what makes a keystroke, or a request closing or opening, between the
	// caller's read and this write win: false means leave the run running.
	MarkRunPaused(ctx context.Context, id uuid.UUID, reason types.PauseReason, activeAt *time.Time) (bool, error)
	// ClearRunPaused clears run id's pause, and reports whether it was paused.
	ClearRunPaused(ctx context.Context, id uuid.UUID) (bool, error)
	// RunHasOpenRequest reports whether run id has a PENDING request inside
	// its wait.
	RunHasOpenRequest(ctx context.Context, id uuid.UUID) (bool, error)
}

var _ RunPauser = PG{}

// PauseSettle is one run a pause compensation may have left with its agent
// container not matching its pause mark (migration 0132).
type PauseSettle struct {
	RunID uuid.UUID
	DueAt time.Time
}

// PauseSettler is the durable record of the runs whose container may not match
// their pause mark. Optional like RunPauser; production is always PG.
type PauseSettler interface {
	// NotePauseSettle records run id as owing a settle from after on (the
	// database's clock), moving a due_at already recorded only forward, and
	// returns the due_at it holds.
	NotePauseSettle(ctx context.Context, id uuid.UUID, after time.Duration) (time.Time, error)
	// DuePauseSettles lists the runs whose settle is due.
	DuePauseSettles(ctx context.Context) ([]PauseSettle, error)
	// ClearPauseSettle deletes run id's record only while it still reads due,
	// so a later note survives.
	ClearPauseSettle(ctx context.Context, id uuid.UUID, due time.Time) error
}

var _ PauseSettler = PG{}

// NotePauseSettle — see PauseSettler.
func (s PG) NotePauseSettle(ctx context.Context, id uuid.UUID, after time.Duration) (time.Time, error) {
	var due time.Time
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO run_pause_settles (run_id, due_at) VALUES ($1, now() + make_interval(secs => $2))
		ON CONFLICT (run_id) DO UPDATE SET due_at = GREATEST(run_pause_settles.due_at, EXCLUDED.due_at)
		RETURNING due_at`, id, after.Seconds()).Scan(&due)
	if err != nil {
		return time.Time{}, fmt.Errorf("store: note a pause settle: %w", err)
	}
	return due, nil
}

// DuePauseSettles — see PauseSettler.
func (s PG) DuePauseSettles(ctx context.Context) ([]PauseSettle, error) {
	rows, err := s.Pool.Query(ctx, `SELECT run_id, due_at FROM run_pause_settles WHERE due_at <= now() ORDER BY due_at`)
	if err != nil {
		return nil, fmt.Errorf("store: list due pause settles: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (PauseSettle, error) {
		var p PauseSettle
		return p, r.Scan(&p.RunID, &p.DueAt)
	})
	if err != nil {
		return nil, fmt.Errorf("store: list due pause settles: %w", err)
	}
	return out, nil
}

// ClearPauseSettle — see PauseSettler.
func (s PG) ClearPauseSettle(ctx context.Context, id uuid.UUID, due time.Time) error {
	if _, err := s.Pool.Exec(ctx, `DELETE FROM run_pause_settles WHERE run_id = $1 AND due_at = $2`, id, due); err != nil {
		return fmt.Errorf("store: clear a pause settle: %w", err)
	}
	return nil
}

// withExtra scans runCols and then the extra columns a query appends.
type withExtra struct {
	pgx.Row
	extra []any
}

func (w withExtra) Scan(dest ...any) error { return w.Row.Scan(append(dest, w.extra...)...) }

// ListPauseCandidates — see RunPauser.
func (s PG) ListPauseCandidates(ctx context.Context) ([]PauseCandidate, time.Time, error) {
	q := `SELECT ` + runCols + `, ` + openHoldSQL + `, ` + waitingHoldSQL + `, now() FROM agent_runs
		WHERE state = $1 AND lost_at IS NULL AND sandbox_ref <> ''`
	rows, err := s.Pool.Query(ctx, q, string(types.RunRunning))
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("store: list pause candidates: %w", err)
	}
	defer rows.Close()
	var (
		out []PauseCandidate
		now time.Time
	)
	for rows.Next() {
		var c PauseCandidate
		run, err := scanRun(withExtra{Row: rows, extra: []any{&c.OpenRequest, &c.WaitingRequest, &now}})
		if err != nil {
			return nil, time.Time{}, err
		}
		c.Run = run
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, time.Time{}, fmt.Errorf("store: iterate pause candidates: %w", err)
	}
	return out, now, nil
}

// StampRunActive — see RunPauser.
func (s PG) StampRunActive(ctx context.Context, id uuid.UUID) (bool, error) {
	var paused bool
	err := s.Pool.QueryRow(ctx, `
		UPDATE agent_runs SET active_at=now()
		WHERE id=$1 AND state = ANY($2)
		RETURNING paused_at IS NOT NULL`, id, nonTerminalStateStrings()).Scan(&paused)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: stamp run active: %w", err)
	}
	return paused, nil
}

// MarkRunPaused — see RunPauser.
func (s PG) MarkRunPaused(ctx context.Context, id uuid.UUID, reason types.PauseReason, activeAt *time.Time) (bool, error) {
	if reason != types.PauseWaiting && reason != types.PauseIdle {
		return false, fmt.Errorf("store: mark run paused: unknown reason %q", reason)
	}
	tag, err := s.Pool.Exec(ctx, `
		UPDATE agent_runs SET paused_at=now(), paused_reason=$2
		WHERE id=$1 AND state=$3 AND lost_at IS NULL AND paused_at IS NULL
		  AND active_at IS NOT DISTINCT FROM $4
		  AND ($2 <> '`+string(types.PauseWaiting)+`' OR `+waitingHoldSQL+`)
		  AND ($2 <> '`+string(types.PauseIdle)+`' OR NOT `+openHoldSQL+`)`,
		id, string(reason), string(types.RunRunning), activeAt)
	if err != nil {
		return false, fmt.Errorf("store: mark run paused: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ClearRunPaused — see RunPauser.
func (s PG) ClearRunPaused(ctx context.Context, id uuid.UUID) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE agent_runs SET paused_at=NULL, paused_reason=''
		WHERE id=$1 AND paused_at IS NOT NULL`, id)
	if err != nil {
		return false, fmt.Errorf("store: clear run paused: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// RunHasOpenRequest — see RunPauser.
func (s PG) RunHasOpenRequest(ctx context.Context, id uuid.UUID) (bool, error) {
	var open bool
	err := s.Pool.QueryRow(ctx, `SELECT `+openHoldSQL+` FROM agent_runs WHERE id=$1`, id).Scan(&open)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: run has open request: %w", err)
	}
	return open, nil
}

// nonTerminalStateStrings is types.NonTerminalRunStates as the text[] a
// state = ANY($n) parameter takes.
func nonTerminalStateStrings() []string {
	out := make([]string, 0, len(types.NonTerminalRunStates))
	for _, st := range types.NonTerminalRunStates {
		out = append(out, string(st))
	}
	return out
}
