// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// SSH gateway key registry (migration 0033). Kept out of store.go on purpose
// (it sits at a lint size boundary), mirroring store_sandbox_ref.go's split.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// AddSSHKey inserts a new registered key. Returns ErrConflict when the
// fingerprint (the PK) already exists — a unique_violation (23505) on this
// table means someone already registered that exact key material.
func (s PG) AddSSHKey(ctx context.Context, k types.SSHPublicKey) (types.SSHPublicKey, error) {
	const q = `
		INSERT INTO ssh_public_keys (fingerprint, principal, name, public_key, role, role_checked_at, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		RETURNING fingerprint, principal, name, public_key, role, role_checked_at, created_at`
	out, err := scanSSHKey(s.Pool.QueryRow(ctx, q, k.Fingerprint, k.Principal, k.Name, k.PublicKey, k.Role, k.RoleCheckedAt, k.CreatedAt))
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
		SELECT fingerprint, principal, name, public_key, role, role_checked_at, created_at
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
		SELECT fingerprint, principal, name, public_key, role, role_checked_at, created_at
		FROM ssh_public_keys WHERE fingerprint = $1`
	return scanSSHKey(s.Pool.QueryRow(ctx, q, fingerprint))
}

// RefreshSSHKeyRoles re-stamps role AND role_checked_at on every key owned by
// principal — the OIDC callback's OnLogin hook (migration 0046), fired on
// every successful login with that login's freshly-derived role. This is what
// narrows the admin-override stamp from "set once at registration, never
// touched again" to "at most WARDYN_SSH_ROLE_TTL stale": a login is a live
// read of the human's CURRENT role, more authoritative than whatever was true
// the day a given key was registered, so it overwrites role too, not only the
// timestamp — a demoted human's keys downgrade to member on their very next
// login, and a promoted human's keys upgrade the same way, with no
// delete-then-re-register needed. A principal with no registered keys is a
// normal, silent no-op (RowsAffected 0) — logging in has nothing to refresh.
func (s PG) RefreshSSHKeyRoles(ctx context.Context, principal, role string, checkedAt time.Time) error {
	_, err := s.Pool.Exec(ctx,
		`UPDATE ssh_public_keys SET role = $1, role_checked_at = $2 WHERE principal = $3`,
		role, checkedAt, principal)
	if err != nil {
		return fmt.Errorf("store: refresh ssh key roles: %w", err)
	}
	return nil
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
	err := row.Scan(&k.Fingerprint, &k.Principal, &k.Name, &k.PublicKey, &k.Role, &k.RoleCheckedAt, &k.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.SSHPublicKey{}, ErrNotFound
	}
	if err != nil {
		return types.SSHPublicKey{}, fmt.Errorf("store: scan ssh key: %w", err)
	}
	return k, nil
}
