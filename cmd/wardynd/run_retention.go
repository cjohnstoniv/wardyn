// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/sweephealth"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runRetentionSweepInterval is how often the leader deletes expired run history.
const runRetentionSweepInterval = time.Hour

// runRetentionBatch and runRetentionMaxBatches bound one tick: at most 10,000 runs an hour, in statements of 500,
// so a window first set on a large install drains over hours instead of holding locks for one long delete.
const (
	runRetentionBatch      = 500
	runRetentionMaxBatches = 20
)

// minRunHistoryRetentionDays is the shortest window accepted. A shorter one would delete the history a
// member's recent runs and a unit's monthly usage report read; 0 (keep forever) is always accepted.
const minRunHistoryRetentionDays = 30

// validateRunHistoryRetentionDays refuses a negative WARDYN_RUN_HISTORY_RETENTION_DAYS, one from 1 to 29,
// and one above maxRetentionDays.
func validateRunHistoryRetentionDays(n int) error {
	if n < 0 || (n > 0 && n < minRunHistoryRetentionDays) || n > maxRetentionDays {
		return fmt.Errorf("WARDYN_RUN_HISTORY_RETENTION_DAYS is %d; it must be 0 (keep run history forever) or from %d to %d days", n, minRunHistoryRetentionDays, maxRetentionDays)
	}
	return nil
}

// runRetentionStore is what the sweep needs from the store.
type runRetentionStore interface {
	DeleteExpiredRuns(ctx context.Context, olderThan time.Duration, limit int) (int, error)
}

// startRunRetention starts the leader's run-history retention sweep when days is above 0. Unset keeps every
// run forever: a run's row is the record its approvals, usage and audit trail hang from, so deleting one is
// the operator's decision, not a default.
func startRunRetention(rootCtx context.Context, leader *db.SweeperLeader, rs runRetentionStore, rec audit.Recorder, days int, ticks *sweephealth.Tracker) {
	if days <= 0 {
		return
	}
	after := time.Duration(days) * 24 * time.Hour
	leaderGo(rootCtx, leader, "run.retention", func(ctx context.Context) {
		ticker := time.NewTicker(runRetentionSweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = ticks.Tick(ctx, sweephealth.RunRetention, func(ctx context.Context) error {
					return runRetentionSweepTick(ctx, rs, rec, after)
				})
			}
		}
	})
	slog.Info("wardynd: run history retention sweeper started", slog.Duration("after", after))
}

// runRetentionSweepTick deletes expired runs in batches and, when it deleted any, records one
// run.retention.sweep audit row, so the disappearance of run history is itself evidence.
func runRetentionSweepTick(ctx context.Context, rs runRetentionStore, rec audit.Recorder, after time.Duration) error {
	total := 0
	var err error
	for i := 0; i < runRetentionMaxBatches; i++ {
		var n int
		n, err = rs.DeleteExpiredRuns(ctx, after, runRetentionBatch)
		total += n
		if err != nil || n < runRetentionBatch {
			break
		}
	}
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: run history retention sweep error", slog.Any("err", err))
	}
	if total == 0 {
		return err
	}
	slog.InfoContext(ctx, "wardynd: run history retention sweep deleted expired runs", slog.Int("deleted", total))
	data, _ := json.Marshal(map[string]any{"deleted": total, "retention_sec": int64(after.Seconds())})
	ev := types.AuditEvent{
		ID:        uuid.New(),
		Time:      time.Now().UTC(),
		ActorType: types.ActorSystem,
		Actor:     "wardyn/run-retention-sweeper",
		Action:    "run.retention.sweep",
		Target:    "runs",
		Outcome:   "success",
		Data:      json.RawMessage(data),
	}
	if rerr := rec.Record(ctx, ev); rerr != nil {
		audit.LogWriteFailure(ctx, ev, rerr)
	}
	return err
}
