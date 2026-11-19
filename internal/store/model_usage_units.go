// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// UnitModelUsageReader sums the usage of the runs frozen to units and created in [since, until). Optional
// like ModelUsageStore: the api answers 501 from a store without it. A run's whole usage counts in the
// window its run was created in, whenever each call was made.
type UnitModelUsageReader interface {
	// UnitModelUsage is each unit's usage rows grouped by vendor, host and model. A unit with none is absent.
	UnitModelUsage(ctx context.Context, units []uuid.UUID, since, until time.Time) (map[uuid.UUID]ModelUsageSummary, error)
	// UnitOpaqueTunnels counts each unit's runs' llm.scan.bypass audit rows. A unit with none is absent.
	UnitOpaqueTunnels(ctx context.Context, units []uuid.UUID, since, until time.Time) (map[uuid.UUID]int64, error)
}

var _ UnitModelUsageReader = PG{}

func (s PG) UnitModelUsage(ctx context.Context, units []uuid.UUID, since, until time.Time) (map[uuid.UUID]ModelUsageSummary, error) {
	out := map[uuid.UUID]ModelUsageSummary{}
	rows, err := s.Pool.Query(ctx, `SELECT r.unit_id, u.vendor, u.host, u.model, `+usageSumsSQL+`,
			count(*), count(*) FILTER (WHERE u.state='uncounted'), count(*) FILTER (WHERE u.state='partial'), max(u.at)
		FROM run_model_usage u JOIN agent_runs r ON r.id = u.run_id
		WHERE r.unit_id = ANY($1) AND r.created_at >= $2 AND r.created_at < $3
		GROUP BY r.unit_id, u.vendor, u.host, u.model ORDER BY r.unit_id, u.vendor, u.host, u.model`, units, since, until)
	if err != nil {
		return nil, fmt.Errorf("store: read unit model usage: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var unit uuid.UUID
		var g ModelUsageGroup
		var newest time.Time
		var over bool
		if err := rows.Scan(&unit, &g.Vendor, &g.Host, &g.Model, &g.Input, &g.Output, &g.CacheRead, &g.CacheWrite, &g.Reasoning, &over,
			&g.Calls, &g.UncountedCalls, &g.PartialCalls, &newest); err != nil {
			return nil, fmt.Errorf("store: scan unit model usage: %w", err)
		}
		if over {
			g = g.Uncounted()
		}
		sum := out[unit]
		if sum.UpdatedAt == nil || newest.After(*sum.UpdatedAt) {
			t := newest
			sum.UpdatedAt = &t
		}
		sum.Groups = append(sum.Groups, g)
		out[unit] = sum
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read unit model usage: %w", err)
	}
	return out, s.unitUncountedReasons(ctx, out, units, since, until)
}

// unitUncountedReasons adds to each group of sums why its uncounted calls were not counted.
func (s PG) unitUncountedReasons(ctx context.Context, sums map[uuid.UUID]ModelUsageSummary, units []uuid.UUID, since, until time.Time) error {
	reasons, err := s.Pool.Query(ctx, `SELECT r.unit_id, u.vendor, u.host, u.model, u.reason, count(*)
		FROM run_model_usage u JOIN agent_runs r ON r.id = u.run_id
		WHERE r.unit_id = ANY($1) AND r.created_at >= $2 AND r.created_at < $3 AND u.state='uncounted'
		GROUP BY r.unit_id, u.vendor, u.host, u.model, u.reason`, units, since, until)
	if err != nil {
		return fmt.Errorf("store: read unit model usage reasons: %w", err)
	}
	defer reasons.Close()
	for reasons.Next() {
		var unit uuid.UUID
		var vendor, host, model, reason string
		var n int64
		if err := reasons.Scan(&unit, &vendor, &host, &model, &reason, &n); err != nil {
			return fmt.Errorf("store: scan unit model usage reasons: %w", err)
		}
		groups := sums[unit].Groups
		for i := range groups {
			if g := &groups[i]; g.Vendor == vendor && g.Host == host && g.Model == model {
				if g.UncountedReasons == nil {
					g.UncountedReasons = map[string]int64{}
				}
				g.UncountedReasons[reason] += n
			}
		}
	}
	if err := reasons.Err(); err != nil {
		return fmt.Errorf("store: read unit model usage reasons: %w", err)
	}
	return nil
}

func (s PG) UnitOpaqueTunnels(ctx context.Context, units []uuid.UUID, since, until time.Time) (map[uuid.UUID]int64, error) {
	rows, err := s.Pool.Query(ctx, `SELECT r.unit_id, count(*)
		FROM audit_events e JOIN agent_runs r ON r.id = e.run_id
		WHERE e.action = 'llm.scan.bypass' AND r.unit_id = ANY($1) AND r.created_at >= $2 AND r.created_at < $3
		GROUP BY r.unit_id`, units, since, until)
	if err != nil {
		return nil, fmt.Errorf("store: count unit opaque tunnels: %w", err)
	}
	defer rows.Close()
	out := map[uuid.UUID]int64{}
	for rows.Next() {
		var unit uuid.UUID
		var n int64
		if err := rows.Scan(&unit, &n); err != nil {
			return nil, fmt.Errorf("store: scan unit opaque tunnels: %w", err)
		}
		out[unit] = n
	}
	return out, rows.Err()
}
