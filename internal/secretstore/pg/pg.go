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

// Put encrypts value with the age key and upserts the ciphertext into the
// secrets table. Duplicate names overwrite the previous ciphertext.
func (s *Store) Put(ctx context.Context, name string, value []byte) error {
	ct, err := s.encrypt(value)
	if err != nil {
		return fmt.Errorf("pg secretstore: encrypt %s: %w", name, err)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO secrets (name, ciphertext)
		VALUES ($1, $2)
		ON CONFLICT (name) DO UPDATE
			SET ciphertext=$2, updated_at=now()`,
		name, ct,
	)
	if err != nil {
		return fmt.Errorf("pg secretstore: put %s: %w", name, err)
	}
	return nil
}

// Get retrieves and decrypts a secret by name.
// Returns an error wrapping pgx.ErrNoRows (or a sentinel message) when absent.
func (s *Store) Get(ctx context.Context, name string) ([]byte, error) {
	var ct []byte
	err := s.pool.QueryRow(ctx,
		`SELECT ciphertext FROM secrets WHERE name=$1`, name,
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

// Delete removes a secret by name. Idempotent (no-op if absent).
func (s *Store) Delete(ctx context.Context, name string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM secrets WHERE name=$1`, name); err != nil {
		return fmt.Errorf("pg secretstore: delete %s: %w", name, err)
	}
	return nil
}

// List returns the names of all stored secrets in lexical order.
func (s *Store) List(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT name FROM secrets ORDER BY name`)
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
