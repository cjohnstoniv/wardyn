// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"maps"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ModelUsageStore is deliberately not part of Store (like RunWatcherLeaser): widening Store would
// route these calls to the embedded nil interface of every test double. The api layer type-asserts
// it and skips a usage row, or answers 501, when a store lacks it; PG always has it.
type ModelUsageStore interface {
	// InsertModelUsage keeps one row per call id and reports whether this call stored it: a retried
	// POST or a replayed spool finds the id taken and stores nothing. evidence is "" for a row the
	// proxy posted itself, or the run's evidence source for a row a runner replayed.
	InsertModelUsage(ctx context.Context, runID uuid.UUID, u egress.ModelUsage, at time.Time, evidence string) (bool, error)
	// RunModelUsage sums a run's usage rows by vendor, host and model.
	RunModelUsage(ctx context.Context, runID uuid.UUID) (ModelUsageSummary, error)
	// RunOpaqueTunnels counts a run's tunnels to a model host that could not be opened, whose calls are not
	// counted: the egress.allow rows marked model_tunnel, or its llm.scan.bypass rows when there are more.
	RunOpaqueTunnels(ctx context.Context, runID uuid.UUID) (int64, error)
}

var _ ModelUsageStore = PG{}

// ModelUsageGroup is the sum of one run's rows for one vendor, host and model. Tokens are as the
// provider reported them: Anthropic and Bedrock Input excludes cache tokens, OpenAI Input includes
// cached tokens, so the fields are kept apart and never folded together here.
type ModelUsageGroup struct {
	Vendor, Host, Model string
	Input, Output       int64
	CacheRead           int64
	CacheWrite          int64
	Reasoning           int64
	Calls               int64
	UncountedCalls      int64
	PartialCalls        int64
	UncountedReasons    map[string]int64
}

// UncountedReasonOutOfRange is the uncounted reason of the calls of a group whose token counts add up past an
// int64: no exact figure exists to show, so the calls are shown as not counted instead of a cut or wrapped one.
const UncountedReasonOutOfRange = "count_out_of_range"

// Uncounted is g with every call it counted moved to not counted, reason UncountedReasonOutOfRange, and no
// tokens. g's reasons map is copied, not changed.
func (g ModelUsageGroup) Uncounted() ModelUsageGroup {
	reasons := make(map[string]int64, len(g.UncountedReasons)+1)
	maps.Copy(reasons, g.UncountedReasons)
	if n := g.Calls - g.UncountedCalls; n > 0 {
		reasons[UncountedReasonOutOfRange] += n
	}
	return ModelUsageGroup{Vendor: g.Vendor, Host: g.Host, Model: g.Model, Calls: g.Calls, UncountedCalls: g.Calls, UncountedReasons: reasons}
}

// usageSumsSQL is a group's five token sums over the rows aliased u, then whether any of them is past bigint.
// Postgres adds bigint as numeric, exactly, and casting a larger sum back fails the whole read: each sum is cut to
// bigint before its cast, and a group past it is read as Uncounted, never with the cut figure.
const usageSumsSQL = `LEAST(COALESCE(sum(u.input_tokens),0), 9223372036854775807)::bigint,
		LEAST(COALESCE(sum(u.output_tokens),0), 9223372036854775807)::bigint,
		LEAST(COALESCE(sum(u.cache_read_tokens),0), 9223372036854775807)::bigint,
		LEAST(COALESCE(sum(u.cache_write_tokens),0), 9223372036854775807)::bigint,
		LEAST(COALESCE(sum(u.reasoning_tokens),0), 9223372036854775807)::bigint,
		GREATEST(COALESCE(sum(u.input_tokens),0), COALESCE(sum(u.output_tokens),0), COALESCE(sum(u.cache_read_tokens),0),
			COALESCE(sum(u.cache_write_tokens),0), COALESCE(sum(u.reasoning_tokens),0)) > 9223372036854775807`

// ModelUsageSummary is a run's usage rows grouped, and the time of the newest row (nil with none).
type ModelUsageSummary struct {
	Groups    []ModelUsageGroup
	UpdatedAt *time.Time
}

const insertModelUsageSQL = `INSERT INTO run_model_usage
	(call_id, run_id, at, vendor, host, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
	 reasoning_tokens, state, reason, evidence_source)
	VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
	ON CONFLICT (call_id) DO NOTHING`

// ValidModelUsage is the stored shape's floor: a call id to key on, a state the parser's closed set
// names, text within the lengths the proxy cuts to, and no negative count.
func ValidModelUsage(u egress.ModelUsage) bool {
	switch u.State {
	case "counted", "partial", "uncounted":
	default:
		return false
	}
	if len(u.Vendor) > 32 || len(u.Host) > 256 || len(u.Model) > 256 || len(u.Reason) > 64 {
		return false
	}
	return u.CallID != uuid.Nil && u.Input >= 0 && u.Output >= 0 && u.CacheRead >= 0 && u.CacheWrite >= 0 && u.Reasoning >= 0
}

// insertModelUsageTx stores u inside tx (a runner's replay) and reports whether it was new.
func insertModelUsageTx(ctx context.Context, tx pgx.Tx, runID uuid.UUID, u egress.ModelUsage, at time.Time, evidence types.RunEvidenceSource) (bool, error) {
	if !ValidModelUsage(u) {
		return false, ErrRunnerSpoolRefused
	}
	tag, err := tx.Exec(ctx, insertModelUsageSQL, u.CallID, runID, at, u.Vendor, u.Host, u.Model, u.Input, u.Output,
		u.CacheRead, u.CacheWrite, u.Reasoning, u.State, u.Reason, string(evidence))
	if err != nil {
		return false, fmt.Errorf("store: insert model usage: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// runnerRowEvents is the audit events a replayed spool row becomes, and whether the row stored a new model
// usage row. A row carrying a model call's token counts is not an egress decision: it is stored in the same
// tx (evidence runner_asserted, like its siblings) and writes no audit row. With drop (PG.ModelUsageOff) the
// row is acknowledged and stored nowhere. A call id already stored stores nothing and reports false.
func runnerRowEvents(ctx context.Context, tx pgx.Tx, runnerID, runID uuid.UUID, r runnerspool.Record, drop bool) ([]types.AuditEvent, bool, error) {
	u := r.Decision.Usage
	if u == nil {
		events, err := runnerDecisionEvents(ctx, tx, runnerID, runID, r)
		return events, false, err
	}
	if drop {
		return nil, false, nil
	}
	at := r.Decision.Request.Time
	if at.IsZero() {
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&at); err != nil {
			return nil, false, err
		}
	}
	stored, err := insertModelUsageTx(ctx, tx, runID, *u, at, types.RunEvidenceRunnerAsserted)
	return nil, stored, err
}

func (s PG) InsertModelUsage(ctx context.Context, runID uuid.UUID, u egress.ModelUsage, at time.Time, evidence string) (bool, error) {
	if !ValidModelUsage(u) {
		return false, fmt.Errorf("store: model usage record is not valid")
	}
	tag, err := s.Pool.Exec(ctx, insertModelUsageSQL, u.CallID, runID, at, u.Vendor, u.Host, u.Model, u.Input, u.Output,
		u.CacheRead, u.CacheWrite, u.Reasoning, u.State, u.Reason, evidence)
	if err != nil {
		return false, fmt.Errorf("store: insert model usage: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (s PG) RunModelUsage(ctx context.Context, runID uuid.UUID) (ModelUsageSummary, error) {
	var out ModelUsageSummary
	rows, err := s.Pool.Query(ctx, `SELECT u.vendor, u.host, u.model, `+usageSumsSQL+`,
			count(*), count(*) FILTER (WHERE u.state='uncounted'), count(*) FILTER (WHERE u.state='partial'), max(u.at)
		FROM run_model_usage u WHERE u.run_id=$1 GROUP BY u.vendor, u.host, u.model ORDER BY u.vendor, u.host, u.model`, runID)
	if err != nil {
		return out, fmt.Errorf("store: read model usage: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var g ModelUsageGroup
		var newest time.Time
		var over bool
		if err := rows.Scan(&g.Vendor, &g.Host, &g.Model, &g.Input, &g.Output, &g.CacheRead, &g.CacheWrite, &g.Reasoning, &over,
			&g.Calls, &g.UncountedCalls, &g.PartialCalls, &newest); err != nil {
			return out, fmt.Errorf("store: scan model usage: %w", err)
		}
		if over {
			g = g.Uncounted()
		}
		if out.UpdatedAt == nil || newest.After(*out.UpdatedAt) {
			t := newest
			out.UpdatedAt = &t
		}
		out.Groups = append(out.Groups, g)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("store: read model usage: %w", err)
	}
	reasons, err := s.Pool.Query(ctx, `SELECT vendor, host, model, reason, count(*)
		FROM run_model_usage WHERE run_id=$1 AND state='uncounted' GROUP BY vendor, host, model, reason`, runID)
	if err != nil {
		return out, fmt.Errorf("store: read model usage reasons: %w", err)
	}
	defer reasons.Close()
	for reasons.Next() {
		var vendor, host, model, reason string
		var n int64
		if err := reasons.Scan(&vendor, &host, &model, &reason, &n); err != nil {
			return out, fmt.Errorf("store: scan model usage reasons: %w", err)
		}
		for i := range out.Groups {
			if g := &out.Groups[i]; g.Vendor == vendor && g.Host == host && g.Model == model {
				if g.UncountedReasons == nil {
					g.UncountedReasons = map[string]int64{}
				}
				g.UncountedReasons[reason] += n
			}
		}
	}
	if err := reasons.Err(); err != nil {
		return out, fmt.Errorf("store: read model usage reasons: %w", err)
	}
	return out, nil
}

func (s PG) RunOpaqueTunnels(ctx context.Context, runID uuid.UUID) (int64, error) {
	var n int64
	// Either row marks a tunnel: the allow the proxy marks model_tunnel, or the bypass row an older runner (or a
	// run with content inspection on) writes. One tunnel can write both, so the larger count, not the sum.
	if err := s.Pool.QueryRow(ctx, `SELECT GREATEST(
		count(*) FILTER (WHERE action='egress.allow' AND data->>'model_tunnel'='true'),
		count(*) FILTER (WHERE action='llm.scan.bypass'))
		FROM audit_events WHERE run_id=$1 AND action IN ('egress.allow','llm.scan.bypass')`, runID).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count opaque tunnels: %w", err)
	}
	return n, nil
}
