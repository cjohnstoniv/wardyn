// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"filippo.io/age"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// ConvertV0 re-seals every legacy (enc_version 0, age-encrypted) row as an
// envelope v1 row under this store's KEK, and returns the rows it converted —
// each one a read of a stored value, which the caller records.
// wardynd runs it at boot BEFORE the boot keys are read (they share the
// table), so there is no v0 read path anywhere else.
//
// Single-writer: the transaction first takes db.SecretConvertLockKey, so a
// second booting replica waits, then selects nothing. All-or-nothing: one
// transaction, and a v0 row that does not decrypt under legacy aborts it,
// naming the row — never skipped, since a skipped row is a credential silently
// lost. That makes it idempotent (a converted store has no v0 rows) and
// resumable (an abort committed nothing; fix the row or the key and boot again).
func (s *Store) ConvertV0(ctx context.Context, legacy age.Identity) ([]secretstore.Row, error) {
	if s.kek == nil {
		return nil, fmt.Errorf("pg secretstore: convert: no local key is configured")
	}
	tx, err := beginReadCommitted(ctx, s.pool)
	if err != nil {
		return nil, fmt.Errorf("pg secretstore: convert begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, db.SecretConvertLockKey); err != nil {
		return nil, fmt.Errorf("pg secretstore: convert lock: %w", err)
	}
	rows, err := tx.Query(ctx, `SELECT owned_by, name, ciphertext FROM secrets WHERE enc_version=0 ORDER BY owned_by, name FOR UPDATE`)
	if err != nil {
		return nil, fmt.Errorf("pg secretstore: convert select: %w", err)
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (envelope, error) {
		var e envelope
		err := r.Scan(&e.ownedBy, &e.name, &e.ct)
		return e, err
	})
	if err != nil {
		return nil, fmt.Errorf("pg secretstore: convert scan: %w", err)
	}

	// Row at a time, so at most ONE plaintext is resident at any moment.
	converted := make([]secretstore.Row, 0, len(all))
	for i, e := range all {
		if err := s.convertRow(ctx, tx, legacy, e); err != nil {
			return nil, fmt.Errorf("pg secretstore: v0 conversion ABORTED after %d of %d rows (nothing committed; the store is still v0 and older wardynd can still read it): %s %w",
				i, len(all), rowRef(e.ownedBy, e.name), err)
		}
		converted = append(converted, secretstore.Row{Store: s.Name(), Owner: e.ownedBy, Name: e.name, Found: true})
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("pg secretstore: convert commit (%d rows, nothing committed): %w", len(all), err)
	}
	return converted, nil
}

func (s *Store) convertRow(ctx context.Context, tx pgx.Tx, legacy age.Identity, e envelope) error {
	plain, err := ageDecrypt(legacy, e.ct)
	if err != nil {
		return fmt.Errorf("does not decrypt with WARDYN_AGE_KEY: %w", err)
	}
	wrapped, ct, err := seal(ctx, s.kek, e.ownedBy, e.name, plain)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx,
		`UPDATE secrets SET enc_version=$3, kek_id=$4, wrapped_dek=$5, ciphertext=$6
		  WHERE owned_by=$1 AND name=$2 AND enc_version=0`,
		e.ownedBy, e.name, encVersion, s.kek.ID(), wrapped, ct)
	if err != nil {
		return fmt.Errorf("update: %w", err)
	}
	return nil
}

// LocalRows counts rows only a local KEK (or the age key itself, for v0) can
// open. wardynd refuses to boot on an ephemeral age key while any exist:
// minting a fresh key over them would strand every one.
func (s *Store) LocalRows(ctx context.Context) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM secrets WHERE enc_version=0 OR kek_id LIKE 'local:%'`,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("pg secretstore: count local rows: %w", err)
	}
	return n, nil
}

// ageDecrypt opens a legacy v0 payload. Its only caller is convertRow.
func ageDecrypt(identity age.Identity, ciphertext []byte) ([]byte, error) {
	r, err := age.Decrypt(bytes.NewReader(ciphertext), identity)
	if err != nil {
		return nil, fmt.Errorf("age decrypt: %w", err)
	}
	plain, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("age decrypt read: %w", err)
	}
	return plain, nil
}
