// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

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
func InsertAuditEvent(ctx context.Context, pool *pgxpool.Pool, ev *types.AuditEvent) error {
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
	// Bound the wait BEFORE asking for the lock, since any open transaction that
	// touched audit_events holds it and this call is on the request path. A
	// timeout is not a lost event: the error reaches spoolingRecorder, which
	// fsyncs it to the local spool for the drain to replay.
	if _, err := tx.Exec(ctx, db.AuditChainLockTimeoutSQL()); err != nil {
		return fmt.Errorf("store: bound audit chain lock wait: %w", err)
	}
	if _, err := tx.Exec(ctx, lockAuditChainSQL, db.AuditChainLockKey); err != nil {
		return fmt.Errorf("store: lock audit chain (waited up to %s; another transaction that inserted into audit_events may still be open): %w",
			db.AuditChainLockTimeout, err)
	}
	// audit_append is the only way a row enters audit_events: it allocates seq and recorded_at under
	// the chain lock and the chain trigger refuses a row it did not allocate. COALESCE: the genesis
	// row's prev_hash is SQL NULL, which won't scan into a string; empty string and NULL both mean
	// "nothing before this row".
	const q = `
		SELECT COALESCE(prev_hash,''), COALESCE(row_hash,'')
		FROM audit_append($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`
	if err := tx.QueryRow(ctx, q,
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
