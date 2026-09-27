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
// writes its credential.reauth.resolve audit row in ONE transaction.
//
// Why a transaction, when DecideApproval + Record would have compiled. This is
// the mint's own argument (internal/broker's b.mint, whose credential.mint row
// commits with the minted_jti burn): the state change is what unblocks a HELD
// credential request, and the audit row is the only durable record that a
// credential-bearing retry had a human act behind it. Committed separately,
// a crash between them leaves a run resuming on a refreshed credential with
// nothing in the trail saying who resolved it — invariant I7's exact failure.
// So the row moves only if its record lands: if the audit write fails or the
// commit does, the approval stays PENDING and the idempotent reconcile-on-read
// (internal/api's reconcileReauthOnRead) tries again on the next poll.
//
// It is NOT part of the Store interface, for the reason iface.go states about
// transactional surfaces and store.Pager's seam documents about test doubles:
// internal/api type-asserts for it, and a store without it simply does not
// resolve eagerly.
//
// The CAS is DecideApproval's own: WHERE state='PENDING', so a concurrent
// resolver (two captures, or a capture racing the reconcile-on-read) loses
// cleanly with ErrAlreadyDecided and writes no second audit row.
func (s PG) ResolveReauthApproval(ctx context.Context, id uuid.UUID, decision types.ApprovalDecision, ev types.AuditEvent) (types.ApprovalRequest, error) {
	if ev.Action != "credential.reauth.resolve" {
		// This method exists for ONE transition. Accepting an arbitrary action
		// here would make it a general "decide anything and audit it however you
		// like" primitive, which is precisely the shape approval.Decide's own
		// attribution rules exist to prevent.
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
			// Not PENDING any more, not this kind, or gone. Either way nobody
			// resolves it twice — and the caller treats this as "already done".
			return types.ApprovalRequest{}, ErrAlreadyDecided
		}
		return types.ApprovalRequest{}, fmt.Errorf("store: resolve reauth approval: %w", err)
	}

	if err := insertAuditEventTx(ctx, tx, &ev); err != nil {
		return types.ApprovalRequest{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return types.ApprovalRequest{}, fmt.Errorf("store: commit reauth resolve: %w", err)
	}
	return ap, nil
}

// insertAuditEventTx is InsertAuditEvent's body on a caller-supplied
// transaction: the same target cap, the same pinned lock-wait bound and the same
// advisory lock on the chain, so a row written this way links exactly as every
// other row does.
//
// Taking the chain lock inside a caller's transaction is safe and is what the
// broker's own in-tx audit write does; it is also why the lock wait is bounded
// BEFORE the ask rather than left to default_transaction_isolation and hope.
func insertAuditEventTx(ctx context.Context, tx pgx.Tx, ev *types.AuditEvent) error {
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
		INSERT INTO audit_events
			(` + auditCols + `)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		RETURNING COALESCE(prev_hash,''), COALESCE(row_hash,'')`
	if err := tx.QueryRow(ctx, q,
		ev.ID, ev.Time, ev.RunID, string(ev.ActorType), ev.Actor, ev.Action,
		ev.Target, ev.Outcome, ev.SourceIP, dataJSON,
	).Scan(&ev.PrevHash, &ev.RowHash); err != nil {
		return fmt.Errorf("store: insert audit event: %w", err)
	}
	return nil
}
