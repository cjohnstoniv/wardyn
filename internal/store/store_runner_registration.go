// Copyright 2025 The Wardyn Authors
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

// RunnerRegistrationStore is the optional capability behind the registration doors.
// A store without it cannot mint or redeem registration tokens.
type RunnerRegistrationStore interface {
	RunnerStore
	MintRunnerRegistrationToken(context.Context, string, types.RunnerRegistrationToken) (types.RunnerRegistrationToken, error)
	ConsumeRunnerRegistrationToken(context.Context, string, string, time.Time) (types.RunnerRegistrationToken, bool, error)
}

var _ RunnerRegistrationStore = PG{}

const runnerRegistrationCols = `id, owner, minted_by, token_sha256, org_url_sha256, created_at, expires_at, consumed_at`

func scanRunnerRegistrationToken(row pgx.Row) (types.RunnerRegistrationToken, error) {
	var t types.RunnerRegistrationToken
	err := row.Scan(&t.ID, &t.Owner, &t.MintedBy, &t.TokenSHA256, &t.OrgURLSHA256, &t.CreatedAt, &t.ExpiresAt, &t.ConsumedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, ErrNotFound
	}
	if err != nil {
		return t, fmt.Errorf("store: scan runner registration token: %w", err)
	}
	return t, nil
}

// MintRunnerRegistrationToken never stores the raw bearer.
func (s PG) MintRunnerRegistrationToken(ctx context.Context, raw string, token types.RunnerRegistrationToken) (types.RunnerRegistrationToken, error) {
	const q = `INSERT INTO runner_registration_tokens (id, owner, minted_by, token_sha256, org_url_sha256, created_at, expires_at)
 VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING ` + runnerRegistrationCols
	out, err := scanRunnerRegistrationToken(s.Pool.QueryRow(ctx, q, token.ID, token.Owner, token.MintedBy, hashToken(raw), token.OrgURLSHA256, token.CreatedAt, token.ExpiresAt))
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return types.RunnerRegistrationToken{}, ErrConflict
	}
	return out, err
}

// ConsumeRunnerRegistrationToken atomically spends a token bound to this org URL.
// Unknown, expired, consumed and differently bound tokens share the same refusal.
func (s PG) ConsumeRunnerRegistrationToken(ctx context.Context, raw, orgHash string, now time.Time) (types.RunnerRegistrationToken, bool, error) {
	const q = `UPDATE runner_registration_tokens SET consumed_at=$3
 WHERE token_sha256=$1 AND org_url_sha256=$2 AND consumed_at IS NULL AND expires_at>$3
 RETURNING ` + runnerRegistrationCols
	token, err := scanRunnerRegistrationToken(s.Pool.QueryRow(ctx, q, hashToken(raw), orgHash, now))
	if errors.Is(err, ErrNotFound) {
		return types.RunnerRegistrationToken{}, false, nil
	}
	return token, err == nil, err
}
