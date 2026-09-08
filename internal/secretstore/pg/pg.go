// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package pg implements secretstore.Store backed by an age-encrypted Postgres
// column (the `secrets` table in the core schema). The age recipient key is
// supplied at construction time; every Put encrypts with it and every Get
// decrypts. The key never leaves this package or enters the sandbox.
//
// Security invariant: the plaintext is only in memory during the Put/Get call.
// The caller is responsible for emitting a "secret.read" audit event before
// using a returned value.
package pg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"filippo.io/age"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// Compile-time assertion: Store implements secretstore.Store.
var _ secretstore.Store = (*Store)(nil)

// Store is an age-encrypted, Postgres-backed secret store.
// The zero value is unusable; use New.
type Store struct {
	pool      *pgxpool.Pool
	recipient age.Recipient // for encryption (Put)
	identity  age.Identity  // for decryption (Get)
	// owner is the secretstore.Store.For namespace this view is scoped to.
	// "" (the zero value, and New's own result) is the operator namespace —
	// every Store built before For existed keeps its exact behavior.
	owner string
}

// New constructs a Store. identity must be an age.X25519Identity (or any
// age.Identity); the corresponding Recipient is derived from it.
//
// Typical usage:
//
//	id, _ := age.GenerateX25519Identity()
//	s, _ := pg.New(pool, id)
func New(pool *pgxpool.Pool, identity age.Identity) (*Store, error) {
	// Derive the Recipient from the Identity so callers only need to supply
	// one key. The assertion must use the CONCRETE return type:
	// (*age.X25519Identity).Recipient() returns *age.X25519Recipient, so an
	// anonymous interface returning the age.Recipient interface never matches.
	type recipientOf interface {
		Recipient() *age.X25519Recipient
	}
	r, ok := identity.(recipientOf)
	if !ok {
		return nil, fmt.Errorf("pg secretstore: identity does not expose Recipient(); use *age.X25519Identity")
	}
	return &Store{
		pool:      pool,
		recipient: r.Recipient(),
		identity:  identity,
	}, nil
}

// Name identifies this backend for audit and UI.
func (s *Store) Name() string { return "pg" }

// For returns a view scoped to owner — see secretstore.Store.For's doc
// comment for the fallback/isolation contract. A shallow copy: owner is the
// only field that differs, so every view over the same *pgxpool.Pool sees
// the same rows, just through a different (owned_by) lens.
func (s *Store) For(owner string) secretstore.Store {
	cp := *s
	cp.owner = owner
	return &cp
}

// Put encrypts value with the age key and upserts the ciphertext into the
// secrets table, scoped to this view's owner. Duplicate (owner, name) pairs
// overwrite the previous ciphertext; a different owner holding the same name
// is a DIFFERENT row (migration 0050's whole point) and is never touched.
func (s *Store) Put(ctx context.Context, name string, value []byte) error {
	ct, err := s.encrypt(value)
	if err != nil {
		return fmt.Errorf("pg secretstore: encrypt %s: %w", name, err)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO secrets (owned_by, name, ciphertext)
		VALUES ($1, $2, $3)
		ON CONFLICT (owned_by, name) DO UPDATE
			SET ciphertext=$3, updated_at=now()`,
		s.owner, name, ct,
	)
	if err != nil {
		return fmt.Errorf("pg secretstore: put %s: %w", name, err)
	}
	return nil
}

// Get retrieves and decrypts a secret by name: this view's own (owner, name)
// row if one exists, else the operator's ("", name) row — a member with no
// key of their own resolves the operator's, exactly as every caller did
// before For existed. For owner="" the IN clause names "" twice, so only the
// operator row can ever match.
// Returns an error wrapping pgx.ErrNoRows (or a sentinel message) when absent.
func (s *Store) Get(ctx context.Context, name string) ([]byte, error) {
	var ct []byte
	err := s.pool.QueryRow(ctx,
		`SELECT ciphertext FROM secrets WHERE owned_by IN ('', $1) AND name=$2 ORDER BY (owned_by = $1) DESC LIMIT 1`,
		s.owner, name,
	).Scan(&ct)
	if errors.Is(err, pgx.ErrNoRows) {
		// Satisfy BOTH the seam sentinel (secretstore.ErrNotFound, what the
		// conformance suite + callers check) and the historical pgx.ErrNoRows
		// match (existing tests + cmd/wardynd loadOrCreateSecret) via errors.Join.
		return nil, fmt.Errorf("pg secretstore: secret %q not found: %w", name,
			errors.Join(secretstore.ErrNotFound, pgx.ErrNoRows))
	}
	if err != nil {
		return nil, fmt.Errorf("pg secretstore: get %s: %w", name, err)
	}
	plain, err := s.decrypt(ct)
	if err != nil {
		return nil, fmt.Errorf("pg secretstore: decrypt %s: %w", name, err)
	}
	return plain, nil
}

// Delete removes this view's own (owner, name) row. Idempotent (no-op if
// absent) and never touches a different owner's row of the same name —
// deleting a member's row leaves the operator's readable, and a member can
// never reach another member's row to delete it in the first place.
func (s *Store) Delete(ctx context.Context, name string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM secrets WHERE owned_by=$1 AND name=$2`, s.owner, name); err != nil {
		return fmt.Errorf("pg secretstore: delete %s: %w", name, err)
	}
	return nil
}

// List returns this view's OWN secret names only, in lexical order — never
// unioned with the operator's. A caller wanting "everything a principal may
// see" composes For("").List() ∪ For(owner).List() itself.
func (s *Store) List(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT name FROM secrets WHERE owned_by=$1 ORDER BY name`, s.owner)
	if err != nil {
		return nil, fmt.Errorf("pg secretstore: list: %w", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("pg secretstore: scan name: %w", err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg secretstore: iterate names: %w", err)
	}
	if names == nil {
		names = []string{}
	}
	return names, nil
}

// Rekey re-encrypts every row of the secrets table from oldID to newID and
// returns how many rows it re-encrypted. It is the body of wardynd's
// `-rotate-age-key` maintenance mode (cmd/wardynd's rotateAgeKeyMode) and is NOT
// part of the secretstore.Store seam: the Store contract is per-name late-bound
// access, while this is a whole-table administrative operation that only a
// column-encrypting backend has (an OpenBao/KMS store rotates in its own
// system, not here).
//
// ALL-OR-NOTHING. One transaction: any row that fails to decrypt aborts the
// whole thing — the returned error names how many of how many rows had been
// re-encrypted when it gave up, and nothing is committed, so every secret is
// still readable with the OLD key. There is no partial-rekey state to reason
// about, by construction.
//
// The FOR UPDATE on the select buys lost-update prevention, NOT exclusivity: it
// holds the rows it read, so a concurrent Put of one of those names waits and
// lands AFTER the commit instead of being clobbered by this transaction's
// re-encryption of the value it replaced. It does NOT keep rows out from under
// the retired key — under READ COMMITTED a Put of a NEW name inserts straight
// past these locks, and a queued Put of an existing name still writes its
// old-key ciphertext once released. That every committed row is readable with
// newID is carried by the offline requirement below, not by the lock.
//
// The caller supplies BOTH identities: the daemon must be offline (its in-memory
// Store still holds the old one), and the caller is responsible for persisting
// newID before a restart and for emitting the secret.rekey audit event.
func Rekey(ctx context.Context, pool *pgxpool.Pool, oldID, newID age.Identity) (int, error) {
	// Two throwaway Stores purely for their encrypt/decrypt halves — the age
	// framing lives there and nothing here wants a second copy of it. Neither
	// one's pool methods are used: every statement below runs on tx.
	from, err := New(pool, oldID)
	if err != nil {
		return 0, fmt.Errorf("pg secretstore: rekey old identity: %w", err)
	}
	to, err := New(pool, newID)
	if err != nil {
		return 0, fmt.Errorf("pg secretstore: rekey new identity: %w", err)
	}

	tx, err := beginReadCommitted(ctx, pool)
	if err != nil {
		return 0, fmt.Errorf("pg secretstore: rekey begin: %w", err)
	}
	// A Rollback after a successful Commit is a documented no-op; on every error
	// path below it is the thing that makes this all-or-nothing.
	defer func() { _ = tx.Rollback(ctx) }()

	type row struct {
		ownedBy string
		name    string
		ct      []byte
	}
	// ORDER BY owned_by, name (not name alone): two rows can now share a name
	// under different owners (migration 0050), and the UPDATE below keys on
	// BOTH columns — ordering by both just keeps the lock/abort order stable
	// and readable, not for correctness.
	rows, err := tx.Query(ctx, `SELECT owned_by, name, ciphertext FROM secrets ORDER BY owned_by, name FOR UPDATE`)
	if err != nil {
		return 0, fmt.Errorf("pg secretstore: rekey select: %w", err)
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.ownedBy, &r.name, &r.ct); err != nil {
			rows.Close()
			return 0, fmt.Errorf("pg secretstore: rekey scan: %w", err)
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("pg secretstore: rekey iterate: %w", err)
	}

	// Row at a time (rather than decrypting everything up front) so at most ONE
	// plaintext is resident at any moment, whatever the store holds.
	for i, r := range all {
		// desc identifies the row in an abort message. Bare name for an
		// operator row (owned_by="") keeps existing abort-message assertions
		// (e.g. "aaa-stray") matching byte-for-byte; a non-"" owner is
		// prefixed since 0050 lets two rows share a name.
		desc := r.name
		if r.ownedBy != "" {
			desc = r.ownedBy + "/" + r.name
		}
		plain, derr := from.decrypt(r.ct)
		if derr != nil {
			return 0, rekeyAbort(i, len(all), desc, "decrypt with the old key", derr)
		}
		ct, eerr := to.encrypt(plain)
		if eerr != nil {
			return 0, rekeyAbort(i, len(all), desc, "encrypt with the new key", eerr)
		}
		// Keyed on BOTH columns (not name alone): with two owners sharing a
		// name, a name-only WHERE would match and overwrite BOTH rows with
		// THIS row's freshly re-encrypted ciphertext — the exact corruption
		// TestRekey_TwoNamespacesKeepDistinctPlaintexts pins.
		if _, uerr := tx.Exec(ctx,
			`UPDATE secrets SET ciphertext=$3, updated_at=now() WHERE owned_by=$1 AND name=$2`, r.ownedBy, r.name, ct,
		); uerr != nil {
			return 0, rekeyAbort(i, len(all), desc, "update", uerr)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("pg secretstore: rekey commit (%d rows, NOTHING committed — the old key still reads every secret): %w", len(all), err)
	}
	return len(all), nil
}

// beginReadCommitted starts a transaction on pool pinned to READ COMMITTED.
//
// Rekey rewrites EVERY ciphertext in the store under one transaction and its
// caller (cmd/wardynd -rotate-age-key) emits the secret.rekey audit event for it,
// so this transaction must not inherit default_transaction_isolation: on a pool
// set to REPEATABLE READ a long rekey takes a snapshot at its first statement and
// then holds it for the whole rewrite, which turns any concurrent writer into a
// serialization failure the rotation reports as an abort. READ COMMITTED is also
// exactly the isolation the FOR UPDATE lock reasoning above is written against.
//
// SET TRANSACTION rather than pgx.TxOptions: equivalent as long as it is the FIRST
// statement of the transaction, which it is. A failed SET rolls the half-open
// transaction back so no unpinned tx is ever returned (same shape as the broker's
// PgxStore.BeginReadCommitted and internal/db's beginReadCommitted).
func beginReadCommitted(ctx context.Context, pool *pgxpool.Pool) (pgx.Tx, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `SET TRANSACTION ISOLATION LEVEL READ COMMITTED`); err != nil {
		_ = tx.Rollback(context.Background())
		return nil, fmt.Errorf("pin read committed: %w", err)
	}
	return tx, nil
}

// rekeyAbort formats the one error Rekey fails with: what broke, on which
// secret, and how far it had got — plus the load-bearing fact that the abort
// left the store untouched, which is what tells an operator to fix the row and
// retry rather than hunt for a half-rotated store. The name is included for the
// same reason Get's decrypt error includes it: without it the operator cannot
// find the row.
func rekeyAbort(done, total int, name, what string, err error) error {
	return fmt.Errorf("pg secretstore: rekey ABORTED after %d of %d rows (nothing committed — every secret is still readable with the OLD key): %s %q: %w",
		done, total, what, name, err)
}

// encrypt encodes plaintext with the age recipient.
func (s *Store) encrypt(plaintext []byte) ([]byte, error) {
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, s.recipient)
	if err != nil {
		return nil, fmt.Errorf("age encrypt init: %w", err)
	}
	if _, err := w.Write(plaintext); err != nil {
		return nil, fmt.Errorf("age encrypt write: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("age encrypt close: %w", err)
	}
	return buf.Bytes(), nil
}

// decrypt decodes ciphertext with the age identity.
func (s *Store) decrypt(ciphertext []byte) ([]byte, error) {
	r, err := age.Decrypt(bytes.NewReader(ciphertext), s.identity)
	if err != nil {
		return nil, fmt.Errorf("age decrypt: %w", err)
	}
	plain, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("age decrypt read: %w", err)
	}
	return plain, nil
}
