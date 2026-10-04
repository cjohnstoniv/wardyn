// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/db"
)

// Querier is the statement surface a store write needs: satisfied by *pgxpool.Pool (one statement,
// its own implicit transaction) and by pgx.Tx (a statement inside a caller's transaction). A write
// written against Querier runs the same SQL either way, which is what lets a held governance change
// apply inside the decision transaction.
//
// A write that relies on a transaction-scoped lock (an advisory xact lock) must be handed a pgx.Tx:
// on a pool the lock is released as soon as its own statement ends.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// inTx runs fn on one read-committed transaction and commits it when fn returns nil.
func (s PG) inTx(ctx context.Context, fn func(q Querier) error) error {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // a no-op after Commit
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// LockGovernanceTarget takes the transaction-scoped lock for one target of a governance change: the
// direct write to it and the approval of a held change to it serialize on it. parts are the target's
// natural key. q must be a pgx.Tx.
func LockGovernanceTarget(ctx context.Context, q Querier, kind string, parts ...string) error {
	if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock($1, hashtext($2))`,
		db.GovernanceTargetLockClass, kind+"\x1f"+strings.Join(parts, "\x1f")); err != nil {
		return fmt.Errorf("store: lock the %s target: %w", kind, err)
	}
	return nil
}
