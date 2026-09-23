// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package pg implements secretstore.Store over the Postgres `secrets` table,
// one envelope-encrypted row per credential (credential-storage design §2.2,
// envelope v1): every Put mints a fresh 32-byte data key (DEK), seals the value
// under it with AES-256-GCM bound to the row's (owned_by, name), and stores the
// DEK wrapped by a key-encryption key (package kek). The row records which KEK
// wrapped it (kek_id), and a read dispatches on enc_version and kek_id.
//
// Security invariant: the plaintext and the DEK are only in memory during the
// Put/Get call, and no error carries either — errors name the row, never its
// value. The caller is responsible for emitting a "secret.read" audit event
// before using a returned value.
package pg

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"

	"filippo.io/age"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
)

// Compile-time assertion: Store implements secretstore.Store.
var _ secretstore.Store = (*Store)(nil)

// encVersion is the row format every local write produces. 0 is the legacy
// age payload, which only the boot conversion (ConvertV0) reads; extVersion is
// the store-mode pointer row (external.go).
const encVersion = 1

// ageHeader opens every age payload, and so every row a pre-envelope wardynd
// writes.
var ageHeader = []byte("age-encryption.org/v1\n")

// unknownVersion refuses a row format this binary predates: a newer wardynd
// wrote it (a mixed-version window, or a rollback). It is never read as
// not-found and never handed to the age path — only ConvertV0 reads age, and
// only enc_version 0.
const unknownVersion = "has enc_version %d which this wardynd does not understand; upgrade wardynd"

// secretAADLabel is the domain label of AAD_secret, the value's binding.
const secretAADLabel = "wardyn/secret/v1"

// Store is an envelope-encrypted, Postgres-backed secret store.
// The zero value is unusable; use New.
type Store struct {
	pool *pgxpool.Pool
	// kek wraps every DEK this store writes and is the only KEK a read accepts:
	// a row whose kek_id names another is refused. nil in store mode with no
	// WARDYN_AGE_KEY: then every local (v1) row is refused by name.
	kek kek.KEK
	// ext is the configured external store, or nil. Pointer rows (enc_version
	// 2) are read through it in every mode; writeExt says whether Put writes
	// there (store mode) or seals locally.
	ext      secretstore.External
	writeExt bool
	// owner is the secretstore.Store.For namespace this view is scoped to.
	// "" (the zero value, and New's own result) is the operator namespace —
	// every Store built before For existed keeps its exact behavior.
	owner string
}

// New constructs a Store whose KEK is the local one derived from identity
// (kek.NewLocal). identity must be an *age.X25519Identity; it is used for that
// derivation only — the one other use of the age key is ConvertV0.
func New(pool *pgxpool.Pool, identity age.Identity) (*Store, error) {
	k, err := localKEK(identity)
	if err != nil {
		return nil, err
	}
	return &Store{pool: pool, kek: k}, nil
}

func localKEK(identity age.Identity) (*kek.Local, error) {
	x, ok := identity.(*age.X25519Identity)
	if !ok {
		return nil, fmt.Errorf("pg secretstore: identity is %T; use *age.X25519Identity", identity)
	}
	return kek.NewLocal(x)
}

// Name identifies this backend for audit and UI: "pg", or in store mode the
// external store's name ("vaultkv").
func (s *Store) Name() string {
	if s.writeExt {
		return s.ext.Name()
	}
	return "pg"
}

// ExternalName names the configured external store ("vaultkv", "azurekv"),
// or "" when there is none.
func (s *Store) ExternalName() string {
	if s.ext == nil {
		return ""
	}
	return s.ext.Name()
}

// StoresExternally describes the external store every write goes to ("Vault
// at vault.example:8200"), or "" in local mode.
func (s *Store) StoresExternally() string {
	if s.writeExt {
		return s.ext.Describe()
	}
	return ""
}

// For returns a view scoped to owner — see secretstore.Store.For's doc
// comment for the fallback/isolation contract. A shallow copy: owner is the
// only field that differs, so every view over the same *pgxpool.Pool sees
// the same rows, just through a different (owned_by) lens.
func (s *Store) For(owner string) secretstore.Store {
	cp := *s
	cp.owner = owner
	return &cp
}

// rowRef names a row in an error: its owner and name, never its value.
func rowRef(owner, name string) string {
	return fmt.Sprintf("(owned_by=%q, name=%q)", owner, name)
}

// Put seals value in a fresh envelope and upserts it, scoped to this view's
// owner. A replace re-keys the row: new DEK, new nonces. A different owner
// holding the same name is a DIFFERENT row (migration 0050) and is never
// touched. Nothing is written unless the whole envelope was built, so a failed
// wrap leaves any existing row as it was.
func (s *Store) Put(ctx context.Context, name string, value []byte) error {
	if s.writeExt {
		return s.putExternal(ctx, name, value)
	}
	wrapped, ct, err := seal(ctx, s.kek, s.owner, name, value)
	if err != nil {
		return fmt.Errorf("pg secretstore: seal %s: %w", rowRef(s.owner, name), err)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO secrets (owned_by, name, enc_version, kek_id, wrapped_dek, ciphertext)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (owned_by, name) DO UPDATE
			SET enc_version=$3, kek_id=$4, wrapped_dek=$5, ciphertext=$6, updated_at=now()`,
		s.owner, name, encVersion, s.kek.ID(), wrapped, ct,
	)
	if err != nil {
		return fmt.Errorf("pg secretstore: put %s: %w", rowRef(s.owner, name), err)
	}
	return nil
}

// seal builds one v1 envelope: a fresh DEK seals value under AAD_secret, then
// k wraps the DEK bound to the same (owner, name).
func seal(ctx context.Context, k kek.KEK, owner, name string, value []byte) (wrapped, ct []byte, err error) {
	dek := make([]byte, kek.DEKSize)
	if _, err := rand.Read(dek); err != nil {
		return nil, nil, fmt.Errorf("draw data key: %w", err)
	}
	if ct, err = kek.Seal(dek, value, secretAAD(owner, name)); err != nil {
		return nil, nil, fmt.Errorf("seal value: %w", err)
	}
	if wrapped, err = k.Wrap(ctx, dek, kek.Bind(owner, name)); err != nil {
		return nil, nil, fmt.Errorf("wrap data key: %w", err)
	}
	return wrapped, ct, nil
}

// secretAAD is AAD_secret = Encode("wardyn/secret/v1", owned_by, name).
func secretAAD(owner, name string) []byte {
	return kek.Encode(secretAADLabel, owner, name)
}

// envelope is one row as read back: owned_by is the ROW's owner (the operator's
// "" on a fallback read), which is what both AADs are checked against.
type envelope struct {
	ownedBy, name string
	version       int16
	kekID         string
	wrapped, ct   []byte
}

// Get retrieves and opens a secret by name: this view's own (owner, name)
// row if one exists, else the operator's ("", name) row — a member with no
// key of their own resolves the operator's, exactly as every caller did
// before For existed. For owner="" the IN clause names "" twice, so only the
// operator row can ever match.
// Returns an error wrapping pgx.ErrNoRows and secretstore.ErrNotFound when
// absent, and ONLY then: a row that exists but will not open is a distinct
// error, so loadOrCreateSecret can never mistake a tampered boot key for a
// missing one and mint over it.
func (s *Store) Get(ctx context.Context, name string) ([]byte, error) {
	e := envelope{name: name}
	err := s.pool.QueryRow(ctx,
		`SELECT owned_by, enc_version, kek_id, wrapped_dek, ciphertext FROM secrets
		  WHERE owned_by IN ('', $1) AND name=$2 ORDER BY (owned_by = $1) DESC LIMIT 1`,
		s.owner, name,
	).Scan(&e.ownedBy, &e.version, &e.kekID, &e.wrapped, &e.ct)
	if errors.Is(err, pgx.ErrNoRows) {
		// Satisfy BOTH the seam sentinel (secretstore.ErrNotFound, what the
		// conformance suite + callers check) and the historical pgx.ErrNoRows
		// match (existing tests + cmd/wardynd loadOrCreateSecret) via errors.Join.
		return nil, fmt.Errorf("pg secretstore: secret %q not found: %w", name,
			errors.Join(secretstore.ErrNotFound, pgx.ErrNoRows))
	}
	if err != nil {
		return nil, fmt.Errorf("pg secretstore: get %s: %w", rowRef(s.owner, name), err)
	}
	return s.open(ctx, e)
}

// open checks and decrypts one row. Every refusal is fail-closed and names the
// row; none wraps a not-found sentinel.
func (s *Store) open(ctx context.Context, e envelope) ([]byte, error) {
	ref := rowRef(e.ownedBy, e.name)
	switch {
	case e.version == 0:
		return nil, fmt.Errorf("pg secretstore: %s is a pre-envelope (v0) row written after this database was converted — an older wardynd is still writing to it; stop every older replica, then restart this one to convert the row", ref)
	case e.version == extVersion:
		return s.openExternal(ctx, e)
	case e.version != encVersion:
		return nil, fmt.Errorf("pg secretstore: row %s "+unknownVersion, ref, e.version)
	case s.kek == nil:
		return nil, fmt.Errorf("pg secretstore: %s is sealed under key %q, but this wardynd has no WARDYN_AGE_KEY — keep it set until `wardynd -migrate-secrets` reports none left", ref, e.kekID)
	case e.kekID != s.kek.ID():
		return nil, fmt.Errorf("pg secretstore: %s is sealed under key %q, but this wardynd is configured with %q", ref, e.kekID, s.kek.ID())
	}
	// An older wardynd's replace is `SET ciphertext=` alone: it leaves this
	// row's v1 columns in place around an age payload. Conversion never revisits
	// a v1 row, so say what happened instead of calling it tampering.
	if bytes.HasPrefix(e.ct, ageHeader) {
		return nil, fmt.Errorf("pg secretstore: %s was overwritten in place by an older wardynd (it holds an age payload under v1 columns) — an older wardynd is still writing to this database; stop every older replica, then set this secret again", ref)
	}
	dek, err := s.kek.Unwrap(ctx, e.wrapped, kek.Bind(e.ownedBy, e.name))
	if err != nil {
		return nil, fmt.Errorf("pg secretstore: %s refused — its data key does not unwrap for this row (moved, forged or corrupted): %w", ref, err)
	}
	plain, err := kek.Open(dek, e.ct, secretAAD(e.ownedBy, e.name))
	if err != nil {
		return nil, fmt.Errorf("pg secretstore: %s refused — its value fails the integrity check for this row (moved, forged or corrupted): %w", ref, err)
	}
	return plain, nil
}

// Delete removes this view's own (owner, name) row. Idempotent (no-op if
// absent) and never touches a different owner's row of the same name —
// deleting a member's row leaves the operator's readable, and a member can
// never reach another member's row to delete it in the first place.
func (s *Store) Delete(ctx context.Context, name string) error {
	if err := s.deleteExternal(ctx, name); err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM secrets WHERE owned_by=$1 AND name=$2`, s.owner, name); err != nil {
		return fmt.Errorf("pg secretstore: delete %s: %w", rowRef(s.owner, name), err)
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

// Rekey rewraps every row's data key from the local KEK of oldID to that of
// newID and returns how many rows it rewrapped. It is the body of wardynd's
// `-rotate-age-key` maintenance mode (cmd/wardynd's rotateAgeKeyMode) and is NOT
// part of the secretstore.Store seam: the Store contract is per-name late-bound
// access, while this is a whole-table administrative operation. Only
// wrapped_dek and kek_id change; the sealed value (and its DEK) is untouched,
// so a rotation never decrypts a credential.
//
// Pointer rows (store mode) hold nothing under the age key and are left alone.
//
// ALL-OR-NOTHING. One transaction: any row that is not a v1 row under the old
// key, or whose data key does not unwrap, aborts the whole thing — the returned
// error names the row and how far it had got, and nothing is committed, so every
// secret is still readable with the OLD key. A v0 row aborts too: the serving
// boot converts those (ConvertV0), and a rotation is not a conversion.
//
// The FOR UPDATE on the select buys lost-update prevention, NOT exclusivity: it
// holds the rows it read, so a concurrent Put of one of those names waits and
// lands AFTER the commit instead of being clobbered by this transaction's
// rewrap of the envelope it replaced. It does NOT keep rows out from under the
// retired key — under READ COMMITTED a Put of a NEW name inserts straight past
// these locks, and a queued Put of an existing name still writes its old-key
// wrap once released. That every committed row is readable with newID is
// carried by the offline requirement below, not by the lock.
//
// The caller supplies BOTH identities: the daemon must be offline (its in-memory
// Store still holds the old KEK), and the caller is responsible for persisting
// newID before a restart and for emitting the secret.rekey audit event.
func Rekey(ctx context.Context, pool *pgxpool.Pool, oldID, newID age.Identity) (int, error) {
	from, err := localKEK(oldID)
	if err != nil {
		return 0, fmt.Errorf("pg secretstore: rekey old identity: %w", err)
	}
	to, err := localKEK(newID)
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

	// ORDER BY owned_by, name keeps the lock/abort order stable and readable;
	// the UPDATE below keys on BOTH columns, since two owners can share a name
	// (migration 0050).
	rows, err := tx.Query(ctx, `SELECT owned_by, name, enc_version, kek_id, wrapped_dek FROM secrets WHERE enc_version <> $1 ORDER BY owned_by, name FOR UPDATE`, extVersion)
	if err != nil {
		return 0, fmt.Errorf("pg secretstore: rekey select: %w", err)
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (envelope, error) {
		var e envelope
		err := r.Scan(&e.ownedBy, &e.name, &e.version, &e.kekID, &e.wrapped)
		return e, err
	})
	if err != nil {
		return 0, fmt.Errorf("pg secretstore: rekey scan: %w", err)
	}

	for i, e := range all {
		wrapped, rerr := rewrap(ctx, from, to, e)
		if rerr != nil {
			return 0, rekeyAbort(i, len(all), rowRef(e.ownedBy, e.name), rerr)
		}
		if _, uerr := tx.Exec(ctx,
			`UPDATE secrets SET kek_id=$3, wrapped_dek=$4 WHERE owned_by=$1 AND name=$2`, e.ownedBy, e.name, to.ID(), wrapped,
		); uerr != nil {
			return 0, rekeyAbort(i, len(all), rowRef(e.ownedBy, e.name), fmt.Errorf("update: %w", uerr))
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("pg secretstore: rekey commit (%d rows, NOTHING committed — the old key still reads every secret): %w", len(all), err)
	}
	return len(all), nil
}

// rewrap moves one row's data key from one KEK to another.
func rewrap(ctx context.Context, from, to kek.KEK, e envelope) ([]byte, error) {
	switch {
	case e.version == 0:
		return nil, fmt.Errorf("is a pre-envelope (v0) row — boot this wardynd once to convert it before rotating")
	case e.version != encVersion:
		return nil, fmt.Errorf(unknownVersion, e.version)
	case e.kekID != from.ID():
		return nil, fmt.Errorf("is sealed under key %q, not the old key %q", e.kekID, from.ID())
	}
	bind := kek.Bind(e.ownedBy, e.name)
	dek, err := from.Unwrap(ctx, e.wrapped, bind)
	if err != nil {
		return nil, fmt.Errorf("unwrap with the old key: %w", err)
	}
	wrapped, err := to.Wrap(ctx, dek, bind)
	if err != nil {
		return nil, fmt.Errorf("wrap with the new key: %w", err)
	}
	return wrapped, nil
}

// beginReadCommitted starts a transaction on pool pinned to READ COMMITTED.
//
// Rekey and ConvertV0 each rewrite EVERY row they select under one transaction,
// so it must not inherit default_transaction_isolation: on a pool set to
// REPEATABLE READ a long rewrite takes a snapshot at its first statement and
// then holds it for the whole rewrite, which turns any concurrent writer into a
// serialization failure reported as an abort — and a ConvertV0 queued behind
// another's advisory lock would select the rows that one already converted.
// READ COMMITTED is also exactly the isolation the FOR UPDATE lock reasoning
// above is written against.
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
// row, and how far it had got — plus the load-bearing fact that the abort left
// the store untouched, which is what tells an operator to fix the row and retry
// rather than hunt for a half-rotated store.
func rekeyAbort(done, total int, ref string, err error) error {
	return fmt.Errorf("pg secretstore: rekey ABORTED after %d of %d rows (nothing committed — every secret is still readable with the OLD key): %s %w",
		done, total, ref, err)
}
