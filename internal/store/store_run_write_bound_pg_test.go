// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_RunWriteStatementTimeout_AStuckStateWriteIsRolledBack: a run's state write stuck behind a row lock another
// transaction holds is ended by the database at RunWriteStatementTimeout with a statement timeout (57014), which is
// not ErrRunWriteUnsettled (no COMMIT was sent), and it changes nothing: the run keeps its state after the holder
// lets go.
func TestPG_RunWriteStatementTimeout_AStuckStateWriteIsRolledBack(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	old := store.RunWriteStatementTimeout
	store.RunWriteStatementTimeout = 200 * time.Millisecond
	t.Cleanup(func() { store.RunWriteStatementTimeout = old })
	run := persistRun(t, ctx, pool, newRun(types.RunRunning))

	holder := holdRunRow(t, ctx, pool, run.ID)
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	applied, err := store.NewPG(pool).UpdateRunStateIf(wctx, run.ID, types.RunRunning, types.RunFailed)
	var pgErr *pgconn.PgError
	if applied || !errors.As(err, &pgErr) || pgErr.Code != "57014" || errors.Is(err, store.ErrRunWriteUnsettled) {
		t.Fatalf("applied %v, err %v; want the database's statement timeout (57014), settled", applied, err)
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := store.NewPG(pool).GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != types.RunRunning {
		t.Fatalf("state %s after a write the database rolled back, want %s", got.State, types.RunRunning)
	}
}

// holdRunRow opens a transaction holding run id's row lock (FOR UPDATE); a test's cleanup rolls it back.
func holdRunRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id uuid.UUID) pgx.Tx {
	t.Helper()
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
	if _, err := holder.Exec(ctx, `SELECT 1 FROM agent_runs WHERE id=$1 FOR UPDATE`, id); err != nil {
		t.Fatal(err)
	}
	return holder
}

// stalledRollback is a tracer whose ROLLBACK never leaves the client until its context ends, as a rollback whose
// reply is withheld does; entered closes when one is reached.
type stalledRollback struct {
	entered chan struct{}
	once    sync.Once
}

func (s *stalledRollback) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if d.SQL == "rollback" {
		s.once.Do(func() { close(s.entered) })
		<-ctx.Done()
	}
	return ctx
}

func (*stalledRollback) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// TestPG_RunWriteRollbackBound_ARollbackWithNoAnswerEndsAtItsBound: a state write that failed before its COMMIT (the
// statement timeout) rolls back on RunWriteRollbackBound of its own, so a ROLLBACK that is never answered cannot
// hold the write, and the run's lock with it. Catches: the rollback on the caller's context with its cancel removed,
// which never ends.
func TestPG_RunWriteRollbackBound_ARollbackWithNoAnswerEndsAtItsBound(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	oldStmt, oldRollback := store.RunWriteStatementTimeout, store.RunWriteRollbackBound
	store.RunWriteStatementTimeout, store.RunWriteRollbackBound = 200*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { store.RunWriteStatementTimeout, store.RunWriteRollbackBound = oldStmt, oldRollback })
	run := persistRun(t, ctx, pool, newRun(types.RunRunning))
	holdRunRow(t, ctx, pool, run.ID)

	tr := &stalledRollback{entered: make(chan struct{})}
	cfg := pool.Config()
	cfg.ConnConfig.Tracer = tr
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(traced.Close)
	done := make(chan error, 1)
	go func() {
		_, err := store.NewPG(traced).UpdateRunStateIf(ctx, run.ID, types.RunRunning, types.RunFailed)
		done <- err
	}()
	select {
	case err := <-done:
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "57014" {
			t.Fatalf("err %v; want the statement timeout (57014)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a state write whose rollback is never answered did not return: its rollback has no bound")
	}
	select {
	case <-tr.entered:
	default:
		t.Fatal("the rollback was never reached; the pin did not exercise it")
	}
}

// commitGateSQL holds the COMMIT of any transaction that moved a run to FAILED until advisory lock 4207 is free,
// through cancels: the COMMIT is sent and stays pending on the server after its client has given up on it.
const commitGateSQL = `
CREATE FUNCTION commit_gate() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
	LOOP
		BEGIN
			PERFORM pg_advisory_xact_lock(4207);
			EXIT;
		EXCEPTION WHEN query_canceled THEN
			NULL;
		END;
	END LOOP;
	RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER commit_gate AFTER UPDATE ON agent_runs DEFERRABLE INITIALLY DEFERRED
	FOR EACH ROW WHEN (NEW.state = 'FAILED') EXECUTE FUNCTION commit_gate();`

type settled struct {
	state types.RunState
	err   error
}

// TestPG_SettleRunState_ReadsTheOutcomeOfACommitStillPendingWhenItsClientReturned: a state write whose client gives up
// while its COMMIT is pending on the server answers ErrRunWriteUnsettled. An unlocked read then still sees the old
// state, though the write commits later; SettleRunState waits behind the row lock until the COMMIT ends and reads
// the committed state. Catches: settling from an unlocked read, and sorting a COMMIT sent and not answered as not
// applied.
func TestPG_SettleRunState_ReadsTheOutcomeOfACommitStillPendingWhenItsClientReturned(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	old := store.RunWriteStatementTimeout
	store.RunWriteStatementTimeout = 30 * time.Second
	t.Cleanup(func() { store.RunWriteStatementTimeout = old })
	if _, err := pool.Exec(ctx, commitGateSQL); err != nil {
		t.Fatal(err)
	}
	run := persistRun(t, ctx, pool, newRun(types.RunRunning))
	gate, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gate.Release)
	if _, err := gate.Exec(ctx, `SELECT pg_advisory_lock(4207)`); err != nil {
		t.Fatal(err)
	}
	open := func() { _, _ = gate.Exec(context.Background(), `SELECT pg_advisory_unlock(4207)`) }
	t.Cleanup(open)

	st := store.NewPG(pool)
	wctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	_, err = st.UpdateRunStateIf(wctx, run.ID, types.RunRunning, types.RunFailed)
	cancel()
	if !errors.Is(err, store.ErrRunWriteUnsettled) {
		t.Fatalf("err %v; want ErrRunWriteUnsettled for a COMMIT sent and not answered", err)
	}
	if got, err := st.GetRun(ctx, run.ID); err != nil || got.State != types.RunRunning {
		t.Fatalf("unlocked read: %s, %v; want %s, the old row while the COMMIT is pending", got.State, err, types.RunRunning)
	}
	res := make(chan settled, 1)
	go func() {
		s, err := st.SettleRunState(ctx, run.ID)
		res <- settled{s, err}
	}()
	select {
	case r := <-res:
		t.Fatalf("settled at %s (%v) with the COMMIT still pending", r.state, r.err)
	case <-time.After(300 * time.Millisecond):
	}
	open()
	select {
	case r := <-res:
		if r.err != nil || r.state != types.RunFailed {
			t.Fatalf("settled at %s, %v; want %s, the state the pending COMMIT wrote", r.state, r.err, types.RunFailed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("SettleRunState never returned after the COMMIT ended")
	}
}

// TestPG_SettleRunState_WaitsForAnOpenWriteAndReadsItsOutcome: SettleRunState does not answer while another
// transaction holds an uncommitted change to the run's row, and then reads what that transaction did: its state if
// it committed, the old state if it rolled back, and a statement timeout (57014) if it is still open at
// RunWriteStatementTimeout.
func TestPG_SettleRunState_WaitsForAnOpenWriteAndReadsItsOutcome(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	old := store.RunWriteStatementTimeout
	store.RunWriteStatementTimeout = 2 * time.Second
	t.Cleanup(func() { store.RunWriteStatementTimeout = old })
	for _, tc := range []struct {
		name string
		end  func(pgx.Tx) error
		want types.RunState
	}{
		{"committed", func(tx pgx.Tx) error { return tx.Commit(ctx) }, types.RunFailed},
		{"rolled back", func(tx pgx.Tx) error { return tx.Rollback(ctx) }, types.RunRunning},
		{"still open", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := persistRun(t, ctx, pool, newRun(types.RunRunning))
			w, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = w.Rollback(context.Background()) })
			if _, err := w.Exec(ctx, `UPDATE agent_runs SET state=$2, updated_at=now(), ended_at=now() WHERE id=$1`, run.ID, string(types.RunFailed)); err != nil {
				t.Fatal(err)
			}
			res := make(chan settled, 1)
			go func() {
				s, err := store.NewPG(pool).SettleRunState(ctx, run.ID)
				res <- settled{s, err}
			}()
			select {
			case r := <-res:
				t.Fatalf("settled at %s (%v) with the write still open", r.state, r.err)
			case <-time.After(300 * time.Millisecond):
			}
			if tc.end != nil {
				if err := tc.end(w); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case r := <-res:
				var pgErr *pgconn.PgError
				switch {
				case tc.end == nil && (!errors.As(r.err, &pgErr) || pgErr.Code != "57014"):
					t.Fatalf("settled at %s, %v; want the statement timeout (57014) for a write still open", r.state, r.err)
				case tc.end != nil && (r.err != nil || r.state != tc.want):
					t.Fatalf("settled at %s, %v; want %s", r.state, r.err, tc.want)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("SettleRunState never returned")
			}
		})
	}
}
