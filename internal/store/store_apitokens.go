// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Per-user API tokens. Kept out of store.go on purpose (lint size boundary),
// mirroring store_sshkeys.go's split.
//
// SECURITY: the raw token never reaches SQL — every method here hashes it
// with hashToken before touching the table, so token_sha256 is the only form
// that exists at rest.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// CreateAPIToken inserts one token row. raw is the PLAINTEXT credential; only
// its hash is stored, and the caller must return the plaintext to its
// creator exactly once (unrecoverable afterwards). t.Token is ignored —
// passing the secret twice would be the one way to accidentally persist it.
//
// A unique_violation (23505) on token_sha256 is ErrConflict: a raw collision
// in a 256-bit space means the caller reused a token value rather than
// minting a fresh one.
//
// SECURITY: groups_truncated binds as SQL NULL when nil. NULL means "minted
// before anything recorded this", and consumers read that as TRUNCATED, not
// false. A plain bool or DEFAULT FALSE would assert "complete" for every
// legacy row — fail OPEN, the exact thing the marker exists to prevent.
func (s PG) CreateAPIToken(ctx context.Context, t types.APIToken, raw string) (types.APIToken, error) {
	groups, err := marshalGroups(t.Groups)
	if err != nil {
		return types.APIToken{}, err
	}
	// created_at is written on the database's clock, back-dated by the request's
	// own age, rather than binding t.CreatedAt straight through: the row's
	// timestamp must not come from wardynd's clock while what it's compared
	// against (oidc_session_revocations.revoked_at) comes from Postgres.
	// SECURITY: with wardynd ahead of the database, a token minted BEFORE a
	// revoke could carry a created_at AFTER the cutoff and survive it —
	// "revoke every session for this human" would silently not.
	//
	// The age, not now(): the API stamps t.CreatedAt at request ADMISSION,
	// before reading the body, so a caller holding a mint open across
	// POST /sessions/revoke can't land a created_at after the cutoff; now()-age
	// keeps that while measuring skew-free on the app's own clock.
	//
	// A ZERO CreatedAt means no admission time to preserve, so it becomes the
	// database's now(). Fail closed is NOT the answer here — an age of two
	// millennia would mint a token already revoked by any cutoff on record.
	age := int64(0)
	if !t.CreatedAt.IsZero() {
		age = db.AppClockAgeMicros(t.CreatedAt, time.Now())
	}
	// expires_at rides the same clock: the lifetime (ExpiresAt - CreatedAt, two
	// readings of the app clock) is added to the created_at the database stamps,
	// so wardynd's skew against Postgres never shortens or lengthens a token.
	// NULL (no ExpiresAt) is a token that never expires.
	var lifetime *int64
	if t.ExpiresAt != nil {
		from := t.CreatedAt
		if from.IsZero() {
			from = time.Now()
		}
		us := t.ExpiresAt.Sub(from).Microseconds()
		lifetime = &us
	}
	// q is built, not const: the created_at expression (db.AppClockAgeSQL) is
	// shared with the session-revocation read and a const can't call it.
	q := `
		INSERT INTO api_tokens (id, principal, email, role, user_type, groups, groups_truncated, name, token_sha256, created_at, expires_at, minted_by, identity_stamped_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,` + db.AppClockAgeSQL("$10") + `,
		        CASE WHEN $11::bigint IS NULL THEN NULL ELSE ` + db.AppClockAgeSQL("$10") + ` + $11::bigint * interval '1 microsecond' END,
		        NULLIF($12, ''), now())
		RETURNING ` + apiTokenCols
	var out types.APIToken
	err = s.guarded(ctx, func(qr queryRower) (e error) {
		out, e = scanAPIToken(qr.QueryRow(ctx, q,
			t.ID, t.Principal, t.Email, t.Role, t.UserType, groups, t.GroupsTruncated, t.Name, hashToken(raw), age, lifetime, t.MintedBy))
		return e
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return types.APIToken{}, ErrConflict
		}
		return types.APIToken{}, err
	}
	return out, nil
}

// GetAPITokenByRaw is the auth-time lookup: given a bearer string, resolve
// the LIVE token it stands for. Deliberately UNSCOPED by principal — this
// call is what authenticates the caller.
//
// SECURITY: `revoked_at IS NULL` and the not-expired test (apiTokenLive) are
// in the WHERE, not checked separately — a revoked, expired, unknown, or
// non-matching token all fail IDENTICALLY with ErrNotFound, so the boundary is
// never an oracle for "this token once existed".
func (s PG) GetAPITokenByRaw(ctx context.Context, raw string) (types.APIToken, error) {
	const q = `
		SELECT ` + apiTokenCols + `
		FROM api_tokens WHERE token_sha256 = $1 AND ` + apiTokenLive
	return scanAPIToken(s.Pool.QueryRow(ctx, q, hashToken(raw)))
}

// TouchAPIToken records that id was just used. BEST EFFORT: the auth branch
// ignores the error, since failing to record a touch must never fail an
// otherwise-valid request.
//
// ponytail: one UPDATE per authenticated request; add a throttle only if a
// hot token ever makes that write volume a problem.
func (s PG) TouchAPIToken(ctx context.Context, id uuid.UUID, now time.Time) error {
	_, err := s.Pool.Exec(ctx, `UPDATE api_tokens SET last_used_at = $2 WHERE id = $1`, id, now)
	if err != nil {
		return fmt.Errorf("store: touch api token: %w", err)
	}
	return nil
}

// ListAPITokensByPrincipal returns principal's own tokens, newest first
// (self-service GET /me/tokens). Revoked rows are INCLUDED so a human can
// confirm a retired token is retired; the row carries no usable credential
// either way.
func (s PG) ListAPITokensByPrincipal(ctx context.Context, principal string) ([]types.APIToken, error) {
	const q = `
		SELECT ` + apiTokenCols + `
		FROM api_tokens WHERE principal = $1 ORDER BY created_at DESC, id`
	return queryAPITokens(ctx, s, q, principal)
}

// ListAPITokens returns every token in the deployment, newest first (admin
// GET /tokens); revoked rows included, same reason as the self-service list.
func (s PG) ListAPITokens(ctx context.Context) ([]types.APIToken, error) {
	const q = `
		SELECT ` + apiTokenCols + `
		FROM api_tokens ORDER BY created_at DESC`
	return queryAPITokens(ctx, s, q)
}

// RefreshAPITokenIdentity re-stamps role, user type, and the group snapshot
// (with its completeness bit) on EVERY token principal holds — the api-token
// twin of RefreshSSHKeyRoles; OnLogin fires both so one login bounds both
// frozen credentials.
//
// SECURITY: a token's identity is frozen at mint with nothing else to refresh
// it — without this, demoting an admin leaves their wdn_ tokens authenticating
// AS AN ADMIN until explicitly revoked, and a token keeps authorizing against
// stale group memberships until a fresh mint.
//
// truncated is bound EXACTLY as passed, never defaulted or inferred: it must
// come from the login's own session-completeness signal, the same bit a fresh
// mint stamps. A NULL groups_truncated already reads as TRUNCATED downstream
// (fail closed); silently defaulting this to false would assert "these are
// all their groups" for a snapshot that isn't — the wrong direction for an
// authorization decision.
//
// identity_stamped_at moves with the role in the SAME statement, so a login
// that stamps a token's role stamps its age too, and a failed statement leaves
// that principal's tokens stale rather than half-updated. WARDYN_ROLE_STAMP_TTL
// reads it; `revoked_at IS NULL` stays in the WHERE, so a login never revives a
// revoked token (an expired one keeps its own expiry, which this never writes).
//
// Still bounded-stale, not live: the ceiling is the owner's next login (see
// docs/SSH.md §Bounds for the key lane's equivalent). No error when the
// principal holds no tokens — a zero-row UPDATE is the ordinary case.
//
// SECURITY: a token an admin minted FOR this principal (minted_by set) is
// revoked instead of re-stamped when the login's role differs from its
// stamp — its role was derived before the person's own groups were known and
// the minter saw its plaintext, so re-stamping upward would hand the minter's
// tier to whoever holds that credential. SET reads the row's OLD role, so the
// comparison is against the stamp.
func (s PG) RefreshAPITokenIdentity(ctx context.Context, principal, role, userType string, groups []string, truncated bool) error {
	g, err := marshalGroups(groups)
	if err != nil {
		return err
	}
	_, err = s.Pool.Exec(ctx,
		`UPDATE api_tokens SET role = $1, user_type = $2, groups = $3, groups_truncated = $4,
		   identity_stamped_at = now(),
		   revoked_at = CASE WHEN minted_by IS NOT NULL AND role <> $1 THEN now() END
		 WHERE principal = $5 AND revoked_at IS NULL`,
		role, userType, g, truncated, principal)
	if err != nil {
		return fmt.Errorf("store: refresh api token identity: %w", err)
	}
	return nil
}

// RevokeAPIToken marks id revoked. principal scopes the UPDATE when non-empty
// (self-service: a human may only revoke their OWN token). SECURITY:
// someone else's id is ErrNotFound, not a distinguishable 403 — no existence
// leak across principals. An EMPTY principal is the ADMIN path and revokes
// anyone's.
//
// Already-revoked is ErrNotFound too (`revoked_at IS NULL` in the WHERE):
// revoke is idempotent, so a second call must not emit a second token.revoke
// audit row for an act that didn't happen.
func (s PG) RevokeAPIToken(ctx context.Context, id uuid.UUID, principal string, now time.Time) (types.APIToken, error) {
	const q = `
		UPDATE api_tokens SET revoked_at = $2
		WHERE id = $1 AND revoked_at IS NULL AND ($3 = '' OR principal = $3)
		RETURNING ` + apiTokenCols
	return scanAPIToken(s.Pool.QueryRow(ctx, q, id, now, principal))
}

// queryAPITokens is the two token lists' shared read, via collect's rows
// loop — including the "empty, never nil" contract (the API renders these
// as `[]`, never `null`).
func queryAPITokens(ctx context.Context, s PG, q string, args ...any) ([]types.APIToken, error) {
	return collect(ctx, s.Pool, "list", "api tokens", q, args, scanAPIToken)
}

// marshalGroups encodes the group snapshot for the nullable JSONB column.
// nil marshals to SQL NULL, not 'null' or '[]': nil and empty mean different
// things to the capability resolver, and the round trip must preserve which
// was stamped.
func marshalGroups(groups []string) (any, error) {
	if groups == nil {
		return nil, nil
	}
	b, err := json.Marshal(groups)
	if err != nil {
		return nil, fmt.Errorf("store: marshal api token groups: %w", err)
	}
	return b, nil
}

// apiTokenLive is the predicate for a token that can still authenticate: not
// revoked, and either without an expiry or not yet at it, on the database's clock.
const apiTokenLive = `revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now())`

// apiTokenCols is THE api_tokens READ column list, in scanAPIToken's order.
// The INSERT list stays spelled out separately: it names token_sha256 (never
// read back) and omits last_used_at/revoked_at (never inserted) — deriving
// one list from the other would hide that difference.
const apiTokenCols = `id, principal, email, role, user_type, groups, groups_truncated, name, created_at, last_used_at, revoked_at, expires_at, COALESCE(minted_by, ''), identity_stamped_at`

func scanAPIToken(row pgx.Row) (types.APIToken, error) {
	var t types.APIToken
	var groups []byte
	err := row.Scan(&t.ID, &t.Principal, &t.Email, &t.Role, &t.UserType, &groups, &t.GroupsTruncated, &t.Name,
		&t.CreatedAt, &t.LastUsedAt, &t.RevokedAt, &t.ExpiresAt, &t.MintedBy, &t.IdentityStampedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.APIToken{}, ErrNotFound
	}
	if err != nil {
		return types.APIToken{}, fmt.Errorf("store: scan api token: %w", err)
	}
	if groups != nil {
		if err := json.Unmarshal(groups, &t.Groups); err != nil {
			return types.APIToken{}, fmt.Errorf("store: scan api token groups: %w", err)
		}
	}
	return t, nil
}
