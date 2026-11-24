// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Registered portals and the delegated tokens they are handed. Like
// store_apitokens.go and store_devices.go, every method hashes the RAW
// credential with hashToken before it reaches SQL.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// DelegateStore is optional, type-asserted like DeviceStore: without it the
// exchange and delegated lane fail closed instead of silently no-op.
//
// Authenticating reads filter revoked/expired rows in their WHERE, so
// revoked, expired, unknown, and mismatched credentials all answer
// ErrNotFound — the boundary gives no oracle signal.
type DelegateStore interface {
	CreateDelegate(ctx context.Context, d types.Delegate, raw string) (types.Delegate, error)
	GetDelegateByRaw(ctx context.Context, raw string) (types.Delegate, error)
	ListDelegates(ctx context.Context) ([]types.Delegate, error)
	RevokeDelegate(ctx context.Context, id uuid.UUID, now time.Time, cut bool) (types.Delegate, int, error)
	MintDelegatedToken(ctx context.Context, t types.DelegatedToken, raw string, now time.Time) (types.DelegatedToken, error)
	GetDelegatedTokenByRaw(ctx context.Context, raw string, now time.Time) (types.DelegatedToken, error)
	GetDelegatedTokenByID(ctx context.Context, id uuid.UUID, now time.Time) (types.DelegatedToken, error)
}

var _ DelegateStore = PG{}

// DelegateReader reads one portal by id whatever its state, so the pool queue's start can tell a
// portal that was revoked while a run waited from one that still stands. Separate from DelegateStore
// for the same reason that interface is optional: a test double that implements one need not the other.
type DelegateReader interface {
	GetDelegateByID(ctx context.Context, id uuid.UUID) (types.Delegate, error)
}

var _ DelegateReader = PG{}

const delegateCols = `id, name, idp_client_id, scope_group, credential_sha256, registered_by, created_at, last_used_at, revoked_at, cut_in_flight_at, run_in_under,
	issuer, internal_issuer, issuer_key, token_profile, groups_claim, email_domains, user_type, ui_session_renewal, audience, required_scopes,
	max_subject_token_age_seconds, grant_ttl_seconds, required_claim, portal_managed_admission, admission_rev`

func scanDelegate(row pgx.Row) (types.Delegate, error) { return scanDelegateWith(row) }

// scanDelegateWith scans delegateCols, then any columns a query selects after them into extra.
func scanDelegateWith(row pgx.Row, extra ...any) (types.Delegate, error) {
	var d types.Delegate
	var audience *string
	var maxAge, grantTTL *int
	var requiredClaim []byte
	err := row.Scan(append([]any{&d.ID, &d.Name, &d.IdPClientID, &d.Group, &d.CredentialSHA256, &d.RegisteredBy,
		&d.CreatedAt, &d.LastUsedAt, &d.RevokedAt, &d.CutInFlightAt, &d.RunInUnder,
		&d.Issuer, &d.InternalIssuer, &d.IssuerKey, &d.TokenProfile, &d.GroupsClaim, &d.EmailDomains, &d.UserType,
		&d.UISessionRenewal, &audience, &d.RequiredScopes, &maxAge, &grantTTL, &requiredClaim,
		&d.PortalManagedAdmission, &d.AdmissionRev}, extra...)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.Delegate{}, ErrNotFound
	}
	if err != nil {
		return types.Delegate{}, fmt.Errorf("store: scan delegate: %w", err)
	}
	if audience != nil {
		d.Audience = *audience
	}
	if maxAge != nil {
		d.MaxSubjectTokenAgeSeconds = *maxAge
	}
	if grantTTL != nil {
		d.GrantTTLSeconds = *grantTTL
	}
	if len(d.EmailDomains) == 0 {
		d.EmailDomains = nil
	}
	if requiredClaim != nil {
		var rc types.PortalRequiredClaim
		if err := json.Unmarshal(requiredClaim, &rc); err != nil {
			return types.Delegate{}, fmt.Errorf("store: scan delegate required claim: %w", err)
		}
		d.RequiredClaim = &rc
	}
	return d, nil
}

// CreateDelegate registers one portal. raw is the PLAINTEXT credential; only
// its hash is stored. Returns ErrConflict on a credential_sha256 collision
// (a reused credential in a 256-bit space).
func (s PG) CreateDelegate(ctx context.Context, d types.Delegate, raw string) (types.Delegate, error) {
	return s.CreateDelegateQ(ctx, s.Pool, d, hashToken(raw))
}

// CreateDelegateQ registers one portal on q with the SHA-256 digest of its credential, which the caller
// hashed (the plaintext never reaches the store here). It is the write a held registration applies inside
// the decision transaction, so a rolled-back decision leaves no portal. Returns ErrConflict on a digest
// collision. Every issuer-portal field is written as given; a console portal's are all zero, and the row
// stores them as the defaults, NULL for the optional ones.
func (s PG) CreateDelegateQ(ctx context.Context, q Querier, d types.Delegate, digest string) (types.Delegate, error) {
	var requiredClaim any
	if d.RequiredClaim != nil {
		b, err := json.Marshal(d.RequiredClaim)
		if err != nil {
			return types.Delegate{}, fmt.Errorf("store: encode required claim: %w", err)
		}
		requiredClaim = string(b)
	}
	emailDomains := d.EmailDomains
	if emailDomains == nil {
		emailDomains = []string{}
	}
	const stmt = `
		INSERT INTO delegates (id, name, idp_client_id, scope_group, credential_sha256, registered_by, run_in_under,
			issuer, internal_issuer, issuer_key, token_profile, groups_claim, email_domains, user_type, ui_session_renewal,
			audience, required_scopes, max_subject_token_age_seconds, grant_ttl_seconds, required_claim, portal_managed_admission)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,NULLIF($16,''),$17,NULLIF($18,0),NULLIF($19,0),$20::jsonb,$21)
		RETURNING ` + delegateCols
	out, err := scanDelegate(q.QueryRow(ctx, stmt, d.ID, d.Name, d.IdPClientID, d.Group, digest, d.RegisteredBy, d.RunInUnder,
		d.Issuer, d.InternalIssuer, d.IssuerKey, d.TokenProfile, d.GroupsClaim, emailDomains, d.UserType, d.UISessionRenewal,
		d.Audience, d.RequiredScopes, d.MaxSubjectTokenAgeSeconds, d.GrantTTLSeconds, requiredClaim, d.PortalManagedAdmission))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return types.Delegate{}, ErrConflict
	}
	return out, err
}

// GetDelegateByRaw resolves the LIVE console portal a credential stands for. A row with an issuer is never
// found here: its credential has its own prefix and its own lookup (GetIssuerDelegateByRaw).
func (s PG) GetDelegateByRaw(ctx context.Context, raw string) (types.Delegate, error) {
	const q = `SELECT ` + delegateCols + ` FROM delegates WHERE credential_sha256 = $1 AND issuer = '' AND revoked_at IS NULL`
	return scanDelegate(s.Pool.QueryRow(ctx, q, hashToken(raw)))
}

// GetIssuerDelegateByRaw resolves the LIVE issuer portal a wdi_ credential stands for: only a row with an
// issuer, and only for a secret that carries the wdi_ prefix. A console portal's credential never resolves
// here, so each prefix is bound to its kind in both directions.
func (s PG) GetIssuerDelegateByRaw(ctx context.Context, raw string) (types.Delegate, error) {
	if !strings.HasPrefix(raw, PortalCredentialPrefix) {
		return types.Delegate{}, ErrNotFound
	}
	const q = `SELECT ` + delegateCols + ` FROM delegates WHERE credential_sha256 = $1 AND issuer <> '' AND revoked_at IS NULL`
	return scanDelegate(s.Pool.QueryRow(ctx, q, hashToken(raw)))
}

// PortalCredentialPrefix starts the credential of an issuer portal.
const PortalCredentialPrefix = "wdi_"

// UpdateDelegateAdmissionQ replaces an issuer portal's admission rule with next when its current one is
// expected, and increments admission_rev. Zero rows (another change landed first, the row is not an
// issuer portal, it is revoked, or it is unknown) is ErrStaleCompare, so two edits cannot cross. The write updates the
// portal's delegates row, which the exchange's identity and grant writes hold FOR SHARE, so it waits for
// an exchange in flight and the next exchange meets the new revision.
func (s PG) UpdateDelegateAdmissionQ(ctx context.Context, q Querier, id uuid.UUID, expected, next types.PortalAdmission) error {
	claim := func(c *types.PortalRequiredClaim) (any, error) {
		if c == nil {
			return nil, nil
		}
		b, err := json.Marshal(c)
		return string(b), err
	}
	was, err := claim(expected.RequiredClaim)
	if err != nil {
		return fmt.Errorf("store: encode required claim: %w", err)
	}
	now, err := claim(next.RequiredClaim)
	if err != nil {
		return fmt.Errorf("store: encode required claim: %w", err)
	}
	tag, err := q.Exec(ctx, `
		UPDATE delegates SET scope_group = $2, portal_managed_admission = $3, required_claim = $4::jsonb, admission_rev = admission_rev + 1
		 WHERE id = $1 AND issuer <> '' AND revoked_at IS NULL
		   AND scope_group = $5 AND portal_managed_admission = $6 AND required_claim IS NOT DISTINCT FROM $7::jsonb`,
		id, next.Group, next.PortalManaged, now, expected.Group, expected.PortalManaged, was)
	if err != nil {
		return fmt.Errorf("store: update portal admission: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrStaleCompare
	}
	return nil
}

// GetDelegateByID returns the portal with id, revoked or not (ErrNotFound only for an id that was
// never registered). The authenticating reads above filter revoked rows; this one exists to see them.
func (s PG) GetDelegateByID(ctx context.Context, id uuid.UUID) (types.Delegate, error) {
	return scanDelegate(s.Pool.QueryRow(ctx, `SELECT `+delegateCols+` FROM delegates WHERE id = $1`, id))
}

// ListDelegates returns every portal, newest first, including revoked —
// admins need to see that a retired portal is retired.
func (s PG) ListDelegates(ctx context.Context) ([]types.Delegate, error) {
	const q = `SELECT ` + delegateCols + ` FROM delegates ORDER BY created_at DESC, id`
	return collect(ctx, s.Pool, "list", "delegates", q, nil, scanDelegate)
}

// RevokeDelegate marks id revoked and, with cut, stamps cut_in_flight_at, after which no run the
// portal launched gets a gateway token (api.gatewayMintVia reads it at every mint). A cut of a
// portal already revoked but not yet cut stamps the cut alone. Unknown, already revoked without a
// cut, or already cut is ErrNotFound, so a repeat writes no second audit row. One statement, so a
// revoke and a cut racing each land once. inFlight counts the portal's non-terminal runs, for the
// audit row. Outstanding delegated tokens need no write of their own: GetDelegatedTokenByRaw joins
// on revoked_at.
func (s PG) RevokeDelegate(ctx context.Context, id uuid.UUID, now time.Time, cut bool) (_ types.Delegate, inFlight int, _ error) {
	states := make([]string, 0, len(types.NonTerminalRunStates))
	for _, st := range types.NonTerminalRunStates {
		states = append(states, string(st))
	}
	const q = `
		WITH d AS (
			UPDATE delegates SET revoked_at = COALESCE(revoked_at, $2),
				cut_in_flight_at = CASE WHEN $3::boolean THEN COALESCE(cut_in_flight_at, $2) ELSE cut_in_flight_at END
			WHERE id = $1 AND (revoked_at IS NULL OR ($3::boolean AND cut_in_flight_at IS NULL))
			RETURNING ` + delegateCols + `
		)
		SELECT ` + delegateCols + `,
			(SELECT count(*) FROM agent_runs r WHERE r.created_via = d.id AND r.state = ANY($4))
		FROM d`
	d, err := scanDelegateWith(s.Pool.QueryRow(ctx, q, id, now, cut, states), &inFlight)
	return d, inFlight, err
}

const delegatedTokenCols = `t.id, t.delegate_id, t.principal, t.email, t.user_type, t.groups, t.groups_truncated, t.created_at, t.expires_at, t.rev_seq, t.actor_sub, t.actor_iss`

func scanDelegatedToken(row pgx.Row) (types.DelegatedToken, error) {
	var t types.DelegatedToken
	var groups []byte
	var actorSub, actorIss string
	err := row.Scan(&t.ID, &t.DelegateID, &t.Principal, &t.Email, &t.UserType, &groups, &t.GroupsTruncated,
		&t.CreatedAt, &t.ExpiresAt, &t.RevSeq, &actorSub, &actorIss)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.DelegatedToken{}, ErrNotFound
	}
	if err != nil {
		return types.DelegatedToken{}, fmt.Errorf("store: scan delegated token: %w", err)
	}
	if groups != nil {
		if err := json.Unmarshal(groups, &t.Groups); err != nil {
			return types.DelegatedToken{}, fmt.Errorf("store: scan delegated token groups: %w", err)
		}
	}
	// No actor is stored as two empty strings, so a token minted without one reads back as before.
	if actorSub != "" {
		t.Actor = &types.DelegationActor{Sub: actorSub, Iss: actorIss}
	}
	return t, nil
}

// MintDelegatedToken stores one token (raw is plaintext; only its hash
// persists), stamps the portal's last use, and sweeps expired tokens.
// created_at is written on the DB clock, back-dated by t.CreatedAt's age
// like CreateAPIToken, since it's compared against a session cutoff that
// Postgres stamps.
//
// ponytail: the sweep rides the mint, like MintAttachTicket — rows live only
// ten minutes. Add a real sweeper if mint volume ever breaks that.
func (s PG) MintDelegatedToken(ctx context.Context, t types.DelegatedToken, raw string, now time.Time) (types.DelegatedToken, error) {
	groups, err := marshalGroups(t.Groups)
	if err != nil {
		return types.DelegatedToken{}, err
	}
	anchor, err := db.CaptureAppClock(ctx, s.Pool, s.now)
	if err != nil {
		return types.DelegatedToken{}, err
	}
	createdAt := anchor.Translate(t.CreatedAt)
	const q = `
		WITH swept AS (DELETE FROM delegated_tokens WHERE expires_at <= $11),
		     touched AS (UPDATE delegates SET last_used_at = $11 WHERE id = $2)
		INSERT INTO delegated_tokens AS t (id, delegate_id, token_sha256, principal, email, user_type, groups, groups_truncated, created_at, expires_at, rev_seq, actor_sub, actor_iss)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$12,$13,$14)
		RETURNING ` + delegatedTokenCols
	var actorSub, actorIss string
	if t.Actor != nil {
		actorSub, actorIss = t.Actor.Sub, t.Actor.Iss
	}
	return scanDelegatedToken(s.Pool.QueryRow(ctx, q, t.ID, t.DelegateID, hashToken(raw), t.Principal, t.Email,
		t.UserType, groups, t.GroupsTruncated, createdAt, t.ExpiresAt, now, t.RevSeq, actorSub, actorIss))
}

// GetDelegatedTokenByRaw is the delegated lane's auth lookup: the token must
// be unexpired at now AND its portal still registered — the join is what
// makes revoking a portal kill all its tokens on their next request. The
// portal must be a console portal: a delegated_tokens row never rides an
// issuer portal's row.
func (s PG) GetDelegatedTokenByRaw(ctx context.Context, raw string, now time.Time) (types.DelegatedToken, error) {
	const q = `
		SELECT ` + delegatedTokenCols + `
		FROM delegated_tokens t JOIN delegates d ON d.id = t.delegate_id
		WHERE t.token_sha256 = $1 AND t.expires_at > $2 AND d.revoked_at IS NULL AND d.issuer = ''`
	return scanDelegatedToken(s.Pool.QueryRow(ctx, q, hashToken(raw), now))
}

// GetDelegatedTokenByID is GetDelegatedTokenByRaw by the grant's id: live while
// unexpired at now AND its portal still registered. A UI session opened through
// a portal re-asks it, so the session ends with the grant (#1475). Expired rows
// are swept on any mint, so ErrNotFound means ended.
func (s PG) GetDelegatedTokenByID(ctx context.Context, id uuid.UUID, now time.Time) (types.DelegatedToken, error) {
	const q = `
		SELECT ` + delegatedTokenCols + `
		FROM delegated_tokens t JOIN delegates d ON d.id = t.delegate_id
		WHERE t.id = $1 AND t.expires_at > $2 AND d.revoked_at IS NULL AND d.issuer = ''`
	return scanDelegatedToken(s.Pool.QueryRow(ctx, q, id, now))
}
