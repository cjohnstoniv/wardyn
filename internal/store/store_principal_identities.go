// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The identity row every successful sign-in leaves (migration 0113).
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// PrincipalIdentity is one human identity Wardyn has seen sign in. An Entra identity is
// (Issuer, TenantID, ObjectID); any other issuer has an empty TenantID and ObjectID and is keyed by
// (Issuer, Principal). Principal is "" on a row written before the person's first sign-in.
type PrincipalIdentity struct {
	ID         uuid.UUID
	Principal  string
	Issuer     string
	TenantID   string
	ObjectID   string
	EmailLower string
	// ScimExternalID and ScimUserName are the SCIM projection, not binding columns.
	ScimExternalID string
	ScimUserName   string
	DeactivatedAt  *time.Time
	AuthorityEpoch int64
	PurgeAfter     *time.Time
	PurgedAt       *time.Time
	CreatedAt      time.Time
	LastLoginAt    *time.Time
}

// LoginIdentity is what an approved sign-in tells the store.
type LoginIdentity struct {
	Principal string
	Issuer    string
	TenantID  string
	ObjectID  string
	Email     string
}

// PrincipalIdentityStore is optional, like PersonStore: it is not part of Store, so callers
// type-assert and treat its absence as "nothing recorded".
type PrincipalIdentityStore interface {
	// UpsertLoginIdentity records an approved sign-in: it creates the identity row, or finds the
	// one for the same (issuer, tenant, object id) on Entra and (issuer, principal) elsewhere, binds
	// its principal if it has none, stamps the sign-in time and keeps the email as an alias. A row's
	// principal, issuer, tenant and object id are never changed once bound; a sign-in that would
	// change one writes nothing: ErrIdentityBindingMismatch when its row is bound to another
	// principal, ErrConflict when it names no row at all.
	UpsertLoginIdentity(ctx context.Context, in LoginIdentity, now time.Time) (PrincipalIdentity, error)
	// GetIdentityByObject is the row for exactly (issuer, tenantID, objectID), or ErrNotFound. An
	// empty objectID is always ErrNotFound.
	GetIdentityByObject(ctx context.Context, issuer, tenantID, objectID string) (PrincipalIdentity, error)
	// IdentitiesByPrincipal is every row bound to principal, oldest first (one per issuer it has
	// signed in on); empty when it has none.
	IdentitiesByPrincipal(ctx context.Context, principal string) ([]PrincipalIdentity, error)
}

var _ PrincipalIdentityStore = PG{}

const principalIdentityCols = `id, COALESCE(principal, ''), issuer, tenant_id, object_id, email_lower,
	scim_external_id, scim_user_name, deactivated_at, authority_epoch, purge_after, purged_at, created_at, last_login_at`

func scanPrincipalIdentity(row pgx.Row) (PrincipalIdentity, error) {
	var p PrincipalIdentity
	err := row.Scan(&p.ID, &p.Principal, &p.Issuer, &p.TenantID, &p.ObjectID, &p.EmailLower,
		&p.ScimExternalID, &p.ScimUserName, &p.DeactivatedAt, &p.AuthorityEpoch, &p.PurgeAfter, &p.PurgedAt, &p.CreatedAt, &p.LastLoginAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return PrincipalIdentity{}, ErrNotFound
	}
	if err != nil {
		return PrincipalIdentity{}, fmt.Errorf("store: scan principal identity: %w", err)
	}
	return p, nil
}

func (s PG) UpsertLoginIdentity(ctx context.Context, in LoginIdentity, now time.Time) (PrincipalIdentity, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return PrincipalIdentity{}, fmt.Errorf("store: upsert login identity: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	out, err := upsertLoginIdentityTx(ctx, tx, in, now)
	if err != nil {
		return PrincipalIdentity{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return PrincipalIdentity{}, fmt.Errorf("store: commit login identity: %w", err)
	}
	return out, nil
}

// upsertLoginIdentityTx is UpsertLoginIdentity on the caller's transaction, so the sign-in gate can
// hold the row it writes while it reads the row's deactivation and epoch.
func upsertLoginIdentityTx(ctx context.Context, tx pgx.Tx, in LoginIdentity, now time.Time) (PrincipalIdentity, error) {
	if in.Principal == "" || in.Issuer == "" {
		return PrincipalIdentity{}, errors.New("store: login identity needs a principal and an issuer")
	}
	email := strings.ToLower(strings.TrimSpace(in.Email))
	var id uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO principal_identities (principal, issuer, tenant_id, object_id, email_lower, created_at, last_login_at)
		VALUES ($1, $2, $3, $4, $5, $6, $6)
		ON CONFLICT DO NOTHING
		RETURNING id`, in.Principal, in.Issuer, in.TenantID, in.ObjectID, email, now).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		// The identity is already known. An object id names it on Entra, the principal elsewhere.
		lookup, args := `SELECT id, principal FROM principal_identities WHERE issuer = $1 AND principal = $2 AND object_id = '' FOR UPDATE`,
			[]any{in.Issuer, in.Principal}
		if in.ObjectID != "" {
			lookup, args = `SELECT id, principal FROM principal_identities WHERE issuer = $1 AND tenant_id = $2 AND object_id = $3 FOR UPDATE`,
				[]any{in.Issuer, in.TenantID, in.ObjectID}
		}
		var principal *string
		if err = tx.QueryRow(ctx, lookup, args...).Scan(&id, &principal); errors.Is(err, pgx.ErrNoRows) {
			return PrincipalIdentity{}, fmt.Errorf("store: login identity clashes with another binding: %w", ErrConflict)
		}
		if err != nil {
			return PrincipalIdentity{}, fmt.Errorf("store: upsert login identity: %w", err)
		}
		// A session under another principal than the bound one would escape the identity's checks.
		if principal != nil && *principal != in.Principal {
			return PrincipalIdentity{}, fmt.Errorf("store: login identity is bound to %q, not %q: %w", *principal, in.Principal, ErrIdentityBindingMismatch)
		}
		// The only write to a binding column: it fills a principal that is still unset.
		if _, err = tx.Exec(ctx, `UPDATE principal_identities SET principal = $2 WHERE id = $1 AND principal IS NULL`, id, in.Principal); err != nil {
			return PrincipalIdentity{}, fmt.Errorf("store: bind login identity: %w", err)
		}
		if _, err = tx.Exec(ctx, `
			UPDATE principal_identities
			   SET email_lower = CASE WHEN $2 <> '' THEN $2 ELSE email_lower END, last_login_at = $3
			 WHERE id = $1`, id, email, now); err != nil {
			return PrincipalIdentity{}, fmt.Errorf("store: stamp login identity: %w", err)
		}
	} else if err != nil {
		return PrincipalIdentity{}, fmt.Errorf("store: insert login identity: %w", err)
	}
	if email != "" {
		if _, err = tx.Exec(ctx, `
			INSERT INTO principal_identity_aliases (identity_id, kind, value_lower, source, first_seen, last_seen)
			VALUES ($1, 'email', $2, 'login', $3, $3)
			ON CONFLICT (identity_id, kind, value_lower) DO UPDATE SET last_seen = EXCLUDED.last_seen`, id, email, now); err != nil {
			return PrincipalIdentity{}, fmt.Errorf("store: record email alias: %w", err)
		}
	}
	return scanPrincipalIdentity(tx.QueryRow(ctx, `SELECT `+principalIdentityCols+` FROM principal_identities WHERE id = $1`, id))
}

func (s PG) GetIdentityByObject(ctx context.Context, issuer, tenantID, objectID string) (PrincipalIdentity, error) {
	if objectID == "" {
		return PrincipalIdentity{}, ErrNotFound
	}
	return scanPrincipalIdentity(s.Pool.QueryRow(ctx, `SELECT `+principalIdentityCols+`
		FROM principal_identities WHERE issuer = $1 AND tenant_id = $2 AND object_id = $3`, issuer, tenantID, objectID))
}

func (s PG) IdentitiesByPrincipal(ctx context.Context, principal string) ([]PrincipalIdentity, error) {
	return collect(ctx, s.Pool, "list", "principal identities", `SELECT `+principalIdentityCols+`
		FROM principal_identities WHERE principal = $1 ORDER BY created_at, id`, []any{principal}, scanPrincipalIdentity)
}

// ErrIdentityNotRebindable is UnbindIdentity's refusal of a row with no object id: such a row is found only by
// its principal, so an unbound one is never reached again and the next sign-in would create a second row
// without this one's epoch or SCIM linkage.
var ErrIdentityNotRebindable = errors.New("store: identity has no object id and cannot be re-bound")

// PrincipalInUseError is UnbindIdentity's refusal while the principal it would release still holds something
// the leaver flow reaches only through the row's principal.
type PrincipalInUseError struct{ APITokens, SSHKeys, ActiveRuns int }

func (e *PrincipalInUseError) Error() string {
	return fmt.Sprintf("store: the principal still holds %d API token(s), %d SSH key(s) and %d active run(s)", e.APITokens, e.SSHKeys, e.ActiveRuns)
}

// IdentityUnbinder is the optional store seam behind POST /admin/identities/{id}/unbind.
type IdentityUnbinder interface {
	GetIdentity(ctx context.Context, id uuid.UUID) (PrincipalIdentity, error)
	// UnbindIdentity clears the principal of identity id, so the next sign-in binds the row afresh, and
	// cuts the released principal's sessions as a suspension does. principal is the one the caller read the
	// row bound to: a row bound to another by the time it is locked is ErrIdentityRebound. Refused, with
	// nothing written: a deactivated or purged row (ErrIdentityDeactivated), a row with no principal
	// (ErrConflict), a row with no object id (ErrIdentityNotRebindable), a principal that still holds an API
	// token, SSH key or non-terminal run (*PrincipalInUseError), a missing row (ErrNotFound). Once the row is
	// re-bound, deactivation and the kill sweep reach only the new principal, so the old one must hold
	// nothing. The epoch, the deactivation columns and the SCIM linkage are untouched.
	UnbindIdentity(ctx context.Context, id uuid.UUID, principal string) error
}

var _ IdentityUnbinder = PG{}

func (s PG) UnbindIdentity(ctx context.Context, id uuid.UUID, principal string) error {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("store: unbind identity: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	row, err := scanPrincipalIdentity(tx.QueryRow(ctx, `SELECT `+principalIdentityCols+` FROM principal_identities WHERE id = $1 FOR UPDATE`, id))
	switch {
	case err != nil:
		return err
	case row.DeactivatedAt != nil || row.PurgedAt != nil:
		return ErrIdentityDeactivated
	case row.Principal == "":
		return fmt.Errorf("store: identity is not bound to a principal: %w", ErrConflict)
	case row.Principal != principal:
		return ErrIdentityRebound
	case row.ObjectID == "":
		return ErrIdentityNotRebindable
	}
	held := &PrincipalInUseError{}
	if err = tx.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM api_tokens WHERE principal = $1 AND `+apiTokenLive+`),
			(SELECT count(*) FROM ssh_public_keys WHERE principal = $1),
			(SELECT count(*) FROM agent_runs WHERE created_by = $1 AND state = ANY($2))`,
		principal, nonTerminalStateNames()).Scan(&held.APITokens, &held.SSHKeys, &held.ActiveRuns); err != nil {
		return fmt.Errorf("store: count what the principal holds: %w", err)
	}
	if *held != (PrincipalInUseError{}) {
		return held
	}
	if _, err = tx.Exec(ctx, `UPDATE principal_identities SET principal = NULL WHERE id = $1`, id); err != nil {
		return fmt.Errorf("store: unbind identity: %w", err)
	}
	// As a suspension does, so a cookie the released sub still holds dies with its binding.
	if _, err = tx.Exec(ctx, `
		INSERT INTO oidc_session_revocations (sub, revoked_at) VALUES ($1, clock_timestamp())
		ON CONFLICT (sub) DO UPDATE SET revoked_at = EXCLUDED.revoked_at`, principal); err != nil {
		return fmt.Errorf("store: session cutoff: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit identity unbind: %w", err)
	}
	return nil
}
