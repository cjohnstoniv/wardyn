// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The identity rows of portal people: one per (issuer, person id), introduced by the first exchange of any
// portal on that issuer and kept for every portal after it. The row's issuer column is
// delegate:person:<k>, which no URL can equal, so no SCIM path owns it.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// PortalIdentityStore is the issuer-portal exchange's identity surface, optional like PortalGrantStore.
type PortalIdentityStore interface {
	IssuePortalIdentity(ctx context.Context, principal, issuerKey, displayEmail, realIssuer string, portal uuid.UUID, admissionRev int64) (uuid.UUID, bool, error)
	LookupPortalPerson(ctx context.Context, issuer, personID string) (types.PortalPerson, error)
	PendingPortalIntroductions(ctx context.Context, limit int) ([]PortalIntroduction, error)
	MarkPortalIntroductionAudited(ctx context.Context, id uuid.UUID) (bool, error)
}

var _ PortalIdentityStore = PG{}

// ErrPortalIssuerNotBound is LookupPortalPerson's answer for an issuer with no binding, a mis-spelling of a
// bound one included. It wraps ErrNotFound, so a caller that only asks "no such person" still matches, and
// one that tells the two 404 reasons apart can.
var ErrPortalIssuerNotBound = fmt.Errorf("%w: no issuer portal is bound to this issuer", ErrNotFound)

// PortalIntroduction is the introduction event of one portal person that was committed and has not been
// marked delivered. Every field is read from the identity row; nothing is recomputed at delivery, so every
// delivery of one introduction is the same event.
type PortalIntroduction struct {
	IdentityID uuid.UUID
	Principal  string
	// IssuerKey is k, the part of the row's issuer after delegate:person:.
	IssuerKey string
	// PortalID is the portal whose exchange introduced the person.
	PortalID uuid.UUID
	// Issuer is the registered public issuer string the introducing portal verified against.
	Issuer  string
	EventID uuid.UUID
	EventAt time.Time
}

// IssuePortalIdentity makes sure the person principal has an identity row under the issuer key issuerKey and
// returns its id, and whether this call created it. principal and issuerKey are the exchange's; realIssuer
// is the registered public issuer; displayEmail is shown and never used for authorisation. portal is the
// exchange's portal and admissionRev the revision it verified against.
//
// One transaction. It first takes the binding row FOR KEY SHARE and then the portal's delegates row FOR SHARE,
// re-checking that the portal is live and its revision unchanged (ErrPortalGone, ErrAdmissionChanged), both held
// to commit, so a retirement, a revoke or an admission change waits for it. It then inserts the row with the
// introduction event's id and time fixed (ON CONFLICT DO NOTHING, so concurrent first exchanges leave one
// introducer), refuses a deactivated or purged person and applies the identity guard. A person already
// there is returned with created false.
func (s PG) IssuePortalIdentity(ctx context.Context, principal, issuerKey, displayEmail, realIssuer string, portal uuid.UUID, admissionRev int64) (uuid.UUID, bool, error) {
	since, err := ReadRevocationSeq(ctx, s.Pool)
	if err != nil {
		return uuid.Nil, false, err
	}
	var id uuid.UUID
	var created bool
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("store: issue portal identity: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	err = func() error {
		var q Querier = tx
		key, err := lockPortalForExchange(ctx, q, portal, admissionRev)
		if err != nil {
			return err
		}
		if key != issuerKey || !hasPortalPrefix(principal, key) {
			return fmt.Errorf("store: the principal is not one of portal %s's people", portal)
		}
		issuer := types.PortalPersonPrefix + key
		err = q.QueryRow(ctx, `
			INSERT INTO principal_identities (principal, issuer, display_email, introduced_by, introduced_issuer,
			                                  introduction_event_id, introduction_event_at)
			VALUES ($1, $2, $3, $4, $5, gen_random_uuid(), clock_timestamp())
			ON CONFLICT DO NOTHING RETURNING id`, principal, issuer, displayEmail, portal, realIssuer).Scan(&id)
		switch {
		case err == nil:
			created = true
		case errors.Is(err, pgx.ErrNoRows):
			if err = q.QueryRow(ctx, `SELECT id FROM principal_identities WHERE issuer = $1 AND principal = $2`, issuer, principal).Scan(&id); err != nil {
				return fmt.Errorf("store: read portal identity: %w", err)
			}
		default:
			return fmt.Errorf("store: insert portal identity: %w", err)
		}
		var dead bool
		if err = q.QueryRow(ctx, `SELECT deactivated_at IS NOT NULL OR purged_at IS NOT NULL FROM principal_identities WHERE id = $1`, id).Scan(&dead); err != nil {
			return fmt.Errorf("store: read portal identity: %w", err)
		}
		if dead {
			return ErrIdentityDeactivated
		}
		return checkIdentityGuard(ctx, tx, IdentityGuard{Principal: principal, Epoch: -1}, since)
	}()
	if err != nil {
		return uuid.Nil, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return uuid.Nil, false, fmt.Errorf("store: commit portal identity: %w", err)
	}
	return id, created, nil
}

// LookupPortalPerson finds a portal person from what an operator knows: the exact issuer string and the
// provider's person id. An issuer with no binding is ErrPortalIssuerNotBound; a bound issuer with no such
// person is ErrNotFound, so a never-seen person is never created by asking. The principal it computes is the
// one an exchange creates.
func (s PG) LookupPortalPerson(ctx context.Context, issuer, personID string) (types.PortalPerson, error) {
	var key string
	err := s.Pool.QueryRow(ctx, `SELECT issuer_key FROM portal_issuers WHERE issuer = $1`, issuer).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.PortalPerson{}, ErrPortalIssuerNotBound
	}
	if err != nil {
		return types.PortalPerson{}, fmt.Errorf("store: read issuer binding: %w", err)
	}
	p := types.PortalPerson{Principal: types.PortalPrincipal(key, personID)}
	err = s.Pool.QueryRow(ctx, `
		SELECT id, display_email, deactivated_at, purged_at FROM principal_identities
		 WHERE issuer = $1 AND principal = $2`, types.PortalPersonPrefix+key, p.Principal).
		Scan(&p.IdentityID, &p.DisplayEmail, &p.DeactivatedAt, &p.PurgedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.PortalPerson{}, ErrNotFound
	}
	if err != nil {
		return types.PortalPerson{}, fmt.Errorf("store: read portal person: %w", err)
	}
	p.PortalsUsed, err = collect(ctx, s.Pool, "list", "portals a person used", `
		SELECT d.id, d.name, d.run_in_under, d.revoked_at FROM portal_person_portals u
		  JOIN delegates d ON d.id = u.delegate_id
		 WHERE u.identity_id = $1
		 ORDER BY u.first_used_at, d.id`, []any{p.IdentityID}, func(r pgx.Row) (types.PortalUsed, error) {
		var u types.PortalUsed
		return u, r.Scan(&u.Delegate, &u.Name, &u.RunInUnder, &u.RevokedAt)
	})
	if err != nil {
		return types.PortalPerson{}, err
	}
	return p, nil
}

// PendingPortalIntroductions is the portal people whose introduction event was committed and never marked
// delivered, oldest first, at most limit. The sweep replays each (WithAuditReplay appends only when no row
// carries the id and time) and marks it with MarkPortalIntroductionAudited.
func (s PG) PendingPortalIntroductions(ctx context.Context, limit int) ([]PortalIntroduction, error) {
	return collect(ctx, s.Pool, "list", "pending portal introductions", `
		SELECT i.id, i.principal, substr(i.issuer, $2::int + 1), i.introduced_by, i.introduced_issuer,
		       i.introduction_event_id, i.introduction_event_at
		  FROM principal_identities i
		 WHERE starts_with(i.issuer, $3) AND i.introduction_event_id IS NOT NULL AND i.introduction_audited_at IS NULL
		 ORDER BY i.introduction_event_at, i.id LIMIT $1`, []any{limit, len(types.PortalPersonPrefix), types.PortalPersonPrefix},
		func(r pgx.Row) (PortalIntroduction, error) {
			var p PortalIntroduction
			return p, r.Scan(&p.IdentityID, &p.Principal, &p.IssuerKey, &p.PortalID, &p.Issuer, &p.EventID, &p.EventAt)
		})
}

// PendingPortalIntroductionFor is identity's own introduction event when it was committed and never marked delivered, and false when none is owed.
func (s PG) PendingPortalIntroductionFor(ctx context.Context, identity uuid.UUID) (PortalIntroduction, bool, error) {
	var p PortalIntroduction
	err := s.Pool.QueryRow(ctx, `
		SELECT i.id, i.principal, substr(i.issuer, $2::int + 1), i.introduced_by, i.introduced_issuer,
		       i.introduction_event_id, i.introduction_event_at
		  FROM principal_identities i
		 WHERE i.id = $1 AND starts_with(i.issuer, $3) AND i.introduction_event_id IS NOT NULL AND i.introduction_audited_at IS NULL`,
		identity, len(types.PortalPersonPrefix), types.PortalPersonPrefix).
		Scan(&p.IdentityID, &p.Principal, &p.IssuerKey, &p.PortalID, &p.Issuer, &p.EventID, &p.EventAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return PortalIntroduction{}, false, nil
	}
	if err != nil {
		return PortalIntroduction{}, false, fmt.Errorf("store: pending portal introduction for: %w", err)
	}
	return p, true, nil
}

// MarkPortalIntroductionAudited records that the introduction event of identity id was delivered, where it
// was not yet, and reports whether this call did. A crash after the append and before this leads to one more
// replay, which adds no row.
func (s PG) MarkPortalIntroductionAudited(ctx context.Context, id uuid.UUID) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE principal_identities SET introduction_audited_at = clock_timestamp()
		 WHERE id = $1 AND introduction_event_id IS NOT NULL AND introduction_audited_at IS NULL`, id)
	if err != nil {
		return false, fmt.Errorf("store: mark portal introduction audited: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

func hasPortalPrefix(principal, key string) bool {
	p := types.PortalPersonPrefix + key + ":"
	return len(principal) > len(p) && principal[:len(p)] == p
}
