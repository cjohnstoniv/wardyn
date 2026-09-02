// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Per-user API tokens (migration 0045). Kept out of store.go on purpose (it
// sits at a lint size boundary), mirroring store_sshkeys.go's split.
//
// The raw token never reaches SQL: every method here takes either an id or the
// raw string and hashes it with hashToken (store_ephemeral.go) before touching
// the table, so api_tokens.token_sha256 is the only form that exists at rest.
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

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// CreateAPIToken inserts one token row. raw is the PLAINTEXT credential; only
// its hash is stored, and the caller is responsible for returning the plaintext
// to its creator exactly once (it is unrecoverable afterwards). t.Token is
// ignored — passing the secret twice would be the one way to accidentally
// persist it.
//
// A unique_violation (23505) on token_sha256 is ErrConflict: that is a raw
// collision in a 256-bit random space, so in practice it means the caller
// reused a token value rather than minting a fresh one.
//
// groups_truncated (migration 0052) rides in as a *bool and binds as SQL NULL
// when nil — the same nil-is-its-own-state discipline marshalGroups keeps for
// the snapshot itself. NULL means "minted before anything recorded this", and
// its consumers read that as TRUNCATED, not as false. A plain bool here (or a
// DEFAULT FALSE on the column) would assert "complete" for every legacy row:
// fail OPEN, the exact thing the marker exists to prevent.
func (s PG) CreateAPIToken(ctx context.Context, t types.APIToken, raw string) (types.APIToken, error) {
	groups, err := marshalGroups(t.Groups)
	if err != nil {
		return types.APIToken{}, err
	}
	const q = `
		INSERT INTO api_tokens (id, principal, email, role, groups, groups_truncated, name, token_sha256, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		RETURNING ` + apiTokenCols
	out, err := scanAPIToken(s.Pool.QueryRow(ctx, q,
		t.ID, t.Principal, t.Email, t.Role, groups, t.GroupsTruncated, t.Name, hashToken(raw), t.CreatedAt))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return types.APIToken{}, ErrConflict
		}
		return types.APIToken{}, err
	}
	return out, nil
}

// GetAPITokenByRaw is the auth-time lookup: given the bearer string a caller
// presented, resolve the LIVE token row it stands for. Deliberately UNSCOPED by
// principal — the caller has not authenticated yet; this call is what
// authenticates them.
//
// `revoked_at IS NULL` is in the WHERE, not checked by the caller, and that is
// the security shape: a revoked token, an unknown token and a token whose hash
// does not match all fail IDENTICALLY with ErrNotFound, so the boundary is not
// an oracle for "this token used to exist".
func (s PG) GetAPITokenByRaw(ctx context.Context, raw string) (types.APIToken, error) {
	const q = `
		SELECT ` + apiTokenCols + `
		FROM api_tokens WHERE token_sha256 = $1 AND revoked_at IS NULL`
	return scanAPIToken(s.Pool.QueryRow(ctx, q, hashToken(raw)))
}

// TouchAPIToken records that id was just used. BEST EFFORT by contract: the auth
// branch ignores the error, because failing to record a touch must never fail an
// otherwise-valid request.
//
// ponytail: one UPDATE per authenticated token request. API tokens serve scripts
// and CI, not a browser's request storm, so the write volume is the caller's own
// call rate — add a coarse `AND last_used_at < now() - interval` throttle only if
// a hot token ever makes that false.
func (s PG) TouchAPIToken(ctx context.Context, id uuid.UUID, now time.Time) error {
	_, err := s.Pool.Exec(ctx, `UPDATE api_tokens SET last_used_at = $2 WHERE id = $1`, id, now)
	if err != nil {
		return fmt.Errorf("store: touch api token: %w", err)
	}
	return nil
}

// ListAPITokensByPrincipal returns principal's own tokens, newest first — the
// self-service GET /me/tokens list. Revoked rows are INCLUDED: a human needs to
// see that the token they retired is in fact retired, and the row carries no
// usable credential either way.
func (s PG) ListAPITokensByPrincipal(ctx context.Context, principal string) ([]types.APIToken, error) {
	const q = `
		SELECT ` + apiTokenCols + `
		FROM api_tokens WHERE principal = $1 ORDER BY created_at DESC`
	return queryAPITokens(ctx, s, q, principal)
}

// ListAPITokens returns every token in the deployment, newest first — the admin
// inventory (GET /tokens). Revoked rows included, same reason as the
// self-service list.
func (s PG) ListAPITokens(ctx context.Context) ([]types.APIToken, error) {
	const q = `
		SELECT ` + apiTokenCols + `
		FROM api_tokens ORDER BY created_at DESC`
	return queryAPITokens(ctx, s, q)
}

// RevokeAPIToken marks id revoked. principal scopes the UPDATE when non-empty
// (the self-service path, where a human may only ever revoke their OWN token and
// someone else's id is ErrNotFound rather than a distinguishable 403 — no
// existence leak across principals); an EMPTY principal is the ADMIN path and
// revokes anyone's.
//
// Already-revoked is ErrNotFound too (`revoked_at IS NULL` in the WHERE): revoke
// is idempotent in effect, and a second call must not emit a second
// `token.revoke` audit row for an act that did not happen.
func (s PG) RevokeAPIToken(ctx context.Context, id uuid.UUID, principal string, now time.Time) (types.APIToken, error) {
	const q = `
		UPDATE api_tokens SET revoked_at = $2
		WHERE id = $1 AND revoked_at IS NULL AND ($3 = '' OR principal = $3)
		RETURNING ` + apiTokenCols
	return scanAPIToken(s.Pool.QueryRow(ctx, q, id, now, principal))
}

// queryAPITokens is the two token lists' shared read. collect (pagination.go)
// already generalises the rows loop, INCLUDING the "empty, never nil" contract
// this hand-rolled version re-derived with its own `out := []types.APIToken{}`
// — the API renders these as `[]`, never `null`.
func queryAPITokens(ctx context.Context, s PG, q string, args ...any) ([]types.APIToken, error) {
	return collect(ctx, s.Pool, "list", "api tokens", q, args, scanAPIToken)
}

// marshalGroups encodes the session's group snapshot for the nullable JSONB
// column. nil marshals to a SQL NULL, not to 'null' and not to '[]': nil and
// empty mean different things to the capability resolver (see oidcGroupsCtxKey
// in internal/api/http.go), and the round trip has to preserve which one was
// stamped.
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

// apiTokenCols is THE api_tokens READ column list, in scanAPIToken's order
// (five pasted sites). The INSERT list stays spelled out on purpose: it names
// token_sha256, which no read ever selects (the hash never leaves the row),
// and omits last_used_at / revoked_at, which no insert sets. Those lists
// differ in BOTH directions, so deriving one from the other would hide that.
const apiTokenCols = `id, principal, email, role, groups, groups_truncated, name, created_at, last_used_at, revoked_at`

func scanAPIToken(row pgx.Row) (types.APIToken, error) {
	var t types.APIToken
	var groups []byte
	err := row.Scan(&t.ID, &t.Principal, &t.Email, &t.Role, &groups, &t.GroupsTruncated, &t.Name,
		&t.CreatedAt, &t.LastUsedAt, &t.RevokedAt)
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
