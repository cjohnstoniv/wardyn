// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

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

// ErrRunNotDispatching is an insert or claim for a run that is no longer starting (a kill or a stop landed
// first): it makes and changes no machine.
var ErrRunNotDispatching = errors.New("store: the run is no longer starting")

// ErrKeptMachineExpired is a claim of a machine whose expires_at has passed: the API refuses it first, and
// the claim's own WHERE refuses it again.
var ErrKeptMachineExpired = fmt.Errorf("%w: this kept machine has expired", ErrConflict)

// ErrKeptMachineResetUnconfirmed is a resume, not a fresh start, of a machine whose fresh start was asked
// of its Mac and never confirmed: its disk may or may not have been reset.
var ErrKeptMachineResetUnconfirmed = fmt.Errorf("%w: this kept machine's fresh start is unconfirmed", ErrConflict)

// RunKeptMachineStore keeps which kept machine a macOS VM run kept or resumed (agent_runs.kept_machine_id
// and kept_machine_mode), and a fresh start's reset until its Mac confirms it (kept_machines.reset_run_id,
// and reset_unconfirmed for an earlier attempt).
type RunKeptMachineStore interface {
	// CreateKeptMachineForRun is CreateKeptMachine for run m.RunID, and records it on the run with mode
	// "new", in one transaction that first locks the run's row: ErrRunNotDispatching unless the run is
	// still STARTING, so a kill that commits first leaves no machine, and one that commits after finds it.
	CreateKeptMachineForRun(ctx context.Context, m types.KeptMachine, limit int) (types.KeptMachine, error)
	// ClaimKeptMachineForRun is BeginKeptMachineUse for run runID, and records it on the run with mode
	// ("resume" or "fresh"), under the same run lock and errors, plus ErrRunNotDispatching. A resume of a
	// machine with an unconfirmed fresh start is refused in the same compare-and-swap, with
	// ErrKeptMachineResetUnconfirmed; only another fresh start may claim it.
	ClaimKeptMachineForRun(ctx context.Context, id uuid.UUID, owner string, runID uuid.UUID, mode string) (types.KeptMachine, error)
	// SetRunKeptMachine records machine id and mode on run runID; a nil id clears both. ErrNotFound when
	// the run is gone.
	SetRunKeptMachine(ctx context.Context, runID uuid.UUID, id *uuid.UUID, mode string) error
	// RunKeptMachine returns the machine id and mode run runID records, a nil id for none, or ErrNotFound.
	RunKeptMachine(ctx context.Context, runID uuid.UUID) (*uuid.UUID, string, error)
	// MarkKeptMachineResetting records that run runID, which holds machine id, is about to ask its Mac for
	// a fresh start; an earlier run's mark still there becomes reset_unconfirmed. ClearKeptMachineResetting
	// takes back run runID's mark when the Mac was never asked, and leaves an earlier attempt's uncertainty.
	MarkKeptMachineResetting(ctx context.Context, id, runID uuid.UUID) error
	ClearKeptMachineResetting(ctx context.Context, id, runID uuid.UUID) error
	// ResetKeptMachineBase records the base run runID's confirmed fresh start of machine id produced, and
	// clears its reset mark and any earlier uncertainty. ErrNotFound unless that run holds the machine with
	// its mark.
	ResetKeptMachineBase(ctx context.Context, id, runID uuid.UUID, base string, preparedAt time.Time) error
	// KeptMachineResetPending reports whether a fresh start of machine id was asked and never confirmed.
	KeptMachineResetPending(ctx context.Context, id uuid.UUID) (bool, error)
}

var _ RunKeptMachineStore = PG{}

// lockStartingRun locks run runID's row for the transaction, or answers ErrRunNotDispatching when it is
// gone or no longer STARTING. A kill's claim updates the same row first, so the two serialise.
func lockStartingRun(ctx context.Context, q Querier, runID uuid.UUID) error {
	var state string
	err := q.QueryRow(ctx, `SELECT state FROM agent_runs WHERE id = $1 FOR UPDATE`, runID).Scan(&state)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrRunNotDispatching
	case err != nil:
		return fmt.Errorf("store: lock the run of a kept machine: %w", err)
	case state != string(types.RunStarting):
		return ErrRunNotDispatching
	}
	return nil
}

func setRunKeptMachineIn(ctx context.Context, q Querier, runID, id uuid.UUID, mode string) error {
	if _, err := q.Exec(ctx, `UPDATE agent_runs SET kept_machine_id = $2, kept_machine_mode = $3 WHERE id = $1`, runID, id, mode); err != nil {
		return fmt.Errorf("store: set run kept machine: %w", err)
	}
	return nil
}

// CreateKeptMachineForRun — see RunKeptMachineStore. The count and insert are CreateKeptMachine's.
func (s PG) CreateKeptMachineForRun(ctx context.Context, m types.KeptMachine, limit int) (types.KeptMachine, error) {
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
		if err := lockStartingRun(ctx, q, *m.RunID); err != nil {
			return err
		}
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
		if err != nil {
			return err
		}
		return setRunKeptMachineIn(ctx, q, *m.RunID, out.ID, "new")
	})
	if err != nil {
		return types.KeptMachine{}, err
	}
	return out, nil
}

// ClaimKeptMachineForRun — see RunKeptMachineStore. The compare-and-swap is BeginKeptMachineUse's.
func (s PG) ClaimKeptMachineForRun(ctx context.Context, id uuid.UUID, owner string, runID uuid.UUID, mode string) (types.KeptMachine, error) {
	if owner == "" {
		return types.KeptMachine{}, errKeptMachineNoOwner
	}
	var out types.KeptMachine
	err := s.inTx(ctx, func(q Querier) error {
		if err := lockStartingRun(ctx, q, runID); err != nil {
			return err
		}
		m, err := scanKeptMachine(q.QueryRow(ctx, `UPDATE kept_machines SET state = 'in_use', run_id = $3, last_used_at = now()
			WHERE id = $1 AND owner_subject = $2 AND delete_intent_at IS NULL AND expires_at > now()
			  AND (state = 'kept' OR (state IN ('creating', 'in_use') AND run_id = $3))
			  AND ($4::text = 'fresh' OR (reset_run_id IS NULL AND NOT reset_unconfirmed))
			RETURNING `+keptMachineCols, id, owner, runID, mode))
		var pgErr *pgconn.PgError
		switch {
		case errors.As(err, &pgErr) && pgErr.Code == "23505":
			return ErrConflict
		case errors.Is(err, ErrNotFound):
			d, err := scanKeptMachineDeletion(q.QueryRow(ctx, `SELECT `+keptMachineDeletionCols+`
				FROM kept_machines WHERE id = $1 AND owner_subject = $2`, id, owner))
			if err != nil {
				return err
			}
			if d.Machine.State == types.KeptMachineDeleting || d.By != "" {
				return ErrKeptMachineDeleting
			}
			var expired bool
			if err := q.QueryRow(ctx, `SELECT expires_at <= now() FROM kept_machines WHERE id = $1`, id).Scan(&expired); err != nil {
				return fmt.Errorf("store: read kept machine expiry: %w", err)
			}
			if expired {
				return ErrKeptMachineExpired
			}
			var pending bool
			if err := q.QueryRow(ctx, `SELECT `+keptResetPendingExpr+` FROM kept_machines WHERE id = $1`, id).Scan(&pending); err != nil {
				return fmt.Errorf("store: read kept machine reset: %w", err)
			}
			if pending && mode != "fresh" {
				return ErrKeptMachineResetUnconfirmed
			}
			return ErrKeptMachineInUse
		case err != nil:
			return err
		}
		out = m
		return setRunKeptMachineIn(ctx, q, runID, m.ID, mode)
	})
	if err != nil {
		return types.KeptMachine{}, err
	}
	return out, nil
}

// SetRunKeptMachine — see RunKeptMachineStore.
func (s PG) SetRunKeptMachine(ctx context.Context, runID uuid.UUID, id *uuid.UUID, mode string) error {
	var m *string
	if id != nil {
		m = &mode
	}
	return s.execRun(ctx, "set run kept machine", `UPDATE agent_runs SET kept_machine_id = $2, kept_machine_mode = $3 WHERE id = $1`, runID, id, m)
}

// RunKeptMachine — see RunKeptMachineStore.
func (s PG) RunKeptMachine(ctx context.Context, runID uuid.UUID) (*uuid.UUID, string, error) {
	var id *uuid.UUID
	var mode *string
	err := s.Pool.QueryRow(ctx, `SELECT kept_machine_id, kept_machine_mode FROM agent_runs WHERE id = $1`, runID).Scan(&id, &mode)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", fmt.Errorf("store: read run kept machine: %w", err)
	}
	if id == nil || mode == nil {
		return nil, "", nil
	}
	return id, *mode, nil
}

// MarkKeptMachineResetting — see RunKeptMachineStore.
func (s PG) MarkKeptMachineResetting(ctx context.Context, id, runID uuid.UUID) error {
	return keptMachineExec(ctx, s, "mark reset", `UPDATE kept_machines
		SET reset_unconfirmed = reset_unconfirmed OR reset_run_id IS NOT NULL, reset_run_id = $2
		WHERE id = $1 AND run_id = $2 AND state = 'in_use'`, id, runID)
}

// ClearKeptMachineResetting — see RunKeptMachineStore.
func (s PG) ClearKeptMachineResetting(ctx context.Context, id, runID uuid.UUID) error {
	return keptMachineExec(ctx, s, "clear reset", `UPDATE kept_machines SET reset_run_id = NULL
		WHERE id = $1 AND reset_run_id = $2`, id, runID)
}

// ResetKeptMachineBase — see RunKeptMachineStore.
func (s PG) ResetKeptMachineBase(ctx context.Context, id, runID uuid.UUID, base string, preparedAt time.Time) error {
	return keptMachineExec(ctx, s, "reset base", `UPDATE kept_machines SET base = $3, base_prepared_at = $4, reset_run_id = NULL,
		reset_unconfirmed = false
		WHERE id = $1 AND run_id = $2 AND reset_run_id = $2`, id, runID, base, preparedAt)
}

// keptResetPendingExpr is a fresh start asked of the machine's Mac and never confirmed: this attempt's or
// an earlier one's.
const keptResetPendingExpr = `(reset_run_id IS NOT NULL OR reset_unconfirmed)`

// KeptMachineResetPending — see RunKeptMachineStore.
func (s PG) KeptMachineResetPending(ctx context.Context, id uuid.UUID) (bool, error) {
	var pending bool
	err := s.Pool.QueryRow(ctx, `SELECT `+keptResetPendingExpr+` FROM kept_machines WHERE id = $1`, id).Scan(&pending)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("store: read kept machine reset: %w", err)
	}
	return pending, nil
}
