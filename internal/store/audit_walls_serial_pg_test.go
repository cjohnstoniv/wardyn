// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The Walls view's live streams read decision rows with a cursor: the next beat asks for the rows after the last
// one it saw. That is only safe if a row can never commit behind a row the reader has already passed. The audit
// writers take one advisory lock around the sequence number and the insert, so sequence order is commit order;
// these tests hold that to account through the accessor itself.
package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// wallsSerialPool migrates a complete schema into a throwaway namespace and returns a pool on it, so the rows these
// tests write never reach another test's audit log. An unreachable server fails the test; only an unset
// WARDYN_TEST_PG skips it.
func wallsSerialPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set")
	}
	ctx := context.Background()
	base, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to the server WARDYN_TEST_PG names: %v", err)
	}
	t.Cleanup(base.Close)
	if err := base.Ping(ctx); err != nil {
		t.Fatalf("ping the server WARDYN_TEST_PG names: %v", err)
	}
	schema := fmt.Sprintf("wardyn_walls_serial_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := base.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create the schema: %v", err)
	}
	t.Cleanup(func() {
		if _, err := base.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
			t.Logf("drop schema %s: %v", schema, err)
		}
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse the DSN: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 16
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect with search_path=%s: %v", schema, err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate schema %s: %v", schema, err)
	}
	return pool
}

// wallsSerialEvent is an egress.deny row written by actor, carrying host in its data.
func wallsSerialEvent(actor, host string) types.AuditEvent {
	return types.AuditEvent{
		ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorSystem, Actor: actor,
		Action: "egress.deny", Outcome: "success", Data: json.RawMessage(`{"host":"` + host + `"}`),
	}
}

// wallsSerialRead is one accessor read of every egress.deny row after cursor and at or after floor.
func wallsSerialRead(t *testing.T, pool *pgxpool.Pool, floor time.Time, cursor store.DecisionCursor) ([]store.DecisionRow, store.DecisionCursor) {
	t.Helper()
	rows, next, err := store.NewPG(pool).AuditWallsRows(context.Background(), store.DecisionQuery{
		Actions: []string{"egress.deny"}, Since: floor, After: cursor, Limit: 100,
	})
	if err != nil {
		t.Fatalf("AuditWallsRows: %v", err)
	}
	return rows, next
}

// TestAuditWallsRowsNeverSkipsACommit_PG: a row that is appended but not committed is invisible, and the writer that
// queues behind it commits after it, so a read from the same cursor after both commit returns both, the first
// writer's first and each once. Nothing commits behind a cursor that has passed it.
func TestAuditWallsRowsNeverSkipsACommit_PG(t *testing.T) {
	pool := wallsSerialPool(t)
	ctx := context.Background()

	tx1, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the first writer's transaction: %v", err)
	}
	defer tx1.Rollback(context.Background()) //nolint:errcheck // committed below on the happy path
	first := uuid.New()
	if _, err := tx1.Exec(ctx, `SELECT audit_append($1, now(), NULL, 'system', 'walls-serial-first', 'egress.deny', '', 'success', '', NULL)`, first); err != nil {
		t.Fatalf("the first writer's append: %v", err)
	}

	second := wallsSerialEvent("walls-serial-second", "second.example")
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- store.InsertAuditEvent(ctx, pool, &second) }()
	select {
	case err := <-done:
		t.Fatalf("the second writer returned (%v) while the first held the chain lock", err)
	case <-time.After(500 * time.Millisecond):
	}

	rows, cursor := wallsSerialRead(t, pool, time.Time{}, store.DecisionCursor{})
	if len(rows) != 0 || cursor.Seq != 0 {
		t.Fatalf("a read before either commit returned %d rows, cursor %d; want none", len(rows), cursor.Seq)
	}

	// The second writer waits for the chain lock for at most store's bound (5 s), so the first commits well inside it.
	if left := 2*time.Second - time.Since(start); left <= 0 {
		t.Fatalf("the first writer was held %v before its commit, past the 2s the test allows", time.Since(start))
	}
	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("commit the first writer: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the second writer: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second writer never returned after the first committed")
	}

	rows, _ = wallsSerialRead(t, pool, time.Time{}, store.DecisionCursor{})
	if len(rows) != 2 || rows[0].ID != first || rows[1].ID != second.ID || rows[0].Seq >= rows[1].Seq {
		t.Fatalf("a read from the same cursor after both commits returned %d rows %+v; want the first writer's row, then the second's, each once", len(rows), rows)
	}
}

// TestAuditWallsRowsConcurrentWritersSeenOnce_PG: eight writers insert fifty rows each while a reader loops the
// accessor with the streams' rule (the cursor it returned, and the last row's recorded time as the floor). Every row
// is seen exactly once, and each writer's rows come back in the order that writer inserted them.
func TestAuditWallsRowsConcurrentWritersSeenOnce_PG(t *testing.T) {
	const writers, each = 8, 50
	pool := wallsSerialPool(t)
	ctx := context.Background()

	expected := map[uuid.UUID]bool{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	failures := make(chan error, writers)
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				ev := wallsSerialEvent(fmt.Sprintf("walls-serial-w%d", w), fmt.Sprintf("%03d", i))
				mu.Lock()
				expected[ev.ID] = true
				mu.Unlock()
				if err := store.InsertAuditEvent(ctx, pool, &ev); err != nil {
					failures <- fmt.Errorf("writer %d row %d: %w", w, i, err)
					return
				}
			}
		}()
	}

	var seen []store.DecisionRow
	var cursor store.DecisionCursor
	var floor time.Time // the zero time at first: recorded_at is the database's clock
	deadline := time.Now().Add(30 * time.Second)
	for len(seen) < writers*each && time.Now().Before(deadline) {
		rows, next := wallsSerialRead(t, pool, floor, cursor)
		if len(rows) == 0 {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		seen = append(seen, rows...)
		cursor, floor = next, rows[len(rows)-1].RecordedAt
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}

	count := map[uuid.UUID]int{}
	lastHost := map[string]string{}
	for _, r := range seen {
		count[r.ID]++
		if prev, ok := lastHost[r.Actor]; ok && r.Host <= prev {
			t.Errorf("%s: row %s came back after %s, out of its insert order", r.Actor, r.Host, prev)
		}
		lastHost[r.Actor] = r.Host
	}
	var missing []uuid.UUID
	for id := range expected {
		switch n := count[id]; {
		case n == 0:
			missing = append(missing, id)
		case n > 1:
			t.Errorf("row %s was read %d times, want once", id, n)
		}
	}
	if len(missing) > 0 {
		t.Errorf("%d of %d rows were never read within 30s; missing %v", len(missing), writers*each, missing)
	}
}
