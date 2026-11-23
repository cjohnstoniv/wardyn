// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// ReplayScope picks the runs a replay reads. Exactly one field is set: Units matches the runs frozen to
// any of those units (the caller expands a unit to itself plus its descendants), Owner matches the runs
// one person created.
type ReplayScope struct {
	Units []uuid.UUID
	Owner string
}

// RunHostDecision is one decision the host list made for one run at one host and port.
type RunHostDecision struct {
	RunID      uuid.UUID
	UnitID     *uuid.UUID
	Owner      string
	CreatedAt  time.Time // the run's, so a caller can order runs
	Host       string    // lower-cased and trimmed
	Port       int       // the port the audit row recorded in data.port; 0 when the row has none (older rows), so unknown
	Decision   string    // "allow" or "deny"
	RuleSource string    // one of the three egress.RuleSource* host-list values
}

// ReplayTotals says how many runs the scope held and how many the read looked at. Both leave out the
// excluded runs the read was given: RunsInScope is the scope's runs in the window that are not excluded, and
// RunsCounted is the most recent of those, up to the cap. A run counts whether or not it has a host-list row.
type ReplayTotals struct {
	RunsInScope int
	RunsCounted int
	Truncated   bool
}

// RunHostDecisionReader reads the host-list decisions of recent runs. Optional like UnitModelUsageReader:
// the api answers 501 from a store without it.
type RunHostDecisionReader interface {
	// RunHostDecisions returns one row per (run, host, port, decision, rule source) for the maxRuns most
	// recent runs in scope created in [since, until), counting only the three rule sources the host list
	// decides. Two attempts at one host on different ports are two rows: the proxy matches a port-qualified
	// entry by host and port.
	//
	// excludeRunIDs are runs that never count (the assistant's own runs). They are left out in SQL before
	// both the total and the recent-run cap, so an excluded run neither displaces an ordinary run from the
	// sample nor stays in RunsInScope or RunsCounted, whether or not it has a host-list row. The caller
	// builds the set (see LANE-TOOLS, simulate); nil or empty excludes nothing.
	RunHostDecisions(ctx context.Context, scope ReplayScope, since, until time.Time, maxRuns int, excludeRunIDs []uuid.UUID) ([]RunHostDecision, ReplayTotals, error)
}

var _ RunHostDecisionReader = PG{}

func (s PG) RunHostDecisions(ctx context.Context, scope ReplayScope, since, until time.Time, maxRuns int, excludeRunIDs []uuid.UUID) ([]RunHostDecision, ReplayTotals, error) {
	var where string
	var scopeArg any
	switch {
	case len(scope.Units) > 0 && scope.Owner == "":
		where, scopeArg = `r.unit_id = ANY($1)`, scope.Units
	case len(scope.Units) == 0 && scope.Owner != "":
		where, scopeArg = `r.created_by = $1`, scope.Owner
	default:
		return nil, ReplayTotals{}, errors.New("store: replay scope needs exactly one of units or owner")
	}
	if maxRuns < 1 {
		return nil, ReplayTotals{}, errors.New("store: replay run cap must be at least 1")
	}
	// A nil slice would reach SQL as NULL, and NOT (id = ANY(NULL)) is NULL, which drops every run.
	if excludeRunIDs == nil {
		excludeRunIDs = []uuid.UUID{}
	}
	// The one run filter both queries share, so the total and the sample can never disagree on it.
	runFilter := where + ` AND r.created_at >= $2 AND r.created_at < $3 AND NOT (r.id = ANY($4))`
	var totals ReplayTotals
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM agent_runs r WHERE `+runFilter,
		scopeArg, since, until, excludeRunIDs).Scan(&totals.RunsInScope); err != nil {
		return nil, ReplayTotals{}, fmt.Errorf("store: count replay runs: %w", err)
	}
	totals.RunsCounted = min(totals.RunsInScope, maxRuns)
	totals.Truncated = totals.RunsInScope > maxRuns

	rows, err := s.Pool.Query(ctx, `WITH scoped AS (
			SELECT r.id, r.unit_id, r.created_by, r.created_at FROM agent_runs r
			WHERE `+runFilter+`
			ORDER BY r.created_at DESC, r.id DESC LIMIT $5)
		SELECT DISTINCT s.id, s.unit_id, s.created_by, s.created_at,
			lower(btrim(e.target)) AS host,
			CASE WHEN e.data->>'port' ~ '^[0-9]{1,5}$' THEN (e.data->>'port')::int ELSE 0 END AS port,
			substring(e.action from 8) AS decision, e.data->>'rule_source' AS rule_source
		FROM scoped s JOIN audit_events e ON e.run_id = s.id
		WHERE e.action IN ('egress.allow','egress.deny') AND e.data->>'rule_source' = ANY($6)
		ORDER BY s.created_at, s.id, host, port, decision, rule_source`,
		scopeArg, since, until, excludeRunIDs, maxRuns,
		[]string{egress.RuleSourcePolicyAllowed, egress.RuleSourceDefaultDeny, egress.RuleSourcePolicyDenied})
	if err != nil {
		return nil, ReplayTotals{}, fmt.Errorf("store: read replay host decisions: %w", err)
	}
	defer rows.Close()
	var out []RunHostDecision
	for rows.Next() {
		var d RunHostDecision
		if err := rows.Scan(&d.RunID, &d.UnitID, &d.Owner, &d.CreatedAt, &d.Host, &d.Port, &d.Decision, &d.RuleSource); err != nil {
			return nil, ReplayTotals{}, fmt.Errorf("store: scan replay host decision: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, ReplayTotals{}, fmt.Errorf("store: read replay host decisions: %w", err)
	}
	return out, totals, nil
}
