// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Hybrid enrolment and audit federation (migration 0066): the organisation-
// side device inventory (devices, device_enrolment_tokens) and the laptop
// side's durable forwarder cursor (org_federation). Kept out of store.go on
// purpose — it is already at a lint size boundary — mirroring store_apitokens.go
// / store_sshkeys.go's split.
//
// hashToken (store_ephemeral.go) is reused for both credential tables here:
// the raw device bearer and the raw enrolment token never reach a row, only
// hex(sha256(raw)).
package store

import (
	"bytes"
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

// DeviceStore is the OPTIONAL store capability behind hybrid enrolment and
// audit federation (docs/design/0.8/PLAN.md, epic #78). Optional for the same
// reason Pager and AuditChainVerifier are: a test fake or a future non-
// Postgres store implements neither, and the device routes that will mount
// this (a later issue — this one is storage only) must type-assert and answer
// 501/401 rather than silently no-op. That is the FAIL-CLOSED half of the
// contract this interface exists to make possible: nothing here has a default
// no-op implementation on Store, so a caller that skips the type assertion is
// a compile error, and a caller that performs it and finds no match has an
// explicit absent branch to fail into.
//
// GetDeviceByRaw's WHERE carries `revoked_at IS NULL` (see its own doc
// comment) so a revoked device credential is not an oracle. IngestDeviceAudit
// is the delicate one — see its own doc comment for the verification and
// idempotency contract.
type DeviceStore interface {
	MintEnrolmentToken(ctx context.Context, token string, t types.DeviceEnrolmentToken) (types.DeviceEnrolmentToken, error)
	ConsumeEnrolmentToken(ctx context.Context, token string, now time.Time) (types.DeviceEnrolmentToken, bool, error)
	ListEnrolmentTokens(ctx context.Context, now time.Time) ([]types.DeviceEnrolmentToken, error)
	RevokeEnrolmentToken(ctx context.Context, id uuid.UUID, now time.Time) (types.DeviceEnrolmentToken, error)
	CreateDevice(ctx context.Context, d types.Device, raw string) (types.Device, error)
	GetDeviceByRaw(ctx context.Context, raw string) (types.Device, error)
	TouchDevice(ctx context.Context, id uuid.UUID, now time.Time) error
	ListDevices(ctx context.Context) ([]types.Device, error)
	RevokeDevice(ctx context.Context, id uuid.UUID, now time.Time) (types.Device, error)
	IngestDeviceAudit(ctx context.Context, deviceID uuid.UUID, peer string, rows []types.FederatedAuditEvent) (DeviceIngestResult, error)
	ListAuditEventsAfterSeq(ctx context.Context, seq int64, limit int) ([]types.FederatedAuditEvent, error)
	GetFederationCursor(ctx context.Context) (int64, error)
	SetFederationCursor(ctx context.Context, seq int64) error
}

// Compile-time assertion: PG satisfies DeviceStore.
var _ DeviceStore = PG{}

const deviceCols = `id, name, credential_sha256, enrolled_by, created_at, last_seen_at, revoked_at, last_seq, last_row_hash`

func scanDevice(row pgx.Row) (types.Device, error) {
	var d types.Device
	err := row.Scan(&d.ID, &d.Name, &d.CredentialSHA256, &d.EnrolledBy, &d.CreatedAt,
		&d.LastSeenAt, &d.RevokedAt, &d.LastSeq, &d.LastRowHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.Device{}, ErrNotFound
	}
	if err != nil {
		return types.Device{}, fmt.Errorf("store: scan device: %w", err)
	}
	return d, nil
}

const enrolmentTokenCols = `id, token_sha256, device_name, minted_by, created_at, expires_at, consumed_at`

func scanEnrolmentToken(row pgx.Row) (types.DeviceEnrolmentToken, error) {
	var t types.DeviceEnrolmentToken
	err := row.Scan(&t.ID, &t.TokenSHA256, &t.DeviceName, &t.MintedBy, &t.CreatedAt, &t.ExpiresAt, &t.ConsumedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.DeviceEnrolmentToken{}, ErrNotFound
	}
	if err != nil {
		return types.DeviceEnrolmentToken{}, fmt.Errorf("store: scan device enrolment token: %w", err)
	}
	return t, nil
}

// MintEnrolmentToken inserts one single-use enrolment token. ErrConflict on a
// unique_violation (23505) against token_sha256 — a raw collision in a
// 256-bit random space, meaning the caller reused a token value rather than
// minting a fresh one, the same reading CreateAPIToken gives its own
// token_sha256 collision.
func (s PG) MintEnrolmentToken(ctx context.Context, token string, t types.DeviceEnrolmentToken) (types.DeviceEnrolmentToken, error) {
	const q = `
		INSERT INTO device_enrolment_tokens (id, token_sha256, device_name, minted_by, expires_at)
		VALUES ($1,$2,$3,$4,$5)
		RETURNING ` + enrolmentTokenCols
	out, err := scanEnrolmentToken(s.Pool.QueryRow(ctx, q, t.ID, hashToken(token), t.DeviceName, t.MintedBy, t.ExpiresAt))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return types.DeviceEnrolmentToken{}, ErrConflict
		}
		return types.DeviceEnrolmentToken{}, err
	}
	return out, nil
}

// ConsumeEnrolmentToken redeems token exactly once: a single-row conditional
// UPDATE ... RETURNING, the same atomic consume-once shape
// ConsumeAttachTicket uses (store_ephemeral.go), adapted to an UPDATE because
// the row — who minted it, when, for which claimed device name — is worth
// keeping after redemption, for the device row CreateDevice creates from it.
// Two racing redemptions can only have one return a row, since the WHERE
// requires consumed_at IS NULL. An unknown token, an already-consumed token
// and an expired token all answer (zero value, false, nil) alike — nothing
// here distinguishes them, matching ConsumeAttachTicket's own no-oracle
// contract.
func (s PG) ConsumeEnrolmentToken(ctx context.Context, token string, now time.Time) (types.DeviceEnrolmentToken, bool, error) {
	const q = `
		UPDATE device_enrolment_tokens
		SET consumed_at = $2
		WHERE token_sha256 = $1 AND consumed_at IS NULL AND expires_at > $2
		RETURNING ` + enrolmentTokenCols
	t, err := scanEnrolmentToken(s.Pool.QueryRow(ctx, q, hashToken(token), now))
	if errors.Is(err, ErrNotFound) {
		return types.DeviceEnrolmentToken{}, false, nil
	}
	if err != nil {
		return types.DeviceEnrolmentToken{}, false, err
	}
	return t, true, nil
}

// ListEnrolmentTokens returns every token still redeemable at now — unconsumed
// and unexpired — newest first: the tokens an admin may still need to cancel.
// A redeemed token is in the device inventory instead, and an expired one can
// no longer be redeemed, so neither is listed.
func (s PG) ListEnrolmentTokens(ctx context.Context, now time.Time) ([]types.DeviceEnrolmentToken, error) {
	const q = `SELECT ` + enrolmentTokenCols + ` FROM device_enrolment_tokens
		WHERE consumed_at IS NULL AND expires_at > $1 ORDER BY created_at DESC`
	return collect(ctx, s.Pool, "list", "device enrolment tokens", q, []any{now}, scanEnrolmentToken)
}

// RevokeEnrolmentToken cancels a token that is still redeemable by setting its
// consumed_at, the column ConsumeEnrolmentToken's WHERE requires to be NULL, so
// a revoke and a racing redemption cannot both win. A token already redeemed,
// already revoked, expired or unknown is ErrNotFound, and the caller writes no
// audit row for a revoke that did not happen.
func (s PG) RevokeEnrolmentToken(ctx context.Context, id uuid.UUID, now time.Time) (types.DeviceEnrolmentToken, error) {
	const q = `
		UPDATE device_enrolment_tokens SET consumed_at = $2
		WHERE id = $1 AND consumed_at IS NULL AND expires_at > $2
		RETURNING ` + enrolmentTokenCols
	return scanEnrolmentToken(s.Pool.QueryRow(ctx, q, id, now))
}

// CreateDevice inserts one device row, following CreateAPIToken's shape: raw
// is the PLAINTEXT device credential minted at enrolment (first-boot token
// exchange); only its hash is stored, and d.CredentialSHA256 is ignored —
// passing the secret twice would be the one way to accidentally persist it.
// The caller returns raw to the device exactly once; it is unrecoverable
// afterwards. ErrConflict on a credential_sha256 collision, the same 256-bit
// random-space reading MintEnrolmentToken's own collision gets.
func (s PG) CreateDevice(ctx context.Context, d types.Device, raw string) (types.Device, error) {
	const q = `
		INSERT INTO devices (id, name, credential_sha256, enrolled_by)
		VALUES ($1,$2,$3,$4)
		RETURNING ` + deviceCols
	out, err := scanDevice(s.Pool.QueryRow(ctx, q, d.ID, d.Name, hashToken(raw), d.EnrolledBy))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return types.Device{}, ErrConflict
		}
		return types.Device{}, err
	}
	return out, nil
}

// GetDeviceByRaw is the device auth-time lookup: given the bearer a device
// presented, resolve the LIVE row it stands for. `revoked_at IS NULL` is in
// the WHERE, not checked by the caller — the same shape GetAPITokenByRaw
// uses — so a revoked credential, an unknown one and a mismatched hash all
// fail IDENTICALLY with ErrNotFound: the boundary is not an oracle for "this
// device was once enrolled".
func (s PG) GetDeviceByRaw(ctx context.Context, raw string) (types.Device, error) {
	const q = `SELECT ` + deviceCols + ` FROM devices WHERE credential_sha256 = $1 AND revoked_at IS NULL`
	return scanDevice(s.Pool.QueryRow(ctx, q, hashToken(raw)))
}

// TouchDevice records that id was just used. Best effort, like TouchAPIToken:
// a caller on the request path must never fail an otherwise-valid request
// because a keepalive write lost a race or hit a revoked/unknown id.
func (s PG) TouchDevice(ctx context.Context, id uuid.UUID, now time.Time) error {
	if _, err := s.Pool.Exec(ctx, `UPDATE devices SET last_seen_at = $2 WHERE id = $1`, id, now); err != nil {
		return fmt.Errorf("store: touch device: %w", err)
	}
	return nil
}

// ListDevices returns every enrolled device, newest first — revoked rows
// included, the same reasoning ListAPITokens gives: an admin needs to see
// that a retired device is in fact retired, and a revoked row carries no
// usable credential either way. Phase one has no console surface for this
// (docs/design/0.8/PLAN.md: "no console device inventory in phase one — CLI
// and API only"); this is the read those future callers use.
func (s PG) ListDevices(ctx context.Context) ([]types.Device, error) {
	const q = `SELECT ` + deviceCols + ` FROM devices ORDER BY created_at DESC`
	return collect(ctx, s.Pool, "list", "devices", q, nil, scanDevice)
}

// RevokeDevice marks id revoked. Already-revoked is ErrNotFound too
// (`revoked_at IS NULL` in the WHERE) — revoke is idempotent in effect, and a
// second call must not emit a second revoke audit row for an act that did not
// happen, matching RevokeAPIToken.
func (s PG) RevokeDevice(ctx context.Context, id uuid.UUID, now time.Time) (types.Device, error) {
	const q = `
		UPDATE devices SET revoked_at = $2
		WHERE id = $1 AND revoked_at IS NULL
		RETURNING ` + deviceCols
	return scanDevice(s.Pool.QueryRow(ctx, q, id, now))
}

// DeviceIngestResult reports what one IngestDeviceAudit call did. Accepted is
// how many of the submitted rows were newly appended — 0 on any refusal,
// since the whole batch is one transaction, and also 0 when every row was at
// or before the device's already-recorded cursor (an idempotent retry:
// nothing new to do, not a refusal). Reset is true when the accepted rows
// began with a genesis (empty-PrevHash) row while the device already had a
// recorded chain — the documented "device chain reset" case (a purge on the
// device's own local table), which is accepted rather than refused.
type DeviceIngestResult struct {
	Accepted int
	Reset    bool
}

// ErrDeviceRevoked is IngestDeviceAudit's answer for a device revoked after
// its request was authenticated. It is distinct from ErrConflict so the caller
// answers it as the revocation it is — the 401 the device's next request gets
// anyway — and never records it as a broken chain.
var ErrDeviceRevoked = errors.New("store: device is revoked")

// ErrFederatedOrgRun refuses a batch in which a row's run_id names one of THIS
// organisation's own runs. Phase one federates audit rows, never run records,
// so a laptop's row cannot legitimately belong to an org run; accepting one
// would file a device's claim in that run's evidence trail.
var ErrFederatedOrgRun = errors.New("store: federated row names an organisation run")

// ErrFederatedRowInvalid refuses a batch holding a row this organisation cannot
// store so that the device's claim re-checks from the stored row
// (FederatedRowProblem), or a claimed value Postgres cannot represent (an
// SQLSTATE class 22 data exception from the recompute — a \u0000 escape, a NUL
// in text). The device's fault, never the store's: the caller answers 4xx so
// the forwarder stops rather than retrying a 5xx forever.
var ErrFederatedRowInvalid = errors.New("store: federated row cannot be stored as claimed")

// FederatedRowProblem says why r cannot be stored so that its claim re-checks
// from the stored row, or "" when it can. data must be a JSON object without a
// top-level device_origin key (that key is this organisation's marker, and
// overwriting a claimed one would lose what the device signed), JSON null, or
// absent; target must be one CapAuditTarget leaves unchanged, which every row a
// laptop stored is, since the cap is applied at its own insert; device_id is
// this organisation's to derive, never the device's to claim. Exported so the
// API refuses these with a 400 before any database work.
func FederatedRowProblem(r types.FederatedAuditEvent) string {
	if r.DeviceID != nil {
		return "device_id is set by this organisation, not claimed"
	}
	if CapAuditTarget(r.Target) != r.Target {
		return fmt.Sprintf("target exceeds %d bytes", MaxAuditTargetLen)
	}
	if len(r.Data) == 0 || isJSONNull(r.Data) {
		return ""
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(r.Data, &m) != nil {
		return "data must be a JSON object or null"
	}
	if _, claimed := m["device_origin"]; claimed {
		return "data carries a device_origin key"
	}
	return ""
}

func isJSONNull(data json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(data), []byte("null"))
}

// IngestDeviceAudit appends one device's forwarded batch to THIS
// organisation's own audit_events, as the organisation's own chained rows —
// "one chain per writer" (docs/design/0.8/PLAN.md). A federated row keeps the
// device's CLAIMED actor_type, action, target and outcome; what marks it as
// forwarded is the device's provenance folded into `data` (mergeDeviceOrigin),
// not a new audit action, so no existing filter or SIEM rule keyed on `action`
// has to learn about it. Three fields are never the device's to choose: actor
// is FederatedActor — the claimed principal behind this device's own prefix,
// so a laptop cannot write a row that reads as an organisation admin's — with
// the claim kept in data.device_origin.actor; source_ip is peer — the address
// this organisation saw the push arrive from — with the claimed value kept in
// data.device_origin.source_ip; and a run_id naming one of this
// organisation's runs refuses the batch (ErrFederatedOrgRun).
//
// Order of work, chosen so a device can make the organisation's own audit
// writers wait for at most its inserts:
//
//  0. Every row must be storable so its claim re-checks (FederatedRowProblem),
//     and every claimed value must be one Postgres can represent; otherwise
//     ErrFederatedRowInvalid.
//  1. Every claimed RowHash is recomputed BEFORE any transaction or lock, in
//     ONE statement (verifyClaimedHashes). Each row is hashed against its OWN
//     claimed PrevHash, so the check needs no cursor; step 4 is what ties the
//     claims to each other and to the recorded head. A refused batch — and a
//     replayed one — therefore never touches the chain lock.
//  2. The device row is locked FOR UPDATE, which serializes this device's
//     concurrent pushes against each other before either reaches the chain
//     lock. Deadlock-free: no transaction takes the chain lock and then a
//     device row. A revoked device answers ErrDeviceRevoked.
//  3. Idempotency rides the cursor, not audit_events.id: rows at or before the
//     recorded LastSeq are skipped (the forwarder re-sends from its own durable
//     cursor, which can lag what was committed here).
//  4. The first NEW row's claimed PrevHash is either empty (a genesis row,
//     accepted, and a chain reset when a chain was already recorded) or equal
//     to the recorded LastRowHash; every later row's claimed PrevHash equals
//     the preceding row's claimed RowHash. Otherwise ErrConflict.
//  5. Only then the chain lock (db.AuditChainLockKey via lockAuditChainSQL),
//     under the lock timeout every lock wait in this transaction obeys, and
//     the inserts: field for field as claimed, except actor, source_ip and
//     the merged data, with target through CapAuditTarget like every writer.
//     The cursor advances in the same transaction.
//
// Any refusal refuses the ENTIRE batch rather than a verified prefix: a
// batch is the unit the caller retries, and a partial accept would leave the
// cursor mid-batch with no way to tell the caller which rows to resend.
func (s PG) IngestDeviceAudit(ctx context.Context, deviceID uuid.UUID, peer string, rows []types.FederatedAuditEvent) (DeviceIngestResult, error) {
	if len(rows) == 0 {
		return DeviceIngestResult{}, nil
	}
	for _, r := range rows {
		if p := FederatedRowProblem(r); p != "" {
			return DeviceIngestResult{}, fmt.Errorf("store: federated row seq %d: %s: %w", r.Seq, p, ErrFederatedRowInvalid)
		}
	}
	if err := s.verifyClaimedHashes(ctx, rows); err != nil {
		return DeviceIngestResult{}, err
	}

	// Pinned READ COMMITTED like every other audit_events writer: at
	// REPEATABLE READ this transaction would chain onto a stale head and the
	// verify sweep would latch a permanent false tamper verdict.
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return DeviceIngestResult{}, fmt.Errorf("store: begin device audit ingest tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // best-effort on the failure path

	// Bound every lock wait in this transaction — the device row's as well as
	// the chain's. A timeout is not a lost event: the forwarder retries from
	// its own durable cursor.
	if _, err := tx.Exec(ctx, db.AuditChainLockTimeoutSQL()); err != nil {
		return DeviceIngestResult{}, fmt.Errorf("store: bound device ingest lock waits: %w", err)
	}
	var lastSeq int64
	var lastRowHash string
	var revokedAt *time.Time
	err = tx.QueryRow(ctx, `SELECT last_seq, last_row_hash, revoked_at FROM devices WHERE id = $1 FOR UPDATE`, deviceID).
		Scan(&lastSeq, &lastRowHash, &revokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return DeviceIngestResult{}, ErrNotFound
	}
	if err != nil {
		return DeviceIngestResult{}, fmt.Errorf("store: read device cursor: %w", err)
	}
	if revokedAt != nil {
		return DeviceIngestResult{}, ErrDeviceRevoked
	}
	if err := refuseOrgRuns(ctx, tx, rows); err != nil {
		return DeviceIngestResult{}, err
	}

	start := 0
	for start < len(rows) && rows[start].Seq <= lastSeq {
		start++
	}
	if start == len(rows) {
		// Every row was already ingested: an idempotent retry, not a refusal.
		if err := tx.Commit(ctx); err != nil {
			return DeviceIngestResult{}, fmt.Errorf("store: commit device audit ingest: %w", err)
		}
		return DeviceIngestResult{}, nil
	}
	toIngest := rows[start:]
	reset := false
	if toIngest[0].PrevHash == "" {
		reset = lastRowHash != ""
	} else if toIngest[0].PrevHash != lastRowHash {
		return DeviceIngestResult{}, fmt.Errorf(
			"store: ingest device audit: row seq %d claims prev_hash %q, device's recorded head is %q: %w",
			toIngest[0].Seq, toIngest[0].PrevHash, lastRowHash, ErrConflict)
	}
	for i := 1; i < len(toIngest); i++ {
		if toIngest[i].PrevHash != toIngest[i-1].RowHash {
			return DeviceIngestResult{}, fmt.Errorf(
				"store: ingest device audit: row seq %d does not chain to the previous row in this batch: %w",
				toIngest[i].Seq, ErrConflict)
		}
	}

	if _, err := tx.Exec(ctx, lockAuditChainSQL, db.AuditChainLockKey); err != nil {
		return DeviceIngestResult{}, fmt.Errorf("store: lock audit chain (waited up to %s; another transaction that inserted into audit_events may still be open): %w",
			db.AuditChainLockTimeout, err)
	}
	const insertQ = `INSERT INTO audit_events (` + auditCols + `) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`
	for _, r := range toIngest {
		data, err := mergeDeviceOrigin(deviceID, r)
		if err != nil {
			return DeviceIngestResult{}, fmt.Errorf("store: merge device origin: %w", err)
		}
		if _, err := tx.Exec(ctx, insertQ,
			r.ID, r.Time, r.RunID, string(r.ActorType), FederatedActor(deviceID, r.Actor), r.Action,
			CapAuditTarget(r.Target), r.Outcome, peer, data,
		); err != nil {
			return DeviceIngestResult{}, fmt.Errorf("store: insert federated audit event: %w", err)
		}
	}

	last := toIngest[len(toIngest)-1]
	if _, err := tx.Exec(ctx,
		`UPDATE devices SET last_seq = $2, last_row_hash = $3, last_seen_at = $4 WHERE id = $1`,
		deviceID, last.Seq, last.RowHash, s.now(),
	); err != nil {
		return DeviceIngestResult{}, fmt.Errorf("store: advance device cursor: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return DeviceIngestResult{}, fmt.Errorf("store: commit device audit ingest: %w", err)
	}
	return DeviceIngestResult{Accepted: len(toIngest), Reset: reset}, nil
}

// verifyClaimedHashes recomputes every row's claimed RowHash IN SQL, from
// that row's own claimed fields and its own claimed PrevHash, in one
// statement — never by re-marshalling in Go (json.Marshal on a round-tripped
// value reorders keys and changes the digest). Data travels as text and is
// cast ::jsonb, the same input function a jsonb parameter or column goes
// through, which is the canonicalization the device's own trigger applied;
// the uuid columns travel as text for the same reason (NULL run_id included).
func (s PG) verifyClaimedHashes(ctx context.Context, rows []types.FederatedAuditEvent) error {
	n := len(rows)
	prev, ids, actorTypes, actors, actions := make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n)
	targets, outcomes, sourceIPs := make([]string, n), make([]string, n), make([]string, n)
	times := make([]time.Time, n)
	runIDs, data := make([]*string, n), make([]*string, n)
	for i, r := range rows {
		prev[i], ids[i], times[i] = r.PrevHash, r.ID.String(), r.Time
		actorTypes[i], actors[i], actions[i] = string(r.ActorType), r.Actor, r.Action
		targets[i], outcomes[i], sourceIPs[i] = r.Target, r.Outcome, r.SourceIP
		if r.RunID != nil {
			id := r.RunID.String()
			runIDs[i] = &id
		}
		if len(r.Data) > 0 {
			d := string(r.Data)
			data[i] = &d
		}
	}
	const q = `
		SELECT audit_row_hash(u.prev, u.id::uuid, u.at, u.run_id::uuid, u.actor_type, u.actor,
		                      u.action, u.target, u.outcome, u.source_ip, u.data::jsonb)
		FROM unnest($1::text[], $2::text[], $3::timestamptz[], $4::text[], $5::text[], $6::text[],
		            $7::text[], $8::text[], $9::text[], $10::text[], $11::text[])
		     WITH ORDINALITY AS u(prev, id, at, run_id, actor_type, actor, action, target, outcome, source_ip, data, n)
		ORDER BY u.n`
	got, err := collect(ctx, s.Pool, "recompute", "federated audit row hashes", q,
		[]any{prev, ids, times, runIDs, actorTypes, actors, actions, targets, outcomes, sourceIPs, data},
		func(row pgx.Row) (string, error) {
			var h string
			return h, row.Scan(&h)
		})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && strings.HasPrefix(pgErr.Code, "22") {
		return fmt.Errorf("store: a claimed value cannot be stored (SQLSTATE %s): %w", pgErr.Code, ErrFederatedRowInvalid)
	}
	if err != nil {
		return err
	}
	if len(got) != n {
		return fmt.Errorf("store: recomputed %d federated row hashes for %d rows", len(got), n)
	}
	for i, r := range rows {
		if got[i] != r.RowHash {
			return fmt.Errorf("store: ingest device audit: row seq %d hash mismatch (edited after the device chained it): %w",
				r.Seq, ErrConflict)
		}
	}
	return nil
}

// refuseOrgRuns answers ErrFederatedOrgRun when any row's run_id is a run in
// this organisation's agent_runs (audit_events.run_id carries no foreign key,
// so nothing else would stop it).
func refuseOrgRuns(ctx context.Context, tx pgx.Tx, rows []types.FederatedAuditEvent) error {
	var ids []string
	for _, r := range rows {
		if r.RunID != nil {
			ids = append(ids, r.RunID.String())
		}
	}
	if len(ids) == 0 {
		return nil
	}
	var hit bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_runs WHERE id = ANY($1::uuid[]))`, ids).Scan(&hit); err != nil {
		return fmt.Errorf("store: check federated run ids: %w", err)
	}
	if hit {
		return ErrFederatedOrgRun
	}
	return nil
}

// FederatedClaimHashSQL recomputes, from one STORED federated row aliased e,
// the hash the device claimed for it. It equals
// e.data->'device_origin'->>'row_hash' for every row IngestDeviceAudit
// accepts: the claimed actor, source_ip and prev_hash are in device_origin,
// the claimed object is the stored data minus that key, and a data-less claim
// is named by device_origin.data_null — "json" for a JSON null (what a laptop
// row with no data holds), "sql" for an absent value.
const FederatedClaimHashSQL = `audit_row_hash(e.data->'device_origin'->>'prev_hash', e.id, e.time, e.run_id,
	e.actor_type, e.data->'device_origin'->>'actor', e.action, e.target, e.outcome, e.data->'device_origin'->>'source_ip',
	CASE e.data->'device_origin'->>'data_null' WHEN 'json' THEN 'null'::jsonb WHEN 'sql' THEN NULL
	     ELSE e.data - 'device_origin' END)`

// FederatedActor is the actor a federated row is stored under: the forwarding
// device's own actor (device:<id>, the name the organisation's rows about a
// device already use), a slash, then the principal the device claimed, so a
// filter or rule keyed on an organisation principal never matches a laptop's
// claim to be that principal.
func FederatedActor(deviceID uuid.UUID, claimed string) string {
	return "device:" + deviceID.String() + "/" + claimed
}

// FederatedDeviceID reports which device forwarded ev, or nil for one of the
// organisation's own rows. A row is federated only when BOTH marks agree — the
// data.device_origin.device_id IngestDeviceAudit writes and the FederatedActor
// prefix naming that same device — so no organisation writer that controls
// only one of them (a sensor's raw data, a principal's name) can make its own
// row read as a device's. federatedRowSQL is the same predicate in SQL.
func FederatedDeviceID(ev types.AuditEvent) *uuid.UUID {
	if len(ev.Data) == 0 || !bytes.Contains(ev.Data, []byte(`"device_origin"`)) {
		return nil
	}
	var m struct {
		DeviceOrigin struct {
			DeviceID string `json:"device_id"`
		} `json:"device_origin"`
	}
	if json.Unmarshal(ev.Data, &m) != nil || m.DeviceOrigin.DeviceID == "" ||
		!strings.HasPrefix(ev.Actor, "device:"+m.DeviceOrigin.DeviceID+"/") {
		return nil
	}
	id, err := uuid.Parse(m.DeviceOrigin.DeviceID)
	if err != nil {
		return nil
	}
	return &id
}

// federatedRowSQL is FederatedDeviceID's predicate over an audit_events row:
// true for a federated row, false (never NULL) for the organisation's own.
const federatedRowSQL = `COALESCE(starts_with(actor, 'device:' || (data->'device_origin'->>'device_id') || '/'), false)`

// mergeDeviceOrigin folds one federated row's provenance into its data as a
// "device_origin" object: which device forwarded it, that device's own local
// seq, the link it claimed, and the actor and source_ip it claimed (the
// columns hold FederatedActor and the peer this organisation saw instead).
//
// The claimed object's members are carried as raw JSON, never decoded, so
// they survive byte-for-byte (a float64 round trip would rewrite a large
// integer); FederatedRowProblem has already refused every other shape. That
// is what keeps the device's claim re-checkable from the stored row
// (FederatedClaimHashSQL). The row's own org-chain hash is computed by this
// organisation's trigger over whatever lands in the column.
func mergeDeviceOrigin(deviceID uuid.UUID, r types.FederatedAuditEvent) (json.RawMessage, error) {
	origin := map[string]any{
		"device_id": deviceID,
		"seq":       r.Seq,
		"row_hash":  r.RowHash,
		"prev_hash": r.PrevHash,
		"actor":     r.Actor,
		"source_ip": r.SourceIP,
	}
	m := map[string]json.RawMessage{}
	switch {
	case len(r.Data) == 0:
		origin["data_null"] = "sql"
	case isJSONNull(r.Data):
		origin["data_null"] = "json"
	default:
		if err := json.Unmarshal(r.Data, &m); err != nil {
			return nil, err
		}
	}
	o, err := json.Marshal(origin)
	if err != nil {
		return nil, err
	}
	m["device_origin"] = o
	return json.Marshal(m)
}

// ListAuditEventsAfterSeq returns THIS deployment's own audit_events strictly
// after seq, oldest first, each carrying its own seq — the laptop-side
// forwarder's read of its local chain to build the next batch
// IngestDeviceAudit expects: the returned PrevHash/RowHash are read straight
// off this table, so they are exactly what this deployment's own trigger
// chained them to, which is what the forwarder then claims upward. limit<=0
// defaults to 1000, matching QueryAuditEvents/QueryRecentAuditEvents.
func (s PG) ListAuditEventsAfterSeq(ctx context.Context, seq int64, limit int) ([]types.FederatedAuditEvent, error) {
	if limit <= 0 {
		limit = 1000
	}
	const q = `
		SELECT id, time, run_id, actor_type, actor, action, target, outcome, source_ip, data,
		       COALESCE(prev_hash,''), COALESCE(row_hash,''), seq
		FROM audit_events
		WHERE seq > $1
		ORDER BY seq
		LIMIT $2`
	return collect(ctx, s.Pool, "list", "federated audit events", q, []any{seq, limit}, scanFederatedAuditEvent)
}

func scanFederatedAuditEvent(row pgx.Row) (types.FederatedAuditEvent, error) {
	var e types.FederatedAuditEvent
	var actorType string
	var dataRaw []byte
	err := row.Scan(&e.ID, &e.Time, &e.RunID, &actorType, &e.Actor, &e.Action, &e.Target, &e.Outcome, &e.SourceIP, &dataRaw,
		&e.PrevHash, &e.RowHash, &e.Seq)
	if err != nil {
		return types.FederatedAuditEvent{}, fmt.Errorf("store: scan federated audit event: %w", err)
	}
	e.ActorType = types.ActorType(actorType)
	e.Data = dataRaw
	return e, nil
}

// GetFederationCursor returns how far THIS deployment's forwarder has pushed
// its local audit_events upward — 0 when nothing has been forwarded yet
// (org_federation carries no row until the first SetFederationCursor call,
// the same zero-value-on-no-row contract GetSiteConfig uses).
func (s PG) GetFederationCursor(ctx context.Context) (int64, error) {
	var seq int64
	err := s.Pool.QueryRow(ctx, `SELECT last_forwarded_seq FROM org_federation WHERE singleton`).Scan(&seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store: get federation cursor: %w", err)
	}
	return seq, nil
}

// SetFederationCursor durably advances the forwarder's cursor, upserting the
// singleton row — the write PutSiteConfig's shape mirrors. Called after a
// batch is successfully accepted upstream (docs/design/0.8/PLAN.md: "the
// forwarder advances a durable cursor"), never before, so a crash between the
// organisation's accept and this write only ever costs a re-verified,
// idempotent retry (see IngestDeviceAudit), never a gap.
func (s PG) SetFederationCursor(ctx context.Context, seq int64) error {
	const q = `
		INSERT INTO org_federation (singleton, last_forwarded_seq, updated_at)
		VALUES (true, $1, now())
		ON CONFLICT (singleton) DO UPDATE SET last_forwarded_seq = EXCLUDED.last_forwarded_seq, updated_at = now()`
	if _, err := s.Pool.Exec(ctx, q, seq); err != nil {
		return fmt.Errorf("store: set federation cursor: %w", err)
	}
	return nil
}
