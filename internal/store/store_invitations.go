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

// ErrInvitationCap is CreateInvitation's refusal when the target already has the maximum pending.
var ErrInvitationCap = errors.New("store: too many pending invitations for the target")

// ErrInvitationNotPending is ClaimInvitation's and RevokeInvitation's refusal of an invitation that
// is claimed, revoked or expired, or was before a concurrent writer got there.
var ErrInvitationNotPending = errors.New("store: invitation is not pending")

// InvitationStore holds person invitations (migration 0157, #1508). Optional, like PersonStore: the
// API answers 501 on a store without it.
type InvitationStore interface {
	// CreateInvitation inserts inv, or returns ErrInvitationCap when maxPending invitations are
	// already pending for inv.Target at now.
	CreateInvitation(ctx context.Context, inv types.Invitation, maxPending int, now time.Time) (types.Invitation, error)
	// GetInvitation is the row with exactly id, or ErrNotFound. A non-empty target also has to match:
	// the claim reads by id AND target, so another person's id is ErrNotFound.
	GetInvitation(ctx context.Context, id uuid.UUID, target string) (types.Invitation, error)
	// ListInvitations returns invitations newest first, narrowed to target and state when set.
	ListInvitations(ctx context.Context, target string, state types.InvitationState, now time.Time, p Page) ([]types.Invitation, error)
	// RevokeInvitation revokes a pending invitation: ErrNotFound for no such id, ErrInvitationNotPending otherwise.
	RevokeInvitation(ctx context.Context, id uuid.UUID, by string, now time.Time) (types.Invitation, error)
	// ClaimInvitation, in one transaction, inserts c.Token, marks the invitation claimed by it and
	// revokes the token the invitation replaces. ErrInvitationNotPending leaves nothing behind.
	ClaimInvitation(ctx context.Context, c InvitationClaim) (InvitationClaimed, error)
}

var _ InvitationStore = PG{}

// InvitationClaim is one claim: the invitation, the only principal that may claim it, the clock
// that decides expiry, and the token to mint with its plaintext.
type InvitationClaim struct {
	ID     uuid.UUID
	Target string
	Now    time.Time
	Token  types.APIToken
	Raw    string
}

// InvitationClaimed is what a claim committed. Retired is the admin-minted token the invitation
// replaced, nil when it had none or that token was no longer live.
type InvitationClaimed struct {
	Invitation types.Invitation
	Token      types.APIToken
	Retired    *types.APIToken
}

const invitationCols = `id, target, created_by, created_at, expires_at, token_name, token_ttl_seconds,
	replaces_token_id, claimed_at, token_id, revoked_at, COALESCE(revoked_by, '')`

func scanInvitation(row pgx.Row) (types.Invitation, error) {
	var i types.Invitation
	err := row.Scan(&i.ID, &i.Target, &i.CreatedBy, &i.CreatedAt, &i.ExpiresAt, &i.TokenName, &i.TokenTTLSeconds,
		&i.ReplacesTokenID, &i.ClaimedAt, &i.TokenID, &i.RevokedAt, &i.RevokedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.Invitation{}, ErrNotFound
	}
	if err != nil {
		return types.Invitation{}, fmt.Errorf("store: scan invitation: %w", err)
	}
	return i, nil
}

// pendingSQL is the pending predicate on person_invitations, $1 being the clock.
const pendingSQL = `claimed_at IS NULL AND revoked_at IS NULL AND expires_at > $1`

func (s PG) CreateInvitation(ctx context.Context, inv types.Invitation, maxPending int, now time.Time) (types.Invitation, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return types.Invitation{}, fmt.Errorf("store: create invitation: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	// Serialises creates for one target, so two at the cap cannot both pass the count.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('person_invitations:' || $1, 0))`, inv.Target); err != nil {
		return types.Invitation{}, fmt.Errorf("store: create invitation: %w", err)
	}
	var pending int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM person_invitations WHERE target = $2 AND `+pendingSQL, now, inv.Target).Scan(&pending); err != nil {
		return types.Invitation{}, fmt.Errorf("store: count pending invitations: %w", err)
	}
	if pending >= maxPending {
		return types.Invitation{}, ErrInvitationCap
	}
	out, err := scanInvitation(tx.QueryRow(ctx, `
		INSERT INTO person_invitations (id, target, created_by, created_at, expires_at, token_name, token_ttl_seconds, replaces_token_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING `+invitationCols,
		inv.ID, inv.Target, inv.CreatedBy, inv.CreatedAt, inv.ExpiresAt, inv.TokenName, inv.TokenTTLSeconds, inv.ReplacesTokenID))
	if err != nil {
		return types.Invitation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return types.Invitation{}, fmt.Errorf("store: commit invitation: %w", err)
	}
	out.State = out.StateAt(now)
	return out, nil
}

func (s PG) GetInvitation(ctx context.Context, id uuid.UUID, target string) (types.Invitation, error) {
	return scanInvitation(s.Pool.QueryRow(ctx,
		`SELECT `+invitationCols+` FROM person_invitations WHERE id = $1 AND ($2 = '' OR target = $2)`, id, target))
}

func (s PG) ListInvitations(ctx context.Context, target string, state types.InvitationState, now time.Time, p Page) ([]types.Invitation, error) {
	// $1 is bound for every state so Postgres can type it even when the filter does not read it.
	where := `$1::timestamptz IS NOT NULL AND ($2 = '' OR target = $2)`
	switch state {
	case types.InvitationPending:
		where += ` AND ` + pendingSQL
	case types.InvitationClaimed:
		where += ` AND claimed_at IS NOT NULL`
	case types.InvitationRevoked:
		where += ` AND revoked_at IS NOT NULL`
	case types.InvitationExpired:
		where += ` AND claimed_at IS NULL AND revoked_at IS NULL AND expires_at <= $1`
	}
	q, args := p.appendTo(`SELECT `+invitationCols+` FROM person_invitations WHERE `+where+` ORDER BY created_at DESC, id`, []any{now, target})
	out, err := collect(ctx, s.Pool, "list", "invitations", q, args, scanInvitation)
	for i := range out {
		out[i].State = out[i].StateAt(now)
	}
	return out, err
}

func (s PG) RevokeInvitation(ctx context.Context, id uuid.UUID, by string, now time.Time) (types.Invitation, error) {
	out, err := scanInvitation(s.Pool.QueryRow(ctx, `
		UPDATE person_invitations SET revoked_at = $2, revoked_by = $3
		WHERE id = $1 AND claimed_at IS NULL AND revoked_at IS NULL AND expires_at > $2
		RETURNING `+invitationCols, id, now, by))
	if errors.Is(err, ErrNotFound) {
		// Distinguish "never existed" from "no longer pending" so the caller can answer 404 for both
		// without a second read of its own.
		if _, gerr := s.GetInvitation(ctx, id, ""); gerr == nil {
			return types.Invitation{}, ErrInvitationNotPending
		}
		return types.Invitation{}, ErrNotFound
	}
	out.State = types.InvitationRevoked
	return out, err
}

func (s PG) ClaimInvitation(ctx context.Context, c InvitationClaim) (InvitationClaimed, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return InvitationClaimed{}, fmt.Errorf("store: claim invitation: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if g, ok := identityGuardFrom(ctx); ok {
		if err := checkIdentityGuard(ctx, tx, g); err != nil {
			return InvitationClaimed{}, err
		}
	}
	token, err := s.insertAPIToken(ctx, tx, c.Token, c.Raw)
	if err != nil {
		return InvitationClaimed{}, err
	}
	// The conditional update is the claim: a concurrent claim, revoke or expiry makes it match no row,
	// and the rollback takes the inserted token with it.
	inv, err := scanInvitation(tx.QueryRow(ctx, `
		UPDATE person_invitations SET claimed_at = $3, token_id = $4
		WHERE id = $1 AND target = $2 AND claimed_at IS NULL AND revoked_at IS NULL AND expires_at > $3
		RETURNING `+invitationCols, c.ID, c.Target, c.Now, token.ID))
	if errors.Is(err, ErrNotFound) {
		return InvitationClaimed{}, ErrInvitationNotPending
	}
	if err != nil {
		return InvitationClaimed{}, err
	}
	out := InvitationClaimed{Invitation: inv, Token: token}
	if inv.ReplacesTokenID != nil {
		retired, err := scanAPIToken(tx.QueryRow(ctx, `
			UPDATE api_tokens SET revoked_at = $2 WHERE id = $1 AND principal = $3 AND revoked_at IS NULL
			RETURNING `+apiTokenCols, *inv.ReplacesTokenID, c.Now, c.Target))
		switch {
		case err == nil:
			out.Retired = &retired
		case !errors.Is(err, ErrNotFound):
			return InvitationClaimed{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return InvitationClaimed{}, fmt.Errorf("store: commit invitation claim: %w", err)
	}
	out.Invitation.State = types.InvitationClaimed
	return out, nil
}
