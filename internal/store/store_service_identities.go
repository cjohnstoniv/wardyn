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

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ErrServiceNameTaken is CreateServiceIdentity's refusal of a name a principal-keyed table already holds
// (or a service identity already carries), so no new identity inherits another's grants or history.
var ErrServiceNameTaken = errors.New("store: that service identity name is already in use")

// ErrServiceSponsorInvalid is the refusal of a sponsor that is not exactly the principal of a bound,
// non-deactivated identity.
var ErrServiceSponsorInvalid = errors.New("store: the sponsor is not a bound, active identity")

// ErrServiceUserTypeUnknown is CreateServiceIdentity's refusal for a user type with no user_types row.
var ErrServiceUserTypeUnknown = errors.New("store: no such user type")

// ErrServiceTokenCap is CreateServiceToken's refusal when the identity already holds the most live tokens.
var ErrServiceTokenCap = errors.New("store: the service identity holds the maximum number of live tokens")

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
	RetireServiceIdentity(ctx context.Context, name, by string) (ServiceIdentityChanged, error)
	// CreateServiceToken inserts t, a token of service:<name>, only while the identity is active, its
	// sponsor is still sponsor and it holds fewer than maxLive live tokens, counted in the same
	// transaction under a row lock that serialises concurrent mints: ErrNotFound for no such identity,
	// ErrServiceIdentityInactive, or ErrServiceTokenCap.
	CreateServiceToken(ctx context.Context, name, sponsor string, maxLive int, t types.APIToken, raw string) (types.APIToken, error)
	// ServiceStillSponsored reports whether name is usable, still sponsored by sponsor, and not re-sponsored
	// since openedAt, an application-clock instant that is translated to the database clock sponsor_since
	// is written on.
	ServiceStillSponsored(ctx context.Context, name, sponsor string, openedAt time.Time) (bool, error)
}

var _ ServiceIdentityStore = PG{}

// ServiceIdentityChange is a PATCH: the fields that are not nil change.
type ServiceIdentityChange struct {
	Description *string
	Sponsor     *string
	Suspended   *bool
	By          string
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
func sponsorBound(ctx context.Context, tx Querier, sponsor string) (bool, error) {
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
	out, err := CreateServiceIdentityQ(ctx, tx, si)
	if err != nil {
		return types.ServiceIdentity{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return types.ServiceIdentity{}, fmt.Errorf("store: commit service identity: %w", err)
	}
	return out, nil
}

// CreateServiceIdentityQ is CreateServiceIdentity's statements on tx (a pgx.Tx), which a held governance
// change applies inside the decision transaction.
func CreateServiceIdentityQ(ctx context.Context, tx Querier, si types.ServiceIdentity) (types.ServiceIdentity, error) {
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
	// Checked before the INSERT so a taken name is a plain refusal, not a failed statement that would end tx.
	var taken bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM service_identities WHERE name = $1)`, si.Name).Scan(&taken); err != nil {
		return types.ServiceIdentity{}, fmt.Errorf("store: create service identity: %w", err)
	}
	if taken {
		return types.ServiceIdentity{}, ErrServiceNameTaken
	}
	if ok, err := sponsorBound(ctx, tx, si.Sponsor); err != nil {
		return types.ServiceIdentity{}, err
	} else if !ok {
		return types.ServiceIdentity{}, ErrServiceSponsorInvalid
	}
	// user_type has no foreign key: the lock orders this create against DeleteUserType, and the type is
	// read after it.
	var typeKnown bool
	if _, err := tx.Exec(ctx, userTypeWriteLock, si.UserType); err != nil {
		return types.ServiceIdentity{}, fmt.Errorf("store: create service identity: %w", err)
	}
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM user_types WHERE id = $1)`, si.UserType).Scan(&typeKnown); err != nil {
		return types.ServiceIdentity{}, fmt.Errorf("store: create service identity: %w", err)
	}
	if !typeKnown {
		return types.ServiceIdentity{}, ErrServiceUserTypeUnknown
	}
	out, err := scanServiceIdentity(tx.QueryRow(ctx, `
		INSERT INTO service_identities (name, description, sponsor, sponsor_since, user_type, created_by, created_at)
		VALUES ($1, $2, $3, clock_timestamp(), $4, $5, clock_timestamp()) RETURNING `+serviceIdentityCols,
		si.Name, si.Description, si.Sponsor, si.UserType, si.CreatedBy))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return types.ServiceIdentity{}, ErrServiceNameTaken
	}
	if err != nil {
		return types.ServiceIdentity{}, err
	}
	return out, nil
}

// GetServiceIdentityForUpdateQ reads name FOR UPDATE on tx (a pgx.Tx), retired or not; ErrNotFound when
// missing.
func GetServiceIdentityForUpdateQ(ctx context.Context, tx Querier, name string) (types.ServiceIdentity, error) {
	return scanServiceIdentity(tx.QueryRow(ctx, `SELECT `+serviceIdentityCols+` FROM service_identities WHERE name = $1 FOR UPDATE`, name))
}

func (s PG) GetServiceIdentity(ctx context.Context, name string) (types.ServiceIdentity, error) {
	return scanServiceIdentity(s.Pool.QueryRow(ctx, `SELECT `+serviceIdentityCols+` FROM service_identities WHERE name = $1`, name))
}

func (s PG) ListServiceIdentities(ctx context.Context, sponsor string, p Page) ([]types.ServiceIdentity, error) {
	q, args := p.appendTo(`SELECT `+serviceIdentityCols+` FROM service_identities WHERE ($1 = '' OR sponsor = $1) ORDER BY name`, []any{sponsor})
	return collect(ctx, s.Pool, "list", "service identities", q, args, scanServiceIdentity)
}

// lockServiceIdentity reads name FOR UPDATE inside tx: ErrNotFound, or ErrServiceIdentityInactive when retired.
func lockServiceIdentity(ctx context.Context, tx Querier, name string) (types.ServiceIdentity, error) {
	si, err := scanServiceIdentity(tx.QueryRow(ctx, `SELECT `+serviceIdentityCols+` FROM service_identities WHERE name = $1 FOR UPDATE`, name))
	if err != nil {
		return types.ServiceIdentity{}, err
	}
	if si.RetiredAt != nil {
		return si, ErrServiceIdentityInactive
	}
	return si, nil
}

// revokeServiceTokens revokes every live token of service:<name>, returning how many. Every stamp here
// is on the database clock, the clock a token's created_at is translated onto.
func revokeServiceTokens(ctx context.Context, tx Querier, name string) (int, error) {
	tag, err := tx.Exec(ctx, `UPDATE api_tokens SET revoked_at = clock_timestamp() WHERE principal = $1 AND revoked_at IS NULL`,
		types.ServicePrincipalPrefix+name)
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
	out, err := UpdateServiceIdentityQ(ctx, tx, name, ch)
	if err != nil {
		return ServiceIdentityChanged{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return ServiceIdentityChanged{}, fmt.Errorf("store: commit service identity update: %w", err)
	}
	return out, nil
}

// UpdateServiceIdentityQ is UpdateServiceIdentity's statements on tx (a pgx.Tx), which a held governance
// change applies inside the decision transaction.
func UpdateServiceIdentityQ(ctx context.Context, tx Querier, name string, ch ServiceIdentityChange) (ServiceIdentityChanged, error) {
	before, err := lockServiceIdentity(ctx, tx, name)
	if err != nil {
		return ServiceIdentityChanged{}, err
	}
	out := ServiceIdentityChanged{Before: before}
	// Every refusal comes before the first write, so a refused change has written nothing in tx.
	moving := ch.Sponsor != nil && *ch.Sponsor != before.Sponsor
	if moving {
		if ok, err := sponsorBound(ctx, tx, *ch.Sponsor); err != nil {
			return ServiceIdentityChanged{}, err
		} else if !ok {
			return ServiceIdentityChanged{}, ErrServiceSponsorInvalid
		}
	}
	if ch.Description != nil {
		if _, err = tx.Exec(ctx, `UPDATE service_identities SET description = $2 WHERE name = $1`, name, *ch.Description); err != nil {
			return ServiceIdentityChanged{}, fmt.Errorf("store: update service identity: %w", err)
		}
	}
	if moving {
		// The cutoff is set in the same transaction as the revoke, so a mint whose row commits after the
		// revoke's snapshot still carries a created_at at or before it and never authenticates.
		if _, err = tx.Exec(ctx, `UPDATE service_identities SET sponsor = $2, sponsor_since = clock_timestamp() WHERE name = $1`, name, *ch.Sponsor); err != nil {
			return ServiceIdentityChanged{}, fmt.Errorf("store: update service identity: %w", err)
		}
		if out.TokensRevoked, err = revokeServiceTokens(ctx, tx, name); err != nil {
			return ServiceIdentityChanged{}, err
		}
	}
	switch {
	case ch.Suspended != nil && *ch.Suspended && before.SuspendedAt == nil:
		_, err = tx.Exec(ctx, `UPDATE service_identities SET suspended_at = clock_timestamp(), suspended_by = $2 WHERE name = $1`, name, ch.By)
	case ch.Suspended != nil && !*ch.Suspended && before.SuspendedAt != nil:
		_, err = tx.Exec(ctx, `UPDATE service_identities SET suspended_at = NULL, suspended_by = NULL WHERE name = $1`, name)
	}
	if err != nil {
		return ServiceIdentityChanged{}, fmt.Errorf("store: update service identity: %w", err)
	}
	if out.After, err = scanServiceIdentity(tx.QueryRow(ctx, `SELECT `+serviceIdentityCols+` FROM service_identities WHERE name = $1`, name)); err != nil {
		return ServiceIdentityChanged{}, err
	}
	return out, nil
}

func (s PG) RetireServiceIdentity(ctx context.Context, name, by string) (ServiceIdentityChanged, error) {
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
		UPDATE service_identities SET retired_at = clock_timestamp(), retired_by = $2 WHERE name = $1 RETURNING `+serviceIdentityCols, name, by))
	if err != nil {
		return ServiceIdentityChanged{}, err
	}
	revoked, err := revokeServiceTokens(ctx, tx, name)
	if err != nil {
		return ServiceIdentityChanged{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return ServiceIdentityChanged{}, fmt.Errorf("store: commit service identity retire: %w", err)
	}
	return ServiceIdentityChanged{Before: before, After: after, TokensRevoked: revoked}, nil
}

func (s PG) CreateServiceToken(ctx context.Context, name, sponsor string, maxLive int, t types.APIToken, raw string) (types.APIToken, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return types.APIToken{}, fmt.Errorf("store: create service token: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	// FOR UPDATE: two mints, and a sponsor change or retire, queue on the row. A change waits for the
	// insert and then revokes the token; a mint after a change re-reads the new sponsor and refuses; and
	// the live count below is exact for the mint that holds the lock.
	si, err := scanServiceIdentity(tx.QueryRow(ctx, `SELECT `+serviceIdentityCols+` FROM service_identities WHERE name = $1 FOR UPDATE`, name))
	if err != nil {
		return types.APIToken{}, err
	}
	if !si.Usable() || si.Sponsor != sponsor {
		return types.APIToken{}, ErrServiceIdentityInactive
	}
	var live int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM api_tokens WHERE principal = $1 AND `+apiTokenLive, types.ServicePrincipalPrefix+name).Scan(&live); err != nil {
		return types.APIToken{}, fmt.Errorf("store: count service tokens: %w", err)
	}
	if live >= maxLive {
		return types.APIToken{}, ErrServiceTokenCap
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

func (s PG) ServiceStillSponsored(ctx context.Context, name, sponsor string, openedAt time.Time) (bool, error) {
	anchor, err := db.CaptureAppClock(ctx, s.Pool, s.now)
	if err != nil {
		return false, err
	}
	si, err := s.GetServiceIdentity(ctx, name)
	if err != nil {
		return false, err
	}
	return si.Usable() && si.Sponsor == sponsor && !si.SponsorSince.After(anchor.Translate(openedAt)), nil
}

// EraseServiceIdentityAttributionOf clears person as the creator, suspender and retirer of every service
// identity and as the linker of every token move, and returns how many service identity rows that changed
// and, by name, the identities that are not retired and that person still sponsors. A sponsor is never cleared: tokens and runs are checked against it, so such an
// identity is handed to another sponsor or retired first. A retired identity keeps its last sponsor.
func (s PG) EraseServiceIdentityAttributionOf(ctx context.Context, person string) (cleared int, sponsored []string, err error) {
	if person == "" {
		return 0, nil, errors.New("store: erase service identity attribution: a person is required")
	}
	err = s.inTx(ctx, func(q Querier) error {
		tag, err := q.Exec(ctx, `UPDATE service_identities
			SET created_by = CASE WHEN created_by = $1 THEN '' ELSE created_by END,
			    suspended_by = CASE WHEN suspended_by = $1 THEN '' ELSE suspended_by END,
			    retired_by = CASE WHEN retired_by = $1 THEN '' ELSE retired_by END
			WHERE created_by = $1 OR suspended_by = $1 OR retired_by = $1`, person)
		if err != nil {
			return err
		}
		cleared = int(tag.RowsAffected())
		// A token move names the person who linked it; clear that too. It is not counted in cleared.
		if _, err = q.Exec(ctx, `UPDATE api_token_moves SET linked_by = '' WHERE linked_by = $1`, person); err != nil {
			return err
		}
		sponsored, err = collect(ctx, q, "list", "sponsored service identities",
			`SELECT name FROM service_identities WHERE sponsor = $1 AND retired_at IS NULL ORDER BY name`, []any{person},
			func(row pgx.Row) (string, error) {
				var name string
				err := row.Scan(&name)
				return name, err
			})
		return err
	})
	if err != nil {
		return 0, nil, fmt.Errorf("store: erase service identity attribution: %w", err)
	}
	return cleared, sponsored, nil
}
