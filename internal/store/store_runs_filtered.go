// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The landing-page GET /runs scoping surface (#1197 L1a): RunFilter, the
// filtered list read and its companion hidden-row counts. Attention
// projection (kind/by/pending) is L1b's job and does not live here — this
// file only scopes, windows and orders the bare AgentRun rows.
package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// killedVisibleFor is how long a KILLED run stays visible on the landing page
// by default: a run that ended KILLED longer ago than this is hidden
// unless the caller opts in with include_killed=1. Fixed, not configurable —
// it is a display default, not a retention policy.
const killedVisibleFor = 24 * time.Hour

// RunFilter narrows ListRunsFiltered/CountHiddenRuns to the landing page's
// opt-in contract. The zero value matches every row (no owner scope, no
// status filter, no age window, killed rows still subject to killedVisibleFor).
type RunFilter struct {
	// Owner exact-matches created_by. Empty = every creator. The api layer
	// resolves ?owner=me|all (and the view=user owner force) to this before
	// calling down — RunFilter itself has no opinion on who "me" is.
	Owner string
	// Statuses is a subset of "active", "ended", "failed", "killed" (OR'd
	// together); empty = no status filter. "needs" reads an attention
	// projection this lane does not compute, and is rejected at the api
	// layer, never reaches here.
	Statuses []string
	// EndedWithin bounds ended rows' age; <= 0 means "all" (unbounded). Live
	// rows are NEVER windowed by this, regardless of value.
	EndedWithin time.Duration
	// IncludeKilled disables killedVisibleFor's default hide.
	IncludeKilled bool
	// Workspace exact-matches COALESCE(NULLIF(repo,''), workspace_path).
	Workspace string
	// Query ILIKE-searches title/task/repo/created_by. Raw (unescaped) — the
	// store escapes '%', '_' and '\' itself, so a caller never has to know the
	// wire format of a LIKE pattern.
	Query string
}

// runLiveSQL is TRUE for a row the landing page's end-time window must never
// apply to: not terminal (types.NonTerminalRunStates, the POSITIVE list — see
// that type's own doc for why this reads the non-terminal set rather than
// hand-copying its terminal complement) and not lease-ended. This is the same
// "active" predicate the status filter's "active" value selects.
const runLiveSQL = `(state = ANY(%[1]s) AND lost_reason IS DISTINCT FROM 'ended')`

// runEndTimeSQL corrects a lease-ended run's end time: it stays RUNNING until
// the ended-run grace stops it, so ITS end time is lost_at (the lease end),
// never ended_at (which that later grace stop would set, moving "ended at
// its end time" to the wrong moment). Every other terminal row's end time is
// ended_at.
const runEndTimeSQL = `CASE WHEN lost_reason = 'ended' THEN lost_at ELSE ended_at END`

// nonTerminalStateList renders types.NonTerminalRunStates as the []string the
// driver binds to state = ANY($n) — computed once per call so a state added
// to the enum reaches every query built here without a second copy of the set.
func nonTerminalStateList() []string {
	out := make([]string, len(types.NonTerminalRunStates))
	for i, st := range types.NonTerminalRunStates {
		out[i] = string(st)
	}
	return out
}

// bindArg appends v to *args and returns its positional placeholder — the
// same growth Page.appendTo uses, pulled out here because this file builds
// several independent clauses that must all agree on one running count.
func bindArg(args *[]any, v any) string {
	*args = append(*args, v)
	return fmt.Sprintf("$%d", len(*args))
}

// escapeLikePattern escapes '\', '%' and '_' (in that order — '\' first, so
// the escapes it introduces are not themselves re-escaped) and wraps the
// result for a substring ILIKE, so a caller's literal '%' or '_' in ?q= is
// matched literally rather than as a LIKE wildcard.
func escapeLikePattern(q string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(q) + "%"
}

// statusSQL renders one RunFilter.Statuses value as a boolean SQL expression,
// or "" for an unrecognised one (the api layer validates the enum before this
// is ever reached, so "" here would only mean a caller bypassed that gate —
// filterStatusClauses drops it rather than building an always-false OR arm).
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

// baseWhere builds the owner/status/workspace/query predicate — the part of
// RunFilter that says WHICH rows the caller asked for, before either
// visibility carve-out (the age window, the killed-hide default) narrows that
// further. CountHiddenRuns shares this exact clause with ListRunsFiltered's
// WHERE, so "hidden" always means "matched what was asked, hidden only by a
// carve-out" — never a row the filter itself would not have shown.
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

// ageWindowSQL is TRUE for a row the ended_within window keeps: every live
// row (never windowed) plus an ended row whose end time falls inside the
// window. secsParam is a nullable int placeholder — NULL means "all" (no
// window at all).
func ageWindowSQL(nonTerminal, secsParam string) string {
	return fmt.Sprintf("(%s OR %s::int IS NULL OR %s >= now() - make_interval(secs => %s::int))",
		fmt.Sprintf(runLiveSQL, nonTerminal), secsParam, runEndTimeSQL, secsParam)
}

// killedVisibleSQL is TRUE for a row killedVisibleFor's default does not hide:
// anything but a KILLED row, or include_killed=1, or a KILLED row that ended
// within killedVisibleFor (killedSecsParam). A KILLED row with no ended_at
// (should not happen past migration 0091 — every terminal transition stamps
// it — but a defensive read, not an assumed invariant) is treated as too old
// to show.
func killedVisibleSQL(includeParam, killedSecsParam string) string {
	return fmt.Sprintf("(state <> 'KILLED' OR %s::boolean OR (%s IS NOT NULL AND %s >= now() - make_interval(secs => %s::int)))",
		includeParam, runEndTimeSQL, runEndTimeSQL, killedSecsParam)
}

// runOrderSQL is the landing page's order — "needs first" is a client-side
// sectioning of this same live set, not a separate server order: every live
// row before every ended row, live rows by created_at DESC, ended rows by
// their end time DESC, id DESC as the final tie-break so two rows with an
// identical timestamp still sort deterministically across pages.
func runOrderSQL(nonTerminal string) string {
	live := fmt.Sprintf(runLiveSQL, nonTerminal)
	return fmt.Sprintf("NOT %[1]s ASC, CASE WHEN %[1]s THEN created_at ELSE %[2]s END DESC, id DESC", live, runEndTimeSQL)
}

// endedWithinSecs renders RunFilter.EndedWithin as the nullable-int arg
// ageWindowSQL/CountHiddenRuns bind: nil ("all") for <= 0, else whole seconds.
func endedWithinSecs(d time.Duration) *int {
	if d <= 0 {
		return nil
	}
	secs := int(d.Seconds())
	return &secs
}

// RunsFilteredPager is the landing-page scoping surface for GET /runs (#1197
// L1a). Kept out of Pager and Store for the reason every capability interface
// in this package is: widening either would silently route a test double's
// embedded-but-not-overridden method to the wrong (unscoped) behaviour. An
// absent implementation is handled fail-CLOSED at the api-layer call site —
// the same posture as RunsByCreatorPager, and for the same reason: this is an
// ownership/visibility-scoped read, and a silent unscoped fallback would leak
// every run to a caller the filter was supposed to narrow.
type RunsFilteredPager interface {
	ListRunsFiltered(ctx context.Context, f RunFilter, p Page) ([]types.AgentRun, error)
	// CountHiddenRuns answers the two X-Wardyn-Hidden-* headers: how many rows
	// baseWhere(f) matches but the ended_within window excludes (olderHidden),
	// and how many more killedVisibleFor's default excludes on top of that
	// (killedHidden). Both are computed by one query (one FILTER'd count each)
	// so a caller never pays for two round trips per page.
	CountHiddenRuns(ctx context.Context, f RunFilter) (olderHidden, killedHidden int, err error)
}

// Compile-time assertion: PG satisfies RunsFilteredPager.
var _ RunsFilteredPager = PG{}

// ListRunsFiltered is ListRunsPage narrowed and reordered per f. The
// owner-scoped read rides the pre-existing agent_runs_created_by_idx
// (EXPLAIN ANALYZE against a 200k-row seed: a bitmap index scan there, then
// an in-memory filter and sort); an org-wide read is a parallel sequential
// scan with a top-N heapsort. Both are linear in the caller's history, which
// the spec accepted — see migration 0091's own doc for why no ended_at index
// backs the CASE-expression ordering/window this function builds.
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

	// olderHidden: matches baseWhere, is an ended row, and falls outside the
	// (bounded) ended_within window.
	olderClause := fmt.Sprintf("NOT %s AND %s::int IS NOT NULL AND %s < now() - make_interval(secs => %s::int)",
		live, secsParam, runEndTimeSQL, secsParam)
	// killedHidden: matches baseWhere, is a KILLED row the window itself would
	// keep (inside it, or the window is "all"), but killedVisibleFor's default
	// still hides it — moot (always 0) once include_killed=1, since nothing is
	// hidden by that rule then.
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
