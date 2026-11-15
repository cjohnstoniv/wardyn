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
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RunWriteStatementTimeout is the server-side limit on each statement of a run-row write that a holder of the run's
// lock makes (inRunWriteTx, setRunWriteTimeout). Past it the database ends the statement and the transaction rolls
// back (SQLSTATE 57014), so a write stuck behind a row lock cannot hold the run's lock. It does not settle a COMMIT:
// one sent and not answered may still commit, and answers ErrRunWriteUnsettled. A variable only so a test can
// shorten it.
var RunWriteStatementTimeout = 10 * time.Second

// RunWriteRollbackBound bounds the rollback of a run write that failed before its COMMIT, on a context of its own:
// the caller's may be done or have no deadline. A rollback not answered by then closes the connection, and the
// database rolls the transaction back when it sees the connection gone. A variable only so a test can shorten it.
var RunWriteRollbackBound = 5 * time.Second

// ErrRunWriteUnsettled is a run write whose COMMIT was sent and not answered: it may have committed or not. Its
// outcome is read behind the run's row lock (SettleRunState), never from an unlocked read, which can still see the
// old row while the COMMIT is pending.
var ErrRunWriteUnsettled = errors.New("store: the run write's commit was sent and not answered")

// setRunWriteTimeout sets RunWriteStatementTimeout on q's transaction (SET LOCAL), for the statements after it. q
// must be a pgx.Tx: on a pool the setting would outlive the statement on a connection another caller gets next.
func setRunWriteTimeout(ctx context.Context, q Querier) error {
	ms := max(RunWriteStatementTimeout.Milliseconds(), 1)
	if _, err := q.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", ms)); err != nil {
		return fmt.Errorf("store: set the run write's statement timeout: %w", err)
	}
	return nil
}

// rollbackRunWrite is the deferred rollback of a run write's transaction, on RunWriteRollbackBound: a no-op after a
// COMMIT was sent, answered or not.
func rollbackRunWrite(ctx context.Context, tx pgx.Tx) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), RunWriteRollbackBound)
	defer cancel()
	_ = tx.Rollback(rctx)
}

// commitRunWrite commits a run write's transaction. A COMMIT that never left the client (pgconn.SafeToRetry) committed
// nothing; any other error may have committed, and wraps ErrRunWriteUnsettled.
func commitRunWrite(ctx context.Context, tx pgx.Tx) error {
	err := tx.Commit(ctx)
	if err != nil && !pgconn.SafeToRetry(err) {
		return fmt.Errorf("%w: %w", ErrRunWriteUnsettled, err)
	}
	return err
}

// inRunWriteTx is inTx for a run write a holder of the run's lock makes: setRunWriteTimeout first, the rollback on
// its own bound, and the COMMIT's error sorted by commitRunWrite.
func (s PG) inRunWriteTx(ctx context.Context, fn func(q Querier) error) error {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer rollbackRunWrite(ctx, tx)
	if err := setRunWriteTimeout(ctx, tx); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return commitRunWrite(ctx, tx)
}

// execRunWrite is Pool.Exec for one run-row write a holder of the run's lock makes, in its own inRunWriteTx.
func (s PG) execRunWrite(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	var tag pgconn.CommandTag
	err := s.inRunWriteTx(ctx, func(q Querier) error {
		var err error
		tag, err = q.Exec(ctx, sql, args...)
		return err
	})
	return tag, err
}

// SettleRunState reads run id's state behind its row lock (FOR SHARE), under RunWriteStatementTimeout. A write whose
// UPDATE took that lock holds it until its transaction ends, so the read waits out an unsettled COMMIT and reads its
// outcome; a transaction still open at the limit answers 57014 and settles nothing.
func (s PG) SettleRunState(ctx context.Context, id uuid.UUID) (types.RunState, error) {
	var state string
	err := s.inRunWriteTx(ctx, func(q Querier) error {
		return q.QueryRow(ctx, `SELECT state FROM agent_runs WHERE id=$1 FOR SHARE`, id).Scan(&state)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return types.RunState(state), err
}
