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

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RunnerStore is the optional store capability behind client mode, optional like DeviceStore: a fake
// or a non-Postgres store implements it or the runner routes answer 501.
type RunnerStore interface {
	CreateRunner(ctx context.Context, r types.Runner) (types.Runner, error)
	GetRunner(ctx context.Context, id uuid.UUID) (types.Runner, error)
	ListRunners(ctx context.Context) ([]types.Runner, error)
	ListRunnersByOwner(ctx context.Context, owner string) ([]types.Runner, error)
	ListVisibleRunnersByOwner(ctx context.Context, owner string, now time.Time, revokedShown time.Duration, p Page) ([]types.Runner, error)
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
	owner_kind, trust_class, trust_set_by, trust_set_at, registration_token_id,
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
		&r.MintedBy, &r.RevokedBy, &r.RevokeReason, &r.MaxRuns, &r.MachineName, &r.Arch,
		&r.OwnerKind, &r.TrustClass, &r.TrustSetBy, &r.TrustSetAt, &r.RegistrationTokenID}, lim.targets()...)...)
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
// r.MaxRuns 0 is types.RunnerMaxRunsDefault. r.OwnerKind is the redeemed token's (empty is person); the
// trust class on r is ignored: a person's runner starts personal and an org runner org_cluster, the only
// class the coupling CHECK allows it.
func (s PG) CreateRunner(ctx context.Context, r types.Runner) (types.Runner, error) {
	const q = `
		INSERT INTO runners (id, owner, name, public_key, key_fingerprint, version, org_url_sha256, minted_by, max_runs, machine_name, arch,
			owner_kind, trust_class, registration_token_id, ` + runnerLimitCols + `)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
		RETURNING ` + runnerCols
	maxRuns := cmp.Or(r.MaxRuns, types.RunnerMaxRunsDefault)
	if r.Limits != nil {
		maxRuns = r.Limits.RunsAtOnce
	}
	kind, trust := types.RunnerOwnerPerson, types.RunnerTrustPersonal
	if r.OwnerKind == types.RunnerOwnerOrg {
		kind, trust = types.RunnerOwnerOrg, types.RunnerTrustOrgCluster
	}
	out, err := scanRunner(s.Pool.QueryRow(ctx, q, append([]any{r.ID, r.Owner, r.Name, r.PublicKey, r.KeyFingerprint, r.Version, r.OrgURLSHA256,
		r.MintedBy, maxRuns, r.MachineName, r.Arch, kind, trust, r.RegistrationTokenID}, limitArgs(r.Limits)...)...))
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

// ListVisibleRunnersByOwner is owner's own list as the console shows it: ListRunnersByOwner without a
// lapsed unclaimed runner (older than types.RunnerUnclaimedTTL at now), and without a revoked one unless
// someone other than owner revoked it within revokedShown. The filter runs in the query, so the history
// the list leaves out is never read. p windows the answer; a Limit of 0 or less is every row.
func (s PG) ListVisibleRunnersByOwner(ctx context.Context, owner string, now time.Time, revokedShown time.Duration, p Page) ([]types.Runner, error) {
	const base = `SELECT ` + runnerCols + ` FROM runners
		WHERE owner = $1
		  AND NOT (state = 'unclaimed' AND created_at <= $2::timestamptz - $3 * interval '1 second')
		  AND (state <> 'revoked' OR (revoked_at IS NOT NULL AND COALESCE(revoked_by, '') <> $1
		       AND revoked_at >= $2::timestamptz - $4 * interval '1 second'))
		ORDER BY created_at DESC, id`
	q, args := p.appendTo(base, []any{owner, now, types.RunnerUnclaimedTTL.Seconds(), revokedShown.Seconds()})
	return collect(ctx, s.Pool, "list", "runners", q, args, scanRunner)
}

// ClaimRunner moves an unclaimed person's runner to claimed in one conditional UPDATE, only for its owner
// and the fingerprint it was registered with. ErrNotFound: no such runner. ErrConflict: it is not
// unclaimed (claimed twice, revoked). ErrRunnerClaimMismatch: wrong owner or fingerprint, or an org runner.
func (s PG) ClaimRunner(ctx context.Context, id uuid.UUID, owner, fingerprint string, now time.Time) (types.Runner, error) {
	const q = `
		UPDATE runners SET state = 'claimed', claimed_at = $4
		WHERE id = $1 AND state = 'unclaimed' AND owner_kind = 'person' AND owner = $2 AND owner <> '' AND key_fingerprint = $3
			AND created_at > $4::timestamptz - interval '24 hours'
		RETURNING ` + runnerCols
	out, err := scanRunner(s.Pool.QueryRow(ctx, q, id, owner, fingerprint, now))
	return out, s.claimMiss(ctx, id, err)
}

// ClaimOrgRunner moves an unclaimed org runner to claimed in one conditional UPDATE: org-owned, the
// fingerprint and organisation URL it registered with, inside its 24 h wait. It stamps trust_class
// org_cluster with by and now as its writer; it verifies the owner kind and never sets it. The errors
// are ClaimRunner's.
func (s PG) ClaimOrgRunner(ctx context.Context, id uuid.UUID, fingerprint, orgHash, by string, now time.Time) (types.Runner, error) {
	const q = `
		UPDATE runners SET state = 'claimed', claimed_at = $5, trust_class = 'org_cluster', trust_set_by = $4, trust_set_at = $5
		WHERE id = $1 AND state = 'unclaimed' AND owner_kind = 'org' AND owner = '' AND key_fingerprint = $2 AND org_url_sha256 = $3
			AND created_at > $5::timestamptz - interval '24 hours'
		RETURNING ` + runnerCols
	out, err := scanRunner(s.Pool.QueryRow(ctx, q, id, fingerprint, orgHash, by, now))
	return out, s.claimMiss(ctx, id, err)
}

// claimMiss turns a claim UPDATE that matched no row into its refusal; any other result passes through.
func (s PG) claimMiss(ctx context.Context, id uuid.UUID, err error) error {
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	cur, err := s.GetRunner(ctx, id)
	if err != nil {
		return err
	}
	if cur.State != types.RunnerUnclaimed {
		return ErrConflict
	}
	return ErrRunnerClaimMismatch
}

// runnerReportsOn reports whether a claimed, unrevoked runner may report on a run bound to it (the run's
// runner_id is the runner's, which the caller checks): a person's runner only on its owner's runs, an
// org runner, whose owner is empty, on any. An empty owner on a person's runner stays refused.
func runnerReportsOn(ownerKind, owner, createdBy string) bool {
	if ownerKind == string(types.RunnerOwnerOrg) {
		return owner == ""
	}
	return owner != "" && owner == createdBy
}

// ErrRunnerTrustRefused is a trust write the row cannot take: the runner is not claimed, or the class
// is one its owner kind cannot hold (the coupling CHECK).
var ErrRunnerTrustRefused = errors.New("store: runner trust class refused")

// GetRunnerForUpdateQ reads a runner and locks its row for the rest of q's transaction.
func GetRunnerForUpdateQ(ctx context.Context, q Querier, id uuid.UUID) (types.Runner, error) {
	return scanRunner(q.QueryRow(ctx, `SELECT `+runnerCols+` FROM runners WHERE id = $1 FOR UPDATE`, id))
}

// SetRunnerTrustClassQ writes a claimed runner's trust class with by and now as its writer, and returns
// the row as it is now. It is the only writer after the claim; the caller holds the row lock
// (GetRunnerForUpdateQ). ErrNotFound: no such runner. ErrRunnerTrustRefused: not claimed, or a class
// its owner kind cannot hold.
func SetRunnerTrustClassQ(ctx context.Context, q Querier, id uuid.UUID, class types.RunnerTrustClass, by string, now time.Time) (types.Runner, error) {
	out, err := scanRunner(q.QueryRow(ctx, `UPDATE runners SET trust_class = $2, trust_set_by = $3, trust_set_at = $4
		WHERE id = $1 AND state = 'claimed' RETURNING `+runnerCols, id, string(class), by, now))
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr) && pgErr.Code == "23514":
		return types.Runner{}, ErrRunnerTrustRefused
	case errors.Is(err, ErrNotFound):
		if _, gerr := scanRunner(q.QueryRow(ctx, `SELECT `+runnerCols+` FROM runners WHERE id = $1`, id)); gerr != nil {
			return types.Runner{}, gerr
		}
		return types.Runner{}, ErrRunnerTrustRefused
	}
	return out, err
}

// SetRunnerTrustClass is the direct trust write: the row locked, then SetRunnerTrustClassQ, in one
// transaction. before is the row as it was.
func (s PG) SetRunnerTrustClass(ctx context.Context, id uuid.UUID, class types.RunnerTrustClass, by string, now time.Time) (before, after types.Runner, err error) {
	err = s.inTx(ctx, func(q Querier) error {
		if before, err = GetRunnerForUpdateQ(ctx, q, id); err != nil {
			return err
		}
		after, err = SetRunnerTrustClassQ(ctx, q, id, class, by, now)
		return err
	})
	return before, after, err
}

// TouchRunner records an authenticated session: last_seen_at and the runner's version; a version of ""
// keeps the one held. Best effort in the caller, like TouchDevice.
func (s PG) TouchRunner(ctx context.Context, id uuid.UUID, version string, now time.Time) error {
	if _, err := s.Pool.Exec(ctx, `UPDATE runners SET last_seen_at = $2, version = COALESCE(NULLIF($3, ''), version) WHERE id = $1`, id, now, version); err != nil {
		return fmt.Errorf("store: touch runner: %w", err)
	}
	return nil
}

// RunnerCapsReport is what one session's caps event reports for the runner row. An empty Version,
// MachineName or Arch keeps the one the row holds.
type RunnerCapsReport struct {
	Version, MachineName, Arch string
	RetireCapable              bool
}

// RecordRunnerCaps stores session's caps report: last_seen_at and the version, the machine name and
// architecture (a revoked runner keeps both) and the retire capability. It writes only while session
// is the runner's current inventory session (BeginRunnerInventory), and says whether it was: a
// superseded session's late caps write nothing over a newer one's. The runner row is locked first,
// the order BeginRunnerInventory takes, so a fresh session's begin waits for this write and a write
// after it sees that session.
func (s PG) RecordRunnerCaps(ctx context.Context, id uuid.UUID, session string, r RunnerCapsReport, now time.Time) (bool, error) {
	var current bool
	err := s.inTx(ctx, func(q Querier) error {
		if _, err := q.Exec(ctx, `SELECT 1 FROM runners WHERE id = $1 FOR NO KEY UPDATE`, id); err != nil {
			return err
		}
		if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM runner_inventories WHERE runner_id = $1 AND session_id::text = $2::text)`,
			id, session).Scan(&current); err != nil || !current {
			return err
		}
		_, err := q.Exec(ctx, `UPDATE runners SET last_seen_at = $2, version = COALESCE(NULLIF($3::text, ''), version),
			machine_name = CASE WHEN $4::text <> '' AND state <> 'revoked' THEN $4::text ELSE machine_name END,
			arch = CASE WHEN $5::text <> '' AND state <> 'revoked' THEN $5::text ELSE arch END,
			retire_capable = $6 WHERE id = $1`, id, now, r.Version, r.MachineName, r.Arch, r.RetireCapable)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("store: record runner caps: %w", err)
	}
	return current, nil
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

// ErrRunnerBusy: a revoke waited revokeLockTimeout for a lock another operation holds on the runner or
// its runs, and revoked nothing. It is a lock refusal (db.LockRefused), answered with a retry.
var ErrRunnerBusy = fmt.Errorf("store: the runner is locked by another operation: %w", db.ErrLockBusy)

// revokeLockTimeout bounds each lock wait of a revoke. The runner transactions that hold its row and
// wait on another lock give up at 2s, so 5s outlasts them and their work; it is also the lock
// refusal's Retry-After.
const revokeLockTimeout = "5s"

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
// is runner, then runs by id, then actions, as everywhere else. A lock held past
// revokeLockTimeout is ErrRunnerBusy, and nothing is revoked.
func (s PG) RevokeRunner(ctx context.Context, id uuid.UUID, now time.Time, by, reason string) (_ RunnerRevocation, err error) {
	defer func() {
		if pgErr := (*pgconn.PgError)(nil); errors.As(err, &pgErr) && pgErr.Code == "55P03" {
			err = fmt.Errorf("%w: %w", ErrRunnerBusy, err)
		}
	}()
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return RunnerRevocation{}, fmt.Errorf("store: begin revoke runner: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // a no-op after Commit
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '`+revokeLockTimeout+`'`); err != nil {
		return RunnerRevocation{}, fmt.Errorf("store: revoke runner lock timeout: %w", err)
	}
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
