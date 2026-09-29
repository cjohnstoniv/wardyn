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
	RevokeDelegate(ctx context.Context, id uuid.UUID, now time.Time) (types.Delegate, error)
	MintDelegatedToken(ctx context.Context, t types.DelegatedToken, raw string, now time.Time) (types.DelegatedToken, error)
	GetDelegatedTokenByRaw(ctx context.Context, raw string, now time.Time) (types.DelegatedToken, error)
}

var _ DelegateStore = PG{}

const delegateCols = `id, name, idp_client_id, scope_group, credential_sha256, registered_by, created_at, last_used_at, revoked_at`

func scanDelegate(row pgx.Row) (types.Delegate, error) {
	var d types.Delegate
	err := row.Scan(&d.ID, &d.Name, &d.IdPClientID, &d.Group, &d.CredentialSHA256, &d.RegisteredBy,
		&d.CreatedAt, &d.LastUsedAt, &d.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.Delegate{}, ErrNotFound
	}
	if err != nil {
		return types.Delegate{}, fmt.Errorf("store: scan delegate: %w", err)
	}
	return d, nil
}

// CreateDelegate registers one portal. raw is the PLAINTEXT credential; only
// its hash is stored. Returns ErrConflict on a credential_sha256 collision
// (a reused credential in a 256-bit space).
func (s PG) CreateDelegate(ctx context.Context, d types.Delegate, raw string) (types.Delegate, error) {
	const q = `
		INSERT INTO delegates (id, name, idp_client_id, scope_group, credential_sha256, registered_by)
		VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING ` + delegateCols
	out, err := scanDelegate(s.Pool.QueryRow(ctx, q, d.ID, d.Name, d.IdPClientID, d.Group, hashToken(raw), d.RegisteredBy))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return types.Delegate{}, ErrConflict
	}
	return out, err
}

// GetDelegateByRaw resolves the LIVE portal a credential stands for.
func (s PG) GetDelegateByRaw(ctx context.Context, raw string) (types.Delegate, error) {
	const q = `SELECT ` + delegateCols + ` FROM delegates WHERE credential_sha256 = $1 AND revoked_at IS NULL`
	return scanDelegate(s.Pool.QueryRow(ctx, q, hashToken(raw)))
}

// ListDelegates returns every portal, newest first, including revoked —
// admins need to see that a retired portal is retired.
func (s PG) ListDelegates(ctx context.Context) ([]types.Delegate, error) {
	const q = `SELECT ` + delegateCols + ` FROM delegates ORDER BY created_at DESC, id`
	return collect(ctx, s.Pool, "list", "delegates", q, nil, scanDelegate)
}

// RevokeDelegate marks id revoked. Unknown or already-revoked is ErrNotFound,
// so a second revoke writes no second audit row. Outstanding delegated
// tokens need no write of their own: GetDelegatedTokenByRaw joins on this
// column.
func (s PG) RevokeDelegate(ctx context.Context, id uuid.UUID, now time.Time) (types.Delegate, error) {
	const q = `
		UPDATE delegates SET revoked_at = $2
		WHERE id = $1 AND revoked_at IS NULL
		RETURNING ` + delegateCols
	return scanDelegate(s.Pool.QueryRow(ctx, q, id, now))
}

const delegatedTokenCols = `t.id, t.delegate_id, t.principal, t.email, t.user_type, t.groups, t.groups_truncated, t.created_at, t.expires_at`

func scanDelegatedToken(row pgx.Row) (types.DelegatedToken, error) {
	var t types.DelegatedToken
	var groups []byte
	err := row.Scan(&t.ID, &t.DelegateID, &t.Principal, &t.Email, &t.UserType, &groups, &t.GroupsTruncated,
		&t.CreatedAt, &t.ExpiresAt)
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
	age := db.AppClockAgeMicros(t.CreatedAt, now)
	q := `
		WITH swept AS (DELETE FROM delegated_tokens WHERE expires_at <= $11),
		     touched AS (UPDATE delegates SET last_used_at = $11 WHERE id = $2)
		INSERT INTO delegated_tokens AS t (id, delegate_id, token_sha256, principal, email, user_type, groups, groups_truncated, created_at, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,` + db.AppClockAgeSQL("$9") + `,$10)
		RETURNING ` + delegatedTokenCols
	return scanDelegatedToken(s.Pool.QueryRow(ctx, q, t.ID, t.DelegateID, hashToken(raw), t.Principal, t.Email,
		t.UserType, groups, t.GroupsTruncated, age, t.ExpiresAt, now))
}

// GetDelegatedTokenByRaw is the delegated lane's auth lookup: the token must
// be unexpired at now AND its portal still registered — the join is what
// makes revoking a portal kill all its tokens on their next request.
func (s PG) GetDelegatedTokenByRaw(ctx context.Context, raw string, now time.Time) (types.DelegatedToken, error) {
	const q = `
		SELECT ` + delegatedTokenCols + `
		FROM delegated_tokens t JOIN delegates d ON d.id = t.delegate_id
		WHERE t.token_sha256 = $1 AND t.expires_at > $2 AND d.revoked_at IS NULL`
	return scanDelegatedToken(s.Pool.QueryRow(ctx, q, hashToken(raw), now))
}
