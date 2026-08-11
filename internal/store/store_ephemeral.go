// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Short-lived control-plane handoff rows (migration 0026): single-use WS attach
// tickets and compose-run proposal uploads. Both were in-process maps, so a
// second control plane never saw them and a restart dropped them. Both are
// consume-once, and here that is a single DELETE ... RETURNING — the atomic form
// of the map's delete-on-read, exact under concurrency AND across processes.
// Kept out of store.go on purpose (it sits at a lint size boundary).
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
// it is bound to and the principal that minted it (attribution — the
// session.attach audit names the human, never the ticket).
type AttachTicket struct {
	RunID     uuid.UUID
	ActorType types.ActorType
	Principal string
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
		WITH swept AS (DELETE FROM attach_tickets WHERE expires_at <= $6)
		INSERT INTO attach_tickets (token_sha256, run_id, actor_type, principal, expires_at)
		VALUES ($1, $2, $3, $4, $5)`,
		hashAttachTicket(token), t.RunID, string(t.ActorType), t.Principal, expiresAt, now,
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
		RETURNING run_id, actor_type, principal`,
		hashAttachTicket(token), now,
	).Scan(&t.RunID, &actorType, &t.Principal)
	if errors.Is(err, pgx.ErrNoRows) {
		return AttachTicket{}, false, nil
	}
	if err != nil {
		return AttachTicket{}, false, fmt.Errorf("store: consume attach ticket: %w", err)
	}
	t.ActorType = types.ActorType(actorType)
	return t, true, nil
}

// composeResultTTL is how long an untaken compose upload lingers before the next
// PutComposeResult sweeps it. Generously larger than the launcher's whole wait
// window (composeRunWaitTimeout, 4m), so it can only ever collect rows whose
// waiter died — never one still being waited on.
const composeResultTTL = time.Hour

// PutComposeResult parks a compose run's RAW proposal upload for the launcher
// waiting on it, and sweeps abandoned rows in the same statement. A re-upload
// for the same run overwrites (the sync.Map's Store did too).
//
// The payload column is BYTEA: this is claude's raw stdout wrapper, which can
// legitimately be empty, non-JSON, or (in a crash) non-UTF-8 — the control
// plane deliberately does not parse or constrain it here (facts-out), exactly
// like the in-process map it replaced. See migration 0026.
func (s PG) PutComposeResult(ctx context.Context, runID uuid.UUID, payload []byte) error {
	_, err := s.Pool.Exec(ctx, `
		WITH swept AS (DELETE FROM compose_results WHERE created_at < now() - $3::interval)
		INSERT INTO compose_results (run_id, payload) VALUES ($1, $2)
		ON CONFLICT (run_id) DO UPDATE SET payload = EXCLUDED.payload, created_at = now()`,
		runID, payload, composeResultTTL.String(),
	)
	if err != nil {
		return fmt.Errorf("store: put compose result: %w", err)
	}
	return nil
}

// TakeComposeResult returns the parked upload for runID and deletes it in the
// same statement (delete-on-read), so exactly one waiter takes it no matter
// which control plane the sandbox uploaded to. A missing row is (false, nil) —
// "the run uploaded nothing" is the expected miss, not an error.
func (s PG) TakeComposeResult(ctx context.Context, runID uuid.UUID) ([]byte, bool, error) {
	var payload []byte
	err := s.Pool.QueryRow(ctx,
		`DELETE FROM compose_results WHERE run_id = $1 RETURNING payload`, runID,
	).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("store: take compose result: %w", err)
	}
	return payload, true, nil
}

// DiscardComposeResult drops an upload no one will take (the launcher's wait
// timed out). Idempotent: discarding a missing row is nil.
func (s PG) DiscardComposeResult(ctx context.Context, runID uuid.UUID) error {
	if _, err := s.Pool.Exec(ctx, `DELETE FROM compose_results WHERE run_id = $1`, runID); err != nil {
		return fmt.Errorf("store: discard compose result: %w", err)
	}
	return nil
}
