// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Registered runners and the credential delivery policy (migration 0139); pending actions are in
// store_runner_actions.go.
// Registration tokens and the routes that mint and consume them are a later lane's; this file is the
// rows the stream, the claim and the pending-action queue read and write.
package store

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	RevokeRunner(ctx context.Context, id uuid.UUID, now time.Time, by, reason string) (RunnerRevocation, error)
	ExpireUnclaimedRunners(ctx context.Context, createdBefore time.Time) (int, error)

	QueueRunnerAction(ctx context.Context, a types.RunnerPendingAction) (action types.RunnerPendingAction, queued bool, err error)
	ListPendingRunnerActions(ctx context.Context, runnerID uuid.UUID) (page []types.RunnerPendingAction, more bool, err error)
	RunHasPendingRunnerAction(ctx context.Context, runID uuid.UUID) (bool, error)
	RecordRunnerActionResult(ctx context.Context, runnerID, runID, actionID uuid.UUID, outcome, reason string, observedAt time.Time, prepare func(context.Context, types.AuditEvent) (types.AuditEvent, error)) (action types.RunnerPendingAction, first bool, err error)

	GetCredentialDeliveryPolicy(ctx context.Context) (types.CredentialDeliveryPolicyDoc, error)
	PutCredentialDeliveryPolicy(ctx context.Context, policy json.RawMessage, by string, now time.Time) (types.CredentialDeliveryPolicyDoc, error)
}

var _ RunnerStore = PG{}

// ErrRunnerClaimMismatch is ClaimRunner's refusal when the runner is unclaimed but the caller is not
// its owner or the fingerprint differs: the API answers runner_claim_mismatch.
var ErrRunnerClaimMismatch = errors.New("store: runner claim does not match the owner or key fingerprint")

const runnerCols = `id, owner, name, public_key, key_fingerprint, state, version, created_at, claimed_at,
	last_seen_at, revoked_at, posture, posture_reported_at, posture_source, org_url_sha256, minted_by, revoked_by, revoke_reason, max_runs, machine_name, arch,
	` + runnerLimitCols

// prefixedRunnerCols is runnerCols on a query that names the runners table r.
var prefixedRunnerCols = "r." + strings.ReplaceAll(strings.Join(strings.Fields(runnerCols), " "), ", ", ", r.")

func scanRunner(row pgx.Row) (types.Runner, error) {
	var r types.Runner
	var state string
	var posture []byte
	var lim limitCols
	err := row.Scan(append([]any{&r.ID, &r.Owner, &r.Name, &r.PublicKey, &r.KeyFingerprint, &state, &r.Version, &r.CreatedAt,
		&r.ClaimedAt, &r.LastSeenAt, &r.RevokedAt, &posture, &r.PostureReportedAt, &r.PostureSource, &r.OrgURLSHA256,
		&r.MintedBy, &r.RevokedBy, &r.RevokeReason, &r.MaxRuns, &r.MachineName, &r.Arch}, lim.targets()...)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.Runner{}, ErrNotFound
	}
	if err != nil {
		return types.Runner{}, fmt.Errorf("store: scan runner: %w", err)
	}
	r.State = types.RunnerState(state)
	r.Limits = lim.limits(&r.MaxRuns)
	if err := json.Unmarshal(posture, &r.Posture); err != nil {
		return types.Runner{}, fmt.Errorf("store: unmarshal runner posture: %w", err)
	}
	return r, nil
}

// CreateRunner inserts an unclaimed runner: state, claim and posture on r are ignored, since every
// registration starts unclaimed. ErrConflict on a key fingerprint already registered. r.Limits, when
// set, is the runner's limit set and its Runs at once; without it the runner declares no limits and
// r.MaxRuns 0 is types.RunnerMaxRunsDefault.
func (s PG) CreateRunner(ctx context.Context, r types.Runner) (types.Runner, error) {
	const q = `
		INSERT INTO runners (id, owner, name, public_key, key_fingerprint, version, org_url_sha256, minted_by, max_runs, machine_name, arch,
			` + runnerLimitCols + `)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
		RETURNING ` + runnerCols
	maxRuns := cmp.Or(r.MaxRuns, types.RunnerMaxRunsDefault)
	if r.Limits != nil {
		maxRuns = r.Limits.RunsAtOnce
	}
	out, err := scanRunner(s.Pool.QueryRow(ctx, q, append([]any{r.ID, r.Owner, r.Name, r.PublicKey, r.KeyFingerprint, r.Version, r.OrgURLSHA256,
		r.MintedBy, maxRuns, r.MachineName, r.Arch}, limitArgs(r.Limits)...)...))
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

// RunnerRevocation is what one committed revoke did: the revoked runner and
// the runs it ended KILLED (RN-Q29), each with the state it was killed from.
type RunnerRevocation struct {
	Runner types.Runner
	Killed []RunnerKilledRun
}

// RunnerKilledRun is one run a runner revocation ended KILLED.
type RunnerKilledRun struct {
	ID   uuid.UUID
	From types.RunState
}

// EraseRunnerAttributionOf clears person as the revoker of every runner and as the minter of every
// registration token minted for someone else, and returns how many rows that changed. A token a person
// minted for themselves keeps minted_by = owner: that equality is how a self-minted runner is told apart
// from one an admin minted. Idempotent: a row already cleared matches nothing.
func (s PG) EraseRunnerAttributionOf(ctx context.Context, person string) (int, error) {
	if person == "" {
		return 0, errors.New("store: erase runner attribution: a person is required")
	}
	n := 0
	for _, stmt := range []string{
		`UPDATE runners SET revoked_by = '' WHERE revoked_by = $1`,
		`UPDATE runner_registration_tokens SET minted_by = '' WHERE minted_by = $1 AND owner <> $1`,
	} {
		tag, err := s.Pool.Exec(ctx, stmt, person)
		if err != nil {
			return 0, fmt.Errorf("store: erase runner attribution: %w", err)
		}
		n += int(tag.RowsAffected())
	}
	return n, nil
}

// RevokeRunner revokes a runner in one transaction: the runner row with who
// revoked it and why, every run holding a slot on it ended KILLED, and every
// unsettled pending action of it settled 'revoked'. A QUEUED run holds no slot
// and is not ended here: the caller ends the ones waiting for this runner alone. Rows are kept as evidence. The fence lifts only
// because the run is terminal in the same commit, never before. Already
// revoked or unknown is ErrNotFound, so a second call ends nothing. Lock order
// is runner, then runs by id, then actions, as everywhere else.
func (s PG) RevokeRunner(ctx context.Context, id uuid.UUID, now time.Time, by, reason string) (RunnerRevocation, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return RunnerRevocation{}, fmt.Errorf("store: begin revoke runner: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // a no-op after Commit
	const q = `UPDATE runners SET state = 'revoked', revoked_at = $2, revoked_by = $3, revoke_reason = $4
		WHERE id = $1 AND state <> 'revoked' RETURNING ` + runnerCols
	out := RunnerRevocation{}
	if out.Runner, err = scanRunner(tx.QueryRow(ctx, q, id, now, by, reason)); err != nil {
		return RunnerRevocation{}, err
	}
	out.Killed, err = collect(ctx, tx, "kill", "revoked runner's runs", `WITH live AS (
			SELECT id, state FROM agent_runs WHERE runner_id = $1 AND state = ANY($3) ORDER BY id FOR UPDATE)
		UPDATE agent_runs a SET state = $2, updated_at = now(), ended_at = now() FROM live
		WHERE a.id = live.id RETURNING a.id, live.state`, []any{id, string(types.RunKilled), slotHoldingStateNames()},
		func(row pgx.Row) (RunnerKilledRun, error) {
			var k RunnerKilledRun
			err := row.Scan(&k.ID, &k.From)
			return k, err
		})
	if err != nil {
		return RunnerRevocation{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE runner_pending_actions SET settled_at = clock_timestamp(), settlement = 'revoked'
		WHERE runner_id = $1 AND binding_version = 1 AND settled_at IS NULL AND (applied_at IS NULL OR outcome = 'deferred')`, id); err != nil {
		return RunnerRevocation{}, fmt.Errorf("store: settle revoked runner's pending actions: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return RunnerRevocation{}, fmt.Errorf("store: commit revoke runner: %w", err)
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

// GetCredentialDeliveryPolicy reads the local_credential_delivery document: an empty one (every class
// refuses) when none was written.
func (s PG) GetCredentialDeliveryPolicy(ctx context.Context) (types.CredentialDeliveryPolicyDoc, error) {
	return GetCredentialDeliveryPolicyQ(ctx, s.Pool)
}

// GetCredentialDeliveryPolicyQ is GetCredentialDeliveryPolicy on q (a governance decision's transaction).
func GetCredentialDeliveryPolicyQ(ctx context.Context, q Querier) (types.CredentialDeliveryPolicyDoc, error) {
	var d types.CredentialDeliveryPolicyDoc
	var policy []byte
	err := q.QueryRow(ctx, `SELECT policy, updated_at, updated_by FROM credential_delivery_policy WHERE singleton`).Scan(&policy, &d.UpdatedAt, &d.UpdatedBy)
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
	return PutCredentialDeliveryPolicyQ(ctx, s.Pool, policy, by, now)
}

// PutCredentialDeliveryPolicyQ is PutCredentialDeliveryPolicy on q.
func PutCredentialDeliveryPolicyQ(ctx context.Context, q Querier, policy json.RawMessage, by string, now time.Time) (types.CredentialDeliveryPolicyDoc, error) {
	const sql = `
		INSERT INTO credential_delivery_policy (singleton, policy, updated_at, updated_by)
		VALUES (true, $1, $2, $3)
		ON CONFLICT (singleton) DO UPDATE SET policy = EXCLUDED.policy, updated_at = EXCLUDED.updated_at, updated_by = EXCLUDED.updated_by
		RETURNING policy, updated_at, updated_by`
	var d types.CredentialDeliveryPolicyDoc
	var out []byte
	if err := q.QueryRow(ctx, sql, []byte(policy), now, by).Scan(&out, &d.UpdatedAt, &d.UpdatedBy); err != nil {
		return types.CredentialDeliveryPolicyDoc{}, fmt.Errorf("store: put credential delivery policy: %w", err)
	}
	d.Policy = out
	return d, nil
}
