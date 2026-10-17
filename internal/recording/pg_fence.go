// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/db"
)

func (s *PGStore) runTx(ctx context.Context, key string, erase bool, fn func(pgx.Tx) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // no-op after commit
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, hashtext($2))`, db.RecordingLockClass, castRun(key)); err != nil {
		return fmt.Errorf("recording: lock run: %w", err)
	}
	if !erase {
		var erased bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM recording_erasures WHERE run_id = ANY($1::text[]))`, fenceKeys(key)).Scan(&erased); err != nil {
			return err
		}
		if erased {
			return ErrErased
		}
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
