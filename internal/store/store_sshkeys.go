// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// SSH gateway key registry (migration 0033). Kept out of store.go on purpose
// (it sits at a lint size boundary), mirroring store_sandbox_ref.go's split.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// AddSSHKey inserts a new registered key. Returns ErrConflict when the
// fingerprint (the PK) already exists — a unique_violation (23505) on this
// table means someone already registered that exact key material.
func (s PG) AddSSHKey(ctx context.Context, k types.SSHPublicKey) (types.SSHPublicKey, error) {
	const q = `
		INSERT INTO ssh_public_keys (fingerprint, principal, name, public_key, role, created_at)
		VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING fingerprint, principal, name, public_key, role, created_at`
	out, err := scanSSHKey(s.Pool.QueryRow(ctx, q, k.Fingerprint, k.Principal, k.Name, k.PublicKey, k.Role, k.CreatedAt))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return types.SSHPublicKey{}, ErrConflict
		}
		return types.SSHPublicKey{}, err
	}
	return out, nil
}

// ListSSHKeysByPrincipal returns principal's own registered keys, newest
// first — the self-service GET /me/ssh-keys list.
func (s PG) ListSSHKeysByPrincipal(ctx context.Context, principal string) ([]types.SSHPublicKey, error) {
	const q = `
		SELECT fingerprint, principal, name, public_key, role, created_at
		FROM ssh_public_keys WHERE principal = $1 ORDER BY created_at DESC`
	rows, err := s.Pool.Query(ctx, q, principal)
	if err != nil {
		return nil, fmt.Errorf("store: list ssh keys: %w", err)
	}
	defer rows.Close()
	out := []types.SSHPublicKey{}
	for rows.Next() {
		k, err := scanSSHKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list ssh keys: %w", err)
	}
	return out, nil
}

// GetSSHKeyByFingerprint is the gateway's pre-auth lookup: given the offered
// key's fingerprint, resolve which principal (if any) registered it — and with
// which role (0043), the gateway's admin-override signal.
// Deliberately UNSCOPED by principal — the caller has not authenticated yet;
// this call is what authenticates them. Returns ErrNotFound when unregistered.
func (s PG) GetSSHKeyByFingerprint(ctx context.Context, fingerprint string) (types.SSHPublicKey, error) {
	const q = `
		SELECT fingerprint, principal, name, public_key, role, created_at
		FROM ssh_public_keys WHERE fingerprint = $1`
	return scanSSHKey(s.Pool.QueryRow(ctx, q, fingerprint))
}

// DeleteSSHKey removes fingerprint, scoped to principal so a human can only
// ever delete their OWN key. Returns ErrNotFound both when the fingerprint
// doesn't exist and when it belongs to someone else — indistinguishable on
// purpose (no existence leak across principals).
func (s PG) DeleteSSHKey(ctx context.Context, fingerprint, principal string) error {
	tag, err := s.Pool.Exec(ctx,
		`DELETE FROM ssh_public_keys WHERE fingerprint = $1 AND principal = $2`, fingerprint, principal)
	if err != nil {
		return fmt.Errorf("store: delete ssh key: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func scanSSHKey(row pgx.Row) (types.SSHPublicKey, error) {
	var k types.SSHPublicKey
	err := row.Scan(&k.Fingerprint, &k.Principal, &k.Name, &k.PublicKey, &k.Role, &k.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.SSHPublicKey{}, ErrNotFound
	}
	if err != nil {
		return types.SSHPublicKey{}, fmt.Errorf("store: scan ssh key: %w", err)
	}
	return k, nil
}
