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
type PgxStore struct {
	Pool *pgxpool.Pool
}

// NewPgxStore wraps a pgx pool for use as the broker's db.
func NewPgxStore(pool *pgxpool.Pool) *PgxStore { return &PgxStore{Pool: pool} }

// Begin starts a real pgx transaction, PINNED TO READ COMMITTED.
//
// THE MINT TRANSACTION IS AN AUDIT-CHAIN WRITER. broker.mint does its work and
// then calls insertAuditEventTx on this same transaction, so the audit row it
// appends is chained inside it — and since migration 0056 the read that decides
// prev_hash happens INSIDE the trigger, i.e. inside this transaction. Under
// REPEATABLE READ the snapshot is taken by the first statement, BEFORE the
// chain's advisory lock is granted, so a mint that queued behind another writer
// reads a head from before that writer committed and chains onto it: two rows
// claiming one predecessor, which store.VerifyAuditChain reports as "a row was
// deleted or reordered" — a permanent false tamper verdict over an untampered
// log. Executed rather than argued: with nothing changed but the isolation
// level, the production mint sequence forked the chain.
//
// The level otherwise came from default_transaction_isolation, a USERSET GUC
// any role can set per-role or per-database with no superuser needed. A
// transaction-level isolation level OVERRIDES it, which is why this is the fix
// rather than a line in a runbook. It is the SAME pin store.InsertAuditEvent
// and db.beginReadCommitted already carry, and this was the one in-tree writer
// left without it — while db.go's boot ERROR and comment already told the
// operator that "Wardyn's own audit writers pin READ COMMITTED per transaction
// and are unaffected".
//
// PINNED HERE RATHER THAN AT THE CALL SITE because this is the only place the
// broker opens a transaction: an interface with a plain Begin() cannot carry
// TxOptions without every fake in the package restating them, and the pin
// belongs with the pool it is a property of.
func (s *PgxStore) Begin(ctx context.Context) (Tx, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
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
	return pgx.CollectRows(rows, pgx.RowTo[string])
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
