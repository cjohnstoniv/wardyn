// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ResolveReauthApproval moves a credential_reauth approval to APPROVED and
// writes its credential.reauth.resolve audit row in ONE transaction: the
// state change unblocks a HELD credential request, and the audit row is the
// only durable record that a human acted behind it. Committed separately, a
// crash between them would leave a run resuming with nothing in the trail
// saying who resolved it (invariant I7). If either write fails the approval
// stays PENDING and the idempotent reconcile-on-read tries again.
//
// It is NOT part of the Store interface (see iface.go on transactional
// surfaces): internal/api type-asserts for it.
//
// The CAS is DecideApproval's own: WHERE state='PENDING', so a concurrent
// resolver loses cleanly with ErrAlreadyDecided and writes no second audit row.
func (s PG) ResolveReauthApproval(ctx context.Context, id uuid.UUID, decision types.ApprovalDecision, ev types.AuditEvent) (types.ApprovalRequest, error) {
	if ev.Action != "credential.reauth.resolve" {
		// This method exists for ONE transition; accepting an arbitrary
		// action would make it a general "audit anything" primitive.
		return types.ApprovalRequest{}, fmt.Errorf("store: ResolveReauthApproval: refusing to write audit action %q", ev.Action)
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return types.ApprovalRequest{}, fmt.Errorf("store: begin reauth resolve tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // best-effort on the failure path

	decidedAt := s.now()
	age := db.AppClockAgeMicros(decidedAt, s.now())
	q := `
		UPDATE approvals
		SET state=$1, decided_at=` + db.AppClockAgeSQL("$2") + `, decided_by=$3, reason=$4
		WHERE id=$5 AND state='PENDING' AND kind='credential_reauth'
		RETURNING ` + approvalCols
	ap, err := scanApproval(tx.QueryRow(ctx, q,
		string(decision.State), age, decision.DecidedBy, decision.Reason, id,
	))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// Not PENDING any more, not this kind, or gone; nobody resolves it twice.
			return types.ApprovalRequest{}, ErrAlreadyDecided
		}
		return types.ApprovalRequest{}, fmt.Errorf("store: resolve reauth approval: %w", err)
	}

	if err := InsertAuditEventTx(ctx, tx, &ev); err != nil {
		return types.ApprovalRequest{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return types.ApprovalRequest{}, fmt.Errorf("store: commit reauth resolve: %w", err)
	}
	return ap, nil
}

// InsertAuditEventTx is InsertAuditEvent's body on a caller-supplied
// transaction: the same target cap, pinned lock-wait bound and advisory lock
// on the chain, so a row written this way links exactly as every other row
// does.
func InsertAuditEventTx(ctx context.Context, tx pgx.Tx, ev *types.AuditEvent) error {
	ev.Target = CapAuditTarget(ev.Target)
	dataJSON, err := json.Marshal(ev.Data)
	if err != nil {
		return fmt.Errorf("store: marshal audit data: %w", err)
	}
	if _, err := tx.Exec(ctx, db.AuditChainLockTimeoutSQL()); err != nil {
		return fmt.Errorf("store: bound audit chain lock wait: %w", err)
	}
	if _, err := tx.Exec(ctx, lockAuditChainSQL, db.AuditChainLockKey); err != nil {
		return fmt.Errorf("store: lock audit chain (waited up to %s): %w", db.AuditChainLockTimeout, err)
	}
	const q = `
		SELECT COALESCE(prev_hash,''), COALESCE(row_hash,'')
		FROM audit_append($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`
	if err := tx.QueryRow(ctx, q,
		ev.ID, ev.Time, ev.RunID, string(ev.ActorType), ev.Actor, ev.Action,
		ev.Target, ev.Outcome, ev.SourceIP, dataJSON,
	).Scan(&ev.PrevHash, &ev.RowHash); err != nil {
		return fmt.Errorf("store: insert audit event: %w", err)
	}
	return nil
}
