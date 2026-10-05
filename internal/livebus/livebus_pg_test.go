// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package livebus_test

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/livebus"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping Postgres-backed test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// A notice published by one replica reaches every replica's handler for its kind, itself
// included, with its origin and data; another kind's handler is not called.
func TestBus_PublishReachesEveryReplica(t *testing.T) {
	pool := testPool(t)
	ctx := t.Context()
	var mu sync.Mutex
	heard := map[string]livebus.Message{}
	for _, name := range []string{"replica-a", "replica-b"} {
		bus := livebus.New(pool, name)
		bus.Handle(livebus.KindRunKill, func(m livebus.Message) {
			mu.Lock()
			heard[name] = m
			mu.Unlock()
		})
		bus.Handle(livebus.KindRunEvent, func(m livebus.Message) { t.Errorf("a %s handler was called for a kill", m.Kind) })
		bus.Start(ctx)
	}
	publisher := livebus.New(pool, "replica-a")
	run := uuid.New()
	// The listeners connect asynchronously: publish until both have heard.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := publisher.Publish(ctx, livebus.KindRunKill, run, map[string]string{"why": "test"}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
		mu.Lock()
		n := len(heard)
		mu.Unlock()
		if n == 2 {
			break
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(heard) != 2 {
		t.Fatalf("%d replicas heard the notice within 10 seconds, want 2", len(heard))
	}
	for name, m := range heard {
		if m.Origin != "replica-a" || m.Run != run || !strings.Contains(string(m.Data), `"why":"test"`) {
			t.Errorf("%s heard %+v, want origin replica-a, run %s and the data", name, m, run)
		}
	}
}

// A notice too large for NOTIFY is refused, never truncated.
func TestBus_OversizeNoticeIsRefused(t *testing.T) {
	pool := testPool(t)
	a := livebus.New(pool, "replica-a")
	if err := a.Publish(t.Context(), livebus.KindRunEvent, uuid.New(), strings.Repeat("x", 9000)); err == nil {
		t.Fatal("a 9000-byte notice was published")
	}
}
