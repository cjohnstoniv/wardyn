// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// fakeRetention is a store.AuditRetention that counts what the sweep asks of it. dropsLeft partitions are
// "eligible": each AutodropAuditPartition takes one.
type fakeRetention struct {
	store.AuditRetention
	ensures   atomic.Int64
	autodrops atomic.Int64
	dropsLeft atomic.Int64
	ensureErr error
}

func (f *fakeRetention) EnsureAuditPartitions(context.Context, int) (int, error) {
	f.ensures.Add(1)
	return 1, f.ensureErr
}

func (f *fakeRetention) AutodropAuditPartition(context.Context) (store.AuditRetentionDrop, bool, error) {
	f.autodrops.Add(1)
	if f.dropsLeft.Add(-1) < 0 {
		return store.AuditRetentionDrop{}, false, nil
	}
	return store.AuditRetentionDrop{Partition: "audit_events_p202401", Rows: 3}, true, nil
}

// With the flag off the sweep creates months and drops nothing, whatever is eligible.
func TestSweepAuditRetention_NothingIsDroppedWithoutTheFlag(t *testing.T) {
	f := &fakeRetention{}
	f.dropsLeft.Store(5)
	sweepAuditRetention(context.Background(), f, false)
	if f.ensures.Load() != 1 || f.autodrops.Load() != 0 {
		t.Fatalf("ensures=%d autodrops=%d, want 1 and 0", f.ensures.Load(), f.autodrops.Load())
	}
}

// With the flag on it drops every eligible oldest partition, one call each, and stops at the first that is not.
func TestSweepAuditRetention_AutodropTakesEachEligiblePartitionThenStops(t *testing.T) {
	f := &fakeRetention{}
	f.dropsLeft.Store(3)
	sweepAuditRetention(context.Background(), f, true)
	if got := f.autodrops.Load(); got != 4 { // three drops, then the one that finds nothing
		t.Fatalf("autodrop called %d times, want 4", got)
	}
	// A failing ensure does not stop the pass.
	f2 := &fakeRetention{ensureErr: errors.New("boom")}
	f2.dropsLeft.Store(1)
	sweepAuditRetention(context.Background(), f2, true)
	if f2.autodrops.Load() != 2 {
		t.Fatalf("autodrop after a failed ensure called %d times, want 2", f2.autodrops.Load())
	}
}

// A sweep bounded by maxAutodropsPerSweep never spins, however many partitions claim to be eligible.
func TestSweepAuditRetention_AutodropIsBounded(t *testing.T) {
	f := &fakeRetention{}
	f.dropsLeft.Store(1_000_000)
	sweepAuditRetention(context.Background(), f, true)
	if got := f.autodrops.Load(); got != maxAutodropsPerSweep {
		t.Fatalf("autodrop called %d times, want the bound %d", got, maxAutodropsPerSweep)
	}
}

// countingEnsure counts the ensure calls of one replica over a real store.
type countingEnsure struct {
	store.AuditRetention
	ensures atomic.Int64
}

func (c *countingEnsure) EnsureAuditPartitions(ctx context.Context, m int) (int, error) {
	c.ensures.Add(1)
	return c.AuditRetention.EnsureAuditPartitions(ctx, m)
}

// TestLeaderGo_AuditEnsureRunsOnOneReplica: two replicas on one database both start the retention sweep;
// only the elected leader's loop runs, so the partitions are ensured once per tick across both.
func TestLeaderGo_AuditEnsureRunsOnOneReplica(t *testing.T) {
	poolA := pgPool(t)
	poolB, err := pgxpool.New(context.Background(), os.Getenv("WARDYN_TEST_PG"))
	if err != nil {
		t.Fatalf("connect poolB: %v", err)
	}
	defer poolB.Close()

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	var a, b countingEnsure
	for _, r := range []struct {
		pool *pgxpool.Pool
		c    *countingEnsure
		name string
	}{{poolA, &a, "replica-a"}, {poolB, &b, "replica-b"}} {
		r.c.AuditRetention = store.NewPG(r.pool)
		leader := db.NewSweeperLeader(r.pool, r.name)
		go leader.Run(ctx)
		leaderGo(ctx, leader, "audit.retention", func(c context.Context) {
			runAuditRetentionSweeper(c, r.c, false, 20*time.Millisecond)
		})
	}
	deadline := time.Now().Add(10 * time.Second)
	for a.ensures.Load()+b.ensures.Load() < 8 {
		if time.Now().After(deadline) {
			t.Fatalf("no replica ensured the partitions: a=%d b=%d", a.ensures.Load(), b.ensures.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if a.ensures.Load() > 0 && b.ensures.Load() > 0 {
		t.Fatalf("both replicas ran the daily ensure (a=%d b=%d)", a.ensures.Load(), b.ensures.Load())
	}
}

// TestRecordAuditRetentionPolicy_BootRecordsAndNeverResetsTheCooldown: the boot helper drives the policy
// function. A decrease is pending, the same value at every later boot leaves its date alone, and an
// increase is effective at the very boot that carries it.
func TestRecordAuditRetentionPolicy_BootRecordsAndNeverResetsTheCooldown(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()
	rs := store.NewPG(pool)
	if _, err := pool.Exec(ctx, `UPDATE audit_partition_meta SET retention_days = 0, pending_days = NULL, pending_effective_at = NULL`); err != nil {
		t.Fatalf("reset the policy: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `UPDATE audit_partition_meta SET retention_days = 0, pending_days = NULL, pending_effective_at = NULL`) //nolint:errcheck // best-effort reset of a shared test database
	})
	recordAuditRetentionPolicy(ctx, rs, 90)
	st, err := rs.AuditRetentionStatus(ctx)
	if err != nil || st.Policy.PendingDays == nil || *st.Policy.PendingDays != 90 || st.Policy.EffectiveDays != 0 {
		t.Fatalf("after a boot with 90: %+v, err %v, want 90 pending and forever in force", st.Policy, err)
	}
	first := *st.Policy.PendingEffectiveAt
	recordAuditRetentionPolicy(ctx, rs, 90)
	st, _ = rs.AuditRetentionStatus(ctx)
	if st.Policy.PendingEffectiveAt == nil || !st.Policy.PendingEffectiveAt.Equal(first) {
		t.Fatalf("a second boot with 90 moved the date: %v then %v", first, st.Policy.PendingEffectiveAt)
	}
	// An increase: from forever (0) nothing is higher, so use the stored finite policy.
	if _, err := pool.Exec(ctx, `UPDATE audit_partition_meta SET retention_days = 30, pending_days = NULL, pending_effective_at = NULL`); err != nil {
		t.Fatalf("set 30: %v", err)
	}
	recordAuditRetentionPolicy(ctx, rs, 365)
	if st, _ = rs.AuditRetentionStatus(ctx); st.Policy.EffectiveDays != 365 || st.Policy.PendingDays != nil {
		t.Fatalf("after a boot with 365 over 30: %+v, want 365 in force at once", st.Policy)
	}
}
