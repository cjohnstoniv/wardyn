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
)

// A run_schedules row is one repeating schedule a person keeps (#3458, migration 0400): the rule, the run
// request less its start fields, and the authority it was made or last resumed under. The leader sweep books
// each occurrence as a one-time scheduled run and then advances next_at with a compare-and-set, so an
// occurrence is never advanced twice.

// ErrRunScheduleLimit is CreateRunSchedule's refusal: the owner already keeps the most schedules allowed.
var ErrRunScheduleLimit = errors.New("store: the owner keeps the most run schedules allowed")

// runScheduleListMax bounds one list read.
const runScheduleListMax = 500

// RunSchedule is one run_schedules row.
type RunSchedule struct {
	ID    uuid.UUID
	Owner string
	Name  string
	// Every is "daily", "weekdays" or "weekly"; Weekday (0 Sunday to 6 Saturday) is set for weekly only.
	Every     string
	Weekday   *int
	LocalTime string // HH:MM in TimeZone
	TimeZone  string
	// Request is the create body less start_at and start_time_zone; Authority the identity the schedule
	// acts as, taken at AuthorityAt.
	Request      json.RawMessage
	Authority    json.RawMessage
	AuthorityAt  time.Time
	NextAt       time.Time
	PausedAt     *time.Time
	PausedReason string
	LastDueAt    *time.Time
	LastRunID    *uuid.UUID
	LastOutcome  string
	CreatedAt    time.Time
}

const runScheduleCols = `id, owner, name, every, weekday, local_time, time_zone, request, authority, authority_at,
	next_at, paused_at, paused_reason, last_due_at, last_run_id, last_outcome, created_at`

func scanRunSchedule(row pgx.Row) (RunSchedule, error) {
	var s RunSchedule
	if err := row.Scan(&s.ID, &s.Owner, &s.Name, &s.Every, &s.Weekday, &s.LocalTime, &s.TimeZone, &s.Request, &s.Authority,
		&s.AuthorityAt, &s.NextAt, &s.PausedAt, &s.PausedReason, &s.LastDueAt, &s.LastRunID, &s.LastOutcome, &s.CreatedAt); err != nil {
		return RunSchedule{}, fmt.Errorf("store: scan run schedule: %w", err)
	}
	return s, nil
}

// CreateRunSchedule inserts sc unless its owner already keeps limit schedules, which returns
// ErrRunScheduleLimit. The count and the insert run under one advisory lock per owner, so two creates at
// once cannot both pass the count.
func (s PG) CreateRunSchedule(ctx context.Context, sc RunSchedule, limit int) error {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("store: begin run schedule create: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // a no-op after Commit
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('run_schedules:' || $1))`, sc.Owner); err != nil {
		return fmt.Errorf("store: lock an owner's run schedules: %w", err)
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM run_schedules WHERE owner = $1`, sc.Owner).Scan(&n); err != nil {
		return fmt.Errorf("store: count an owner's run schedules: %w", err)
	}
	if n >= limit {
		return ErrRunScheduleLimit
	}
	if _, err := tx.Exec(ctx, `INSERT INTO run_schedules (id, owner, name, every, weekday, local_time, time_zone, request,
			authority, authority_at, next_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		sc.ID, sc.Owner, sc.Name, sc.Every, sc.Weekday, sc.LocalTime, sc.TimeZone, jsonOrEmpty(sc.Request),
		jsonOrEmpty(sc.Authority), sc.AuthorityAt, sc.NextAt); err != nil {
		return fmt.Errorf("store: insert run schedule: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit run schedule create: %w", err)
	}
	return nil
}

// ListRunSchedules is owner's schedules, or every schedule when owner is "" (the admin view), soonest
// first, at most runScheduleListMax.
func (s PG) ListRunSchedules(ctx context.Context, owner string) ([]RunSchedule, error) {
	return collect(ctx, s.Pool, "list", "run schedules",
		`SELECT `+runScheduleCols+` FROM run_schedules WHERE ($1 = '' OR owner = $1) ORDER BY next_at, id LIMIT $2`,
		[]any{owner, runScheduleListMax}, scanRunSchedule)
}

// GetRunSchedule is one schedule, or ErrNotFound.
func (s PG) GetRunSchedule(ctx context.Context, id uuid.UUID) (RunSchedule, error) {
	sc, err := scanRunSchedule(s.Pool.QueryRow(ctx, `SELECT `+runScheduleCols+` FROM run_schedules WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return RunSchedule{}, ErrNotFound
	}
	return sc, err
}

// PauseRunSchedule pauses an active schedule with reason; false when it is gone or already paused.
func (s PG) PauseRunSchedule(ctx context.Context, id uuid.UUID, reason string, at time.Time) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `UPDATE run_schedules SET paused_at = $3, paused_reason = $2 WHERE id = $1 AND paused_at IS NULL`,
		id, reason, at)
	if err != nil {
		return false, fmt.Errorf("store: pause run schedule: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// ResumeRunSchedule makes a paused schedule active with a fresh authority and its next start; false when
// it is gone or not paused.
func (s PG) ResumeRunSchedule(ctx context.Context, id uuid.UUID, authority json.RawMessage, authorityAt, next time.Time) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `UPDATE run_schedules SET paused_at = NULL, paused_reason = '', authority = $2,
			authority_at = $3, next_at = $4 WHERE id = $1 AND paused_at IS NOT NULL`,
		id, jsonOrEmpty(authority), authorityAt, next)
	if err != nil {
		return false, fmt.Errorf("store: resume run schedule: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// DeleteRunSchedule deletes one schedule; false when there was none.
func (s PG) DeleteRunSchedule(ctx context.Context, id uuid.UUID) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM run_schedules WHERE id = $1`, id)
	if err != nil {
		return false, fmt.Errorf("store: delete run schedule: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// DeleteRunSchedulesOf deletes every schedule owner keeps (the run tasks and credentials erasures) and returns how
// many. It is skipped for a role without INSERT on the table (a split-role app role the migration's grant missed),
// which can have written no schedule. A role that can insert but not delete fails the erasure loudly.
func (s PG) DeleteRunSchedulesOf(ctx context.Context, owner string) (int, error) {
	var canInsert bool
	if err := s.Pool.QueryRow(ctx, `SELECT has_table_privilege(current_user, 'run_schedules', 'INSERT')`).Scan(&canInsert); err != nil {
		return 0, fmt.Errorf("store: read the run schedules privilege: %w", err)
	}
	if !canInsert {
		return 0, nil
	}
	tag, err := s.Pool.Exec(ctx, `DELETE FROM run_schedules WHERE owner = $1`, owner)
	if err != nil {
		return 0, fmt.Errorf("store: delete an owner's run schedules: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// DueRunSchedules is the active schedules whose next start is at or before before, soonest first, at most
// limit.
func (s PG) DueRunSchedules(ctx context.Context, before time.Time, limit int) ([]RunSchedule, error) {
	return collect(ctx, s.Pool, "list", "due run schedules",
		`SELECT `+runScheduleCols+` FROM run_schedules WHERE paused_at IS NULL AND next_at <= $1 ORDER BY next_at, id LIMIT $2`,
		[]any{before, limit}, scanRunSchedule)
}

// AdvanceRunSchedule moves a schedule from the occurrence wasNext to next and records that occurrence's
// outcome. It is a compare-and-set on next_at: false when another advance already moved it, so one
// occurrence is never advanced twice.
func (s PG) AdvanceRunSchedule(ctx context.Context, id uuid.UUID, wasNext, next time.Time, runID *uuid.UUID, outcome string) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `UPDATE run_schedules SET next_at = $3, last_due_at = $2, last_run_id = $4, last_outcome = $5
		WHERE id = $1 AND next_at = $2`, id, wasNext, next, runID, outcome)
	if err != nil {
		return false, fmt.Errorf("store: advance run schedule: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// ScheduleOwnerSignedInAt is the database time of principal's latest SSO sign-in to Wardyn itself (the
// principal_groups row of the empty portal, migration 0290); false when none is recorded.
func (s PG) ScheduleOwnerSignedInAt(ctx context.Context, principal string) (time.Time, bool, error) {
	var at time.Time
	err := s.Pool.QueryRow(ctx, `SELECT signed_in_at FROM principal_groups WHERE principal = $1 AND portal = ''`, principal).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store: read a schedule owner's sign-in: %w", err)
	}
	return at, true, nil
}
