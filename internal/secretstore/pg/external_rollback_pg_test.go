// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

// A Put's rollback ends by its caller's bound. Guarded by WARDYN_TEST_PG.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/multitracer"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// rolledBack is the context the first rollback a pool runs ran on: whether it
// had ended, and its deadline, if any, against when the rollback started.
type rolledBack struct {
	ended       bool
	at          time.Time
	deadline    time.Time
	hasDeadline bool
}

// rollbackTracer reports the first rollback a pool runs.
type rollbackTracer struct{ rolled chan rolledBack }

func (r rollbackTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if d.SQL == "rollback" {
		dl, ok := ctx.Deadline()
		select {
		case r.rolled <- rolledBack{ended: ctx.Err() != nil, at: time.Now(), deadline: dl, hasDeadline: ok}:
		default:
		}
	}
	return ctx
}

func (rollbackTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// Each Put path that defers a rollback (putExternal, putOverPointer and
// putFenced) ends that rollback by its caller's bound. The Put waits on the
// erase counter row, which the test holds, until the test ends its context.
// putExternal rolls back on its own bounded context, which ended with the
// caller's: pgx closes the connection instead, which ends the transaction.
// putOverPointer and putFenced roll back through db.Rollback: not on the ended
// context, but on one that ends within db.RollbackBound, as the caller here
// sets no deadline.
func TestPG_Put_RollsBackOnTheCallersContext(t *testing.T) {
	for _, c := range []struct {
		path     string
		ext      secretstore.External
		writeExt bool
	}{
		{"putExternal", newRotExt(), true},
		{"putOverPointer", newRotExt(), false},
		{"putFenced", nil, false},
	} {
		t.Run(c.path, func(t *testing.T) {
			cfg := rekeyDatabase(t).Config()
			rolled := make(chan rolledBack, 1)
			// multitracer keeps the pool guard's tracer, set when WARDYN_TEST_POOL_GUARD=1.
			if base := cfg.ConnConfig.Tracer; base != nil {
				cfg.ConnConfig.Tracer = multitracer.New(base, rollbackTracer{rolled})
			} else {
				cfg.ConnConfig.Tracer = rollbackTracer{rolled}
			}
			pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			s := mixedStore(t, pool, mustIdentity(t), false, c.ext, c.writeExt)
			release := holdCounterRow(t, pool, "bob")
			ctx, cancel := context.WithCancel(secretstore.WithIfGeneration(t.Context(), "bob", 0))
			defer cancel()
			put := make(chan error, 1)
			go func() { put <- s.For("bob").Put(ctx, "tok", []byte("v1")) }()
			waitCounterRowWaiter(t, pool)
			cancel()
			select {
			case err := <-put:
				if err == nil {
					t.Fatal("a Put whose context ended while it waited succeeded")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the Put did not answer once its context ended")
			}
			select {
			case r := <-rolled:
				if c.path == "putExternal" {
					if !r.ended {
						t.Fatal("the Put rolled back on a context that cannot end")
					}
					break
				}
				if r.ended {
					t.Fatal("the Put rolled back on its caller's ended context, not through db.Rollback")
				}
				if !r.hasDeadline {
					t.Fatal("the Put rolled back on a context that cannot end")
				}
				if r.deadline.After(r.at.Add(db.RollbackBound)) {
					t.Fatalf("the Put's rollback may run %v, past db.RollbackBound (%v)", r.deadline.Sub(r.at), db.RollbackBound)
				}
			default:
				t.Fatal("the Put ran no rollback")
			}
			release()
			mustPut(t, s, "bob", "tok", "v2") // the first Put's transaction has ended
			mustGetOwned(t, s, "bob", "tok", "v2")
		})
	}
}

// holdCounterRow holds owner's erase counter row FOR UPDATE, which a fenced
// Put's share lock waits on, until the returned func runs or the test ends. A
// goroutine of its own holds the transaction.
func holdCounterRow(t *testing.T, pool *pgxpool.Pool, owner string) (release func()) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `INSERT INTO secret_owner_generations (owned_by) VALUES ($1) ON CONFLICT (owned_by) DO NOTHING`, owner); err != nil {
		t.Fatal(err)
	}
	held, done := make(chan error, 1), make(chan struct{})
	go func() {
		tx, err := pool.Begin(context.Background())
		if err != nil {
			held <- err
			return
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		_, err = tx.Exec(context.Background(), `SELECT 1 FROM secret_owner_generations WHERE owned_by=$1 FOR UPDATE`, owner)
		held <- err
		if err == nil {
			<-done
		}
	}()
	if err := <-held; err != nil {
		t.Fatal(err)
	}
	release = sync.OnceFunc(func() { close(done) })
	t.Cleanup(release)
	return release
}

// waitCounterRowWaiter returns once a session in this database waits for a
// lock on a statement that reads the erase counter row.
func waitCounterRowWaiter(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		var n int
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database()
			AND pid <> pg_backend_pid() AND wait_event_type='Lock' AND query LIKE '%secret_owner_generations%'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no session waits on the erase counter row")
		}
	}
}
