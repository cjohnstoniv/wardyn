// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// maxAuditBatch bounds one InsertAuditEvents call.
const maxAuditBatch = 512

// auditBatchReadBackSQL reads the stored rows of a replayed batch: an event is held when a row with its id and
// time is stored for the same run. The run comes back too, so the match in Go is by id, time and run, as the join
// is. The time index finds each; under the chain lock, every earlier writer has
// committed. seq order makes the first stored row the one whose hashes a duplicate takes, as
// auditReadBackSQL does.
const auditBatchReadBackSQL = `
	SELECT e.id, e.time, e.run_id, COALESCE(e.prev_hash,''), COALESCE(e.row_hash,'')
	FROM audit_events e
	JOIN unnest($1::uuid[], $2::timestamptz[], $3::uuid[]) AS k(id, t, run_id)
	  ON e.time = k.t AND e.id = k.id AND e.run_id IS NOT DISTINCT FROM k.run_id
	ORDER BY e.seq`

var _ audit.BatchRecorder = Recorder{}

// RecordBatch appends evs under one lock (InsertAuditEvents). Every outcome is checked first, as Record checks
// one, and one the table cannot store refuses the whole batch, so a spool that keeps the refused batch never
// holds half of one that also landed.
func (rec Recorder) RecordBatch(ctx context.Context, evs []types.AuditEvent) error {
	var bad error
	for _, ev := range evs {
		switch ev.Outcome {
		case "success", "failure", "denied":
		default:
			refusedAuditOutcomes.Add(1)
			slog.ErrorContext(ctx, "store: audit row refused: its outcome is not one the audit table stores",
				slog.String("action", ev.Action), slog.String("outcome", ev.Outcome))
			if bad == nil {
				bad = fmt.Errorf("%w: action %q outcome %q", ErrAuditOutcome, ev.Action, ev.Outcome)
			}
		}
	}
	if bad != nil {
		return bad
	}
	cp := append([]types.AuditEvent(nil), evs...)
	ptrs := make([]*types.AuditEvent, len(cp))
	for i := range cp {
		ptrs[i] = &cp[i]
	}
	held, err := InsertAuditEvents(ctx, rec.Pool, ptrs)
	if err != nil {
		return err
	}
	for i, ev := range cp {
		if held[i] {
			audit.NoteBatch(ctx, ev.ID, audit.BatchHeld)
		} else {
			audit.NoteBatch(ctx, ev.ID, audit.BatchAppended)
		}
	}
	return nil
}

// auditBatchKey matches a read-back row to its event: the id, the time at the database's precision and the run
// (Valid false for a row with no run).
type auditBatchKey struct {
	id  uuid.UUID
	us  int64
	run pgtype.UUID
}

// InsertAuditEvents appends evs in order under one take of the chain lock and one append slot, in one
// transaction: their seqs are contiguous and commit together, or none is written. Under WithAuditReplay an
// event whose id, time and run are already stored is not appended again, carries the stored hashes and is
// reported held. On success each ev carries the PrevHash and RowHash the database computed.
//
// Each row is appended by the same audit_append call InsertAuditEvent makes, so the chain format, the
// trigger and the row hash are the single path's. The lock is taken once and held across the appends and the
// commit, so seq order is commit order, as it is for single rows.
func InsertAuditEvents(ctx context.Context, pool *pgxpool.Pool, evs []*types.AuditEvent) (held []bool, err error) {
	if len(evs) == 0 {
		return nil, nil
	}
	if len(evs) > maxAuditBatch {
		return nil, fmt.Errorf("store: audit batch of %d events is over the limit of %d", len(evs), maxAuditBatch)
	}
	data := make([][]byte, len(evs))
	replay, _ := ctx.Value(auditReplayKey{}).(bool)
	for i, ev := range evs {
		ev.Target = CapAuditTarget(ev.Target)
		if data[i], err = json.Marshal(ev.Data); err != nil {
			return nil, fmt.Errorf("store: marshal audit data: %w", err)
		}
		if ev.ID == uuid.Nil {
			replay = false // the read-back matches on ids; a batch with an unset one appends every event
		}
	}
	release, left, err := takeAuditAppendSlot(ctx, pool, db.AuditChainLockTimeout)
	if err != nil {
		return nil, err
	}
	defer release()
	// Read committed is pinned, not inherited, for InsertAuditEvent's reason: the trigger's head read happens
	// inside this transaction.
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("store: begin audit tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // best-effort on the failure path

	held = make([]bool, len(evs))
	if err := lockAuditBatch(ctx, tx, evs, left, replay, held); err != nil {
		return nil, err
	}
	b := &pgx.Batch{}
	for i, ev := range evs {
		if !held[i] {
			b.Queue(auditAppendSQL, ev.ID, ev.Time, ev.RunID, string(ev.ActorType), ev.Actor, ev.Action,
				ev.Target, ev.Outcome, ev.SourceIP, data[i])
		}
	}
	if b.Len() > 0 {
		br := tx.SendBatch(ctx, b)
		defer br.Close() //nolint:errcheck // closes the pipeline on every error path; a second Close is a no-op
		for i, ev := range evs {
			if held[i] {
				continue
			}
			if err := br.QueryRow().Scan(&ev.PrevHash, &ev.RowHash); err != nil {
				return nil, fmt.Errorf("store: insert audit event %d of %d: %w", i+1, len(evs), err)
			}
		}
		if err := br.Close(); err != nil {
			return nil, fmt.Errorf("store: insert audit events: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store: commit audit events: %w", err)
	}
	return held, nil
}

// lockAuditBatch sends the lock wait bound, the chain lock and, for a replay, the read-back of evs already
// stored, in one round trip. A held event takes the stored row's hashes and is marked in held.
func lockAuditBatch(ctx context.Context, tx pgx.Tx, evs []*types.AuditEvent, left time.Duration, replay bool, held []bool) error {
	b := &pgx.Batch{}
	wait, waitArg := db.AuditChainLockWaitSQL(left)
	b.Queue(wait, waitArg)
	b.Queue(lockAuditChainSQL, db.AuditChainLockKey)
	runs := make([]pgtype.UUID, len(evs)) // each event's run as the read-back returns it; zero for none
	if replay {
		ids := make([]uuid.UUID, len(evs))
		ats := make([]time.Time, len(evs))
		for i, ev := range evs {
			ids[i], ats[i] = ev.ID, ev.Time
			if ev.RunID != nil {
				runs[i] = pgtype.UUID{Bytes: *ev.RunID, Valid: true}
			}
		}
		b.Queue(auditBatchReadBackSQL, ids, ats, runs)
	}
	br := tx.SendBatch(ctx, b)
	defer br.Close() //nolint:errcheck // closes the pipeline on every error path; a second Close is a no-op
	if _, err := br.Exec(); err != nil {
		return fmt.Errorf("store: bound audit chain lock wait: %w", err)
	}
	if _, err := br.Exec(); err != nil {
		return fmt.Errorf("store: lock audit chain (waited up to %s; another transaction that inserted into audit_events may still be open): %w",
			db.AuditChainLockTimeout, err)
	}
	if replay {
		type stored struct{ prev, row string }
		found := map[auditBatchKey]stored{}
		rows, err := br.Query()
		if err != nil {
			return fmt.Errorf("store: look up replayed audit events: %w", err)
		}
		for rows.Next() {
			var k auditBatchKey
			var at pgtype.Timestamptz
			var s stored
			if err := rows.Scan(&k.id, &at, &k.run, &s.prev, &s.row); err != nil {
				rows.Close()
				return fmt.Errorf("store: look up replayed audit events: %w", err)
			}
			k.us = at.Time.UnixMicro()
			if _, dup := found[k]; !dup { // the first stored row of an at-least-once duplicate
				found[k] = s
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("store: look up replayed audit events: %w", err)
		}
		for i, ev := range evs {
			if s, ok := found[auditBatchKey{ev.ID, ev.Time.UnixMicro(), runs[i]}]; ok {
				ev.PrevHash, ev.RowHash, held[i] = s.prev, s.row, true
			}
		}
	}
	if err := br.Close(); err != nil {
		return fmt.Errorf("store: lock audit chain: %w", err)
	}
	return nil
}
