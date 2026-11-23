// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type auditReplayKey struct{}

// WithAuditReplay marks ctx's audit writes as replays of events whose id and time are already fixed
// (an owed row's pinned event, a spooled line): InsertAuditEvent appends one only when no row already
// carries that id and time, so replaying an event the table holds adds no row. Writers serialise on
// the chain lock, so two replays of one event at once land one row.
func WithAuditReplay(ctx context.Context) context.Context {
	return context.WithValue(ctx, auditReplayKey{}, true)
}

// InsertAuditEvent appends a single audit event. Implements audit.Recorder.
// The Postgres trigger blocks UPDATE/DELETE; this function only ever INSERTs.
//
// ev is taken by POINTER so the hash chain can be handed back: on success
// ev.PrevHash/ev.RowHash carry the values Postgres computed, and ev.RowHash IS
// the chain head at that instant, which fanoutRecorder emits to the audit sinks
// so an external SIEM ends up holding a head hash Wardyn cannot later disown.
//
// Runs in a transaction because pg_advisory_xact_lock must be held across the
// append: this caller's seq allocation and head read must not interleave with
// another writer's, keeping seq order and chain order identical. audit_append
// takes the same lock itself (the only way a row enters the table since 0111);
// the lock here is re-entrant and free, and keeps the bounded wait below in
// front of it.
//
// At most a few writers (auditAppendSlots) wait on the chain lock at once, so a held lock cannot take every
// connection the pool has: a writer waits for a slot first, within the same bound as the lock wait, and the lock
// wait gets what is left of it. A writer that finds every slot taken for the whole bound answers
// ErrAuditAppendBusy without having sent anything.
func InsertAuditEvent(ctx context.Context, pool *pgxpool.Pool, ev *types.AuditEvent) error {
	release, left, err := takeAuditAppendSlot(ctx, pool, db.AuditChainLockTimeout)
	if err != nil {
		return err
	}
	defer release()
	// The cap lives at the one INSERT every audit writer reaches (api server,
	// broker, identity, approval sweeper, spool drain), not in Server.auditEvent,
	// which internal/approval bypasses by building its own AuditEvent values.
	ev.Target = CapAuditTarget(ev.Target)
	dataJSON, err := json.Marshal(ev.Data)
	if err != nil {
		return fmt.Errorf("store: marshal audit data: %w", err)
	}
	// Read committed is pinned, not inherited: the trigger's prev_hash head read
	// happens inside THIS transaction, so under REPEATABLE READ a writer queued
	// behind the lock would read a stale snapshot and fork the chain. The
	// isolation level otherwise comes from default_transaction_isolation, a
	// USERSET GUC any role can change, so chain correctness can't depend on it.
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("store: begin audit tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // best-effort on the failure path
	if replay, _ := ctx.Value(auditReplayKey{}).(bool); replay && ev.ID != uuid.Nil {
		return insertReplayedAuditEvent(ctx, tx, ev, dataJSON, left)
	}
	// The lock wait bound, the chain lock and the append go to the database together: the lock is held for one
	// round trip less. Results are read strictly in order and the first error returns before Commit: a failed
	// statement aborts the transaction, so the statements queued behind it fail too, and the deferred Rollback
	// stays. Bound the wait BEFORE asking for the lock, since any open transaction that touched audit_events
	// holds it and this call is on the request path. A timeout is not a lost event: the error reaches
	// spoolingRecorder, which fsyncs it to the local spool for the drain to replay.
	b := &pgx.Batch{}
	wait, waitArg := db.AuditChainLockWaitSQL(left)
	b.Queue(wait, waitArg)
	b.Queue(lockAuditChainSQL, db.AuditChainLockKey)
	b.Queue(auditAppendSQL, ev.ID, ev.Time, ev.RunID, string(ev.ActorType), ev.Actor, ev.Action,
		ev.Target, ev.Outcome, ev.SourceIP, dataJSON)
	br := tx.SendBatch(ctx, b)
	defer br.Close() //nolint:errcheck // closes the pipeline on every error path; a second Close is a no-op
	if _, err := br.Exec(); err != nil {
		return fmt.Errorf("store: bound audit chain lock wait: %w", err)
	}
	if _, err := br.Exec(); err != nil {
		return fmt.Errorf("store: lock audit chain (waited up to %s; another transaction that inserted into audit_events may still be open): %w",
			db.AuditChainLockTimeout, err)
	}
	if err := br.QueryRow().Scan(&ev.PrevHash, &ev.RowHash); err != nil {
		return fmt.Errorf("store: insert audit event: %w", err)
	}
	if err := br.Close(); err != nil {
		return fmt.Errorf("store: insert audit event: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit audit event: %w", err)
	}
	return nil
}

// auditAppendSQL is the one statement that enters a row into audit_events: audit_append allocates seq and
// recorded_at under the chain lock and the chain trigger refuses a row it did not allocate. COALESCE: the
// genesis row's prev_hash is SQL NULL, which won't scan into a string; empty string and NULL both mean
// "nothing before this row".
const auditAppendSQL = `
		SELECT COALESCE(prev_hash,''), COALESCE(row_hash,'')
		FROM audit_append($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`

// auditReadBackSQL reads the first stored row of a replayed event (audit_events has no unique id, so an
// at-least-once duplicate gives two rows; the sinks always get the first stored row's hashes). The time index
// finds it; under the chain lock, every earlier writer has committed.
const auditReadBackSQL = `SELECT COALESCE(prev_hash,''), COALESCE(row_hash,'') FROM audit_events WHERE time = $1 AND id = $2 ORDER BY seq LIMIT 1`

// insertReplayedAuditEvent is InsertAuditEvent's body for a replayed event (WithAuditReplay): an event the
// table already holds is not appended again, and ev carries the stored row's hashes. The lock wait bound, the
// chain lock and the read-back go to the database together.
func insertReplayedAuditEvent(ctx context.Context, tx pgx.Tx, ev *types.AuditEvent, dataJSON []byte, left time.Duration) error {
	b := &pgx.Batch{}
	wait, waitArg := db.AuditChainLockWaitSQL(left)
	b.Queue(wait, waitArg)
	b.Queue(lockAuditChainSQL, db.AuditChainLockKey)
	b.Queue(auditReadBackSQL, ev.Time, ev.ID)
	br := tx.SendBatch(ctx, b)
	defer br.Close() //nolint:errcheck // closes the pipeline on every error path; a second Close is a no-op
	if _, err := br.Exec(); err != nil {
		return fmt.Errorf("store: bound audit chain lock wait: %w", err)
	}
	if _, err := br.Exec(); err != nil {
		return fmt.Errorf("store: lock audit chain (waited up to %s; another transaction that inserted into audit_events may still be open): %w",
			db.AuditChainLockTimeout, err)
	}
	err := br.QueryRow().Scan(&ev.PrevHash, &ev.RowHash)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("store: look up a replayed audit event: %w", err)
	}
	if cerr := br.Close(); cerr != nil {
		return fmt.Errorf("store: look up a replayed audit event: %w", cerr)
	}
	if err == nil {
		// Already recorded: ev carries the stored row's hashes. Nothing was written, so the commit is read-only.
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("store: commit audit event: %w", err)
		}
		return nil
	}
	// A new event: the append, on the same transaction, with the chain lock already held.
	if err := tx.QueryRow(ctx, auditAppendSQL,
		ev.ID, ev.Time, ev.RunID, string(ev.ActorType), ev.Actor, ev.Action,
		ev.Target, ev.Outcome, ev.SourceIP, dataJSON,
	).Scan(&ev.PrevHash, &ev.RowHash); err != nil {
		return fmt.Errorf("store: insert audit event: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit audit event: %w", err)
	}
	return nil
}
