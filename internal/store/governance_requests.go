// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// GovernanceRequestReader lists the held requests a caller may see: pending ones, and ones decided in the last day. Optional like Pager.
type GovernanceRequestReader interface {
	ListGovernanceRequests(ctx context.Context, kinds []string, decidedSince time.Time) ([]types.GovernanceChange, error)
}

// ElevationSweeper deletes memberships past their end, under the graph lock, and returns them.
type ElevationSweeper interface {
	SweepExpiredMemberships(ctx context.Context, now time.Time, limit int) ([]types.GovernanceMembership, error)
}

// ListGovernanceRequests is the changes of the request kinds that are pending or were decided at or after
// decidedSince, newest first, at most 500. A pending row past its TTL reads as expired.
func (s PG) ListGovernanceRequests(ctx context.Context, kinds []string, decidedSince time.Time) ([]types.GovernanceChange, error) {
	const stmt = `SELECT ` + governanceChangeColsRead + ` FROM governance_changes
		WHERE target_kind = ANY($1) AND (state = 'pending' OR decided_at >= $2)
		ORDER BY proposed_at DESC LIMIT 500`
	return collect(ctx, s.Pool, "list", "governance requests", stmt, []any{kinds, decidedSince}, scanGovernanceChange)
}

// SweepExpiredMemberships deletes at most limit memberships whose end is at or before now, in one
// transaction under the graph lock, and returns them. A row another transaction holds is left for the
// next pass.
func (s PG) SweepExpiredMemberships(ctx context.Context, now time.Time, limit int) ([]types.GovernanceMembership, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // a no-op after Commit
	if err := LockGovernanceGraph(ctx, tx); err != nil {
		return nil, err
	}
	out, err := collect(ctx, tx, "sweep", "expired governance memberships", `
		DELETE FROM governance_memberships WHERE id IN (
			SELECT id FROM governance_memberships WHERE expires_at <= $1 ORDER BY expires_at LIMIT $2 FOR UPDATE SKIP LOCKED)
		RETURNING `+governanceMembershipCols, []any{now, limit}, scanGovernanceMembership)
	if err != nil {
		return nil, err
	}
	return out, tx.Commit(ctx)
}

// DeleteExpiredMembershipAtKeyQ deletes the membership at one natural key on q when its end has passed,
// and returns it; nil when none was deleted. An approval clears an unswept row before it writes the new one.
func DeleteExpiredMembershipAtKeyQ(ctx context.Context, q Querier, unitID uuid.UUID, subjectType, subject string) (*types.GovernanceMembership, error) {
	m, err := scanGovernanceMembership(q.QueryRow(ctx, `DELETE FROM governance_memberships
		WHERE unit_id = $1 AND subject_type = $2 AND subject = $3 AND expires_at <= now()
		RETURNING `+governanceMembershipCols, unitID, subjectType, subject))
	switch {
	case errors.Is(err, ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("store: delete an expired governance membership: %w", err)
	}
	return &m, nil
}

// LastAppliedGovernanceChangeQ is the newest applied change at one target on q, by its decision; nil when
// none. Governance changes are never deleted, so it sees every past one.
func LastAppliedGovernanceChangeQ(ctx context.Context, q Querier, targetKind, targetKey string) (*types.GovernanceChange, error) {
	ch, err := scanGovernanceChange(q.QueryRow(ctx, `SELECT `+governanceChangeCols+` FROM governance_changes
		WHERE target_kind = $1 AND target_key = $2 AND state = 'applied'
		ORDER BY decided_at DESC LIMIT 1`, targetKind, targetKey))
	switch {
	case errors.Is(err, ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("store: read the last applied governance change: %w", err)
	}
	return &ch, nil
}
