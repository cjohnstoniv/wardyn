// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
)

// RewrapResult is what Rewrap did.
type RewrapResult struct {
	// KEK is the kek_id every sealed row now names.
	KEK string
	// Rewrapped is how many rows' data keys moved.
	Rewrapped int
	// KeyVersion is the key version every row is now wrapped under when the
	// KEK is versioned (Transit), else 0: raising Transit's
	// min_decryption_version to it retires every older version.
	KeyVersion int
}

// Rewrap moves every sealed (v1) row's data key to the KEK every write uses
// (WARDYN_KEK), and to that key's latest version when it is versioned. It is
// the body of `wardynd -rewrap` (design §2.3, §2.4): it changes provider in
// either direction (local ⇄ Transit) and moves rows off a Transit version
// before min_decryption_version retires it.
//
// The rewrap is CLIENT-SIDE: the data key is unwrapped under the row's own
// KEK and wrapped again under the target, both bound to the row. Transit's
// server-side rewrap endpoint is never called — Vault does not document
// associated_data on it, and a rewrap that dropped the binding would be
// silent. Only wrapped_dek and kek_id change; the sealed value never is
// decrypted.
//
// Safe while a daemon serves: one row per transaction under its row lock, a
// row a concurrent Put already rewrote is skipped, and a row deleted or moved
// to an external store meanwhile is left alone. Idempotent and resumable: it
// aborts on the first row it cannot move, naming it, with every earlier row
// committed. Pointer rows hold no data key and are never touched.
func (s *Store) Rewrap(ctx context.Context) (RewrapResult, error) {
	var res RewrapResult
	if s.kek == nil {
		return res, errors.New("pg secretstore: rewrap needs a key to wrap under: set WARDYN_KEK, or WARDYN_AGE_KEY for the local key")
	}
	res.KEK = s.kek.ID()
	v, _ := s.kek.(kek.Versioned)
	if v != nil {
		n, err := v.LatestVersion(ctx)
		if err != nil {
			return res, fmt.Errorf("pg secretstore: rewrap: read the latest version of %s: %w", res.KEK, err)
		}
		res.KeyVersion = n
	}
	rows, err := s.pool.Query(ctx, `SELECT owned_by, name FROM secrets WHERE enc_version=$1 ORDER BY owned_by, name`, encVersion)
	if err != nil {
		return res, fmt.Errorf("pg secretstore: rewrap select: %w", err)
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (envelope, error) {
		var e envelope
		err := r.Scan(&e.ownedBy, &e.name)
		return e, err
	})
	if err != nil {
		return res, fmt.Errorf("pg secretstore: rewrap scan: %w", err)
	}
	for _, e := range all {
		moved, err := s.rewrapRow(ctx, e.ownedBy, e.name, v, res.KeyVersion)
		if err != nil {
			return res, fmt.Errorf("pg secretstore: rewrap to %s ABORTED at %s after %d rows (each rewrapped row is committed; fix this row and re-run): %w",
				res.KEK, rowRef(e.ownedBy, e.name), res.Rewrapped, err)
		}
		if moved {
			res.Rewrapped++
		}
	}
	return res, nil
}

// rewrapRow rewraps one row under its row lock, if it is still stale.
func (s *Store) rewrapRow(ctx context.Context, owner, name string, v kek.Versioned, latest int) (bool, error) {
	ctx, cancel := s.bounded(ctx)
	defer cancel()
	tx, err := beginReadCommitted(ctx, s.pool)
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	e := envelope{ownedBy: owner, name: name}
	err = tx.QueryRow(ctx,
		`SELECT enc_version, kek_id, wrapped_dek FROM secrets WHERE owned_by=$1 AND name=$2 FOR UPDATE`,
		owner, name,
	).Scan(&e.version, &e.kekID, &e.wrapped)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock: %w", err)
	}
	if e.version != encVersion {
		return false, nil // moved to an external store meanwhile
	}
	stale, err := s.stale(e, v, latest)
	if err != nil || !stale {
		return false, err
	}
	from, err := s.kekFor(e.kekID)
	if err != nil {
		return false, err
	}
	wrapped, err := rewrap(ctx, from, s.kek, e)
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE secrets SET kek_id=$3, wrapped_dek=$4 WHERE owned_by=$1 AND name=$2`, owner, name, s.kek.ID(), wrapped,
	); err != nil {
		return false, fmt.Errorf("update: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return true, nil
}

// stale reports whether a row's data key is not yet under the target KEK's
// latest version.
func (s *Store) stale(e envelope, v kek.Versioned, latest int) (bool, error) {
	if e.kekID != s.kek.ID() {
		return true, nil
	}
	if v == nil {
		return false, nil
	}
	n, err := v.WrapVersion(e.wrapped)
	if err != nil {
		return false, err
	}
	return n < latest, nil
}
