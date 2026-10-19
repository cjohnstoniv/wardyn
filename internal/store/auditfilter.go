// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// AuditFilter narrows the audit feed to answer the questions a bigger window
// never can — "what happened between 14:00 and 16:00", "every secret.write this
// quarter", "every failure" — where the answer is 40 events inside 200k. The
// zero value matches everything, so an unfiltered read keeps taking the plain
// QueryRecentAuditEventsPage / QueryAuditEventsPage path unchanged.
//
// It renders TWO ways from one declaration — SQL (where) for the Pager path and
// Go (Matches) for the fetch-all fallback — because a filter applied on only one
// of them returns silently UNFILTERED events on the other, which in a governance
// product is a quiet wrong answer. Keep the two in step; they are deliberately
// adjacent.
//
// Actor IS filterable: for a HUMAN event it is the operator/member principal
// (e.g. "alice@corp.example"), so ?actor= answers "everything developer X did" —
// the per-principal evidence a vendor/compliance question needs and that no wider
// window yields. (An earlier note dismissed actor as filterable because for an
// AGENT event it is the per-run SPIFFE ID and would merely restate ?run_id= — but
// that reasoning only ever considered agent events; a human's approvals, kills,
// and policy writes all carry their principal here, and that is exactly what the
// finding asks for.) The agent-side "everything under X's runs" view still needs
// the agent_runs.created_by join, which does not fit this table-local filter.
type AuditFilter struct {
	Since        time.Time       // events at or after this instant
	Until        time.Time       // events strictly before this instant
	Action       string          // exact action, e.g. "run.kill"
	ActionPrefix string          // action family, e.g. "credential." or "egress."
	Actor        string          // exact principal, e.g. "alice@corp.example" (human evidence)
	ActorType    types.ActorType // human / agent / system
	Outcome      string          // success / failure / warn
	Origin       string          // AuditOriginDevice or AuditOriginOrganisation
	// ActorAlt is a second stored actor that also matches Actor: the "subject:<id>"
	// a row holds for the person Actor names under WARDYN_AUDIT_SEAL=full. Rows
	// written before it was turned on keep the name, so both must match.
	ActorAlt string
	// Target is the exact target column, e.g. a person's principal for a person.*
	// row. TargetAlt is its second stored form, as ActorAlt is Actor's: the
	// "subject:<id>" a person-target row holds under WARDYN_AUDIT_SEAL=full.
	Target    string
	TargetAlt string
	// DataContains is a JSON object the row's data must contain (Postgres
	// `data @> $n::jsonb`): every key present with an equal value, objects
	// recursively. A string, not a map, so the filter stays comparable (IsZero).
	DataContains string
}

// The two AuditFilter.Origin values: rows a device forwarded, and rows the
// organisation wrote itself (FederatedDeviceID decides which is which).
const (
	AuditOriginDevice       = "device"
	AuditOriginOrganisation = "organisation"
)

// IsZero reports whether the filter narrows nothing.
func (f AuditFilter) IsZero() bool { return f == AuditFilter{} }

// Matches reports whether ev satisfies every set predicate.
func (f AuditFilter) Matches(ev types.AuditEvent) bool {
	switch {
	case !f.Since.IsZero() && ev.Time.Before(f.Since):
		return false
	case !f.Until.IsZero() && !ev.Time.Before(f.Until):
		return false
	case f.Action != "" && ev.Action != f.Action:
		return false
	case f.ActionPrefix != "" && !strings.HasPrefix(ev.Action, f.ActionPrefix):
		return false
	case f.Actor != "" && ev.Actor != f.Actor && (f.ActorAlt == "" || ev.Actor != f.ActorAlt):
		return false
	case f.Target != "" && ev.Target != f.Target && (f.TargetAlt == "" || ev.Target != f.TargetAlt || FederatedDeviceID(ev) != nil):
		return false
	case f.ActorType != "" && ev.ActorType != f.ActorType:
		return false
	case f.Outcome != "" && ev.Outcome != f.Outcome:
		return false
	case f.Origin != "" && (FederatedDeviceID(ev) != nil) != (f.Origin == AuditOriginDevice):
		return false
	case f.DataContains != "" && !dataContains(ev.Data, f.DataContains):
		return false
	}
	return true
}

// dataContains is `data @> want` in Go: want and data are JSON; an unparseable
// side contains nothing (Postgres would reject a malformed filter outright).
func dataContains(data []byte, want string) bool {
	var w, d any
	if json.Unmarshal([]byte(want), &w) != nil || json.Unmarshal(data, &d) != nil {
		return false
	}
	return jsonContains(d, w)
}

func jsonContains(have, want any) bool {
	switch w := want.(type) {
	case map[string]any:
		h, ok := have.(map[string]any)
		if !ok {
			return false
		}
		for k, wv := range w {
			if hv, ok := h[k]; !ok || !jsonContains(hv, wv) {
				return false
			}
		}
		return true
	case []any:
		h, ok := have.([]any)
		if !ok {
			return false
		}
		for _, wv := range w {
			found := false
			for _, hv := range h {
				if jsonContains(hv, wv) {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
		return true
	default:
		return have == want
	}
}

// Keep returns the events matching f, in order. A zero filter returns events
// unchanged (no copy).
func (f AuditFilter) Keep(events []types.AuditEvent) []types.AuditEvent {
	if f.IsZero() {
		return events
	}
	out := make([]types.AuditEvent, 0, len(events))
	for _, ev := range events {
		if f.Matches(ev) {
			out = append(out, ev)
		}
	}
	return out
}

// where renders f as SQL clauses ANDed onto the caller's existing ones, binding
// positional args after the len(args) already bound. Returns "" when f narrows
// nothing.
func (f AuditFilter) where(args []any) ([]string, []any) {
	var clauses []string
	add := func(expr string, v any) {
		args = append(args, v)
		clauses = append(clauses, fmt.Sprintf(expr, len(args)))
	}
	if !f.Since.IsZero() {
		add("time >= $%d", f.Since)
	}
	if !f.Until.IsZero() {
		add("time < $%d", f.Until)
	}
	if f.Action != "" {
		add("action = $%d", f.Action)
	}
	if f.ActionPrefix != "" {
		// starts_with, not LIKE: the prefix is caller-supplied and `%`/`_` are LIKE
		// wildcards, so LIKE would need escaping to mean what Matches means.
		add("starts_with(action, $%d)", f.ActionPrefix)
	}
	if f.Actor != "" {
		if f.ActorAlt != "" {
			args = append(args, f.Actor, f.ActorAlt)
			clauses = append(clauses, fmt.Sprintf("actor IN ($%d, $%d)", len(args)-1, len(args)))
		} else {
			add("actor = $%d", f.Actor)
		}
	}
	if f.Target != "" {
		if f.TargetAlt != "" {
			args = append(args, f.Target, f.TargetAlt)
			// TargetAlt (the sealed subject form) names the organisation's own rows
			// only: a forwarded row keeps its claimed target verbatim.
			clauses = append(clauses, fmt.Sprintf("(target = $%d OR (target = $%d AND NOT %s))", len(args)-1, len(args), federatedRowSQL))
		} else {
			add("target = $%d", f.Target)
		}
	}
	if f.ActorType != "" {
		add("actor_type = $%d", string(f.ActorType))
	}
	if f.Outcome != "" {
		add("outcome = $%d", f.Outcome)
	}
	if f.DataContains != "" {
		add("data @> $%d::jsonb", f.DataContains)
	}
	switch f.Origin {
	case AuditOriginDevice:
		clauses = append(clauses, federatedRowSQL)
	case AuditOriginOrganisation:
		clauses = append(clauses, "NOT "+federatedRowSQL)
	}
	return clauses, args
}

// QueryAuditEventsFilteredPage returns audit events narrowed by f, bounded by p.
// A non-nil runID scopes to one run and keeps that trail's chronological seq ASC
// order (the per-run contract in docs/sdk.md) unless p.Newest asks for its newest
// rows first; nil is the newest-first global feed. Only a request that actually
// sets a predicate takes this path — an unfiltered read stays on the two plain
// queries above, whose plans are unchanged.
//
// No new index: 0001 covers (time) and 0017 (action, seq DESC); a bounded LIMIT
// query index-scans the pkey backward and filters, which is what the caps are for.
// DataContains adds a per-row `data @>` test with no GIN index behind it; the
// planner walks the pkey or the (time) index and filters, and the LIMIT plus a
// Since window bound it (about 0.1 s over 2M rows in the #1449 review).
func (s PG) QueryAuditEventsFilteredPage(ctx context.Context, runID *uuid.UUID, f AuditFilter, p Page) ([]types.AuditEvent, error) {
	q, args := filteredAuditQuery(auditCols, runID, f, p.Newest, 0)
	q, args = p.appendTo(q, args)
	return collect(ctx, s.Pool, "query", "audit events", q, args, scanAuditEvent)
}

// filteredAuditQuery is the filtered audit read without its LIMIT: cols from audit_events narrowed by the
// run and f, in the order QueryAuditEventsFilteredPage documents (newest flips a run's trail to newest-first).
// A positive afterSeq continues past that row in the same direction (below it newest-first, above it for a
// run's trail read oldest-first).
func filteredAuditQuery(cols string, runID *uuid.UUID, f AuditFilter, newest bool, afterSeq int64) (string, []any) {
	q := `
		SELECT ` + cols + `
		FROM audit_events`
	var args []any
	var clauses []string
	if runID != nil {
		args = append(args, *runID)
		clauses = append(clauses, fmt.Sprintf("run_id = $%d", len(args)))
	}
	extra, args := f.where(args)
	clauses = append(clauses, extra...)
	dir, cmp := "DESC", "<"
	if runID != nil && !newest {
		dir, cmp = "ASC", ">"
	}
	if afterSeq > 0 {
		args = append(args, afterSeq)
		clauses = append(clauses, fmt.Sprintf("seq %s $%d", cmp, len(args)))
	}
	if len(clauses) > 0 {
		q += " WHERE " + strings.Join(clauses, " AND ")
	}
	return q + " ORDER BY seq " + dir, args
}

// AuditKeysetPager pages the filtered audit read by position, for the export. An OFFSET page re-walks every
// row before it, and a row written mid-export shifts every later OFFSET window by one, so the newest-first
// feed repeated a row at each page boundary. Optional like Pager: a store without it keeps the OFFSET loop.
type AuditKeysetPager interface {
	// QueryAuditEventsFilteredAfter is QueryAuditEventsFilteredPage for the rows past afterSeq, limited to
	// limit rows. afterSeq 0 starts at the first row. It also returns the seq of the last row returned,
	// the next call's afterSeq; the event type carries no seq, so the cursor travels beside it.
	QueryAuditEventsFilteredAfter(ctx context.Context, runID *uuid.UUID, f AuditFilter, afterSeq int64, limit int) ([]types.AuditEvent, int64, error)
}

var _ AuditKeysetPager = PG{}

// seqRow scans a row's trailing seq into *seq after the columns scanAuditEvent reads.
type seqRow struct {
	pgx.Row
	seq *int64
}

func (r seqRow) Scan(dest ...any) error { return r.Row.Scan(append(dest, r.seq)...) }

// QueryAuditEventsFilteredAfter walks seq, the table's primary order: each page is an index range scan
// from the cursor, so the export costs O(rows) however long it runs, and rows committed after it began sit
// beyond the cursor (newest-first) or past the last page, never inside one already sent.
func (s PG) QueryAuditEventsFilteredAfter(ctx context.Context, runID *uuid.UUID, f AuditFilter, afterSeq int64, limit int) ([]types.AuditEvent, int64, error) {
	q, args := filteredAuditQuery(auditCols+", seq", runID, f, false, afterSeq)
	q, args = Page{Limit: limit}.appendTo(q, args)
	last := afterSeq
	evs, err := collect(ctx, s.Pool, "query", "audit events", q, args, func(r pgx.Row) (types.AuditEvent, error) {
		return scanAuditEvent(seqRow{Row: r, seq: &last})
	})
	return evs, last, err
}

// RunAuditMatcher answers "does this run have ANY audit row matching f?" as an
// EXISTS, so the answer does not depend on how many other rows the run has or
// where the match sits in its trail — which a bounded, oldest-first
// QueryAuditEvents window cannot promise. A capability interface for the
// reason Pager gives.
type RunAuditMatcher interface {
	HasRunAuditEvent(ctx context.Context, runID uuid.UUID, f AuditFilter) (bool, error)
}

var _ RunAuditMatcher = PG{}

// HasRunAuditEvent reports whether runID has an audit row matching f.
func (s PG) HasRunAuditEvent(ctx context.Context, runID uuid.UUID, f AuditFilter) (bool, error) {
	clauses, args := f.where([]any{runID})
	q := `SELECT EXISTS (SELECT 1 FROM audit_events WHERE ` +
		strings.Join(append([]string{"run_id = $1"}, clauses...), " AND ") + `)`
	var found bool
	if err := s.Pool.QueryRow(ctx, q, args...).Scan(&found); err != nil {
		return false, fmt.Errorf("store: probe audit events: %w", err)
	}
	return found, nil
}
