// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// UnitAuditReader reads the audit rows of the runs frozen to a set of governance units, newest first: a
// unit admin's audit feed. A run with no unit never matches. The feed also matches a row with no run when
// it is a listed governance row naming one of the units. Optional like Pager; the api serves no
// unit-scoped read from a store without it.
//
// The unit set is not an AuditFilter field: IsZero compares the filter with ==, which a slice cannot be.
type UnitAuditReader interface {
	// QueryAuditEventsForUnits is the unit feed narrowed by f, bounded by p.
	QueryAuditEventsForUnits(ctx context.Context, units []uuid.UUID, f AuditFilter, p Page) ([]types.AuditEvent, error)
}

var _ UnitAuditReader = PG{}

// QueryAuditEventsForUnits reads through agent_runs_unit_idx for the runs and audit_events' run_id index
// for their rows, and the governance arm (unitGovernanceActions) for the rows with no run. An actor filter
// reads the run rows alone: the view masks a governance row's actor, so matching on it would confirm a name.
func (s PG) QueryAuditEventsForUnits(ctx context.Context, units []uuid.UUID, f AuditFilter, p Page) ([]types.AuditEvent, error) {
	q, args := unitAuditQuery(units, f, f.Actor == "" && f.ActorAlt == "")
	q, args = p.appendTo(q, args)
	return collect(ctx, s.Pool, "query", "unit audit events", q, args, scanAuditEvent)
}

func unitAuditQuery(units []uuid.UUID, f AuditFilter, withGovernance bool) (string, []any) {
	first, args := unitAuditFirstClause(units, withGovernance)
	clauses := []string{first}
	extra, args := f.where(args)
	clauses = append(clauses, extra...)
	return `
		SELECT ` + auditCols + `
		FROM audit_events
		WHERE ` + strings.Join(clauses, " AND ") + `
		ORDER BY seq DESC`, args
}
