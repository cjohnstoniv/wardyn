// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

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

// ErrTokenNotMovable is MoveAPIToken's refusal of a token that is revoked or expired, or that no admin minted
// for someone else.
var ErrTokenNotMovable = errors.New("store: the token is not a live admin-minted token")

// ErrMoveTargetUnknown is MoveAPIToken's refusal of a service identity that does not exist. A retired one is
// ErrServiceIdentityInactive.
var ErrMoveTargetUnknown = errors.New("store: no such service identity")

// TokenReplacement is an invitation that names a token as the one its claim replaces, only while pending or
// once claimed: a revoked or expired one decides nothing about the token.
type TokenReplacement struct {
	TokenID   uuid.UUID
	Target    string
	ClaimedAt *time.Time // nil while pending
}

// TokenDispositionFacts is everything a disposition is derived from beside the token row itself.
type TokenDispositionFacts struct {
	Replacements map[uuid.UUID][]TokenReplacement
	Moves        map[uuid.UUID]types.TokenMove
}

// TokenMoveStore holds the links between admin-minted tokens and the service identities that replace them
// (api_token_moves, migration 0157, #1508). Optional, like InvitationStore.
type TokenMoveStore interface {
	// MoveAPIToken links a live admin-minted token to a non-retired service identity, replacing an earlier
	// link. It never revokes. ErrNotFound is no such token; ErrTokenNotMovable, ErrMoveTargetUnknown and
	// ErrServiceIdentityInactive (retired) are the refusals.
	MoveAPIToken(ctx context.Context, id uuid.UUID, serviceIdentity, by string, now time.Time) (types.TokenMove, error)
	// TokenDispositionFacts reads the invitations that replace tokens and the move rows, at now.
	TokenDispositionFacts(ctx context.Context, now time.Time) (TokenDispositionFacts, error)
}

var _ TokenMoveStore = PG{}

func (s PG) MoveAPIToken(ctx context.Context, id uuid.UUID, serviceIdentity, by string, now time.Time) (types.TokenMove, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return types.TokenMove{}, fmt.Errorf("store: move api token: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	// FOR UPDATE: a revoke committing between this check and the insert would otherwise link a dead token.
	var movable bool
	err = tx.QueryRow(ctx, `SELECT COALESCE(minted_by, '') NOT IN ('', principal) AND `+apiTokenLive+`
		FROM api_tokens WHERE id = $1 FOR UPDATE`, id).Scan(&movable)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.TokenMove{}, ErrNotFound
	}
	if err != nil {
		return types.TokenMove{}, fmt.Errorf("store: move api token: %w", err)
	}
	if !movable {
		return types.TokenMove{}, ErrTokenNotMovable
	}
	// FOR SHARE: a retire takes the row FOR UPDATE, so one cannot slip between this read and the insert.
	var retired *time.Time
	err = tx.QueryRow(ctx, `SELECT retired_at FROM service_identities WHERE name = $1 FOR SHARE`, serviceIdentity).Scan(&retired)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.TokenMove{}, ErrMoveTargetUnknown
	}
	if err != nil {
		return types.TokenMove{}, fmt.Errorf("store: move api token: %w", err)
	}
	if retired != nil {
		return types.TokenMove{}, ErrServiceIdentityInactive
	}
	move := types.TokenMove{TokenID: id, ServiceIdentity: serviceIdentity, LinkedBy: by, LinkedAt: now}
	if _, err = tx.Exec(ctx, `
		INSERT INTO api_token_moves (token_id, service_identity, linked_by, linked_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (token_id) DO UPDATE SET service_identity = EXCLUDED.service_identity,
			linked_by = EXCLUDED.linked_by, linked_at = EXCLUDED.linked_at`, id, serviceIdentity, by, now); err != nil {
		return types.TokenMove{}, fmt.Errorf("store: move api token: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return types.TokenMove{}, fmt.Errorf("store: commit api token move: %w", err)
	}
	return move, nil
}

func (s PG) TokenDispositionFacts(ctx context.Context, now time.Time) (TokenDispositionFacts, error) {
	out := TokenDispositionFacts{Replacements: map[uuid.UUID][]TokenReplacement{}, Moves: map[uuid.UUID]types.TokenMove{}}
	repl, err := collect(ctx, s.Pool, "list", "token replacements", `
		SELECT replaces_token_id, target, claimed_at FROM person_invitations
		WHERE replaces_token_id IS NOT NULL AND (claimed_at IS NOT NULL OR (revoked_at IS NULL AND expires_at > $1))`,
		[]any{now}, func(r pgx.Row) (TokenReplacement, error) {
			var t TokenReplacement
			err := r.Scan(&t.TokenID, &t.Target, &t.ClaimedAt)
			return t, err
		})
	if err != nil {
		return out, err
	}
	for _, t := range repl {
		out.Replacements[t.TokenID] = append(out.Replacements[t.TokenID], t)
	}
	moves, err := collect(ctx, s.Pool, "list", "token moves",
		`SELECT token_id, service_identity, linked_by, linked_at FROM api_token_moves`, nil, func(r pgx.Row) (types.TokenMove, error) {
			var m types.TokenMove
			err := r.Scan(&m.TokenID, &m.ServiceIdentity, &m.LinkedBy, &m.LinkedAt)
			return m, err
		})
	if err != nil {
		return out, err
	}
	for _, m := range moves {
		out.Moves[m.TokenID] = m
	}
	return out, nil
}
