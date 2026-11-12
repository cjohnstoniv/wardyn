// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

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

// terminalSandboxPageMax is the most rows ListTerminalSandboxPage returns in one page.
const terminalSandboxPageMax = 500

// terminalRunStates are the states types.RunState.IsTerminal holds; the contain reads and writes are
// guarded by them so a run that is not over is never listed, fenced or stamped.
var terminalRunStates = []string{
	string(types.RunCompleted), string(types.RunFailed), string(types.RunKilled), string(types.RunStopped), string(types.RunArchived),
}

// TerminalSandboxRow is the narrow view of an ended run that still holds a sandbox: what the containment
// loop needs to decide whether and how to cut it, and nothing else.
type TerminalSandboxRow struct {
	ID                 uuid.UUID
	State              types.RunState
	SandboxRef         string
	RunnerID           *uuid.UUID
	MachineKind        types.MachineKind
	EndedAt            *time.Time
	UpdatedAt          time.Time
	ContainmentErrorAt *time.Time
	ForcedStopAt       *time.Time
	FencedAt           *time.Time
}

// TerminalSandboxCursor is the keyset position after a page: the last row's end (EndedAt, or UpdatedAt
// when unset) and id.
type TerminalSandboxCursor struct {
	At time.Time
	ID uuid.UUID
}

// RunContainer is the containment loop's store surface. PG implements it.
type RunContainer interface {
	ListTerminalSandboxPage(ctx context.Context, after *TerminalSandboxCursor, limit int, unfencedOnly bool) ([]TerminalSandboxRow, *TerminalSandboxCursor, error)
	MarkRunFenced(ctx context.Context, id uuid.UUID) error
	SetRunContainmentErrorSince(ctx context.Context, id uuid.UUID, msg string, now, since time.Time) (bool, error)
	ClearRunContainmentError(ctx context.Context, id uuid.UUID) (bool, error)
}

var _ RunContainer = PG{}

// ContainmentStreaker is the stop path's failure-streak test. PG implements it; it is a separate surface so
// a store that lists and stamps ended runs need not answer it.
type ContainmentStreaker interface {
	ContainmentStreakDue(ctx context.Context, id uuid.UUID, tau time.Duration, since time.Time) (bool, error)
}

var _ ContainmentStreaker = PG{}

// ListTerminalSandboxPage lists ended runs that hold a sandbox, newest end first, ties by id, one keyset
// page of at most limit rows (never more than 500) after the cursor, or from the newest when after is nil.
// The cursor answered is nil on the last page. With unfencedOnly it skips runs whose agent was already cut
// off (fenced_at set) and reads agent_runs_unfenced_sandbox_idx; without it the page reads
// agent_runs_terminal_sandbox_idx. No running run is ever listed.
func (s PG) ListTerminalSandboxPage(ctx context.Context, after *TerminalSandboxCursor, limit int, unfencedOnly bool) ([]TerminalSandboxRow, *TerminalSandboxCursor, error) {
	if limit <= 0 || limit > terminalSandboxPageMax {
		limit = terminalSandboxPageMax
	}
	q := `SELECT id, state, sandbox_ref, runner_id, machine_kind, ended_at, updated_at, containment_error_at, forced_stop_at, fenced_at
		FROM agent_runs WHERE sandbox_ref <> '' AND state = ANY($1)`
	args := []any{terminalRunStates}
	if unfencedOnly {
		q += ` AND fenced_at IS NULL`
	}
	if after != nil {
		q += ` AND (COALESCE(ended_at, updated_at), id) < ($2, $3)`
		args = append(args, pgTime(after.At), after.ID)
	}
	q += fmt.Sprintf(` ORDER BY COALESCE(ended_at, updated_at) DESC, id DESC LIMIT %d`, limit)
	rows, err := collect(ctx, s.Pool, "list", "terminal sandbox runs", q, args, func(row pgx.Row) (TerminalSandboxRow, error) {
		var r TerminalSandboxRow
		var state, kind string
		err := row.Scan(&r.ID, &state, &r.SandboxRef, &r.RunnerID, &kind, &r.EndedAt, &r.UpdatedAt, &r.ContainmentErrorAt, &r.ForcedStopAt, &r.FencedAt)
		r.State, r.MachineKind = types.RunState(state), types.MachineKind(kind)
		return r, err
	})
	if err != nil {
		return nil, nil, err
	}
	if len(rows) < limit {
		return rows, nil, nil
	}
	last := rows[len(rows)-1]
	at := last.UpdatedAt
	if last.EndedAt != nil {
		at = *last.EndedAt
	}
	return rows, &TerminalSandboxCursor{At: at, ID: last.ID}, nil
}

// MarkRunFenced records that an ended run's agent was cut off every network: fenced_at is set once, and
// only on a terminal run. It is written only after FenceProxy returned nil; a probe never counts.
func (s PG) MarkRunFenced(ctx context.Context, id uuid.UUID) error {
	if _, err := s.Pool.Exec(ctx, `UPDATE agent_runs SET fenced_at = now() WHERE id=$1 AND fenced_at IS NULL AND state = ANY($2)`,
		id, terminalRunStates); err != nil {
		return fmt.Errorf("store: mark run fenced: %w", err)
	}
	return nil
}

// SetRunContainmentErrorSince records a failed containment cut of an ended run: the message always, and
// the time the failures began. containment_error_at keeps an earlier stamp only if it is not older than
// since; one older than since, or a missing one, is set to now. since is the caller's streak start (the
// later of the run's due time, its last unobserved round and the process start), so a stamp left over
// from before it never starts an ended run's streak: a kept-phase failure is recorded while the run is
// still RUNNING, and nothing clears it at the end. It answers started when this call set the stamp to
// now. A run that is not terminal has no row to update: started false, no error.
func (s PG) SetRunContainmentErrorSince(ctx context.Context, id uuid.UUID, msg string, now, since time.Time) (bool, error) {
	var started bool
	err := s.Pool.QueryRow(ctx, `
		UPDATE agent_runs SET containment_error = $2,
		       containment_error_at = CASE WHEN containment_error_at IS NULL OR containment_error_at < $4 THEN $3 ELSE containment_error_at END
		 WHERE id = $1 AND state = ANY($5)
		RETURNING containment_error_at = $3`, id, msg, pgTime(now), pgTime(since), terminalRunStates).Scan(&started)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: set run containment error since: %w", err)
	}
	return started, nil
}

// ContainmentStreakDue reports whether run id's recorded containment failure began at least tau ago and is
// not older than since or than the run's containment due: a second failure that far after the first. The
// age is measured on the database's clock, so replicas with different clocks agree. A stamp from before
// since (a kept-phase failure, a round that did not observe the sandbox, an older process) never counts,
// and neither does a missing stamp or run.
func (s PG) ContainmentStreakDue(ctx context.Context, id uuid.UUID, tau time.Duration, since time.Time) (bool, error) {
	var due bool
	err := s.Pool.QueryRow(ctx, `SELECT containment_error_at IS NOT NULL
		AND containment_error_at >= GREATEST(`+ContainmentDueSQL+`, $3)
		AND containment_error_at <= now() - make_interval(secs => $2)
		FROM agent_runs WHERE id = $1`, id, tau.Seconds(), pgTime(since)).Scan(&due)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: containment streak: %w", err)
	}
	return due, nil
}
