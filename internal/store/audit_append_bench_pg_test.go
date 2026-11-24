// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// benchDatabase is a migrated database of its own, made from WARDYN_TEST_PG.
func benchDatabase(tb testing.TB) *pgxpool.Pool {
	tb.Helper()
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		tb.Skip("WARDYN_TEST_PG not set")
	}
	ctx := context.Background()
	admin, err := db.Connect(ctx, dsn)
	if err != nil {
		tb.Fatal(err)
	}
	name := "wardyn_bench_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		admin.Close()
		tb.Fatal(err)
	}
	tb.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1 AND pid <> pg_backend_pid()`, name)
		_, _ = admin.Exec(context.Background(), `DROP DATABASE IF EXISTS `+name)
		admin.Close()
	})
	u, err := url.Parse(dsn)
	if err != nil {
		tb.Fatal(err)
	}
	u.Path = "/" + name
	pool, err := db.Connect(ctx, u.String())
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		tb.Fatal(err)
	}
	return pool
}

// BenchmarkAuditAppend_V1VsV2: rows per second and the p99 of one append, at 16, 64 and 256 clients appending
// to the one chain. It measures whichever hash format the database's append function writes, so the same file
// run at a base without format 2 and at a head with it gives the two sides of the comparison.
func BenchmarkAuditAppend_V1VsV2(b *testing.B) {
	const total = 3000
	for _, clients := range []int{16, 64, 256} {
		b.Run(fmt.Sprintf("clients=%d", clients), func(b *testing.B) {
			for range b.N {
				pool := benchDatabase(b)
				ctx := context.Background()
				var mu sync.Mutex
				durations := make([]time.Duration, 0, total)
				var wg sync.WaitGroup
				start := time.Now()
				for c := range clients {
					wg.Add(1)
					go func() {
						defer wg.Done()
						n := total / clients
						if c < total%clients {
							n++
						}
						for range n {
							ev := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorSystem, Actor: "bench", Action: "bench.append", Outcome: "success"}
							t0 := time.Now()
							if err := store.InsertAuditEvent(ctx, pool, &ev); err != nil {
								b.Error(err)
								return
							}
							d := time.Since(t0)
							mu.Lock()
							durations = append(durations, d)
							mu.Unlock()
						}
					}()
				}
				wg.Wait()
				elapsed := time.Since(start)
				slices.Sort(durations)
				b.ReportMetric(float64(len(durations))/elapsed.Seconds(), "rows/s")
				b.ReportMetric(float64(durations[len(durations)*99/100].Microseconds()), "p99_us")
			}
		})
	}
}

// BenchmarkRunnerSpoolIngest_LockShare_PG: how much of a runner's spooled record is spent from the moment its rows
// are ready to be written (after they are masked and sealed) to the commit, which is where the audit chain's
// lock is waited for and held. It reports that span as a percentage of each record's whole ingest, for one
// runner and for eight at once.
func BenchmarkRunnerSpoolIngest_LockShare_PG(b *testing.B) {
	const perRunner = 150
	for _, runners := range []int{1, 8} {
		b.Run(fmt.Sprintf("runners=%d", runners), func(b *testing.B) {
			for range b.N {
				pool := benchDatabase(b)
				ctx := context.Background()
				st := store.NewPG(pool)
				type lane struct {
					runner uuid.UUID
					rows   []runnerspool.Record
				}
				lanes := make([]lane, runners)
				for i := range lanes {
					rn, err := st.CreateRunner(ctx, newRunner(fmt.Sprintf("bench-owner-%d", i)))
					if err != nil {
						b.Fatal(err)
					}
					if rn, err = st.ClaimRunner(ctx, rn.ID, rn.Owner, rn.KeyFingerprint, time.Now()); err != nil {
						b.Fatal(err)
					}
					run := newRun(types.RunRunning)
					run.CreatedBy, run.Placement, run.RunnerID, run.EvidenceSource = rn.Owner, types.PlacementLocal, &rn.ID, types.RunEvidenceRunnerAsserted
					if run, err = st.CreateRun(ctx, run); err != nil {
						b.Fatal(err)
					}
					spool, err := runnerspool.Open(filepath.Join(b.TempDir(), "spool"), run.ID)
					if err != nil {
						b.Fatal(err)
					}
					for j := range perRunner {
						log := egress.DecisionLog{Request: egress.Request{RunID: run.ID, Host: fmt.Sprintf("h%d.test", j), Time: time.Now().UTC().Truncate(time.Microsecond)}, Decision: egress.Allow, RuleSource: "policy:allowed"}
						if err := spool.Append(ctx, log); err != nil {
							b.Fatal(err)
						}
					}
					rows, err := spool.After(ctx, 0, perRunner)
					if err != nil {
						b.Fatal(err)
					}
					_ = spool.Close()
					lanes[i] = lane{runner: rn.ID, rows: rows}
				}
				var mu sync.Mutex
				var inChain, whole time.Duration
				var wg sync.WaitGroup
				for _, l := range lanes {
					wg.Add(1)
					go func() {
						defer wg.Done()
						for _, row := range l.rows {
							var ready time.Time
							t0 := time.Now()
							prepare := func(_ context.Context, ev types.AuditEvent) (types.AuditEvent, error) {
								ready = time.Now()
								return ev, nil
							}
							if _, err := st.IngestRunnerDecision(ctx, l.runner, row, prepare); err != nil {
								b.Error(err)
								return
							}
							t1 := time.Now()
							mu.Lock()
							whole += t1.Sub(t0)
							if !ready.IsZero() {
								inChain += t1.Sub(ready)
							}
							mu.Unlock()
						}
					}()
				}
				wg.Wait()
				b.ReportMetric(100*float64(inChain)/float64(whole), "lock_share_pct")
			}
		})
	}
}
