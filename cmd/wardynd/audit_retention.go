// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// auditRetentionInterval is how often the leader sweeper keeps the audit partitions ahead (and, with
// WARDYN_AUDIT_RETENTION_AUTODROP, drops what has aged out). Daily: a month is the unit of both.
const auditRetentionInterval = 24 * time.Hour

// maxAutodropsPerSweep bounds one sweep's drops (ten years of monthly partitions), so a freshly enabled
// flag over a long history works through it over a few days of sweeps at worst, and never spins.
const maxAutodropsPerSweep = 120

// startAuditRetention records the retention policy this boot was given and starts the daily sweep. The
// policy goes through audit_retention_set_policy, which decides inside the database what takes effect when
// (an increase at once, a decrease 30 days after the first boot that saw it, never reset by a restart), so
// every replica may call it with the same value. A policy that could not be recorded is logged and the
// stored one stands, which can only keep more history, never less. The sweep runs on the sweeper leader
// only, so with several replicas exactly one creates the months and drops the partitions.
func startAuditRetention(rootCtx context.Context, f *bootFlags, pool *pgxpool.Pool, leader *db.SweeperLeader, siem audit.Sink) {
	rs := store.NewPG(pool)
	rs.SIEM = siem
	recordAuditRetentionPolicy(rootCtx, rs, *f.auditRetentionDays)
	autodrop := *f.auditRetentionAutodrop
	if autodrop {
		slog.Warn("wardynd: WARDYN_AUDIT_RETENTION_AUTODROP is on: the sweeper drops audit partitions past the retention window with no operator having checked an export first. Each drop is still a chained event and an anchor, and verify still passes, but it is not attested")
	}
	leaderGo(rootCtx, leader, "audit.retention", func(ctx context.Context) {
		runAuditRetentionSweeper(ctx, rs, autodrop, auditRetentionInterval)
	})
}

// recordAuditRetentionPolicy writes the configured window and reports what it did.
func recordAuditRetentionPolicy(ctx context.Context, rs store.AuditRetention, days int) {
	ch, err := rs.SetAuditRetentionPolicy(ctx, days)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: audit retention policy NOT recorded; the stored policy stands", slog.Int("days", days), slog.Any("err", err))
		return
	}
	attrs := []any{slog.String("outcome", ch.Outcome), slog.Int("effective_days", ch.EffectiveDays)}
	if ch.PendingDays != nil && ch.PendingEffectiveAt != nil {
		attrs = append(attrs, slog.Int("pending_days", *ch.PendingDays), slog.Time("pending_effective_at", *ch.PendingEffectiveAt))
	}
	if ch.Outcome == "unchanged" {
		slog.InfoContext(ctx, "wardynd: audit retention policy", attrs...)
		return
	}
	slog.WarnContext(ctx, "wardynd: audit retention policy changed", attrs...)
}

// runAuditRetentionSweeper sweeps now and then every interval until ctx ends.
func runAuditRetentionSweeper(ctx context.Context, rs store.AuditRetention, autodrop bool, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		sweepAuditRetention(ctx, rs, autodrop)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// sweepAuditRetention is one pass: create the months ahead, then (autodrop only) drop every eligible
// oldest partition, one at a time, as the system actor. A failure is logged and the next pass tries again.
func sweepAuditRetention(ctx context.Context, rs store.AuditRetention, autodrop bool) {
	if n, err := rs.EnsureAuditPartitions(ctx, db.AuditPartitionMonthsAhead); err != nil {
		slog.WarnContext(ctx, "wardynd: audit partition ensure failed; it retries at the next sweep", slog.Any("err", err))
	} else if n > 0 {
		slog.InfoContext(ctx, "wardynd: created audit partitions", slog.Int("months", n))
	}
	if !autodrop {
		return
	}
	for i := 0; i < maxAutodropsPerSweep && ctx.Err() == nil; i++ {
		d, ok, err := rs.AutodropAuditPartition(ctx)
		if err != nil {
			slog.WarnContext(ctx, "wardynd: audit autodrop failed; it retries at the next sweep", slog.Any("err", err))
			return
		}
		if !ok {
			return
		}
		slog.WarnContext(ctx, "wardynd: dropped an audit partition past the retention window (unattested)",
			slog.String("partition", d.Partition), slog.Int64("rows", d.Rows), slog.String("digest", d.Digest), slog.Int64("event_seq", d.EventSeq))
	}
}
