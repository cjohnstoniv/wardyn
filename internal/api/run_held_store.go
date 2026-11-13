// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// heldWriteBound bounds one run-row write a holder of a run's lock makes: a margin past the database's own limit on
// each of its statements (store.RunWriteStatementTimeout), so a statement the database is still running is not
// given up on first. It does not stop a COMMIT already sent (store.ErrRunWriteUnsettled). A variable only so a test
// can shorten it.
var heldWriteBound = store.RunWriteStatementTimeout + 5*time.Second

// heldRead runs one read a holder of a run's lock makes, on heldStoreCallBound: the holder's context may have no
// deadline (the completion watcher, a person's request, a sweep's tick), and the lock is released only when the
// read returns.
func heldRead[T any](ctx context.Context, read func(context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, heldStoreCallBound)
	defer cancel()
	return read(ctx)
}

// heldWrite is heldRead for a run-row write the store runs under store.RunWriteStatementTimeout, on heldWriteBound;
// the store's rollback of a failed write has a bound of its own (store.RunWriteRollbackBound). A write whose COMMIT
// was sent and not answered may still commit (store.ErrRunWriteUnsettled); a caller that acts on the outcome settles
// it (casRunStateHeld), and the others answer the error and are retried or re-derived.
func heldWrite[T any](ctx context.Context, write func(context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, heldWriteBound)
	defer cancel()
	return write(ctx)
}

// getRunHeld is GetRun for a holder of the run's lock, on heldStoreCallBound.
func (s *Server) getRunHeld(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	return heldRead(ctx, func(ctx context.Context) (types.AgentRun, error) { return s.cfg.Store.GetRun(ctx, id) })
}

// recordAuditHeld is recordAudit for a holder of the run's lock, on heldStoreCallBound. A row its bound cuts goes to
// the audit spool as any failed row does.
func (s *Server) recordAuditHeld(ctx context.Context, ev types.AuditEvent) {
	heldCall(ctx, heldStoreCallBound, func(ctx context.Context) { s.recordAudit(ctx, ev) })
}

// runStateSettler is the store's settle of an unsettled state write (store.PG.SettleRunState).
type runStateSettler interface {
	SettleRunState(ctx context.Context, id uuid.UUID) (types.RunState, error)
}

// settleRunWrite answers a held write that moves run runID from from to to, given its (applied, err): an error before
// the COMMIT committed nothing and is answered as it is. A COMMIT sent and not answered (store.ErrRunWriteUnsettled)
// may still commit, so it is settled behind the run's row lock, which the write holds until it ends: a run now in to
// reads as applied (or another writer's move to the same state, whose tail this one then runs again), a run still in
// from is an error, any other state is a move someone else won, and a settle that fails keeps the error. An
// unsettled write is never answered as not applied.
func (s *Server) settleRunWrite(ctx context.Context, runID uuid.UUID, from, to types.RunState, applied bool, err error) (bool, error) {
	settler, ok := s.cfg.Store.(runStateSettler)
	if !errors.Is(err, store.ErrRunWriteUnsettled) || !ok {
		return applied && err == nil, err
	}
	cur, serr := heldWrite(ctx, func(ctx context.Context) (types.RunState, error) { return settler.SettleRunState(ctx, runID) })
	switch {
	case serr != nil:
		return false, fmt.Errorf("%w (settle: %v)", err, serr)
	case cur == to:
		return true, nil
	case cur == from:
		return false, fmt.Errorf("the state write did not commit (settled behind the run's row lock): %w", err)
	default:
		return false, nil
	}
}

// casRunStateHeld is casRunState for a holder of the run's lock whose context may have no deadline, on
// heldWriteBound, its outcome settled by settleRunWrite.
func (s *Server) casRunStateHeld(ctx context.Context, runID uuid.UUID, from, to types.RunState) (bool, error) {
	applied, err := heldWrite(ctx, func(ctx context.Context) (bool, error) {
		return s.cfg.Store.UpdateRunStateIf(ctx, runID, from, to)
	})
	if applied, err = s.settleRunWrite(ctx, runID, from, to, applied, err); err != nil {
		return false, err
	}
	s.runStateMoved(runID, from, to, applied)
	return applied, nil
}
