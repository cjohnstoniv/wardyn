// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
