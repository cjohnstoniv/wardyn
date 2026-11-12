// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Kept machines: one person's macOS guest disk, kept on that person's own Mac
// between runs. owner_subject is written once, on INSERT; no statement here
// updates it, and every statement a person's request reaches carries it in its
// WHERE clause, so no race can move a machine between people.
package store

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ErrKeptMachineLimit is returned by CreateKeptMachine when the owner already
// keeps the most machines allowed. Rows being deleted still count.
var ErrKeptMachineLimit = errors.New("store: the most kept machines one person may hold already exist")

// ErrKeptMachineNameTaken is returned by CreateKeptMachine when the owner
// already keeps a machine of that name, compared case-blind.
var ErrKeptMachineNameTaken = fmt.Errorf("%w: you already keep a machine with this name", ErrConflict)

// ErrKeptMachineInUse is returned when a run holds the machine: it is in use,
// or a keep run that has not started yet created it.
var ErrKeptMachineInUse = fmt.Errorf("%w: a run holds this kept machine", ErrConflict)

// ErrKeptMachineDeleting is returned when the machine is being deleted, or a
// kill has recorded that it will be.
var ErrKeptMachineDeleting = fmt.Errorf("%w: this kept machine is being deleted", ErrConflict)

// errKeptMachineNoOwner refuses an empty owner, a caller bug, never a person.
var errKeptMachineNoOwner = errors.New("store: a kept machine needs an owner")

// errKeptMachineNoBy refuses a delete or a delete intent that does not say who.
var errKeptMachineNoBy = errors.New("store: a kept machine delete needs a by")

// keptMachineNameIndex is the migration's per-owner unique name index.
const keptMachineNameIndex = "kept_machines_owner_name_uniq"

// keptMachineTakeover is how old a delete tombstone must be before another
// delete may take it over (a crashed delete must not pin the machine forever).
const keptMachineTakeover = "60 seconds"

const keptMachineCols = `id, name, owner_subject, runner_id, base, base_prepared_at, state, run_id, scrubbed,
	disk_bytes, created_at, last_used_at, expires_at, delete_started_at`

// KeptMachineDeletion is a machine with a delete started or intended: By and
// Killer say who (Killer only for a kill), At is the tombstone's
// delete_started_at, or the intent's time for a pending intent.
type KeptMachineDeletion struct {
	Machine types.KeptMachine
	By      string
	Killer  string
	At      time.Time
}

// KeptMachineStop is the runner's stop report for the run that held a machine.
type KeptMachineStop struct {
	ID        uuid.UUID
	RunnerID  uuid.UUID
	RunID     uuid.UUID
	Scrubbed  bool
	DiskBytes int64
	ExpiresAt time.Time
}

// KeptMachineFilter narrows the admin list: an empty Owner or a nil RunnerID
// matches every one.
type KeptMachineFilter struct {
	Owner    string
	RunnerID uuid.UUID
}

func keptMachineDest(m *types.KeptMachine, state *string) []any {
	return []any{&m.ID, &m.Name, &m.OwnerSubject, &m.RunnerID, &m.Base, &m.BasePreparedAt, state, &m.RunID,
		&m.Scrubbed, &m.DiskBytes, &m.CreatedAt, &m.LastUsedAt, &m.ExpiresAt, &m.DeleteStartedAt}
}

func scanKeptMachine(row pgx.Row) (types.KeptMachine, error) {
	var m types.KeptMachine
	var state string
	err := row.Scan(keptMachineDest(&m, &state)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.KeptMachine{}, ErrNotFound
	}
	if err != nil {
		return types.KeptMachine{}, fmt.Errorf("store: scan kept machine: %w", err)
	}
	m.State = types.KeptMachineState(state)
	return m, nil
}

// keptMachineDeletionCols reads a row as a KeptMachineDeletion: the tombstone's
// by, killer and start when it has one, else the intent's.
const keptMachineDeletionCols = keptMachineCols + `,
	CASE WHEN state = 'deleting' THEN delete_by ELSE delete_intent_by END,
	CASE WHEN state = 'deleting' THEN delete_killer ELSE delete_intent_killer END,
	COALESCE(delete_started_at, delete_intent_at)`

func scanKeptMachineDeletion(row pgx.Row) (KeptMachineDeletion, error) {
	var d KeptMachineDeletion
	var state string
	var at *time.Time // NULL on a row with no delete started or intended
	err := row.Scan(append(keptMachineDest(&d.Machine, &state), &d.By, &d.Killer, &at)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return KeptMachineDeletion{}, ErrNotFound
	}
	if err != nil {
		return KeptMachineDeletion{}, fmt.Errorf("store: scan kept machine delete: %w", err)
	}
	d.Machine.State = types.KeptMachineState(state)
	if at != nil {
		d.At = *at
	}
	return d, nil
}

// CreateKeptMachine inserts m in state creating for the keep run m.RunID,
// unless owner m.OwnerSubject already keeps limit machines. The count and the
// insert run in one READ COMMITTED transaction under the owner's
// KeptMachineLockClass lock. The caller sets ExpiresAt; created_at and
// last_used_at are the database's now.
func (s PG) CreateKeptMachine(ctx context.Context, m types.KeptMachine, limit int) (types.KeptMachine, error) {
	if m.OwnerSubject == "" {
		return types.KeptMachine{}, errKeptMachineNoOwner
	}
	if m.RunID == nil {
		return types.KeptMachine{}, errors.New("store: a kept machine is created for a run")
	}
	if m.ID == uuid.Nil {
		m.ID = uuid.New()
	}
	var out types.KeptMachine
	err := s.inTx(ctx, func(q Querier) error {
		if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock($1, $2)`,
			db.KeptMachineLockClass, int32(crc32.ChecksumIEEE([]byte(m.OwnerSubject)))); err != nil {
			return fmt.Errorf("store: lock kept machines: %w", err)
		}
		var n int
		if err := q.QueryRow(ctx, `SELECT count(*) FROM kept_machines WHERE owner_subject = $1`, m.OwnerSubject).Scan(&n); err != nil {
			return fmt.Errorf("store: count kept machines: %w", err)
		}
		if n >= limit {
			return ErrKeptMachineLimit
		}
		var err error
		out, err = scanKeptMachine(q.QueryRow(ctx, `
			INSERT INTO kept_machines (id, name, owner_subject, runner_id, base, base_prepared_at, state, run_id, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, 'creating', $7, $8)
			RETURNING `+keptMachineCols,
			m.ID, m.Name, m.OwnerSubject, m.RunnerID, m.Base, m.BasePreparedAt, *m.RunID, m.ExpiresAt))
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			if pgErr.ConstraintName == keptMachineNameIndex {
				return ErrKeptMachineNameTaken
			}
			return ErrConflict
		}
		return err
	})
	if err != nil {
		return types.KeptMachine{}, err
	}
	return out, nil
}

// GetKeptMachine returns machine id whoever owns it (the admin routes), or ErrNotFound.
func (s PG) GetKeptMachine(ctx context.Context, id uuid.UUID) (types.KeptMachine, error) {
	return scanKeptMachine(s.Pool.QueryRow(ctx, `SELECT `+keptMachineCols+` FROM kept_machines WHERE id = $1`, id))
}

// FindKeptMachineOf returns owner's machine whose id or name (case-blind) is
// ref, the id first, else ErrNotFound: another person's machine is not found.
func (s PG) FindKeptMachineOf(ctx context.Context, owner, ref string) (types.KeptMachine, error) {
	if owner == "" {
		return types.KeptMachine{}, errKeptMachineNoOwner
	}
	return scanKeptMachine(s.Pool.QueryRow(ctx, `SELECT `+keptMachineCols+` FROM kept_machines
		WHERE owner_subject = $1 AND (id::text = lower($2) OR lower(name) = lower($2))
		ORDER BY (id::text = lower($2)) DESC LIMIT 1`, owner, ref))
}

// GetKeptMachineByRun returns the machine run runID has or last had, or ErrNotFound.
func (s PG) GetKeptMachineByRun(ctx context.Context, runID uuid.UUID) (types.KeptMachine, error) {
	return scanKeptMachine(s.Pool.QueryRow(ctx, `SELECT `+keptMachineCols+` FROM kept_machines WHERE run_id = $1`, runID))
}

// ListKeptMachinesOf returns owner's machines by name, tombstoned ones included.
func (s PG) ListKeptMachinesOf(ctx context.Context, owner string) ([]types.KeptMachine, error) {
	if owner == "" {
		return nil, errKeptMachineNoOwner
	}
	return collect(ctx, s.Pool, "list", "kept machines",
		`SELECT `+keptMachineCols+` FROM kept_machines WHERE owner_subject = $1 ORDER BY lower(name), id`,
		[]any{owner}, scanKeptMachine)
}

// ListKeptMachines returns one page of every person's machines that f matches,
// by owner then name, and the total that f matches.
func (s PG) ListKeptMachines(ctx context.Context, f KeptMachineFilter, p Page) ([]types.KeptMachine, int, error) {
	var runner any
	if f.RunnerID != uuid.Nil {
		runner = f.RunnerID
	}
	const where = ` FROM kept_machines WHERE ($1::text = '' OR owner_subject = $1) AND ($2::uuid IS NULL OR runner_id = $2)`
	var total int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*)`+where, f.Owner, runner).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: count kept machines: %w", err)
	}
	q, args := p.appendTo(`SELECT `+keptMachineCols+where+` ORDER BY owner_subject, lower(name), id`, []any{f.Owner, runner})
	out, err := collect(ctx, s.Pool, "list", "kept machines", q, args, scanKeptMachine)
	return out, total, err
}

// BeginKeptMachineUse gives owner's machine id to run runID: a kept machine
// (a resume), or the machine runID's own keep created. It is one
// compare-and-swap on state, owner and run, so of two runs racing for one
// machine exactly one gets it, and a caller who does not own the machine
// changes nothing. Again for the run that holds it, it is a no-op. A machine
// with a delete started or intended is never given out.
// Errors: ErrNotFound (no such machine of owner's), ErrKeptMachineDeleting,
// ErrKeptMachineInUse (another run holds it), ErrConflict (runID holds another).
func (s PG) BeginKeptMachineUse(ctx context.Context, id uuid.UUID, owner string, runID uuid.UUID) (types.KeptMachine, error) {
	if owner == "" {
		return types.KeptMachine{}, errKeptMachineNoOwner
	}
	m, err := scanKeptMachine(s.Pool.QueryRow(ctx, `UPDATE kept_machines SET state = 'in_use', run_id = $3, last_used_at = now()
		WHERE id = $1 AND owner_subject = $2 AND delete_intent_at IS NULL
		  AND (state = 'kept' OR (state IN ('creating', 'in_use') AND run_id = $3))
		RETURNING `+keptMachineCols, id, owner, runID))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return types.KeptMachine{}, ErrConflict
	}
	if !errors.Is(err, ErrNotFound) {
		return m, err
	}
	// No row: told apart only on the failing path, under the same owner predicate.
	d, err := scanKeptMachineDeletion(s.Pool.QueryRow(ctx, `SELECT `+keptMachineDeletionCols+`
		FROM kept_machines WHERE id = $1 AND owner_subject = $2`, id, owner))
	if err != nil {
		return types.KeptMachine{}, err
	}
	if d.Machine.State == types.KeptMachineDeleting || d.By != "" { // tombstoned, or a kill's intent
		return types.KeptMachine{}, ErrKeptMachineDeleting
	}
	return types.KeptMachine{}, ErrKeptMachineInUse
}

// RecordKeptMachineStop records runner s.RunnerID's stop report for run
// s.RunID: the machine that run holds is kept, with the report's scrub, size
// and expiry, and last used now. ErrNotFound when that run does not hold the
// machine on that runner (another run has it, it is being deleted, or the
// report came already).
func (s PG) RecordKeptMachineStop(ctx context.Context, st KeptMachineStop) (types.KeptMachine, error) {
	return scanKeptMachine(s.Pool.QueryRow(ctx, `UPDATE kept_machines
		SET state = 'kept', scrubbed = $4, disk_bytes = $5, expires_at = $6, last_used_at = now()
		WHERE id = $1 AND runner_id = $2 AND run_id = $3 AND state IN ('creating', 'in_use')
		RETURNING `+keptMachineCols, st.ID, st.RunnerID, st.RunID, st.Scrubbed, st.DiskBytes, st.ExpiresAt))
}

// DropUnstartedKeptMachine removes the machine run runID's keep created while
// it is still creating: the run ended before its machine was made. ErrNotFound
// otherwise.
func (s PG) DropUnstartedKeptMachine(ctx context.Context, id, runID uuid.UUID) error {
	return keptMachineExec(ctx, s, "drop", `DELETE FROM kept_machines WHERE id = $1 AND run_id = $2 AND state = 'creating'`, id, runID)
}

// BeginKeptMachineDelete tombstones a kept machine of owner's (an empty owner
// means any owner) for a delete by by, and returns the tombstone. A tombstone
// older than a minute is taken over and keeps its first by and killer.
// Errors: ErrNotFound (not owner's), ErrKeptMachineInUse (a run holds it),
// ErrKeptMachineDeleting (another delete holds it, or a kill intends one).
func (s PG) BeginKeptMachineDelete(ctx context.Context, owner string, id uuid.UUID, by string) (KeptMachineDeletion, error) {
	if by == "" {
		return KeptMachineDeletion{}, errKeptMachineNoBy
	}
	d, err := scanKeptMachineDeletion(s.Pool.QueryRow(ctx, `UPDATE kept_machines SET
		  delete_by = CASE WHEN state = 'deleting' THEN delete_by ELSE $3 END,
		  state = 'deleting', delete_started_at = now()
		WHERE id = $1 AND ($2::text = '' OR owner_subject = $2)
		  AND ((state = 'kept' AND delete_intent_at IS NULL)
		    OR (state = 'deleting' AND delete_started_at < now() - interval '`+keptMachineTakeover+`'))
		RETURNING `+keptMachineDeletionCols, id, owner, by))
	if !errors.Is(err, ErrNotFound) {
		return d, err
	}
	cur, err := scanKeptMachineDeletion(s.Pool.QueryRow(ctx, `SELECT `+keptMachineDeletionCols+`
		FROM kept_machines WHERE id = $1 AND ($2::text = '' OR owner_subject = $2)`, id, owner))
	if err != nil {
		return KeptMachineDeletion{}, err
	}
	if cur.Machine.State == types.KeptMachineDeleting || cur.By != "" { // tombstoned, or a kill's intent
		return KeptMachineDeletion{}, ErrKeptMachineDeleting
	}
	return KeptMachineDeletion{}, ErrKeptMachineInUse
}

// AbortKeptMachineDelete puts a machine whose delete failed back to kept,
// only while it carries the tombstone started. A kill's tombstone is never
// aborted: that machine is deleted whatever it takes. ErrNotFound otherwise.
func (s PG) AbortKeptMachineDelete(ctx context.Context, id uuid.UUID, started time.Time) error {
	return keptMachineExec(ctx, s, "abort", `UPDATE kept_machines
		SET state = 'kept', delete_started_at = NULL, delete_by = '', delete_killer = ''
		WHERE id = $1 AND delete_started_at = $2 AND delete_intent_at IS NULL`, id, started)
}

// FinishKeptMachineDelete removes the row while it still carries the
// tombstone started. ErrNotFound when it does not.
func (s PG) FinishKeptMachineDelete(ctx context.Context, id uuid.UUID, started time.Time) error {
	return keptMachineExec(ctx, s, "finish", `DELETE FROM kept_machines WHERE id = $1 AND delete_started_at = $2`, id, started)
}

// ListKeptMachineDeletes returns every tombstoned machine, the deletes that
// wait for their Mac or were cut short, which the Mac's reconnect and the
// sweep finish.
func (s PG) ListKeptMachineDeletes(ctx context.Context) ([]KeptMachineDeletion, error) {
	return collect(ctx, s.Pool, "list", "kept machine deletes",
		`SELECT `+keptMachineDeletionCols+` FROM kept_machines WHERE state = 'deleting' ORDER BY delete_started_at, id`,
		nil, scanKeptMachineDeletion)
}

// ListExpiredKeptMachines returns the kept machines past their expiry with no
// delete started or intended, oldest first.
func (s PG) ListExpiredKeptMachines(ctx context.Context) ([]types.KeptMachine, error) {
	return collect(ctx, s.Pool, "list", "expired kept machines",
		`SELECT `+keptMachineCols+` FROM kept_machines WHERE state = 'kept' AND delete_intent_at IS NULL AND expires_at <= now()
		ORDER BY expires_at, id`, nil, scanKeptMachine)
}

// SetKeptMachineDeleteIntent records, on the machine run runID has or last
// had, that a kill by killer (by is the delete's by) will delete it. A kill
// writes it before it claims the run, so a replica that dies between the two
// leaves the intent for the reconcile sweep. Idempotent: a machine that has an
// intent keeps the first one, which is returned. ErrNotFound when the run has
// no machine; ErrKeptMachineDeleting when a delete has already started.
func (s PG) SetKeptMachineDeleteIntent(ctx context.Context, runID uuid.UUID, by, killer string) (KeptMachineDeletion, error) {
	if by == "" {
		return KeptMachineDeletion{}, errKeptMachineNoBy
	}
	d, err := scanKeptMachineDeletion(s.Pool.QueryRow(ctx, `UPDATE kept_machines
		SET delete_intent_at = now(), delete_intent_by = $2, delete_intent_killer = $3
		WHERE run_id = $1 AND state <> 'deleting' AND delete_intent_at IS NULL
		RETURNING `+keptMachineDeletionCols, runID, by, killer))
	if !errors.Is(err, ErrNotFound) {
		return d, err
	}
	cur, err := scanKeptMachineDeletion(s.Pool.QueryRow(ctx, `SELECT `+keptMachineDeletionCols+` FROM kept_machines WHERE run_id = $1`, runID))
	switch {
	case err != nil:
		return KeptMachineDeletion{}, err
	case cur.Machine.State == types.KeptMachineDeleting:
		return KeptMachineDeletion{}, ErrKeptMachineDeleting
	}
	return cur, nil
}

// KillRunMarkingKeptMachine is a kill's claim, run runID from fromState to
// KILLED, and, only when the claim lands, that kill's delete intent on the
// machine the run has or last had, in one transaction: a kill that ends the run
// always leaves its delete behind for the reconcile sweep, and one that loses
// leaves none. A machine exempt owns (the killer's own) or one a delete already
// has gets no intent, and the deletion is zero. fromState is never QUEUED: a
// queued run leaves through LeaveRunQueue and holds no booted machine.
func (s PG) KillRunMarkingKeptMachine(ctx context.Context, runID uuid.UUID, fromState types.RunState, by, killer, exempt string) (bool, KeptMachineDeletion, error) {
	if fromState == types.RunQueued {
		return false, KeptMachineDeletion{}, errQueuedLeave
	}
	if by == "" {
		return false, KeptMachineDeletion{}, errKeptMachineNoBy
	}
	var applied bool
	var d KeptMachineDeletion
	err := s.inTx(ctx, func(q Querier) error {
		tag, err := q.Exec(ctx, `UPDATE agent_runs SET state = $1, updated_at = now(), ended_at = now() WHERE id = $2 AND state = $3`,
			string(types.RunKilled), runID, string(fromState))
		if err != nil {
			return fmt.Errorf("store: claim a run's kill: %w", err)
		}
		if applied = tag.RowsAffected() > 0; !applied {
			return nil
		}
		d, err = scanKeptMachineDeletion(q.QueryRow(ctx, `UPDATE kept_machines
			SET delete_intent_at = now(), delete_intent_by = $2, delete_intent_killer = $3
			WHERE run_id = $1 AND state <> 'deleting' AND delete_intent_at IS NULL AND owner_subject <> $4
			RETURNING `+keptMachineDeletionCols, runID, by, killer, exempt))
		if errors.Is(err, ErrNotFound) {
			d, err = KeptMachineDeletion{}, nil
		}
		return err
	})
	if err != nil {
		return false, KeptMachineDeletion{}, err
	}
	return applied, d, nil
}

// ClearKeptMachineDeleteIntent drops the intent set at at, before any delete
// started from it: the kill it was written for did not happen. ErrNotFound
// when that intent is gone or a delete has started.
func (s PG) ClearKeptMachineDeleteIntent(ctx context.Context, id uuid.UUID, at time.Time) error {
	return keptMachineExec(ctx, s, "clear intent", `UPDATE kept_machines
		SET delete_intent_at = NULL, delete_intent_by = '', delete_intent_killer = ''
		WHERE id = $1 AND delete_intent_at = $2 AND state <> 'deleting'`, id, at)
}

// ListKeptMachineDeleteIntents returns every intent no delete has started from
// yet, oldest first, for the reconcile sweep: it starts the delete when the
// run was killed, and clears the intent when the kill never claimed it.
func (s PG) ListKeptMachineDeleteIntents(ctx context.Context) ([]KeptMachineDeletion, error) {
	return collect(ctx, s.Pool, "list", "kept machine delete intents",
		`SELECT `+keptMachineDeletionCols+` FROM kept_machines WHERE delete_intent_at IS NOT NULL AND state <> 'deleting'
		ORDER BY delete_intent_at, id`, nil, scanKeptMachineDeletion)
}

// BeginKeptMachineDeleteForKill tombstones the machine from its intent set at
// at, in any state, with the intent's by and killer. The intent stays on the
// row, so AbortKeptMachineDelete never undoes it. Idempotent: a tombstone
// already started from that intent is returned. ErrNotFound when the machine
// has no such intent.
func (s PG) BeginKeptMachineDeleteForKill(ctx context.Context, id uuid.UUID, at time.Time) (KeptMachineDeletion, error) {
	d, err := scanKeptMachineDeletion(s.Pool.QueryRow(ctx, `UPDATE kept_machines SET state = 'deleting', delete_started_at = now(),
		  delete_by = delete_intent_by, delete_killer = delete_intent_killer
		WHERE id = $1 AND delete_intent_at = $2 AND state <> 'deleting'
		RETURNING `+keptMachineDeletionCols, id, at))
	if !errors.Is(err, ErrNotFound) {
		return d, err
	}
	return scanKeptMachineDeletion(s.Pool.QueryRow(ctx, `SELECT `+keptMachineDeletionCols+`
		FROM kept_machines WHERE id = $1 AND delete_intent_at = $2 AND state = 'deleting'`, id, at))
}

// CloseKeptMachinesOfRunner removes every machine row of a revoked runner and
// returns them: the runner erases the disks on its side, so the organisation
// only closes its records. A runner that is not revoked closes nothing.
func (s PG) CloseKeptMachinesOfRunner(ctx context.Context, runnerID uuid.UUID) ([]types.KeptMachine, error) {
	return collect(ctx, s.Pool, "close", "kept machines of runner", `DELETE FROM kept_machines
		WHERE runner_id = $1 AND EXISTS (SELECT 1 FROM runners WHERE id = $1 AND revoked_at IS NOT NULL)
		RETURNING `+keptMachineCols, []any{runnerID}, scanKeptMachine)
}

func keptMachineExec(ctx context.Context, s PG, verb, q string, args ...any) error {
	tag, err := s.Pool.Exec(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("store: %s kept machine: %w", verb, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
