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
)

// RunCreateKeyTTL is how long a POST /runs Idempotency-Key answers for its first request, from first use.
// A constant: it outlasts any CI job's retries, and the rows are small.
const RunCreateKeyTTL = 24 * time.Hour

// MaxRunCreateResponseBytes caps the answer kept for a key. A larger body is not kept, and a replay is
// rebuilt from the run instead.
const MaxRunCreateResponseBytes = 256 << 10

// ErrRunCreateKeySuperseded refuses a run insert whose key claim is no longer current: its lease was
// taken over by a later request, or the key already names a run. The insert rolls back with it.
var ErrRunCreateKeySuperseded = errors.New("store: idempotency key claim superseded")

// RunCreateKeyID is one key's scope: the caller, the delegate that acted for it ("" for a direct
// request) and the SHA-256 of the key. The key itself is never stored.
type RunCreateKeyID struct {
	Principal string
	Via       string
	KeySHA256 string
}

// RunCreateKey is a key and the fingerprint of the request it came with.
type RunCreateKey struct {
	RunCreateKeyID
	RequestSHA256 string
}

// RunCreateKeyState is ClaimRunCreateKey's answer.
type RunCreateKeyState int

const (
	// RunCreateKeyClaimed: the caller holds the key under Token until it inserts its run or releases it.
	RunCreateKeyClaimed RunCreateKeyState = iota + 1
	// RunCreateKeyMismatch: the key was used with a different request.
	RunCreateKeyMismatch
	// RunCreateKeyInFlight: another request holds the key and its lease is live.
	RunCreateKeyInFlight
	// RunCreateKeyDone: the key names RunID; Response is the answer kept for it, nil when none was kept.
	RunCreateKeyDone
)

// RunCreateResponse is the answer a key's first request got.
type RunCreateResponse struct {
	Status   int
	Location string
	Body     []byte
}

// RunCreateKeyClaim is the state of one key after a claim.
type RunCreateKeyClaim struct {
	State     RunCreateKeyState
	Token     uuid.UUID          // RunCreateKeyClaimed
	RunID     uuid.UUID          // RunCreateKeyDone
	Response  *RunCreateResponse // RunCreateKeyDone, when an answer was kept
	CreatedAt time.Time          // the key's first use (every state but Claimed of a new row)
	// ClaimUntil is when the lease of the key's latest claim ends, and LeaseLive whether it was still
	// ahead of the database clock at the read. Both are read before any takeover and are unset for a new
	// row. A Done key with no saved answer and a live lease may still be answering its first request.
	ClaimUntil time.Time
	LeaseLive  bool
	// ResponseErased: erasing the person's run tasks cleared the kept answer (ClearRunCreateResponsesOf).
	ResponseErased bool
}

// RunCreateKeyStore keeps POST /runs Idempotency-Keys. A claim leases the key to one request; the run
// insert binds the key to its run in the insert's own transaction (WithRunCreateKey); the answer is
// then saved for replay, or the claim released when no run was made.
type RunCreateKeyStore interface {
	ClaimRunCreateKey(ctx context.Context, k RunCreateKey, lease time.Duration) (RunCreateKeyClaim, error)
	SaveRunCreateResponse(ctx context.Context, id RunCreateKeyID, token uuid.UUID, status int, location string, body []byte) (bool, error)
	ReleaseRunCreateKey(ctx context.Context, id RunCreateKeyID, token uuid.UUID) error
}

var _ RunCreateKeyStore = PG{}

// claimAttempts bounds the claim's insert-or-read loop: a read finds no row only when the holder
// released it between this claim's insert and its read, and each turn retries the insert.
const claimAttempts = 3

// ClaimRunCreateKey leases k to the caller for lease, or reports why it cannot: the key was used with
// another request, another request holds a live lease, or the key already names a run. A key past its
// 24 hours is deleted and claimed afresh. An unbound claim whose lease ran out is taken over by the same
// request under a new token, which makes the old holder's insert fail (ErrRunCreateKeySuperseded).
func (s PG) ClaimRunCreateKey(ctx context.Context, k RunCreateKey, lease time.Duration) (RunCreateKeyClaim, error) {
	var out RunCreateKeyClaim
	err := s.inTx(ctx, func(q Querier) error {
		if _, err := q.Exec(ctx, `DELETE FROM run_create_keys
			WHERE principal = $1 AND via = $2 AND key_sha256 = $3 AND expires_at <= now()`,
			k.Principal, k.Via, k.KeySHA256); err != nil {
			return fmt.Errorf("store: delete expired idempotency key: %w", err)
		}
		for range claimAttempts {
			token := uuid.New()
			err := q.QueryRow(ctx, `INSERT INTO run_create_keys
				(principal, via, key_sha256, request_sha256, claim_token, claim_until, expires_at)
				VALUES ($1, $2, $3, $4, $5, now() + make_interval(secs => $6), now() + make_interval(secs => $7))
				ON CONFLICT (principal, via, key_sha256) DO NOTHING
				RETURNING claim_token`,
				k.Principal, k.Via, k.KeySHA256, k.RequestSHA256, token, lease.Seconds(), RunCreateKeyTTL.Seconds()).Scan(&out.Token)
			if err == nil {
				out.State = RunCreateKeyClaimed
				return nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("store: claim idempotency key: %w", err)
			}
			found, err := readRunCreateKey(ctx, q, k, token, lease, &out)
			if err != nil || found {
				return err
			}
		}
		out = RunCreateKeyClaim{State: RunCreateKeyInFlight}
		return nil
	})
	if err != nil {
		return RunCreateKeyClaim{}, err
	}
	return out, nil
}

// readRunCreateKey locks the key's existing row and fills out from it, taking the lease over under
// token when the row is an unbound claim of the same request whose lease ran out. It reports false when
// the row is gone (released since the insert found it).
func readRunCreateKey(ctx context.Context, q Querier, k RunCreateKey, token uuid.UUID, lease time.Duration, out *RunCreateKeyClaim) (bool, error) {
	var (
		request  string
		live     bool
		runID    *uuid.UUID
		status   *int
		location *string
		body     []byte
	)
	err := q.QueryRow(ctx, `SELECT request_sha256, claim_until > now(), run_id, response_status, response_location,
			response_body, created_at, claim_until, response_erased
		FROM run_create_keys WHERE principal = $1 AND via = $2 AND key_sha256 = $3 FOR UPDATE`,
		k.Principal, k.Via, k.KeySHA256).Scan(&request, &live, &runID, &status, &location, &body, &out.CreatedAt, &out.ClaimUntil,
		&out.ResponseErased)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: read idempotency key: %w", err)
	}
	out.LeaseLive = live
	switch {
	case request != k.RequestSHA256:
		out.State = RunCreateKeyMismatch
	case runID != nil:
		out.State, out.RunID = RunCreateKeyDone, *runID
		if status != nil {
			out.Response = &RunCreateResponse{Status: *status, Body: body}
			if location != nil {
				out.Response.Location = *location
			}
		}
	case live:
		out.State = RunCreateKeyInFlight
	default:
		tag, err := q.Exec(ctx, `UPDATE run_create_keys SET claim_token = $4, claim_until = now() + make_interval(secs => $5)
			WHERE principal = $1 AND via = $2 AND key_sha256 = $3 AND run_id IS NULL AND claim_until < now() AND request_sha256 = $6`,
			k.Principal, k.Via, k.KeySHA256, token, lease.Seconds(), k.RequestSHA256)
		if err != nil {
			return false, fmt.Errorf("store: take over idempotency key: %w", err)
		}
		if tag.RowsAffected() != 1 {
			// The row is locked FOR UPDATE above, so only a lease ending exactly at now() (the
			// transaction's start) lands here; it is treated as still live.
			out.State = RunCreateKeyInFlight
			return true, nil
		}
		out.State, out.Token = RunCreateKeyClaimed, token
	}
	return true, nil
}

type runCreateKeyCtx struct{}

type runCreateKeyClaimRef struct {
	id    RunCreateKeyID
	token uuid.UUID
}

// WithRunCreateKey makes the run insert writers (CreateRun, CreateRunUnderCap, CreateQueuedRun) bind the
// key id to the run they insert, in the insert's own transaction, only while token is still the key's
// claim and the key names no run. Otherwise the insert rolls back with ErrRunCreateKeySuperseded.
func WithRunCreateKey(ctx context.Context, id RunCreateKeyID, token uuid.UUID) context.Context {
	return context.WithValue(ctx, runCreateKeyCtx{}, runCreateKeyClaimRef{id: id, token: token})
}

func hasRunCreateKey(ctx context.Context) bool {
	_, ok := ctx.Value(runCreateKeyCtx{}).(runCreateKeyClaimRef)
	return ok
}

// bindRunCreateKey binds the context's key, if any, to runID. q must be the transaction that inserted
// the run.
func bindRunCreateKey(ctx context.Context, q queryRower, runID uuid.UUID) error {
	c, ok := ctx.Value(runCreateKeyCtx{}).(runCreateKeyClaimRef)
	if !ok {
		return nil
	}
	var bound uuid.UUID
	err := q.QueryRow(ctx, `UPDATE run_create_keys SET run_id = $5
		WHERE principal = $1 AND via = $2 AND key_sha256 = $3 AND claim_token = $4 AND run_id IS NULL
		RETURNING run_id`, c.id.Principal, c.id.Via, c.id.KeySHA256, c.token, runID).Scan(&bound)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrRunCreateKeySuperseded
	}
	if err != nil {
		return fmt.Errorf("store: bind idempotency key: %w", err)
	}
	return nil
}

// SaveRunCreateResponse keeps the answer for a key that names a run under token, whatever its status, so
// a refusal after the insert replays as itself. It reports whether it kept it: an unbound claim keeps
// nothing, so a refusal before the insert is never replayed, and nor does a key whose answer an erasure
// cleared (the release that follows a false finds the key bound and deletes nothing). A body over
// MaxRunCreateResponseBytes is not kept, and a replay is then rebuilt from the run.
func (s PG) SaveRunCreateResponse(ctx context.Context, id RunCreateKeyID, token uuid.UUID, status int, location string, body []byte) (bool, error) {
	var (
		st  *int
		loc *string
		raw []byte
	)
	if len(body) <= MaxRunCreateResponseBytes {
		st, loc, raw = &status, &location, body
		if raw == nil {
			raw = []byte{}
		}
	}
	tag, err := s.Pool.Exec(ctx, `UPDATE run_create_keys SET response_status = $5, response_location = $6, response_body = $7
		WHERE principal = $1 AND via = $2 AND key_sha256 = $3 AND claim_token = $4 AND run_id IS NOT NULL
			AND NOT response_erased`,
		id.Principal, id.Via, id.KeySHA256, token, st, loc, raw)
	if err != nil {
		return false, fmt.Errorf("store: save idempotency key response: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// ReleaseRunCreateKey deletes the key's claim under token when it names no run, so a request refused
// before its insert leaves the key free for a retry. A key bound to a run stays.
func (s PG) ReleaseRunCreateKey(ctx context.Context, id RunCreateKeyID, token uuid.UUID) error {
	if _, err := s.Pool.Exec(ctx, `DELETE FROM run_create_keys
		WHERE principal = $1 AND via = $2 AND key_sha256 = $3 AND claim_token = $4 AND run_id IS NULL`,
		id.Principal, id.Via, id.KeySHA256, token); err != nil {
		return fmt.Errorf("store: release idempotency key: %w", err)
	}
	return nil
}
