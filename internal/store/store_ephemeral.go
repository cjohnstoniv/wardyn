// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Short-lived control-plane handoff row (migration 0026): single-use WS
// attach tickets, durable and shared across every control plane — an
// in-process map can't survive a restart or be seen by a second control
// plane. Consume-once is a single DELETE ... RETURNING, atomic under
// concurrency and across processes.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// hashToken returns hex(sha256(token)) — what's actually stored in every
// *_sha256 credential column this package writes. The raw token never
// reaches SQL, so a live-DB reader (reporting role, hot standby, pg_dump)
// can't read a usable bearer credential off the row.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// AttachTicket is what one redeemed single-use WS attach ticket carries: the
// run it's bound to, the minting principal (audit attribution), and that
// principal's role at mint time. The ?ticket= WS lane bypasses
// humanOrAdminAuth entirely, so this stamped role is the only signal
// available to re-check owner-or-admin at consume time.
type AttachTicket struct {
	RunID     uuid.UUID
	ActorType types.ActorType
	Principal string
	Role      string
	// Via is the delegated lane the ticket was minted on (#1142), nil
	// otherwise; the ticket lane replays it onto the rows it writes.
	Via *types.DelegationVia
	// AuthorizedAt is when the minting request was admitted, and Email the
	// verified email a session revoke may name ("" on the admin-token and local
	// lanes). Redemption checks both against the revoke cutoff (#1474). A zero
	// AuthorizedAt is a row written before the column existed: never exempt.
	AuthorizedAt time.Time
	Email        string
}

// MintAttachTicket records one outstanding ticket, expiring at expiresAt, and
// sweeps already-expired rows in the same statement. The sweep compares
// against the caller's clock (now), NOT now(), so a DB clock running ahead
// can't silently delete live tickets.
//
// ponytail: the sweep rides the mint instead of a background worker — the table
// holds only unredeemed tickets inside a 30s TTL, i.e. a handful of rows. Add a
// sweeper (or an expires_at index) only if mint volume ever makes that false.
func (s PG) MintAttachTicket(ctx context.Context, token string, t AttachTicket, now, expiresAt time.Time) error {
	var viaDelegate, viaGrant *uuid.UUID
	if t.Via != nil {
		viaDelegate, viaGrant = &t.Via.Delegate, &t.Via.Grant
	}
	_, err := s.Pool.Exec(ctx, `
		WITH swept AS (DELETE FROM attach_tickets WHERE expires_at <= $7)
		INSERT INTO attach_tickets (token_sha256, run_id, actor_type, principal, role, expires_at, via_delegate, via_grant, authorized_at, email)
		VALUES ($1, $2, $3, $4, $5, $6, $8, $9, $10, $11)`,
		hashToken(token), t.RunID, string(t.ActorType), t.Principal, t.Role, expiresAt, now, viaDelegate, viaGrant,
		t.AuthorizedAt, t.Email,
	)
	if err != nil {
		return fmt.Errorf("store: mint attach ticket: %w", err)
	}
	return nil
}

// ConsumeAttachTicket redeems token exactly once, returning the ticket it
// stood for. The DELETE ... RETURNING is the single-use guarantee: two
// racing redemptions can only have one return a row.
//
// The run-id binding is checked by the CALLER, not here, so a redemption
// against the wrong run still BURNS the ticket (a leaked ticket probed
// against a guessed run must not survive the probe). A miss and an expired
// row are both (ok=false), indistinguishable to the caller.
func (s PG) ConsumeAttachTicket(ctx context.Context, token string, now time.Time) (AttachTicket, bool, error) {
	var t AttachTicket
	var actorType string
	var viaDelegate, viaGrant *uuid.UUID
	var authorizedAt *time.Time
	var email *string
	err := s.Pool.QueryRow(ctx, `
		DELETE FROM attach_tickets
		WHERE token_sha256 = $1 AND expires_at > $2
		RETURNING run_id, actor_type, principal, role, via_delegate, via_grant, authorized_at, email`,
		hashToken(token), now,
	).Scan(&t.RunID, &actorType, &t.Principal, &t.Role, &viaDelegate, &viaGrant, &authorizedAt, &email)
	if errors.Is(err, pgx.ErrNoRows) {
		return AttachTicket{}, false, nil
	}
	if err != nil {
		return AttachTicket{}, false, fmt.Errorf("store: consume attach ticket: %w", err)
	}
	t.ActorType = types.ActorType(actorType)
	if authorizedAt != nil {
		t.AuthorizedAt = *authorizedAt
	}
	if email != nil {
		t.Email = *email
	}
	if viaDelegate != nil && viaGrant != nil {
		t.Via = &types.DelegationVia{Delegate: *viaDelegate, Grant: *viaGrant}
	}
	return t, true, nil
}
