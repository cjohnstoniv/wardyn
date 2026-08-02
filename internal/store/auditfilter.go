// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

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
// Note actor is NOT filterable: for an agent event it IS the per-run SPIFFE ID,
// so ?actor= would merely restate ?run_id=. ActorType (human/agent/system) is the
// question that actually has more than one answer today.
type AuditFilter struct {
	Since        time.Time       // events at or after this instant
	Until        time.Time       // events strictly before this instant
	Action       string          // exact action, e.g. "run.kill"
	ActionPrefix string          // action family, e.g. "credential." or "egress."
	ActorType    types.ActorType // human / agent / system
	Outcome      string          // success / failure / warn
}

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
	case f.ActorType != "" && ev.ActorType != f.ActorType:
		return false
	case f.Outcome != "" && ev.Outcome != f.Outcome:
		return false
	}
	return true
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
	if f.ActorType != "" {
		add("actor_type = $%d", string(f.ActorType))
	}
	if f.Outcome != "" {
		add("outcome = $%d", f.Outcome)
	}
	return clauses, args
}

// QueryAuditEventsFilteredPage returns audit events narrowed by f, bounded by p.
// A non-nil runID scopes to one run and keeps that trail's chronological seq ASC
// order (the per-run contract in docs/sdk.md); nil is the newest-first global
// feed. Only a request that actually sets a predicate takes this path — an
// unfiltered read stays on the two plain queries above, whose plans are unchanged.
//
// No new index: 0001 covers (time) and 0017 (action, seq DESC); a bounded LIMIT
// query index-scans the pkey backward and filters, which is what the caps are for.
func (s PG) QueryAuditEventsFilteredPage(ctx context.Context, runID *uuid.UUID, f AuditFilter, p Page) ([]types.AuditEvent, error) {
	q := `
		SELECT id, time, run_id, actor_type, actor, action, target, outcome, source_ip, data
		FROM audit_events`
	var args []any
	var clauses []string
	if runID != nil {
		args = append(args, *runID)
		clauses = append(clauses, fmt.Sprintf("run_id = $%d", len(args)))
	}
	extra, args := f.where(args)
	clauses = append(clauses, extra...)
	if len(clauses) > 0 {
		q += " WHERE " + strings.Join(clauses, " AND ")
	}
	if runID != nil {
		q += " ORDER BY seq ASC"
	} else {
		q += " ORDER BY seq DESC"
	}
	q, args = p.appendTo(q, args)
	return collect(ctx, s.Pool, "query", "audit events", q, args, scanAuditEvent)
}
