// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// This file freezes the Runs board's read as v0.10.2 shipped it: one ordered query, a sponsor arm written as an
// OR, and the age window as one clause. The arms test compares every page of the current read against it, so it
// calls neither baseWhere nor ageWindowSQL. It may use the helpers v0.10.2's text used (statusSQL,
// killedVisibleSQL, runOrderSQL, sponsoredServicePrincipalsSQL, RunUnitScope.sql and the ones they use), which the
// current read does not change.

// baseWhere builds the owner/status/workspace/query predicate, before either
// visibility carve-out (age window, killed-hide default) narrows further.
// CountHiddenRuns shares this exact clause with ListRunsFiltered's WHERE, so
// "hidden" always means "matched, hidden only by a carve-out".
func baseWhereV0102(f RunFilter, nonTerminal string, args *[]any) string {
	var clauses []string
	if f.Scope != nil {
		clauses = append(clauses, f.Scope.sql(args))
	}
	if f.Owner != "" {
		owner := bindArg(args, f.Owner)
		if f.WithSponsored {
			clauses = append(clauses, "(created_by = "+owner+" OR created_by IN "+sponsoredServicePrincipalsSQL(owner)+")")
		} else {
			clauses = append(clauses, "created_by = "+owner)
		}
	}
	if len(f.Statuses) > 0 {
		var arms []string
		for _, st := range f.Statuses {
			if expr := statusSQL(st, nonTerminal); expr != "" {
				arms = append(arms, expr)
			}
		}
		if len(arms) > 0 {
			clauses = append(clauses, "("+strings.Join(arms, " OR ")+")")
		}
	}
	if f.NoEnd {
		// A QUEUED run's ends_at is empty because it has not started, not because nothing will end it:
		// its start writes the end. So "No end" holds only the runs that hold a slot, and not one whose
		// lease already ended (lost_reason 'ended', as runLiveSQL reads it).
		clauses = append(clauses, "(ends_at IS NULL AND state = ANY("+bindArg(args, slotHoldingStateNames())+") AND lost_reason IS DISTINCT FROM 'ended')")
	}
	if f.ServiceOnly {
		clauses = append(clauses, "service IS NOT NULL")
	}
	if f.Workspace != "" {
		clauses = append(clauses, "COALESCE(NULLIF(repo,''), workspace_path) = "+bindArg(args, f.Workspace))
	}
	if f.Query != "" {
		p := bindArg(args, escapeLikePattern(f.Query))
		clauses = append(clauses, fmt.Sprintf(
			"(title ILIKE %[1]s ESCAPE '\\' OR task ILIKE %[1]s ESCAPE '\\' OR repo ILIKE %[1]s ESCAPE '\\' OR created_by ILIKE %[1]s ESCAPE '\\')",
			p))
	}
	return strings.Join(clauses, " AND ")
}

// ListRunsFilteredV0102 is ListRunsFiltered as v0.10.2 read it, as the ids in page order.
func ListRunsFilteredV0102(ctx context.Context, pool *pgxpool.Pool, f RunFilter, p Page) ([]uuid.UUID, error) {
	var args []any
	nonTerminal := bindArg(&args, nonTerminalStateList())
	where := baseWhereV0102(f, nonTerminal, &args)
	secsParam := bindArg(&args, endedWithinSecs(f.EndedWithin))
	includeParam := bindArg(&args, f.IncludeKilled)
	killedSecsParam := bindArg(&args, int(killedVisibleFor.Seconds()))
	ageWindow := fmt.Sprintf("(%s OR %s::int IS NULL OR %s >= now() - make_interval(secs => %s::int))",
		fmt.Sprintf(runLiveSQL, nonTerminal), secsParam, runEndTimeSQL, secsParam)
	clauses := []string{ageWindow, killedVisibleSQL(includeParam, killedSecsParam)}
	if where != "" {
		clauses = append([]string{where}, clauses...)
	}
	q := `SELECT ` + runCols + ` FROM agent_runs WHERE ` + strings.Join(clauses, " AND ") +
		` ORDER BY ` + runOrderSQL(nonTerminal)
	q, args = p.appendTo(q, args)
	runs, err := collect(ctx, pool, "list", "runs filtered", q, args, scanRun)
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, len(runs))
	for i, r := range runs {
		ids[i] = r.ID
	}
	return ids, nil
}

// CountHiddenRunsV0102 is CountHiddenRuns as v0.10.2 read it.
func CountHiddenRunsV0102(ctx context.Context, pool *pgxpool.Pool, f RunFilter) (olderHidden, killedHidden int, err error) {
	var args []any
	nonTerminal := bindArg(&args, nonTerminalStateList())
	where := baseWhereV0102(f, nonTerminal, &args)
	secsParam := bindArg(&args, endedWithinSecs(f.EndedWithin))
	includeParam := bindArg(&args, f.IncludeKilled)
	killedSecsParam := bindArg(&args, int(killedVisibleFor.Seconds()))
	live := fmt.Sprintf(runLiveSQL, nonTerminal)

	// olderHidden: matches baseWhere, ended, outside the ended_within window.
	olderClause := fmt.Sprintf("NOT %s AND %s::int IS NOT NULL AND %s < now() - make_interval(secs => %s::int)",
		live, secsParam, runEndTimeSQL, secsParam)
	// killedHidden: matches baseWhere, KILLED, kept by the window itself, but
	// hidden by killedVisibleFor's default — moot once include_killed=1.
	killedClause := fmt.Sprintf(
		"state = 'KILLED' AND NOT %s::boolean AND (%s::int IS NULL OR %s >= now() - make_interval(secs => %s::int)) AND (%s IS NULL OR %s < now() - make_interval(secs => %s::int))",
		includeParam, secsParam, runEndTimeSQL, secsParam, runEndTimeSQL, runEndTimeSQL, killedSecsParam)

	q := `SELECT count(*) FILTER (WHERE ` + olderClause + `), count(*) FILTER (WHERE ` + killedClause + `) FROM agent_runs`
	if where != "" {
		q += ` WHERE ` + where
	}
	if err := pool.QueryRow(ctx, q, args...).Scan(&olderHidden, &killedHidden); err != nil {
		return 0, 0, fmt.Errorf("store: count hidden runs: %w", err)
	}
	return olderHidden, killedHidden, nil
}
