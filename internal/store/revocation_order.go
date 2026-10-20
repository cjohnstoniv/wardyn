// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/db"
)

// A credential is ordered against a revocation cut by the logical sequence oidc_revocation_seq (migration
// 0176), never by two wall-clock readings. A cut draws its seq under the exclusive revocation-order lock
// and holds the lock until it commits; an issuer reads the sequence under the shared lock. A cut whose
// value the issuer's read covers has therefore committed before the read, and every later cut draws a
// greater one.

// batcher is the pipeline surface of *pgxpool.Pool and *pgx.Conn.
type batcher interface {
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}

// ReadRevocationSeq is the ordering value a credential issued now records. Always at least 1 once
// migration 0176 has run; the caller stores it as the credential's rev_seq.
func ReadRevocationSeq(ctx context.Context, q batcher) (int64, error) {
	b := &pgx.Batch{}
	b.Queue(`SELECT pg_advisory_xact_lock_shared($1, 0)`, db.RevocationOrderLockClass)
	b.Queue(`SELECT oidc_revocation_position()`) // SECURITY DEFINER (0176): an app role holds no privilege on the sequence
	br := q.SendBatch(ctx, b)
	defer br.Close()
	if _, err := br.Exec(); err != nil {
		return 0, fmt.Errorf("store: lock revocation order: %w", err)
	}
	var seq int64
	if err := br.QueryRow().Scan(&seq); err != nil {
		return 0, fmt.Errorf("store: read revocation seq: %w", err)
	}
	return seq, nil
}

// LockRevocationOrder takes the exclusive revocation-order lock for the rest of tx, which must contain the
// cut write it protects. tx must be a pgx.Tx.
func LockRevocationOrder(ctx context.Context, tx Querier) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, 0)`, db.RevocationOrderLockClass); err != nil {
		return fmt.Errorf("store: lock revocation order: %w", err)
	}
	return nil
}

// ExecRevocationCut runs one cut write (an upsert into oidc_session_revocations or oidc_session_cuts,
// whose $1 is sub) under the exclusive revocation-order lock, as a single pipelined transaction that
// also ends the CLI sessions the cut reaches (endCLISessionsSQL), stamped cliReason.
func ExecRevocationCut(ctx context.Context, q batcher, sql, sub, cliReason string) error {
	b := &pgx.Batch{}
	b.Queue(`SELECT pg_advisory_xact_lock($1, 0)`, db.RevocationOrderLockClass)
	b.Queue(sql, sub)
	b.Queue(endCLISessionsSQL, sub, cliReason)
	br := q.SendBatch(ctx, b)
	defer br.Close()
	if _, err := br.Exec(); err != nil {
		return fmt.Errorf("store: lock revocation order: %w", err)
	}
	if _, err := br.Exec(); err != nil {
		return err
	}
	if _, err := br.Exec(); err != nil {
		return fmt.Errorf("store: end cli sessions: %w", err)
	}
	return nil
}
