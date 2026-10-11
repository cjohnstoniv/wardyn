// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

import (
	"context"
	"errors"
	"fmt"

	"filippo.io/age"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
)

// RowReader is the one connection CheckRows reads over: a *pgxpool.Conn satisfies it.
type RowReader interface {
	subjectkey.DB
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// RowOutcome is what CheckRows found for one row.
type RowOutcome string

const (
	// RowOpened: the value opened and its integrity check passed.
	RowOpened RowOutcome = "opened"
	// RowConverts: a pre-envelope (enc_version 0) row opened under the age key; the first boot would
	// convert it, and this read did not.
	RowConverts RowOutcome = "converts on first boot"
	// RowDestroyed: the owner's principal key was destroyed by an erasure, which is the erasure
	// working, not a failure.
	RowDestroyed RowOutcome = "destroyed"
	// RowExternal: a store-mode pointer row. Its value lives in the external store, which proves
	// nothing about a restored set, so it is never read.
	RowExternal RowOutcome = "external"
	// RowFailed: the row did not open; Err says why, naming the row and never a value.
	RowFailed RowOutcome = "failed"
)

// CheckedRow is one row's result. It carries the row's reference and key class, never a value.
// A secrets row has Name and EncVersion; a principal key (PrincipalKey) has Name the purpose and
// Generation its version.
type CheckedRow struct {
	PrincipalKey bool
	Owner, Name  string
	EncVersion   int16
	Generation   int
	KekID        string
	Outcome      RowOutcome
	Err          error
}

// CheckRows opens every row of the secrets table in memory, one at a time, and reports each to visit:
// it is the read-only proof, for a restored copy, that the keys in this process open what the copy
// holds (wardynd -recovery-rehearse). Every plaintext is checked by its integrity tag and cleared with
// its data key; none is returned.
//
// Everything runs over q, one connection. It never writes: a pre-envelope row is opened with
// ageDecrypt, the read convertRow makes, and left as it is. legacy is the age identity for those rows,
// nil when the deployment has none. The error is a failure to read the table; a row that does not
// open is a CheckedRow, not an error.
func (s *Store) CheckRows(ctx context.Context, q RowReader, legacy age.Identity, visit func(CheckedRow)) error {
	list, err := listSecretRows(ctx, q)
	if err != nil {
		return err
	}
	reader := *s
	reader.pool = nil
	reader.subjects = subjectkey.New(q, subjectkey.Resolver{Reader: s.pkReader})
	for _, c := range list {
		c.Outcome, c.Err = reader.checkRow(ctx, q, legacy, c)
		visit(c)
	}
	return reader.checkPrincipalKeys(ctx, q, visit)
}

// ListRows reports every secrets row and every live principal key with its key class and no outcome,
// opening nothing and reading no ciphertext: it is how a manifest counts rows per key class
// (wardynd -recovery-manifest). A pointer row has Outcome RowExternal, as CheckRows reports it.
func ListRows(ctx context.Context, q RowReader, visit func(CheckedRow)) error {
	list, err := listSecretRows(ctx, q)
	if err != nil {
		return err
	}
	for _, c := range list {
		if c.EncVersion == extVersion {
			c.Outcome = RowExternal
		}
		visit(c)
	}
	keys, err := listPrincipalKeys(ctx, q)
	if err != nil {
		return err
	}
	for _, c := range keys {
		visit(c)
	}
	return nil
}

func listSecretRows(ctx context.Context, q RowReader) ([]CheckedRow, error) {
	rows, err := q.Query(ctx, `SELECT owned_by, name, enc_version, kek_id FROM secrets ORDER BY owned_by, name`)
	if err != nil {
		return nil, fmt.Errorf("pg secretstore: list rows: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (CheckedRow, error) {
		var c CheckedRow
		err := r.Scan(&c.Owner, &c.Name, &c.EncVersion, &c.KekID)
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("pg secretstore: list rows: %w", err)
	}
	return list, nil
}

func listPrincipalKeys(ctx context.Context, q RowReader) ([]CheckedRow, error) {
	rows, err := q.Query(ctx, `SELECT owner, purpose, version, kek_id FROM principal_keys WHERE destroyed_at IS NULL ORDER BY owner, purpose, version`)
	if err != nil {
		return nil, fmt.Errorf("pg secretstore: list principal keys: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (CheckedRow, error) {
		c := CheckedRow{PrincipalKey: true}
		err := r.Scan(&c.Owner, &c.Name, &c.Generation, &c.KekID)
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("pg secretstore: list principal keys: %w", err)
	}
	return list, nil
}

// checkPrincipalKeys unwraps every live principal key, so a key no credential row names (an audit-seal
// key) is proved too.
func (s *Store) checkPrincipalKeys(ctx context.Context, q RowReader, visit func(CheckedRow)) error {
	list, err := listPrincipalKeys(ctx, q)
	if err != nil {
		return err
	}
	for _, c := range list {
		key, err := s.subjects.Key(ctx, c.Owner, c.Name, c.Generation)
		clear(key)
		switch {
		case errors.Is(err, subjectkey.ErrDataLoss):
			c.Outcome = RowDestroyed
		case err != nil:
			c.Outcome, c.Err = RowFailed, fmt.Errorf("pg secretstore: principal key (owner=%q, purpose=%q, generation %d): %w", c.Owner, c.Name, c.Generation, err)
		default:
			c.Outcome = RowOpened
		}
		visit(c)
	}
	return nil
}

func (s *Store) checkRow(ctx context.Context, q RowReader, legacy age.Identity, c CheckedRow) (RowOutcome, error) {
	if c.EncVersion == extVersion {
		return RowExternal, nil
	}
	e := envelope{ownedBy: c.Owner, name: c.Name, version: c.EncVersion, kekID: c.KekID}
	if err := q.QueryRow(ctx, `SELECT wrapped_dek, ciphertext FROM secrets WHERE owned_by=$1 AND name=$2`, c.Owner, c.Name).Scan(&e.wrapped, &e.ct); err != nil {
		return RowFailed, fmt.Errorf("pg secretstore: read %s: %w", rowRef(c.Owner, c.Name), err)
	}
	if c.EncVersion == 0 {
		return checkLegacy(legacy, e)
	}
	plain, err := s.open(ctx, e)
	clear(plain)
	switch {
	case errors.Is(err, subjectkey.ErrDataLoss):
		return RowDestroyed, nil
	case err != nil:
		return RowFailed, err
	}
	return RowOpened, nil
}

func checkLegacy(legacy age.Identity, e envelope) (RowOutcome, error) {
	if legacy == nil {
		return RowFailed, fmt.Errorf("pg secretstore: %s is a pre-envelope row and there is no WARDYN_AGE_KEY to open it", rowRef(e.ownedBy, e.name))
	}
	plain, err := ageDecrypt(legacy, e.ct)
	clear(plain)
	if err != nil {
		return RowFailed, fmt.Errorf("pg secretstore: %s does not decrypt with WARDYN_AGE_KEY: %w", rowRef(e.ownedBy, e.name), err)
	}
	return RowConverts, nil
}
