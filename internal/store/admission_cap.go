// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Writes decided by a read: a count-then-insert cap (approvals per run, live API tokens and SSH keys
// per person) and the launch preset version floor. The writer takes its scope's transaction-scoped
// advisory lock first and makes that read a later statement of the same transaction, so its snapshot
// follows the lock and a concurrent writer of the same scope waits for this one's commit instead of
// reading the same count or floor.
package store

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/notify"
)

// ErrApprovalRunCap, ErrAPITokenCap and ErrSSHKeyCap refuse a capped insert whose scope already holds
// the cap WithAdmissionCap set.
var (
	ErrApprovalRunCap = errors.New("store: the run holds its approval cap")
	ErrAPITokenCap    = errors.New("store: the owner holds the live API token cap")
	ErrSSHKeyCap      = errors.New("store: the owner holds the SSH key cap")
)

var errAdmissionOutsideTx = errors.New("store: a capped insert must run in a transaction")

type admissionCapKey struct{}

// WithAdmissionCap makes the insert writers that honour it (CreateApproval for the run, every API
// token insert and AddSSHKey for the owner) count again under the scope's lock, in their own
// transaction, and refuse once limit rows are held. A handler's count before the write is a
// pre-check; this count is the one a concurrent writer cannot pass.
func WithAdmissionCap(ctx context.Context, limit int) context.Context {
	return context.WithValue(ctx, admissionCapKey{}, limit)
}

// lockScope takes class's transaction-scoped advisory lock for key, held to commit. A crc32
// collision only serializes two keys' writers.
func lockScope(ctx context.Context, q Querier, class int32, key string) error {
	if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock($1, $2)`, class, int32(crc32.ChecksumIEEE([]byte(key)))); err != nil {
		return fmt.Errorf("store: take the write's scope lock: %w", err)
	}
	return nil
}

// admitUnderCap is a capped writer's step before its insert on q: with a cap on ctx, it takes class's
// lock for scope and counts with countSQL ($1 = scope), returning full at the cap. Without one it
// takes nothing.
func admitUnderCap(ctx context.Context, q queryRower, class int32, scope any, countSQL string, full error) error {
	limit, ok := ctx.Value(admissionCapKey{}).(int)
	if !ok {
		return nil
	}
	tx, ok := q.(pgx.Tx)
	if !ok {
		return errAdmissionOutsideTx
	}
	if err := lockScope(ctx, tx, class, fmt.Sprint(scope)); err != nil {
		return err
	}
	var n int
	if err := tx.QueryRow(ctx, countSQL, scope).Scan(&n); err != nil {
		return fmt.Errorf("store: count under the cap's lock: %w", err)
	}
	if n >= limit {
		return full
	}
	return nil
}

// admitApproval is what an approval insert takes in tx before its row, returning the run's leaf
// governance profile. With notifications on: the run's notification budget lock, and the profile, read
// in this transaction so a concurrent profile change cannot route the approval by stale data. With a
// cap on ctx: the run's cap lock and its count of approvals in any state. Lock order: the budget lock,
// then the cap lock, then rows.
func admitApproval(ctx context.Context, tx pgx.Tx, runID uuid.UUID) (*uuid.UUID, error) {
	var profileID *uuid.UUID
	if notify.Enabled() {
		if _, err := tx.Exec(ctx, notify.BudgetLockSQL, runID.String()); err != nil {
			return nil, fmt.Errorf("store: lock the run's notification budget: %w", err)
		}
		if err := tx.QueryRow(ctx, notify.UnitSQL, runID).Scan(&profileID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("store: read run profile: %w", err)
		}
	}
	return profileID, admitUnderCap(ctx, tx, db.ApprovalRunCapLockClass, runID, `SELECT count(*) FROM approvals WHERE run_id = $1`, ErrApprovalRunCap)
}
