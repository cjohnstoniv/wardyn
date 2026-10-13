// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// DecidedAlwaysEgressPager is the read behind the leader-term heal of `always`
// egress verdicts (api.ReconcileWorkspaceEgressDecisions). A capability
// interface like ApprovalsForRunsPager, asserted on the approval service.
type DecidedAlwaysEgressPager interface {
	// ListDecidedAlwaysEgressPage returns up to limit egress_domain approvals in
	// state whose decision scope is `always`, newest request first, strictly
	// after the keyset (afterRequestedAt, afterID); a nil afterID starts at the
	// newest row. Each row carries only ID, RunID, Kind, RequestedScope, State,
	// RequestedAt, DecidedAt and DecisionScope: the reason and the other decided
	// columns are never read.
	ListDecidedAlwaysEgressPage(ctx context.Context, state types.ApprovalState, afterRequestedAt time.Time, afterID uuid.UUID, limit int) ([]types.ApprovalRequest, error)
}

var _ DecidedAlwaysEgressPager = PG{}

// decidedAlwaysEgressSQL is filtered in SQL to the rows the heal can replay, so
// a run-scoped decision is never sent, whatever its reason holds. The scope test
// is the literal 'always': types.ApprovalScope.Normalize maps nothing else to
// ScopeAlways. The existing requested_at indexes serve the order and the keyset
// bound, the rest is a row filter, so no new index is needed; id breaks a
// requested_at tie so the keyset neither repeats nor skips a row.
const decidedAlwaysEgressSQL = `
		SELECT id, run_id, kind, requested_scope, state, requested_at, decided_at, decision_scope
		FROM approvals
		WHERE state = $1 AND kind = 'egress_domain' AND decision_scope = 'always'`

// ListDecidedAlwaysEgressPage — see DecidedAlwaysEgressPager.
func (s PG) ListDecidedAlwaysEgressPage(ctx context.Context, state types.ApprovalState, afterRequestedAt time.Time, afterID uuid.UUID, limit int) ([]types.ApprovalRequest, error) {
	q, args := decidedAlwaysEgressSQL, []any{string(state)}
	if afterID != uuid.Nil {
		q += ` AND (requested_at, id) < ($2, $3)`
		args = append(args, afterRequestedAt, afterID)
	}
	q, args = Page{Limit: limit}.appendTo(q+` ORDER BY requested_at DESC, id DESC`, args)
	return collect(ctx, s.Pool, "list", "decided always-egress approvals", q, args, scanDecidedAlwaysEgress)
}

func scanDecidedAlwaysEgress(row pgx.Row) (types.ApprovalRequest, error) {
	var a types.ApprovalRequest
	var kind, state, decisionScope string
	var scopeRaw []byte
	if err := row.Scan(&a.ID, &a.RunID, &kind, &scopeRaw, &state, &a.RequestedAt, &a.DecidedAt, &decisionScope); err != nil {
		return a, err
	}
	a.RequestedScope = scopeRaw
	a.Kind, a.State, a.DecisionScope = types.ApprovalKind(kind), types.ApprovalState(state), types.ApprovalScope(decisionScope)
	return a, nil
}
