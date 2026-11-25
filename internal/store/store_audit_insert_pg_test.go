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

// A replay of an event the table already holds adds no row and hands the caller the stored row's hashes, so a
// sink that gets the event carries the chain position the database gave it.
func TestPG_AuditReplayOfAHeldEventCarriesTheStoredHashes(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	first := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC().Truncate(time.Microsecond), ActorType: types.ActorSystem, Actor: "test",
		Action: "test.replay.held", Outcome: "success"}
	if err := store.InsertAuditEvent(ctx, pool, &first); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if first.RowHash == "" {
		t.Fatal("the first write carries no row hash")
	}

	again := first
	again.PrevHash, again.RowHash = "", ""
	if err := store.InsertAuditEvent(store.WithAuditReplay(ctx), pool, &again); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if n := auditRowsWithID(t, pool, first.ID); n != 1 {
		t.Fatalf("%d rows carry the event's id after a replay, want 1", n)
	}
	if again.PrevHash != first.PrevHash || again.RowHash != first.RowHash {
		t.Errorf("replay hashes %q/%q, want the stored %q/%q", again.PrevHash, again.RowHash, first.PrevHash, first.RowHash)
	}
}

// A replay of an event the table does not hold appends it and carries the hashes it was given.
func TestPG_AuditReplayOfANewEventAppendsAndCarriesItsHashes(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	ev := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC().Truncate(time.Microsecond), ActorType: types.ActorSystem, Actor: "test",
		Action: "test.replay.new", Outcome: "success"}
	if err := store.InsertAuditEvent(store.WithAuditReplay(ctx), pool, &ev); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if n := auditRowsWithID(t, pool, ev.ID); n != 1 {
		t.Fatalf("%d rows carry the event's id, want 1", n)
	}
	var prev, hash string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(prev_hash,''), COALESCE(row_hash,'') FROM audit_events WHERE id = $1`, ev.ID).Scan(&prev, &hash); err != nil {
		t.Fatal(err)
	}
	if hash == "" || ev.RowHash != hash || ev.PrevHash != prev {
		t.Errorf("event hashes %q/%q, want the stored %q/%q", ev.PrevHash, ev.RowHash, prev, hash)
	}
}
