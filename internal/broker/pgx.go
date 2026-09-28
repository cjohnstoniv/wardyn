// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"errors"
	"fmt"

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

// BeginReadCommitted starts a real pgx transaction pinned to READ COMMITTED via
// SET TRANSACTION (must be the first statement) rather than pgx.TxOptions, so the
// broker's Tx seam stays free of pgx-specific types. A failed SET rolls the
// half-open tx back instead of returning one at the pool's isolation: the audit
// chain's serialization depends on this, so an unpinned tx must never be returned.
func (s *PgxStore) BeginReadCommitted(ctx context.Context) (Tx, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `SET TRANSACTION ISOLATION LEVEL READ COMMITTED`); err != nil {
		_ = tx.Rollback(context.Background())
		return nil, fmt.Errorf("broker: pin read committed: %w", err)
	}
	return &pgxTx{tx: tx}, nil
}

// mintedCredentialsSQL sources RevokeRun's cascade from what the run ACTUALLY
// minted, not from what a human approved: the approvals half (`minted_jti`)
// alone misses auto-minted and re-minted (leased) credentials, so it is UNIONed
// (deduped) with audit_events' credential.mint successes, which cover every
// credential actually handed out. The LEFT JOIN keeps a mint whose grant row was
// deleted (kind ""); grant_id is matched as TEXT so a malformed
// data->>'grant_id' cannot abort the cascade with an invalid-uuid error.
const mintedCredentialsSQL = `
	SELECT minted_jti AS jti, COALESCE(g.spec->>'kind', '') AS kind
	  FROM approvals a
	  LEFT JOIN credential_grants g ON g.id = a.grant_id
	 WHERE a.run_id = $1 AND a.kind = 'credential' AND a.minted_jti <> ''
	UNION
	SELECT e.data->>'jti' AS jti, COALESCE(g.spec->>'kind', '') AS kind
	  FROM audit_events e
	  LEFT JOIN credential_grants g ON g.id::text = e.data->>'grant_id'
	 WHERE e.run_id = $1 AND e.action = 'credential.mint' AND e.outcome = 'success'
	   AND COALESCE(e.data->>'jti', '') <> ''`

// MintedCredentials lists every credential this run minted, with its grant kind.
func (s *PgxStore) MintedCredentials(ctx context.Context, runID uuid.UUID) ([]MintedCredential, error) {
	rows, err := s.Pool.Query(ctx, mintedCredentialsSQL, runID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[MintedCredential])
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
