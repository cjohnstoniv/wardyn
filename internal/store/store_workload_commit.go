// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrWorkloadBindingGone is returned by SetBoundWorkloadRun when the namespace binding the launch was
// authorised under no longer exists: deleted by hand, with its service identity's retirement or with its
// pool. Nothing is written, so the launch's claim still holds for its refusal.
var ErrWorkloadBindingGone = errors.New("store: the namespace binding the launch was authorised under was deleted")

// SetBoundWorkloadRun is SetWorkloadRun for a custom resource's launch, authorised under the namespace binding
// binding: the record holds only while that binding exists. Its transaction locks the binding row first, FOR
// KEY SHARE, which a delete of the row waits for and which waits for a delete not yet committed. A delete that
// commits first refuses the record (ErrWorkloadBindingGone); a later one waits for the record to commit. Once a
// binding's delete has returned, no launch under it is recorded.
func (s PG) SetBoundWorkloadRun(ctx context.Context, id, claim uuid.UUID, generation int64, runID, binding uuid.UUID) (*uuid.UUID, error) {
	return s.setWorkloadRun(ctx, func(q Querier) error { return lockWorkloadBinding(ctx, q, binding) }, id, claim, generation, runID)
}

// lockWorkloadBinding locks the binding row id FOR KEY SHARE on q until q's transaction ends, the weakest
// lock a delete of the row waits for and that waits for a delete not yet committed; ErrWorkloadBindingGone
// once the row is deleted.
func lockWorkloadBinding(ctx context.Context, q Querier, id uuid.UUID) error {
	var one int
	err := q.QueryRow(ctx, `SELECT 1 FROM workload_bindings WHERE id = $1 FOR KEY SHARE`, id).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrWorkloadBindingGone
	}
	return err
}

// GetWorkloadBinding returns the binding id, or ErrNotFound once it is deleted.
func (s PG) GetWorkloadBinding(ctx context.Context, id uuid.UUID) (WorkloadBinding, error) {
	return scanWorkloadBinding(s.Pool.QueryRow(ctx, `SELECT `+workloadBindingCols+` FROM workload_bindings b WHERE b.id = $1`, id))
}

// WorkloadHoldsRun reports whether owner's workload name holds runID as its current run (held); false when
// there is no such workload. The row is found by its key and read under a share lock, so a record of the run
// still committing (SetWorkloadRun holds the row FOR UPDATE until it commits or rolls back) is waited for and
// its outcome is what is read, never the row as it was before a commit that then lands. A caller whose record
// answered an error that may come from a commit that landed reads the outcome back with it.
//
// deferred, when runID is not held, reports that runID was the workload's and a later record replaced it with
// a run that waits in its queue: runID is owed an end that waits on that run (after_run_id), so ending it now
// would take the service down before its replacement starts. It is a second statement under the same lock,
// which a record replacing the run needs FOR UPDATE, so it sees every record committed before the lock and
// none after; one statement would not, since its snapshot of workload_run_ends predates the lock wait.
func (s PG) WorkloadHoldsRun(ctx context.Context, owner, name string, runID uuid.UUID) (held, deferred bool, err error) {
	err = s.inTx(ctx, func(q Querier) error {
		var cur *uuid.UUID
		err := q.QueryRow(ctx, `SELECT current_run_id FROM workloads WHERE owner = $1 AND name = $2 FOR SHARE`, owner, name).Scan(&cur)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if held = cur != nil && *cur == runID; held {
			return nil
		}
		return q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM workload_run_ends WHERE run_id = $1 AND owner = $2 AND name = $3 AND after_run_id IS NOT NULL)`,
			runID, owner, name).Scan(&deferred)
	})
	if err != nil {
		return false, false, fmt.Errorf("store: read a workload's current run: %w", err)
	}
	return held, deferred, nil
}

// DropWorkloadRunEndIfHeld drops runID's owed end, one that waits on no replacement, when owner's workload
// name holds runID as its current run, and reports whether it does. The workload row stays locked FOR
// SHARE until the drop commits, so a record that replaces the run (it locks the row FOR UPDATE and owes the
// run its end) lands wholly before the read or wholly after the drop: the run is never left neither the
// workload's current run nor owed its end.
func (s PG) DropWorkloadRunEndIfHeld(ctx context.Context, owner, name string, runID uuid.UUID) (bool, error) {
	held := false
	err := s.inTx(ctx, func(q Querier) error {
		var cur *uuid.UUID
		err := q.QueryRow(ctx, `SELECT current_run_id FROM workloads WHERE owner = $1 AND name = $2 FOR SHARE`, owner, name).Scan(&cur)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil || cur == nil || *cur != runID {
			return err
		}
		held = true
		_, err = q.Exec(ctx, `DELETE FROM workload_run_ends WHERE run_id = $1 AND after_run_id IS NULL`, runID)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("store: drop a held workload run's owed end: %w", err)
	}
	return held, nil
}
