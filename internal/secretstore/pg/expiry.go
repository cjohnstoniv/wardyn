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

// DeleteExpired deletes every row whose expires_at has passed (credential-
// storage design §2.7, CS-5's sweep) and returns the rows it deleted. It is
// not part of the secretstore.Store seam, for the reason Rekey is not: it is a
// whole-table act, not per-name access.
//
// Each row is re-checked under its lock, so a sign-in renewed since the scan
// is kept; a pointer row loses its external value first, and a row whose value
// could not be removed is kept and named in the returned error. Rows in every
// namespace are swept, the operator's included: an expiry is only ever set on
// a sign-in, never on a boot key.
func (s *Store) DeleteExpired(ctx context.Context) ([]secretstore.Expired, error) {
	rows, err := s.pool.Query(ctx, `SELECT owned_by, name FROM secrets WHERE expires_at <= now() ORDER BY owned_by, name`)
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
	var out []secretstore.Expired
	var errs []error
	for _, d := range due {
		e, ok, err := s.deleteIfExpired(ctx, d.Owner, d.Name)
		if err != nil {
			errs = append(errs, fmt.Errorf("pg secretstore: expired %s kept: %w", rowRef(d.Owner, d.Name), err))
			continue
		}
		if ok {
			out = append(out, e)
		}
	}
	return out, errors.Join(errs...)
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
	var version int16
	var kekID string
	err = tx.QueryRow(ctx,
		`SELECT enc_version, kek_id, expires_at FROM secrets WHERE owned_by=$1 AND name=$2 AND expires_at <= now() FOR UPDATE`,
		owner, name,
	).Scan(&version, &kekID, &e.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return e, false, nil // renewed or removed since the scan
	}
	if err != nil {
		return e, false, fmt.Errorf("lock: %w", err)
	}
	if version == extVersion {
		store, loc := splitRef(kekID)
		if !s.reachable(store) {
			return e, false, fmt.Errorf("it is stored in %q, which this wardynd is not configured to reach", store)
		}
		if err := s.ext.Delete(ctx, owner, name, loc); err != nil {
			return e, false, fmt.Errorf("delete from %s: %w", store, err)
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM secrets WHERE owned_by=$1 AND name=$2`, owner, name); err != nil {
		return e, false, fmt.Errorf("delete: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return e, false, fmt.Errorf("commit: %w", err)
	}
	return e, true, nil
}
