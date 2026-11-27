// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestRunOutageTerms_WrittenOnce_PG(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	run := persistRun(t, ctx, pool, newRun(types.RunStarting))
	first := types.OutageTerms{AllowanceSec: 1800, Strict: true}
	got, err := pg.SetRunOutageTerms(ctx, run.ID, first)
	if err != nil || got != first {
		t.Fatalf("first write = %+v %v, want %+v", got, err, first)
	}
	got, err = pg.SetRunOutageTerms(ctx, run.ID, types.OutageTerms{AllowanceSec: 7200, EndUnattended: true})
	if err != nil || got != first {
		t.Fatalf("second write = %+v %v, want the first terms %+v", got, err, first)
	}
	if read, err := pg.RunOutageTerms(ctx, run.ID); err != nil || read != first {
		t.Fatalf("read = %+v %v, want %+v", read, err, first)
	}
}

func TestRunOutageTerms_MissingRowIsDefault_PG(t *testing.T) {
	pg := store.NewPG(runsPGPoolIsolated(t))
	got, err := pg.RunOutageTerms(context.Background(), uuid.New())
	if err != nil || got != types.DefaultOutageTerms() {
		t.Fatalf("read of a run with no row = %+v %v, want the default terms", got, err)
	}
}

func TestRunOutageTerms_CheckRefusesOutOfRange_PG(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	for _, sec := range []int64{1799, 86401} {
		run := persistRun(t, ctx, pool, newRun(types.RunStarting))
		if got, err := pg.SetRunOutageTerms(ctx, run.ID, types.OutageTerms{AllowanceSec: sec}); err == nil {
			t.Fatalf("allowance %d s recorded as %+v, want the CHECK to refuse it", sec, got)
		}
	}
	for _, sec := range []int64{1800, 86400} {
		run := persistRun(t, ctx, pool, newRun(types.RunStarting))
		if _, err := pg.SetRunOutageTerms(ctx, run.ID, types.OutageTerms{AllowanceSec: sec}); err != nil {
			t.Fatalf("allowance %d s refused: %v", sec, err)
		}
	}
}

func TestRunOutageTerms_ConcurrentFirstWritesAgree_PG(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	terms := [2]types.OutageTerms{{AllowanceSec: 1800, Strict: true}, {AllowanceSec: 7200, EndUnattended: true}}
	for round := range 20 {
		run := persistRun(t, ctx, pool, newRun(types.RunStarting))
		var wg sync.WaitGroup
		var got [2]types.OutageTerms
		var errs [2]error
		start := make(chan struct{})
		for i := range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				got[i], errs[i] = pg.SetRunOutageTerms(ctx, run.ID, terms[i])
			}()
		}
		close(start)
		wg.Wait()
		stored, err := pg.RunOutageTerms(ctx, run.ID)
		if errs[0] != nil || errs[1] != nil || err != nil || got[0] != got[1] || got[0] != stored {
			t.Fatalf("round %d: calls = %+v %v and %+v %v, stored %+v %v; want both nil and equal to the row", round, got[0], errs[0], got[1], errs[1], stored, err)
		}
	}
}

func TestRunOutageTerms_SecondWriteAfterBlockedInsertReadsFirst_PG(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	run := persistRun(t, ctx, pool, newRun(types.RunStarting))
	first := types.OutageTerms{AllowanceSec: 1800, Strict: true}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `INSERT INTO run_outage_terms (run_id, allowance_s, strict, end_unattended) VALUES ($1, $2, $3, $4)`,
		run.ID, first.AllowanceSec, first.Strict, first.EndUnattended); err != nil {
		t.Fatal(err)
	}
	var pid int
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	type result struct {
		t   types.OutageTerms
		err error
	}
	done := make(chan result, 1)
	go func() {
		got, err := pg.SetRunOutageTerms(ctx, run.ID, types.OutageTerms{AllowanceSec: 7200})
		done <- result{got, err}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))`, pid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the second write never blocked on the first insert's row")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil || r.t != first {
		t.Fatalf("blocked second write = %+v %v, want the first terms %+v", r.t, r.err, first)
	}
}
