// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ErrRunnerHealthTooLarge is a runner's offline summary or relay status past
// types.MaxRunnerOfflineSummaryBytes once encoded.
var ErrRunnerHealthTooLarge = errors.New("store: runner health report too large")

// RecordRunnerHealth keeps what runner id last reported about its own health in its one runner_health
// row: relay, when not nil, replaces the stored relay status; summary, when not nil, replaces the stored
// offline summary unless the last one was stored less than gap before now, which bounds how often one
// runner writes a summary (and its audit row). It says whether summary was stored.
func (s PG) RecordRunnerHealth(ctx context.Context, id uuid.UUID, summary *types.RunnerOfflineSummary, relay *types.RunnerRelayStatus, now time.Time, gap time.Duration) (bool, error) {
	sum, err := healthJSON(summary)
	if err != nil {
		return false, err
	}
	rel, err := healthJSON(relay)
	if err != nil {
		return false, err
	}
	var stored bool
	err = s.inTx(ctx, func(q Querier) error {
		var at *time.Time
		err := q.QueryRow(ctx, `SELECT offline_summary_at FROM runner_health WHERE runner_id = $1 FOR UPDATE`, id).Scan(&at)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		stored = sum != nil && (at == nil || !now.Before(at.Add(gap)))
		if !stored {
			sum = nil
		}
		var summaryAt *time.Time
		if stored {
			summaryAt = &now
		}
		_, err = q.Exec(ctx, `INSERT INTO runner_health (runner_id, offline_summary, offline_summary_at, relay_status)
			VALUES ($1, $2::jsonb, $3, $4::jsonb)
			ON CONFLICT (runner_id) DO UPDATE SET
				offline_summary = COALESCE(EXCLUDED.offline_summary, runner_health.offline_summary),
				offline_summary_at = COALESCE(EXCLUDED.offline_summary_at, runner_health.offline_summary_at),
				relay_status = COALESCE(EXCLUDED.relay_status, runner_health.relay_status)`, id, sum, summaryAt, rel)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("store: record runner health: %w", err)
	}
	return stored, nil
}

// RunnerHealth is runner id's stored offline summary and relay status; either is nil when none is held.
func (s PG) RunnerHealth(ctx context.Context, id uuid.UUID) (*types.RunnerOfflineSummary, *types.RunnerRelayStatus, error) {
	var sum, rel []byte
	err := s.Pool.QueryRow(ctx, `SELECT offline_summary, relay_status FROM runner_health WHERE runner_id = $1`, id).Scan(&sum, &rel)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("store: read runner health: %w", err)
	}
	var summary *types.RunnerOfflineSummary
	var relay *types.RunnerRelayStatus
	if sum != nil {
		summary = new(types.RunnerOfflineSummary)
		if err := json.Unmarshal(sum, summary); err != nil {
			return nil, nil, err
		}
	}
	if rel != nil {
		relay = new(types.RunnerRelayStatus)
		if err := json.Unmarshal(rel, relay); err != nil {
			return nil, nil, err
		}
	}
	return summary, relay, nil
}

// healthJSON is v encoded, nil for a nil v, refused past types.MaxRunnerOfflineSummaryBytes.
func healthJSON[T any](v *T) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if len(b) > types.MaxRunnerOfflineSummaryBytes {
		return nil, ErrRunnerHealthTooLarge
	}
	return b, nil
}
