// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// expiredScanBatch is how many expired rows one scan of DeleteExpired reads.
// A var so a test can page with a handful of rows.
var expiredScanBatch = 500

// DeleteExpired deletes every row whose expires_at has passed (credential-
// storage design §2.7, CS-5's sweep) and returns the rows it deleted. It is
// not part of the secretstore.Store seam, for the reason Rekey is not: it is a
// whole-table act, not per-name access.
//
// Each row is re-checked under its lock, so a sign-in renewed since the scan
// is kept; a pointer row loses its external value first, and a row whose value
// could not be removed is kept and named in the returned error as a
// *secretstore.ExpiredKept. Rows in every namespace are swept, the operator's
// included: an expiry is only ever set on a sign-in, never on a boot key.
//
// The scan pages by (owned_by, name), each page bounded in rows and time, and
// the cursor moves past a row whatever became of it, so rows the store keeps
// refusing never starve the ones after them.
func (s *Store) DeleteExpired(ctx context.Context) ([]secretstore.Expired, error) {
	var out []secretstore.Expired
	var errs []error
	var after secretstore.Expired
	for {
		due, err := s.scanExpired(ctx, after.Owner, after.Name, expiredScanBatch)
		if err != nil {
			return out, errors.Join(append(errs, err)...)
		}
		for _, d := range due {
			e, ok, err := s.deleteIfExpired(ctx, d.Owner, d.Name)
			if err != nil {
				errs = append(errs, &secretstore.ExpiredKept{Owner: d.Owner, Name: d.Name, Err: err})
				continue
			}
			if ok {
				out = append(out, e)
			}
		}
		if len(due) < expiredScanBatch {
			return out, errors.Join(errs...)
		}
		after = due[len(due)-1]
	}
}

// scanExpired reads at most limit expired rows after (owner, name), in order.
func (s *Store) scanExpired(ctx context.Context, owner, name string, limit int) ([]secretstore.Expired, error) {
	ctx, cancel := s.bounded(ctx)
	defer cancel()
	rows, err := s.pool.Query(ctx, `SELECT owned_by, name FROM secrets
		WHERE expires_at <= now() AND (owned_by, name) > ($1, $2)
		ORDER BY owned_by, name LIMIT $3`, owner, name, limit)
	if err != nil {
		return nil, fmt.Errorf("pg secretstore: expired select: %w", err)
	}
	due, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (secretstore.Expired, error) {
		var e secretstore.Expired
		err := r.Scan(&e.Owner, &e.Name)
		return e, err
	})
	if err != nil {
		return nil, fmt.Errorf("pg secretstore: expired scan: %w", err)
	}
	return due, nil
}

// deleteIfExpired deletes one row if it is still expired once locked.
func (s *Store) deleteIfExpired(ctx context.Context, owner, name string) (secretstore.Expired, bool, error) {
	e := secretstore.Expired{Owner: owner, Name: name}
	ctx, cancel := s.bounded(ctx)
	defer cancel()
	tx, err := beginReadCommitted(ctx, s.pool)
	if err != nil {
		return e, false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := lockRow(ctx, tx, owner, name); err != nil {
		return e, false, err
	}
	err = tx.QueryRow(ctx,
		`SELECT expires_at FROM secrets WHERE owned_by=$1 AND name=$2 AND expires_at <= now() FOR UPDATE`,
		owner, name,
	).Scan(&e.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return e, false, nil // renewed or removed since the scan
	}
	if err != nil {
		return e, false, fmt.Errorf("lock: %w", err)
	}
	if _, err := s.deleteLocked(ctx, tx, owner, name); err != nil {
		return e, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return e, false, fmt.Errorf("commit: %w", err)
	}
	return e, true, nil
}
