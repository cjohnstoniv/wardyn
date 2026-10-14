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

var _ secretstore.Revisioned = (*Store)(nil)

// revisionExpr names a row's current content. Every local write re-keys the
// row (a fresh data key and fresh nonces), so its ciphertext changes with it;
// a store-mode write may keep the same external ref, so updated_at joins the
// digest. Neither is a value or a key.
const revisionExpr = `encode(sha256(ciphertext || wrapped_dek ||
	convert_to(kek_id || '|' || extract(epoch from updated_at)::text, 'UTF8')), 'hex')`

// Revision names the current content of this view's own row of name, or "" when
// there is none — see secretstore.Revisioned.
func (s *Store) Revision(ctx context.Context, name string) (string, error) {
	var rev string
	err := s.pool.QueryRow(ctx, `SELECT `+revisionExpr+` FROM secrets WHERE owned_by=$1 AND name=$2`, s.owner, name).Scan(&rev)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("pg secretstore: revision of %s: %w: %w", rowRef(s.owner, name), secretstore.ErrUnavailable, err)
	}
	return rev, nil
}

// putIfRevision is the local-mode guarded upsert: one statement, so the check
// and the write are atomic. rev "" requires an absent row.
func (s *Store) putIfRevision(ctx context.Context, q execer, name, rev string, r sealedRow) error {
	var n int64
	if rev == "" {
		tag, err := q.Exec(ctx, `
			INSERT INTO secrets (owned_by, name, enc_version, kek_id, wrapped_dek, ciphertext, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (owned_by, name) DO NOTHING`,
			s.owner, name, r.version, r.kekID, r.wrapped, r.ct, expiresAt(ctx))
		if err != nil {
			return fmt.Errorf("pg secretstore: put %s: %w", rowRef(s.owner, name), err)
		}
		n = tag.RowsAffected()
	} else {
		tag, err := q.Exec(ctx, `
			UPDATE secrets SET enc_version=$3, kek_id=$4, wrapped_dek=$5, ciphertext=$6, expires_at=$7, updated_at=now()
			WHERE owned_by=$1 AND name=$2 AND `+revisionExpr+` = $8`,
			s.owner, name, r.version, r.kekID, r.wrapped, r.ct, expiresAt(ctx), rev)
		if err != nil {
			return fmt.Errorf("pg secretstore: put %s: %w", rowRef(s.owner, name), err)
		}
		n = tag.RowsAffected()
	}
	if n == 0 {
		return fmt.Errorf("pg secretstore: put %s: %w", rowRef(s.owner, name), secretstore.ErrRevisionChanged)
	}
	return nil
}

// checkRevision is putExternal's guard, inside its transaction after the row's
// write lock is held: no other writer can land between this read and the
// write, and the check precedes any write to the external store.
func (s *Store) checkRevision(ctx context.Context, tx pgx.Tx, name, rev string) error {
	var cur string
	err := tx.QueryRow(ctx, `SELECT `+revisionExpr+` FROM secrets WHERE owned_by=$1 AND name=$2`, s.owner, name).Scan(&cur)
	if errors.Is(err, pgx.ErrNoRows) {
		cur = ""
	} else if err != nil {
		return fmt.Errorf("pg secretstore: put %s: read revision: %w", rowRef(s.owner, name), err)
	}
	if cur != rev {
		return fmt.Errorf("pg secretstore: put %s: %w", rowRef(s.owner, name), secretstore.ErrRevisionChanged)
	}
	return nil
}

// lockRevision is the guarded Delete's check: checkRevision with the row itself
// locked (FOR UPDATE) for the rest of tx. A local-mode Put takes no advisory
// lock, so only the row's lock stands between this read and the delete: a
// replace open when it runs commits first, and the revision read here is then
// that replace's own, or it waits for tx. found is false when the holder
// expected no row and there is none: nothing is locked then, so the caller
// returns rather than delete a row that lands next.
func (s *Store) lockRevision(ctx context.Context, tx pgx.Tx, name, rev string) (found bool, err error) {
	var cur string
	err = tx.QueryRow(ctx, `SELECT `+revisionExpr+` FROM secrets WHERE owned_by=$1 AND name=$2 FOR UPDATE`, s.owner, name).Scan(&cur)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return false, fmt.Errorf("pg secretstore: delete %s: read revision: %w", rowRef(s.owner, name), err)
	default:
		found = true
	}
	if cur != rev {
		return false, fmt.Errorf("pg secretstore: delete %s: %w", rowRef(s.owner, name), secretstore.ErrRevisionChanged)
	}
	return found, nil
}
