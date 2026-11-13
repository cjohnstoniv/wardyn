// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The two statements the pins below hold, exactly as the store sends them.
const (
	runnerExecutionOfSQL = `SELECT run_id,runner_id,generation,operation,exec_id FROM runner_executions WHERE run_id=$1`
	bindExecutionSQL     = `UPDATE runner_executions SET exec_id=$4 WHERE run_id=$1 AND runner_id=$2 AND generation=$3 AND (exec_id='' OR exec_id=$4)`
)

// sqlStall is a tracer that holds one statement (match) until its context ends, as a pool or network that never
// answers it does; entered closes when it is reached, so the statements before it have answered.
type sqlStall struct {
	match   string
	entered chan struct{}
	once    sync.Once
}

func (s *sqlStall) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if d.SQL == s.match {
		s.once.Do(func() { close(s.entered) })
		<-ctx.Done()
	}
	return ctx
}

func (*sqlStall) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// reached fails t unless the stalled statement was reached.
func (s *sqlStall) reached(t *testing.T) {
	t.Helper()
	select {
	case <-s.entered:
	default:
		t.Fatalf("the statement %q was never reached; the pin did not exercise it", s.match)
	}
}

// stalledStore is f's database on a pool of its own whose statement match never answers.
func stalledStore(t *testing.T, f *runnerLeaseFixture, match string) (store.PG, *sqlStall) {
	t.Helper()
	s := &sqlStall{match: match, entered: make(chan struct{})}
	cfg := f.st.Pool.Config()
	cfg.ConnConfig.Tracer = s
	p, err := pgxpool.NewWithConfig(f.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return store.NewPG(p), s
}

// startedNotBound leases and creates the run on the runner and starts its agent under G1, as dispatch does, and
// stops before the bind: the generation and the Exec's handle, for the bind under test.
func (f *runnerLeaseFixture) startedNotBound(t *testing.T, run *types.AgentRun) (runnerExecution, string) {
	t.Helper()
	e, err := f.srv.leaseRunOnRunner(f.ctx, *run)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.create(t, run); err != nil {
		t.Fatal(err)
	}
	execID, err := f.srv.cfg.Runner.Exec(e.bind(f.ctx), run.SandboxRef, []string{"/usr/local/bin/agent-run", "t"})
	if err != nil {
		t.Fatal(err)
	}
	return e, execID
}

// TestPG_RunnerLease_AStalledGenerationReadFreesTheRunLock: a lease renewal holds the run's lock on its caller's
// context, which may have none (an end edit's renewal detaches it). Its run and lease reads answer and its last
// read, the run's execution generation, never does: it ends at heldStoreCallBound, the renewal answers an error,
// and the run's lock is free. Catches: the generation read on the holder's context.
func TestPG_RunnerLease_AStalledGenerationReadFreesTheRunLock(t *testing.T) {
	f := newRunnerLeaseFixture(t)
	run := f.localRun(t, 0)
	f.placed(t, &run)
	old := heldStoreCallBound
	heldStoreCallBound = 500 * time.Millisecond
	t.Cleanup(func() { heldStoreCallBound = old })
	st, stall := stalledStore(t, f, runnerExecutionOfSQL)
	lockFreesWithin(t, f.srv, run.ID, "the lease renewal", func(ctx context.Context) {
		if err := f.srv.renewRunnerLeaseHeld(ctx, st, run, 0); err == nil {
			t.Error("the renewal answered nil; want the stalled read's error")
		}
	})
	stall.reached(t)
}

// TestPG_RunnerLease_ABindBehindAHeldRunRowEndsAtTheStatementTimeout: after the agent's Exec, the bind waits for the
// run's row, which another transaction holds. The database ends it at store.RunWriteStatementTimeout (57014), the
// bind answers that error, its run lock is free, and nothing is bound. Catches: the bind's transaction with no
// statement timeout, which waits out db.LockWait and heldWriteBound under the lock.
func TestPG_RunnerLease_ABindBehindAHeldRunRowEndsAtTheStatementTimeout(t *testing.T) {
	f := newRunnerLeaseFixture(t)
	run := f.localRun(t, 0)
	e, execID := f.startedNotBound(t, &run)
	old := store.RunWriteStatementTimeout
	store.RunWriteStatementTimeout = 200 * time.Millisecond
	t.Cleanup(func() { store.RunWriteStatementTimeout = old })
	holder, err := f.st.Pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
	if _, err := holder.Exec(f.ctx, `SELECT 1 FROM agent_runs WHERE id=$1 FOR UPDATE`, run.ID); err != nil {
		t.Fatal(err)
	}
	lockFreesWithin(t, f.srv, run.ID, "the bind behind a held run row", func(ctx context.Context) {
		var pgErr *pgconn.PgError
		if err := f.srv.bindRunnerExecution(ctx, e, execID); !errors.As(err, &pgErr) || pgErr.Code != "57014" {
			t.Errorf("bind = %v; want the database's statement timeout (57014)", err)
		}
	})
	if err := holder.Rollback(f.ctx); err != nil {
		t.Fatal(err)
	}
	if cur := f.execution(t, run); cur.ExecID != "" {
		t.Fatalf("exec id %q bound by a bind the database rolled back; want none", cur.ExecID)
	}
}

// TestPG_RunnerLease_AStalledBindEndsAtItsBound: after the agent's Exec, the bind's statement never answers (a pool
// or network stall, which no server-side limit sees). The bind ends at db.LockWait plus heldWriteBound on the
// dispatch's context, which has no deadline of its own; its run lock is free and nothing is bound. Catches: the bind
// on its caller's context.
func TestPG_RunnerLease_AStalledBindEndsAtItsBound(t *testing.T) {
	f := newRunnerLeaseFixture(t)
	run := f.localRun(t, 0)
	e, execID := f.startedNotBound(t, &run)
	oldWait, oldWrite := db.LockWait, heldWriteBound
	db.LockWait, heldWriteBound = 200*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { db.LockWait, heldWriteBound = oldWait, oldWrite })
	st, stall := stalledStore(t, f, bindExecutionSQL)
	f.srv.cfg.Store = st
	lockFreesWithin(t, f.srv, run.ID, "the bind after the agent's start", func(ctx context.Context) {
		if err := f.srv.bindRunnerExecution(ctx, e, execID); err == nil {
			t.Error("the bind answered nil; want the stalled statement's error")
		}
	})
	stall.reached(t)
	if cur := f.execution(t, run); cur.ExecID != "" {
		t.Fatalf("exec id %q bound by a bind that never answered; want none", cur.ExecID)
	}
}
