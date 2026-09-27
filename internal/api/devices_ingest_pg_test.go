// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// One enrolled laptop replays a refused 500-row x 16 KiB batch from many
// connections at once, through the real handler and a real Postgres, while
// the organisation writes its own audit rows through the same pool. The
// one-push-per-device cap keeps the laptop to one hash recompute at a time,
// so it cannot occupy the pool the organisation's writer needs.
// WARDYN_TEST_INGEST_WORKERS sets the concurrency (default 16). Guarded by
// WARDYN_TEST_PG; runs in a throwaway database.
func TestPG_DeviceIngest_OneDevicesConcurrentPushesDoNotStarveOrgAuditWriters(t *testing.T) {
	pool := throwawayPGPool(t)
	ctx := context.Background()
	cfg := baseTestConfig(newHarness(t), store.NewPG(pool))
	cfg.Audit = &safeRecorder{}
	srv := New(cfg)
	id, tok := enrolTestDevice(t, srv, "laptop")

	pad := strings.Repeat("x", 16000)
	rows := make([]types.FederatedAuditEvent, 0, 500)
	prev := ""
	for i := 1; i <= 500; i++ {
		ev := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC().Truncate(time.Microsecond), ActorType: types.ActorSystem,
			Actor: "device", Action: "x", Outcome: "success", Data: json.RawMessage(`{"pad":"` + pad + `"}`), PrevHash: prev}
		if err := pool.QueryRow(ctx, `SELECT audit_row_hash($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::jsonb)`,
			prev, ev.ID, ev.Time, ev.RunID, string(ev.ActorType), ev.Actor, ev.Action, ev.Target, ev.Outcome, ev.SourceIP, []byte(ev.Data),
		).Scan(&ev.RowHash); err != nil {
			t.Fatal(err)
		}
		prev = ev.RowHash
		rows = append(rows, types.FederatedAuditEvent{AuditEvent: ev, Seq: int64(i)})
	}
	rows[499].RowHash = "deadbeef"
	body := string(mustJSON(rows))
	path := "/api/v1/devices/" + id.String() + "/audit"

	workers := 16
	if n, _ := strconv.Atoi(os.Getenv("WARDYN_TEST_INGEST_WORKERS")); n > 0 {
		workers = n
	}
	const window = 15 * time.Second
	deadline := time.Now().Add(window)
	var pushes, throttled atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				if do(t, srv, http.MethodPost, path, tok, body).Code == http.StatusTooManyRequests {
					throttled.Add(1)
				}
				pushes.Add(1)
			}
		}()
	}
	var writes, failures int
	var maxLat time.Duration
	for time.Now().Before(deadline) {
		ev := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorSystem, Actor: "wardyn/test",
			Action: "run.create", Outcome: "success", Data: json.RawMessage(`{}`)}
		s := time.Now()
		err := store.InsertAuditEvent(ctx, pool, &ev)
		maxLat = max(maxLat, time.Since(s))
		writes++
		if err != nil {
			failures++
		}
	}
	wg.Wait()
	t.Logf("%d workers pushing for %s: %d pushes (%d answered 429); org writer: %d inserts, %d failed, max latency %s (pool_max_conns %d)",
		workers, window, pushes.Load(), throttled.Load(), writes, failures, maxLat, pool.Config().MaxConns)
	if failures > 0 || maxLat > 5*time.Second {
		t.Errorf("org audit writes failed %d times (max latency %s) while one device pushed from %d connections", failures, maxLat, workers)
	}
}
