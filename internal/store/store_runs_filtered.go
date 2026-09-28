// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Landing-page GET /runs scoping: RunFilter, the filtered list read, and
// hidden-row counts. Attention projection lives elsewhere; this file only
// scopes, windows and orders the bare AgentRun rows.
package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// killedVisibleFor: a KILLED run older than this is hidden unless
// include_killed=1. A display default, not a retention policy.
const killedVisibleFor = 24 * time.Hour

// RunFilter narrows ListRunsFiltered/CountHiddenRuns to the landing page's
// opt-in contract. Zero value matches every row (killed rows still subject
// to killedVisibleFor).
type RunFilter struct {
	// Owner exact-matches created_by; empty = every creator. The api layer
	// resolves ?owner=me|all before calling down.
	Owner string
	// Statuses is a subset of "active"/"ended"/"failed"/"killed", OR'd
	// together; empty = no filter. "needs" is rejected at the api layer.
	Statuses []string
	// EndedWithin bounds ended rows' age; <= 0 = unbounded. Live rows are
	// never windowed by this.
	EndedWithin time.Duration
	// IncludeKilled disables killedVisibleFor's default hide.
	IncludeKilled bool
	// Workspace exact-matches COALESCE(NULLIF(repo,''), workspace_path).
	Workspace string
	// Query ILIKE-searches title/task/repo/created_by. Raw — the store
	// escapes '%', '_' and '\' itself.
	Query string
}

// runLiveSQL is TRUE for a row the end-time window must never apply to: not
// terminal (types.NonTerminalRunStates) and not lease-ended. Same predicate
// the "active" status value selects.
const runLiveSQL = `(state = ANY(%[1]s) AND lost_reason IS DISTINCT FROM 'ended')`

// runEndTimeSQL corrects a lease-ended run's end time: it stays RUNNING
// until the ended-run grace stops it, so its end time is lost_at, never
// ended_at. Every other terminal row's end time is ended_at.
const runEndTimeSQL = `CASE WHEN lost_reason = 'ended' THEN lost_at ELSE ended_at END`

// nonTerminalStateList renders types.NonTerminalRunStates as the []string
// bound to state = ANY($n); computed once per call so a new enum value
// reaches every query here automatically.
func nonTerminalStateList() []string {
	out := make([]string, len(types.NonTerminalRunStates))
	for i, st := range types.NonTerminalRunStates {
		out[i] = string(st)
	}
	return out
}

// bindArg appends v and returns its $n placeholder, shared by every clause
// built here so they agree on one running count.
func bindArg(args *[]any, v any) string {
	*args = append(*args, v)
	return fmt.Sprintf("$%d", len(*args))
}

// escapeLikePattern escapes '\', '%', '_' in that order — backslash first,
// so its own escapes aren't re-escaped — and wraps for substring ILIKE.
func escapeLikePattern(q string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(q) + "%"
}

// statusSQL renders one status value as SQL, or "" if unrecognized (the api
// layer validates the enum; "" here means that gate was bypassed).
func statusSQL(status string, nonTerminal string) string {
	switch status {
	case "active":
		return fmt.Sprintf(runLiveSQL, nonTerminal)
	case "ended":
		return "NOT " + fmt.Sprintf(runLiveSQL, nonTerminal)
	case "failed":
		return "state = 'FAILED'"
	case "killed":
		return "state = 'KILLED'"
	default:
		return ""
	}
}

// baseWhere builds the owner/status/workspace/query predicate, before either
// visibility carve-out (age window, killed-hide default) narrows further.
// CountHiddenRuns shares this exact clause with ListRunsFiltered's WHERE, so
// "hidden" always means "matched, hidden only by a carve-out".
func baseWhere(f RunFilter, nonTerminal string, args *[]any) string {
	var clauses []string
	if f.Owner != "" {
		clauses = append(clauses, "created_by = "+bindArg(args, f.Owner))
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

// ageWindowSQL is TRUE for a row ended_within keeps: every live row plus an
// ended row whose end time falls inside the window. secsParam NULL = no
// window.
func ageWindowSQL(nonTerminal, secsParam string) string {
	return fmt.Sprintf("(%s OR %s::int IS NULL OR %s >= now() - make_interval(secs => %s::int))",
		fmt.Sprintf(runLiveSQL, nonTerminal), secsParam, runEndTimeSQL, secsParam)
}

// killedVisibleSQL is TRUE for a row killedVisibleFor's default doesn't
// hide: non-KILLED, or include_killed=1, or KILLED within killedSecsParam.
// A KILLED row with no ended_at (defensive; shouldn't happen post-migration
// 0092) is treated as too old to show.
func killedVisibleSQL(includeParam, killedSecsParam string) string {
	return fmt.Sprintf("(state <> 'KILLED' OR %s::boolean OR (%s IS NOT NULL AND %s >= now() - make_interval(secs => %s::int)))",
		includeParam, runEndTimeSQL, runEndTimeSQL, killedSecsParam)
}

// runOrderSQL: live rows before ended rows, live by created_at DESC, ended
// by end time DESC, id DESC as tie-break for deterministic paging.
func runOrderSQL(nonTerminal string) string {
	live := fmt.Sprintf(runLiveSQL, nonTerminal)
	return fmt.Sprintf("NOT %[1]s ASC, CASE WHEN %[1]s THEN created_at ELSE %[2]s END DESC, id DESC", live, runEndTimeSQL)
}

// endedWithinSecs renders EndedWithin as nil ("all") for <= 0, else whole
// seconds.
func endedWithinSecs(d time.Duration) *int {
	if d <= 0 {
		return nil
	}
	secs := int(d.Seconds())
	return &secs
}

// RunsFilteredPager is the landing-page scoping surface for GET /runs. Kept
// out of Pager/Store: widening either would silently route a test double's
// embedded method to the wrong (unscoped) behaviour. An absent
// implementation fails CLOSED at the api-layer call site, same as
// RunsByCreatorPager — a silent unscoped fallback would leak every run to a
// caller the filter was meant to narrow.
type RunsFilteredPager interface {
	ListRunsFiltered(ctx context.Context, f RunFilter, p Page) ([]types.AgentRun, error)
	// CountHiddenRuns answers the X-Wardyn-Hidden-* headers: rows baseWhere
	// matches but ended_within excludes (olderHidden), plus rows
	// killedVisibleFor additionally excludes (killedHidden) — one query.
	CountHiddenRuns(ctx context.Context, f RunFilter) (olderHidden, killedHidden int, err error)
}

// Compile-time assertion: PG satisfies RunsFilteredPager.
var _ RunsFilteredPager = PG{}

// ListRunsFiltered is ListRunsPage narrowed and reordered per f. Owner-
// scoped reads ride agent_runs_created_by_idx (bitmap scan + in-memory
// sort); an org-wide read is a parallel seq scan with top-N heapsort — both
// linear in the caller's history, which the spec accepted (no ended_at
// index backs this CASE-based order/window).
func (s PG) ListRunsFiltered(ctx context.Context, f RunFilter, p Page) ([]types.AgentRun, error) {
	var args []any
	nonTerminal := bindArg(&args, nonTerminalStateList())
	where := baseWhere(f, nonTerminal, &args)
	secsParam := bindArg(&args, endedWithinSecs(f.EndedWithin))
	includeParam := bindArg(&args, f.IncludeKilled)
	killedSecsParam := bindArg(&args, int(killedVisibleFor.Seconds()))
	clauses := []string{ageWindowSQL(nonTerminal, secsParam), killedVisibleSQL(includeParam, killedSecsParam)}
	if where != "" {
		clauses = append([]string{where}, clauses...)
	}
	q := `SELECT ` + runCols + ` FROM agent_runs WHERE ` + strings.Join(clauses, " AND ") +
		` ORDER BY ` + runOrderSQL(nonTerminal)
	q, args = p.appendTo(q, args)
	return collect(ctx, s.Pool, "list", "runs filtered", q, args, scanRun)
}

// CountHiddenRuns — see RunsFilteredPager.
func (s PG) CountHiddenRuns(ctx context.Context, f RunFilter) (olderHidden, killedHidden int, err error) {
	var args []any
	nonTerminal := bindArg(&args, nonTerminalStateList())
	where := baseWhere(f, nonTerminal, &args)
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
	if err := s.Pool.QueryRow(ctx, q, args...).Scan(&olderHidden, &killedHidden); err != nil {
		return 0, 0, fmt.Errorf("store: count hidden runs: %w", err)
	}
	return olderHidden, killedHidden, nil
}
