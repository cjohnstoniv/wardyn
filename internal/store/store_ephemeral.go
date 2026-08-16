// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Short-lived control-plane handoff row (migration 0026): single-use WS attach
// tickets. Was an in-process map, so a second control plane never saw it and a
// restart dropped it. Consume-once, and here that is a single
// DELETE ... RETURNING — the atomic form of the map's delete-on-read, exact
// under concurrency AND across processes. Kept out of store.go on purpose (it
// sits at a lint size boundary).
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

// hashAttachTicket returns hex(sha256(token)) — what's actually stored in
// attach_tickets.token_sha256 (STORE-3). The raw token never leaves the
// minting process: MintAttachTicket hashes before INSERT and
// ConsumeAttachTicket hashes before the DELETE ... RETURNING, so every
// consume-once/expiry property is unchanged (the hash is just as unique and
// just as unguessable as the token it's derived from) while a live-DB reader
// (a reporting role, a hot standby, a pg_dump) can no longer read a usable
// bearer credential off the row.
func hashAttachTicket(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// AttachTicket is what one redeemed single-use WS attach ticket carries: the run
// it is bound to, the principal that minted it (attribution — the session.attach
// audit names the human, never the ticket), and that principal's role
// (admin/member — internal/auth/oidc's RoleAdmin/RoleMember) at mint time. The
// ?ticket= WS lane bypasses humanOrAdminAuth entirely, so this stamped role is
// the only signal available to re-check owner-or-admin at consume time (see
// internal/api's ticketOrHumanAuth / handleAttachWS).
type AttachTicket struct {
	RunID     uuid.UUID
	ActorType types.ActorType
	Principal string
	Role      string
}

// MintAttachTicket records one outstanding ticket, expiring at expiresAt, and
// sweeps already-expired rows in the same statement. The sweep compares against
// the caller's clock (now), NOT now(): expires_at is written from the app clock
// and consume compares against the app clock, so a DB clock running ahead must
// not silently delete live tickets.
//
// ponytail: the sweep rides the mint instead of a background worker — the table
// holds only unredeemed tickets inside a 30s TTL, i.e. a handful of rows. Add a
// sweeper (or an expires_at index) only if mint volume ever makes that false.
func (s PG) MintAttachTicket(ctx context.Context, token string, t AttachTicket, now, expiresAt time.Time) error {
	_, err := s.Pool.Exec(ctx, `
		WITH swept AS (DELETE FROM attach_tickets WHERE expires_at <= $7)
		INSERT INTO attach_tickets (token_sha256, run_id, actor_type, principal, role, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		hashAttachTicket(token), t.RunID, string(t.ActorType), t.Principal, t.Role, expiresAt, now,
	)
	if err != nil {
		return fmt.Errorf("store: mint attach ticket: %w", err)
	}
	return nil
}

// ConsumeAttachTicket redeems token exactly once, returning the ticket it stood
// for. The DELETE ... RETURNING is the single-use guarantee: two racing
// redemptions can only have one return a row.
//
// The run-id binding is checked by the CALLER, not here, so a redemption against
// the wrong run still BURNS the ticket — the in-memory map deleted on any
// redemption attempt and that property is load-bearing (a leaked ticket probed
// against a guessed run must not survive the probe). Expiry is in the WHERE
// instead: an expired row is unredeemable anyway and the next mint sweeps it.
// A miss and an expired row are both (ok=false), indistinguishable to the caller.
func (s PG) ConsumeAttachTicket(ctx context.Context, token string, now time.Time) (AttachTicket, bool, error) {
	var t AttachTicket
	var actorType string
	err := s.Pool.QueryRow(ctx, `
		DELETE FROM attach_tickets
		WHERE token_sha256 = $1 AND expires_at > $2
		RETURNING run_id, actor_type, principal, role`,
		hashAttachTicket(token), now,
	).Scan(&t.RunID, &actorType, &t.Principal, &t.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return AttachTicket{}, false, nil
	}
	if err != nil {
		return AttachTicket{}, false, fmt.Errorf("store: consume attach ticket: %w", err)
	}
	t.ActorType = types.ActorType(actorType)
	return t, true, nil
}
