// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PgxStore adapts a *pgxpool.Pool to the broker's TxBeginner/Tx surface so the
// transaction logic stays testable behind an interface (no pgxmock dependency).
// It also implements rowsBeginner for the RevokeRun bulk read.
type PgxStore struct {
	Pool *pgxpool.Pool
}

// NewPgxStore wraps a pgx pool for use as the broker's db.
func NewPgxStore(pool *pgxpool.Pool) *PgxStore { return &PgxStore{Pool: pool} }

// Begin starts a real pgx transaction.
func (s *PgxStore) Begin(ctx context.Context) (Tx, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &pgxTx{tx: tx}, nil
}

// MintedJTIs lists non-empty minted_jti for a run's credential approvals.
func (s *PgxStore) MintedJTIs(ctx context.Context, runID uuid.UUID) ([]string, error) {
	rows, err := s.Pool.Query(ctx,
		`SELECT minted_jti FROM approvals
		  WHERE run_id = $1 AND kind = 'credential' AND minted_jti <> ''`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var jti string
		if err := rows.Scan(&jti); err != nil {
			return nil, err
		}
		out = append(out, jti)
	}
	return out, rows.Err()
}

type pgxTx struct{ tx pgx.Tx }

func (t *pgxTx) QueryRow(ctx context.Context, sql string, args ...any) Row {
	return pgxRow{row: t.tx.QueryRow(ctx, sql, args...)}
}

func (t *pgxTx) Exec(ctx context.Context, sql string, args ...any) (int64, error) {
	tag, err := t.tx.Exec(ctx, sql, args...)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (t *pgxTx) Commit(ctx context.Context) error   { return t.tx.Commit(ctx) }
func (t *pgxTx) Rollback(ctx context.Context) error { return t.tx.Rollback(ctx) }

type pgxRow struct{ row pgx.Row }

// Scan translates pgx.ErrNoRows into the broker-internal errNoRow sentinel so
// the SQL layer can branch on it without importing pgx.
func (r pgxRow) Scan(dest ...any) error {
	err := r.row.Scan(dest...)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNoRow
	}
	return err
}
