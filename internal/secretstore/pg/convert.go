// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"filippo.io/age"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// ConvertV0 re-seals every legacy (enc_version 0, age-encrypted) row as an
// envelope v1 row under the KEK of its purpose, and returns the rows it
// converted. Single-writer (db.SecretConvertLockKey) and all-or-nothing: a v0
// row that fails to decrypt aborts the whole transaction, naming the row,
// rather than silently skipping a credential — idempotent and resumable. On a
// later-row or commit failure the rows it opened are returned with the error,
// the failing row among them when its value was opened before it failed (a
// row that will not decrypt was never opened), so the caller can record each
// read although nothing was committed.
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

	// A platform key is configured only after the boot keys were converted
	// (the documented order boots once on the age key alone first), so a v0
	// boot key beside it was written since, by whoever holds the age key and
	// the table: converting it would seal their key under the platform key.
	if s.separate || s.platformService != nil {
		for _, e := range all {
			if secretstore.Kind(e.ownedBy, e.name) == "platform" {
				return nil, &refusal{ErrV0BootKey, fmt.Sprintf("pg secretstore: v0 conversion REFUSED (nothing committed): %s is a pre-envelope boot key, "+
					"but a platform key is configured, and boot keys are converted only before it is set. "+
					"If this install is upgrading from 0.7.x or earlier, start wardynd once WITHOUT the platform key so it converts its secrets, "+
					"then set the platform key and run `wardynd -rewrap -rewrap-adopt-boot-keys`. "+
					"If it was already running 0.8 with a platform key, this row was not written by Wardyn: "+
					"investigate before moving anything (updated_at, the audit log, database access logs) and restore the boot key from a backup if it is forged", rowRef(e.ownedBy, e.name))}
			}
		}
	}

	// Row at a time, so at most ONE plaintext is resident at any moment.
	converted := make([]secretstore.Row, 0, len(all))
	for i, e := range all {
		opened, err := s.convertRow(ctx, tx, legacy, e)
		if opened {
			converted = append(converted, secretstore.Row{Store: s.Name(), Owner: e.ownedBy, Name: e.name, Found: true})
		}
		if err != nil {
			return converted, fmt.Errorf("pg secretstore: v0 conversion ABORTED after %d of %d rows (nothing committed; the store is still v0 and older wardynd can still read it): %s %w",
				i, len(all), rowRef(e.ownedBy, e.name), err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return converted, fmt.Errorf("pg secretstore: convert commit (%d rows, nothing committed): %w", len(all), err)
	}
	return converted, nil
}

// ErrV0BootKey is a pre-envelope boot key found while a platform key is
// configured; ConvertV0 refuses it and commits nothing.
var ErrV0BootKey = errors.New("pre-envelope boot key beside a platform key")

// convertRow re-seals one row. opened reports that its value was decrypted,
// whether or not the conversion then succeeded: that is a read to record.
func (s *Store) convertRow(ctx context.Context, tx pgx.Tx, legacy age.Identity, e envelope) (opened bool, err error) {
	plain, err := ageDecrypt(legacy, e.ct)
	if err != nil {
		return false, fmt.Errorf("does not decrypt with WARDYN_AGE_KEY: %w", err)
	}
	k := s.writer(e.ownedBy, e.name)
	wrapped, ct, err := seal(ctx, k, e.ownedBy, e.name, plain)
	if err != nil {
		return true, err
	}
	_, err = tx.Exec(ctx,
		`UPDATE secrets SET enc_version=$3, kek_id=$4, wrapped_dek=$5, ciphertext=$6
		  WHERE owned_by=$1 AND name=$2 AND enc_version=0`,
		e.ownedBy, e.name, encVersion, k.ID(), wrapped, ct)
	if err != nil {
		return true, fmt.Errorf("update: %w", err)
	}
	return true, nil
}

// LocalRows counts rows only a local KEK (or the age key itself, for v0) can
// open. wardynd refuses to boot on an ephemeral age key while any exist.
func (s *Store) LocalRows(ctx context.Context) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM secrets WHERE enc_version=0 OR kek_id LIKE 'local:%' OR kek_id LIKE 'local/%'`,
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
