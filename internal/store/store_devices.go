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
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	CreateDevice(ctx context.Context, d types.Device, raw string) (types.Device, error)
	GetDeviceByRaw(ctx context.Context, raw string) (types.Device, error)
	TouchDevice(ctx context.Context, id uuid.UUID, now time.Time) error
	ListDevices(ctx context.Context) ([]types.Device, error)
	RevokeDevice(ctx context.Context, id uuid.UUID, now time.Time) (types.Device, error)
	IngestDeviceAudit(ctx context.Context, deviceID uuid.UUID, rows []types.FederatedAuditEvent) (DeviceIngestResult, error)
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
// device used to be enrolled".
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

// IngestDeviceAudit appends one device's forwarded batch to THIS
// organisation's own audit_events, as the organisation's own chained rows —
// "one chain per writer" (docs/design/0.8/PLAN.md): a federated row is
// otherwise indistinguishable from one this organisation wrote itself, with
// the device's provenance folded into `data` instead of a new audit action,
// so no existing filter or SIEM rule keyed on `action` has to learn about it.
//
// The whole batch is ONE transaction, under the SAME advisory lock and lock
// timeout InsertAuditEvent takes (db.AuditChainLockKey via lockAuditChainSQL,
// bounded by db.AuditChainLockTimeoutSQL) — this call inserts into
// audit_events too, so it must serialize against every other writer the same
// way. The device's own cursor row is locked (FOR UPDATE) inside the same
// transaction, re-entrant with the advisory lock, so two concurrent pushes
// from the same device cannot both read the same cursor and both believe they
// are extending the chain.
//
// Verification, in order:
//
//  1. Idempotency rides the cursor, not audit_events.id — a device's id is
//     not unique across a retried push (the forwarder re-sends from its own
//     durable cursor, which can be behind what was already committed here if
//     it crashed between this call's success and its own cursor advance), but
//     the device's local seq is monotonic and gapless. Rows at or before the
//     device's recorded LastSeq are skipped, not re-verified or refused.
//  2. The first NEW row's claimed PrevHash decides reset vs. continuation: an
//     EMPTY PrevHash is always accepted as a genesis row (the device's local
//     chain started fresh, whether this is the very first ingest ever or a
//     reset after a purge on the device); anything else must equal the
//     device's recorded LastRowHash exactly, or the claim does not attach to
//     what this organisation already recorded and the WHOLE BATCH is refused
//     (ErrConflict).
//  3. Every later row's claimed PrevHash must equal the PRECEDING row's
//     claimed RowHash — the batch's own internal chain.
//  4. Every row's claimed RowHash must equal audit_row_hash(...) recomputed
//     IN SQL from that row's own fields — never by re-marshalling in Go
//     (json.Marshal on a round-tripped Go value reorders map keys and changes
//     the digest). Data is bound straight through as the ::jsonb parameter:
//     Postgres normalizes a jsonb INPUT parameter on parse exactly the way it
//     normalizes a jsonb COLUMN on insert, which is the same canonicalization
//     the device's own local trigger applied when it first computed this
//     digest — so this recomputation is bit-for-bit comparable to that one
//     without ever decoding Data into a Go value.
//
// Any mismatch anywhere refuses the ENTIRE batch (the transaction rolls
// back) rather than accepting a verified prefix: a batch is the unit the
// caller retries, and a partial accept would leave the cursor pointing
// mid-batch with no way to tell the caller which rows to resend.
//
// On success every row in the batch is inserted field-for-field (same id,
// time, run_id, actor_type, actor, action, target, outcome, source_ip as the
// device claimed — target still passes through CapAuditTarget, like every
// other audit_events writer) with the origin merged into data, and the
// device's LastSeq/LastRowHash advance to the batch's last row, atomically
// with the inserts.
func (s PG) IngestDeviceAudit(ctx context.Context, deviceID uuid.UUID, rows []types.FederatedAuditEvent) (DeviceIngestResult, error) {
	if len(rows) == 0 {
		return DeviceIngestResult{}, nil
	}

	// Pinned READ COMMITTED like every other audit_events writer: at
	// REPEATABLE READ this transaction would chain onto a stale head and the
	// verify sweep would latch a permanent false tamper verdict.
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return DeviceIngestResult{}, fmt.Errorf("store: begin device audit ingest tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // best-effort on the failure path

	// Bound the wait before asking for the lock — see InsertAuditEvent's own
	// comment: a timeout here is not a lost event, the caller (the forwarder)
	// retries from its own durable cursor.
	if _, err := tx.Exec(ctx, db.AuditChainLockTimeoutSQL()); err != nil {
		return DeviceIngestResult{}, fmt.Errorf("store: bound audit chain lock wait: %w", err)
	}
	if _, err := tx.Exec(ctx, lockAuditChainSQL, db.AuditChainLockKey); err != nil {
		return DeviceIngestResult{}, fmt.Errorf("store: lock audit chain (waited up to %s; another transaction that inserted into audit_events may still be open): %w",
			db.AuditChainLockTimeout, err)
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
		return DeviceIngestResult{}, fmt.Errorf("store: ingest device audit: device is revoked: %w", ErrConflict)
	}

	start := 0
	for start < len(rows) && rows[start].Seq <= lastSeq {
		start++
	}
	if start == len(rows) {
		// Every row in the batch was already ingested: an idempotent retry,
		// not a refusal. Nothing to verify or insert.
		if err := tx.Commit(ctx); err != nil {
			return DeviceIngestResult{}, fmt.Errorf("store: commit device audit ingest: %w", err)
		}
		return DeviceIngestResult{}, nil
	}
	toIngest := rows[start:]

	prev := lastRowHash
	reset := false
	if toIngest[0].PrevHash == "" {
		reset = lastRowHash != ""
		prev = ""
	} else if toIngest[0].PrevHash != lastRowHash {
		return DeviceIngestResult{}, fmt.Errorf(
			"store: ingest device audit: row seq %d claims prev_hash %q, device's recorded head is %q: %w",
			toIngest[0].Seq, toIngest[0].PrevHash, lastRowHash, ErrConflict)
	}

	for i, r := range toIngest {
		if i > 0 && r.PrevHash != toIngest[i-1].RowHash {
			return DeviceIngestResult{}, fmt.Errorf(
				"store: ingest device audit: row seq %d does not chain to the previous row in this batch: %w",
				r.Seq, ErrConflict)
		}
		var want string
		if err := tx.QueryRow(ctx,
			`SELECT audit_row_hash($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::jsonb)`,
			prev, r.ID, r.Time, r.RunID, string(r.ActorType), r.Actor,
			r.Action, r.Target, r.Outcome, r.SourceIP, []byte(r.Data),
		).Scan(&want); err != nil {
			return DeviceIngestResult{}, fmt.Errorf("store: recompute device audit row hash: %w", err)
		}
		if want != r.RowHash {
			return DeviceIngestResult{}, fmt.Errorf(
				"store: ingest device audit: row seq %d hash mismatch (edited after the device chained it): %w",
				r.Seq, ErrConflict)
		}
		prev = r.RowHash
	}

	const insertQ = `INSERT INTO audit_events (` + auditCols + `) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`
	for _, r := range toIngest {
		data, err := mergeDeviceOrigin(r.Data, deviceID, r.Seq, r.RowHash, r.PrevHash)
		if err != nil {
			return DeviceIngestResult{}, fmt.Errorf("store: merge device origin: %w", err)
		}
		if _, err := tx.Exec(ctx, insertQ,
			r.ID, r.Time, r.RunID, string(r.ActorType), r.Actor, r.Action, CapAuditTarget(r.Target), r.Outcome, r.SourceIP, data,
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

// mergeDeviceOrigin folds one federated row's provenance into its data as a
// "device_origin" object: which device forwarded it, that device's own local
// seq, and the link it claimed. This is what "one chain per writer" means in
// practice, in place of a new audit action — a federated row stays
// indistinguishable from an ordinary one to every existing filter and SIEM
// rule keyed on `action`, with the provenance riding in data instead.
//
// Unlike IngestDeviceAudit's hash VERIFICATION above, a Go-side json.Marshal
// here is fine: this builds the data for a BRAND NEW row on the
// organisation's own chain, hashed by ITS OWN trigger from whatever actually
// lands in the column — nothing here is compared against a digest the device
// already claimed.
func mergeDeviceOrigin(data json.RawMessage, deviceID uuid.UUID, seq int64, rowHash, prevHash string) (json.RawMessage, error) {
	m := map[string]any{}
	if len(data) > 0 && string(data) != "null" {
		if err := json.Unmarshal(data, &m); err != nil {
			// Not a JSON object (array/scalar) — wrap it rather than lose it.
			m = map[string]any{"data": json.RawMessage(data)}
		}
	}
	m["device_origin"] = map[string]any{
		"device_id": deviceID,
		"seq":       seq,
		"row_hash":  rowHash,
		"prev_hash": prevHash,
	}
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
