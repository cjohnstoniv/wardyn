// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The registry of issuer bindings (portal_issuers) and the record of console issuers (console_issuers).
//
// Every registry decision (a direct registration, the approval of a held one, the retirement of an unused
// binding, the boot-time record of the console issuer) takes LockPortalIssuerRegistry first, before any
// conflict read, so two decisions that each read "no conflict" cannot both commit. The binding insert also has
// the issuer UNIQUE constraint as a second stop.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// LockPortalIssuerRegistry takes the deployment-wide registry lock for the rest of q's transaction. q must be
// a pgx.Tx: on a pool the lock would be released as soon as its own statement ended.
func LockPortalIssuerRegistry(ctx context.Context, q Querier) error {
	if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, db.PortalIssuerRegistryLockKey); err != nil {
		return fmt.Errorf("store: lock the portal issuer registry: %w", err)
	}
	return nil
}

const portalIssuerCols = `issuer_key, issuer, internal_issuer_norm, person_id_claim, created_at, created_by`

func scanPortalIssuer(row pgx.Row) (types.PortalIssuer, error) {
	var b types.PortalIssuer
	err := row.Scan(&b.IssuerKey, &b.Issuer, &b.InternalIssuerNorm, &b.PersonIDClaim, &b.CreatedAt, &b.CreatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.PortalIssuer{}, ErrNotFound
	}
	if err != nil {
		return types.PortalIssuer{}, fmt.Errorf("store: scan portal issuer: %w", err)
	}
	return b, nil
}

// BindPortalIssuerQ makes sure the issuer has a binding that agrees with a registration, and returns it. The
// caller holds the registry lock. issuer is the exact registered string, internalNorm is NormIssuerURL of the
// portal's internal address and personIDClaim is uid or sub.
//
//   - A bound issuer with another internal address is ErrIssuerAddressConflict, and one with another
//     person-id claim ErrIssuerProfileConflict: a second portal or a credential rotation must agree with the
//     binding.
//   - A new issuer that differs from a bound one only under NormIssuerURL (another spelling) is
//     ErrIssuerSpellingConflict.
//   - Otherwise the binding is created, with its key from the exact bytes of the issuer.
func (s PG) BindPortalIssuerQ(ctx context.Context, q Querier, issuer, internalNorm, personIDClaim, by string) (types.PortalIssuer, error) {
	existing, err := scanPortalIssuer(q.QueryRow(ctx, `SELECT `+portalIssuerCols+` FROM portal_issuers WHERE issuer = $1`, issuer))
	switch {
	case err == nil:
		if existing.InternalIssuerNorm != internalNorm {
			return types.PortalIssuer{}, ErrIssuerAddressConflict
		}
		if existing.PersonIDClaim != personIDClaim {
			return types.PortalIssuer{}, ErrIssuerProfileConflict
		}
		return existing, nil
	case !errors.Is(err, ErrNotFound):
		return types.PortalIssuer{}, err
	}
	bound, err := collect(ctx, q, "list", "portal issuers", `SELECT issuer FROM portal_issuers`, nil, func(r pgx.Row) (string, error) {
		var v string
		return v, r.Scan(&v)
	})
	if err != nil {
		return types.PortalIssuer{}, err
	}
	for _, b := range bound {
		if types.NormIssuerURL(b) == types.NormIssuerURL(issuer) {
			return types.PortalIssuer{}, ErrIssuerSpellingConflict
		}
	}
	out, err := scanPortalIssuer(q.QueryRow(ctx, `
		INSERT INTO portal_issuers (issuer_key, issuer, internal_issuer_norm, person_id_claim, created_by)
		VALUES ($1, $2, $3, $4, $5) RETURNING `+portalIssuerCols, types.PortalIssuerKey(issuer), issuer, internalNorm, personIDClaim, by))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return types.PortalIssuer{}, ErrIssuerSpellingConflict
	}
	return out, err
}

// PortalIssuersQ is every issuer binding, oldest first.
func PortalIssuersQ(ctx context.Context, q Querier) ([]types.PortalIssuer, error) {
	return collect(ctx, q, "list", "portal issuers", `SELECT `+portalIssuerCols+` FROM portal_issuers ORDER BY created_at, issuer_key`, nil, scanPortalIssuer)
}

// RetirePortalIssuerQ deletes the binding for issuerKey when nothing uses it. The caller holds the registry
// lock. It takes the binding row FOR UPDATE, so it waits for every exchange write and allow-row write that holds
// it FOR KEY SHARE and every later one waits for it. When want is not nil, the locked row must still be the
// record the caller decided on: created_at, issuer, internal_issuer_norm and person_id_claim must equal want's,
// or it returns ErrStaleCompare before any use check, so a binding retired and registered again under another
// address since the decision is left alone. It then re-checks under that lock and refuses with ErrIssuerInUse
// unless: no live portal names the key, no identity row has the issuer delegate:person:<k>, no grant has a
// principal under it, and the namespace census finds nothing for it. So only a binding that never admitted
// anyone can go. An unknown key is ErrNotFound. A nil want skips the record comparison.
func (s PG) RetirePortalIssuerQ(ctx context.Context, q Querier, issuerKey string, want *types.PortalIssuer) error {
	cur, err := scanPortalIssuer(q.QueryRow(ctx, `SELECT `+portalIssuerCols+` FROM portal_issuers WHERE issuer_key = $1 FOR UPDATE`, issuerKey))
	if errors.Is(err, ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: lock issuer binding: %w", err)
	}
	if want != nil && (!cur.CreatedAt.Equal(want.CreatedAt) || cur.Issuer != want.Issuer ||
		cur.InternalIssuerNorm != want.InternalIssuerNorm || cur.PersonIDClaim != want.PersonIDClaim) {
		return ErrStaleCompare
	}
	var inUse bool
	err = q.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM delegates WHERE issuer_key = $1 AND revoked_at IS NULL)
		    OR EXISTS (SELECT 1 FROM principal_identities WHERE issuer = $2)
		    OR EXISTS (SELECT 1 FROM portal_grants WHERE starts_with(principal, $3))`,
		issuerKey, types.PortalPersonPrefix+issuerKey, types.PortalPersonPrefix+issuerKey+":").Scan(&inUse)
	if err != nil {
		return fmt.Errorf("store: check issuer binding use: %w", err)
	}
	if inUse {
		return ErrIssuerInUse
	}
	hits, err := NamespaceCensusQ(ctx, q, issuerKey)
	if err != nil {
		return err
	}
	if len(hits) > 0 {
		return ErrIssuerInUse
	}
	if _, err = q.Exec(ctx, `DELETE FROM portal_issuers WHERE issuer_key = $1`, issuerKey); err != nil {
		return fmt.Errorf("store: delete issuer binding: %w", err)
	}
	return nil
}

// IssuerKeyBoundQ reports whether an issuer binding exists for issuerKey, reading it with FOR KEY SHARE in
// the caller's transaction. An allow-row writer calls it before its insert, so a retirement either commits
// first, and the writer finds no binding, or waits for the writer, and its census finds the row.
func IssuerKeyBoundQ(ctx context.Context, q Querier, issuerKey string) (bool, error) {
	var one int
	err := q.QueryRow(ctx, `SELECT 1 FROM portal_issuers WHERE issuer_key = $1 FOR KEY SHARE`, issuerKey).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: read issuer binding: %w", err)
	}
	return true, nil
}

// RecordConsoleIssuerQ records a console issuer a replica has booted with, under the registry lock the
// caller holds. The row is keyed by NormIssuerURL of the issuer, so a spelling of one already recorded adds
// nothing. Rows are only added, never changed or removed.
func (s PG) RecordConsoleIssuerQ(ctx context.Context, q Querier, issuer, replica string) error {
	if _, err := q.Exec(ctx, `
		INSERT INTO console_issuers (issuer_norm, issuer, first_booted_by_replica) VALUES ($1, $2, $3)
		ON CONFLICT (issuer_norm) DO NOTHING`, types.NormIssuerURL(issuer), issuer, replica); err != nil {
		return fmt.Errorf("store: record console issuer: %w", err)
	}
	return nil
}

// ConsoleIssuersQ is every console issuer a replica of this deployment has booted with, oldest first.
func (s PG) ConsoleIssuersQ(ctx context.Context, q Querier) ([]types.ConsoleIssuer, error) {
	return collect(ctx, q, "list", "console issuers", `
		SELECT issuer_norm, issuer, first_booted_at, first_booted_by_replica FROM console_issuers
		 ORDER BY first_booted_at, issuer_norm`, nil, func(r pgx.Row) (types.ConsoleIssuer, error) {
		var c types.ConsoleIssuer
		return c, r.Scan(&c.IssuerNorm, &c.Issuer, &c.FirstBootedAt, &c.FirstBootedByReplica)
	})
}

// PortalIssuerRegistryStore is the registry's surface for the registration, retirement and boot paths.
type PortalIssuerRegistryStore interface {
	BindPortalIssuerQ(ctx context.Context, q Querier, issuer, internalNorm, personIDClaim, by string) (types.PortalIssuer, error)
	RetirePortalIssuerQ(ctx context.Context, q Querier, issuerKey string, want *types.PortalIssuer) error
	RecordConsoleIssuerQ(ctx context.Context, q Querier, issuer, replica string) error
	ConsoleIssuersQ(ctx context.Context, q Querier) ([]types.ConsoleIssuer, error)
	UpdateDelegateAdmissionQ(ctx context.Context, q Querier, id uuid.UUID, expected, next types.PortalAdmission) error
	CreateDelegateQ(ctx context.Context, q Querier, d types.Delegate, digest string) (types.Delegate, error)
}

var _ PortalIssuerRegistryStore = PG{}
