// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func auditRowsWithID(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A replayed event (store.WithAuditReplay) is appended once however often it is written, by one writer
// or by several at once; an unmarked write appends every time, as before.
func TestPG_AuditReplayAppendsOneRowPerEvent(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	event := func() types.AuditEvent {
		return types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorSystem, Actor: "test",
			Action: "test.replay", Outcome: "success"}
	}

	plain := event()
	for range 2 {
		ev := plain
		if err := store.InsertAuditEvent(ctx, pool, &ev); err != nil {
			t.Fatal(err)
		}
	}
	if n := auditRowsWithID(t, pool, plain.ID); n != 2 {
		t.Fatalf("an unmarked event written twice left %d rows, want 2 (unchanged)", n)
	}

	replayed := event()
	for range 3 {
		ev := replayed
		if err := store.InsertAuditEvent(store.WithAuditReplay(ctx), pool, &ev); err != nil {
			t.Fatal(err)
		}
	}
	if n := auditRowsWithID(t, pool, replayed.ID); n != 1 {
		t.Fatalf("a replayed event written three times left %d rows, want 1", n)
	}

	concurrent := event()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ev := concurrent
			errs <- store.InsertAuditEvent(store.WithAuditReplay(ctx), pool, &ev)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := auditRowsWithID(t, pool, concurrent.ID); n != 1 {
		t.Fatalf("eight writers replaying one event at once left %d rows, want 1", n)
	}
}
