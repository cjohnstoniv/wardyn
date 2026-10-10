// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A capacity-lock wait must retain the admitted run's idle-clock timestamp.
func TestPG_AppClockRunPreservesAdmissionAcrossCapacityLock(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	locker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = locker.Rollback(context.WithoutCancel(ctx)) }()
	var lockerPID int
	if err := locker.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&lockerPID); err != nil {
		t.Fatal(err)
	}
	if _, err := locker.Exec(ctx, `SELECT pg_advisory_xact_lock($1,0)`, db.RunCapLockClass); err != nil {
		t.Fatal(err)
	}
	origin := time.Now()
	start := origin.UTC().Add(time.Minute)
	st.Now = func() time.Time { return start.Add(time.Since(origin)) }
	run := newRun(types.RunPending)
	run.UpdatedAt = st.Now()
	type response struct {
		run types.AgentRun
		err error
	}
	result := make(chan response, 1)
	go func() { got, err := st.CreateRunUnderCap(ctx, run, 10); result <- response{got, err} }()
	for {
		var waiting bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, lockerPID).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("run never waited on capacity lock")
		case <-time.After(time.Millisecond):
		}
	}
	var cutoff time.Time
	if err := pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&cutoff); err != nil {
		t.Fatal(err)
	}
	if err := locker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-result:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.run.UpdatedAt.After(cutoff) {
			t.Fatalf("capacity wait advanced admission: got=%s cutoff=%s", got.run.UpdatedAt, cutoff)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
