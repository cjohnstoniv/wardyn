// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
)

type rowSnapshot struct {
	envelope
	revision string
}

type snapshotQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// Include the whole row and its tuple generation: even a metadata-only change or
// a delete/reinsert must invalidate material prepared without the write lock.
func readRowSnapshot(ctx context.Context, q snapshotQuerier, owner, name string, locked bool) (rowSnapshot, error) {
	e := rowSnapshot{envelope: envelope{ownedBy: owner, name: name}}
	query := `SELECT enc_version, kek_id, wrapped_dek, ciphertext,
        encode(sha256(convert_to(row_to_json(secrets)::text || ':' || xmin::text, 'UTF8')), 'hex')
        FROM secrets WHERE owned_by=$1 AND name=$2`
	if locked {
		query += ` FOR UPDATE`
	}
	err := q.QueryRow(ctx, query, owner, name).Scan(&e.version, &e.kekID, &e.wrapped, &e.ct, &e.revision)
	return e, err
}

func lockPrincipalGeneration(ctx context.Context, tx pgx.Tx, owner string, r sealedRow) error {
	if r.version != pkVersion {
		return nil
	}
	version, _ := pkVersionOf(r.kekID)
	var live bool
	err := tx.QueryRow(ctx, `SELECT destroyed_at IS NULL AND superseded_at IS NULL FROM principal_keys
        WHERE owner=$1 AND purpose=$2 AND version=$3 FOR SHARE`, owner, subjectkey.PurposeCred, version).Scan(&live)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !live) {
		return secretstore.ErrRevisionChanged
	}
	return err
}

func replaceSnapshot(ctx context.Context, tx pgx.Tx, before rowSnapshot, after sealedRow) error {
	cur, err := readRowSnapshot(ctx, tx, before.ownedBy, before.name, true)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && cur.revision != before.revision) {
		return secretstore.ErrRevisionChanged
	}
	if err != nil {
		return err
	}
	if before.version == pkVersion {
		version, _ := pkVersionOf(before.kekID)
		var live bool
		err := tx.QueryRow(ctx, `SELECT destroyed_at IS NULL FROM principal_keys
            WHERE owner=$1 AND purpose=$2 AND version=$3 FOR SHARE`, before.ownedBy, subjectkey.PurposeCred, version).Scan(&live)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !live) {
			return subjectkey.ErrDataLoss
		}
		if err != nil {
			return err
		}
	}
	if err := lockPrincipalGeneration(ctx, tx, before.ownedBy, after); err != nil {
		return err
	}
	return flipRow(ctx, tx, before.ownedBy, before.name, after.version, after.kekID, after.wrapped, after.ct)
}

// External objects may reuse a deterministic path. Keep the same row lock as
// Put/Delete across their network calls, without keeping a transaction open.
// Every statement during the hold uses conn; key preparation precedes it.
func (s *Store) lockExternalRow(ctx context.Context, owner, name string) (*pgxpool.Conn, func(), error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, nil, err
	}
	release := func() {
		bg, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		var unlocked bool
		err := conn.QueryRow(bg, `SELECT pg_advisory_unlock($1, $2)`, db.SecretRowLockClass, rowLockKey(owner, name)).Scan(&unlocked)
		if err != nil || !unlocked {
			_ = conn.Conn().Close(bg)
			conn.Release()
			return
		}
		conn.Release()
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1, $2)`, db.SecretRowLockClass, rowLockKey(owner, name)); err != nil {
		release()
		return nil, nil, fmt.Errorf("lock the row: %w", err)
	}
	return conn, release, nil
}
