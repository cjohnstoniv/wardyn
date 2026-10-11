// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

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

// ErrServiceNameTaken is CreateServiceIdentity's refusal of a name a principal-keyed table already holds
// (or a service identity already carries), so no new identity inherits another's grants or history.
var ErrServiceNameTaken = errors.New("store: that service identity name is already in use")

// ErrServiceSponsorInvalid is the refusal of a sponsor that is not exactly the principal of a bound,
// non-deactivated identity.
var ErrServiceSponsorInvalid = errors.New("store: the sponsor is not a bound, active identity")

// ErrServiceIdentityInactive is the refusal of a write to a service identity that is retired, or, for a
// token mint, suspended or sponsored by someone else than the minter.
var ErrServiceIdentityInactive = errors.New("store: the service identity is not active")

// ServiceIdentityStore holds service identities (migration 0157, #1508). Optional, like PersonStore: the
// API answers 501 on a store without it.
type ServiceIdentityStore interface {
	// CreateServiceIdentity inserts si, or refuses with ErrServiceNameTaken or ErrServiceSponsorInvalid.
	CreateServiceIdentity(ctx context.Context, si types.ServiceIdentity) (types.ServiceIdentity, error)
	// GetServiceIdentity is the row named name, or ErrNotFound.
	GetServiceIdentity(ctx context.Context, name string) (types.ServiceIdentity, error)
	// ListServiceIdentities returns identities by name, narrowed to sponsor when it is not empty.
	ListServiceIdentities(ctx context.Context, sponsor string, p Page) ([]types.ServiceIdentity, error)
	// UpdateServiceIdentity applies ch to a non-retired identity. A sponsor change sets sponsor_since and
	// revokes every live token of the identity in the same transaction. ErrNotFound, ErrServiceIdentityInactive
	// (retired) and ErrServiceSponsorInvalid are the refusals.
	UpdateServiceIdentity(ctx context.Context, name string, ch ServiceIdentityChange) (ServiceIdentityChanged, error)
	// RetireServiceIdentity retires a non-retired identity and revokes its live tokens in one transaction.
	RetireServiceIdentity(ctx context.Context, name, by string, now time.Time) (ServiceIdentityChanged, error)
	// CreateServiceToken inserts t, a token of service:<name>, only while the identity is active and its
	// sponsor is still sponsor: ErrNotFound for no such identity, ErrServiceIdentityInactive otherwise.
	CreateServiceToken(ctx context.Context, name, sponsor string, t types.APIToken, raw string) (types.APIToken, error)
}

var _ ServiceIdentityStore = PG{}

// ServiceIdentityChange is a PATCH: the fields that are not nil change.
type ServiceIdentityChange struct {
	Description *string
	Sponsor     *string
	Suspended   *bool
	By          string
	Now         time.Time
}

// ServiceIdentityChanged is what a committed change did.
type ServiceIdentityChanged struct {
	Before, After types.ServiceIdentity
	TokensRevoked int
}

const serviceIdentityCols = `name, description, sponsor, sponsor_since, user_type, created_by, created_at,
	suspended_at, COALESCE(suspended_by, ''), retired_at, COALESCE(retired_by, '')`

func scanServiceIdentity(row pgx.Row) (types.ServiceIdentity, error) {
	var si types.ServiceIdentity
	err := row.Scan(&si.Name, &si.Description, &si.Sponsor, &si.SponsorSince, &si.UserType, &si.CreatedBy, &si.CreatedAt,
		&si.SuspendedAt, &si.SuspendedBy, &si.RetiredAt, &si.RetiredBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.ServiceIdentity{}, ErrNotFound
	}
	if err != nil {
		return types.ServiceIdentity{}, fmt.Errorf("store: scan service identity: %w", err)
	}
	si.State = si.BaseState()
	return si, nil
}

// servicePrincipalProbes are the principal-keyed columns a new service name must not appear in, the list
// migration 0157 refuses an upgrade over. A grant or secret written early would otherwise be inherited.
var servicePrincipalProbes = [][3]string{
	{"principal_identities", "principal", "true"},
	{"people", "principal", "true"},
	{"api_tokens", "principal", "true"},
	{"ssh_public_keys", "principal", "true"},
	{"agent_runs", "created_by", "true"},
	{"workspaces", "owned_by", "true"},
	{"secrets", "owned_by", "true"},
	{"capability_grants", "subject", "subject_type = 'user'"},
	{"governance_assignments", "subject", "subject_type = 'user'"},
}

// sponsorBound locks the sponsor's identity row FOR SHARE, so the unbind that would release it waits for
// this transaction, and reports whether a bound, non-deactivated, non-purged identity holds it exactly.
func sponsorBound(ctx context.Context, tx pgx.Tx, sponsor string) (bool, error) {
	var one int
	err := tx.QueryRow(ctx, `SELECT 1 FROM principal_identities
		WHERE principal = $1 AND deactivated_at IS NULL AND purged_at IS NULL LIMIT 1 FOR SHARE`, sponsor).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: check sponsor: %w", err)
	}
	return true, nil
}

func (s PG) CreateServiceIdentity(ctx context.Context, si types.ServiceIdentity) (types.ServiceIdentity, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return types.ServiceIdentity{}, fmt.Errorf("store: create service identity: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	principal := si.Principal()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('service_identities:' || $1, 0))`, si.Name); err != nil {
		return types.ServiceIdentity{}, fmt.Errorf("store: create service identity: %w", err)
	}
	for _, p := range servicePrincipalProbes {
		var held bool
		q := fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %s WHERE lower(%s) = $1 AND %s)`, p[0], p[1], p[2])
		if err := tx.QueryRow(ctx, q, principal).Scan(&held); err != nil {
			return types.ServiceIdentity{}, fmt.Errorf("store: probe %s: %w", p[0], err)
		}
		if held {
			return types.ServiceIdentity{}, ErrServiceNameTaken
		}
	}
	if ok, err := sponsorBound(ctx, tx, si.Sponsor); err != nil {
		return types.ServiceIdentity{}, err
	} else if !ok {
		return types.ServiceIdentity{}, ErrServiceSponsorInvalid
	}
	out, err := scanServiceIdentity(tx.QueryRow(ctx, `
		INSERT INTO service_identities (name, description, sponsor, sponsor_since, user_type, created_by, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $4) RETURNING `+serviceIdentityCols,
		si.Name, si.Description, si.Sponsor, si.CreatedAt, si.UserType, si.CreatedBy))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return types.ServiceIdentity{}, ErrServiceNameTaken
	}
	if err != nil {
		return types.ServiceIdentity{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return types.ServiceIdentity{}, fmt.Errorf("store: commit service identity: %w", err)
	}
	return out, nil
}

func (s PG) GetServiceIdentity(ctx context.Context, name string) (types.ServiceIdentity, error) {
	return scanServiceIdentity(s.Pool.QueryRow(ctx, `SELECT `+serviceIdentityCols+` FROM service_identities WHERE name = $1`, name))
}

func (s PG) ListServiceIdentities(ctx context.Context, sponsor string, p Page) ([]types.ServiceIdentity, error) {
	q, args := p.appendTo(`SELECT `+serviceIdentityCols+` FROM service_identities WHERE ($1 = '' OR sponsor = $1) ORDER BY name`, []any{sponsor})
	return collect(ctx, s.Pool, "list", "service identities", q, args, scanServiceIdentity)
}

// lockServiceIdentity reads name FOR UPDATE inside tx: ErrNotFound, or ErrServiceIdentityInactive when retired.
func lockServiceIdentity(ctx context.Context, tx pgx.Tx, name string) (types.ServiceIdentity, error) {
	si, err := scanServiceIdentity(tx.QueryRow(ctx, `SELECT `+serviceIdentityCols+` FROM service_identities WHERE name = $1 FOR UPDATE`, name))
	if err != nil {
		return types.ServiceIdentity{}, err
	}
	if si.RetiredAt != nil {
		return si, ErrServiceIdentityInactive
	}
	return si, nil
}

// revokeServiceTokens revokes every live token of service:<name> at now, returning how many.
func revokeServiceTokens(ctx context.Context, tx pgx.Tx, name string, now time.Time) (int, error) {
	tag, err := tx.Exec(ctx, `UPDATE api_tokens SET revoked_at = $2 WHERE principal = $1 AND revoked_at IS NULL`,
		types.ServicePrincipalPrefix+name, now)
	if err != nil {
		return 0, fmt.Errorf("store: revoke service tokens: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

func (s PG) UpdateServiceIdentity(ctx context.Context, name string, ch ServiceIdentityChange) (ServiceIdentityChanged, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return ServiceIdentityChanged{}, fmt.Errorf("store: update service identity: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	before, err := lockServiceIdentity(ctx, tx, name)
	if err != nil {
		return ServiceIdentityChanged{}, err
	}
	out := ServiceIdentityChanged{Before: before}
	if ch.Description != nil {
		if _, err = tx.Exec(ctx, `UPDATE service_identities SET description = $2 WHERE name = $1`, name, *ch.Description); err != nil {
			return ServiceIdentityChanged{}, fmt.Errorf("store: update service identity: %w", err)
		}
	}
	if ch.Sponsor != nil && *ch.Sponsor != before.Sponsor {
		if ok, err := sponsorBound(ctx, tx, *ch.Sponsor); err != nil {
			return ServiceIdentityChanged{}, err
		} else if !ok {
			return ServiceIdentityChanged{}, ErrServiceSponsorInvalid
		}
		// The cutoff is set in the same transaction as the revoke, so a mint whose row commits after the
		// revoke's snapshot still carries a created_at at or before it and never authenticates.
		if _, err = tx.Exec(ctx, `UPDATE service_identities SET sponsor = $2, sponsor_since = $3 WHERE name = $1`, name, *ch.Sponsor, ch.Now); err != nil {
			return ServiceIdentityChanged{}, fmt.Errorf("store: update service identity: %w", err)
		}
		if out.TokensRevoked, err = revokeServiceTokens(ctx, tx, name, ch.Now); err != nil {
			return ServiceIdentityChanged{}, err
		}
	}
	switch {
	case ch.Suspended != nil && *ch.Suspended && before.SuspendedAt == nil:
		_, err = tx.Exec(ctx, `UPDATE service_identities SET suspended_at = $2, suspended_by = $3 WHERE name = $1`, name, ch.Now, ch.By)
	case ch.Suspended != nil && !*ch.Suspended && before.SuspendedAt != nil:
		_, err = tx.Exec(ctx, `UPDATE service_identities SET suspended_at = NULL, suspended_by = NULL WHERE name = $1`, name)
	}
	if err != nil {
		return ServiceIdentityChanged{}, fmt.Errorf("store: update service identity: %w", err)
	}
	if out.After, err = scanServiceIdentity(tx.QueryRow(ctx, `SELECT `+serviceIdentityCols+` FROM service_identities WHERE name = $1`, name)); err != nil {
		return ServiceIdentityChanged{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return ServiceIdentityChanged{}, fmt.Errorf("store: commit service identity update: %w", err)
	}
	return out, nil
}

func (s PG) RetireServiceIdentity(ctx context.Context, name, by string, now time.Time) (ServiceIdentityChanged, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return ServiceIdentityChanged{}, fmt.Errorf("store: retire service identity: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	before, err := lockServiceIdentity(ctx, tx, name)
	if err != nil {
		return ServiceIdentityChanged{}, err
	}
	after, err := scanServiceIdentity(tx.QueryRow(ctx, `
		UPDATE service_identities SET retired_at = $2, retired_by = $3 WHERE name = $1 RETURNING `+serviceIdentityCols, name, now, by))
	if err != nil {
		return ServiceIdentityChanged{}, err
	}
	revoked, err := revokeServiceTokens(ctx, tx, name, now)
	if err != nil {
		return ServiceIdentityChanged{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return ServiceIdentityChanged{}, fmt.Errorf("store: commit service identity retire: %w", err)
	}
	return ServiceIdentityChanged{Before: before, After: after, TokensRevoked: revoked}, nil
}

func (s PG) CreateServiceToken(ctx context.Context, name, sponsor string, t types.APIToken, raw string) (types.APIToken, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return types.APIToken{}, fmt.Errorf("store: create service token: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	// FOR SHARE: a sponsor change, suspend or retire waits for this insert and then revokes the row.
	si, err := scanServiceIdentity(tx.QueryRow(ctx, `SELECT `+serviceIdentityCols+` FROM service_identities WHERE name = $1 FOR SHARE`, name))
	if err != nil {
		return types.APIToken{}, err
	}
	if !si.Usable() || si.Sponsor != sponsor {
		return types.APIToken{}, ErrServiceIdentityInactive
	}
	out, err := s.insertAPIToken(ctx, tx, t, raw)
	if err != nil {
		return types.APIToken{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return types.APIToken{}, fmt.Errorf("store: commit service token: %w", err)
	}
	return out, nil
}
