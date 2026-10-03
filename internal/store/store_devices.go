// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Hybrid enrolment and audit federation (migration 0066): the organisation-side device inventory
// (devices, device_enrolment_tokens) and the laptop side's durable forwarder cursor (org_federation).
// Kept out of store.go (already at a lint size boundary), mirroring store_apitokens.go/store_sshkeys.go's split.
//
// SECURITY: hashToken (store_ephemeral.go) is reused for both credential tables — the raw device
// bearer and raw enrolment token never reach a row, only hex(sha256(raw)).
package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// DeviceStore is the optional store capability behind hybrid enrolment and audit federation
// (docs/design/0.8/PLAN.md, epic #78). Optional like Pager and AuditChainVerifier: a test fake or
// future non-Postgres store implements neither, so mounting routes must type-assert and answer
// 501/401 rather than silently no-op. SECURITY (fail-closed): nothing here has a default no-op on
// Store, so a skipped assertion is a compile error and a failed one has an explicit absent branch.
//
// GetDeviceByRaw's WHERE carries `revoked_at IS NULL` so a revoked credential is not an oracle.
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
	GetFederationCursor(ctx context.Context) (seq int64, rowHash string, err error)
	SetFederationCursor(ctx context.Context, seq int64, rowHash string) error
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

// MintEnrolmentToken inserts one single-use enrolment token. ErrConflict on a unique_violation
// (23505) against token_sha256 — a raw collision in a 256-bit random space, same as CreateAPIToken's.
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

// ConsumeEnrolmentToken redeems token exactly once: a single-row conditional UPDATE...RETURNING, the
// same atomic consume-once shape as ConsumeAttachTicket, adapted to an UPDATE since the row is worth
// keeping after redemption for CreateDevice. TRUST BOUNDARY: an unknown, already-consumed, and expired
// token all answer (zero, false, nil) alike — no oracle, matching ConsumeAttachTicket.
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

// ListEnrolmentTokens returns every token still redeemable at now (unconsumed, unexpired), newest
// first — the ones an admin may still cancel; a redeemed or expired token is never listed.
func (s PG) ListEnrolmentTokens(ctx context.Context, now time.Time) ([]types.DeviceEnrolmentToken, error) {
	const q = `SELECT ` + enrolmentTokenCols + ` FROM device_enrolment_tokens
		WHERE consumed_at IS NULL AND expires_at > $1 ORDER BY created_at DESC`
	return collect(ctx, s.Pool, "list", "device enrolment tokens", q, []any{now}, scanEnrolmentToken)
}

// RevokeEnrolmentToken cancels a redeemable token by setting consumed_at (the column
// ConsumeEnrolmentToken's WHERE requires NULL), so a revoke can't race a redemption.
// Already-redeemed/revoked/expired/unknown is ErrNotFound.
func (s PG) RevokeEnrolmentToken(ctx context.Context, id uuid.UUID, now time.Time) (types.DeviceEnrolmentToken, error) {
	const q = `
		UPDATE device_enrolment_tokens SET consumed_at = $2
		WHERE id = $1 AND consumed_at IS NULL AND expires_at > $2
		RETURNING ` + enrolmentTokenCols
	return scanEnrolmentToken(s.Pool.QueryRow(ctx, q, id, now))
}

// CreateDevice inserts one device row, following CreateAPIToken's shape: raw is the plaintext device
// credential minted at enrolment. SECURITY: only its hash is stored, and d.CredentialSHA256 is
// ignored, so the caller can never accidentally persist the secret twice; raw is returned to the
// device exactly once, unrecoverable afterwards. ErrConflict on a credential_sha256 collision, same
// 256-bit space as MintEnrolmentToken.
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

// GetDeviceByRaw is the device auth-time lookup: given the bearer a device presented, resolve the
// live row it stands for. TRUST BOUNDARY: `revoked_at IS NULL` is in the WHERE (mirrors
// GetAPITokenByRaw), so a revoked/unknown/mismatched credential all fail identically with ErrNotFound.
func (s PG) GetDeviceByRaw(ctx context.Context, raw string) (types.Device, error) {
	const q = `SELECT ` + deviceCols + ` FROM devices WHERE credential_sha256 = $1 AND revoked_at IS NULL`
	return scanDevice(s.Pool.QueryRow(ctx, q, hashToken(raw)))
}

// TouchDevice records that id was just used. Best effort, like TouchAPIToken: the request path must
// never fail an otherwise-valid request over a lost keepalive write.
func (s PG) TouchDevice(ctx context.Context, id uuid.UUID, now time.Time) error {
	if _, err := s.Pool.Exec(ctx, `UPDATE devices SET last_seen_at = $2 WHERE id = $1`, id, now); err != nil {
		return fmt.Errorf("store: touch device: %w", err)
	}
	return nil
}

// ListDevices returns every enrolled device, newest first, revoked rows included (an admin needs to
// see a retired device is retired). Phase one has no console surface for this (CLI/API only).
func (s PG) ListDevices(ctx context.Context) ([]types.Device, error) {
	const q = `SELECT ` + deviceCols + ` FROM devices ORDER BY created_at DESC`
	return collect(ctx, s.Pool, "list", "devices", q, nil, scanDevice)
}

// RevokeDevice marks id revoked. Already-revoked is ErrNotFound too (`revoked_at IS NULL` in the
// WHERE): idempotent, so a second call emits no second revoke audit row.
func (s PG) RevokeDevice(ctx context.Context, id uuid.UUID, now time.Time) (types.Device, error) {
	const q = `
		UPDATE devices SET revoked_at = $2
		WHERE id = $1 AND revoked_at IS NULL
		RETURNING ` + deviceCols
	return scanDevice(s.Pool.QueryRow(ctx, q, id, now))
}

// DeviceIngestResult reports what one IngestDeviceAudit call did. Accepted is how many rows were
// newly appended: 0 on any refusal (whole batch is one transaction) and also on an idempotent retry.
// Reset is true when accepted rows began with a genesis (empty-PrevHash) row while the device already
// had a recorded chain — a "device chain reset", accepted rather than refused.
type DeviceIngestResult struct {
	Accepted int
	Reset    bool
}

// ErrDeviceRevoked is IngestDeviceAudit's answer for a device revoked after its request was
// authenticated. Distinct from ErrConflict so the caller answers it as revocation, never a broken chain.
var ErrDeviceRevoked = errors.New("store: device is revoked")

// ErrFederatedOrgRun refuses a batch where a row's run_id names one of this organisation's own runs:
// phase one federates audit rows, never run records, so a laptop's row can't legitimately belong to one.
var ErrFederatedOrgRun = errors.New("store: federated row names an organisation run")

// ErrFederatedRowInvalid refuses a batch holding a row this organisation can't store
// (FederatedRowProblem) or a value Postgres can't represent (SQLSTATE class 22); the caller answers
// 4xx so the forwarder stops rather than retrying a 5xx forever.
var ErrFederatedRowInvalid = errors.New("store: federated row cannot be stored as claimed")

// FederatedRowProblem says why r cannot be stored so its claim re-checks from the stored row, or ""
// when it can. data must be a JSON object without a top-level device_origin key (this organisation's
// marker), JSON null, or absent; target must be one CapAuditTarget leaves unchanged; device_id is
// this organisation's to derive, never the device's to claim. Exported so the API refuses with a 400
// before any database work.
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

// IngestDeviceAudit appends one device's forwarded batch to this organisation's own audit_events, as
// the organisation's own chained rows ("one chain per writer"). A federated row keeps the device's
// claimed actor_type, action, target and outcome; what marks it forwarded is the device's provenance
// folded into `data` (mergeDeviceOrigin), not a new audit action, so no filter/SIEM rule keyed on
// `action` has to learn about it. TRUST BOUNDARY: three fields are never the device's to choose —
// actor is FederatedActor (claim kept in data.device_origin.actor); source_ip is peer (claim kept in
// data.device_origin.source_ip); and a run_id naming one of this organisation's own runs refuses the
// batch (ErrFederatedOrgRun).
//
// Order of work, chosen so a device can make the org's own audit writers wait for at most its inserts:
//
//  0. Every row must be storable (FederatedRowProblem) and representable by Postgres, or ErrFederatedRowInvalid.
//  1. Every claimed RowHash is recomputed in SQL before any transaction or lock, in ONE statement
//     (verifyClaimedHashes), against its own claimed PrevHash. A refused or replayed batch never
//     touches the chain lock.
//  2. The device row is locked FOR UPDATE, serializing this device's concurrent pushes before either
//     reaches the chain lock (deadlock-free: no transaction takes the chain lock then a device row).
//     A revoked device answers ErrDeviceRevoked.
//  3. Idempotency rides the cursor and the hash, not audit_events.id: rows at or before LastSeq that
//     are the rows held here (heldPrefix) are skipped — the forwarder's cursor can lag what was
//     committed. A reused seq is not a re-send.
//  4. The first new row's claimed PrevHash is either empty (genesis, accepted — a chain reset if one
//     was already recorded) or equal to LastRowHash with seq past LastSeq; every later row's claimed
//     PrevHash equals the preceding row's claimed RowHash. Otherwise ErrConflict.
//  5. Only then the chain lock (db.AuditChainLockKey), under the lock timeout, and the inserts: field
//     for field as claimed except actor, source_ip and merged data. Cursor advances in the same transaction.
//
// Any refusal refuses the entire batch, never a verified prefix: it's the retry unit, and a partial
// accept would leave the cursor mid-batch with no way to tell the caller which rows to resend.
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

	// Pinned READ COMMITTED like every other audit_events writer: at REPEATABLE READ this transaction
	// would chain onto a stale head and the verify sweep would latch a permanent false tamper verdict.
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return DeviceIngestResult{}, fmt.Errorf("store: begin device audit ingest tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // best-effort on the failure path

	// Bound every lock wait in this transaction — the device row's as well as the chain's. A timeout
	// is not a lost event: the forwarder retries from its own durable cursor.
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

	start, err := heldPrefix(ctx, tx, deviceID, lastSeq, rows)
	if err != nil {
		return DeviceIngestResult{}, err
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
	} else if toIngest[0].Seq <= lastSeq || toIngest[0].PrevHash != lastRowHash {
		// At or before the cursor, a non-held row can only begin a new chain (genesis, above); anything else rewrites history already held.
		return DeviceIngestResult{}, fmt.Errorf(
			"store: ingest device audit: row seq %d claims prev_hash %q, device's recorded head is seq %d %q: %w",
			toIngest[0].Seq, toIngest[0].PrevHash, lastSeq, lastRowHash, ErrConflict)
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
	const insertQ = `SELECT seq FROM audit_append($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`
	for _, r := range toIngest {
		data, err := mergeDeviceOrigin(deviceID, r)
		if err != nil {
			return DeviceIngestResult{}, fmt.Errorf("store: merge device origin: %w", err)
		}
		var appended int64
		if err := tx.QueryRow(ctx, insertQ,
			r.ID, r.Time, r.RunID, string(r.ActorType), FederatedActor(deviceID, r.Actor), r.Action,
			CapAuditTarget(r.Target), r.Outcome, peer, data,
		).Scan(&appended); err != nil {
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

// heldPrefix counts the batch's leading rows already held: each at or before the recorded cursor AND
// carrying the row_hash of the newest row ingested at that device seq. Seq alone isn't identity — a
// laptop table reset (TRUNCATE...RESTART IDENTITY, a restore) reuses seqs — so matching on seq alone
// would silently drop a new chain's rows as re-sends. Rows for one device are written only under its
// row lock, held by the caller, so this read can't race an ingest.
func heldPrefix(ctx context.Context, tx pgx.Tx, deviceID uuid.UUID, lastSeq int64, rows []types.FederatedAuditEvent) (int, error) {
	var seqs []string
	for _, r := range rows {
		if r.Seq > lastSeq {
			break
		}
		seqs = append(seqs, strconv.FormatInt(r.Seq, 10))
	}
	if len(seqs) == 0 {
		return 0, nil
	}
	// Text comparison on both keys, matching audit_events_device_origin_idx: mergeDeviceOrigin wrote a
	// uuid string and a JSON integer, whose ->> text is exactly FormatInt's.
	res, err := tx.Query(ctx, `
		SELECT DISTINCT ON (data->'device_origin'->>'seq') data->'device_origin'->>'seq', data->'device_origin'->>'row_hash'
		FROM audit_events
		WHERE data ? 'device_origin' AND data->'device_origin'->>'device_id' = $1
		  AND data->'device_origin'->>'seq' = ANY($2::text[])
		ORDER BY data->'device_origin'->>'seq', seq DESC`, deviceID.String(), seqs)
	if err != nil {
		return 0, fmt.Errorf("store: read held device rows: %w", err)
	}
	held := map[string]string{}
	var seq, hash string
	if _, err := pgx.ForEachRow(res, []any{&seq, &hash}, func() error { held[seq] = hash; return nil }); err != nil {
		return 0, fmt.Errorf("store: read held device rows: %w", err)
	}
	n := 0
	for n < len(seqs) && held[seqs[n]] == rows[n].RowHash {
		n++
	}
	return n, nil
}

// verifyClaimedHashes recomputes every row's claimed RowHash in SQL, from its own claimed fields and
// PrevHash, in one statement — never re-marshalled in Go (json.Marshal on a round-tripped value
// reorders keys, changing the digest). Data and uuid columns travel as text for the same
// canonicalization the device's own trigger applied.
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

// refuseOrgRuns answers ErrFederatedOrgRun when any row's run_id names a run in this organisation's
// agent_runs (audit_events.run_id has no foreign key, so nothing else would stop it).
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

// FederatedClaimHashSQL recomputes, from one stored federated row aliased e, the hash the device
// claimed for it: equals e.data->'device_origin'->>'row_hash' for every row IngestDeviceAudit
// accepts. Claimed actor/source_ip/prev_hash live in device_origin; the claimed object is stored data
// minus that key; a data-less claim is named by device_origin.data_null ("json" for JSON null, "sql" for absent).
const FederatedClaimHashSQL = `audit_row_hash(e.data->'device_origin'->>'prev_hash', e.id, e.time, e.run_id,
	e.actor_type, e.data->'device_origin'->>'actor', e.action, e.target, e.outcome, e.data->'device_origin'->>'source_ip',
	CASE e.data->'device_origin'->>'data_null' WHEN 'json' THEN 'null'::jsonb WHEN 'sql' THEN NULL
	     ELSE e.data - 'device_origin' END)`

// FederatedActor is the actor a federated row is stored under: the forwarding device's own actor
// (device:<id>), a slash, then the claimed principal, so an org-principal filter never matches a laptop's claim.
func FederatedActor(deviceID uuid.UUID, claimed string) string {
	return "device:" + deviceID.String() + "/" + claimed
}

// FederatedDeviceID reports which device forwarded ev, or nil for the organisation's own row. TRUST
// BOUNDARY: federated only when both marks agree — data.device_origin.device_id and the
// FederatedActor prefix naming the same device — so no writer controlling only one can make its own
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

// federatedRowSQL is FederatedDeviceID's predicate over an audit_events row: true for a federated
// row, false (never NULL) for the organisation's own.
const federatedRowSQL = `COALESCE(starts_with(actor, 'device:' || (data->'device_origin'->>'device_id') || '/'), false)`

// mergeDeviceOrigin folds one federated row's provenance into its data as a "device_origin" object:
// which device forwarded it, its own local seq, the link it claimed, and the actor/source_ip it
// claimed (the columns instead hold FederatedActor and the peer this organisation saw).
//
// The claimed object's members are carried as raw JSON, never decoded, so they survive byte-for-byte
// (a float64 round trip would rewrite a large integer); FederatedRowProblem already refused every
// other shape. That's what keeps the claim re-checkable from the stored row (FederatedClaimHashSQL).
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

// ListAuditEventsAfterSeq returns this deployment's own audit_events strictly after seq, oldest
// first — the forwarder's read of its local chain to build the next batch IngestDeviceAudit expects.
// limit<=0 defaults to 1000, matching QueryAuditEvents/QueryRecentAuditEvents.
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

// AuditHeadSeq is this deployment's newest audit_events seq, 0 on an empty table: the head the
// forwarder measures lag against, read every tick whether or not a push succeeds.
func (s PG) AuditHeadSeq(ctx context.Context) (int64, error) {
	var seq int64
	if err := s.Pool.QueryRow(ctx, `SELECT COALESCE(max(seq), 0) FROM audit_events`).Scan(&seq); err != nil {
		return 0, fmt.Errorf("store: audit head seq: %w", err)
	}
	return seq, nil
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

// GetFederationCursor returns how far this deployment's forwarder has pushed audit_events upward,
// and the row_hash the organisation acknowledged at that seq — 0 and "" when nothing forwarded yet
// (matching GetSiteConfig's zero-value-on-no-row contract).
func (s PG) GetFederationCursor(ctx context.Context) (int64, string, error) {
	var seq int64
	var rowHash string
	err := s.Pool.QueryRow(ctx, `SELECT last_forwarded_seq, last_forwarded_row_hash FROM org_federation WHERE singleton`).Scan(&seq, &rowHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", nil
	}
	if err != nil {
		return 0, "", fmt.Errorf("store: get federation cursor: %w", err)
	}
	return seq, rowHash, nil
}

// FederationRevoked reports whether the organisation revoked this laptop's
// device credential (MarkFederationRevoked), false when no row exists.
func (s PG) FederationRevoked(ctx context.Context) (bool, error) {
	var revoked bool
	err := s.Pool.QueryRow(ctx, `SELECT revoked_at IS NOT NULL FROM org_federation WHERE singleton`).Scan(&revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: get federation revoked: %w", err)
	}
	return revoked, nil
}

// MarkFederationRevoked durably records that the organisation revoked this
// laptop, so a restart comes back still refusing new runs.
func (s PG) MarkFederationRevoked(ctx context.Context) error {
	const q = `
		INSERT INTO org_federation (singleton, revoked_at, updated_at)
		VALUES (true, now(), now())
		ON CONFLICT (singleton) DO UPDATE SET revoked_at = COALESCE(org_federation.revoked_at, now()), updated_at = now()`
	if _, err := s.Pool.Exec(ctx, q); err != nil {
		return fmt.Errorf("store: mark federation revoked: %w", err)
	}
	return nil
}

// ResetFederation is a (re-)enrolment: a new device identity starts an empty
// chain at the organisation, resetting the cursor to 0 and clearing the
// revoked mark. The only writer that clears it.
func (s PG) ResetFederation(ctx context.Context) error {
	const q = `
		INSERT INTO org_federation (singleton, last_forwarded_seq, last_forwarded_row_hash, revoked_at, updated_at)
		VALUES (true, 0, '', NULL, now())
		ON CONFLICT (singleton) DO UPDATE SET last_forwarded_seq = 0, last_forwarded_row_hash = '', revoked_at = NULL, updated_at = now()`
	if _, err := s.Pool.Exec(ctx, q); err != nil {
		return fmt.Errorf("store: reset federation: %w", err)
	}
	return nil
}

// SetFederationCursor durably advances the forwarder's cursor (seq and the local row's row_hash, so a
// later tick can tell it from a table-reset row at the same seq), upserting the singleton row. Called
// after a batch is accepted upstream, so a crash between accept and this write only costs an
// idempotent retry (see IngestDeviceAudit), never a gap.
func (s PG) SetFederationCursor(ctx context.Context, seq int64, rowHash string) error {
	const q = `
		INSERT INTO org_federation (singleton, last_forwarded_seq, last_forwarded_row_hash, updated_at)
		VALUES (true, $1, $2, now())
		ON CONFLICT (singleton) DO UPDATE SET last_forwarded_seq = EXCLUDED.last_forwarded_seq,
			last_forwarded_row_hash = EXCLUDED.last_forwarded_row_hash, updated_at = now()`
	if _, err := s.Pool.Exec(ctx, q, seq, rowHash); err != nil {
		return fmt.Errorf("store: set federation cursor: %w", err)
	}
	return nil
}
