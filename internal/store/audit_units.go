// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// UnitAuditReader reads the audit rows of the runs frozen to a set of governance units, newest first: a
// unit admin's audit feed. A run with no unit never matches. The feed also matches a row with no run when
// it is a listed governance row naming one of the units; the export matches run rows only. Optional like
// Pager; the api serves no unit-scoped read from a store without it.
//
// The unit set is not an AuditFilter field: IsZero compares the filter with ==, which a slice cannot be.
type UnitAuditReader interface {
	// QueryAuditEventsForUnits is the unit feed narrowed by f, bounded by p.
	QueryAuditEventsForUnits(ctx context.Context, units []uuid.UUID, f AuditFilter, p Page) ([]types.AuditEvent, error)
	// QueryAuditEventsForUnitsAfter is its keyset twin for the export: the rows below afterSeq (0 starts at
	// the newest), at most limit, and the seq of the last row returned.
	QueryAuditEventsForUnitsAfter(ctx context.Context, units []uuid.UUID, f AuditFilter, afterSeq int64, limit int) ([]types.AuditEvent, int64, error)
}

var _ UnitAuditReader = PG{}

// QueryAuditEventsForUnits reads through agent_runs_unit_idx for the runs and audit_events' run_id index
// for their rows, and the governance arm (unitGovernanceActions) for the rows with no run.
func (s PG) QueryAuditEventsForUnits(ctx context.Context, units []uuid.UUID, f AuditFilter, p Page) ([]types.AuditEvent, error) {
	q, args := unitAuditQuery(auditCols, units, f, 0, true)
	q, args = p.appendTo(q, args)
	return collect(ctx, s.Pool, "query", "unit audit events", q, args, scanAuditEvent)
}

// QueryAuditEventsForUnitsAfter walks seq downward from the cursor, as QueryAuditEventsFilteredAfter does.
func (s PG) QueryAuditEventsForUnitsAfter(ctx context.Context, units []uuid.UUID, f AuditFilter, afterSeq int64, limit int) ([]types.AuditEvent, int64, error) {
	q, args := unitAuditQuery(auditCols+", seq", units, f, afterSeq, false)
	q, args = Page{Limit: limit}.appendTo(q, args)
	last := afterSeq
	evs, err := collect(ctx, s.Pool, "query", "unit audit events", q, args, func(r pgx.Row) (types.AuditEvent, error) {
		return scanAuditEvent(seqRow{Row: r, seq: &last})
	})
	return evs, last, err
}

func unitAuditQuery(cols string, units []uuid.UUID, f AuditFilter, afterSeq int64, withGovernance bool) (string, []any) {
	first, args := unitAuditFirstClause(units, withGovernance)
	clauses := []string{first}
	extra, args := f.where(args)
	clauses = append(clauses, extra...)
	if afterSeq > 0 {
		args = append(args, afterSeq)
		clauses = append(clauses, fmt.Sprintf("seq < $%d", len(args)))
	}
	return `
		SELECT ` + cols + `
		FROM audit_events
		WHERE ` + strings.Join(clauses, " AND ") + `
		ORDER BY seq DESC`, args
}
