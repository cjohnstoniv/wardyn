// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Registered runners, their pending actions and the credential delivery policy (migration 0139).
// Registration tokens and the routes that mint and consume them are a later lane's; this file is the
// rows the stream, the claim and the pending-action queue read and write.
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

// RunnerStore is the optional store capability behind client mode, optional like DeviceStore: a fake
// or a non-Postgres store implements it or the runner routes answer 501.
type RunnerStore interface {
	CreateRunner(ctx context.Context, r types.Runner) (types.Runner, error)
	GetRunner(ctx context.Context, id uuid.UUID) (types.Runner, error)
	ListRunners(ctx context.Context) ([]types.Runner, error)
	ListRunnersByOwner(ctx context.Context, owner string) ([]types.Runner, error)
	ClaimRunner(ctx context.Context, id uuid.UUID, owner, fingerprint string, now time.Time) (types.Runner, error)
	TouchRunner(ctx context.Context, id uuid.UUID, version string, now time.Time) error
	RecordRunnerPosture(ctx context.Context, id uuid.UUID, p types.RunnerPosture, now time.Time) (changed bool, err error)
	RevokeRunner(ctx context.Context, id uuid.UUID, now time.Time) (types.Runner, error)
	ExpireUnclaimedRunners(ctx context.Context, createdBefore time.Time) (int, error)

	QueueRunnerAction(ctx context.Context, a types.RunnerPendingAction) (action types.RunnerPendingAction, queued bool, err error)
	ListPendingRunnerActions(ctx context.Context, runnerID uuid.UUID) ([]types.RunnerPendingAction, error)
	RunHasPendingRunnerAction(ctx context.Context, runID uuid.UUID) (bool, error)
	ApplyRunnerAction(ctx context.Context, id uuid.UUID, outcome string, observedAt, now time.Time) (types.RunnerPendingAction, error)

	GetCredentialDeliveryPolicy(ctx context.Context) (types.CredentialDeliveryPolicyDoc, error)
	PutCredentialDeliveryPolicy(ctx context.Context, policy json.RawMessage, by string, now time.Time) (types.CredentialDeliveryPolicyDoc, error)
}

var _ RunnerStore = PG{}

// ErrRunnerClaimMismatch is ClaimRunner's refusal when the runner is unclaimed but the caller is not
// its owner or the fingerprint differs: the API answers runner_claim_mismatch.
var ErrRunnerClaimMismatch = errors.New("store: runner claim does not match the owner or key fingerprint")

const runnerCols = `id, owner, name, public_key, key_fingerprint, state, version, created_at, claimed_at,
	last_seen_at, revoked_at, posture, posture_reported_at, posture_source, org_url_sha256`

func scanRunner(row pgx.Row) (types.Runner, error) {
	var r types.Runner
	var state string
	var posture []byte
	err := row.Scan(&r.ID, &r.Owner, &r.Name, &r.PublicKey, &r.KeyFingerprint, &state, &r.Version, &r.CreatedAt,
		&r.ClaimedAt, &r.LastSeenAt, &r.RevokedAt, &posture, &r.PostureReportedAt, &r.PostureSource, &r.OrgURLSHA256)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.Runner{}, ErrNotFound
	}
	if err != nil {
		return types.Runner{}, fmt.Errorf("store: scan runner: %w", err)
	}
	r.State = types.RunnerState(state)
	if err := json.Unmarshal(posture, &r.Posture); err != nil {
		return types.Runner{}, fmt.Errorf("store: unmarshal runner posture: %w", err)
	}
	return r, nil
}

// CreateRunner inserts an unclaimed runner: state, claim and posture on r are ignored, since every
// registration starts unclaimed. ErrConflict on a key fingerprint already registered.
func (s PG) CreateRunner(ctx context.Context, r types.Runner) (types.Runner, error) {
	const q = `
		INSERT INTO runners (id, owner, name, public_key, key_fingerprint, version, org_url_sha256)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		RETURNING ` + runnerCols
	out, err := scanRunner(s.Pool.QueryRow(ctx, q, r.ID, r.Owner, r.Name, r.PublicKey, r.KeyFingerprint, r.Version, r.OrgURLSHA256))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return types.Runner{}, ErrConflict
	}
	return out, err
}

func (s PG) GetRunner(ctx context.Context, id uuid.UUID) (types.Runner, error) {
	return scanRunner(s.Pool.QueryRow(ctx, `SELECT `+runnerCols+` FROM runners WHERE id = $1`, id))
}

// ListRunners returns every runner, newest first, revoked included.
func (s PG) ListRunners(ctx context.Context) ([]types.Runner, error) {
	return collect(ctx, s.Pool, "list", "runners", `SELECT `+runnerCols+` FROM runners ORDER BY created_at DESC, id`, nil, scanRunner)
}

func (s PG) ListRunnersByOwner(ctx context.Context, owner string) ([]types.Runner, error) {
	const q = `SELECT ` + runnerCols + ` FROM runners WHERE owner = $1 ORDER BY created_at DESC, id`
	return collect(ctx, s.Pool, "list", "runners", q, []any{owner}, scanRunner)
}

// ClaimRunner moves an unclaimed runner to claimed in one conditional UPDATE, only for its owner and
// the fingerprint it was registered with. ErrNotFound: no such runner. ErrConflict: it is not
// unclaimed (claimed twice, revoked). ErrRunnerClaimMismatch: wrong owner or fingerprint.
func (s PG) ClaimRunner(ctx context.Context, id uuid.UUID, owner, fingerprint string, now time.Time) (types.Runner, error) {
	const q = `
		UPDATE runners SET state = 'claimed', claimed_at = $4
		WHERE id = $1 AND state = 'unclaimed' AND owner = $2 AND key_fingerprint = $3 AND created_at > $4::timestamptz - interval '24 hours'
		RETURNING ` + runnerCols
	out, err := scanRunner(s.Pool.QueryRow(ctx, q, id, owner, fingerprint, now))
	if !errors.Is(err, ErrNotFound) {
		return out, err
	}
	cur, err := s.GetRunner(ctx, id)
	if err != nil {
		return types.Runner{}, err
	}
	if cur.State != types.RunnerUnclaimed {
		return types.Runner{}, ErrConflict
	}
	return types.Runner{}, ErrRunnerClaimMismatch
}

// TouchRunner records an authenticated session: last_seen_at and the runner's version. Best effort in
// the caller, like TouchDevice.
func (s PG) TouchRunner(ctx context.Context, id uuid.UUID, version string, now time.Time) error {
	if _, err := s.Pool.Exec(ctx, `UPDATE runners SET last_seen_at = $2, version = $3 WHERE id = $1`, id, now, version); err != nil {
		return fmt.Errorf("store: touch runner: %w", err)
	}
	return nil
}

// RecordRunnerPosture stores a posture report from a claimed runner and says whether it differs from
// the one held, so only a change writes runner.posture.record. A runner that is not claimed is ErrNotFound.
func (s PG) RecordRunnerPosture(ctx context.Context, id uuid.UUID, p types.RunnerPosture, now time.Time) (bool, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return false, err
	}
	const q = `
		UPDATE runners r SET posture = $2::jsonb, posture_reported_at = $3, posture_source = 'runner_asserted'
		FROM (SELECT id, posture FROM runners WHERE id = $1 AND state = 'claimed' FOR UPDATE) old
		WHERE r.id = old.id
		RETURNING old.posture IS DISTINCT FROM $2::jsonb`
	var changed bool
	err = s.Pool.QueryRow(ctx, q, id, b, now).Scan(&changed)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("store: record runner posture: %w", err)
	}
	return changed, nil
}

// RevokeRunner marks the runner revoked and drops its open pending actions in the same transaction (a
// revoked runner cannot authenticate to apply them). Already revoked or unknown is ErrNotFound, so a
// second call writes no second revoke row.
func (s PG) RevokeRunner(ctx context.Context, id uuid.UUID, now time.Time) (types.Runner, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return types.Runner{}, fmt.Errorf("store: begin revoke runner: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // a no-op after Commit
	const q = `UPDATE runners SET state = 'revoked', revoked_at = $2 WHERE id = $1 AND state <> 'revoked' RETURNING ` + runnerCols
	out, err := scanRunner(tx.QueryRow(ctx, q, id, now))
	if err != nil {
		return types.Runner{}, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM runner_pending_actions WHERE runner_id = $1 AND applied_at IS NULL`, id); err != nil {
		return types.Runner{}, fmt.Errorf("store: drop revoked runner's pending actions: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return types.Runner{}, fmt.Errorf("store: commit revoke runner: %w", err)
	}
	return out, nil
}

// ExpireUnclaimedRunners deletes unclaimed runners registered before createdBefore (an unclaimed
// runner lives 24h) and returns how many.
func (s PG) ExpireUnclaimedRunners(ctx context.Context, createdBefore time.Time) (int, error) {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM runners WHERE state = 'unclaimed' AND created_at < $1`, createdBefore)
	if err != nil {
		return 0, fmt.Errorf("store: expire unclaimed runners: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

const actionCols = `id, runner_id, run_id, kind, ref, queued_at, applied_at, outcome, observed_at`

func scanAction(row pgx.Row) (types.RunnerPendingAction, error) {
	var a types.RunnerPendingAction
	var kind string
	err := row.Scan(&a.ID, &a.RunnerID, &a.RunID, &kind, &a.Ref, &a.QueuedAt, &a.AppliedAt, &a.Outcome, &a.ObservedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.RunnerPendingAction{}, ErrNotFound
	}
	if err != nil {
		return types.RunnerPendingAction{}, fmt.Errorf("store: scan runner action: %w", err)
	}
	a.Kind = types.RunnerActionKind(kind)
	return a, nil
}

// QueueRunnerAction queues a for a claimed runner, idempotently per (run, kind): when an unapplied one
// exists it is returned with queued=false instead of a second row. A runner that is not claimed (revoked,
// unclaimed, unknown) takes none, so no row can exist that nothing will ever apply: ErrNotFound. The runner row is locked FOR SHARE, so a queue
// that meets a revoke in flight waits for it and then sees the runner revoked.
func (s PG) QueueRunnerAction(ctx context.Context, a types.RunnerPendingAction) (types.RunnerPendingAction, bool, error) {
	const ins = `
		INSERT INTO runner_pending_actions (id, runner_id, run_id, kind, ref)
		SELECT $1,$2,$3,$4,$5 WHERE EXISTS (SELECT 1 FROM runners WHERE id = $2 AND state = 'claimed' FOR SHARE)
		ON CONFLICT (run_id, kind) WHERE applied_at IS NULL DO NOTHING
		RETURNING ` + actionCols
	out, err := scanAction(s.Pool.QueryRow(ctx, ins, a.ID, a.RunnerID, a.RunID, string(a.Kind), a.Ref))
	if err == nil {
		return out, true, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return types.RunnerPendingAction{}, false, err
	}
	const sel = `SELECT ` + actionCols + ` FROM runner_pending_actions WHERE run_id = $1 AND kind = $2 AND applied_at IS NULL`
	out, err = scanAction(s.Pool.QueryRow(ctx, sel, a.RunID, string(a.Kind)))
	return out, false, err
}

// ListPendingRunnerActions is the runner's unapplied actions, oldest first: the PENDING payload.
func (s PG) ListPendingRunnerActions(ctx context.Context, runnerID uuid.UUID) ([]types.RunnerPendingAction, error) {
	const q = `SELECT ` + actionCols + ` FROM runner_pending_actions WHERE runner_id = $1 AND applied_at IS NULL ORDER BY queued_at, id`
	return collect(ctx, s.Pool, "list", "runner pending actions", q, []any{runnerID}, scanAction)
}

// RunHasPendingRunnerAction is the fence: a run with an unapplied action gets no relay.
func (s PG) RunHasPendingRunnerAction(ctx context.Context, runID uuid.UUID) (bool, error) {
	var has bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM runner_pending_actions WHERE run_id = $1 AND applied_at IS NULL)`, runID).Scan(&has)
	if err != nil {
		return false, fmt.Errorf("store: check pending runner actions: %w", err)
	}
	return has, nil
}

// ApplyRunnerAction records the runner's action_result. An action already applied, or unknown, is
// ErrNotFound: a replayed result changes nothing.
func (s PG) ApplyRunnerAction(ctx context.Context, id uuid.UUID, outcome string, observedAt, now time.Time) (types.RunnerPendingAction, error) {
	const q = `
		UPDATE runner_pending_actions SET applied_at = $4, outcome = $2, observed_at = $3
		WHERE id = $1 AND applied_at IS NULL
		RETURNING ` + actionCols
	return scanAction(s.Pool.QueryRow(ctx, q, id, outcome, observedAt, now))
}

// GetCredentialDeliveryPolicy reads the local_credential_delivery document: an empty one (every class
// refuses) when none was written.
func (s PG) GetCredentialDeliveryPolicy(ctx context.Context) (types.CredentialDeliveryPolicyDoc, error) {
	var d types.CredentialDeliveryPolicyDoc
	var policy []byte
	err := s.Pool.QueryRow(ctx, `SELECT policy, updated_at, updated_by FROM credential_delivery_policy WHERE singleton`).Scan(&policy, &d.UpdatedAt, &d.UpdatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.CredentialDeliveryPolicyDoc{Policy: json.RawMessage(`{"classes":{}}`)}, nil
	}
	if err != nil {
		return types.CredentialDeliveryPolicyDoc{}, fmt.Errorf("store: get credential delivery policy: %w", err)
	}
	d.Policy = policy
	return d, nil
}

// PutCredentialDeliveryPolicy upserts the document. The caller has validated it (placement.DeliveryPolicy.Validate).
func (s PG) PutCredentialDeliveryPolicy(ctx context.Context, policy json.RawMessage, by string, now time.Time) (types.CredentialDeliveryPolicyDoc, error) {
	const q = `
		INSERT INTO credential_delivery_policy (singleton, policy, updated_at, updated_by)
		VALUES (true, $1, $2, $3)
		ON CONFLICT (singleton) DO UPDATE SET policy = EXCLUDED.policy, updated_at = EXCLUDED.updated_at, updated_by = EXCLUDED.updated_by
		RETURNING policy, updated_at, updated_by`
	var d types.CredentialDeliveryPolicyDoc
	var out []byte
	if err := s.Pool.QueryRow(ctx, q, []byte(policy), now, by).Scan(&out, &d.UpdatedAt, &d.UpdatedBy); err != nil {
		return types.CredentialDeliveryPolicyDoc{}, fmt.Errorf("store: put credential delivery policy: %w", err)
	}
	d.Policy = out
	return d, nil
}
